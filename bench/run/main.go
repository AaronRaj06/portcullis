// Command run is the reproducible benchmark harness (`make bench`).
//
// Everything runs in Docker on one bridge network, each target container
// limited to the same CPU quota: the backend, Portcullis, the standard
// library's httputil.ReverseProxy, NGINX, and the vegeta load generator. It
// measures:
//
//   - throughput: closed loop (vegeta -rate=0) at fixed concurrency;
//   - latency: open loop at fixed request rates, so slow responses cannot hide
//     queueing (no coordinated omission);
//   - failure injection: a backend stops accepting connections under load.
//
// Containers are pinned to disjoint vCPU sets (--cpuset-cpus): the load
// generator, the proxy under test (one core, so the proxy is the bottleneck),
// and the backends never share a core.
//
// Every scenario runs -runs times; the summary reports medians. Raw vegeta
// files, per-run JSON and docker stats samples stay in the output directory.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	image = "portcullis-bench"
	nginx = "nginx:1.27.5-alpine"
)

// target is one system under test.
type target struct {
	name      string // in reports
	container string // whose CPU/memory we sample
	url       string // base URL inside the bench network
}

// All containers share the Docker VM's network namespace (--network host) and
// talk over its loopback, so no bridge, veth pair or NAT sits in the path. On
// a bridge network every target, even the bare backend, capped out at
// ~12-13k req/s while below its CPU limit: the virtual network was the
// bottleneck, not the proxies. (On Docker Desktop "host" is the Linux VM, so no
// ports open on the Mac.)
var targets = []target{
	{"direct", "be1", "http://127.0.0.1:9000"},
	{"portcullis", "pc", "http://127.0.0.1:8080"},
	{"httputil", "rp", "http://127.0.0.1:8081"},
	{"nginx", "nx", "http://127.0.0.1:8082"},
}

var failoverTargets = []target{
	{"portcullis", "pcf", "http://127.0.0.1:8090"},
	{"nginx", "nx", "http://127.0.0.1:8083"},
}

// scenario is one load shape.
type scenario struct {
	Name      string `json:"name"`
	Path      string `json:"path"`      // /small or /large
	KeepAlive bool   `json:"keepAlive"` // client-side keep-alive
	Workers   int    `json:"workers"`   // closed loop if > 0
	Rate      int    `json:"rate"`      // open loop if > 0 (req/s)
}

func (s scenario) id() string {
	ka := "ka"
	if !s.KeepAlive {
		ka = "noka"
	}
	if s.Workers > 0 {
		return fmt.Sprintf("throughput-%s-%s-c%d", strings.TrimPrefix(s.Path, "/"), ka, s.Workers)
	}
	return fmt.Sprintf("latency-%s-%s-r%d", strings.TrimPrefix(s.Path, "/"), ka, s.Rate)
}

// result is one run of one scenario against one target.
type result struct {
	Scenario   string   `json:"scenario"`
	Target     string   `json:"target"`
	Run        int      `json:"run"`
	Requests   int      `json:"requests"`
	Throughput float64  `json:"throughput"` // successful req/s
	Success    float64  `json:"success"`    // 0..1
	P50ms      float64  `json:"p50_ms"`
	P95ms      float64  `json:"p95_ms"`
	P99ms      float64  `json:"p99_ms"`
	MaxMs      float64  `json:"max_ms"`
	CPUPct     float64  `json:"cpu_pct"`    // mean of docker stats samples, 100 = one core
	MemMiB     float64  `json:"mem_mib"`    // peak
	HostLoad1  float64  `json:"host_load1"` // macOS 1-minute load average when the run ended: context for noise
	Errors     []string `json:"errors,omitempty"`
}

func main() {
	var (
		out      = flag.String("out", filepath.Join("bench", "results", time.Now().UTC().Format("20060102-150405")), "output directory")
		duration = flag.Duration("duration", 10*time.Second, "length of each measured run")
		runs     = flag.Int("runs", 3, "runs per scenario (medians are reported)")
		genCPUs  = flag.String("gen-cpus", "0-2", "vCPUs (cpuset) for the vegeta load generator")
		tgtCPUs  = flag.String("target-cpus", "3", "vCPUs (cpuset) for each proxy under test")
		beCPUs   = flag.String("backend-cpus", "4,5", "vCPUs (cpuset) for the backends")
		only     = flag.String("targets", "direct,portcullis,httputil,nginx", "targets to run")
		rates    = flag.String("rates", "2000,6000", "open-loop rates for /small (req/s)")
		quick    = flag.Bool("quick", false, "small matrix (for optimisation rounds)")
		failover = flag.Bool("failover", true, "run the failure-injection scenario")
		foRuns   = flag.Int("failover-runs", 3, "runs of the failure-injection scenario")
		profile  = flag.Duration("profile", 0, "if > 0, capture a Portcullis CPU profile of this length during the first throughput run")
		build    = flag.Bool("build", true, "build the bench image first")
		matrix   = flag.Bool("matrix", true, "run the throughput/latency matrix (false: failure injection only)")
	)
	flag.Parse()
	must(os.MkdirAll(filepath.Join(*out, "raw"), 0o750))
	abs, err := filepath.Abs(*out)
	must(err)
	h := &harness{out: abs, dur: *duration, gen: *genCPUs, tgt: *tgtCPUs, be: *beCPUs}

	if *build {
		h.sh("docker", "build", "-q", "-t", image, "-f", "bench/Dockerfile", ".")
	}
	h.up()
	defer h.down()
	sysctls := h.tuneVM()

	var scenarios []scenario
	for _, c := range []int{16, 64, 256} {
		scenarios = append(scenarios, scenario{Path: "/small", KeepAlive: true, Workers: c})
		if !*quick {
			scenarios = append(scenarios, scenario{Path: "/small", KeepAlive: false, Workers: c})
		}
	}
	if !*quick {
		for _, c := range []int{16, 64} {
			scenarios = append(scenarios, scenario{Path: "/large", KeepAlive: true, Workers: c})
		}
	}
	for _, r := range strings.Split(*rates, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(r))
		must(err)
		scenarios = append(scenarios, scenario{Path: "/small", KeepAlive: true, Rate: n})
	}
	if !*quick {
		scenarios = append(scenarios, scenario{Path: "/large", KeepAlive: true, Rate: 500})
	}
	for i := range scenarios {
		scenarios[i].Name = scenarios[i].id()
	}

	selected := map[string]bool{}
	for _, t := range strings.Split(*only, ",") {
		selected[strings.TrimSpace(t)] = true
	}
	h.writeJSON("meta.json", map[string]any{
		"started": time.Now().UTC(), "duration": duration.String(), "runs": *runs,
		"cpusets":    map[string]string{"generator": *genCPUs, "target": *tgtCPUs, "backends": *beCPUs},
		"vm_sysctls": sysctls,
		"scenarios":  scenarios, "host": hostInfo(), "commit": strings.TrimSpace(h.output("git", "rev-parse", "--short", "HEAD")),
	})

	var all []result
	profiled := false
	if !*matrix {
		scenarios = nil
	}
	for _, s := range scenarios {
		h.drainTimeWait()
		var ts []target
		for _, t := range targets {
			if selected[t.name] {
				ts = append(ts, t)
				h.attack(s, t, 2*time.Second, "warmup") // discarded: fills connection pools, warms caches
			}
		}
		// Runs are interleaved across targets (A B C D, A B C D, ...) so slow
		// drift on the host, such as thermal throttling, affects all of them alike.
		for i := 1; i <= *runs; i++ {
			for _, t := range ts {
				var prof chan struct{}
				if *profile > 0 && !profiled && t.name == "portcullis" && s.Workers > 0 {
					profiled = true
					prof = h.captureProfile(*profile)
				}
				r := h.measure(s, t, i)
				if prof != nil {
					<-prof
				}
				log.Printf("%-34s %-10s run %d: %8.0f req/s  p50 %6.2fms  p99 %7.2fms  ok %.4f  cpu %5.0f%%  mem %5.1fMiB",
					s.Name, t.name, i, r.Throughput, r.P50ms, r.P99ms, r.Success, r.CPUPct, r.MemMiB)
				all = append(all, r)
				h.writeJSON("results.json", all) // after every run: a crash keeps what finished
			}
		}
	}
	if *matrix {
		h.writeJSON("results.json", all)
		h.summarise(scenarios, all)
	}
	if *failover {
		h.failover(*foRuns)
	}
	log.Printf("done: %s", abs)
}

type harness struct {
	out          string
	dur          time.Duration
	gen, tgt, be string // cpusets
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func (h *harness) sh(args ...string) {
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // fixed commands built by this harness
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%s: %v", strings.Join(args, " "), err)
	}
}

// shRetry is sh for commands that may hit a transient Docker engine error
// (the first final run died on one "500 Internal Server Error" from
// containers/create). It retries up to 3 times, logging each failure.
func (h *harness) shRetry(args ...string) {
	for attempt := 1; ; attempt++ {
		cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // fixed commands built by this harness
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		err := cmd.Run()
		if err == nil {
			return
		}
		if attempt == 3 {
			log.Fatalf("%s: %v (after %d attempts)", strings.Join(args, " "), err, attempt)
		}
		log.Printf("attempt %d failed (%v); retrying in %ds", attempt, err, attempt*5)
		time.Sleep(time.Duration(attempt*5) * time.Second)
	}
}

func (h *harness) output(args ...string) string {
	b, _ := exec.Command(args[0], args[1:]...).Output() //nolint:gosec // fixed commands
	return string(b)
}

func (h *harness) quiet(args ...string) { _ = exec.Command(args[0], args[1:]...).Run() } //nolint:gosec

var containers = []string{"be1", "be2", "pc", "pcf", "rp", "nx"}

func (h *harness) up() {
	h.down()
	run := func(name, cpus string, cmd ...string) {
		args := append([]string{"run", "-d", "--name", name, "--network", "host", "--cpuset-cpus", cpus}, cmd...)
		h.sh(append([]string{"docker"}, args...)...)
	}
	run("be1", h.be, image, "backend", "-name", "be1", "-listen", "127.0.0.1:9000")
	run("be2", h.be, image, "backend", "-name", "be2", "-listen", "127.0.0.1:9001")
	run("pc", h.tgt, image, "portcullis", "-config", "/etc/portcullis/portcullis.yaml",
		"-listen", "127.0.0.1:8080", "-admin-listen", "127.0.0.1:9901", "-pprof", "-log-level", "warn")
	run("pcf", h.tgt, image, "portcullis", "-config", "/etc/portcullis/portcullis-failover.yaml",
		"-listen", "127.0.0.1:8090", "-admin-listen", "127.0.0.1:9902", "-log-level", "warn")
	run("rp", h.tgt, image, "rproxy", "-listen", "127.0.0.1:8081", "-upstream", "http://127.0.0.1:9000")
	conf, err := filepath.Abs("bench/nginx/nginx.conf")
	must(err)
	run("nx", h.tgt, "-v", conf+":/etc/nginx/nginx.conf:ro", nginx)
	// Wait until every target answers.
	for _, t := range append(append([]target{}, targets...), failoverTargets...) {
		ok := false
		for i := 0; i < 30 && !ok; i++ {
			r := h.attack(scenario{Name: "ready", Path: "/small", KeepAlive: true, Rate: 20}, t, time.Second, "ready")
			ok = r.Success == 1
			if !ok {
				time.Sleep(time.Second)
			}
		}
		if !ok {
			log.Fatalf("%s never became ready", t.name)
		}
	}
}

func (h *harness) down() {
	for _, c := range containers {
		h.quiet("docker", "rm", "-f", c)
	}
}

// attack runs vegeta in a container and returns its parsed report.
func (h *harness) attack(s scenario, t target, d time.Duration, id string) result {
	return h.attackURL(s, t, t.url+s.Path, d, id)
}

func (h *harness) attackURL(s scenario, t target, url string, d time.Duration, id string) result {
	h.checkDisk()
	file := fmt.Sprintf("%s-%s-%s", s.Name, t.name, id)
	must(os.WriteFile(filepath.Join(h.out, "raw", file+".targets"), []byte("GET "+url+"\n"), 0o600))
	args := []string{"run", "--rm", "--user", "0", "--network", "host", "--cpuset-cpus", h.gen,
		"-v", h.out + "/raw:/out", image, "vegeta", "attack",
		"-targets=/out/" + file + ".targets", "-duration=" + d.String(), "-timeout=10s",
		// Don't store response bodies: at 256 KiB per /large response vegeta's
		// output reached gigabytes and filled the disk (it crashed Docker's VM
		// once). Latency and status don't need the body.
		"-max-body=0",
		"-keepalive=" + strconv.FormatBool(s.KeepAlive), "-output=/out/" + file + ".bin"}
	if s.Workers > 0 {
		w := strconv.Itoa(s.Workers)
		args = append(args, "-rate=0", "-workers="+w, "-max-workers="+w)
	} else {
		args = append(args, "-rate="+strconv.Itoa(s.Rate), "-max-workers=2048")
	}
	h.shRetry(append([]string{"docker"}, args...)...)
	bin := filepath.Join(h.out, "raw", file+".bin")
	r, rawReport := report(bin)
	r.Scenario, r.Target = s.Name, t.name
	switch {
	case id == "warmup" || id == "ready":
		_ = os.Remove(bin)
		_ = os.Remove(filepath.Join(h.out, "raw", file+".targets"))
	case s.Name != "failover":
		// Keep vegeta's full JSON report (latency distribution, status codes,
		// errors) and drop the per-request binary: at ~20k req/s a 10s run is
		// ~20 MB, and the full matrix would need gigabytes of disk.
		must(os.WriteFile(filepath.Join(h.out, "raw", file+".vegeta.json"), rawReport, 0o600))
		_ = os.Remove(bin)
	}
	return r
}

// vmShell runs a command in the Docker VM's own namespaces (all containers use
// --network host, so this is the network stack every target shares).
func (h *harness) vmShell(cmd string) string {
	out, err := exec.Command("docker", "run", "--rm", "--privileged", "--pid=host", "alpine:3.20", //nolint:gosec
		"nsenter", "-t", "1", "-m", "-n", "--", "sh", "-c", cmd).Output()
	if err != nil {
		log.Fatalf("vm command %q: %v", cmd, err)
	}
	return strings.TrimSpace(string(out))
}

// tuneVM applies the usual load-testing settings to the VM's network stack.
// Every container shares one ephemeral port range (host networking), and
// keep-alive-off scenarios leave tens of thousands of sockets in TIME_WAIT for
// 60 s. Without this, later scenarios failed inside the load generator itself
// ("bind: address already in use"). The settings last until Docker restarts.
func (h *harness) tuneVM() map[string]string {
	h.vmShell("sysctl -qw net.ipv4.ip_local_port_range='1024 65535' net.ipv4.tcp_tw_reuse=1")
	return map[string]string{
		"net.ipv4.ip_local_port_range": h.vmShell("sysctl -n net.ipv4.ip_local_port_range"),
		"net.ipv4.tcp_tw_reuse":        h.vmShell("sysctl -n net.ipv4.tcp_tw_reuse"),
	}
}

// drainTimeWait waits (up to 90 s) until few sockets are in TIME_WAIT, so one
// scenario's closed connections cannot starve the next one of ports.
func (h *harness) drainTimeWait() {
	for i := 0; i < 18; i++ {
		f := strings.Fields(h.vmShell("grep '^TCP:' /proc/net/sockstat"))
		tw := 0
		for j := 0; j+1 < len(f); j++ {
			if f[j] == "tw" {
				tw, _ = strconv.Atoi(f[j+1])
			}
		}
		if tw < 2000 {
			return
		}
		log.Printf("waiting for %d TIME_WAIT sockets to drain", tw)
		time.Sleep(5 * time.Second)
	}
}

// minFreeDisk is the free space the harness insists on before each attack. A
// full host disk makes Docker's VM filesystem abort its journal and go
// read-only, which is far worse than a stopped benchmark.
const minFreeDisk = 3 << 30

func (h *harness) checkDisk() {
	var st syscall.Statfs_t
	if err := syscall.Statfs(h.out, &st); err != nil {
		return
	}
	if free := st.Bavail * uint64(st.Bsize); free < minFreeDisk { //nolint:gosec // Bsize is int64 on Linux but never negative

		log.Fatalf("only %.1f GiB free on the results disk; stopping before Docker runs out of space", float64(free)/(1<<30))
	}
}

// measure runs one attack while sampling the target's CPU and memory.
func (h *harness) measure(s scenario, t target, run int) result {
	ctx, cancel := context.WithCancel(context.Background())
	samples := make(chan []stat, 1)
	go func() { samples <- sampleStats(ctx, t.container) }()
	r := h.attack(s, t, h.dur, "run"+strconv.Itoa(run))
	cancel()
	st := <-samples
	r.Run = run
	r.CPUPct, r.MemMiB = summariseStats(st)
	r.HostLoad1 = hostLoad1()
	h.writeJSON(filepath.Join("raw", fmt.Sprintf("%s-%s-run%d.stats.json", s.Name, t.name, run)), st)
	return r
}

// report parses `vegeta report -type=json` and also returns the raw report.
func report(bin string) (result, []byte) {
	out, err := exec.Command(vegetaBin(), "report", "-type=json", bin).Output() //nolint:gosec
	must(err)
	var rep struct {
		Requests   int     `json:"requests"`
		Throughput float64 `json:"throughput"`
		Success    float64 `json:"success"`
		Latencies  struct {
			P50 float64 `json:"50th"`
			P95 float64 `json:"95th"`
			P99 float64 `json:"99th"`
			Max float64 `json:"max"`
		} `json:"latencies"`
		Errors []string `json:"errors"`
	}
	must(json.Unmarshal(out, &rep))
	ms := func(ns float64) float64 { return ns / 1e6 }
	errs := rep.Errors
	if len(errs) > 5 {
		errs = errs[:5]
	}
	return result{Requests: rep.Requests, Throughput: rep.Throughput, Success: rep.Success,
		P50ms: ms(rep.Latencies.P50), P95ms: ms(rep.Latencies.P95), P99ms: ms(rep.Latencies.P99), MaxMs: ms(rep.Latencies.Max), Errors: errs}, out
}

func vegetaBin() string {
	if p, err := filepath.Abs("bin/tools/vegeta"); err == nil {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	log.Fatal("bin/tools/vegeta missing: run `make tools`")
	return ""
}

type stat struct {
	At     time.Time `json:"at"`
	CPUPct float64   `json:"cpu_pct"`
	MemMiB float64   `json:"mem_mib"`
}

// sampleStats streams `docker stats` (about one sample per second) until ctx ends.
func sampleStats(ctx context.Context, container string) []stat {
	cmd := exec.CommandContext(ctx, "docker", "stats", "--format", "{{.CPUPerc}}|{{.MemUsage}}", container) //nolint:gosec
	pipe, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return nil
	}
	var out []stat
	sc := bufio.NewScanner(pipe)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.LastIndex(line, "\x1b[H"); i >= 0 { // docker stats redraws with ANSI codes
			line = line[i+3:]
		}
		line = strings.TrimPrefix(line, "\x1b[2J")
		cpu, mem, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		c, err1 := strconv.ParseFloat(strings.TrimSuffix(cpu, "%"), 64)
		m, err2 := parseMem(strings.TrimSpace(strings.SplitN(mem, "/", 2)[0]))
		if err1 == nil && err2 == nil {
			out = append(out, stat{time.Now(), c, m})
		}
	}
	_ = cmd.Wait()
	return out
}

func parseMem(s string) (float64, error) {
	for _, u := range []struct {
		suf string
		mul float64
	}{{"GiB", 1024}, {"MiB", 1}, {"KiB", 1.0 / 1024}, {"kB", 1.0 / 1024}, {"MB", 1}, {"GB", 1024}, {"B", 1.0 / (1 << 20)}} {
		if strings.HasSuffix(s, u.suf) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suf), 64)
			return v * u.mul, err
		}
	}
	return 0, errors.New("unknown memory unit: " + s)
}

// summariseStats returns mean CPU (ignoring the idle first sample) and peak memory.
func summariseStats(st []stat) (cpu, mem float64) {
	if len(st) > 1 {
		st = st[1:]
	}
	for _, s := range st {
		cpu += s.CPUPct
		mem = math.Max(mem, s.MemMiB)
	}
	if len(st) > 0 {
		cpu /= float64(len(st))
	}
	return cpu, mem
}

func (h *harness) writeJSON(name string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(h.out, name), b, 0o600))
}

func spread(rs []result, f func(result) float64) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, r := range rs {
		lo, hi = math.Min(lo, f(r)), math.Max(hi, f(r))
	}
	return lo, hi
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// summarise writes medians per scenario and target as CSV, JSON and Markdown.
func (h *harness) summarise(scenarios []scenario, all []result) {
	type key struct{ s, t string }
	group := map[key][]result{}
	for _, r := range all {
		group[key{r.Scenario, r.Target}] = append(group[key{r.Scenario, r.Target}], r)
	}
	var csv, md bytes.Buffer
	csv.WriteString("scenario,target,runs,throughput_rps,throughput_min,throughput_max,success,p50_ms,p95_ms,p99_ms,p99_min,p99_max,max_ms,cpu_pct,cpu_pct_per_1k_rps,mem_mib\n")
	md.WriteString("| scenario | target | req/s (min–max) | success | p50 ms | p95 ms | p99 ms (min–max) | CPU % | CPU %/1k req/s | mem MiB | host load (max) |\n|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	var summary []result
	for _, s := range scenarios {
		for _, t := range targets {
			rs := group[key{s.Name, t.name}]
			if len(rs) == 0 {
				continue
			}
			pick := func(f func(result) float64) float64 {
				xs := make([]float64, len(rs))
				for i, r := range rs {
					xs[i] = f(r)
				}
				return median(xs)
			}
			m := result{Scenario: s.Name, Target: t.name, Run: len(rs),
				Throughput: pick(func(r result) float64 { return r.Throughput }), Success: pick(func(r result) float64 { return r.Success }),
				P50ms: pick(func(r result) float64 { return r.P50ms }), P95ms: pick(func(r result) float64 { return r.P95ms }),
				P99ms: pick(func(r result) float64 { return r.P99ms }), MaxMs: pick(func(r result) float64 { return r.MaxMs }),
				CPUPct: pick(func(r result) float64 { return r.CPUPct }), MemMiB: pick(func(r result) float64 { return r.MemMiB })}
			summary = append(summary, m)
			lo, hi := spread(rs, func(r result) float64 { return r.Throughput })
			p99lo, p99hi := spread(rs, func(r result) float64 { return r.P99ms })
			_, loadHi := spread(rs, func(r result) float64 { return r.HostLoad1 }) // above ~4 on this 8-core Mac the runs are noise
			perK := 0.0                                                            // CPU cost: % of one core per 1000 req/s served
			if m.Throughput > 0 {
				perK = m.CPUPct / (m.Throughput / 1000)
			}
			fmt.Fprintf(&csv, "%s,%s,%d,%.0f,%.0f,%.0f,%.4f,%.3f,%.3f,%.3f,%.3f,%.3f,%.3f,%.0f,%.2f,%.1f\n", m.Scenario, m.Target, m.Run,
				m.Throughput, lo, hi, m.Success, m.P50ms, m.P95ms, m.P99ms, p99lo, p99hi, m.MaxMs, m.CPUPct, perK, m.MemMiB)
			fmt.Fprintf(&md, "| %s | %s | %.0f (%.0f–%.0f) | %.4f | %.2f | %.2f | %.2f (%.2f–%.2f) | %.0f | %.1f | %.1f | %.1f |\n", m.Scenario, m.Target,
				m.Throughput, lo, hi, m.Success, m.P50ms, m.P95ms, m.P99ms, p99lo, p99hi, m.CPUPct, perK, m.MemMiB, loadHi)
		}
	}
	must(os.WriteFile(filepath.Join(h.out, "summary.csv"), csv.Bytes(), 0o600))
	must(os.WriteFile(filepath.Join(h.out, "summary.md"), md.Bytes(), 0o600))
	h.writeJSON("summary.json", summary)
}

// captureProfile fetches a CPU profile from Portcullis's admin port in the background.
func (h *harness) captureProfile(d time.Duration) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The admin port is on the Docker VM's loopback; fetch it from a
		// container in that namespace (pinned with the load generator).
		url := fmt.Sprintf("http://127.0.0.1:9901/debug/pprof/profile?seconds=%d", int(d.Seconds()))
		b, err := exec.Command("docker", "run", "--rm", "--network", "host", "--cpuset-cpus", h.gen, //nolint:gosec
			"alpine:3.20", "wget", "-q", "-O", "-", url).Output()
		if err != nil || len(b) == 0 {
			log.Printf("profile capture failed: %v", err)
			return
		}
		must(os.WriteFile(filepath.Join(h.out, "portcullis-cpu.prof"), b, 0o600))
	}()
	return done
}

// failover kills one of two backends under load (it stops accepting
// connections, like a crashed process) and brings it back, measuring errors,
// recovery time and re-admission for Portcullis and NGINX.
func (h *harness) failover(runs int) {
	const rate = 1000
	total, kill, revive := 30*time.Second, 10*time.Second, 20*time.Second
	type fo struct {
		Target       string  `json:"target"`
		Run          int     `json:"run"`
		Requests     int     `json:"requests"`
		Errors       int     `json:"errors"`
		ErrorPct     float64 `json:"error_pct"`
		RecoveryMs   float64 `json:"recovery_ms"`   // kill → last error (0 if no errors)
		ReadmitMs    float64 `json:"readmit_ms"`    // revive → first response from be2
		P99BeforeMs  float64 `json:"p99_before_ms"` // [0, kill)
		P99AfterMs   float64 `json:"p99_after_ms"`  // [kill, kill+5s)
		PerSecondErr []int   `json:"per_second_errors"`
		KillOffsetMs float64 `json:"kill_offset_ms"` // when be2 was really stopped, relative to the first request
	}
	var out []fo
	s := scenario{Name: "failover", Path: "/small", KeepAlive: true, Rate: rate}
	for _, t := range failoverTargets {
		for run := 1; run <= runs; run++ {
			h.quiet("docker", "kill", "-s", "USR2", "be2") // make sure be2 is up
			time.Sleep(7 * time.Second)                    // past any ejection / fail_timeout from the last run
			h.drainTimeWait()
			// The kill and revive are timed from when the load generator's
			// container is actually running, and their real wall-clock times are
			// recorded: container start-up takes seconds and would otherwise skew
			// every offset (it did in the first full run).
			var wg sync.WaitGroup
			var killedAt, revivedAt time.Time
			wg.Add(1)
			go func() {
				defer wg.Done()
				t0 := h.waitForVegeta()
				time.Sleep(time.Until(t0.Add(kill)))
				killedAt = time.Now()
				h.quiet("docker", "kill", "-s", "USR1", "be2")
				time.Sleep(time.Until(t0.Add(revive)))
				revivedAt = time.Now()
				h.quiet("docker", "kill", "-s", "USR2", "be2")
			}()
			id := "run" + strconv.Itoa(run)
			h.attackURL(s, t, t.url+s.Path, total, id)
			wg.Wait()
			file := filepath.Join(h.out, "raw", fmt.Sprintf("failover-%s-%s.bin", t.name, id))
			reqs := decode(file)
			r := fo{Target: t.name, Run: run, Requests: len(reqs), PerSecondErr: make([]int, int(total.Seconds())+2)}
			var before, after []float64
			t0 := killedAt.Add(-kill)
			if len(reqs) > 0 {
				t0 = reqs[0].At
			}
			readmit := -1.0
			for _, q := range reqs {
				off := q.At.Sub(t0)
				sinceKill, sinceRevive := q.At.Sub(killedAt), q.At.Sub(revivedAt)
				if q.Code != 200 {
					r.Errors++
					if sec := int(off.Seconds()); sec >= 0 && sec < len(r.PerSecondErr) {
						r.PerSecondErr[sec]++
					}
					if sinceKill >= 0 {
						r.RecoveryMs = math.Max(r.RecoveryMs, float64(sinceKill.Milliseconds()))
					}
				}
				switch {
				case sinceKill < 0:
					before = append(before, q.LatencyMs)
				case sinceKill < 5*time.Second:
					after = append(after, q.LatencyMs)
				}
				if sinceRevive >= 0 && q.Backend == "be2" && readmit < 0 {
					readmit = float64(sinceRevive.Milliseconds())
				}
			}
			r.KillOffsetMs = float64(killedAt.Sub(t0).Milliseconds())
			r.ReadmitMs = readmit
			if r.Requests > 0 {
				r.ErrorPct = 100 * float64(r.Errors) / float64(r.Requests)
			}
			r.P99BeforeMs, r.P99AfterMs = percentile(before, 0.99), percentile(after, 0.99)
			log.Printf("failover %-10s run %d: %d/%d errors (%.3f%%), recovery %.0fms, readmit %.0fms, p99 %.2f→%.2fms",
				t.name, run, r.Errors, r.Requests, r.ErrorPct, r.RecoveryMs, r.ReadmitMs, r.P99BeforeMs, r.P99AfterMs)
			out = append(out, r)
		}
	}
	h.writeJSON("failover.json", out)
}

// waitForVegeta returns once the load generator's container is running,
// which is when the attack starts (to within the 50 ms poll).
func (h *harness) waitForVegeta() time.Time {
	for i := 0; i < 400; i++ {
		if strings.Contains(h.output("docker", "ps", "--filter", "ancestor="+image, "--format", "{{.Command}}"), "vegeta") {
			return time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Now()
}

type req struct {
	At        time.Time
	Code      int
	LatencyMs float64
	Backend   string
}

// decode reads per-request results via `vegeta encode -to json`.
func decode(bin string) []req {
	cmd := exec.Command(vegetaBin(), "encode", "-to", "json", bin) //nolint:gosec
	b, err := cmd.Output()
	must(err)
	var out []req
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var r struct {
			Timestamp time.Time           `json:"timestamp"`
			Code      int                 `json:"code"`
			Latency   int64               `json:"latency"`
			Headers   map[string][]string `json:"headers"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		be := ""
		if v := r.Headers["X-Backend"]; len(v) > 0 {
			be = v[0]
		}
		out = append(out, req{r.Timestamp, r.Code, float64(r.Latency) / 1e6, be})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[int(math.Ceil(p*float64(len(s))))-1]
}

// hostLoad1 is the Mac's 1-minute load average ("{ 1.37 1.74 1.97 }").
func hostLoad1() float64 {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func hostInfo() map[string]string {
	out := func(args ...string) string {
		b, _ := exec.Command(args[0], args[1:]...).Output() //nolint:gosec
		return strings.TrimSpace(string(b))
	}
	return map[string]string{
		"cpu":    out("sysctl", "-n", "machdep.cpu.brand_string"),
		"docker": out("docker", "info", "--format", "{{.ServerVersion}} cpus={{.NCPU}} mem={{.MemTotal}} kernel={{.KernelVersion}}"),
		"go":     out("go", "version"),
		"load":   out("sysctl", "-n", "vm.loadavg"),
	}
}

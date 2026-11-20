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
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	image   = "portcullis-bench"
	network = "pbench"
	nginx   = "nginx:1.27.5-alpine"
)

// target is one system under test.
type target struct {
	name      string // in reports
	container string // whose CPU/memory we sample
	url       string // base URL inside the bench network
}

var targets = []target{
	{"direct", "be1", "http://be1:9000"},
	{"portcullis", "pc", "http://pc:8080"},
	{"httputil", "rp", "http://rp:8080"},
	{"nginx", "nx", "http://nx:8080"},
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
	CPUPct     float64  `json:"cpu_pct"` // mean of docker stats samples, 100 = one core
	MemMiB     float64  `json:"mem_mib"` // peak
	Errors     []string `json:"errors,omitempty"`
}

func main() {
	var (
		out      = flag.String("out", filepath.Join("bench", "results", time.Now().UTC().Format("20060102-150405")), "output directory")
		duration = flag.Duration("duration", 10*time.Second, "length of each measured run")
		runs     = flag.Int("runs", 3, "runs per scenario (medians are reported)")
		cpus     = flag.String("cpus", "2", "CPU quota for every target and the load generator")
		only     = flag.String("targets", "direct,portcullis,httputil,nginx", "targets to run")
		rates    = flag.String("rates", "2000,6000", "open-loop rates for /small (req/s)")
		quick    = flag.Bool("quick", false, "small matrix (for optimisation rounds)")
		failover = flag.Bool("failover", true, "run the failure-injection scenario")
		profile  = flag.Duration("profile", 0, "if > 0, capture a Portcullis CPU profile of this length during the first throughput run")
		build    = flag.Bool("build", true, "build the bench image first")
	)
	flag.Parse()
	must(os.MkdirAll(filepath.Join(*out, "raw"), 0o750))
	abs, err := filepath.Abs(*out)
	must(err)
	h := &harness{out: abs, dur: *duration, cpus: *cpus}

	if *build {
		h.sh("docker", "build", "-q", "-t", image, "-f", "bench/Dockerfile", ".")
	}
	h.up()
	defer h.down()

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
		"started": time.Now().UTC(), "duration": duration.String(), "runs": *runs, "cpus": *cpus,
		"scenarios": scenarios, "host": hostInfo(), "commit": strings.TrimSpace(h.output("git", "rev-parse", "--short", "HEAD")),
	})

	var all []result
	profiled := false
	for _, s := range scenarios {
		for _, t := range targets {
			if !selected[t.name] {
				continue
			}
			h.attack(s, t, 2*time.Second, "warmup") // discarded: fills connection pools, warms caches
			for i := 1; i <= *runs; i++ {
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
			}
		}
	}
	h.writeJSON("results.json", all)
	h.summarise(scenarios, all)
	if *failover {
		h.failover(*runs)
	}
	log.Printf("done: %s", abs)
}

type harness struct {
	out  string
	dur  time.Duration
	cpus string
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

func (h *harness) output(args ...string) string {
	b, _ := exec.Command(args[0], args[1:]...).Output() //nolint:gosec // fixed commands
	return string(b)
}

func (h *harness) quiet(args ...string) { _ = exec.Command(args[0], args[1:]...).Run() } //nolint:gosec

var containers = []string{"be1", "be2", "pc", "pcf", "rp", "nx"}

func (h *harness) up() {
	h.down()
	h.quiet("docker", "network", "create", network)
	run := func(name string, cmd ...string) {
		args := append([]string{"run", "-d", "--name", name, "--network", network, "--cpus", h.cpus}, cmd...)
		h.sh(append([]string{"docker"}, args...)...)
	}
	run("be1", image, "backend", "-name", "be1")
	run("be2", image, "backend", "-name", "be2")
	run("pc", "-p", "127.0.0.1:19901:9901", image, "portcullis", "-config", "/etc/portcullis/portcullis.yaml",
		"-listen", ":8080", "-admin-listen", ":9901", "-pprof", "-log-level", "warn")
	run("pcf", image, "portcullis", "-config", "/etc/portcullis/portcullis-failover.yaml", "-listen", ":8080",
		"-admin-listen", ":9901", "-log-level", "warn")
	run("rp", image, "rproxy", "-upstream", "http://be1:9000")
	conf, err := filepath.Abs("bench/nginx/nginx.conf")
	must(err)
	run("nx", "-v", conf+":/etc/nginx/nginx.conf:ro", nginx)
	// Wait until every target answers.
	for _, t := range append(targets, target{"failover-portcullis", "pcf", "http://pcf:8080"}, target{"failover-nginx", "nx", "http://nx:8081"}) {
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
	file := fmt.Sprintf("%s-%s-%s", s.Name, t.name, id)
	must(os.WriteFile(filepath.Join(h.out, "raw", file+".targets"), []byte("GET "+url+"\n"), 0o600))
	args := []string{"run", "--rm", "--user", "0", "--network", network, "--cpus", h.cpus,
		"-v", h.out + "/raw:/out", image, "vegeta", "attack",
		"-targets=/out/" + file + ".targets", "-duration=" + d.String(), "-timeout=10s",
		"-keepalive=" + strconv.FormatBool(s.KeepAlive), "-output=/out/" + file + ".bin"}
	if s.Workers > 0 {
		w := strconv.Itoa(s.Workers)
		args = append(args, "-rate=0", "-workers="+w, "-max-workers="+w)
	} else {
		args = append(args, "-rate="+strconv.Itoa(s.Rate), "-max-workers=2048")
	}
	h.sh(append([]string{"docker"}, args...)...)
	r := report(filepath.Join(h.out, "raw", file+".bin"))
	r.Scenario, r.Target = s.Name, t.name
	if id == "warmup" || id == "ready" {
		_ = os.Remove(filepath.Join(h.out, "raw", file+".bin"))
		_ = os.Remove(filepath.Join(h.out, "raw", file+".targets"))
	}
	return r
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
	h.writeJSON(filepath.Join("raw", fmt.Sprintf("%s-%s-run%d.stats.json", s.Name, t.name, run)), st)
	return r
}

// report parses `vegeta report -type=json`.
func report(bin string) result {
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
		P50ms: ms(rep.Latencies.P50), P95ms: ms(rep.Latencies.P95), P99ms: ms(rep.Latencies.P99), MaxMs: ms(rep.Latencies.Max), Errors: errs}
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
	csv.WriteString("scenario,target,runs,throughput_rps,success,p50_ms,p95_ms,p99_ms,max_ms,cpu_pct,mem_mib\n")
	md.WriteString("| scenario | target | req/s | success | p50 ms | p95 ms | p99 ms | CPU % | mem MiB |\n|---|---|--:|--:|--:|--:|--:|--:|--:|\n")
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
			fmt.Fprintf(&csv, "%s,%s,%d,%.0f,%.4f,%.3f,%.3f,%.3f,%.3f,%.0f,%.1f\n", m.Scenario, m.Target, m.Run,
				m.Throughput, m.Success, m.P50ms, m.P95ms, m.P99ms, m.MaxMs, m.CPUPct, m.MemMiB)
			fmt.Fprintf(&md, "| %s | %s | %.0f | %.4f | %.2f | %.2f | %.2f | %.0f | %.1f |\n", m.Scenario, m.Target,
				m.Throughput, m.Success, m.P50ms, m.P95ms, m.P99ms, m.CPUPct, m.MemMiB)
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
		url := fmt.Sprintf("http://127.0.0.1:19901/debug/pprof/profile?seconds=%d", int(d.Seconds()))
		resp, err := http.Get(url) //nolint:gosec,noctx // fixed local admin URL
		if err != nil {
			log.Printf("profile capture failed: %v", err)
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 200 {
			log.Printf("profile capture failed: %d %v", resp.StatusCode, err)
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
	}
	var out []fo
	s := scenario{Name: "failover", Path: "/small", KeepAlive: true, Rate: rate}
	for _, t := range []target{{"portcullis", "pcf", "http://pcf:8080"}, {"nginx", "nx", "http://nx:8081"}} {
		for run := 1; run <= runs; run++ {
			h.quiet("docker", "kill", "-s", "USR2", "be2") // make sure be2 is up
			time.Sleep(7 * time.Second)                    // past any ejection / fail_timeout from the last run
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(kill)
				h.quiet("docker", "kill", "-s", "USR1", "be2")
				time.Sleep(revive - kill)
				h.quiet("docker", "kill", "-s", "USR2", "be2")
			}()
			id := "run" + strconv.Itoa(run)
			start := time.Now()
			h.attackURL(s, t, t.url+s.Path, total, id)
			wg.Wait()
			file := filepath.Join(h.out, "raw", fmt.Sprintf("failover-%s-%s.bin", t.name, id))
			reqs := decode(file)
			r := fo{Target: t.name, Run: run, Requests: len(reqs), PerSecondErr: make([]int, int(total.Seconds())+2)}
			var before, after []float64
			t0 := start
			if len(reqs) > 0 {
				t0 = reqs[0].At
			}
			readmit := -1.0
			for _, q := range reqs {
				off := q.At.Sub(t0)
				if q.Code != 200 {
					r.Errors++
					if sec := int(off.Seconds()); sec < len(r.PerSecondErr) {
						r.PerSecondErr[sec]++
					}
					if off >= kill {
						r.RecoveryMs = math.Max(r.RecoveryMs, float64((off - kill).Milliseconds()))
					}
				}
				switch {
				case off < kill:
					before = append(before, q.LatencyMs)
				case off < kill+5*time.Second:
					after = append(after, q.LatencyMs)
				}
				if off >= revive && q.Backend == "be2" && readmit < 0 {
					readmit = float64((off - revive).Milliseconds())
				}
			}
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

func hostInfo() map[string]string {
	out := func(args ...string) string {
		b, _ := exec.Command(args[0], args[1:]...).Output() //nolint:gosec
		return strings.TrimSpace(string(b))
	}
	return map[string]string{
		"cpu":    out("sysctl", "-n", "machdep.cpu.brand_string"),
		"docker": out("docker", "info", "--format", "{{.ServerVersion}} cpus={{.NCPU}} mem={{.MemTotal}} kernel={{.KernelVersion}}"),
		"go":     out("go", "version"),
	}
}

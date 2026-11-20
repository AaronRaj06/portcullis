package config

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AaronRaj06/portcullis/internal/proxy"
)

const minimal = `
pools:
  - name: p
    backends: [{address: 127.0.0.1:9000}]
routes:
  - name: r
    backends: [{pool: p}]
`

func mustParse(t *testing.T, s string) *Config {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseIsStrict(t *testing.T) {
	_, err := Parse([]byte(minimal + "\nrotues: []\n"))
	if err == nil || !strings.Contains(err.Error(), "rotues") {
		t.Fatalf("typo'd field accepted or not named: %v", err)
	}
	if _, err := Parse([]byte(strings.Repeat(" ", MaxSize+1))); err == nil {
		t.Fatal("oversized config accepted")
	}
}

func TestDurations(t *testing.T) {
	c := mustParse(t, strings.Replace(minimal, "backends: [{address", "healthCheck: {interval: 250ms, timeout: 100ms}\n    backends: [{address", 1))
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(c.Pools[0].HealthCheck.Interval); got != 250*time.Millisecond {
		t.Fatalf("interval = %s", got)
	}
	if _, err := Parse([]byte(strings.Replace(minimal, "backends: [{address", "healthCheck: {interval: 5}\n    backends: [{address", 1))); err == nil {
		t.Fatal("bare number accepted as a duration")
	}
}

func TestDefaults(t *testing.T) {
	c := mustParse(t, minimal)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	p, r := c.Pools[0], c.Routes[0]
	if p.Panic == nil || !*p.Panic {
		t.Error("panic mode must default to on (ADR 0009)")
	}
	if p.Scheme != "http" || p.LB != "round_robin" || p.Backends[0].Weight != 1 || p.Ejection.After != 5 ||
		p.Retries.Attempts != 2 || p.HealthCheck != nil || r.Path != "/" || r.PathType != "Prefix" || *r.Backends[0].Weight != 1 {
		t.Fatalf("defaults not applied: %+v %+v", p, r)
	}
}

func TestValidationReportsEveryProblemWithItsPath(t *testing.T) {
	c := mustParse(t, `
trustedProxies: [nope]
pools:
  - name: p
    scheme: ftp
    lb: random
    backends:
      - address: noport
      - {address: "10.0.0.1:99999", weight: 5000}
  - name: p
    lb: hash
    hashKey: "header:"
    backends: []
    healthCheck: {path: healthz, interval: 1ms}
  - name: q
    tls: {caFile: /x}
    backends: [{address: a:1}]
routes:
  - name: r
    path: api
    pathType: Regex
    hosts: ["a.*.com"]
    backends: [{pool: missing}, {pool: p, weight: 0}]
  - name: r
    backends: [{pool: p, weight: 0}]
`)
	err := c.Validate()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	all := strings.Join(ve.Problems, "\n")
	for _, want := range []string{
		"trustedProxies[0]", "pools[0].scheme", "pools[0].lb", "pools[0].backends[0].address",
		"pools[0].backends[1].address", "pools[0].backends[1].weight", "pools[1].name: duplicate",
		"pools[1].hashKey", "pools[1].backends: at least one", "pools[1].healthCheck.path", "pools[1].healthCheck.interval",
		"pools[2].tls: only valid with scheme: https", "routes[0].path", "routes[0].pathType", "routes[0].hosts[0]",
		`routes[0].backends[0].pool: unknown pool "missing"`, "routes[1].name: duplicate", "routes[1].backends: all weights are 0",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing problem %q in:\n%s", want, all)
		}
	}
}

func TestHashIgnoresFormattingButNotMeaning(t *testing.T) {
	yamlCfg := mustParse(t, minimal)
	jsonCfg := mustParse(t, `{"routes":[{"backends":[{"pool":"p"}],"name":"r"}],"pools":[{"name":"p","backends":[{"address":"127.0.0.1:9000","weight":1}]}]}`)
	for _, c := range []*Config{yamlCfg, jsonCfg} {
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if yamlCfg.Hash() != jsonCfg.Hash() {
		t.Fatal("same config in YAML and JSON (one with explicit defaults) hashed differently")
	}
	other := mustParse(t, strings.Replace(minimal, "9000", "9001", 1))
	_ = other.Validate()
	if other.Hash() == yamlCfg.Hash() {
		t.Fatal("different configs share a hash")
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../docs/examples/portcullis.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(c, proxy.DefaultLimits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(compiled.Snapshot.Router.Pools()); n != 3 {
		t.Fatalf("example compiled to %d pools, want 3", n)
	}
}

func TestCompileTransports(t *testing.T) {
	ca := writeCA(t)
	c := mustParse(t, `
pools:
  - {name: grpc, scheme: h2c, backends: [{address: "10.0.0.1:50051"}]}
  - {name: secure, scheme: https, tls: {caFile: "`+ca+`", serverName: api.internal}, backends: [{address: "10.0.0.2:443"}]}
  - {name: plain, backends: [{address: "10.0.0.3:80"}]}
routes:
  - {name: g, path: /grpc, backends: [{pool: grpc}]}
  - {name: s, path: /s, backends: [{pool: secure}]}
  - {name: p, backends: [{pool: plain}]}
`)
	compiled, err := Compile(c, proxy.DefaultLimits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]bool{}
	for _, p := range compiled.Snapshot.Router.Pools() {
		byName[p.Name] = p.Transport != nil
		if p.Name == "grpc" && p.Scheme != "http" {
			t.Errorf("h2c pool scheme %q, want http", p.Scheme)
		}
	}
	if !byName["grpc"] || !byName["secure"] || byName["plain"] || len(compiled.Transports) != 2 {
		t.Fatalf("transports per pool %v (%d built)", byName, len(compiled.Transports))
	}
}

func TestReloadKeepsBackendState(t *testing.T) {
	v1 := mustParse(t, `
pools: [{name: p, backends: [{address: "10.0.0.1:80"}, {address: "10.0.0.2:80"}]}]
routes: [{name: r, backends: [{pool: p}]}]`)
	c1, err := Compile(v1, proxy.DefaultLimits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c1.Snapshot.Router.Pools()[0].Backends[0].SetHealthy(false)
	v2 := mustParse(t, `
pools: [{name: p, lb: least_conn, backends: [{address: "10.0.0.1:80"}, {address: "10.0.0.3:80"}]}]
routes: [{name: r, backends: [{pool: p}]}]`)
	c2, err := Compile(v2, proxy.DefaultLimits, c1.Snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	bs := c2.Snapshot.Router.Pools()[0].Backends
	if bs[0].Healthy() {
		t.Error("10.0.0.1 was down before the reload and came back healthy")
	}
	if !bs[1].Healthy() {
		t.Error("new backend 10.0.0.3 should start healthy")
	}
}

func TestManagerApply(t *testing.T) {
	h, err := proxy.New(proxy.Options{Transport: proxy.NewTransport(proxy.DefaultLimits), Limits: proxy.DefaultLimits})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), h, proxy.DefaultLimits, "", nil, nil)
	st, err := m.Apply([]byte(minimal), "test")
	if err != nil || st.Generation != 1 || !st.Changed {
		t.Fatalf("first apply: %+v %v", st, err)
	}
	first := h.Current()

	if st, err := m.Apply([]byte(minimal+"\n# only a comment differs\n"), "test"); err != nil || st.Changed || st.Generation != 1 || h.Current() != first {
		t.Fatalf("identical config must be a no-op: %+v %v", st, err)
	}
	if _, err := m.Apply([]byte("pools: []\nroutes: []"), "test"); err == nil {
		t.Fatal("invalid config accepted")
	}
	if h.Current() != first {
		t.Fatal("invalid config replaced the serving snapshot")
	}
	if _, err := m.Apply([]byte("{not yaml"), "test"); err == nil || h.Current() != first {
		t.Fatal("unparseable config accepted or swapped in")
	}
	st, err = m.Apply([]byte(strings.Replace(minimal, "9000", "9001", 1)), "test")
	if err != nil || st.Generation != 2 || h.Current() == first || h.Current().Generation != 2 {
		t.Fatalf("second apply: %+v %v", st, err)
	}
	if _, err := m.Reload(); err == nil {
		t.Fatal("Reload without a file must fail")
	}
}

// Health checks belong to the snapshot: after a reload the old ones stop.
func TestManagerRestartsHealthChecks(t *testing.T) {
	var oldChecks, newChecks atomic.Int64
	srv := func(n *atomic.Int64) string {
		s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { n.Add(1) }))
		t.Cleanup(s.Close)
		return strings.TrimPrefix(s.URL, "http://")
	}
	oldAddr, newAddr := srv(&oldChecks), srv(&newChecks)
	h, _ := proxy.New(proxy.Options{Transport: proxy.NewTransport(proxy.DefaultLimits), Limits: proxy.DefaultLimits})
	m := NewManager(context.Background(), h, proxy.DefaultLimits, "", nil, nil)
	cfg := func(addr string) []byte {
		return []byte("pools: [{name: p, healthCheck: {interval: 10ms, timeout: 5ms}, backends: [{address: \"" + addr + "\"}]}]\nroutes: [{name: r, backends: [{pool: p}]}]")
	}
	if _, err := m.Apply(cfg(oldAddr), "t"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := m.Apply(cfg(newAddr), "t"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // a check already in flight may still land
	before := oldChecks.Load()
	time.Sleep(100 * time.Millisecond)
	if oldChecks.Load() != before || before == 0 {
		t.Fatalf("old snapshot's checks: %d before, %d after reload; want >0 and then no more", before, oldChecks.Load())
	}
	if newChecks.Load() == 0 {
		t.Fatal("new snapshot's checks never ran")
	}
}

func writeCA(t *testing.T) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHashCoversReferencedFiles(t *testing.T) {
	ca := writeCA(t)
	c := mustParse(t, `
pools: [{name: s, scheme: https, tls: {caFile: "`+ca+`"}, backends: [{address: "10.0.0.2:443"}]}]
routes: [{name: r, backends: [{pool: s}]}]`)
	_ = c.Validate()
	before := c.Hash()
	other := writeCA(t)
	data, _ := os.ReadFile(other)
	if err := os.WriteFile(ca, data, 0o600); err != nil { // rotate the CA in place
		t.Fatal(err)
	}
	if c.Hash() == before {
		t.Fatal("replacing the CA file's contents did not change the config hash")
	}
}

func TestTransportsReusedWhenSettingsUnchanged(t *testing.T) {
	ca := writeCA(t)
	cfg := func(route string) *Config {
		return mustParse(t, `
pools:
  - {name: g, scheme: h2c, backends: [{address: "10.0.0.1:50051"}]}
  - {name: s, scheme: https, tls: {caFile: "`+ca+`"}, backends: [{address: "10.0.0.2:443"}]}
routes:
  - {name: `+route+`, backends: [{pool: g}]}
  - {name: other, path: /s, backends: [{pool: s}]}`)
	}
	c1, err := Compile(cfg("r1"), proxy.DefaultLimits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Compile(cfg("r2"), proxy.DefaultLimits, c1.Snapshot, c1.Transports)
	if err != nil {
		t.Fatal(err)
	}
	for key, tr := range c1.Transports {
		if c2.Transports[key] != tr {
			t.Errorf("transport %q rebuilt although its settings did not change", key)
		}
	}
	other := writeCA(t)
	data, _ := os.ReadFile(other)
	_ = os.WriteFile(ca, data, 0o600)
	c3, err := Compile(cfg("r3"), proxy.DefaultLimits, c2.Snapshot, c2.Transports)
	if err != nil {
		t.Fatal(err)
	}
	if c3.Transports["h2c"] != c1.Transports["h2c"] {
		t.Error("h2c transport rebuilt although only the CA changed")
	}
	reusedTLS := false
	for key, tr := range c3.Transports {
		if key != "h2c" && tr == c2.Transports[key] {
			reusedTLS = true
		}
	}
	if reusedTLS || len(c3.Transports) != 2 {
		t.Error("TLS transport must be rebuilt when its CA contents change")
	}
}

func TestPanicCanBeTurnedOff(t *testing.T) {
	c := mustParse(t, strings.Replace(minimal, "backends: [{address", "panic: false\n    backends: [{address", 1))
	compiled, err := Compile(c, proxy.DefaultLimits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Snapshot.Router.Pools()[0].Panic {
		t.Fatal("explicit panic: false ignored")
	}
}

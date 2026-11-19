package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "portcullis ") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("pools: []\nroutes: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{},                       // neither -config nor -upstream
		{"-upstream", "ftp://x"}, // bad scheme
		{"-upstream", "http://x:1", "-trusted-proxies", "nope"},            // bad CIDR
		{"-upstream", "http://x:1", "-tls-cert", "/missing"},               // unreadable cert
		{"-upstream", "http://a:1,https://b:2"},                            // mixed schemes
		{"-upstream", "http://a:1/base"},                                   // path not supported
		{"-upstream", "http://a:1", "-lb", "random"},                       // unknown algorithm
		{"-upstream", "http://a:1", "-lb", "hash", "-hash-key", "header:"}, // empty header name
		{"-upstream", "http://a:1=0"},                                      // weight out of range
		{"-upstream", "http://a:1=x"},                                      // weight not a number
		{"-upstream", "http://a:1", "-upstream-ca", "/x"},                  // CA needs https
		{"-upstream", "https://a:1", "-upstream-ca", "/missing"},           // unreadable CA
		{"-upstream", "http://a:1", "-config", bad},                        // both sources
		{"-config", "/missing.yaml"},                                       // unreadable config
		{"-config", bad},                                                   // invalid config
		{"-upstream", "http://a:1", "-admin-token-file", "/missing"},       // unreadable token
	} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("run(%q) = nil, want error", args)
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func namedBackend(t *testing.T, name string) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, name) }))
	t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "http://")
}

func configFor(version, addr string) string {
	return fmt.Sprintf("version: %s\npools:\n  - name: p\n    backends:\n      - address: %s\nroutes:\n  - name: r\n    backends: [{pool: p}]\n", version, addr)
}

func get(t *testing.T, url string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if len(hdr) == 2 {
		req.Header.Set(hdr[0], hdr[1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// End to end through run(): file config, SIGHUP reload, a rejected bad file,
// and an authenticated PUT /config.
func TestRunReloadEndToEnd(t *testing.T) {
	a, b, c := namedBackend(t, "A"), namedBackend(t, "B"), namedBackend(t, "C")
	dir := t.TempDir()
	cfgPath, tokPath := filepath.Join(dir, "portcullis.yaml"), filepath.Join(dir, "token")
	write := func(s string) {
		if err := os.WriteFile(cfgPath, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(configFor("v1", a))
	if err := os.WriteFile(tokPath, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyAddr, adminAddr := freeAddr(t), freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var logs syncBuffer
	go func() {
		done <- run(ctx, []string{"-config", cfgPath, "-listen", proxyAddr, "-admin-listen", adminAddr,
			"-admin-token-file", tokPath, "-drain-timeout", "2s"}, &logs)
	}()
	base, adm := "http://"+proxyAddr, "http://"+adminAddr
	eventually(t, "readyz", func() bool { code, _ := get(t, adm+"/readyz"); return code == 200 })
	if _, body := get(t, base+"/"); body != "A" {
		t.Fatalf("v1 served %q, want A", body)
	}

	write(configFor("v2", b))
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	eventually(t, "SIGHUP reload to v2", func() bool { _, body := get(t, base+"/"); return body == "B" })

	write("pools: [{name: p, backends: [{address: nope}]}]\nroutes: []\n")
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	eventually(t, "rejection logged", func() bool { return strings.Contains(logs.String(), "SIGHUP reload failed") })
	if _, body := get(t, base+"/"); body != "B" {
		t.Fatalf("after a bad file, served %q; the good config must keep serving", body)
	}

	if code, _ := get(t, adm+"/config"); code != http.StatusUnauthorized {
		t.Fatalf("GET /config without token: %d, want 401", code)
	}
	code, body := get(t, adm+"/config", "Authorization", "Bearer s3cret")
	var st struct {
		Version    string `json:"version"`
		Generation uint64 `json:"generation"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil || code != 200 || st.Version != "v2" || st.Generation != 2 {
		t.Fatalf("GET /config: %d %s", code, body)
	}

	req, _ := http.NewRequest(http.MethodPut, adm+"/config", strings.NewReader(configFor("v3", c)))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT /config: %d", resp.StatusCode)
	}
	if _, body := get(t, base+"/"); body != "C" {
		t.Fatalf("after PUT served %q, want C", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

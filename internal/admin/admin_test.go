package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AaronRaj06/portcullis/internal/config"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

const good = `{"version":"v1","pools":[{"name":"p","backends":[{"address":"127.0.0.1:9000"}]}],"routes":[{"name":"r","backends":[{"pool":"p"}]}]}`

func setup(t *testing.T, token string, pprof bool) (*Server, *proxy.Handler, *httptest.Server) {
	t.Helper()
	h, err := proxy.New(proxy.Options{Transport: proxy.NewTransport(proxy.DefaultLimits), Limits: proxy.DefaultLimits})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Manager: config.NewManager(context.Background(), h, proxy.DefaultLimits, "", nil, nil),
		Gatherer: prometheus.NewRegistry(), Token: token, Pprof: pprof}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, h, srv
}

func call(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestReadiness(t *testing.T) {
	s, _, srv := setup(t, "", false)
	if code, _ := call(t, "GET", srv.URL+"/readyz", "", ""); code != 503 {
		t.Fatalf("ready before any config: %d", code)
	}
	if code, _ := call(t, "GET", srv.URL+"/healthz", "", ""); code != 200 {
		t.Fatal("liveness must not depend on config")
	}
	if code, body := call(t, "PUT", srv.URL+"/config", "", good); code != 200 {
		t.Fatalf("PUT good config: %d %s", code, body)
	}
	if code, _ := call(t, "GET", srv.URL+"/readyz", "", ""); code != 200 {
		t.Fatalf("not ready after a config: %d", code)
	}
	s.SetDraining()
	if code, _ := call(t, "GET", srv.URL+"/readyz", "", ""); code != 503 {
		t.Fatalf("ready while draining: %d", code)
	}
}

func TestAuth(t *testing.T) {
	_, _, srv := setup(t, "s3cret", true)
	for _, ep := range []struct{ method, path string }{
		{"GET", "/config"}, {"PUT", "/config"}, {"POST", "/reload"}, {"GET", "/debug/pprof/"},
	} {
		for _, tok := range []string{"", "wrong", "s3cre"} {
			if code, _ := call(t, ep.method, srv.URL+ep.path, tok, good); code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: %d, want 401", ep.method, ep.path, tok, code)
			}
		}
	}
	for _, path := range []string{"/healthz", "/metrics"} {
		if code, _ := call(t, "GET", srv.URL+path, "", ""); code != 200 {
			t.Errorf("%s needs no token, got %d", path, code)
		}
	}
	if code, _ := call(t, "PUT", srv.URL+"/config", "s3cret", good); code != 200 {
		t.Fatalf("PUT with the right token: %d", code)
	}
	if code, _ := call(t, "GET", srv.URL+"/debug/pprof/", "s3cret", ""); code != 200 {
		t.Fatalf("pprof with token: %d", code)
	}
}

func TestPprofOffByDefault(t *testing.T) {
	_, _, srv := setup(t, "", false)
	if code, _ := call(t, "GET", srv.URL+"/debug/pprof/", "", ""); code != 404 {
		t.Fatalf("pprof served while disabled: %d", code)
	}
}

func TestPutRejectsBadConfigAndKeepsServing(t *testing.T) {
	_, h, srv := setup(t, "", false)
	if code, _ := call(t, "PUT", srv.URL+"/config", "", good); code != 200 {
		t.Fatal("seed config rejected")
	}
	before := h.Current()
	bad := `{"pools":[{"name":"p","backends":[{"address":"nope"}]}],"routes":[{"name":"r","backends":[{"pool":"q"}]}]}`
	code, body := call(t, "PUT", srv.URL+"/config", "", bad)
	if code != 400 {
		t.Fatalf("bad config: %d", code)
	}
	var resp struct {
		Problems []string      `json:"problems"`
		Current  config.Status `json:"current"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.Problems, "\n")
	if !strings.Contains(joined, "pools[0].backends[0].address") || !strings.Contains(joined, "routes[0].backends[0].pool") {
		t.Fatalf("problems not reported with paths: %s", joined)
	}
	if resp.Current.Version != "v1" || h.Current() != before {
		t.Fatal("bad config replaced the serving one")
	}
	if code, _ := call(t, "POST", srv.URL+"/reload", "", ""); code != 400 {
		t.Fatalf("reload without a config file: %d, want 400", code)
	}
}

func TestGetConfig(t *testing.T) {
	_, _, srv := setup(t, "", false)
	call(t, "PUT", srv.URL+"/config", "", good)
	_, body := call(t, "GET", srv.URL+"/config", "", "")
	var st config.Status
	if err := json.Unmarshal([]byte(body), &st); err != nil || st.Version != "v1" || len(st.Hash) != 64 || st.Generation != 1 || st.Source != "api" {
		t.Fatalf("GET /config = %s", body)
	}
	if _, full := call(t, "GET", srv.URL+"/config?full=1", "", ""); !strings.Contains(full, `"round_robin"`) {
		t.Fatalf("full config missing (defaults should be visible): %s", full)
	}
}

func TestTokenFromFile(t *testing.T) {
	if tok, err := TokenFromFile([]byte("  abc \n")); err != nil || tok != "abc" {
		t.Fatalf("got %q %v", tok, err)
	}
	if _, err := TokenFromFile([]byte("\n")); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestIfMatchRejectsStalePushes(t *testing.T) {
	_, h, srv := setup(t, "", false)
	put := func(body, match string) (int, http.Header) {
		req, _ := http.NewRequest("PUT", srv.URL+"/config", strings.NewReader(body))
		if match != "" {
			req.Header.Set("If-Match", match)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	if code, _ := put(good, "*"); code != 412 {
		t.Fatalf("If-Match * with no config applied: %d, want 412", code)
	}
	code, hdr := put(good, "")
	etag := hdr.Get("ETag")
	if code != 200 || len(etag) != 66 {
		t.Fatalf("first push: %d etag %q", code, etag)
	}
	v2 := strings.Replace(good, "v1", "v2", 1)
	v3 := strings.Replace(good, "v1", "v3", 1)
	code, hdr = put(v2, etag) // writer A read v1 and pushes v2 over it
	if code != 200 {
		t.Fatalf("push with the current ETag: %d", code)
	}
	if code, _ := put(v3, etag); code != 412 { // writer B also read v1: stale
		t.Fatalf("push with a stale ETag: %d, want 412", code)
	}
	if h.Current().Version != "v2" {
		t.Fatalf("stale push changed the config to %q", h.Current().Version)
	}
	_, getHdr := call2(t, srv.URL+"/config")
	if getHdr.Get("ETag") != hdr.Get("ETag") {
		t.Fatalf("GET ETag %q != PUT ETag %q", getHdr.Get("ETag"), hdr.Get("ETag"))
	}
	if code, _ := put(v3, "*"); code != 200 {
		t.Fatalf("If-Match * with a config applied: %d", code)
	}
}

func call2(t *testing.T, url string) (int, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

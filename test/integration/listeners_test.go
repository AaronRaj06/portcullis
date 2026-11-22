package integration

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AaronRaj06/portcullis/internal/admin"
	"github.com/AaronRaj06/portcullis/internal/config"
	"github.com/AaronRaj06/portcullis/internal/proxy"
	"github.com/AaronRaj06/portcullis/internal/testcert"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestListenersTLSFiltersAndIsolation(t *testing.T) {
	be := backend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "set=%v add=%v gone=%v", r.Header["X-Set"], r.Header["X-Add"], r.Header["X-Gone"])
	})
	h, err := proxy.New(proxy.Options{Transport: proxy.NewTransport(proxy.DefaultLimits), Limits: proxy.DefaultLimits})
	if err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(context.Background(), h, proxy.DefaultLimits, "", nil, nil)
	mgr.Listeners = proxy.NewListeners(h, "127.0.0.1", proxy.DefaultLimits, 0)
	t.Cleanup(mgr.Listeners.Close)

	plain, secure := freePort(t), freePort(t)
	cert := func(host, name string) config.Certificate {
		c, k := testcert.New(t, name)
		return config.Certificate{Host: host, Cert: string(c), Key: string(k)}
	}
	cfg := &config.Config{
		Pools: []config.Pool{{Name: "app", Backends: []config.Backend{{Address: strings.TrimPrefix(be.URL, "http://")}}}, {Name: "broken", Status: 500}},
		Listeners: []config.Listener{{Port: plain}, {Port: secure, Certificates: []config.Certificate{
			cert("", "default.test"), cert("*.wild.test", "wild.test"), cert("exact.wild.test", "exact.test")}}},
		Routes: []config.Route{
			{Name: "edit", Port: plain, Path: "/edit", Backends: []config.PoolRef{{Pool: "app"}}, RequestHeaders: &config.HeaderEdit{
				Set: []config.HeaderMatch{{Name: "x-set", Value: "new"}}, Add: []config.HeaderMatch{{Name: "x-add", Value: "two"}}, Remove: []string{"x-gone"}}},
			{Name: "redirect", Port: plain, Path: "/go", Backends: []config.PoolRef{{Pool: "broken"}},
				Redirect: &config.Redirect{Scheme: "http", Host: "example.org", Port: 80, Status: 301}},
			{Name: "broken", Port: plain, Path: "/broken", Backends: []config.PoolRef{{Pool: "broken"}}},
			{Name: "secure", Port: secure, Path: "/only-https", Backends: []config.PoolRef{{Pool: "app"}}},
		},
	}
	if _, err := mgr.ApplyConfig(cfg, "test"); err != nil {
		t.Fatal(err)
	}

	var seen string
	c := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // the test inspects the certificate itself
			VerifyConnection: func(cs tls.ConnectionState) error {
				seen = cs.PeerCertificates[0].Subject.CommonName
				return nil
			}}},
	}
	type result struct {
		StatusCode int
		Header     http.Header
	}
	get := func(url, sni string, hdr http.Header) (result, string) {
		t.Helper()
		c.Transport.(*http.Transport).TLSClientConfig.ServerName = sni
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		for k, v := range hdr {
			req.Header[k] = v
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return result{resp.StatusCode, resp.Header}, string(b)
	}
	p, s := fmt.Sprintf("http://127.0.0.1:%d", plain), fmt.Sprintf("https://127.0.0.1:%d", secure)

	if _, body := get(p+"/edit", "", http.Header{"X-Set": {"old"}, "X-Add": {"one"}, "X-Gone": {"x"}}); body != "set=[new] add=[one two] gone=[]" {
		t.Errorf("header edit: %s", body)
	}
	if r, _ := get(p+"/go/a%20b?q=1", "", nil); r.StatusCode != 301 || r.Header.Get("Location") != "http://example.org/go/a%20b?q=1" {
		t.Errorf("redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := get(p+"/broken", "", nil); r.StatusCode != 500 {
		t.Errorf("status pool: %d", r.StatusCode)
	}
	if r, _ := get(p+"/only-https", "", nil); r.StatusCode != 404 {
		t.Errorf("a route leaked to another port: %d", r.StatusCode)
	}
	if r, _ := get(s+"/edit", "default.test", nil); r.StatusCode != 404 {
		t.Errorf("a route leaked to another port: %d", r.StatusCode)
	}
	for sni, want := range map[string]string{"exact.wild.test": "exact.test", "a.wild.test": "wild.test", "unknown.test": "default.test"} {
		if r, _ := get(s+"/only-https", sni, nil); r.StatusCode != 200 || seen != want {
			t.Errorf("sni %s: %d with certificate %s, want %s", sni, r.StatusCode, seen, want)
		}
	}

	// GET /config?full=1 must not show private keys.
	rec := httptest.NewRecorder()
	(&admin.Server{Manager: mgr}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config?full=1", nil))
	if strings.Contains(rec.Body.String(), "PRIVATE KEY") || !strings.Contains(rec.Body.String(), "REDACTED") || !json.Valid(rec.Body.Bytes()) {
		t.Error("admin API exposes a private key")
	}
	if !strings.Contains(mgr.Config().Listeners[1].Certificates[0].Key, "PRIVATE KEY") {
		t.Error("redaction changed the live config")
	}

	// A certificate rotates in place, and a removed listener stops accepting.
	next := *cfg
	next.Listeners = []config.Listener{{Port: secure, Certificates: []config.Certificate{cert("", "rotated.test")}}}
	next.Routes = cfg.Routes[3:]
	if _, err := mgr.ApplyConfig(&next, "test"); err != nil {
		t.Fatal(err)
	}
	if r, _ := get(s+"/only-https", "any.test", nil); r.StatusCode != 200 || seen != "rotated.test" {
		t.Errorf("rotation: %d %s", r.StatusCode, seen)
	}
	if conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", plain)); err == nil {
		conn.Close()
		t.Error("removed listener still accepts")
	}

	// A port may change from HTTPS to HTTP (the controller reuses freed ports).
	next.Listeners = []config.Listener{{Port: secure}}
	if _, err := mgr.ApplyConfig(&next, "test"); err != nil {
		t.Fatalf("protocol change: %v", err)
	}
	if r, _ := get(fmt.Sprintf("http://127.0.0.1:%d/only-https", secure), "", nil); r.StatusCode != 200 {
		t.Errorf("after protocol change: %d", r.StatusCode)
	}
}

package proxy

import (
	"net/http/httptest"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"//admin":              "/admin",
		"///admin//x":          "/admin/x",
		"/x/../admin":          "/admin",
		"/./admin":             "/admin",
		"/x/%2e%2e/admin":      "/admin",
		"/x%2F..%2Fadmin":      "/admin",
		"/x/../admin/":         "/admin/",
		"/../../admin":         "/admin",
		"//":                   "/",
		"/":                    "/",
		"/a/":                  "/a/",
		"/a%2Fb":               "/a%2Fb",
		"/.well-known/x":       "/.well-known/x",
		"/files/report..final": "/files/report..final",
	} {
		r := httptest.NewRequest("GET", "http://h"+in, nil)
		if got := normalizePath(r).URL.EscapedPath(); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
	r := httptest.NewRequest("GET", "http://h/already/clean/", nil)
	if n := testing.AllocsPerRun(100, func() { normalizePath(r) }); n != 0 {
		t.Errorf("clean path allocates %v times", n)
	}
}

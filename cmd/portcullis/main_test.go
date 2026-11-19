package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/AaronRaj06/portcullis/internal/proxy"
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
	for _, args := range [][]string{
		{},                       // no upstream
		{"-upstream", "ftp://x"}, // bad scheme
		{"-upstream", "http://x", "-trusted-proxies", "nope"},              // bad CIDR
		{"-upstream", "http://x", "-tls-cert", "/missing"},                 // unreadable cert
		{"-upstream", "http://a:1,https://b:2"},                            // mixed schemes
		{"-upstream", "http://a:1/base"},                                   // path not supported
		{"-upstream", "http://a:1", "-lb", "random"},                       // unknown algorithm
		{"-upstream", "http://a:1", "-lb", "hash", "-hash-key", "header:"}, // empty header name
		{"-upstream", "http://a:1=0"},                                      // weight out of range
		{"-upstream", "http://a:1=x"},                                      // weight not a number
		{"-upstream", "http://a:1", "-upstream-ca", "/x"},                  // CA needs https
		{"-upstream", "https://a:1", "-upstream-ca", "/missing"},           // unreadable CA
	} {
		if err := run(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%q) = nil, want error", args)
		}
	}
}

func TestBuildPool(t *testing.T) {
	p, err := buildPool("http://a:1=3, http://b:2", "weighted_round_robin", "ip", "", proxy.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Backends) != 2 || p.Backends[0].Weight != 3 || p.Backends[1].Weight != 1 || p.Scheme != "http" || p.Transport != nil {
		t.Fatalf("unexpected pool %+v", p)
	}
	h, err := buildPool("h2c://a:1", "round_robin", "ip", "", proxy.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if h.Scheme != "http" || h.Transport == nil {
		t.Fatal("h2c upstream must use http scheme with a dedicated HTTP/2 transport")
	}
}

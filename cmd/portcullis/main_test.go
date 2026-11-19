package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
		{"-upstream", "http://x", "-trusted-proxies", "nope"}, // bad CIDR
		{"-upstream", "http://x", "-tls-cert", "/missing"},    // unreadable cert
	} {
		if err := run(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%q) = nil, want error", args)
		}
	}
}

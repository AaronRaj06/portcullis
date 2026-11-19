package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Smoke test: the binary builds and -version works.
func TestVersionFlag(t *testing.T) {
	out, err := exec.Command("go", "run", ".", "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("go run -version: %v\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "portcullis ") {
		t.Fatalf("unexpected output %q", out)
	}
}

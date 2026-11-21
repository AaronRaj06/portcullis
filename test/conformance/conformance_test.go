// Package conformance runs the upstream Gateway API conformance suite against
// Portcullis. It is its own module so the suite's dependencies stay out of the
// main go.mod. It runs inside the cluster (deploy/conformance.sh): on kind with
// Docker Desktop the Gateway's ClusterIP address is not reachable from the host.
package conformance

import (
	"io/fs"
	"strings"
	"testing"

	"sigs.k8s.io/gateway-api/conformance"
	"sigs.k8s.io/gateway-api/conformance/utils/kubernetes"
)

const base = "base/manifests.yaml"

// TestConformance runs the suite with one change to its base manifests: the
// HTTPS-only base Gateway is marked with the suite's own readiness opt-out.
// Portcullis does not program HTTPS listeners, so that Gateway is never
// Accepted, and without the opt-out the suite's setup times out before running
// any test. The tests that use it still run and fail; docs/CONFORMANCE.md lists them.
func TestConformance(t *testing.T) {
	raw, err := fs.ReadFile(&conformance.Manifests, base)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "  name: same-namespace-with-https-listener\n  namespace: gateway-conformance-infra\n"
	patched := strings.Replace(string(raw), anchor,
		anchor+"  annotations:\n    "+kubernetes.GatewayExcludedFromReadinessChecks+": \"true\"\n", 1)
	if patched == string(raw) {
		t.Fatal("base manifest changed upstream: HTTPS Gateway anchor not found")
	}
	opts := conformance.DefaultOptions(t)
	// One filesystem: the suite concatenates a path found in several.
	opts.ManifestFS = []fs.FS{overlay{FS: &conformance.Manifests, path: base, data: []byte(patched)}}
	conformance.RunConformanceWithOptions(t, opts)
}

// overlay serves data at path and everything else from FS.
type overlay struct {
	fs.FS
	path string
	data []byte
}

func (o overlay) ReadFile(name string) ([]byte, error) {
	if name == o.path {
		return o.data, nil
	}
	return fs.ReadFile(o.FS, name)
}

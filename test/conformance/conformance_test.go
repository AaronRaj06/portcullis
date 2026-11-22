// Package conformance runs the upstream Gateway API conformance suite,
// unmodified, against Portcullis. It is its own module so the suite's
// dependencies stay out of the main go.mod. It runs inside the cluster
// (deploy/conformance.sh): on kind with Docker Desktop the Gateway's ClusterIP
// address is not reachable from the host.
package conformance

import (
	"testing"

	"sigs.k8s.io/gateway-api/conformance"
)

func TestConformance(t *testing.T) { conformance.RunConformance(t) }

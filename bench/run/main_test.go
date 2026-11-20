package main

import (
	"testing"
	"time"
)

func TestHelpers(t *testing.T) {
	if median([]float64{3, 1, 2}) != 2 || median([]float64{4, 1, 2, 3}) != 2.5 || median(nil) != 0 {
		t.Fatal("median")
	}
	if percentile([]float64{5, 1, 4, 2, 3}, 0.99) != 5 || percentile([]float64{1, 2, 3, 4}, 0.5) != 2 {
		t.Fatal("percentile")
	}
	for in, want := range map[string]float64{"12.5MiB": 12.5, "1GiB": 1024, "512KiB": 0.5} {
		if got, err := parseMem(in); err != nil || got != want {
			t.Errorf("parseMem(%q) = %v %v", in, got, err)
		}
	}
	cpu, mem := summariseStats([]stat{{time.Now(), 999, 1}, {time.Now(), 100, 5}, {time.Now(), 50, 9}})
	if cpu != 75 || mem != 9 {
		t.Fatalf("summariseStats = %v %v (first sample must be ignored)", cpu, mem)
	}
	if (scenario{Path: "/small", KeepAlive: false, Workers: 64}).id() != "throughput-small-noka-c64" ||
		(scenario{Path: "/large", KeepAlive: true, Rate: 500}).id() != "latency-large-ka-r500" {
		t.Fatal("scenario ids")
	}
}

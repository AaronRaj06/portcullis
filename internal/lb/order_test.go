package lb

import "testing"

func TestSmoothOrderIsBounded(t *testing.T) {
	for _, w := range [][]int{{1000000, 999999}, {1000000, 1}, {1 << 62, 1<<62 - 1}} {
		o := SmoothOrder(w)
		if len(o) > MaxCycle+len(w) {
			t.Fatalf("weights %v: cycle of %d", w, len(o))
		}
		seen := map[int]int{}
		for _, i := range o {
			seen[i]++
		}
		if seen[0] == 0 || seen[1] == 0 || seen[0] < seen[1] {
			t.Fatalf("weights %v: shares %v", w, seen)
		}
	}
}

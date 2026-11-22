//go:build e2e

// Package e2e checks the Phase 6 demo cluster (make demo): canary split,
// policy attachment and failover through a real kind cluster, the controller
// and the data plane. Run with: make e2e.
package e2e

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const base = "http://127.0.0.1:18080"

var client = &http.Client{Timeout: 3 * time.Second}

func get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := client.Get(base + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func kubectl(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func eventually(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func TestCanarySplit(t *testing.T) {
	counts := map[string]int{}
	for range 500 {
		code, body := get(t, "/")
		if code != 200 {
			t.Fatalf("status %d: %s", code, body)
		}
		counts[strings.Fields(body)[0]]++
	}
	t.Logf("500 requests: %v", counts)
	// Each data-plane replica splits exactly 9:1; the Service spreads clients between replicas.
	if v2 := counts["v2"]; v2 < 25 || v2 > 75 {
		t.Fatalf("v2 got %d of 500, want about 50 (10%%)", v2)
	}
}

func TestPolicyBlocksPath(t *testing.T) {
	code, body := get(t, "/admin/settings")
	if code != 403 || !strings.Contains(body, "admin is blocked by policy") {
		t.Fatalf("/admin: %d %s", code, body)
	}
	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatal("no request ID")
	}
	if got := kubectl(t, "", "-n", "demo", "get", "portcullispolicy", "block-admin", "-o", `jsonpath={.status.conditions[?(@.type=="Accepted")].status}`); got != "True" {
		t.Fatalf("policy Accepted = %q", got)
	}
}

func TestPolicyCannotBeDodgedByPathSpelling(t *testing.T) {
	for _, p := range []string{"//admin", "/x/../admin", "/./admin", "/x%2F..%2Fadmin", "/%61dmin"} {
		if code, _ := get(t, p); code != 403 {
			t.Errorf("%s: %d, want 403", p, code)
		}
	}
}

func TestInvalidPolicyFollowsOnError(t *testing.T) {
	const bad = `apiVersion: portcullis.dev/v1alpha1
kind: PortcullisPolicy
metadata: {name: broken, namespace: demo}
spec:
  targetRef: {group: gateway.networking.k8s.io, kind: HTTPRoute, name: app}
  onError: fail_open
  script: |
    policy "broken" {
      on request { if path { deny(403) } }
    }
`
	kubectl(t, bad, "apply", "-f", "-")
	defer kubectl(t, "", "-n", "demo", "delete", "portcullispolicy", "broken", "--ignore-not-found")
	eventually(t, 30*time.Second, "status Invalid with line:col", func() bool {
		msg := kubectl(t, "", "-n", "demo", "get", "portcullispolicy", "broken", "-o", `jsonpath={.status.conditions[0].reason} {.status.conditions[0].message}`)
		return strings.HasPrefix(msg, "Invalid ") && strings.Contains(msg, "2:19: if condition must be bool")
	})
	if code, _ := get(t, "/"); code != 200 {
		t.Fatalf("traffic broken by an invalid policy: %d", code)
	}
	if code, _ := get(t, "/admin"); code != 403 {
		t.Fatalf("the valid policy must still apply: %d", code)
	}
	// With the default, fail_closed, a broken policy closes its route...
	kubectl(t, strings.Replace(bad, "fail_open", "fail_closed", 1), "apply", "-f", "-")
	eventually(t, 30*time.Second, "500 from fail_closed", func() bool { code, _ := get(t, "/"); return code == 500 })
	// ...until it is fixed or removed.
	kubectl(t, "", "-n", "demo", "delete", "portcullispolicy", "broken")
	eventually(t, 30*time.Second, "recovery", func() bool { code, _ := get(t, "/"); return code == 200 })
}

func TestFailoverWhenPodsDeleted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(50 * time.Millisecond) // 20 req/s
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				code, _ := get(t, "/")
				mu.Lock()
				codes[code]++
				mu.Unlock()
			}
		}
	})
	time.Sleep(3 * time.Second)
	kubectl(t, "", "-n", "demo", "delete", "pod", "-l", "version=v1", "--wait=false")
	wg.Wait()
	total, failed := 0, 0
	for c, n := range codes {
		total += n
		if c != 200 {
			failed += n
		}
	}
	t.Logf("failover: %d requests, %v", total, codes)
	if failed*100 > total*2 {
		t.Fatalf("%d of %d requests failed (> 2%%) while v1 pods were replaced", failed, total)
	}
	kubectl(t, "", "-n", "demo", "rollout", "status", "deployment/app-v1", "--timeout=120s")
	eventually(t, 30*time.Second, "v1 back in rotation", func() bool {
		for range 50 {
			if _, body := get(t, "/"); strings.HasPrefix(body, "v1") {
				return true
			}
		}
		return false
	})
}

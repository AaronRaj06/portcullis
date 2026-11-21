#!/usr/bin/env bash
# A paced, narrated walk-through of the `make demo` cluster, made for recording:
#
#   make demo                                    # once: cluster, controller, data plane, app
#   asciinema rec -c deploy/record-demo.sh portcullis.cast
#
# Every command is typed out, then run for real; nothing is pre-recorded.
# PAUSE=0 runs it at full speed (for checking that it still works).
set -euo pipefail
cd "$(dirname "$0")/.."

URL=${URL:-http://127.0.0.1:18080}
PAUSE=${PAUSE:-1.5}
kubectl config use-context "kind-${CLUSTER:-portcullis}" >/dev/null

say() { printf '\n\033[1;36m# %s\033[0m\n' "$*"; sleep "$PAUSE"; }
run() {
  printf '\033[1;32m$\033[0m '
  if [ "$PAUSE" != 0 ]; then
    for ((i = 0; i < ${#1}; i++)); do printf '%s' "${1:i:1}"; sleep 0.02; done
  else printf '%s' "$1"; fi
  printf '\n'
  bash -c "$1"
  sleep "$PAUSE"
}
cleanup() { kubectl -n demo delete portcullispolicy broken --ignore-not-found >/dev/null 2>&1 || true; }
trap cleanup EXIT

say "Portcullis: a Go L7 proxy, a policy language, and a Gateway API controller."
say "The cluster runs the controller, two data-plane pods, and an app at v1 (2 pods) and v2 (1 pod)."
run "kubectl -n portcullis-system get pods"

say "A standard Gateway API HTTPRoute splits traffic 90/10 between the versions."
run "kubectl -n demo get httproute app -o jsonpath='{range .spec.rules[0].backendRefs[*]}{.name}={.weight}{\"\\n\"}{end}'"
run "kubectl -n demo get gateway gw"

say "200 requests through the proxy:"
run "for i in \$(seq 200); do curl -s $URL/; done | cut -d' ' -f1 | sort | uniq -c"

say "A PortcullisPolicy attaches PolicyScript to the route."
run "kubectl -n demo get portcullispolicy block-admin -o jsonpath='{.spec.script}'"
run "curl -s -w ' (HTTP %{http_code})\\n' $URL/admin"

say "A broken policy is rejected with line:col, and never affects the others."
run "kubectl -n demo apply -f - <<'EOF'
apiVersion: portcullis.dev/v1alpha1
kind: PortcullisPolicy
metadata: {name: broken}
spec:
  targetRef: {group: gateway.networking.k8s.io, kind: HTTPRoute, name: app}
  script: |
    policy \"broken\" {
      on request { if path == { deny(403) } }
    }
EOF"
run "sleep 2; kubectl -n demo get portcullispolicy broken -o jsonpath='{.status.conditions[0].reason}: {.status.conditions[0].message}{\"\\n\"}'"
run "curl -s -w ' (HTTP %{http_code})\\n' $URL/admin"

say "Failover: delete every v1 pod while sending 10 requests a second."
run "(for i in \$(seq 100); do curl -s -o /dev/null -w '%{http_code}\\n' --max-time 2 $URL/; sleep 0.1; done) > /tmp/portcullis-failover.txt & sleep 2; kubectl -n demo delete pod -l version=v1 --wait=false; wait; sort /tmp/portcullis-failover.txt | uniq -c"

say "Every config the controller pushed is acknowledged by hash; the data plane exports Prometheus metrics."
run "kubectl -n portcullis-system logs deploy/portcullis-controller --tail=200 | grep 'config pushed' | tail -2 | cut -c1-200"

say "That's it. Code, ADRs, benchmarks and the conformance table: github.com/AaronRaj06/portcullis"

#!/usr/bin/env bash
# Gateway API conformance (make conformance): runs the upstream suite for the
# GATEWAY-HTTP profile, core features only, inside the `make demo` cluster, and
# saves the log and the report under bench/results/conformance/.
#
# It runs in a Pod because on kind with Docker Desktop the Gateway's address
# (the proxy Service's ClusterIP) is not reachable from the host.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-portcullis}
KIND=${KIND:-bin/tools/kind}
VERSION=${VERSION:-$(git describe --tags --always --dirty)}
FEATURES=${FEATURES:-Gateway,HTTPRoute,ReferenceGrant}
RUN_TEST=${RUN_TEST:-} # one test name, e.g. HTTPRouteSimpleSameNamespace
OUT=bench/results/conformance${RUN_TEST:+/$RUN_TEST}
ARCH=$(docker version -f '{{.Server.Arch}}')
GW_API=$(go list -m -f '{{.Dir}}' sigs.k8s.io/gateway-api)/config/crd/standard
kubectl config use-context "kind-$CLUSTER" >/dev/null

# The suite checks that the whole standard channel is installed at its version.
kubectl apply --server-side -f "$GW_API" >/dev/null

(cd test/conformance && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go test -c -o ../../bin/conformance.test .)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cp bin/conformance.test "$TMP/"
printf 'FROM gcr.io/distroless/static-debian12:nonroot\nCOPY conformance.test /conformance.test\nUSER 65532:65532\nENTRYPOINT ["/conformance.test"]\n' >"$TMP/Dockerfile"
docker build -q -t portcullis-conformance:dev "$TMP" >/dev/null
docker save --platform "linux/$ARCH" -o "$TMP/image.tar" portcullis-conformance:dev
"$KIND" load image-archive --name "$CLUSTER" "$TMP/image.tar"

# Every Gateway shares the one data-plane address, so the demo app's catch-all
# route would answer the suite's requests. Remove it for the run; put it back after.
kubectl delete -f deploy/demo/app.yaml --ignore-not-found --wait >/dev/null
trap 'rm -rf "$TMP"; kubectl apply -f deploy/demo/app.yaml >/dev/null' EXIT

kubectl delete job -n portcullis-system conformance --ignore-not-found --wait >/dev/null
kubectl apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: conformance, namespace: portcullis-system}
---
# The suite creates namespaces, CRD-backed objects, Deployments and Services.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: portcullis-conformance}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin}
subjects: [{kind: ServiceAccount, name: conformance, namespace: portcullis-system}]
---
apiVersion: batch/v1
kind: Job
metadata: {name: conformance, namespace: portcullis-system}
spec:
  backoffLimit: 0
  template:
    metadata: {labels: {app: conformance}}
    spec:
      serviceAccountName: conformance
      restartPolicy: Never
      containers:
        - name: conformance
          image: portcullis-conformance:dev
          imagePullPolicy: Never
          args:
            - -test.v
            - -test.timeout=90m
            - -gateway-class=portcullis
            - -conformance-profiles=GATEWAY-HTTP
            - -supported-features=$FEATURES
            - -organization=AaronRaj06
            - -project=portcullis
            - -url=https://github.com/AaronRaj06/portcullis
            - -version=$VERSION
            - -contact=https://github.com/AaronRaj06/portcullis/issues
            - -report-output=/dev/stderr
            - -run-test=$RUN_TEST
YAML

echo "conformance Job started; waiting for it to finish (this takes a while)"
mkdir -p "$OUT"
# Wait for the Job to finish, then fetch the whole log once: a long `logs -f`
# stream does not survive a busy API server.
while :; do
  state=$(kubectl -n portcullis-system get job conformance -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{end}' 2>/dev/null || true)
  case "$state" in *Complete*) status=passed; break ;; *Failed*) status=failed; break ;; esac
  sleep 15
done
kubectl -n portcullis-system logs job/conformance >"$TMP/conformance.log" 2>&1 || true
# Top-level conformance test results, for docs/CONFORMANCE.md.
grep -E '^    --- (PASS|FAIL|SKIP)' "$TMP/conformance.log" | sed 's/^ *//' | sort -k3 >"$TMP/results.txt" || true
if [ ! -s "$TMP/results.txt" ]; then # setup failed: do not replace the last real results
  cp "$TMP/conformance.log" "$OUT/incomplete.log"
  echo "suite $status without results; previous results kept, see $OUT/incomplete.log" >&2
  exit 1
fi
cp "$TMP/conformance.log" "$TMP/results.txt" "$OUT/"
echo "suite $status; $(grep -c PASS "$OUT/results.txt") pass, $(grep -c FAIL "$OUT/results.txt") fail, $(grep -c SKIP "$OUT/results.txt") skip -> $OUT/"

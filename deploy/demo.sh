#!/usr/bin/env bash
# One-command demo (make demo): kind cluster, CRDs, controller, data plane,
# two app versions, then shows the 90/10 canary, a policy block and a failover.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-portcullis}
KIND=${KIND:-bin/tools/kind}
GW_API=$(go list -m -f '{{.Dir}}' sigs.k8s.io/gateway-api)/config/crd/standard
URL=http://127.0.0.1:18080
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

step "kind cluster '$CLUSTER'"
if ! "$KIND" get clusters | grep -qx "$CLUSTER"; then
  "$KIND" create cluster --name "$CLUSTER" --config deploy/kind.yaml --wait 120s
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

step "images"
make docker VERSION=dev
docker image inspect nginx:1.27.5-alpine >/dev/null 2>&1 || docker pull nginx:1.27.5-alpine
# Save only the node's platform: with Docker's containerd image store, `kind load
# docker-image` imports every platform of a multi-arch image and fails on the missing ones.
ARCH=$(docker version -f '{{.Server.Arch}}')
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
for img in portcullis:dev portcullis-controller:dev nginx:1.27.5-alpine; do
  docker save --platform "linux/$ARCH" -o "$TMP/image.tar" "$img"
  "$KIND" load image-archive --name "$CLUSTER" "$TMP/image.tar"
done

step "CRDs: Gateway API (standard channel, from the gateway-api Go module) and PortcullisPolicy"
for crd in gatewayclasses gateways httproutes referencegrants; do
  kubectl apply --server-side -f "$GW_API/gateway.networking.k8s.io_$crd.yaml" >/dev/null
done
kubectl apply -f deploy/crds/ >/dev/null
kubectl wait --for=condition=Established crd --all --timeout=60s >/dev/null

step "control plane and data plane"
kubectl apply -f deploy/manifests/portcullis.yaml
if ! kubectl -n portcullis-system get secret portcullis-admin-token >/dev/null 2>&1; then
  kubectl -n portcullis-system create secret generic portcullis-admin-token --from-literal=token="$(openssl rand -hex 32)"
fi
if ! kubectl -n portcullis-system get secret portcullis-admin-tls >/dev/null 2>&1; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 825 -subj /CN=portcullis-admin \
    -addext subjectAltName=DNS:portcullis-admin -keyout "$TMP/admin.key" -out "$TMP/admin.crt" 2>/dev/null
  kubectl -n portcullis-system create secret tls portcullis-admin-tls --cert="$TMP/admin.crt" --key="$TMP/admin.key"
fi
kubectl -n portcullis-system rollout restart deployment >/dev/null # pick up rebuilt :dev images
kubectl -n portcullis-system rollout status deployment/portcullis-controller --timeout=180s
kubectl -n portcullis-system rollout status deployment/portcullis --timeout=180s

step "demo app: v1 (2 pods) and v2 (1 pod), HTTPRoute 90/10, PortcullisPolicy"
kubectl apply -f deploy/demo/app.yaml
kubectl -n demo rollout status deployment/app-v1 --timeout=120s
kubectl -n demo rollout status deployment/app-v2 --timeout=120s
kubectl -n demo wait --for=condition=Programmed gateway/gw --timeout=120s
kubectl -n demo get gateway,httproute,portcullispolicy
# Each Gateway has its own Service. kind publishes one NodePort, so point it at the demo Gateway.
IP=$(kubectl -n demo get gateway gw -o jsonpath='{.status.addresses[0].value}')
TARGET=$(kubectl -n portcullis-system get svc -o jsonpath="{.items[?(@.spec.clusterIP=='$IP')].spec.ports[0].targetPort}")
kubectl -n portcullis-system patch svc portcullis --type=json \
  -p "[{\"op\":\"replace\",\"path\":\"/spec/ports/0/targetPort\",\"value\":$TARGET}]"
for _ in $(seq 60); do curl -sf -o /dev/null --max-time 2 "$URL/" && break; sleep 1; done

step "canary: 200 requests"
for _ in $(seq 200); do curl -s "$URL/"; done | cut -d' ' -f1 | sort | uniq -c

step "policy: GET /admin"
curl -s -w ' (HTTP %{http_code})\n' "$URL/admin"

step "failover: delete every v1 pod while sending 10 req/s for 20 s"
( for _ in $(seq 200); do curl -s -o /dev/null -w '%{http_code}\n' --max-time 2 "$URL/"; sleep 0.1; done ) > "$TMP/failover.txt" &
sleep 2
kubectl -n demo delete pod -l version=v1 --wait=false
wait
sort "$TMP/failover.txt" | uniq -c
echo
echo "Done. Try: curl $URL/   |   make e2e   |   $KIND delete cluster --name $CLUSTER"

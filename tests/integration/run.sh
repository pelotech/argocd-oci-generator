#!/usr/bin/env bash
# End-to-end test on kind: an authenticated TLS registry, an OCI artifact with a config.json and a manifest,
# Argo CD, the generator, and an ApplicationSet that creates one Application from the artifact.
# Usage: tests/integration/run.sh   (KEEP=1 keeps the cluster)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
cluster=${CLUSTER:-argocd-oci-generator}
argocd_version=${ARGOCD_VERSION:-v3.5.5}
work=$(mktemp -d)
registry_user=tester
registry_password=$(openssl rand -hex 12)
# A high local port: macOS keeps 5000 for AirPlay.
local_port=${LOCAL_PORT:-15000}

cleanup() {
  kill "${forward_pid:-}" 2>/dev/null || true
  [[ -n "${KEEP:-}" ]] || kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

wait_for() { # <seconds> <description> <command...>: polls the command until it succeeds
  local deadline=$(( $(date +%s) + $1 )) description=$2; shift 2
  until "$@" >/dev/null 2>&1; do
    (( $(date +%s) < deadline )) || { echo "timeout: $description" >&2; "$@" >&2 || true; return 1; }
    sleep 5
  done
}

echo "== cluster"
kind create cluster --name "$cluster" --wait 120s
docker build -q -t ghcr.io/pelotech/argocd-oci-generator:test "$root"
kind load docker-image --name "$cluster" ghcr.io/pelotech/argocd-oci-generator:test

echo "== registry"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=registry.registry.svc" \
  -addext "subjectAltName=DNS:registry.registry.svc" -keyout "$work/tls.key" -out "$work/tls.crt" 2>/dev/null
htpasswd -Bbc "$work/htpasswd" "$registry_user" "$registry_password" 2>/dev/null
kubectl create namespace registry
kubectl -n registry create secret tls registry-tls --cert="$work/tls.crt" --key="$work/tls.key"
kubectl -n registry create secret generic registry-auth --from-file="$work/htpasswd"
kubectl apply -f "$here/manifests/registry.yaml"
kubectl -n registry rollout status deployment/registry --timeout=180s

echo "== artifact"
mkdir -p "$work/artifact"
echo '{"name": "app-001", "replicas": 2}' > "$work/artifact/config.json"
cat > "$work/artifact/configmap.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-app-001
data:
  from: the artifact
YAML
tar -C "$work/artifact" -czf "$work/app-001.tar.gz" config.json configmap.yaml
kubectl -n registry port-forward svc/registry "$local_port:443" >/dev/null 2>&1 &
forward_pid=$!
wait_for 30 "the registry port-forward" curl -fsS --cacert "$work/tls.crt" -u "$registry_user:$registry_password" \
  --resolve "registry.registry.svc:$local_port:127.0.0.1" "https://registry.registry.svc:$local_port/v2/"
for tag in 0.9.0 1.0.0; do # oras takes the file relative to the current directory
  (cd "$work" && oras push --ca-file tls.crt --resolve "registry.registry.svc:443:127.0.0.1:$local_port" \
    -u "$registry_user" --password-stdin --annotation "org.opencontainers.image.version=$tag" \
    "registry.registry.svc/demo/app-001:$tag" "app-001.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip" <<<"$registry_password" >/dev/null)
done
digest=$(oras resolve --ca-file "$work/tls.crt" --resolve "registry.registry.svc:443:127.0.0.1:$local_port" \
  -u "$registry_user" --password-stdin registry.registry.svc/demo/app-001:1.0.0 <<<"$registry_password")

echo "== argo cd $argocd_version"
kubectl create namespace argocd
# Server-side: the ApplicationSet CRD is too large for a client-side apply annotation.
kubectl apply --server-side -n argocd -f "https://raw.githubusercontent.com/argoproj/argo-cd/$argocd_version/manifests/install.yaml" >/dev/null
kubectl -n argocd create secret generic registry-creds --from-literal=type=oci \
  --from-literal=url=oci://registry.registry.svc/demo --from-literal=username="$registry_user" \
  --from-literal=password="$registry_password"
kubectl -n argocd label secret registry-creds argocd.argoproj.io/secret-type=repo-creds
# The registry's certificate, keyed by host, before the repo-server starts.
kubectl -n argocd patch configmap argocd-tls-certs-cm --type merge \
  -p "$(python3 -c 'import json,sys; print(json.dumps({"data": {"registry.registry.svc": open(sys.argv[1]).read()}}))' "$work/tls.crt")"

echo "== generator"
kubectl -n argocd create secret generic registry-ca --from-file=ca.crt="$work/tls.crt"
kubectl -n argocd create secret generic argocd-oci-generator \
  --from-literal=REGISTRY=registry.registry.svc/demo --from-literal=REGISTRY_USER="$registry_user" \
  --from-literal=REGISTRY_PASSWORD="$registry_password" --from-literal=PLUGIN_TOKEN=plugin-token-for-the-test \
  --from-literal=SSL_CERT_FILE=/etc/ssl/custom/ca.crt
kubectl apply -k "$here/manifests/generator"
kubectl apply -f "$here/manifests/appset.yaml"
kubectl -n argocd rollout status deployment/argocd-oci-generator --timeout=180s
kubectl -n argocd rollout status deployment/argocd-repo-server --timeout=300s
kubectl -n argocd rollout status deployment/argocd-applicationset-controller --timeout=300s

echo "== assert"
wait_for 300 "the Application demo-app-001" kubectl -n argocd get application demo-app-001
annotation() { kubectl -n argocd get application demo-app-001 -o jsonpath="{.metadata.annotations.demo/$1}"; }
[[ $(annotation tag) == 1.0.0 ]] || { echo "tag: $(annotation tag)" >&2; exit 1; }
[[ $(annotation digest) == "$digest" ]] || { echo "digest: $(annotation digest) != $digest" >&2; exit 1; }
[[ $(annotation version) == 1.0.0 ]] || { echo "version annotation: $(annotation version)" >&2; exit 1; }
[[ $(annotation content) == "$(cat "$work/artifact/config.json")" ]] || { echo "content: $(annotation content)" >&2; exit 1; }
wait_for 300 "the Application to sync" bash -c \
  '[[ $(kubectl -n argocd get application demo-app-001 -o jsonpath="{.status.sync.status}/{.status.health.status}") == Synced/Healthy ]]' \
  || { kubectl -n argocd get application demo-app-001 -o jsonpath='{.status.conditions}{"\n"}{.status.operationState.message}{"\n"}' >&2; exit 1; }
[[ $(kubectl get configmap demo-app-001 -o jsonpath='{.data.from}') == "the artifact" ]]
echo "ok: demo-app-001 generated from registry.registry.svc/demo/app-001:1.0.0 and synced"

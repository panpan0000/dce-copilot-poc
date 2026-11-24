#!/usr/bin/env bash
# 安装基础组件（幂等）。版本全部来自 .env。
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env
require_vars KGATEWAY_VERSION SUBSTRATE_VERSION KAGENT_VERSION NS_KAGENT

need helm; need kubectl; need envsubst

# 渲染 values 模板
export NS_SUBSTRATE NS_KAGENT WORKER_REPLICAS="${WORKER_REPLICAS:-3}" SUBSTRATE_VERSION STORAGE_CLASS
VALS="$REPO_ROOT/build/helm-values"; rm -rf "$VALS"; mkdir -p "$VALS"
for f in "$REPO_ROOT"/deploy/helm-values/*.yaml; do envsubst < "$f" > "$VALS/$(basename "$f")"; done
log "1/4 Gateway API CRD（已存在则跳过）"
kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1 || \
  kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml

log "2/4 kgateway $KGATEWAY_VERSION"
helm upgrade --install kgateway-crds oci://cr.kgateway.dev/kgateway-dev/charts/kgateway-crds \
  --version "$KGATEWAY_VERSION" -n "$NS_GATEWAY" --create-namespace --wait
helm upgrade --install kgateway oci://cr.kgateway.dev/kgateway-dev/charts/kgateway \
  --version "$KGATEWAY_VERSION" -n "$NS_GATEWAY" --wait

log "3/4 substrate $SUBSTRATE_VERSION"
helm upgrade --install substrate-crds oci://ghcr.io/kagent-dev/substrate/helm/substrate-crds \
  --version "$SUBSTRATE_VERSION" -n "$NS_SUBSTRATE" --create-namespace --wait
helm upgrade --install substrate oci://ghcr.io/kagent-dev/substrate/helm/substrate \
  --version "$SUBSTRATE_VERSION" -n "$NS_SUBSTRATE" \
  -f "$VALS/substrate.yaml" --wait --timeout 15m

log "4/4 kagent $KAGENT_VERSION"
helm upgrade --install kagent-crds oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
  --version "$KAGENT_VERSION" -n "$NS_KAGENT" --create-namespace --wait
helm upgrade --install kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
  --version "$KAGENT_VERSION" -n "$NS_KAGENT" \
  -f "$VALS/kagent.yaml" --wait --timeout 15m

ok "基础组件安装完成"

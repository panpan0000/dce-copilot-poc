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
# ── 0. 集群能力预检（能自动补的自动补，补不了的说清楚） ──────────────
log "0/5 集群能力预检"
FAIL=0
# 0a gVisor：只能节点预装，不能靠 YAML
if "$KUBECTL" get runtimeclass gvisor >/dev/null 2>&1; then
  ok "runtimeClass/gvisor 存在"
else
  warn "缺 runtimeClass/gvisor —— gVisor 必须在节点上预装（containerd 里注册 runsc）"
  echo "   安装参考: https://gvisor.dev/docs/user_guide/install/"
  FAIL=1
fi
# 0b StorageClass：没有默认 SC 时 substrate 的 PVC 会一直 Pending
if "$KUBECTL" get sc -o jsonpath='{range .items[*]}{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{"\n"}{end}' 2>/dev/null | grep -q true; then
  ok "存在默认 StorageClass"
else
  warn "没有默认 StorageClass，substrate 的 PVC 会 Pending"
  FAIL=1
fi
# 0c LoadBalancer：没有 LB 实现时 Gateway 拿不到 IP（可选自动装 MetalLB）
if [ "${INSTALL_METALLB:-false}" = "true" ]; then
  log "安装 MetalLB（INSTALL_METALLB=true）"
  "$KUBECTL" apply -f https://raw.githubusercontent.com/metallb/metallb/v0.14.8/config/manifests/metallb-native.yaml
  "$KUBECTL" -n metallb-system wait --for=condition=Ready pod -l app=metallb --timeout=180s || warn "MetalLB 未就绪"
  [ -n "${METALLB_POOL:-}" ] && cat <<EOF | "$KUBECTL" apply -f -
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata: {name: default, namespace: metallb-system}
spec: {addresses: ["${METALLB_POOL}"]}
EOF
else
  if "$KUBECTL" get sc >/dev/null 2>&1 && "$KUBECTL" get svc -A 2>/dev/null | grep -q LoadBalancer; then
    ok "集群已有 LoadBalancer 能力（或 svclb）"
  else
    warn "没检测到 LoadBalancer；Gateway 可能拿不到 IP。可设 INSTALL_METALLB=true METALLB_POOL=<cidr> 自动安装"
  fi
fi
[ "$FAIL" = "0" ] || die "预检未通过：先补齐上面标注的集群能力（它们装不出来，只能预置）"

log "1/5 Gateway API CRD（已存在则跳过）"
kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1 || \
  kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml

log "2/5 kgateway $KGATEWAY_VERSION"
helm upgrade --install kgateway-crds oci://cr.kgateway.dev/kgateway-dev/charts/kgateway-crds \
  --version "$KGATEWAY_VERSION" -n "$NS_GATEWAY" --create-namespace --wait
helm upgrade --install kgateway oci://cr.kgateway.dev/kgateway-dev/charts/kgateway \
  --version "$KGATEWAY_VERSION" -n "$NS_GATEWAY" --wait

log "3/5 substrate $SUBSTRATE_VERSION"
helm upgrade --install substrate-crds oci://ghcr.io/kagent-dev/substrate/helm/substrate-crds \
  --version "$SUBSTRATE_VERSION" -n "$NS_SUBSTRATE" --create-namespace --wait
helm upgrade --install substrate oci://ghcr.io/kagent-dev/substrate/helm/substrate \
  --version "$SUBSTRATE_VERSION" -n "$NS_SUBSTRATE" \
  -f "$VALS/substrate.yaml" --wait --timeout 15m

log "4/5 kagent $KAGENT_VERSION"
helm upgrade --install kagent-crds oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
  --version "$KAGENT_VERSION" -n "$NS_KAGENT" --create-namespace --wait
helm upgrade --install kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
  --version "$KAGENT_VERSION" -n "$NS_KAGENT" \
  -f "$VALS/kagent.yaml" --wait --timeout 15m

log "5/5 校验关键 CRD"
for g in gatewayclasses.gateway.networking.k8s.io workerpools.ate.dev sandboxagents.kagent.dev gatewayclasses.gateway.networking.k8s.io; do
  "$KUBECTL" get crd "$g" >/dev/null 2>&1 && ok "CRD $g" || die "CRD 缺失: $g"
done
ok "基础组件安装完成 → 下一步 make deploy"

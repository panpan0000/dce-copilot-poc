#!/usr/bin/env bash
# 渲染清单 → apply → 等就绪 → 回填 Actor 镜像 digest。
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env
require_vars IMAGE_REGISTRY IMAGE_TAG NS_APP NS_KAGENT JWT_ISSUER JWT_AUDIENCE DCE_ORIGIN DCE_CLUSTER

KUBECTL="${KUBECTL:-kubectl}"
RENDER="$REPO_ROOT/build/render"
rm -rf "$RENDER"; mkdir -p "$RENDER"

# ── 变量准备 ───────────────────────────────────────────────
export IMAGE_ADAPTER="$(image_ref adapter)"
export IMAGE_SRE_AGENT="$(image_ref sre-agent)"
export JWKS_INLINE
JWKS_INLINE="$(tr -d '\n' < "${JWKS_PATH:-$REPO_ROOT/deploy/jwks/jwks.json}")"
export WORKER_REPLICAS="${WORKER_REPLICAS:-3}"
export SNAPSHOT_LOCATION="${SNAPSHOT_LOCATION:-gs://ate-snapshots/${NS_KAGENT}/dsh-sandbox}"
export SRE_TARGET_CLUSTER="${SRE_TARGET_CLUSTER:-$DCE_CLUSTER}"

# Actor 镜像必须用 digest（BYO 的硬性要求）；从本地 docker 里取，取不到就报错
dsh_ref="$(image_ref dsh-a2a)"
digest="$(${DOCKER:-docker} image inspect "$dsh_ref" --format '{{index .RepoDigests 0}}' 2>/dev/null || true)"
if [ -z "$digest" ] && [ "${DRY_RUN:-0}" = "1" ]; then
  warn "DRY_RUN：没有 digest，用 tag 渲染（真实部署必须以 digest 固定）"
  digest="$dsh_ref"
fi
[ -n "$digest" ] || die "拿不到 $dsh_ref 的 digest，先 make build 并确认已 push"
export IMAGE_DSH_A2A_DIGEST="$digest"

# ── 渲染 ──────────────────────────────────────────────────
render() {
  local src="$1" dst="$RENDER/$(basename "$1")"
  envsubst < "$src" > "$dst"
  grep -q '\${' "$dst" && warn "渲染后仍有未替换变量: $(basename "$src")" || true
}
for f in "$REPO_ROOT"/deploy/manifests/*.yaml; do render "$f"; done
ok "清单已渲染到 $RENDER"

# DRY_RUN=1：只渲染并 diff，不 apply
if [ "${DRY_RUN:-0}" = "1" ]; then
  for f in "$RENDER"/*.yaml; do
    log "diff $(basename "$f")"
    "$KUBECTL" diff -f "$f" 2>&1 | head -40 || true
  done
  ok "DRY_RUN 完成（未改动集群）"; exit 0
fi

# ── 应用 ──────────────────────────────────────────────────
"$KUBECTL" get ns "$NS_APP" >/dev/null 2>&1 || "$KUBECTL" create ns "$NS_APP"
"$KUBECTL" apply -f "$RENDER/adapter.yaml"
"$KUBECTL" apply -f "$RENDER/gateway.yaml"
"$KUBECTL" apply -f "$RENDER/sre.yaml"
"$KUBECTL" apply -f "$RENDER/workerpool.yaml"
"$KUBECTL" apply -f "$RENDER/sandboxagent.yaml"

if [ "${WITH_NETWORKPOLICY:-false}" = "true" ]; then
  log "应用 NetworkPolicy（WITH_NETWORKPOLICY=true）"
  "$KUBECTL" apply -f "$RENDER/networkpolicy.yaml"
  warn "装了策略就要自证: make netpol-test"
else
  log "跳过 NetworkPolicy（默认不装，避免 CNI 不执行时给人假安全感）"
fi

# ── 等就绪 ────────────────────────────────────────────────
log "等待 adapter / SRE 就绪"
"$KUBECTL" -n "$NS_APP" rollout status deploy/substrate-adapter --timeout=180s
"$KUBECTL" -n "$NS_APP" rollout status deploy/sre-gateway --timeout=180s || warn "sre-gateway 未就绪，检查 Secret sre-backend-token"
"$KUBECTL" -n "$NS_APP" rollout status deploy/sre-agent --timeout=300s || warn "sre-agent 未就绪，检查 Secret dsh-config"

log "等待 SandboxAgent 生成快照（首次约 1-3 分钟）"
for i in $(seq 1 60); do
  phase="$("$KUBECTL" -n "$NS_KAGENT" get sandboxagent dsh-sandbox -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
  [ "$phase" = "True" ] && { ok "SandboxAgent Ready"; break; }
  sleep 5
done
[ "${phase:-}" = "True" ] || die "SandboxAgent 未就绪，看: kubectl -n $NS_KAGENT describe sandboxagent dsh-sandbox"

log "入口地址"
case "${GATEWAY_MODE}" in
  loadbalancer)
    ip="$("$KUBECTL" -n "$NS_APP" get svc poc-gateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    echo "  http://${GATEWAY_HOST:-${GATEWAY_IP:-$ip}}:8080/sandbox" ;;
  nodeport)
    node="$("$KUBECTL" get nodes -o jsonpath='{.items[0].status.addresses[0].address}')"
    port="$("$KUBECTL" -n "$NS_APP" get svc poc-gateway -o jsonpath='{.spec.ports[0].nodePort}')"
    echo "  http://${node}:${port}/sandbox" ;;
  *) echo "  ingress 模式请自行配置 host，EXTENSION_GATEWAY_BASE=${EXTENSION_GATEWAY_BASE}" ;;
esac
ok "部署完成，下一步: make extension && make verify"

#!/usr/bin/env bash
# 部署前自检：配置项、依赖命令、registry 登录、DCE 可达、集群能力。
# 任何一项失败都会指出"要改哪个变量/装哪个东西"，而不是等到运行时 401。
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env

FAILED=0
fail() { printf '\033[0;31m✗\033[0m %s\n' "$*"; FAILED=$((FAILED+1)); }
pass() { printf '\033[0;32m✓\033[0m %s\n' "$*"; }

echo "── 1. 必填配置 ─────────────────────────────"
for v in DCE_ORIGIN DCE_CLUSTER JWT_ISSUER JWT_AUDIENCE IMAGE_REGISTRY IMAGE_TAG \
         DSH_BASE_IMAGE NS_KAGENT NS_APP GATEWAY_MODE EXTENSION_MATCH_ORIGIN EXTENSION_GATEWAY_BASE; do
  if [ -n "${!v:-}" ]; then pass "$v=${!v}"; else fail "缺少 $v"; fi
done

echo "── 2. 占位符 ───────────────────────────────"
for v in IMAGE_REGISTRY DSH_BASE_IMAGE; do
  case "${!v:-}" in
    *example.com*) fail "$v 还是模板值: ${!v}";;
    *) pass "$v 已配置";;
  esac
done

echo "── 3. 依赖命令 ─────────────────────────────"
for c in docker kubectl git curl envsubst; do
  command -v "$c" >/dev/null 2>&1 && pass "$c" || fail "缺少命令 $c"
done

echo "── 4. 集群连通 ─────────────────────────────"
if kubectl version --request-timeout=5s >/dev/null 2>&1; then
  pass "kubectl 可连集群: $(kubectl config current-context 2>/dev/null)"
  for ns in "$NS_KAGENT" "$NS_APP"; do
    kubectl get ns "$ns" >/dev/null 2>&1 && pass "命名空间 $ns 存在" || warn "命名空间 $ns 不存在（bootstrap 会创建）"
  done
  # 执行 NetworkPolicy 的 CNI？只提示，不阻断
  if kubectl get crd 2>/dev/null | grep -q cilium; then pass "检测到 Cilium（支持 NetworkPolicy）"; else warn "未见 Cilium/Calico 类 CNI，NetworkPolicy 可能不生效"; fi
else
  fail "kubectl 连不上集群"
fi

echo "── 5. DCE 可达性 ───────────────────────────"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 "$DCE_ORIGIN/apis/ghippo.io/v1alpha1/current-user" || true)
case "$code" in
  401|403) pass "DCE 可达，未带 token 返回 ${code}（符合预期）";;
  200) warn "DCE 未鉴权就返回 200，检查是否真的需要身份";;
  000) fail "连不上 $DCE_ORIGIN";;
  *) warn "DCE 返回 ${code}，确认地址是否正确";;
esac

echo "── 6. 镜像仓库 ─────────────────────────────"
if docker image ls >/dev/null 2>&1; then
  pass "docker 可用"
  if [ "${PUSH_IMAGES:-false}" = "true" ]; then
    registry_host="${IMAGE_REGISTRY%%/*}"
    if curl -s --max-time 5 "https://$registry_host/v2/" -o /dev/null; then
      pass "registry 可达: $registry_host"
    else
      warn "registry $registry_host 未响应 /v2/，推送前请先 docker login"
    fi
  fi
fi

echo
if [ "$FAILED" -gt 0 ]; then
  die "$FAILED 项未通过，先修完再部署"
fi
ok "配置检查全部通过，可以 make build"

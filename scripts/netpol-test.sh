#!/usr/bin/env bash
# 证明 NetworkPolicy 真的生效：应拒的必须被拒，应通的必须通。
# 任何一条不符 → 这个 CNI 不执行策略，别把它当安全边界。
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env

K="${KUBECTL:-kubectl}"
echo "── 策略是否已装 ──"
"$K" -n "$NS_APP" get networkpolicy agent-ingress-from-gateway >/dev/null 2>&1 \
  || die "没有 agent-ingress-from-gateway，先 WITH_NETWORKPOLICY=true make deploy"

echo "── 从 default 命名空间直连 adapter（应超时/被拒）──"
if "$K" -n default run netpol-probe-$RANDOM --rm -i --restart=Never --image=busybox:1.36 \
     --timeout=25s -- wget -qO- --timeout=5 "http://substrate-adapter.${NS_APP}.svc:9099/healthz" >/dev/null 2>&1; then
  die "策略没生效：默认命名空间能直连 adapter"
fi
ok "直连被拒（策略生效）"

echo "── 从网关命名空间访问 adapter（应通）──"
code=$("$K" -n "$NS_GATEWAY" run netpol-probe-$RANDOM --rm -i --restart=Never --image=curlimages/curl:8.10.1 \
  --timeout=40s -- -s -o /dev/null -w '%{http_code}' "http://substrate-adapter.${NS_APP}.svc:9099/healthz" 2>/dev/null | tr -dc '0-9')
[ "$code" = "200" ] && ok "网关侧可达（200）" || die "网关侧也不可达（$code），检查 namespaceSelector 标签"
ok "NetworkPolicy 自证通过"

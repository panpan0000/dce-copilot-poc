#!/bin/sh
set -e
# 凭据：Secret 挂到 /model/.credentials.yaml，复制到 DSH 的固定位置
mkdir -p /home/agent/.dsh /home/agent/work /home/agent/.kube
# substrate 的 BYO 不会把 volume 带进 actor，所以凭据/kubeconfig 走 env（base64）
if [ -n "${DSH_CREDENTIALS_B64:-}" ]; then
  printf %s "$DSH_CREDENTIALS_B64" | base64 -d > /home/agent/.dsh/.credentials.yaml
  chmod 600 /home/agent/.dsh/.credentials.yaml
fi
if [ -n "${KUBECONFIG_B64:-}" ]; then
  printf %s "$KUBECONFIG_B64" | base64 -d > /home/agent/.kube/config
  chmod 600 /home/agent/.kube/config
fi
if [ -f /model/.credentials.yaml ]; then
  cp /model/.credentials.yaml /home/agent/.dsh/.credentials.yaml
  chmod 600 /home/agent/.dsh/.credentials.yaml
fi
# 沙箱里 kubeconfig 可选（只读巡检用）
if [ -f /model/kubeconfig ]; then
  cp /model/kubeconfig /home/agent/.kube/config
  chmod 600 /home/agent/.kube/config
fi
export KUBECONFIG=/home/agent/.kube/config
export A2A_PORT=80
export DCE_HOST=${DCE_HOST:-http://10.31.223.20:31284}
# 思考档位：off | low | high | max。不设就沿用云端默认（DSH 不发该字段）。
PATCH_ARG=""
case "${DSH_REASONING_EFFORT:-}" in
  off|low|high|max)
    cat > /tmp/agent-model.patch.yml <<EOF
- id: agent-default-model
  config:
    provider: deepseek-official
    model: deepseek-v4-flash
    reasoningEffort: ${DSH_REASONING_EFFORT}
EOF
    PATCH_ARG="--patch /tmp/agent-model.patch.yml"
    ;;
esac
# shellcheck disable=SC2086
dsh --profile web $PATCH_ARG --no-open --host 127.0.0.1 --port 18790 &
for i in $(seq 1 40); do
  if curl -sf --max-time 2 http://127.0.0.1:18790/ >/dev/null 2>&1; then break; fi
  sleep 1
done
exec node /opt/dsh-a2a/a2a-shim.js

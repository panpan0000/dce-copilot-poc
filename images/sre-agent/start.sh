#!/bin/sh
set -e
# 模型凭据：由 Secret 挂到 /model/credentials，拷到 DSH 需要的位置
if [ -f /model/credentials ]; then
  mkdir -p /home/agent/.dsh
  cp /model/credentials /home/agent/.dsh/.credentials.yaml
  chmod 600 /home/agent/.dsh/.credentials.yaml
fi
dsh --profile web --no-open --host 127.0.0.1 --port 18790 &
exec node /app/sre-agent.js

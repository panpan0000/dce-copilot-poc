#!/usr/bin/env bash
# 证明 NetworkPolicy 真的生效：允许的请求应通，应拒的请求必须被拒。
# 任何一条不符 → 说明 CNI 不执行策略，别把它当成安全边界。
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env
echo "TODO: 等 deploy 里加上 NetworkPolicy 后再实现（当前默认不安装）"
exit 1

#!/usr/bin/env bash
# 打印入口地址。部署日志关掉之后，随时用 make url 再查一次。
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env
KUBECTL="${KUBECTL:-kubectl}"
NS="${NS_APP:-watsons-agent-poc}"

type="$("$KUBECTL" -n "$NS" get svc poc-gateway -o jsonpath='{.spec.type}' 2>/dev/null || true)"
[ -n "$type" ] || die "找不到 $NS/poc-gateway（集群连对了吗？）"

case "$type" in
  NodePort)
    node="$("$KUBECTL" get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' | awk '{print $1}')"
    port="$("$KUBECTL" -n "$NS" get svc poc-gateway -o jsonpath='{.spec.ports[0].nodePort}')"
    echo "$node:$port"
    ;;
  LoadBalancer)
    ip="$("$KUBECTL" -n "$NS" get svc poc-gateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    echo "${ip:-<pending>}:8080"
    ;;
  *) echo "<$type：请自行配置 host>" ;;
esac

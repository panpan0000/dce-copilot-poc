#!/usr/bin/env bash
# substrate 沙箱出网修复。
#
# substrate 0.0.8 的 dns-controller 生成的 Corefile 只有内部 actor zone，没有外部转发；
# 且它找 kube-system 的 `kube-dns` ConfigMap（jinye 用 CoreDNS，没有）→ 外部域名 REFUSED。
#
# 修复两件事：
# 1. 建 kube-system/kube-dns ConfigMap（让 dns-controller 的 stub resolver 逻辑不再跳过）
# 2. 给 substrate coredns 加一个 `.:53` zone：所有外部 A 记录都答成 atenet-router IP
#    （沙箱流量统一走 egress 网关，Envoy 按 SNI/Host 路由到真实域名）。
#
# 用法：KUBECONFIG=<jinye> scripts/fix-dns-egress.sh
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env
KUBECTL="${KUBECTL:-kubectl}"
NS_SUBSTRATE="${NS_SUBSTRATE:-ate-system}"

ROUTER_IP="$("$KUBECTL" -n "$NS_SUBSTRATE" get svc atenet-router -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
[ -n "$ROUTER_IP" ] || die "找不到 atenet-router Service"

# 1. kube-dns ConfigMap（用集群 CoreDNS 的 IP 作为上游）
CLUSTER_DNS_IP="$("$KUBECTL" -n kube-system get svc coredns -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
[ -n "$CLUSTER_DNS_IP" ] || CLUSTER_DNS_IP="$("$KUBECTL" -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
[ -n "$CLUSTER_DNS_IP" ] || die "找不到集群 DNS Service（coredns/kube-dns）"

KUBE_DNS_CM=$(cat <<_EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: kube-dns
  namespace: kube-system
data:
  stubDomains: |
    {}
  upstreamNameservers: |
    ["$CLUSTER_DNS_IP"]
_EOF
)
printf '%s\n' "$KUBE_DNS_CM" | "$KUBECTL" apply -f - >/dev/null

# 2. substrate coredns：外部 A 记录 → router IP
EXTRA_CM=$(cat <<_EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: substrate-dns-extra
  namespace: $NS_SUBSTRATE
data:
  main.Corefile: |
    import /etc/coredns/Corefile

    .:53 {
        errors
        template IN A . {
            answer "{{ .Name }} 60 IN A $ROUTER_IP"
        }
    }
_EOF
)
printf '%s\n' "$EXTRA_CM" | "$KUBECTL" apply -f - >/dev/null

# 3. 改 dns Deployment：coredns 用 import 入口，挂载 extra ConfigMap
"$KUBECTL" -n "$NS_SUBSTRATE" patch deploy dns --type=strategic --patch "{\"spec\":{\"template\":{\"spec\":{
  \"containers\":[
    {\"name\":\"coredns\",
     \"args\":[\"-conf\",\"/etc/coredns-extra/main.Corefile\"],
     \"volumeMounts\":[
       {\"name\":\"dns-config-volume\",\"mountPath\":\"/etc/coredns\"},
       {\"name\":\"dns-extra-volume\",\"mountPath\":\"/etc/coredns-extra\"}
     ]}
  ],
  \"volumes\":[
    {\"name\":\"dns-config-volume\",\"emptyDir\":{}},
    {\"name\":\"dns-extra-volume\",\"configMap\":{\"name\":\"substrate-dns-extra\"}}
  ]
}}}}"

"$KUBECTL" -n "$NS_SUBSTRATE" rollout restart deploy/dns >/dev/null 2>&1
ok "substrate DNS egress 已修复（外部域名 → $ROUTER_IP 走 egress 网关）"

# 已解决的问题

只写"现象 → 原因 → 现在的处理"。不复述排查过程。

## 1. Actor 起不来：`runsc create … device or resource busy`
- **现象**：沙箱 Pod 卡在创建，日志里 `cgroup.subtree_control`。
- **原因**：worker 处于私有 cgroup namespace，根 cgroup 已有进程，不能再写 subtree_control。
- **处理**：给 runsc 注入 `-ignore-cgroups`；默认 `auto` 探测（能写就不注入），普通 x86 节点自动跳过。
  这是本机绕过，不属于可移植配置。

## 2. SandboxAgent 创建被拒
- **现象**：`must set spec.byo.deployment.cmd` / `image must be pinned with a digest`。
- **原因**：BYO 的契约要求。
- **处理**：`cmd` 是字符串 + `args` 是数组；`deploy.sh` 自动把本地镜像 digest 回填进 SandboxAgent。

## 3. Actor 里没有模型 key / kubectl 没身份
- **现象**：`llm-deepseek: no API key`；kubectl 报 `no configuration has been provided`。
- **原因**：**BYO 不会把 volumes/volumeMounts 传进 Actor**，只有 `env` 生效。
- **处理**：凭据一律走 `env` + `secretKeyRef`；kubeconfig 由 adapter 每次请求下发，shim 写入 Actor。

## 4. 请求撞上休眠就超时
- **现象**：白天好好的，放一会儿再问就 120s 超时。
- **原因**：kagent controller 的唤醒竞态——检查时还在跑，等路由时已 SUSPENDED，于是干等。
- **处理**：`images/controller-fix/` 提供补丁镜像（可达性等待中若变 SUSPENDED 就再唤醒），附恢复官方镜像的命令。

## 5. 长回复被截断 / 面板报"连接中断"
- **现象**：输出到一半停了；30 秒左右最常见。
- **原因**：三处叠加——网关超时、MV3 后台 30 秒空闲被回收、SSE 链路断。
- **处理**：网关 300s；shim 心跳；扩展断线自动回落非流式并补全答案。

## 6. Valkey 重置后服务起不来
- **现象**：`dial tcp :6379: connection refused` / MOVED 重定向失败。
- **原因**：单节点 Redis Cluster 若没设置对外宣告地址，客户端拿到的地址是空的。
- **处理**：显式 `cluster-announce-ip` + `cluster-announce-bus-port` + `config rewrite`；
  文档里注明"换 IP 就要重新设置，不要照搬"。

## 7. 换会话要改环境变量
- **现象**：`SESSION_EPOCH` 每次 bump，所有用户的历史会话同时失效。
- **原因**：早期没有删 Actor 的接口，只能用"整体换代"绕过卡死的 Actor。
- **处理**：会话改成 `hash(用户 + 会话标识)`，每个会话独立；`SESSION_EPOCH` 保留但已不参与推导。

## 8. 用户根本看不到自己的 Pod
- **现象**：Agent 说"你没有权限"，其实只是集群级 list 被拒。
- **原因**：kubeconfig 直连 K8s 受 RBAC 约束，`get ns` / `get pods -A` 通常被拒；
  但命名空间内往往有权限，而用户又列不出命名空间。
- **处理**：提示词要求"先用 DCE API 列可见命名空间（asl/namespaces），再 `kubectl -n <ns>`"。

## 9. 小 Agent 抢了 SRE 的活
- **现象**：要"巡检报告"，结果 Agent 自己用 kubectl 拼了一份。
- **原因**：提示词把 SRE 当"可选项"，而沙箱里又有 dce CLI 和诊断 skill。
- **处理**：preamble 写成规则——报告必须来自 `sre-cli report`；自检必须声明"不是 SRE 报告"。

# 已解决的问题

只写"现象 → 原因 → 现在的处理"。不复述排查过程。

## 1. Actor 起不来：`runsc create … device or resource busy`
- **现象**：沙箱 Pod 卡在创建，日志里 `cgroup.subtree_control`。
- **原因**：worker 处于私有 cgroup namespace，根 cgroup 已有进程，不能再写 subtree_control。
- **处理**：给 runsc 注入 `-ignore-cgroups`；默认 `auto` 探测（能写就不注入），普通 x86 节点自动跳过。
  这只在 macOS/OrbStack 上需要，不是可移植配置。

## 2. Actor `no free workers available`
- **现象**：`ResumeActor → AssignWorker: no free workers available`，worker Pod 明明 Running。
- **原因**：substrate worker 的可用容量由 Pod 的 `resources.limits.memory` 推算，没设 resources 时容量算成 0。
- **处理**：WorkerPool 必须设 `template.resources`（至少 `limits.memory`），否则永远"无空闲 worker"。

## 3. golden snapshot 一直不就绪（ActorTemplate 卡住）
- **现象**：`ActorTemplate golden snapshot is not ready`，反复 retry。
- **原因**：kagent 硬编码 golden 探针为 `GET :8081/readyz`，Actor 镜像没这个端点就永远不就绪。
- **处理**：Actor 镜像（a2a-shim）必须监听 `8081` 并提供 `/readyz`。

## 4. 沙箱里模型 `no API key` / 凭据不被识别
- **现象**：`llm-deepseek: no API key for provider route "deepseek-official"`。
- **原因**：新版 DSH 的凭据文件要 `version: 1 + refs:` 格式（旧平铺格式会被拒），且 `refs.DEEPSEEK_API_KEY` 要有值。
- **处理**：凭据走 `DSH_CREDENTIALS_B64`（base64 的新格式文件）+ `DEEPSEEK_API_KEY` 字面 env 双保险。

## 5. 沙箱里模型 `TRANSPORT`（出网失败）
- **现象**：模型 API 请求 `failed (TRANSPORT)`。
- **原因**：substrate 0.0.8 的 dns-controller 生成的 Corefile 只有内部 actor zone，外部域名 `REFUSED`。
- **处理**：外部域名 DNS 答成 atenet-router IP（沙箱流量走 egress 网关）。`make install-dependency` 已内置（`scripts/fix-dns-egress.sh`）。

## 6. 沙箱里模型 `TRANSPORT`（另一个原因：不信任 egress 网关证书）
- **现象**：模型调用反复 `llm/retry` 后 `TRANSPORT`，但 DNS 解析正常、节点能直连模型 API。
- **原因**：substrate 的 egress 网关用自己的 CA（`api-ca`）签发证书做 TLS 终止；Actor 里没有这个 CA，
  也没有 `NODE_EXTRA_CA_CERTS`，于是证书校验失败。
- **处理**：把 `ate-system/ateapi-ca` 的 `ca.crt` 做成 `egress-trust` ConfigMap（key 必须叫 `trust-bundle.pem`），
  挂到 Actor 的 `/run/kagent/egress`，并设 `NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE` / `CURL_CA_BUNDLE`。
  `make install-agent` 已自动做前两步，SandboxAgent manifest 里已带 env 和挂载。

## 7. gVisor 资产下载失败（离线集群）
- **现象**：沙箱起不来，atelet 拉 runsc 报错（`anonymous open failed` / `InvalidBucketName`）。
- **原因**：runsc 从 SandboxConfig 的 `gs://gvisor/...` 下载，离线集群出不了网；且资产 URL 只认 `gs://`/`s3://`，不认 `http://`。
- **处理**：把 runsc（sha256 必须一致）上传到集群内 rustfs，把 SandboxConfig URL 改成 `s3://`。见 [install.md](install.md)「gVisor 说明」。

## 8. SandboxAgent 创建被拒
- **现象**：`must set spec.byo.deployment.cmd` / `image must be pinned with a digest`。
- **原因**：BYO 的契约要求。
- **处理**：`cmd` 是字符串 + `args` 是数组；`deploy.sh` 自动把本地镜像 digest 回填进 SandboxAgent。

## 9. Actor 里没有模型 key / kubectl 没身份
- **现象**：`llm-deepseek: no API key`；kubectl 报 `no configuration has been provided`。
- **原因**：**BYO 不会把 volumes/volumeMounts 传进 Actor**，只有 `env` 生效。
- **处理**：凭据一律走 `env` + `secretKeyRef`；kubeconfig 由 adapter 每次请求下发，shim 写入 Actor。

## 10. 请求撞上休眠就超时
- **现象**：白天好好的，放一会儿再问就 120s 超时。
- **原因**：kagent controller 的唤醒竞态——检查时还在跑，等路由时已 SUSPENDED，于是干等。
- **处理**：`images/controller-fix/` 提供补丁镜像（可达性等待中若变 SUSPENDED 就再唤醒），附恢复官方镜像的命令。

## 11. 长回复被截断 / 面板报"连接中断"
- **现象**：输出到一半停了；30 秒左右最常见。
- **原因**：三处叠加——网关超时、MV3 后台 30 秒空闲被回收、SSE 链路断。
- **处理**：网关 300s；shim 心跳；扩展断线自动回落非流式并补全答案。

## 12. 清库或换 IP 后服务起不来
- **现象**：`dial tcp :6379: connection refused` 或 MOVED 重定向到空地址。
- **原因**：单节点 Redis Cluster 若没设置对外宣告地址，客户端拿到的地址是空的。
- **处理**：显式 `cluster-announce-ip` + `cluster-announce-bus-port` + `config rewrite`；
  文档里注明"换 IP 就要重新设置，不要照搬"。

## 13. 想给某个用户换一个新会话
- **现象**：早期实现里换会话要改全局环境变量，一改所有用户的历史会话同时失效。
- **原因**：没有删 Actor 的接口，只能用"整体换代"绕过卡死的 Actor。
- **处理**：会话改为 `hash(用户 + 会话标识)`，每个会话独立；`SESSION_EPOCH` 保留但不再参与推导，换会话只需新的 `conversation_id`。

## 14. 用户根本看不到自己的 Pod
- **现象**：Agent 说"你没有权限"，其实只是集群级 list 被拒。
- **原因**：kubeconfig 直连 K8s 受 RBAC 约束，`get ns` / `get pods -A` 通常被拒；
  但命名空间内往往有权限，而用户又列不出命名空间。
- **处理**：提示词要求"先用 DCE API 列可见命名空间（asl/namespaces），再 `kubectl -n <ns>`"。

## 15. 小 Agent 抢了 SRE 的活
- **现象**：要"巡检报告"，结果 Agent 自己用 kubectl 拼了一份。
- **原因**：提示词把 SRE 当"可选项"，而沙箱里又有 dce CLI 和诊断 skill。
- **处理**：preamble 写成规则——报告必须来自 `sre-cli report`；自检必须声明"不是 SRE 报告"。

# 已解决的问题

只写"现象 → 原因 → 现在的处理"。不复述排查过程。

## 1. Actor 起不来：`runsc create … device or resource busy`
- **现象**：沙箱 Pod 卡在创建，日志里 `cgroup.subtree_control`。
- **原因**：worker 处于私有 cgroup namespace，根 cgroup 已有进程，不能再写 subtree_control。
- **处理**：给 runsc 注入 `-ignore-cgroups`；默认 `auto` 探测（能写就不注入），普通 x86 节点自动跳过。
  这只在 macOS/OrbStack 上需要，不是可移植配置。


> **2026-11-27 补充（实测数据）**：jinye 的盘确实是短板，但要分清"顺序"和"随机"：
> ```bash
> dd bs=1M count=512 conv=fdatasync      # 写 163 MB/s（看着还行）
> dd bs=4k count=5120 oflag=direct       # 4K 随机写 1.6 MB/s ≈ 400 IOPS（很差）
> ```
> VMware 虚拟盘没缓存电池时就是这样。**所以"沙箱恢复慢"在旧栈成立**：旧栈每次 resume 要搬
> 几百 MB 内存镜像 + 大量小文件 fsync，正好踩在 400 IOPS 上。
>
> 但新栈（substrate 1.0）把这块 I/O 移出了关键路径：golden 快照总量只有 **<1MB**
> （`du -sh /var/lib/ate/snapshots` ≈ 948K），恢复只碰几个文件；同一台机器上实测
> 新会话首问 **4.5s / 后续 1.4s / TTFB 1.1s**。
>
> 结论：**盘还是慢的，只是现在不用它搬大东西了**。一旦会话体量变大
> （装了 skills、下载了依赖、`/data` 写了很多文件），快照会变大，restore 会重新被这块盘拖住
> —— 那时该换 SSD/NVMe 或给虚拟机开 write-back 缓存。

## 2. checkpoint/restore 慢（磁盘 I/O，不是平台）
- **现象**：restore 8~11s、checkpoint 8~9s（快盘应 <1s）。
- **原因**：checkpoint/restore 本质是"把内存快照写盘/读盘"，**瓶颈在磁盘**；
  `runsc` 平台（KVM vs systrap）影响的是运行期 syscall，与快照读写无关。
  jinye 实测：VMware 虚盘写速只有 26.6 MB/s，134M 快照写盘就要 5s。
- **处理**：换快盘 / 把 `/var/lib/ateom-gvisor` 挂 tmpfs / 减小快照体积。

## 2.5 沙箱模型调用 `TRANSPORT`（根因：gVisor systrap 在 VM 上崩溃 + 版本旧）
- **现象**：对话能进沙箱，模型调用 `DeepSeek API request ... failed, code=TRANSPORT`，retry 5 次无输出。
- **根因（最终结论）**：gVisor 的 systrap 平台在 **VMware guest kernel（5.15）** 上 sentry 间歇性崩溃
  （worker 日志里 `connecting to control server ... connection refused` = 沙箱内核已死）。
  沙箱进程一死，模型调用自然失败。macOS 不崩是因为 OrbStack 定制内核 7.0.11 + arm64 gVisor。
- **为什么不是别的原因**（都已排查并排除）：
  - 不是 egress 缺失——沙箱靠直接 NAT 出网，曾多次成功返回 "ok"；
  - 不是证书——egress CA 已烤进镜像（见 #7）；
  - 不是 DNS——substrate DNS 已 forward 到集群 CoreDNS 解析真实 IP（见 #6）。
- **版本是关键**：substrate 0.0.8 + kagent 0.10.0-beta6 是老版本；新版（substrate 0.6.x / kagent 1.0.x）
  已补全 egress 网关，并有 **RevertSession** 自动恢复 CRASHED actor。**强烈建议升级，而不是在旧版上打补丁。**
- **临时缓解**（治标不治本，重启 atenet-router + 重建 golden 能恢复一次）：
  ```bash
  kubectl -n ate-system rollout restart deploy/atenet-router
  kubectl -n kagent delete actortemplate --all --ignore-not-found
  kubectl -n kagent delete pod -l ate.dev/worker-pool=kagent-default --grace-period=0 --force
  ssh <worker-node> 'rm -rf /var/lib/ateom-gvisor/actors/*'
  ```

## 3. Actor `no free workers available`
- **现象**：`ResumeActor → AssignWorker: no free workers available`，worker Pod 明明 Running。
- **原因**：两个独立原因，都会报同一句话——
  1. substrate worker 的可用容量由 Pod 的 `resources.limits.memory` 推算，没设 resources 时容量算成 0；
  2. **WorkerPool 副本太少**：每轮对话的 Actor 挂起/恢复期间一直占着 worker，1 个副本必然排队。
- **处理**：① WorkerPool 必须设 `template.resources`（至少 `limits.memory`）；
  ② **WorkerPool 至少 3 个副本**（`replicas: 3`），1 个不够用。

## 4. golden snapshot 一直不就绪（ActorTemplate 卡住）
- **现象**：`ActorTemplate golden snapshot is not ready`，反复 retry。
- **原因**：kagent 硬编码 golden 探针为 `GET :8081/readyz`，Actor 镜像没这个端点就永远不就绪。
- **处理**：Actor 镜像（a2a-shim）必须监听 `8081` 并提供 `/readyz`。

## 5. 沙箱里模型 `no API key` / 凭据不被识别
- **现象**：`llm-deepseek: no API key for provider route "deepseek-official"`。
- **原因**：新版 DSH 的凭据文件要 `version: 1 + refs:` 格式（旧平铺格式会被拒），且 `refs.DEEPSEEK_API_KEY` 要有值。
- **处理**：凭据走 `DSH_CREDENTIALS_B64`（base64 的新格式文件）+ `DEEPSEEK_API_KEY` 字面 env 双保险。

## 6. 沙箱 DNS 外部域名 REFUSED（前置修复，非 TRANSPORT 根因）
- **现象**：substrate 0.0.8 的 dns-controller 生成的 Corefile 只有内部 actor zone，外部域名 `REFUSED`。
- **处理**：外部域名 DNS 答成 atenet-router IP（沙箱流量走 egress 网关）。`make install-dependency` 已内置（`scripts/fix-dns-egress.sh`）。

## 7. 沙箱不信任 egress 网关证书（前置修复，非 TRANSPORT 根因）
- **原因**：substrate 的 egress 网关用自己的 CA（`api-ca`）签发证书做 TLS 终止；Actor 里没有这个 CA，
  也没有 `NODE_EXTRA_CA_CERTS`，于是证书校验失败。
- **处理**：把 `ate-system/ateapi-ca` 的 `ca.crt` 做成 `egress-trust` ConfigMap（key 必须叫 `trust-bundle.pem`），
  挂到 Actor 的 `/run/kagent/egress`，并设 `NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE` / `CURL_CA_BUNDLE`。
  `make install-agent` 已自动做前两步，SandboxAgent manifest 里已带 env 和挂载。

## 8. gVisor 资产下载失败（离线集群）
- **现象**：沙箱起不来，atelet 拉 runsc 报错（`anonymous open failed` / `InvalidBucketName`）。
- **原因**：runsc 从 SandboxConfig 的 `gs://gvisor/...` 下载，离线集群出不了网；且资产 URL 只认 `gs://`/`s3://`，不认 `http://`。
- **处理**：把 runsc（sha256 必须一致）上传到集群内 rustfs，把 SandboxConfig URL 改成 `s3://`。见 [install.md](install.md)「gVisor 说明」。

## 9. SandboxAgent 创建被拒
- **现象**：`must set spec.byo.deployment.cmd` / `image must be pinned with a digest`。
- **原因**：BYO 的契约要求。
- **处理**：`cmd` 是字符串 + `args` 是数组；`deploy.sh` 自动把本地镜像 digest 回填进 SandboxAgent。

## 10. Actor 里没有模型 key / kubectl 没身份
- **现象**：`llm-deepseek: no API key`；kubectl 报 `no configuration has been provided`。
- **原因**：**BYO 不会把 volumes/volumeMounts 传进 Actor**，只有 `env` 生效。
- **处理**：凭据一律走 `env` + `secretKeyRef`；kubeconfig 由 adapter 每次请求下发，shim 写入 Actor。

## 11. 请求撞上休眠就超时 / `ResumeActor` 报 `DeadlineExceeded`
两个不同的问题，症状都是"消息发不出去"：

**(a) 唤醒竞态**（`kagent-resume-race.patch` 修的）
- **现象**：第二条消息卡住；或第一条在 Actor 正在 checkpoint 时到达。
- **原因**：请求到达时 Actor 正处于 `SUSPENDING`（上一轮结束的 checkpoint 还没做完），
  kagent 只检查一次状态，看到不是 `RUNNING` 就放弃，不会重新唤醒。
- **处理**：打 `kagent-resume-race.patch`（仓库根目录），补丁做两件事：
  请求撞上 `SUSPENDING` 时等它落定再显式 `ResumeActor`；可达性等待中发现变 `SUSPENDED` 就再唤醒一次。
  构建方式见 `images/controller-fix/`。

**(b) 30s 超时**（补丁修不了）
- **现象**：`ResumeActor` 正好 29.998s 后失败，`workflow failed at step CallAteletRestore:
  while creating workload from golden snapshot: context canceled`。
- **原因**：kagent 里对 resume 有硬编码的 **30s** 上限；慢硬件上"从 golden 重建 actor"
  （restore + 沙箱创建 + dsh 启动）会超过 30s。macOS 上整套 5~8s 所以从不触发。
- **处理**：需要改 kagent 源码放宽超时后重编 controller（和上面的补丁一起做）。

## 12. 长回复被截断 / 面板报"连接中断"
- **现象**：输出到一半停了；30 秒左右最常见。
- **原因**：三处叠加——网关超时、MV3 后台 30 秒空闲被回收、SSE 链路断。
- **处理**：网关 300s；shim 心跳；扩展断线自动回落非流式并补全答案。

## 13. 清库或换 IP 后服务起不来
- **现象**：`dial tcp :6379: connection refused` 或 MOVED 重定向到空地址。
- **原因**：单节点 Redis Cluster 若没设置对外宣告地址，客户端拿到的地址是空的。
- **处理**：显式 `cluster-announce-ip` + `cluster-announce-bus-port` + `config rewrite`；
  文档里注明"换 IP 就要重新设置，不要照搬"。

## 14. 想给某个用户换一个新会话
- **现象**：早期实现里换会话要改全局环境变量，一改所有用户的历史会话同时失效。
- **原因**：没有删 Actor 的接口，只能用"整体换代"绕过卡死的 Actor。
- **处理**：会话改为 `hash(用户 + 会话标识)`，每个会话独立；`SESSION_EPOCH` 保留但不再参与推导，换会话只需新的 `conversation_id`。

## 15. 用户根本看不到自己的 Pod
- **现象**：Agent 说"你没有权限"，其实只是集群级 list 被拒。
- **原因**：kubeconfig 直连 K8s 受 RBAC 约束，`get ns` / `get pods -A` 通常被拒；
  但命名空间内往往有权限，而用户又列不出命名空间。
- **处理**：提示词要求"先用 DCE API 列可见命名空间（asl/namespaces），再 `kubectl -n <ns>`"。

## 16. 小 Agent 抢了 SRE 的活
- **现象**：要"巡检报告"，结果 Agent 自己用 kubectl 拼了一份。
- **原因**：提示词把 SRE 当"可选项"，而沙箱里又有 dce CLI 和诊断 skill。
- **处理**：preamble 写成规则——报告必须来自 `sre-cli report`；自检必须声明"不是 SRE 报告"。

## 17. kagent 1.0：`Agent` 卡在 `waiting for the ActorTemplate golden snapshot`
- **现象**：`kubectl -n kagent get agent <name>` 一直 `Ready=False`，
  message = `waiting for the ActorTemplate golden snapshot`，`reason` 在
  `ActorTemplatePending` / `ActorTemplateRetrying` 之间跳。
- **先纠正一个误区**：`ActorTemplate` **不是 K8s CRD**（`kubectl get actortemplates` 会报
  `the server doesn't have a resource type`）。它是 substrate 的 gRPC 对象，存在 ate-api 的
  PostgreSQL 里（`substrate.actor_templates`）。K8s 侧只有 `sandboxtemplates.api.kagent.dev`（另一条路径）。
- **排查顺序**（从下游往上游，不要猜）：
  1. `kubectl -n ate-system logs deploy/ate-api-server --since=15m | grep -v "Handle RPC"`
     —— 看有没有 `Failed to reconcile actor template, requeueing`。有的话 `err` 字段就是根因。
  2. `kubectl -n kagent exec <postgres-pod> -c postgresql -- psql -U postgres -d kagent \
     -c "select * from substrate.actor_templates;"` —— 模板是否创建（`proto` 里含 image / revision）。
     再看 `substrate.leases` 里对应 `lease:actortemplate:<ns>:<name>` 是否一直被续租（说明有 worker 在跑）。
  3. `kubectl -n ate-system logs <atelet-pod> --since=15m` —— 真正干活的是 atelet（node agent），
     它的 `Atelet/Run` 响应里的 `err` 字段才是第一手信息。
  4. kagent-controller 侧每秒一次 `GetActorTemplate`，`status` 为空一直等，是**症状不是原因**。
- **实测遇到的两类根因**：镜像拉不下来（见 #18）、内核不支持 overlayfs 新 API（见 #19）。

## 18. 国内集群拉不到 ghcr 镜像 → golden 快照永远建不完
- **现象**：atelet 反复
  `while creating "kagent" OCI bundle: in imageCache.EnsureImage("ghcr.io/...@sha256:..."): while waiting for pull of ...: context canceled`，
  每轮耗时精确 `4m59.98s`。
- **原因**：atelet 自己拉镜像（不复用 containerd），而 ata-api 侧那一步硬超时 **5 分钟**。
  实测 jinye 拉 ghcr.io 的 blob 只有 **39 KB/s**（65MB 镜像要 20+ 分钟），所以永远拉不完。
  注意 `curl https://ghcr.io/v2/` 返回 401 只证明 API 可达，**不代表能拉层**。
- **处理（在集群内做镜像中转，节点走内网）**：
  1. 集群内起一个 `registry:2`（HTTP）：
     `kubectl -n kagent apply -f` 一个 Deployment+Service `registry:5000`（存储用 emptyDir，重启会丢，POC 够用）。
  2. 在能出网的机器上用 `crane` 把镜像按 **amd64 子 manifest 的 digest** 推进去
     （`--platform linux/amd64` 会把子 manifest 推到 index 的 digest 下，必然报
     `DIGEST_INVALID`，所以要显式用子 digest）：
     ```bash
     kubectl -n kagent port-forward svc/registry 5000:5000 &
     crane copy --platform linux/amd64 \
       ghcr.nju.edu.cn/kagent-dev/kagent/golang-adk@sha256:<index> \
       localhost:5000/kagent-dev/kagent/golang-adk@sha256:<amd64-child>
     ```
     `ghcr.nju.edu.cn` 是国内可用镜像（实测 2.4 MB/s，55MB 层 22s）。
  3. 给 atelet 加一条 flag，让它把 `localhost:5000/...` 改写成集群内 Service 并且用明文 HTTP
     （源码里 `parseRef` 对改写过的 ref 用 `name.Insecure`）：
     ```bash
     kubectl -n ate-system patch ds atelet --type=json \
       -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--localhost-registry-replacement=registry.kagent.svc:5000"}]'
     ```
  4. 把 `Harness.spec.workload.image` 改成 `localhost:5000/kagent-dev/kagent/golang-adk@sha256:<amd64-child>`。
     revision 变了会自动生成新的 `ActorTemplate`，旧的在 `runtime-revision-gc` 里回收。
- **注意**：`docker pull` + `docker push` 会重建 manifest，digest 对不上；`crane`/`skopeo --all` 才保 digest。

## 19. 内核 5.15 跑不了 substrate 1.0：overlayfs `lowerdir+` 不支持（硬门槛）
- **现象**：镜像拉取解决后，atelet 报
  `while calling worker RunWorkload: ... while composing "kagent" rootfs: while mounting overlay rootfs at ".../bundles/kagent/rootfs": while creating overlay superblock: invalid argument`。
- **原因**：substrate 1.0 用**新挂载 API**（`fsopen`+`fsconfig`+`fsmount`）拼 rootfs，
  为了绕开 `mount(2)` 单页选项串上限（约 34 层），它用的是 `lowerdir+` 这个 key。
  `lowerdir+` 需要 **Linux 6.7+，或回填到 6.6.23+**；Ubuntu 22.04 默认 5.15 没有。
  （代码注释见 `internal/imagecache/bundle_linux.go` 的 `mountOverlay`。）
- **上游依据（已核对，不是推测）**：
  - substrate `v1.0.0` 的 `internal/imagecache/README.md:161` 原文：
    "**Minimum supported kernel: Linux 6.7, or 6.6.23** (`lowerdir+`, backported to 6.6.y)"。
  - 当前 `main` 分支同文件同一行仍是这句话，`bundle_linux.go` 仍然只有 `set("lowerdir+")`，
    **没有 mount(2) 回退分支**（grep 过 `fallback|mount(2)|legacy`，无）。
  - 代码里**没有启动时的内核版本检查**（grep 不到 `uname`/`kernelVersion` 门禁），所以是
    fail-late：装得上、跑得起来，只在第一次组 rootfs 时炸一个裸 `EINVAL`。
  - 上游 quickstart 没写这个前置条件（只提了内核邻居表），所以只能自己踩出来。
- **定位方法（别再猜内核）**：在节点上起一个 privileged + hostPath 的 pod，跑一个探针程序，
  用 `fsopen/fsconfig/fsmount` 分别试 `lowerdir` 和 `lowerdir+`：
  ```
  plain lowerdir            OK
  plain lower+upper         OK
  lowerdir+ single          create: invalid argument
  lowerdir+ pair            create: invalid argument
  ```
  这就把「内核不支持」和「目录/权限不对」彻底分开了。
- **必须满足的前置条件**：**节点内核 ≥ 6.6.23（建议 6.8）**。
  Ubuntu 22.04 升级路径：
  ```bash
  # 在节点上（可用 privileged pod chroot /host 执行）
  apt-get update && apt-get install -y linux-generic-hwe-22.04
  update-grub && reboot        # 保留 5.15 作为 grub 回退项
  ```
  重启后 `kubectl get node -o wide` 确认 `KERNEL` 变成 6.8.x，kagent 会自动重试 golden 快照。
- **不要**为了绕这个去改 substrate 源码回退 `mount(2)`：层数一多就会撞页面大小限制，
  而且下游升级要一直背这个补丁。

## 20. 集群里的 substrate 被"重装"之后，一堆东西会静默失效
- **现象**：本来跑通的栈，过一段时间 `Agent` 还在、`Ready=True`，但发消息一直
  `input was not accepted; retry after the session becomes available`。
- **原因**：有人/某个平台侧把 substrate 重新 `helm install` 了一遍，于是：
  1. `ate-system` 的 CRD 被重建 → `WorkerPool`、`SandboxConfig` 等**对象全没了**；
  2. 我们给 Deployment 打的 **pod 级 sysctl 补丁也没了** → `ate-api-server` 立刻
     `listen tcp 0.0.0.0:443: bind: permission denied` 而 CrashLoop；
  3. 旧 Helm release 的遗留对象（CRD / ResourceQuota / ServiceAccount）会让
     `kagent install` 报 `invalid ownership metadata ... cannot be imported`。
- **处理**：
  - CRD 遗留：给 CRD 补 `app.kubernetes.io/managed-by=Helm` 和
    `meta.helm.sh/release-name/-namespace` 三个标注，让它能被新 release 接管；
  - 其它遗留对象（如 `atelet-critical-pods` ResourceQuota）直接删，让 chart 重建；
  - 补回 sysctl 补丁（`ate-api-server` 和 worker 的 `kagent-default` 两个 Deployment）；
  - **`WorkerPool` 需要自己重建**（chart 默认 `substrateWorkerPool.create=false`），
    参考 `templates/substrate-workerpool.yaml`，至少要 `replicas / workerImage /
    sandboxClasses[].configRef / template.serviceAccountName / template.resources`。
- **教训**：判断"栈还活着"不能只看 `Agent Ready=True`，要同时看
  `kubectl -n ate-system get pods`、`kubectl -n kagent get workerpool` 和
  `substrate.leases` 里有没有 actor 在续租。`Agent Ready` 只代表 golden 快照在。

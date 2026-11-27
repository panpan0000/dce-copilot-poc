# 新版本踩坑：substrate 1.0 + kagent 1.0

> 这份文档只讲**新栈**（substrate 1.0.0 / kagent 1.0.0）和旧栈（0.0.8 / 0.10.0-beta6）的差异与限制。
> 旧栈的问题看 [gotchas.md](gotchas.md)；新栈的逐条排查过程看 gotchas 的 #17 / #18 / #19。

## 一句话结论

新栈解决的是旧栈「gVisor systrap 在 VMware 上崩 → `TRANSPORT`」那类问题，
但它把**运行环境门槛**抬高了：**内核 ≥ 6.6.23、K8s ≥ 1.37、Helm 3**，且**镜像必须能在集群内拉得动**。
这三条任何一条不满足，`Agent` 会一直 `Ready=False`，而且报错信息都指向错误的方向。

## 一、硬性前置条件（不满足就别开始）

| 条件 | 要求 | 不满足的症状 | 能否绕过 |
|---|---|---|---|
| **内核** | **≥ 6.6.23**（建议 6.8） | `atelet` 报 `while creating overlay superblock: invalid argument`；`Agent` 卡在 `waiting for the ActorTemplate golden snapshot` | ❌ 不能。上游明确写死，无回退分支 |
| **K8s** | **≥ 1.34**，并开启 3 个 feature gate + `certificates.k8s.io/v1beta1` | substrate 依赖 `PodCertificateRequest`、`ClusterTrustBundle`、`ClusterTrustBundleProjection` | ✅ 低版本**手开 gate 即可**；1.37 起默认开启，所以升到 1.37 只是省掉手工配置 |
| **Helm** | **3.x**（不是 4.x） | `kagent install` 报 `rendered controller deployment has no service account` / `invalid Helm values` | ✅ 降级 Helm 到 3.22 即可 |
| **镜像可达** | 集群能拉 `ghcr.io/kagent-dev/...` | atelet 每轮精确 5 分钟超时：`imageCache.EnsureImage(...): while waiting for pull ...: context canceled` | ✅ 集群内镜像中转（见第三节） |
| **模型 provider** | kagent CLI 的 provider 枚举里**没有 DeepSeek** | `kagent install` 报 `OPENAI_API_KEY is not set` | ✅ 用 OpenAI 兼容的 `ModelConfig` + `openAI.baseUrl` 指到 DeepSeek |

内核门槛的上游原文（`substrate` `v1.0.0` 与当前 `main` 同一行，`internal/imagecache/README.md:161`）：

> **Minimum supported kernel: Linux 6.7, or 6.6.23** (`lowerdir+`, backported to 6.6.y)

Ubuntu 22.04 升级（**并存安装，5.15 保留在 grub 里做回退**）：

```bash
apt-get update && apt-get install -y linux-generic-hwe-22.04   # 装到 6.8.x
update-grub            # 包本身会触发；确认 6.8 的 menuentry 在 5.15 之前
reboot
kubectl get node -o wide     # KERNEL 应变成 6.8.x
```

> ⚠️ **只升承载 worker 的节点**。如果 GPU 节点和 master 是同一台，不要动它：
> vGPU/GRID 驱动对内核版本敏感，换内核要重装驱动，风险远大于收益。

### 关于 K8s 版本：1.35 能跑，不是非 1.37 不可

上游自己的开发脚本 `hack/create-kind-cluster.sh` 里写得很清楚
（注释原文："They are not enabled by default as of Kubernetes v1.36"）：

```yaml
featureGates:
  ClusterTrustBundle: true
  ClusterTrustBundleProjection: true
  PodCertificateRequest: true
runtimeConfig:
  "certificates.k8s.io/v1beta1": "true"
```

也就是说：

- **硬要求是这几个特性可用**（`PodCertificateRequest` 从 1.34 起是 alpha），**不是某个具体版本**。
- 在 **1.34 / 1.35 / 1.36** 上，只要给 kube-apiserver / kube-controller-manager /
  kube-scheduler / kubelet 加 `--feature-gates=...`，并给 apiserver 加
  `--runtime-config=certificates.k8s.io/v1beta1=true`，就能跑通。
  **社区在 1.35 上跑通是合理的。**
- **1.37 起这些默认开启**：本集群 1.37.3 的 apiserver 命令行里**没有任何** `--feature-gates`
  或 `--runtime-config` 参数，`/apis/certificates.k8s.io/v1beta1` 直接可用。
- 所以「升到 1.37」的真实收益是**省掉手工开 gate、少一次配置分叉**，属于工程取舍，
  不是 substrate 的硬性依赖。反过来，1.35 上跑要注意 controller-manager 和 kubelet 也要开，
  只开 apiserver 会出现「Pod 一直 Pending、证书签不出来」。

## 二、新栈的 API 形状（和 0.10 完全不一样）

`api.kagent.dev/v1alpha3` 把「运行时」和「Agent 定义」拆开了：

| 对象 | 作用 | 关键字段 |
|---|---|---|
| `Harness` | 运行时：跑在哪个 worker 池、用什么镜像、快照存哪 | `spec.workload.image`、`spec.substrate.workerPoolRef`、`spec.substrate.snapshotPolicy.location` |
| `AgentTemplate` | 模型 + 系统提示词 | `spec.modelConfig`、`spec.systemPrompt` |
| `Agent` | 把上面两个「组装」起来 | `spec.template.ref` + `spec.harnessRef` |

旧栈的 `SandboxAgent` / `Agent`（0.10）写法和这些**不兼容**，`substrate-adapter` 之类的组件要跟着改。

> 注意：`ActorTemplate` **不是 K8s CRD**。它是 substrate 的 gRPC 对象，落在 ate-api 的
> PostgreSQL（`substrate.actor_templates` 表）。`kubectl get actortemplates` 会直接报
> `the server doesn't have a resource type`，别再在那儿找。

`Agent` 的 condition 依次是：`Accepted` → `EgressPoliciesApplied` → `ResolvedRefs` → `Compatible` → `Ready`。
前四个都是 `True` 而 `Ready=False: waiting for the ActorTemplate golden snapshot`，说明**配置没问题，是运行时（substrate/atelet/内核）的问题**。

## 三、国内集群：镜像拉不动是必踩的一关

atelet **自己拉镜像**（不复用 containerd 的镜像存储），而 ate-api 那一步硬超时 **5 分钟**。
实测 jinye 到 `ghcr.io` 的 blob 只有 **39 KB/s**，65MB 的镜像要 20 多分钟 → 永远拉不完。

> 判别技巧：`curl https://ghcr.io/v2/` 返回 401 只证明「API 可达」，**不代表层能拉下来**。
> 一定要测 blob 的实际速度。

推荐做法：集群内起一个 `registry:2`（HTTP），节点走内网拉。

```bash
# 1) 集群内 registry（POC 用 emptyDir 即可，但重启会丢镜像，见下方注意）
kubectl -n kagent apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: local-registry, namespace: kagent}
spec:
  replicas: 1
  selector: {matchLabels: {app: local-registry}}
  template:
    metadata: {labels: {app: local-registry}}
    spec:
      containers:
      - name: registry
        image: registry:2.8.3
        ports: [{containerPort: 5000}]
---
apiVersion: v1
kind: Service
metadata: {name: registry, namespace: kagent}
spec:
  selector: {app: local-registry}
  ports: [{port: 5000, targetPort: 5000}]
EOF

# 2) 用 crane 按「amd64 子 manifest 的 digest」推（docker pull/push 会重建 manifest，digest 对不上）
kubectl -n kagent port-forward svc/registry 5000:5000 &
crane copy --platform linux/amd64 \
  ghcr.nju.edu.cn/kagent-dev/kagent/golang-adk@sha256:<index-digest> \
  localhost:5000/kagent-dev/kagent/golang-adk@sha256:<amd64-child-digest>

# 3) 让 atelet 把 localhost:5000 改写成集群内 Service，并用明文 HTTP
kubectl -n ate-system patch ds atelet --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--localhost-registry-replacement=registry.kagent.svc:5000"}]'

# 4) Harness 的镜像改成内网地址
kubectl -n kagent patch harness <name> --type=merge \
  -p '{"spec":{"workload":{"image":"localhost:5000/kagent-dev/kagent/golang-adk@sha256:<amd64-child-digest>"}}}'
```

几个坑：

- `--platform linux/amd64` + 目标写 **index digest** → 必然 `DIGEST_INVALID`。目标要写 **amd64 子 manifest 的 digest**。
- `registry:2` 用 `emptyDir` 时，**节点重启/registry Pod 重建后镜像就没了**，要重推一次；
  长期用改成 hostPath 或给个 PVC。
- `ghcr.nju.edu.cn` 是本次实测可用的国内镜像（2.4 MB/s）；`ghcr.dockerproxy.com` 超时，`ghcr.geekery.cn` 域名不解析。

## 四、安装顺序与版本对齐

`kagent` CLI 1.0.1 的安装顺序是固定的：
`kagent-crds → substrate-crds → podcert → bundled postgres → substrate → atelet → kagent`。
手工 `helm install` 时也必须按这个顺序，否则 controller 起不来（缺 CRD / 缺 cert 签发的 signer）。

版本对照：

| 组件 | 本次验证的版本 |
|---|---|
| Kubernetes | 1.37.3 |
| substrate chart | 1.0.0 |
| kagent chart | 1.0.0 |
| kagent CLI | 1.0.1 |
| Helm | 3.22.1（**4.x 会挂**） |
| 内核 | 6.8.0-40（升级前 5.15 跑不了） |

## 五、验证清单（怎么判断这栈是真的通了）

```bash
# 1) 控制面
kubectl -n ate-system get pods          # ate-api-server / ate-controller / atelet / atenet-* 全 Running
kubectl -n kagent   get pods            # controller / worker / postgresql / tools / ui 全 Running

# 2) Agent 对象
kubectl -n kagent get agent <name> -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
# 期望：Ready=True, message=ActorTemplate golden snapshot is ready

# 3) golden 快照真的建出来了（看 PG，不是看 K8s）
kubectl -n kagent exec <postgres-pod> -c postgresql -- psql -U postgres -d kagent \
  -c "select atespace, name, version from substrate.actor_templates;"

# 4) 真跑一条对话
kagent agent invoke ... / 或走 kagent UI
```

状态字段含义：

| `Ready.reason` | 含义 | 该看哪 |
|---|---|---|
| `ActorTemplatePending` / `ActorTemplateRetrying` | 正在建 golden 快照 / 建失败在重试 | ate-api-server 日志 + atelet 日志（gotchas #17） |
| `Ready` | 快照就绪 | 可以对话了 |

## 六、还没验证 / 已知限制

- **GPU 没参与**：本次集群的 GPU（Tesla-P4 **vGPU**）在 master 上，且 master 带 `demo` +
  `node.kubernetes.io/disk-pressure` 两条 taint，`nvidia.com/gpu` 的 capacity **是 0**——
  也就是说这个「GPU 集群」目前对 K8s 是 0 张卡可用。整套 POC 走的是 CPU + gVisor，不依赖 GPU。
- **真实对话链路尚未在这套新栈上重跑**：`substrate-adapter` 还停在 kagent 0.10 的 API，
  要迁到 1.0 的 `Harness/AgentTemplate/Agent` 才能端到端。
- **快照存储**：`snapshotPolicy.location` 指向集群内 rustfs（`s3://ate-snapshots/...`），
  不是云对象存储；换环境要改。
- **`registry:2` 是 POC 级**：单副本 + emptyDir，没有 GC、没有 TLS、没有认证。

## 七、适配器从 0.10 迁到 1.0（已实测跑通）

`substrate-adapter` 原来直接对 kagent 0.10 的 sandbox A2A 入口做转发，把
「由 JWT 身份推导出的会话 id」当 `contextId` 塞给上游。1.0 里这条路走不通了，
三处协议差异必须一起改（只改一处会得到很难懂的报错）：

| 差异 | 0.10 | 1.0 | 不改的报错 |
|---|---|---|---|
| A2A 路径 | `/api/a2a-sandboxes/<ns>/<agent>/` | **`/agents/<ns>/<agent>`** | 404 / 空 |
| 协议版本 | 无 | 必须 `A2A-Version: 1.0` | `this version is not supported: requested "0.3", served "1.0"` |
| 方法名 | `message/send`、`message/stream` | **`SendMessage`、`SendStreamingMessage`** | `method not found` |
| 消息角色 | `"user"` | **`"ROLE_USER"`**（枚举） | `message role cannot be unspecified` |
| 会话 | 调用方自选 `contextId` | **服务端创建 Session**，`contextId` 由服务端生成 UUID | `not found` / `input was not accepted` |

### 会话映射（关键）

1.0 不允许调用方命名 Session，所以适配器改成：

1. 仍然用 `hash(身份 + conversation_id)` 算出**逻辑** `sid`（对外契约不变）；
2. 本地维护 `sid -> contextId` 映射，落盘到 `SESSION_MAP_FILE`（默认 `/data/adapter-sessions.json`）；
3. 第一次访问某会话时**不带 `contextId`**，让上游新建 Session，并从流里回填 `contextId`；
4. 之后都带上这个 `contextId`，同一个 `conversation_id` 就会命中同一个沙箱；
5. 适配器重启后从文件恢复映射，不会把同一个会话拆成两个沙箱。

> 局限：文件在 Pod 的 `emptyDir` 里，**Pod 重建**（不是容器重启）仍会丢映射。
> 要做到完全无状态，应改成调 gRPC `SessionService/CreateSession` 并用
> `request_id = hash(身份+会话)` 做幂等（服务端按 request id 去重）。

### 新建会话的竞态

新建 Session 时 actor 还在拉起，上游会先回一个
`KAGENT_SEND_NOT_ACCEPTED / "input was not accepted; retry after the session becomes available"`。
适配器对**首帧**是这个错误的请求整轮重试（最多 12 次、间隔 300ms），
一旦已经开始向浏览器发流就不再重试，避免前端看到"半截回复"。

### 身份分区

1.0 用 `X-User-Id` 头划分会话归属。适配器把 DCE 的 `identityOf(sub, preferred)`
（例如 `alice`）塞进这个头，于是**每个 DCE 用户只看到自己的会话**，
这也正是 BYOA 要的隔离语义。

## 八、端到端验证结果（2026-11-27，jinye）

链路：`curl/扩展 → kgateway(/sandbox) → substrate-adapter → kagent 1.0 A2A → substrate actor(gVisor) → DeepSeek`

| # | 用例 | 结果 | 耗时 |
|---|---|---|---|
| 1 | 新会话首问 `9+9` | `9+9 等于 18。` | 7.5s（含冷启沙箱） |
| 2 | 同会话追问"刚才我问什么" | `9+9 等于几？`（历史正确） | 1.7s |
| 3 | 另一个会话问"我们之前聊过什么" | `无。`（会话隔离） | 2.0s |
| 4 | 非流式 `/poc/chat` | `5+5 等于 10。` | 1.5s |
| 5 | 经 kgateway `/sandbox/poc/chat/stream` | 逐个 delta 流式返回 `7+7 等于 14。` | 2.5s |
| 6 | 会话映射落盘 | `session mapping u-xxx -> 01a21b35-...`（adapter 日志） | — |

扩展侧契约没变：`service_worker.js` 仍然调 `/sandbox/poc/whoami` 和 `/sandbox/poc/chat`，
所以浏览器那一端不需要改，只要 `gatewayBase` 指到 `http://<节点IP>:<NodePort>`。

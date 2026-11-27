# dce-copilot-mini-poc

**BYOA（Bring Your Own Agent）for DCE** —— 每个 DCE 用户，在浏览器里和自己的沙箱 Agent 对话；
Agent 用这个用户自己的身份调用 DCE API 和 kubectl。

> BYOA = 能力归属不变、凭证归属不变、审计归属不变。
> 共享一套集群和模型，但每个用户的 Agent 有自己的文件系统、自己的进程、自己的身份。
> 不是「一个 Agent 服务所有人」，而是「每人生成一个 Agent，用完即挂起」。

一张表说清区别：

| | 共享 Copilot | **BYOA（本项目）** |
|---|---|---|
| 谁能用 | 所有人共用一个会话 | 每个用户一个会话 |
| 身份 | Agent 用服务账号，再「假装」成用户 | Agent 直接用用户自己的 JWT / kubeconfig |
| 越权 | 靠提示词和代码审查 | 由 DCE 与 Kubernetes 的 RBAC 拒绝 |
| 数据 | 同一个进程里混着 | 各自沙箱，快照隔离 |
| 成本 | 常驻 | 空闲挂起，恢复约 0.3 秒 |

---

## 架构

```
浏览器(DCE 页面 + 扩展)
   │  Authorization: Bearer <用户 JWT>
   ▼
kgateway (Envoy)         校验 JWT、限流/入口策略
   │
   ▼
substrate-adapter        再验一次 JWT（不信任网关）
   │                     会话 = hash(用户名 + 会话标识)
   │                     用用户 JWT 换 1h kubeconfig
   ▼
kagent controller        A2A 入口，把请求送到该会话的 Actor
   │
   ▼
Substrate Actor         每个会话一个 gVisor 沙箱，空闲即挂起（快照恢复 ~0.3s）
   ├── DSH                对话 + 工具执行
   ├── dce CLI            以用户身份调 DCE OpenAPI
   ├── kubectl            用用户自己的 kubeconfig
   └── sre-cli            经 agentgateway → 独立 SRE Agent
```

为什么这么绕：浏览器是 HTTP + JWT，kagent 的沙箱入口是 A2A，DSH 又只暴露本地接口。
adapter 和 shim 就是这三段之间的两座桥。**它们是本 POC 的实现，不是官方能力。**

## 名词表

| 词 | 指的是 |
|---|---|
| **DCE** | DaoCloud Enterprise，你的企业云平台（本项目依赖它的登录、权限、集群托管） |
| **DSH** | DeepSeek Harness，跑在沙箱里的 Agent 运行时（对话 + 工具调用） |
| **Actor** | 一个用户的沙箱实例。一个会话一个，空闲即挂起，下次恢复约 0.3 秒 |
| **Substrate** | 管理 Actor 生命周期的组件（创建、挂起、快照、路由） |
| **kagent** | 把 A2A 协议请求转给 Actor 的控制器 |
| **kgateway** | 浏览器请求的入口（校验 JWT、限流、超时） |
| **A2A** | Agent-to-Agent 协议，kagent 的入口协议 |

---

## 快速开始

面向**全新 x86（linux/amd64）服务器集群**。默认使用 `release-ci.daocloud.io/common-ci` 镜像仓库和
`demo-tf-alpha.daocloud.cn` 这套 DCE 作为示例，换成你自己的即可。

## ⚠️ 性能前提：快照读写走磁盘，磁盘速度才是关键

**这一条决定对话是 3 秒还是 60 秒，装之前务必先看清。**

substrate 的 Actor 是「每轮对话结束就挂起、下轮恢复」的。挂起/恢复 = gVisor 把
**Actor 的内存快照写盘 / 读盘**：

```
挂起（checkpoint） = 把内存 dump 成文件 + 写盘
恢复（restore）    = 读文件 + 重建内存
```

所以 **checkpoint/restore 本质是磁盘 I/O，瓶颈在磁盘，不在 gVisor 平台。**

### gVisor 平台（KVM vs systrap）到底影响什么

`runsc` 默认平台是 `systrap`（不会自动识别 KVM，要用 KVM 必须显式传 `--platform=kvm`，
见 [gVisor: Changing Platforms](https://gvisor.dev/docs/user_guide/platforms/)）。

但平台影响的是**沙箱运行时的系统调用拦截开销**，**对"把内存快照写盘"这件事关系不大**。
官方也明确：KVM 只在物理机最快，虚拟机里 systrap 往往更好。

> 一句话：**平台影响"跑得快不快"，磁盘影响"挂起/恢复快不快"。** 这是两件事。

### 实测：磁盘约 60%，其余是 CPU 逐页序列化

**jinye（VMware 虚机，134M 快照）：**

```
checkpoint:  磁盘 8.8s  →  tmpfs 3.5s     （去掉磁盘省 5.3s ≈ 60%）
restore:     磁盘 11s   →  tmpfs 6.8s
节点磁盘:     写 26.6 MB/s、读 67.7 MB/s（dd 直写，VMware 虚拟盘）
```

**macOS / OrbStack（arm64，同一份 runsc CLI 直测，不经过 kagent/substrate）：**

```
checkpoint 134M = 160ms   ← 落盘，不是 tmpfs
```

**所以差异拆成两部分：**

| 部分 | 占比 | 依据 |
|---|---|---|
| 磁盘 I/O | **~60%** | 8.8s → 3.5s（tmpfs 实测），134M @ 26.6 MB/s ≈ 5s |
| CPU 逐页序列化 | **~40%** | 3.5s 远大于纯 memcpy 的 0.1s（1378 MB/s 实测），且 tmpfs 已排除磁盘 |

> **注意：剩下那 40% 不是"内存带宽"。** jinye 单线程 memcpy 1378 MB/s，134M 只要 0.1s；
> 而 checkpoint 要 3.5s，慢 35 倍 —— 说明瓶颈是 gVisor **逐页走页表/编码状态**的 CPU 开销，
> 不是带宽。Apple Silicon 单核强，所以 macOS 只要 160ms。

### 参考延迟构成（macOS 实测，来自现场文档）

```
快照恢复约 0.3s + 模型约 3s + 控制面恢复与建路由约 4–5s  →  端到端 5–8s
```

jinye 上光是快照恢复就要 6.8~11s（是 macOS 的 20~35 倍），所以端到端会到 30~60s。

### 怎么优化（按性价比）

| 方案 | 效果 | 代价 |
|---|---|---|
| **换快盘**（SSD datastore / NVMe） | 8.8s → 1s 内 | 基础设施：把 VM 迁到 SSD 存储 |
| **快照目录挂 tmpfs（内存盘）** | 写入变内存速度，~1s | 吃内存（节点内存要够装快照） |
| **减小快照体积**（减 actor 内存） | 按比例降 | 治标 |

**首选 tmpfs**：把 `/var/lib/ateom-gvisor` 挂到 tmpfs，绕开慢盘，不用等基础设施改造。

### 0. 前置条件

| 项 | 说明 |
|---|---|
| 一个 x86 K8s 集群 | 能装 CRD，有默认 StorageClass |
| **节点磁盘要快**（SSD/NVMe） | **checkpoint/restore 的瓶颈在磁盘，见上一节** |
| **WorkerPool 至少 3 副本** | 每个 Actor 挂起/恢复期间占着一个 worker，1 个副本必然排队 → `no free workers` |
| 一个 DCE 环境 | 身份、权限、集群托管都来自 DCE；没有就先跑 `make mock-dce` |
| 能拉 `release-ci` 镜像 | 集群节点要能匿名拉 `release-ci.daocloud.io`（已公开） |
| DeepSeek 模型 Key | 放 `secrets/.env.secret`，不进 git |

### 1. 配置文件

```bash
cp config.env.example .env
$EDITOR .env          # 至少确认：DCE_ORIGIN / DCE_CLUSTER / JWT_ISSUER / JWT_AUDIENCE
                      #            IMAGE_REGISTRY / IMAGE_TAG / GATEWAY_MODE

mkdir -p secrets
cp secrets.env.example secrets/.env.secret
$EDITOR secrets/.env.secret     # 填 DEEPSEEK_API_KEY（形如 sk-xxxx，来自 DeepSeek 开放平台）
```

**关键字段怎么填**（最容易卡住的地方）：

| `.env` 变量 | 是什么 | 示例 |
|---|---|---|
| `DCE_ORIGIN` | 你平时登录 DCE 控制台的浏览器地址（不带末尾 `/`） | `https://demo-tf-alpha.daocloud.cn` |
| `DCE_CLUSTER` | DCE 里托管的 K8s 集群名 | `jinye-gpu-cluster` |
| `JWT_ISSUER` / `JWT_AUDIENCE` | DCE 签发 token 的 issuer / audience | 解开登录 token 看 `iss` / `aud` |
| `IMAGE_REGISTRY` | 集群能拉到的镜像仓库 | `release-ci.daocloud.io/common-ci` |
| `GATEWAY_MODE` | 集群没 LoadBalancer 就填 `nodeport` | `nodeport` |

读 `iss` / `aud`：在已登录的 DCE 页面按 F12，控制台执行 `copy(localStorage.getItem('ANAKIN_TOKEN'))`，
粘到 jwt.io 看 payload。这一步只是为了 `make verify` 能在命令行验证；日常使用扩展自己会读 token。

### 2. 自检

```bash
make check
```

逐项检查配置、命令、DCE 连通性、集群 StorageClass/CNI。**不过就别往下走。**

### 3. 装基础组件

```bash
make install-dependency
```

装 Gateway API CRD → kgateway → substrate → kagent，并自动修复 substrate 沙箱出网（外部域名走 egress 网关）。
开头会预检 gVisor / 默认 StorageClass，缺什么当场报错。

### 4. 构建 + 部署

```bash
make build          # 构建 dsh-a2a / sre-agent / adapter（amd64），并推到 IMAGE_REGISTRY
make install-agent  # 部署 adapter / Gateway / SandboxAgent / SRE
```

`make install-agent` 结束会打印入口地址，例如 `http://<节点IP>:30080/sandbox`。
**忘了地址随时查**：`make url`（或 `kubectl -n watsons-agent-poc get svc poc-gateway -o wide`）。

### 5. 加载浏览器扩展

扩展在仓库的 **`extension/`** 目录（当前版本 **0.6.0**）。

1. Chrome 打开 `chrome://extensions` → 开启**开发者模式** → **加载已解压的扩展程序**
2. 选择仓库里的 `extension/` 目录 → **首次安装会自动弹出设置页**
3. 在设置页里**只需要填一项**：
   - **入口网关地址**：`make install-agent` 打印的那个（`make url` 可再查）
   - DCE 地址留空即可（扩展会直接用当前页面的地址）
4. 保存 → 打开 DCE 登录 → 刷新页面 → 右下角出现「当前用户 <你的用户名>」的面板

> **换 DCE 时**：`extension/manifest.json` 的 `content_scripts[0].matches` 要加上你的 DCE 域名
> （默认只有 `https://demo-tf-alpha.daocloud.cn/*`）。改完在 `chrome://extensions` 点刷新即可。

### 6. 验证

```bash
TOKEN_FILE=~/token.txt make verify
```

`~/token.txt` 放当前 DCE 用户的 JWT（仅用于命令行验证）。9 条全 PASS 才算装好。

---

## 已验证（2026-11，真实 DCE `demo-tf-alpha` + x86 集群 jinye）

| 项 | 结果 |
|---|---|
| 未带 token 被网关拒绝 | PASS（401） |
| whoami 解析到用户 | PASS |
| 自然对话（非流式 + 流式） | PASS，真实模型回答 |
| 浏览器面板 | PASS（显示当前用户、能对话） |
| 沙箱出网（模型 API） | ⚠️ 能通，但间歇性 `TRANSPORT`（gVisor sentry 崩溃，见下节「版本结论」） |

> 完整 9 项 `make verify` 需用当前有效 token 重跑。核心链路（身份 + 对话 + 面板）已实测通过；出网受 gVisor sentry 崩溃影响不稳定。

## ⚠️ 已知限制与版本结论（重要，先看这个）

### 前置条件：节点内核必须 ≥ 6.6.23（substrate 1.0 硬门槛）

substrate 1.0 用新挂载 API（`fsopen`/`fsconfig`/`fsmount`）拼沙箱 rootfs，靠 `lowerdir+`
绕开 `mount(2)` 的单页选项上限；`lowerdir+` 需要 **Linux 6.7+ 或 6.6.23+**。
Ubuntu 22.04 默认 5.15 **跑不起来**，症状是 `Agent` 卡在
`waiting for the ActorTemplate golden snapshot`，atelet 报
`while creating overlay superblock: invalid argument`。

已在 jinye 节点上用 `fsopen/fsconfig` 探针实测：`plain lowerdir` OK、`lowerdir+` 必然 `EINVAL`。
升级方式（Ubuntu 22.04，保留 5.15 作为 grub 回退）：

```bash
apt-get update && apt-get install -y linux-generic-hwe-22.04
update-grub && reboot
kubectl get node -o wide          # KERNEL 应为 6.8.x
```

另外：国内集群拉 `ghcr.io` 只有 ~39KB/s，而 atelet 拉镜像这一步硬超时 5 分钟。
需要在集群内做镜像中转（`registry:2` + `crane` 保 digest 推送 + atelet 的
`--localhost-registry-replacement`）。完整步骤见
[docs/gotchas.md](docs/gotchas.md) 的 #17 / #18 / #19。

### 核心结论（旧版本 0.0.8）：版本旧 + gVisor 在 VMware 上不稳

这套 POC 用的 substrate **0.0.8** + kagent **0.10.0-beta6** 是老版本；当前 substrate 已到 0.6.x、kagent 已到 1.0.x。

在 jinye（VMware 虚机，内核 5.15）上实测，**gVisor 的 systrap 平台 sentry 会间歇性崩溃**
（worker 日志 `connecting to control server ... connection refused` = 沙箱内核已死），
导致沙箱进程死亡、模型调用 `TRANSPORT`。这是「gVisor systrap 跑在 VMware guest kernel 上」的固有问题，不是配置能修的。

| 环境 | 内核 | gVisor | 结果 |
|---|---|---|---|
| macOS / OrbStack | 7.0.11-orbstack（定制） | arm64 | 稳定，5~8s/条 |
| jinye / VMware | 5.15.0-143-generic | amd64 systrap | sentry 间歇崩溃，30~90s 或 TRANSPORT |

- 新版 kagent 有 **RevertSession** 自动恢复 CRASHED actor；beta6 没有。
- **强烈建议：升级 substrate/kagent，而不是在旧版上打补丁。**

### 无 KVM 时的实测表现

| 场景 | 表现 |
|---|---|
| 新会话第 1 条消息 | 30~50 秒（冷启动 + 软件快照） |
| 同一会话第 2 条 | 有时 5~7 秒，有时超时 |
| 连续多条 | 容易 `no free workers` |

### 临时缓解（治标不治本）

```bash
kubectl -n ate-system rollout restart deploy/atenet-router
# 重新触发 golden 快照：改 Harness 的 image（或删了重建 Agent），revision 变了会自动新建 ActorTemplate
kubectl -n kagent delete agent <name> && kubectl apply -f <agent>.yaml
kubectl -n kagent delete pod -l ate.dev/worker-pool=kagent-default --grace-period=0 --force
ssh <worker-node> 'rm -rf /var/lib/ate/actors/*'
```

> 注意：`ActorTemplate` **不是 K8s CRD**，`kubectl delete actortemplate` 会报
> `the server doesn't have a resource type`；它是 substrate 的 gRPC 对象，存在
> ate-api 的 PostgreSQL（`substrate.actor_templates`）里。

## 已经解决过的问题（不需要你再踩）

| 问题 | 结论 |
|---|---|
| Actor 起不来 / `no free workers` | WorkerPool 必须设 `template.resources.limits.memory`，否则 worker 容量算成 0 |
| 沙箱起不来，`runsc` 资产拉不到 | gVisor 的 runsc 从 `gs://gvisor` 下载，出不了网就失败；离线环境要本地化 |
| golden snapshot 一直不就绪 | Actor 镜像必须监听 `:8081/readyz`（kagent 硬编码的探针）；kagent 1.0 另有两类根因：镜像拉不下来（见 gotchas #18）、内核 <6.6.23 不支持 overlayfs `lowerdir+`（见 #19） |
| 沙箱里模型报 `no API key` | 凭据用新版格式 `version:1 + refs:`，且补 `DEEPSEEK_API_KEY` env |
| 沙箱里模型报 `TRANSPORT` | 根因是 gVisor systrap 在 VMware 上 sentry 崩溃（见上节），非 DNS/证书/egress；建议升级 |
| 请求撞上 Actor 休眠就超时 | controller 打了唤醒竞态补丁（`images/controller-fix/`） |
| Valkey 重置后适配器起不来 | 单节点集群要显式设 `cluster-announce-ip` / bus-port |
| 换会话要改环境变量、全员历史失效 | 会话 = hash(用户 + 会话标识) |

细节见 [docs/gotchas.md](docs/gotchas.md)。

## 镜像

| 镜像 | 内容 |
|---|---|
| `dsh-runtime` | DSH 运行时（dsh + kubectl + python + git），amd64，已推到仓库 |
| `dsh-a2a` | Actor 运行时：DSH + A2A shim + `dce` CLI + skills |
| `sre-agent` | 独立 SRE Agent |
| `adapter` | 身份→会话、SSE 流式、DCE kubeconfig |

镜像名形如 `release-ci.daocloud.io/common-ci/dce-copilot-dsh-a2a:0.2.0-x86`（amd64）。
`dsh-runtime` 是基础镜像，通常只需构建一次；其余三个是它的薄 COPY 层。

## 仓库结构

```
config.env.example     唯一配置入口（IP、域名、版本、开关）
secrets.env.example    密钥模板（真实值放 secrets/.env.secret，不进 git）
images/
  dsh-runtime/         DSH 运行时基础镜像（Dockerfile）
  dsh-a2a/             Actor 运行时：a2a-shim.js / start.sh / sre-cli.js / dce-cli.js
  adapter/             Go 源码：JWT 校验、会话推导、SSE 流式、DCE kubeconfig
  controller-fix/      kagent controller 的唤醒竞态补丁
deploy/
  manifests/           SandboxAgent / WorkerPool / Gateway / HTTPRoute / adapter / SRE
  jwks/                DCE 的 JWKS（公钥，运行时挂载）
extension/             Chrome MV3 扩展
scripts/               check / build / bootstrap / deploy / verify / fix-dns-egress
docs/                  architecture / install / gotchas / security / mock-dce
```

## 文档

- [docs/architecture.md](docs/architecture.md) —— 链路、组件职责、为什么自研
- [docs/install.md](docs/install.md) —— 从零安装（前置条件、镜像、bootstrap、deploy、verify）
- [docs/gotchas.md](docs/gotchas.md) —— 踩过的坑：gVisor、valkey、会话、出网
- [docs/security.md](docs/security.md) —— 威胁模型与已知边界
- [docs/mock-dce.md](docs/mock-dce.md) —— 没有 DCE 时怎么跑

## 许可与来源

- kagent / substrate / kgateway / agentgateway：各自上游许可证
- `dce` CLI 与 skills：[DaoCloud/daocloud-skills](https://github.com/DaoCloud/daocloud-skills)（构建期获取，不 vendor）
- DSH 运行时：`@deepseek-ai/dsh`（构建期安装，仓库不含二进制）

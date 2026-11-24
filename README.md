# dce-copilot-mini-poc

**BYOA（Bring Your Own Agent）for DCE** —— 每个 DCE 用户，在浏览器里和自己的沙箱 Agent 对话；
Agent 用这个用户自己的身份调用 DCE API 和 kubectl。

> BYOA = 能力归属不变、凭证归属不变、审计归属不变。
> 共享一套集群和模型，但每个用户的 Agent 有自己的文件系统、自己的进程、自己的身份。
> 不是"一个 Agent 服务所有人"，而是"每人生成一个 Agent，用完即挂起"。

一张表说清区别：

| | 共享 Copilot | **BYOA（本项目）** |
|---|---|---|
| 谁能用 | 所有人共用一个会话 | 每个用户一个会话 |
| 身份 | Agent 用服务账号，再"假装"成用户 | Agent 直接用用户自己的 JWT / kubeconfig |
| 越权 | 靠提示词和代码审查 | 由 DCE 与 Kubernetes 的 RBAC 拒绝 |
| 数据 | 同一个进程里混着 | 各自沙箱，快照隔离 |
| 成本 | 常驻 | 空闲挂起，恢复约 0.3 秒 |



用户身份 → kgateway → substrate-adapter → kagent → Substrate Actor（DSH + skills）→ DCE / Kubernetes。
SRE Agent 独立运行，只通过 agentgateway 暴露两个入口。

> **前置条件：一个可用的 DCE 环境。** 本仓库是 DCE 的附庸——身份、权限、指标、Notebook 全部来自 DCE。
> 没有 DCE 时可以用 `images/mock-dce/` 起一个假 DCE 把对话链路跑通（授权部分没有意义），见 [docs/mock-dce.md](docs/mock-dce.md)。

---

## 名词表

| 词 | 指的是 |
|---|---|
| **DCE** | 你的企业云平台（本项目依赖它的登录、权限、指标与 Notebook 接口） |
| **DSH** | DeepSeek Harness，跑在沙箱里的 Agent 运行时（对话 + 工具调用） |
| **Actor** | 一个用户的沙箱实例。一个会话一个，空闲即挂起，下次恢复约 0.3 秒 |
| **Substrate** | 管理 Actor 生命周期的组件（创建、挂起、快照、路由） |
| **kagent** | 把 A2A 协议请求转给 Actor 的控制器 |
| **kgateway** | 浏览器请求的入口（校验 JWT、限流、超时） |
| **A2A** | Agent-to-Agent 协议，kagent 的入口协议 |
| **alice / bob** | 文档里的示例用户，指两个不同 DCE 账号 |

## 60 秒理解架构

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
   ├── dce CLI            以用户身份调 DCE OpenAPI（JWT 在 /run/dsh/token）
   ├── kubectl            用用户自己的 kubeconfig
   └── sre-cli            经 agentgateway → 独立 SRE Agent
```

为什么这么绕：浏览器是 HTTP + JWT，kagent 的沙箱入口是 A2A，DSH 又只暴露本地接口。
adapter 和 shim 就是这三段之间的两座桥。**它们是本 POC 的实现，不是官方能力。**

---

## 快速开始

### 0. 先弄清三个地址（最容易卡住的地方）

| `.env` 里的变量 | 是什么 | 去哪找 | 长什么样 |
|---|---|---|---|
| `DCE_ORIGIN` | **你平时登录 DCE 控制台的浏览器地址**（不带末尾 `/`） | 浏览器地址栏 | `http://10.31.223.20:31284` |
| `DCE_CLUSTER` | DCE 里托管的 K8s 集群名 | DCE 控制台 → 集群管理 → 集群名 | `kpanda-global-cluster` |
| `JWT_ISSUER` | DCE 签发 token 的 issuer | 解开一个登录 token 看 `iss`（见下） | `https://<dce>:31909/auth/realms/ghippo` |
| `JWT_AUDIENCE` | token 的受众 | 同一个 token 看 `aud` | `__internal-ghippo` |
| `EXTENSION_GATEWAY_BASE` | **本仓库自己部署的入口网关**（不是 DCE 的网关） | `make deploy` 结束会打印 | `http://<gateway-ip>:8080` |
| `IMAGE_REGISTRY` / `IMAGE_TAG` | 镜像仓库与版本 | 默认已指向发布好的镜像 | `release-ci.daocloud.io/common-ci` / `0.1.0` |

`iss` / `aud` 怎么读：在已登录的 DCE 页面按 F12，控制台执行

```js
copy(localStorage.getItem('ANAKIN_TOKEN'))
```

粘到 https://jwt.io 看 payload 里的 `iss` 和 `aud`。

> **这一步只是为了让 `make verify` 能在命令行验证。** 日常使用时浏览器扩展自己会读 `localStorage['ANAKIN_TOKEN']`，
> 不需要任何人复制 token；JWT 过期后 DCE 也会自动刷新。**生产配置里不应出现任何手工 token。**

### 1. 前置：集群能拉到镜像

默认直接使用已发布的镜像，集群必须能访问并拉取：

```
release-ci.daocloud.io/common-ci/dce-copilot-dsh-a2a:0.1.0
release-ci.daocloud.io/common-ci/dce-copilot-adapter:0.1.0
release-ci.daocloud.io/common-ci/dce-copilot-sre-agent:0.1.0
```

先验证一句（在集群里能跑一个 Pod）：

```bash
kubectl run pulltest --image=release-ci.daocloud.io/common-ci/dce-copilot-adapter:0.1.0 \
  --restart=Never --command -- sleep 5 && kubectl get pod pulltest
```

- 拉得到 → 继续，**不需要 `make build`**
- 拉不到（网络不通 / 需要认证）→ 三选一：
  1. 配 `imagePullSecrets` 后继续用已发布镜像
  2. 用 `docker pull` 再 `docker tag` + 推到你自己集群能拉的仓库，改 `IMAGE_REGISTRY`
  3. 从源码构建（见文末「自己构建镜像」）

### 2. 配置文件

```bash
cp config.env.example .env
$EDITOR .env          # 至少改这 6 项：
                      # DCE_ORIGIN / DCE_CLUSTER / JWT_ISSUER / JWT_AUDIENCE
                      # IMAGE_REGISTRY / IMAGE_TAG
mkdir -p secrets
cp secrets.env.example secrets/.env.secret
$EDITOR secrets/.env.secret     # 填 DEEPSEEK_API_KEY
```

**模型 key 从哪来**：DeepSeek 开放平台（platform.deepseek.com）创建一个 API Key，形如 `sk-xxxx`。
如果你们用的是内网模型网关，把 `DSH_MODEL_PROVIDER` / `DSH_MODEL` 改成对应值，key 换成网关发的。

**镜像仓库怎么填**：如果只用官方已发布的镜像，`IMAGE_REGISTRY=release-ci.daocloud.io/common-ci`、`IMAGE_TAG=0.1.0`，跳过第 2 步。
要自己构建，填一个集群能拉到的仓库地址（`make build` 后 `docker push` 会推到它）。

### 3. 自检

```bash
make check
```

它会逐项告诉你：哪个配置是空的、哪个还是模板值、DCE 连不连得上、集群有没有默认 StorageClass、CNI 能不能执行 NetworkPolicy。
**这一步不过就别往下走**。

### 4. 装基础组件

```bash
make bootstrap
```

官方 Helm 顺序：Gateway API CRD → kgateway → substrate → kagent。
开头会预检 gVisor / 默认 StorageClass / LoadBalancer，缺什么当场报错（这三样装不出来，必须预先准备）。

### 5. 部署应用 + 扩展 + 验证

```bash
make deploy        # 会打印入口地址，把它填回 EXTENSION_GATEWAY_BASE
make extension     # 生成 build/extension/，在 chrome://extensions 加载
TOKEN_FILE=~/token.txt make verify
```

`~/token.txt` 就是上面 `copy(...)` 得到的那串 token（**仅用于命令行验证**）。9 条用例全 PASS 才算装好。

## 仓库结构

```
config.env.example     唯一配置入口（IP、域名、版本、开关）
secrets.env.example    密钥模板（真实值放 secrets/.env.secret，不进 git）
images/
  dsh-a2a/             Actor 运行时：a2a-shim.js / start.sh / sre-cli.js / dce-cli.js
  adapter/             Go 源码：JWT 校验、会话推导、SSE 流式、DCE kubeconfig
  controller-fix/      kagent controller 的唤醒竞态补丁
deploy/
  manifests/           SandboxAgent / WorkerPool / Gateway / HTTPRoute / adapter / SRE
  jwks/                DCE 的 JWKS（公钥，构建期不打包，运行时挂载）
extension/             Chrome MV3，config.js 由 make extension 生成
scripts/               check / build / bootstrap / deploy / verify / netpol-test
docs/                  architecture / install / gotchas / security / mock-dce
```

---

## 已验证（2026-11-24，真实 DCE + 真实集群）

`make verify` 在 alice 的真实 JWT 上跑满 9 条，全绿：

| 用例 | 结果 |
|---|---|
| 未带 token 被网关拒绝 | PASS（401） |
| whoami 解析到用户 | PASS（alice） |
| 自然对话（非流式） | PASS |
| 沙箱内执行 shell | PASS |
| kubectl（本人身份） | PASS |
| DCE API（dce CLI） | PASS |
| SRE 巡检报告 | PASS（经 agentgateway → 独立 SRE） |
| 流式返回 delta | PASS |
| 流式返回 done | PASS |

```
结果: 9 pass · 0 fail · 0 skip
```

复现：

```bash
cp config.env.example .env          # 填 DCE / 镜像仓库 / 网关地址
make check && make build && make bootstrap && make deploy && make extension
TOKEN_FILE=~/token.txt make verify  # token 从浏览器 localStorage['ANAKIN_TOKEN'] 取
```

## 已经解决过的问题（不需要你再踩）

| 问题 | 结论 |
|---|---|
| Actor 起不来：`runsc create … device or resource busy` | 私有 cgroupns 下注入 `-ignore-cgroups`，默认 auto 探测，普通节点自动跳过 |
| SandboxAgent 拒绝部署 | BYO 必须给 `cmd`（字符串）+ `args`，镜像必须用 digest 固定 |
| Actor 里 kubectl 没身份 / 模型报 no API key | BYO **不会**传 volumes/volumeMounts，凭据只能走 `env` + `secretKeyRef` |
| 请求撞上 Actor 休眠就超时 | controller 打了唤醒竞态补丁（`images/controller-fix/`），并给了恢复命令 |
| 长回复被截断、页面报"连接中断" | 三件事一起修：网关 300s、shim 心跳、SSE 断流自动回落非流式 |
| Valkey 重置后适配器起不来 | 单节点集群要显式设置 `cluster-announce-ip` / bus-port |
| 换会话要改环境变量、全员历史失效 | 会话 = hash(用户 + 会话标识)，`SESSION_EPOCH` 已废弃 |

细节在 [docs/gotchas.md](docs/gotchas.md)，按"现象 → 原因 → 已做的处理"组织，不复述排查过程。

## 镜像在哪

公开（公司内网 registry）：

```
release-ci.daocloud.io/common-ci/dce-copilot-dsh-a2a:0.1.0      # Actor 运行时（DSH + shim + dce CLI + skills）
release-ci.daocloud.io/common-ci/dce-copilot-adapter:0.1.0      # substrate-adapter
release-ci.daocloud.io/common-ci/dce-copilot-sre-agent:0.1.0    # 独立 SRE Agent
```

本仓库只放 Dockerfile 与源码，**不放镜像**；`make build` 会从源码重建，`IMAGE_REGISTRY` 指向上面的地址即可直接复用。

### controller 的存档
kagent controller 的唤醒竞态补丁没有发布镜像（基于上游 beta6，带 10 行改动），仓库里留的是**可重建的存档**：

```
images/controller-fix/
├── Dockerfile                      # 把编译产物盖到官方 beta6 镜像上
├── build-controller.sh             # 从 kagent 源码 + 补丁构建（需 Go 1.26.3 + 官方 ldflags）
├── UPSTREAM-ISSUE.md               # 问题记录
└── poc-overrides.yaml
kagent-resume-race.patch            # 补丁本体
```

## 上游状态：beta6 与 main

我们钉在 `v0.10.0-beta6`（预发布快照）。上游 main 后来**重写了 Session 与挂起的协调逻辑**，我们补丁针对的那段等待代码在 main 上已不存在。

- 在 beta6 上，同一个会话连续两次请求，若第二次正好落在"上一轮结束 → 挂起"的窗口，controller 会一直等到超时；补丁让它在发现 `SUSPENDED` 时再唤醒一次。
- 在 main 上，这段逻辑被替换；用可控测试（真 PostgreSQL + 假 Actor）没能复现同样的卡死。
- 结论：**这是 beta6 的问题，不是需要上游修的新 bug**。我们保留补丁 + 版本钉子，不提交 issue。
- 升级到 main 不是换镜像：kagent / Substrate / CRD / adapter 都要一起动，且必须在真实链路上重跑 `make verify` 才能下结论。

## 分发策略

| 决定 | 为什么 |
|---|---|
| 图片/二进制进不了 git | `dce` CLI 16MB 且分架构；skills 会漂移。build 脚本按架构下载 + 按 tag 克隆（离线可预放 build/ 目录） |
| 镜像只给 Dockerfile | 本机是 macOS/arm64，别人服务器是 amd64。只分发源码 |
| NetworkPolicy 默认不装 | 很多 CNI 不执行 NetworkPolicy，装了会给人假安全感。要装请配 `WITH_NETWORKPOLICY=true` 并跑 `make netpol-test` 自证 |
| JWT 会写进 Actor 的 `/run/dsh/token` | 为了让 `dce` CLI 以用户身份调用。这是**已知边界**，见 docs/security.md |

---

## 文档

- [docs/architecture.md](docs/architecture.md) —— 链路、组件职责、为什么自研
- [docs/install.md](docs/install.md) —— 从零安装（前置条件、镜像、bootstrap、deploy、verify）
- [docs/gotchas.md](docs/gotchas.md) —— 踩过的坑：gVisor、valkey、会话代数、MV3 30s、SSE 断流
- [docs/security.md](docs/security.md) —— 威胁模型与已知边界
- [docs/mock-dce.md](docs/mock-dce.md) —— 没有 DCE 时怎么跑（TODO）

## 许可与来源

- kagent / substrate / kgateway / agentgateway：各自上游许可证
- `dce` CLI 与 skills：[DaoCloud/daocloud-skills](https://github.com/DaoCloud/daocloud-skills)（构建期获取，不 vendor）
- DSH 运行时基础镜像：需自行提供（仓库不含）

## 自己构建镜像

默认不需要构建（用已发布镜像）。要改代码或换基础镜像时才用：

```bash
IMAGE_REGISTRY=<你的仓库> IMAGE_PREFIX= IMAGE_TAG=<tag> PUSH_IMAGES=true make build
```

4 个镜像：

| 镜像 | 内容 |
|---|---|
| `dsh-a2a` | Actor 运行时：DSH + A2A shim + `dce` CLI + skills |
| `sre-agent` | 独立 SRE Agent |
| `adapter` | 身份→会话、SSE 流式、DCE kubeconfig |
| `controller-fix` | kagent controller + 唤醒竞态补丁（需先跑 `images/controller-fix/build-controller.sh`） |

构建期自动取 `dce` CLI（按架构下载）与 skills（按 tag 克隆）；离线环境把它们放到 `build/` 下即可。

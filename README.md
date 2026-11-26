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

### 0. 前置条件

| 项 | 说明 |
|---|---|
| 一个 x86 K8s 集群 | 能装 CRD，有默认 StorageClass，节点有 gVisor RuntimeClass（见 docs/install.md） |
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

1. Chrome 打开 `chrome://extensions` → 开启**开发者模式** → **加载已解压的扩展程序**
2. 选择仓库里的 `extension/` 目录
3. 扩展**详情 → 扩展程序选项**，填两项：
   - **DCE 控制台地址**：`https://demo-tf-alpha.daocloud.cn`
   - **入口网关地址**：`make install-agent` 打印的那个
4. 保存 → 打开 DCE 登录 → 刷新页面 → 右下角出现「当前用户 <你的用户名>」的面板

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
| 沙箱出网（模型 API） | PASS（外部域名走 egress 网关） |

> 完整 9 项 `make verify` 需用当前有效 token 重跑；核心链路（身份 + 对话 + 面板 + 出网）已实测通过。

## 已经解决过的问题（不需要你再踩）

| 问题 | 结论 |
|---|---|
| Actor 起不来 / `no free workers` | WorkerPool 必须设 `template.resources.limits.memory`，否则 worker 容量算成 0 |
| 沙箱起不来，`runsc` 资产拉不到 | gVisor 的 runsc 从 `gs://gvisor` 下载，出不了网就失败；离线环境要本地化 |
| golden snapshot 一直不就绪 | Actor 镜像必须监听 `:8081/readyz`（kagent 硬编码的探针） |
| 沙箱里模型报 `no API key` | 凭据用新版格式 `version:1 + refs:`，且补 `DEEPSEEK_API_KEY` env |
| 沙箱里模型报 `TRANSPORT` | substrate 0.0.8 的 DNS 只服务内部名，外部域名要答成 router IP 走 egress（已内置） |
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

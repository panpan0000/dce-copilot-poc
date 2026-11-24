# dsh-sandbox-poc

**每个 DCE 用户，在浏览器里和自己的沙箱 Agent 对话；Agent 用这个用户自己的身份调用 DCE API 和 kubectl。**

用户身份 → kgateway → substrate-adapter → kagent → Substrate Actor（DSH + skills）→ DCE / Kubernetes。
SRE Agent 独立运行，只通过 agentgateway 暴露两个入口。

> **前置条件：一个可用的 DCE 环境。** 本仓库是 DCE 的附庸——身份、权限、指标、Notebook 全部来自 DCE。
> 没有 DCE 时只能跑 mock 模式（TODO: docs/mock-dce.md），链路里的授权部分没有意义。

---

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

```bash
cp config.env.example .env      # 改：DCE 地址、镜像仓库、网关地址
mkdir -p secrets && cp secrets.env.example secrets/.env.secret   # 改：模型 key
make check                      # 配置/依赖/连通性自检
make build                      # 3 个镜像（构建期自动取 dce CLI 与 skills）
make bootstrap                  # 装 Gateway API / kgateway / substrate / kagent
make deploy                     # 应用侧资源
make extension                  # 生成扩展 config.js 并打包
make verify                     # 端到端用例，输出 pass/fail
```

`make verify` 是唯一能证明"复刻成功"的东西——README 不算。

---

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
docs/                  architecture / replicate / gotchas / security / mock-dce
```

---

## 关键取舍（先看这几条，能省你两天）

| 决定 | 为什么 |
|---|---|
| 图片/二进制进不了 git | `dce` CLI 16MB 且分架构；skills 会漂移。build 脚本按架构下载 + 按 tag 克隆 |
| 镜像只给 Dockerfile | 本机是 macOS/arm64，别人服务器是 amd64。只分发源码 |
| NetworkPolicy 默认不装 | 很多 CNI 不执行 NetworkPolicy，装了会给人假安全感。要装请配 `WITH_NETWORKPOLICY=true` 并跑 `make netpol-test` 自证 |
| gVisor `-ignore-cgroups` 默认 auto | 只有 OrbStack 这类"私有 cgroupns"需要；真实 x86 节点一般不需要 |
| JWT 会写进 Actor 的 `/run/dsh/token` | 为了让 `dce` CLI 以用户身份调用。这是**已知边界**，见 docs/security.md |

---

## 文档

- [docs/architecture.md](docs/architecture.md) —— 链路、组件职责、为什么自研
- [docs/replicate.md](docs/replicate.md) —— 换一套 K8s 集群复刻（含 DCE 侧依赖）
- [docs/gotchas.md](docs/gotchas.md) —— 踩过的坑：gVisor、valkey、会话代数、MV3 30s、SSE 断流
- [docs/security.md](docs/security.md) —— 威胁模型与已知边界
- [docs/mock-dce.md](docs/mock-dce.md) —— 没有 DCE 时怎么跑（TODO）

## 许可与来源

- kagent / substrate / kgateway / agentgateway：各自上游许可证
- `dce` CLI 与 skills：[DaoCloud/daocloud-skills](https://github.com/DaoCloud/daocloud-skills)（构建期获取，不 vendor）
- DSH 运行时基础镜像：需自行提供（仓库不含）

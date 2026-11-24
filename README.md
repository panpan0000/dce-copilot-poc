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
- [docs/replicate.md](docs/replicate.md) —— 换一套 K8s 集群复刻（含 DCE 侧依赖）
- [docs/gotchas.md](docs/gotchas.md) —— 踩过的坑：gVisor、valkey、会话代数、MV3 30s、SSE 断流
- [docs/security.md](docs/security.md) —— 威胁模型与已知边界
- [docs/mock-dce.md](docs/mock-dce.md) —— 没有 DCE 时怎么跑（TODO）

## 许可与来源

- kagent / substrate / kgateway / agentgateway：各自上游许可证
- `dce` CLI 与 skills：[DaoCloud/daocloud-skills](https://github.com/DaoCloud/daocloud-skills)（构建期获取，不 vendor）
- DSH 运行时基础镜像：需自行提供（仓库不含）

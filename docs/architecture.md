# 架构

## 一句话
用户浏览器里的 JWT，一路走到一个只属于这个用户的沙箱里，变成 kubectl 和 DCE API 的权限。

## 链路

```
浏览器（DCE 页面 + 扩展）
  │ Authorization: Bearer <用户 JWT>
  ▼
kgateway（Envoy）—— 策略点
  │ · 校验 JWT（issuer/audience/JWKS）
  │ · 这里加限流、TLS、按用户配额
  ▼
substrate-adapter —— 身份 → 会话
  │ · 再验一次 JWT（不信任网关，防内网绕过）
  │ · 会话 = hash(用户名 + 会话标识)，用户之间不可能共用 Actor
  │ · 用用户 JWT 换 1 小时 kubeconfig（DCE clustercert）
  │ · 把 JWT + kubeconfig 作为 A2A data 下发
  ▼
kagent controller —— A2A 入口
  │ · 把请求送到该会话的 Actor；Actor 睡着就先唤醒
  ▼
Substrate Actor —— 每会话一个 gVisor 沙箱
  ├── DSH             对话与工具执行
  ├── a2a-shim        A2A ↔ DSH 本地接口；写 /run/dsh/token、kubeconfig
  ├── dce CLI         以用户身份调 DCE OpenAPI（+ skills 教它怎么用）
  └── sre-cli         经 agentgateway → 独立 SRE Agent
```

## 组件职责

| 组件 | 谁维护 | 作用 | 为什么必须有 |
|---|---|---|---|
| kgateway | 开源 | 入口、JWT 校验、限流/超时 | 唯一的策略点；adapter 不做这些 |
| substrate-adapter | 本项目 | 身份→会话、换 kubeconfig、SSE 流式转发 | kagent 只认 A2A，浏览器只会 HTTP |
| a2a-shim | 本项目 | A2A→DSH、凭据落位、SRE/DCE 代理 | DSH 只暴露本地 RPC |
| kagent controller | 上游 + 补丁 | 会话调度、Actor 唤醒 | 上游有休眠唤醒竞态，补丁见 images/controller-fix |
| Substrate | 开源 | gVisor 沙箱、快照、路由 | 每会话隔离与秒级恢复 |
| SRE Agent | 本项目 | 集群巡检（只读） | 与用户 Agent 隔离，能力收敛到两个入口 |

## 为什么自研两座桥
浏览器是 HTTP+JWT，kagent 是 A2A，DSH 是本地 RPC。这不是官方能力，
也不代表以后要保留同样实现——它只是当下最短的粘合方式。

## 一次请求发生了什么

1. 扩展读 `localStorage['ANAKIN_TOKEN']`，POST `/sandbox/poc/chat/stream`
2. 网关验签 → adapter 再验签 → 算出 `sid = hash(user#conv)`
3. adapter 换 kubeconfig（缓存 50 分钟）→ A2A `message/stream`
4. controller 找到/唤醒 Actor（SUSPENDED → RUNNING，约 0.3s），转 A2A
5. shim 写 `/run/dsh/token` 与 `~/.kube/config`，把文本交给 DSH
6. DSH 决定调工具：`dce` / `kubectl` / `sre-cli`
7. 回复以 SSE 增量回到 adapter → 网关 → 扩展，边收边渲染

## 容量模型
1 个 worker = 1000 个 actor 的容量，但 actor 只在"一轮对话进行中"占用 worker。
所以 worker 数按**并发轮次**估，不按用户数估。空闲 actor 挂起，恢复 0.3 秒。

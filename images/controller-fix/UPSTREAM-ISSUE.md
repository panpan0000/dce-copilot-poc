# 拟提交给上游的 issue（草稿）

**Title**: Substrate sandbox: concurrent request during end-of-turn checkpoint can hang until client timeout (actor stays SUSPENDED while controller waits for reachability)

## 环境
- kagent v0.10.0-beta6 (4f6238e), substrate 0.0.8
- SandboxAgent type=BYO, WorkerPool replicas=1

## 现象
同一会话连续两次请求，若第二次恰好落在上一轮结束的 checkpoint 窗口内：

- 第一次请求正常返回；
- 第二次请求在网关/adapter 侧挂到超时（我们观察到 120s+），
- controller 日志里没有报错，只有 `waitForActorReachableViaAtenet` 在轮询；
- 期间 actor 状态是 `SUSPENDED`（上一轮结束时被挂起），直到客户端放弃也没被唤醒。

## 根因（我们的判断）
`go/core/pkg/sandboxbackend/substrate/actor_reachability.go` 的 `waitForActorReachableViaAtenet`：

1. 进入时 `EnsureSessionActor` 已确认过一次状态；
2. 之后循环里只用 `actors.GetActor` 读状态，**只在 RUNNING 时才去 probe**；
3. 如果这期间 Actor 因为"回合结束 → checkpoint → SUSPENDED"而变了状态，函数没有任何分支去重新 `ResumeActor`，于是一直等到 ctx 超时。

这是时序竞态：请求与 `SUSPENDING`/`SUSPENDED` 交错。

## 最小复现
1. 部署 SandboxAgent（BYO + substrate），1 个 worker；
2. 连续发两次请求，间隔控制在上一轮结束前后 1–2 秒；
3. 观察第二次请求是否超时，以及此时 actor 状态是否为 `SUSPENDED`。

## 我们用的缓解补丁
在等待循环中，如果发现状态变成 `SUSPENDED` 就再调一次 `ResumeActor`：

```go
if getErr == nil && actor.GetStatus() == ateapipb.Actor_STATUS_SUSPENDED {
    if resumer, ok := actors.(interface {
        ResumeActor(context.Context, string, string) (*ateapipb.Actor, error)
    }); ok {
        actor, getErr = resumer.ResumeActor(waitCtx, atespace, actorID)
    }
}
```

打在 v0.10.0-beta6 上，单测与该竞态相关的可达性测试通过。

## 更彻底的方向（供讨论）
- 在 substrate backend 内部把 "SUSPENDED ⇒ resume" 作为等待逻辑的一部分，而不是留给调用方；
- 或者让 turn 结束到真正 suspend 之间有一段可被新请求"取消挂起"的宽限期（类似保留期），避免每个新请求都要走一次冷恢复；
- 补充一个并发测试：`TestEnsureSessionActorWakesSuspendedActorDuringReachabilityWait`。

## 说明
我们在 v0.10.0-beta6 上验证了该问题；上游 main 的 substrate 后端已重构（`sandboxbackend` 包不再存在，改为 sandboxtemplate/v1alpha3），未确认新版是否仍存在同一竞态。

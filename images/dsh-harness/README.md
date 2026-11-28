# dsh-harness：把 DSH 作为 kagent 1.0 的 actor 运行时

kagent 1.0 的 actor 契约是 **A2A gRPC（`lf.a2a.v1.A2AService`）监听 :80 + `/readyz` 在 :8081**，
而这个 readyz 与 :80 是由 kagent 自己的 `A2AServer` 在同一个 `Run()` 里原子启动的。

本目录是「让 DSH 满足该契约」的最小实现，直接复用 kagent 官方 harness 框架，不自己实现协议：

| 文件 | 作用 |
|---|---|
| `cmd/main.go` | bootstrap（`KAGENT_BOOTSTRAP_JSON`）→ `adk/pkg/app`.New(port=80) → Run()；从这里白拿 :80 的 A2A gRPC 和 :8081 的 `/readyz` |
| `executor/executor.go` | `runtime/a2a` 的 executor + `continuation` store；**启动期不校验 DSH**（否则 golden 探针会被拖死） |
| `internal/dsh/runner.go` | DSH HTTP 客户端：`session.create` / `session.prompt` / 轮询 `session.history`，按 seq 前缀 diff 吐 `TextDelta` |

## 放到 kagent 源码树里编译

```sh
cp -r images/dsh-harness/ cmd/ internal/ executor/  →  $KAGENT/go/harness/dsh/
cd $KAGENT/go && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o kagent-dsh ./harness/dsh/cmd
```

## 两个必须知道的坑（踩过）

1. **`/data` 在 golden 阶段是只读的** → `continuation.New("/data/...")` 会 `mkdir /data: read-only file system`，
   进程直接退出、探针永远起不来、表现为 `GoldenActorCrashed`。代码里用 `writableDir("/data","/tmp/dsh-data")` 兜底。
2. **telemetry 身份必须三件套齐全**（runtime + agentName + namespace），只传 AgentName 会被
   `runtimea2a.New` 拒绝。用零值 `tracing.RuntimeTelemetry{}`。

## 镜像

```sh
docker build --platform linux/amd64 -t dsh-harness:latest images/dsh-harness
```
底座用 `dce-copilot-dsh-a2a:0.4.1-ca`（自带 DSH + kubectl + dce-cli + sre-cli + skills）。

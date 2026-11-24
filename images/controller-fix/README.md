# kagent controller: Actor 休眠时并发请求的 POC 修复

2026-11-20。基于 kagent v0.10.0-beta6（4f6238e）源码，补丁见 ../kagent-resume-race.patch。请求与 Actor SUSPENDING 交错时，等待状态变化；可达性等待期间如变为 SUSPENDED，主动 ResumeActor。针对该竞态的单测及相关可达性测试通过。

本机镜像 `kagent-controller-resume-fix:0.10.0-beta6`，基于官方 `ghcr.io/kagent-dev/kagent/controller:0.10.0-beta6`，替换 /app，原镜像构建参数（包括运行时镜像 digest）从原二进制的 `go version -m` 提取后保留。镜像目前只在 OrbStack 本机，其他机器需要重建和推送。当前 WorkerPool 为 3，适配器 SESSION_EPOCH=8，切换会话使 Alice/Bob 开始新会话。

还原官方 controller：

```sh
kubectl -n kagent set image deploy/kagent-controller controller=ghcr.io/kagent-dev/kagent/controller:0.10.0-beta6
kubectl -n kagent rollout status deploy/kagent-controller
```

对官方 kagent 源码（tag v0.10.0-beta6）应用补丁后，从官方原镜像提取 /app，读取 `go version -m` 的 -ldflags 值，以 linux/arm64、CGO_ENABLED=0 构建 `./core/cmd/controller` 为 `codex-kagent-controller-fixed`，将它与本目录 Dockerfile 放在同一构建目录构建镜像。需使用 Go 1.26.3。不要将其他镜像的 flags 直接套用。

限制：旧 Actor 仍可能卡在 RESUMING 并占用 Worker；曾出现 Actor 本地文件系统 5.9GB 满、历史目录清理误删活动目录等问题。补丁未解决这些问题，不证明并发、重启或长期运行稳定；避免直接清空 Valkey 和删除活跃 Actor 目录。

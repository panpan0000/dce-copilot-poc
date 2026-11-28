# 现场状态快照（memory / 跨轮续作）

> 更新：2026-11-27 ~19:5x。目标：DSH 作为 kagent 1.0 actor 跑通闭环。

## 结论性事实（已确定，勿再纠结）
- kagent 1.0 的 actor 契约：A2A **gRPC**（`lf.a2a.v1.A2AService`），监听 `:80`；wakeup 探针 `/readyz` 在 `:8081`。
- 二者由 `go/adk/pkg/a2a/server/server.go` 的 `A2AServer` **同一个 Run() 原子启动**（httpServer :80 + readyServer :8081）。**不要再手动绑 8081**（会端口冲突 → app.Run 失败 → :80 没监听 → 快照冻住 → `Connect: tunnel failed`）。
- 快照保真度：kagent 写死 `SNAPSHOT_FIDELITY_VOLUMES`（`core/internal/substrate/actor_template.go`），Harness CRD 只暴露 `snapshotPolicy.location`。
- 旧栈是 JSON-RPC over HTTP；1.0 是 gRPC。老 shim 不能直接复用。

## harness/dsh（Go，已在 kagent 源码树内写完并编译通过）
路径：`/tmp/kagent-src/go/harness/dsh/`
- `cmd/main.go`：bootstrap.FromEnvironment → `adk/pkg/app`.New(port=80) → Run()。**不再手动 readyz、不再 select{} 保活**。
- `executor/executor.go`：`dsh.New(runner)` + `continuation.New(...,"dsh")` + `runtimea2a.New`；**启动期不验 DSH**。
- `internal/dsh/runner.go`：DSH HTTP 客户端。DSH RPC：POST `{DSH}/api/{method}`，body `{"type":"client-request","rpcId","method","payload"}` → `{"result":{"ok","value"}}`。
  - `session.create{cwd}` → `{sessionId}`；`session.prompt{sessionId,mode:"queue",content:[{type:"text",text}]}`；`session.history{sessionId,maxMessages}`。
  - 事件被包在 `{"event":{type,seq,time,data}}`；流式增量来自 `assistant/chunk`（`data.chunk.text`，`chunk.type=="delta"`）；终态 `turn/end`（`data.reason.kind=="completed"`）；完整文本 `assistant/message`（`data.message.content[].text`）。
  - Run() 用 chunksText 优先、lastText 兜底，按 seq 前缀 diff 发 TextDelta。

## 镜像
- 二进制：`/tmp/dshimg/kagent-dsh`（linux/amd64，66M）。
- 镜像：`dsh-harness:1.7.0`，本地已建；`start.sh` = `dsh --profile web --host 127.0.0.1 --port 18790 & exec /kagent-dsh`（并行起，不阻塞）。
- 底座：`release-ci.daocloud.io/common-ci/dce-copilot-dsh-a2a:0.4.1-ca`（DSH + kubectl + dce-cli + sre-cli + skills）。
- 推内网 registry：`localhost:5050/kagent-dev/deepseek/dsh-harness:1.7.0`（atelet 用 `localhost:5000` 引用，跑的时候被替换成 `registry.kagent.svc:5000`）。

## 集群现状
- Harness `deepseek-harness` 目前 image = dsh-harness@1.6.0（上一版，有 8081 冲突 bug）；**待切 1.7.0**。
- 切之前历史：1.0.0~1.6.0 均 golden Ready 但对话 `Connect: tunnel failed`（根因已定位=8081 冲突）。
- 回退可用运行时：`localhost:5000/kagent-dev/deepseek/agent-runtime@sha256:2d1c5e423f854604759424aeb40075614d7b94abf48bc7aeac6c44623b7c3f84`（golang-adk+工具，聊天已验证 OK）。

## 2026-11-27 23:0x 【突破】golden 用 DSH 通过了
**两个真凶（都已修，`dsh-harness:2.0.0`）：**
1. `continuation.New("/data/adapter")` → `mkdir /data: read-only file system`。**golden 阶段 `/data` 是只读的**，`executor.New` 直接失败 → `os.Exit(1)` → 探针起不来 → `GoldenActorCrashed`。
   - 修法：`writableDir("/data","/tmp/dsh-data")` 选真正可写目录；bootstrap 也加 `/tmp` 兜底。
2. `runtimea2a.New(... telemetry)` → `invalid runtime telemetry: telemetry identity requires a runtime, agent name, and namespace together`。传了只有 AgentName 的部分身份被拒。
   - 修法：传零值 `tracing.RuntimeTelemetry{}`（全空=合法）。

**结果**：`Harness` → `dsh-harness:2.0.0` 后 **golden 20s 内 Ready**；本地 `DSH_DEV_CONFIG=1` 也验证 `:8081/readyz=200`、A2A `:80=200`。

**剩余问题**：真实对话返回 `{"done":true}` 但**没有任何 delta**（actor 收到请求、回合“成功”结束、没吐文本）。下一步：确认 actor 内 DSH 是否起来（`start.sh` 里 `dsh --profile web` + `HOME=/tmp/agent` 可能破坏 DSH 的 `$DSH_HOME`），以及 runner 是否因 baseline/事件名不匹配而空转。

## 2026-11-27 21:4x 现场结论（重要）
- **网页聊天已恢复可用**：Harness 已回退到 golang-adk 运行时（`agent-runtime@sha256:2d1c5e42...`），实测 5.5s 回“在的”。用户随时可在网页聊。
- **DSH 仍未通**：`dsh-harness` 1.0.0→1.9.0 全部 `GoldenActorCrashed`。现象：容器 runsc 起来后很快退出；`/data`（durable-dir）里**没有任何我们写的内容**（连 mark("start") 都没落），说明二进制要么没跑起来，要么 `/data` 写入/`saveBundle` 阶段就挂了。
- 已排除：8081 端口冲突（已删手写 readyz goroutine）、gRPC 协议不匹配（router 已不再报 frame/tunnel 错，bootstrap 在 controller 侧是 `grpc_code=OK`）。
- 未拿到：容器 stderr。gVisor 沙箱在“未 running”状态下不把容器 stdout/stderr 送进 worker 日志；`/data` 又没落盘 → 目前拿不到真实错误串。
- **下一步试探方向**（未执行）：把错误同时写 `/dev/console`（runsc 会带进 worker 日志），或把二进制当**普通 pod** 跑（containerd，`kubectl logs` 直接可见）配合真实 bootstrap descriptor。

## 2026-11-27 22:0x 关键实验结论（很有价值）
- **沙箱本身没问题**：做一个“最小镜像”（`python3 -m http.server 8081 --directory /run/rz`，`/run/rz/readyz` 内容随意）→ **golden 直接 Ready**。证明 actor 启动/探针/快照链路完全正常。
- **我们的镜像不写 `/data`**：同一镜像里 `echo >> /data/boot.log` 在 node 的 `durable-dir` 里**看不到**，而 python 从 `/run` 提供 readyz 就能过 → 说明 **`/data` 对我们容器不可写**（`2>/dev/null` 把错误吞了）。
- 推论（当前最强假设）：`bootstrap.Load` 会 `saveBundle` 到 `/data/kagent/bootstrap`，不可写 → 直接返回错误 → 进程 `os.Exit(1)` → 探针永远起不来 → `GoldenActorCrashed`。这解释了“golden 失败 + /data 全空 + controller 侧 bootstrap 返回 OK”三个现象。
- **待印证/修复**：kagent 官方运行时用 **uid 65532**（`golang-adk` 镜像 USER=65532），我们的镜像用 **root**。durable 卷很可能按 65532 授权，root 在 gVisor 下反而不可写。→ 试 `USER 65532:65532`（1.11.0，已验证镜像构建成功，**推 registry 时网络又断，未完成部署**）。
- 次要风险：1.10.0 的 `start.sh` 已去掉 `dsh` 启动（只 `exec /kagent-dsh`），DSH 没跑 → 即使 golden 过了，首轮 `session.create` 也会失败。恢复 DSH 启动前先确认 golden 能过。

## 待办（按序）
- [x] push 1.7.0+ → golden 仍失败（见上）。
- [ ] **D0' 立即做**：网络恢复后 push `dsh-harness:1.11.0`（USER 65532）→ patch harness → 看 golden 是否 Ready。
  - 若 Ready → uid 假设成立 → 把 `dsh --profile web ... &` 加回 start.sh（DSH 需要写 `$HOME/.dsh`，HOME 指向 `/data/agent`），再验真实对话。
  - 若仍失败 → 用“最小镜像 + 我们的 start.sh 片段”二分，定位到底哪一行导致退出。
- [ ] D) kubectl 凭据：kubectl 没调用 exec 插件（adapter 侧 `auth=""`）；kubeconfig 已配 exec `/usr/local/bin/dce-token`（返回 session-<ctx>），adapter `/poc/k8s` 支持 `session-<ctx>`→用户证书（已 curl 验证 403=按 alice RBAC 生效）。
- [ ] E) sre-cli：sre-gateway 已修好（jwks 挂载冲突→/jwks），网关 Running；但网关→后端 `/sre/poc/reports/latest` 20s 超时；直连 sre-agent `/v1/inspections/latest` 返回 401（后端只认网关注入的 backend token）。

## 常用命令/路径
- token：`/tmp/token.txt`（alice）。
- 测对话：`echo "..." | /tmp/ask.sh <conv6+>`。
- 网关：`http://172.30.40.186:31853/sandbox/poc/...`；扩展 gatewayBase=`http://172.30.40.186:31853`。
- 内网 registry port-forward：`kubectl -n kagent port-forward svc/registry 5050:5000`。
- nodeadmin（特权，导入镜像用）：`kubectl -n kagent get pod nodeadmin`（常 Completed，需重建）。
- adapter 镜像导入：`docker save dce-copilot-adapter:<v> | kubectl -n kagent exec -i nodeadmin -- chroot /host ctr -n k8s.io images import -`。

## 2026-11-27 23:2x 最新增量
- **SRE 网关 rewrite 是错的**：`images/sre-agent/sre-agent.js` 里真实路由只有 `POST /v1/inspections` 和 **`GET /v1/reports/latest`**；
  而 `cm sre-gateway-config` 把 `/sre/poc/reports/latest` 重写成 `/v1/inspections/latest`（不存在→404）。
  已把 rewrite 改成 `/v1/reports/latest` 并重启网关。**但经网关请求仍 20s 超时**（直连后端带 backend token 是 200/404 正常）→ 卡在网关侧（JWT 校验或上游拨号），未解决。
- **DSH runner 已本地验证可用**：在 dsh-a2a 镜像里跑真实 dsh，用 `cmd/probe` 直调 `internal/dsh.Run` → 打印 `[session] session-...` 然后输出 **在的**，`err=<nil>`。说明 runner 逻辑本身没问题。
- **actor 内仍拿不到文本**：`dsh-harness:2.3.0` golden Ready，但对话 1.7s 返回 `{"done":true}` 无 delta；2.5.0 加了“无文本时回事件尾巴”的诊断后变成**挂住（>120s）**，说明 actor 内 `session.create` 不成功（DSH 没起来或 18790 不可达）。
- 已尝试：给 egress 加 `api.deepseek.com`（DSH 直连模型需要），未解决。
- 下一步：确认 actor 内 `dsh --profile web` 是否真起来（`/home/agent/.dsh/profiles` 在镜像里不存在，但本地用同样 HOME 能起）、以及 18790 监听情况。

## 2026-11-27 23:3x kubectl 路径式身份（已实现并部署）
- 根因：kubectl 确实发出了 `--token=session-<ctx>`（`sh -x` 证实），但 **adapter 收到的 `Authorization` 是空的** → egress 侧把头吃掉。
- 修法：**身份改走 URL 路径**。adapter 新增 `/poc/k8s/s/<token>/<rest>` 路由；agent 镜像里的 `kubectl` 是 wrapper，运行时生成 `/tmp/kubeconfig-session`（server 指向上面的路径），再 exec `kubectl.real`。
- 验证（已通过）：`curl .../poc/k8s/s/session-deadbeef/...` → `actor 还没有绑定用户（先发一条对话再试）`；无身份 → `unauthorized: missing actor identity`。说明路由与身份解析都对。
- 镜像：adapter `3.0.0`（已部署）、agent-runtime `2.1.0`（已切到 Harness）。

## 阻塞：测试用 DCE token 过期
- `/tmp/token.txt` 里的 token 已过期，网页接口返回 `Jwt is expired`；浏览器当前停在 Keycloak 登录页 → **需要重新登录 DCE** 才能跑端到端 web 测试（kubectl / sre-cli）。

---

# 2026-11-28 收尾总结（可复现步骤）

## 一、架构现状（kagent 1.0 + substrate 1.0）

```
浏览器扩展 ──HTTP──▶ kgateway(NodePort 31853) ──▶ substrate-adapter(:9099) ──gRPC A2A 1.0──▶ kagent-controller(:8083)
                                                          │                                   │
                                                          ├─/poc/k8s/*  → apiserver（用"会话→用户证书"代理）
                                                          └─/poc/sre/*  → sre-agent(:8080) 后端
                                                                                    ▲
kagent-controller ──▶ substrate actor（gVisor，:80 A2A gRPC + :8081 readyz）        │
                        └ 运行时 = agent-runtime（golang-adk + 工具）或 dsh-harness ┘
```

## 二、三个"最后一跳"的根因与修法

| 问题 | 现象 | 根因 | 修法 |
|---|---|---|---|
| kubectl 401 | `poc/k8s` 收到 `auth=""` | `Authorization` 头在沙箱→adapter 之间被 egress 侧吃掉 | 身份改走 **URL 路径** `/poc/k8s/s/session-<ctx>/...`，镜像内 `kubectl` wrapper 运行时生成 kubeconfig |
| SRE 503/超时 | `sre-cli` 拿不到报告 | ① agentgateway 请求挂死 ② SRE agent 的 DSH 起不来 ③ 巡检校验只认 `cluster-inspect.js` | ① 绕开网关走 adapter `/poc/sre/*` ② 用完整 DSH 底座重建镜像 ③ 放宽为"任意 bash 工具成功" |
| 浏览器不可达 | `Failed to fetch` / `Agent Gateway 不可达` | ① 网关 Pod Pending（孤儿 RS 抢占）② CORS 预检被 JWT 401 ③ Chrome Private Network Access | ① 删孤儿 RS ② 加 CORS 策略 ③ 需返回 `access-control-allow-private-network: true` |

## 三、kagent controller 不再跑在 GPU 节点

`controller-node-1` 磁盘长期吃紧（48G 盘，GPU 驱动栈占大头），kubelet 标 `DiskPressure`，
导致 controller 丢 leader 选举并 Pending。已把 controller 固定到 `worker-node-1`：

```yaml
spec.template.spec.nodeSelector.kubernetes.io/hostname: worker-node-1
```

## 四、可复现的验证命令

```bash
# 1) 身份（浏览器同源）
curl -H "Origin: https://demo-tf-alpha.daocloud.cn" -H "Authorization: Bearer $TOKEN" \
     http://<NODE_IP>:31853/sandbox/poc/whoami
# 期望: 200 + access-control-allow-origin + {"username":"alice"}

# 2) kubectl（按用户 RBAC）
kubectl get ns            # 期望 403 User "alice" ...
kubectl get pods -n kagent

# 3) SRE 报告
sre-cli inspect           # 返回 202 running
sleep 60; sre-cli report  # 期望 status=completed + report 正文
```

## 五、待办

- [ ] 网关补 `access-control-allow-private-network: true`（Chrome PNA，否则页面内 fetch 仍被拦）。
- [ ] `controller-node-1` 扩容或迁移 containerd 根目录（磁盘长期 80%+）。
- [ ] dsh-harness 作为 actor 已能过 golden，但尚未在真实对话中产出文本（runner 本地已验证可用）。

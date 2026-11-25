# 在 x86 集群（jinye）上验证：进行中的实验记录

> 目标：在一套真实的 x86 集群上验证"仓库能复刻"，而不是只在 Apple Silicon 上跑通。
> 环境：jinye-gpu（172.30.40.175 控制面/GPU、172.30.40.186 工作节点/x86_64）

## 已完成

| 步骤 | 结果 |
|---|---|
| SSH 免密（两节点） | ✅ |
| GPU 节点识别 | 175 有 GPU（`/var` 仅剩 8.5G）；186 无 GPU（70G 空闲） |
| 给 175 打污点 `demo=reserved:NoSchedule` | ✅（测完要删） |
| 186 装 gVisor（runsc + containerd runtime + RuntimeClass） | ⚠️ 部分：二进制已装，`gvisor-bin/` 需整体放置；smoke Pod 仍 ContainerCreating |
| 186 装 buildkit（buildctl/buildkitd v0.17.2） | ✅ |
| amd64 基础镜像 `node:24-bookworm-slim` 传到 186 | ✅（225MB，已 `nerdctl load`） |
| `adapter` amd64 交叉编译 | ✅（6.3MB 静态二进制） |

## 卡点

1. **DSH 运行时没有 amd64 版本**：原 `watsons-dsh-poc:v3` 只有 arm64。
   解决方向：`images/dsh-runtime/Dockerfile`（node:24 + dsh + kubectl + python）从零构建。
2. **buildkit 无法访问 docker.io**：节点出网受限，本地镜像代理 `10.5.10.120` 在 buildkit 下不工作。
   绕开方式：Mac 上 `docker save` → scp → `nerdctl load`，再用 `nerdctl run + commit` 产出镜像。
3. **kubectl 用例的语义**：适配器默认用 DCE `clustercert` 签发 kubeconfig，指向的是 DCE 管的集群，不是 jinye。
   需要 `KUBECONFIG_SOURCE=local` 模式（未实现）。

## 下一步（按顺序）

1. 用已加载的 `node:24-bookworm-slim` 跑容器，装 runtime：
   `apt-get install python3 git curl jq` + `npm i -g @deepseek-ai/dsh@0.1.1-rc.2` + kubectl
   验证 `dsh --version` / `kubectl version --client` / `python3 --version`
2. `nerdctl commit` → `dsh-runtime:0.1.0`（amd64）
3. 三个 COPY 型镜像：`dsh-a2a` / `sre-agent` / `adapter`（带 `--change` 保留 ENTRYPOINT/USER/ENV）
4. jinye 上 `make install-dependency`（Gateway API → kgateway → substrate → kagent）
5. 适配器加 `KUBECONFIG_SOURCE=local`，让 kubectl 打到 jinye
6. `make install-agent` + `make verify`

## 回滚

```bash
# 去掉 GPU 节点污点
kubectl --kubeconfig <jinye> taint node controller-node-1 demo=reserved:NoSchedule-
# 恢复 containerd 配置（186）
ssh root@172.30.40.186 'cp /etc/containerd/config.toml.bak-dcecopilot /etc/containerd/config.toml && systemctl restart containerd'
```

---

## 工作约定（长期有效）

- **长任务（预计 >10 分钟）必须每 15–20 分钟主动报一次进度**，不要等人问。
- 报告内容：当前阶段 / 已完成项 / 卡点 / 下一步，四行以内。
- 镜像搬运策略：优先节点侧国内镜像源（`ghcr.nju.edu.cn` 已验证可用）+ `nerdctl tag` 回原名；失败才回退到"本机 pull → save → scp → load"。
- `make install` = `install-dependency` + `install-agent`。

## 事故复盘：为什么"说继续"却没继续（2026-11-25）

**现象**：镜像搬完（16:00 前）后，我说"接着跑 make install"，结果 42 分钟没有任何动作，直到用户来问。

**根因（三条，按重要性）**：
1. **我没有自发唤醒机制**。我的回合只在用户发消息（或 goal 轮次）时开始。我在回合末尾写"16:05 报告"，
   但这只是文字，不是定时器——除非我显式创建 `create_goal` 或定时提醒，否则不会有任何东西在 16:05 叫醒我。
2. **长步骤没有链式执行**。正确做法是把"搬镜像 → make install → install-agent"串成一条后台命令，
   让 shell 自己往下跑；我只在末尾看一眼结果。我把它们拆成了多个回合，而回合之间没人驱动。
3. **过度依赖"下次继续"的表述**。对用户是承诺，对我不是机制。

**规则（避免再犯）**：
- 涉及多步、跨分钟的活：**要么一条命令串完（nohup + 落日志），要么先 `create_goal`**，不要靠"我待会儿继续"。
- 回合结束前，必须确认：后台任务在跑且有日志路径、或 goal 处于 active、或有 schedule 提醒。
- 任何"我会在 X 分钟后报告"的承诺，只有在存在 goal/schedule/后台链式任务时才允许说。

## 2026-11-25 18:20 现状（jinye + demo-tf）

### 已通
- `make install-dependency`：Gateway API / kgateway / substrate / kagent 全部装上
- 修掉的坑：kgateway CRD 已被别的 release 占用（跳过 CRD 只装运行时）；`local-path-provisioner` 镜像缺失；
  静态 PV 绑定 PVC（WaitForFirstConsumer 死锁）；valkey 单节点 0 槽位；`ateom-gvisor:0.0.8` 应为 `v0.0.8`；
  kagent controller 在 `valueFrom.secretKeyRef` / 空 `value` 的 env 上 panic
- **身份链路通**：`curl -H "Authorization: Bearer <alice>" http://172.30.40.186:31853/sandbox/poc/whoami` → 200 `{"username":"alice"}`
- 浏览器面板可显示（扩展 `matches` 需含 demo-tf；SW 的 origin 白名单已放宽/自动采用页面来源）

### 未通
- 对话：`/poc/chat` 返回 `failed to send HTTP request: Post http://dsh-sandbox.kagent...`
  根因：Actor 没被创建出来 —— `ActorTemplate` 卡在 `ResumeGoldenActor`，ate-controller 报
  `while resuming golden actor: DeadlineExceeded`
- 怀疑：worker（ateom）在创建 sandbox 时需要从 `release-ci.daocloud.io` 拉 Actor 镜像，
  而 jinye 出网拉不到该 registry；`nerdctl load` 只进了节点 containerd，worker 有自己的内容存储
- SRE 网关：镜像可拉，但容器启动即崩（待查）

### 下一步
1. 给 worker 配镜像源，或把 Actor 镜像推到 jinye 可达的 registry（内网 Harbor）
2. 或者把 `ateom` 的镜像拉取改成 `IfNotPresent`/本地导入方式
3. SRE 网关崩溃日志

## 2026-11-25 18:35 根因定位：worker 的 gVisor static-files 是空的

```
$ ssh 186 'ls -la /var/lib/ateom-gvisor/static-files'
total 8   ← 空目录（只有 . 和 ..）

对比 OrbStack（同一套 chart）：
/var/lib/ateom-gvisor/static-files/runsc-62ee…40c0   (1.4MB, wrapper)
/var/lib/ateom-gvisor/static-files/runsc-62ee…40c0.real (120MB)
```

**worker（ateom）创建 sandbox 时要从 static-files 里取 runsc，目录为空 → 创建失败 →
`ResumeGoldenActor` 一直 DeadlineExceeded → Actor 建不出来 → `/poc/chat` 报
`Post http://dsh-sandbox.kagent:8080 ...` 失败。**

这一条也解释了为什么之前所有尝试（镜像推送、registry 认证、worker 重建）都没用——
根因不在镜像，在 worker 的运行时文件缺失。

### 下一步（明确）
1. 从 ateom 镜像里取出 runsc 布局（`crictl inspecti` / 导出镜像），按 `runsc-<hash>` 命名放入 static-files
2. 或让 substrate chart 的初始化 Job 跑起来（它本该负责填充这个目录）
3. 之后重跑 `ResumeGoldenActor`


## 2026-11-25 18:55 关键突破：no free workers 的根因

**worker 的可用容量由 Pod 的 `resources.limits.memory` 推算**（substrate 源码
`internal/ateworker/register/register.go:FromFiles`）。WorkerPool 没设 resources 时容量为 0，
于是 `ResumeActor → AssignWorker: no free workers available`。

修复：

```yaml
spec.template.resources:
  limits:   {cpu: "4", memory: 8Gi}
  requests: {cpu: 500m, memory: 1Gi}
```

修复后 AssignWorker 立刻成功，卡点前移到 **CallAteletRestore**（30s DeadlineExceeded）：
atelet 收到 `AteomHerder/Run`，但 worker 侧创建/恢复沙箱超过 30s 超时。


## 2026-11-25 19:20 修正一个错误假设（重要）

我之前猜"30s 不够是因为要下 670MB 镜像"，**证据不支持**：

```
$ ssh 186 'ps aux | grep -E "ateom|runsc"'
/ko-app/ateom-gvisor --pod-uid=0bbf...   Ssl   ← 两个 worker 进程活着
（没有任何 runsc 子进程）

$ /usr/local/bin/runsc --root=/tmp/rs1 do echo hi
hi                                        ← runsc 本机 0.3s 级完成
```

**runsc 从未被启动**。worker 在"调用 runsc 之前"就卡住了，30s 是它上一个环节的等待，
不是沙箱启动慢。最可能卡在 worker 自己的镜像获取（internal/imagecache）——worker 用
独立的 registry 客户端，不读节点 containerd 的认证，也没有 kubelet 的 imagePullSecret。

### 因此正确的下一步
1. 让 worker 能匿名拉到 Actor 镜像（推到 jinye 可匿名拉取的 registry，或配 worker 侧凭证）
2. 或确认 worker 镜像缓存路径（是否还需要挂另一个 hostPath）并预置镜像
3. 定位 worker 卡点的最直接手段：给 ateom 打开 debug 日志 / 抓它打开的 socket

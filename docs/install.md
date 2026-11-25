# 安装

一台 Kubernetes 集群 ≈ 30 分钟。前提条件不满足时，`make install-dependency` 会停在第一步并告诉你缺什么。

## 1. 前置条件

| 需要 | 为什么 | 怎么准备 |
|---|---|---|
| Kubernetes ≥ 1.28 + Helm 3 | 装 kgateway / substrate / kagent | 任意发行版，k3s / kind / 云托管都行 |
| **gVisor（runsc）** | Actor 跑在 gVisor 沙箱里，不装就起不来 | 节点预装并在 containerd 注册 RuntimeClass（[官方](https://gvisor.dev/docs/user_guide/install/)），`make install-dependency` 会检查 `runtimeclass/gvisor` |
| **默认 StorageClass** | substrate 的快照与 Actor 目录要 PVC | k3s 自带 local-path；云上用默认云盘 SC |
| **LoadBalancer** | `poc-gateway` 要一个外部 IP | 云 LB / MetalLB / k3s 的 svclb 都可以。没有时：`INSTALL_METALLB=true METALLB_POOL=<cidr> make install-dependency` |
| 可拉取的镜像仓库 | 集群要从它拉自建镜像（本项目不提供现成镜像） | 一个你集群能访问的 registry，`IMAGE_REGISTRY` 指向它 |
| **一个 DCE 环境** | 身份、权限、指标、Notebook 都来自 DCE | 提供 kpanda / insight / baize 接口，并知道 issuer / audience |

## 2. 配置

```bash
cp config.env.example .env
$EDITOR .env          # DCE 地址、镜像仓库、网关地址、版本
mkdir -p secrets && cp secrets.env.example secrets/.env.secret   # 模型 key
make check            # 自检：缺项 / 占位符 / 连通性 / CNI 都会提示
```

必填项：

```bash
DCE_ORIGIN / DCE_CLUSTER / JWT_ISSUER / JWT_AUDIENCE   # DCE
IMAGE_REGISTRY / IMAGE_TAG                              # 镜像
DSH_BASE_IMAGE                                          # DSH 运行时基础镜像（仓库不含）
NS_APP / NS_KAGENT                                      # 命名空间
EXTENSION_MATCH_ORIGIN / EXTENSION_GATEWAY_BASE         # 浏览器扩展
```

## 3. 镜像

本项目不提供现成镜像，一律从源码构建：

```bash
IMAGE_REGISTRY=<你集群能拉的仓库> IMAGE_TAG=0.1.0 make build
```

默认构建 `linux/amd64`（服务器多为 x86）。Apple Silicon 本机试跑改 `TARGET_PLATFORM=linux/arm64`。
构建期会自动取 `dce` CLI 与 skills；离线环境把它们放到 `build/` 下。

## 4. 安装基础组件

```bash
make install-dependency
```

它会：

1. 预检 gVisor / 默认 StorageClass / LoadBalancer（缺什么直接报错，不全等装完才失败）
2. 按官方顺序装 CRD 与组件：Gateway API → kgateway → substrate → kagent（全部用官方 helm chart）
3. 校验关键 CRD 真的存在（`gatewayclasses` / `workerpools` / `sandboxagents`）

## 5. 部署应用

```bash
make install-agent
```

做的事：渲染清单（`build/render/`）→ apply adapter / Gateway / HTTPRoute / JWT 策略 / SRE / WorkerPool / SandboxAgent
→ 等 rollout 与快照就绪 → 打印入口地址。

**Actor 镜像必须用 digest**（BYO 的硬性要求），`deploy.sh` 会从本地 docker 取，取不到就报错。

## 6. 浏览器扩展

```bash
make extension
# chrome://extensions → 开发者模式 → 加载已解压的扩展程序 → build/extension/
```

扩展的 DCE 地址与网关地址来自 `.env`，不需要改源码。DCE 登录后刷新页面即可。

## 7. 验证

```bash
TOKEN_FILE=~/token.txt make verify
```

`TOKEN_FILE` 里放当前 DCE 用户的 JWT（浏览器控制台 `localStorage.getItem('ANAKIN_TOKEN')`）。
输出 9 条 pass/fail；不带 token 时身份相关用例会标 SKIP。

## 常见失败

| 现象 | 原因 |
|---|---|
| bootstrap 第 0 步失败 | 缺 gVisor / 默认 StorageClass；按提示准备 |
| `poc-gateway` 一直是 `<pending>` | 集群没有 LB 实现，见前置条件 |
| adapter CrashLoopBackOff | 缺 `dce-jwks` ConfigMap（`make install-agent` 会建），或 `.env` 缺项 |
| SandboxAgent 拒绝创建 | Actor 镜像用了 tag 而非 digest |
| 网关 401 | issuer / audience 与 DCE 不一致，或 JWKS 里没有对应 kid |
| Actor 创建失败 | 见 [gotchas.md](gotchas.md) 第 1 条 |

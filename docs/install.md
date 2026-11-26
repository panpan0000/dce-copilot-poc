# 安装

一台 x86（linux/amd64）Kubernetes 集群 ≈ 30 分钟。前提条件不满足时，`make install-dependency` 会停在第一步并告诉你缺什么。

## 1. 前置条件

| 需要 | 为什么 | 怎么准备 |
|---|---|---|
| Kubernetes ≥ 1.28 + Helm 3 | 装 kgateway / substrate / kagent | 任意发行版，k3s / kind / 云托管都行 |
| **默认 StorageClass** | substrate 的快照与 Actor 目录要 PVC | k3s 自带 local-path；云上用默认云盘 SC |
| **可拉取 `release-ci` 镜像** | 集群节点要匿名拉 `release-ci.daocloud.io`（已公开） | 节点出网策略放行该域名，或配 registry mirror |
| **一个 DCE 环境** | 身份、权限、集群托管都来自 DCE | 提供 kpanda / insight / baize 接口，知道 issuer / audience |
| **gVisor（runsc）** | Actor 跑在 gVisor 沙箱里 | 见下节 |

### gVisor 说明

substrate 的沙箱不是用节点 containerd 的 RuntimeClass，而是**由 atelet 从 SandboxConfig 的资产 URL 下载 runsc**。
默认资产是 `gs://gvisor/releases/release/<版本>/<arch>/runsc`（Google Cloud Storage，匿名拉取）。

- **集群能出网**：什么都不用做，atelet 自动下载。
- **集群不能出网（如内网机房）**：把 runsc 上传到集群内的 rustfs（S3），再把 SandboxConfig 的 URL 改成本地：

  ```bash
  # 1. 找到 SandboxConfig 里钉死的 runsc 版本与 sha256
  kubectl get sandboxconfig gvisor-default -o yaml
  # 2. 在能出网的机器上下载对应 runsc（sha256 必须一致），上传到 rustfs
  #    aws s3 cp runsc s3://ate-snapshots/gvisor/runsc --endpoint-url http://rustfs.ate-system.svc:9000
  # 3. 改 URL 指向本地（保留 sha256 不变）
  kubectl patch sandboxconfig gvisor-default --type=json \
    -p '[{"op":"replace","path":"/spec/assets/amd64/runsc/url","value":"s3://ate-snapshots/gvisor/runsc"}]'
  ```

  > 注意：资产 URL 只认 `gs://` 或 `s3://`，**不认 `http://`**（会被当成 bucket 名报 `InvalidBucketName`）。

## 2. 配置

```bash
cp config.env.example .env
$EDITOR .env          # DCE 地址、镜像仓库、网关地址、版本
mkdir -p secrets && cp secrets.env.example secrets/.env.secret   # 模型 key（不进 git）
make check            # 自检：缺项 / 占位符 / 连通性 / CNI 都会提示
```

必填项：

```bash
DCE_ORIGIN / DCE_CLUSTER / JWT_ISSUER / JWT_AUDIENCE   # DCE（示例见 README）
IMAGE_REGISTRY / IMAGE_TAG                              # 镜像仓库
DSH_BASE_IMAGE                                          # DSH 运行时基础镜像（已推到 release-ci）
GATEWAY_MODE                                            # 集群没 LB 就填 nodeport
```

## 3. 镜像

镜像已构建好并推到 `release-ci.daocloud.io/common-ci/`（amd64）：

| 镜像 | tag | 说明 |
|---|---|---|
| `dsh-runtime` | `0.1.1` | DSH 运行时基础镜像（dsh + kubectl + python + git），**只需构建一次** |
| `dce-copilot-dsh-a2a` | `0.2.0-x86` | Actor 运行时（DSH + A2A shim + dce CLI + skills） |
| `dce-copilot-sre-agent` | `0.2.0-x86` | 独立 SRE Agent |
| `dce-copilot-adapter` | `0.2.0-x86` | 身份→会话、SSE 流式、DCE kubeconfig |

改代码后重新构建（默认 `linux/amd64`）：

```bash
make build    # 构建 dsh-a2a / sre-agent / adapter（FROM DSH_BASE_IMAGE），并推到 IMAGE_REGISTRY
```

> `dsh-runtime` 通常不用重构建。要改它（比如换 DSH 版本）见 [gotchas.md](gotchas.md)「DSH 运行时」。

## 4. 安装基础组件

```bash
make install-dependency
```

它会：

1. 预检 gVisor RuntimeClass / 默认 StorageClass（缺什么直接报错）
2. 按官方顺序装 CRD 与组件：Gateway API → kgateway → substrate → kagent（全部官方 helm chart）
3. 校验关键 CRD 存在（`gatewayclasses` / `workerpools` / `sandboxagents`）
4. **修复 substrate 沙箱出网**（外部域名答成 router IP，走 egress 网关）——这一步不做，沙箱里模型 API 会 `TRANSPORT` 失败

## 5. 部署应用

```bash
make install-agent
```

做的事：渲染清单 → apply adapter / Gateway / HTTPRoute / JWT 策略 / SRE / WorkerPool / SandboxAgent
→ 等 rollout 与快照就绪 → 打印入口地址。

**入口地址忘了随时查**：`make url`（等价 `kubectl -n watsons-agent-poc get svc poc-gateway -o wide`）。

**注意**：`DEEPSEEK_API_KEY` 不写进 manifest，部署后要手动建 Secret（或走你们自己的密钥注入）：

```bash
kubectl -n watsons-agent-poc create secret generic dsh-config \
  --from-literal=DEEPSEEK_API_KEY='sk-xxxx'      # 真值别写进 git
```

## 6. 浏览器扩展

```bash
# chrome://extensions → 开发者模式 → 加载已解压的扩展程序 → 选仓库里的 extension/
```

扩展详情 → 扩展程序选项，填 DCE 地址和网关地址（都来自 `.env`）。DCE 登录后刷新页面即可。

## 7. 验证

```bash
TOKEN_FILE=~/token.txt make verify
```

`TOKEN_FILE` 里放当前 DCE 用户的 JWT（浏览器控制台 `localStorage.getItem('ANAKIN_TOKEN')`）。
输出 9 条 pass/fail；不带 token 时身份相关用例标 SKIP。

## 常见失败

| 现象 | 原因 | 处理 |
|---|---|---|
| bootstrap 第 0 步失败 | 缺默认 StorageClass / gVisor | 按提示准备 |
| `poc-gateway` 一直 `<pending>` | 集群没 LB 实现 | `GATEWAY_MODE=nodeport` 重跑 |
| adapter CrashLoopBackOff | 缺 `dce-jwks` ConfigMap（`make install-agent` 会建） | 确认 JWKS 与 DCE 一致 |
| Actor `no free workers` | WorkerPool 没设 `template.resources.limits.memory` | 补上资源限制（见 gotchas） |
| golden snapshot 一直不就绪 | Actor 镜像没监听 `:8081/readyz` | 确认 shim 有 readiness 监听 |
| 沙箱里模型 `no API key` | 凭据格式/环境变量不对 | 用 `version:1 + refs:` + `DEEPSEEK_API_KEY` env |
| 沙箱里模型 `TRANSPORT` | 沙箱出网没修 | 重跑 `make install-dependency`（已内置 DNS 修复） |
| 网关 401 | issuer / audience 与 DCE 不一致 | 对照 token 的 `iss` / `aud` |

更多细节见 [gotchas.md](gotchas.md)。

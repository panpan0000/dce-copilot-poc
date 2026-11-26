# 安装

一台 x86（linux/amd64）Kubernetes 集群 ≈ 30 分钟。前提条件不满足时，`make install-dependency` 会停在第一步并告诉你缺什么。

## 0. 换集群 / 换环境要改什么（对照清单）

整个项目只有**一个配置入口** `config.env`，换环境改它就够了。下面是"最容易漏"的清单，按重要程度排：

| 改什么 | 变量 | 怎么填 | 漏了会怎样 |
|---|---|---|---|
| **DCE 地址** | `DCE_ORIGIN` | 你登录 DCE 的浏览器地址（不带 `/`） | 身份全挂 |
| **DCE issuer** | `JWT_ISSUER` | `<DCE_ORIGIN>/auth/realms/ghippo` | 网关 401 |
| **DCE audience** | `JWT_AUDIENCE` | 通常 `__internal-ghippo` | 网关 401 |
| **DCE 集群名** | `DCE_CLUSTER` | DCE 里纳管的 K8s 集群名（kubectl 用例打它） | kubectl 用例错 |
| **DCE 公钥（JWKS）** | 文件 | `curl <DCE_ORIGIN>/auth/realms/ghippo/protocol/openid-connect/certs > deploy/jwks/jwks.json` | 网关 401 |
| **扩展匹配源** | `EXTENSION_MATCH_ORIGIN` | 与 `DCE_ORIGIN` 一致 | 面板不出现 |
| **镜像仓库** | `IMAGE_REGISTRY` | 集群能匿名拉到的仓库 | 拉不到镜像 |
| **存储类** | `STORAGE_CLASS` | 新集群的默认 StorageClass | PVC 卡 Pending |
| **网关模式** | `GATEWAY_MODE` | 集群没 LB 就 `nodeport` | 入口一直 pending |
| **节点 IP** | `GATEWAY_IP` | NodePort 模式填节点 IP（脚本也会自动探测） | 扩展连不上 |
| **KUBECONFIG** | 环境变量 | `export KUBECONFIG=...` 指向新集群 | 命令打到旧集群 |
| **模型 key** | `secrets/.env.secret` | 新环境的 DeepSeek key | 沙箱里 no API key |

**最容易被忽略的 3 个**：① JWKS（换 DCE 必须重新拉，否则 401）；② `EXTENSION_MATCH_ORIGIN`（不跟 DCE 一致，面板不出现）；③ `STORAGE_CLASS`（换集群常忘记改，PVC 卡死）。

填完跑 `make check`，它会逐项告诉你哪个还是模板值、哪个连不上。

## 1. 前置条件

| 需要 | 为什么 | 怎么准备 |
|---|---|---|
| Kubernetes ≥ 1.28 + Helm 3 | 装 kgateway / substrate / kagent | 任意发行版，k3s / kind / 云托管都行 |
| **默认 StorageClass** | substrate 的快照与 Actor 目录要 PVC | k3s 自带 local-path；云上用默认云盘 SC |
| **可拉取 `release-ci` 镜像** | 集群节点要匿名拉 `release-ci.daocloud.io`（已公开） | 节点出网策略放行该域名，或配 registry mirror |
| **一个 DCE 环境** | 身份、权限、集群托管都来自 DCE | 提供 kpanda / insight / baize 接口，知道 issuer / audience |
| **节点磁盘要快（SSD/NVMe）** | **checkpoint/restore 的瓶颈在磁盘 I/O**（见 README「性能前提」） | 慢盘会拖到秒级 |

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

# 拉 DCE 公钥（JWKS），换 DCE 必须重拉，否则网关校验 token 会 401
curl -fsSL "$(grep '^DCE_ORIGIN=' .env | cut -d= -f2)/auth/realms/ghippo/protocol/openid-connect/certs" \
  > deploy/jwks/jwks.json

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
# chrome://extensions → 开发者模式 → 加载已解压的扩展程序 → 选仓库里的 extension/（当前版本 0.6.0）
```

**首次安装会自动弹出设置页**；之后也可以从扩展详情 → 扩展程序选项打开。
只需填网关地址；DCE 地址留空（扩展会自动用当前页面地址）。
DCE 登录后刷新页面即可。

> 换 DCE 时记得把 `extension/manifest.json` 的 `content_scripts[0].matches` 加上你的 DCE 域名。

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

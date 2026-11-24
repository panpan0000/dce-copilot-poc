# 换一套 Kubernetes 集群复刻

**这是参考，不是一键安装包。** 全流程未在空集群完整复跑。

## 前置
- Kubernetes + Helm 3 + kubectl + Docker + 可被集群拉取的镜像仓库 + StorageClass
- 一个可用的 DCE（≥ 提供 kpanda / insight / baize 接口）
- 能执行 NetworkPolicy 的 CNI（若要开 networkpolicy）
- 节点架构与镜像一致（本机是 macOS/arm64，服务器多为 amd64）

## 步骤
1. `cp config.env.example .env`，填 DCE 地址、镜像仓库、网关地址
2. `make check` —— 配置/依赖/连通性自检，缺什么它会说
3. `make build` —— 4 个镜像（构建期按架构取 dce CLI、按 tag 取 skills）
4. `make bootstrap` —— Gateway API → kgateway → substrate → kagent
5. `make deploy` —— 应用侧资源（自动回填 Actor digest、等快照就绪）
6. `make extension` —— 生成扩展 config.js 并打包
7. `make verify` —— 端到端用例，输出 pass/fail

## DSH 基础镜像
`images/dsh-a2a` 与 `images/sre-agent` 都 `FROM ${DSH_BASE_IMAGE}`。
这个基础镜像是 DSH 运行时，**仓库不含**。没有它就只能跑到 "构建失败"。
有两条路：
- 用你们自己的 DSH 官方镜像，把 `DSH_BASE_IMAGE` 指过去；
- 按 DSH 文档自己构建一个，只要满足：`dsh` 在 PATH、`/home/agent/.dsh` 可写、Node 可用。

## 与 DCE 的耦合点（三处，改这里）
1. `config.env`: `DCE_ORIGIN` / `DCE_CLUSTER` / `JWT_ISSUER` / `JWT_AUDIENCE`
2. `deploy/jwks/jwks.json`: DCE 的公钥（`curl $ISSUER/protocol/openid-connect/certs`）
3. `DCE_APISERVER`（可选）：源码里对内网 server 地址做了替换，换环境要检查映射与 CA

## 常见失败
| 现象 | 原因 |
|---|---|
| adapter 启动即退出 | `.env` 缺项或 JWKS 路径错 |
| 网关 401 | issuer/audience 不匹配，或 JWKS 里没有对应 kid |
| SandboxAgent 拒绝 | 用了 tag 而不是 digest（`deploy.sh` 会自动回填） |
| Actor 创建失败 | 见 gotchas 第 1 条（cgroup） |
| 扩展请求被 CORS/跨域挡 | `EXTENSION_GATEWAY_BASE` 与浏览器可达地址不一致 |

## 回滚
```bash
kubectl -n kagent set image deploy/kagent-controller controller=ghcr.io/kagent-dev/kagent/controller:0.10.0-beta6
kubectl -n ${NS_APP} delete -f build/render/sandboxagent.yaml   # 删 Actor 模板
```

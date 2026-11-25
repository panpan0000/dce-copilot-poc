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

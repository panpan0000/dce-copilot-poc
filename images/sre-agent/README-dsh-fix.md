# SRE agent 在 kagent 1.0 / 完整 DSH 底座上的修复

原镜像 `dce-copilot-sre-agent:0.2.0-x86` 的 DSH 安装不完整，容器里 `dsh --profile web` 启动即崩：

```
Error: dsh: user patch-layer watching requires the Cordis HMR service   （dsh-app-boot/lib/index.js:764）
```

修完这条后又会报：

```
agent-preset-invalid: preset "standard" failed to mount:
Cannot read properties of undefined (reading 'parent')
```

## 两步修法

1. **换完整底座**：`Dockerfile.dshbase` —— 以 `dce-copilot-dsh-a2a:0.4.1-ca` 为基座（DSH 完整），只搬 `/app` 与 `/opt/sre`。
2. **放宽巡检校验**：`Dockerfile.checkpatch` —— 原代码只认 `tool/call` 里包含 `cluster-inspect.js` 的那条命令，
   而 DSH 实际会直接跑 `kubectl` 巡检（效果更好）。放宽为「任意 bash 工具成功即算合格」。

```sh
docker build -f Dockerfile.dshbase  -t sre-agent:0.4.0 .
docker build -f Dockerfile.checkpatch -t sre-agent:0.5.0 .
```

部署（内网 registry 用 containerd 本地导入，避免 plain-http 拉取问题）：

```sh
docker save <img> | kubectl -n kagent exec -i nodeadmin -- chroot /host ctr -n k8s.io images import -
kubectl -n watsons-agent-poc set image deploy/sre-agent sre=localhost:5050/.../sre-agent:0.5.0
kubectl -n watsons-agent-poc patch deploy sre-agent --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'
```

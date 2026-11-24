# 没有 DCE 时怎么跑（骨架）

本仓库默认假设有 DCE。没有 DCE 时，身份与权限这一段是空的，但**沙箱与流式链路**仍可验证。

最小替代：
1. 起一个本地 JWKS 签发器（任意 RSA key，issuer 固定），把 `deploy/jwks/jwks.json` 换成它
2. 在 `config.env` 里把 `DCE_ORIGIN` 指到一个 mock 服务，实现两个接口即可：
   - `POST /apis/kpanda.io/v1alpha1/clusters/<c>/clustercert` → 返回一个本地 kubeconfig
   - `GET  /apis/ghippo.io/v1alpha1/current-user` → 返回 `{"username": "<user>"}`
3. `dce` CLI 会失败（它需要真实 DCE），但 `kubectl` 与 DSH 对话可以跑通

TODO：把上面的 mock 服务做成 `images/mock-dce/`（约 100 行 Go）。

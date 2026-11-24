# 没有 DCE 时怎么跑

`images/mock-dce/` 是一个 150 行的假 DCE，只为你能在没有 DCE 的环境里把链路跑起来。
**绝不用于生产**：它不校验 token，任何 string 都当合法身份。

## 它提供什么

| 路径 | 作用 |
|---|---|
| `GET /.well-known/jwks.json` | 启动时生成的 RSA 公钥 |
| `GET /dev/token?user=alice` | 直接签发一个可用的测试 JWT（仅 mock 有） |
| `GET /apis/ghippo.io/v1alpha1/current-user` | 返回 `{username, uid}` |
| `POST /apis/kpanda.io/v1alpha1/clusters/<c>/clustercert` | 返回 `MOCK_KUBECONFIG_B64` |
| `GET /apis/kpanda.io/v1alpha1/asl/namespaces` | 返回假命名空间 `alice` |
| `GET .../namespaces/<ns>/pods` | 返回一个假 Pod |

## 用法

```bash
# 1. 起 mock
docker build -t mock-dce images/mock-dce
docker run -d -p 8090:8090 -e JWT_ISSUER=http://localhost:8090/realms/mock \
  -e JWT_AUDIENCE=mock-audience -e MOCK_KUBECONFIG_B64="$(base64 < 你的kubeconfig)" mock-dce

# 2. 把 .env 指向 mock
JWT_ISSUER=http://localhost:8090/realms/mock
JWT_AUDIENCE=mock-audience
DCE_ORIGIN=http://localhost:8090
DCE_CLUSTER=mock

# 3. 取 token 并跑 verify
TOKEN=$(curl -s "http://127.0.0.1:8090/dev/token?user=alice" | jq -r .token)
TOKEN=$TOKEN make verify
```

## 与真实 DCE 的差距
- `dce` CLI 会返回 404（mock 没实现 baize/insight 的完整接口）——这是预期的
- kubectl 能否用取决于你给的 `MOCK_KUBECONFIG_B64`
- 身份隔离、越权测试在 mock 里没有意义

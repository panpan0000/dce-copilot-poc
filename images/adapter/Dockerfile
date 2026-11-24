# substrate-adapter：身份 → 会话、SSE 流式、DCE kubeconfig、A2A 转发
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /adapter .

FROM busybox:1.36
COPY --from=build /adapter /adapter
# JWKS 由 deploy 里的 ConfigMap 挂到 /etc/adapter/jwks.json（见 deploy/manifests/adapter.yaml）
EXPOSE 9099
ENTRYPOINT ["/adapter"]

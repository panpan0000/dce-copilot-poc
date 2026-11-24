#!/usr/bin/env bash
# 从上游源码构建带补丁的 controller 二进制（需要 github 可达 + Go 1.26.3）。
# 产物 codex-kagent-controller-fixed 放回本目录，scripts/build.sh 才会构建镜像。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
KAGENT_REPO="${KAGENT_REPO:-https://github.com/kagent-dev/kagent}"
KAGENT_TAG="${KAGENT_TAG:-v0.10.0-beta6}"
GO_VERSION_REQUIRED="${GO_VERSION_REQUIRED:-1.26.3}"

need() { command -v "$1" >/dev/null || { echo "缺 $1" >&2; exit 1; }; }
need git; need go
go version | grep -q "$GO_VERSION_REQUIRED" || {
  echo "需要 Go $GO_VERSION_REQUIRED（当前 $(go version)）——补丁必须与官方二进制的构建参数一致" >&2; exit 2; }

src="$(mktemp -d)"; trap 'rm -rf "$src"' EXIT
git clone --depth 1 --branch "$KAGENT_TAG" "$KAGENT_REPO" "$src/kagent"
cd "$src/kagent"
git apply "$ROOT/kagent-resume-race.patch"
# ldflags 必须与官方镜像一致：从原二进制 `go version -m` 里取
LD_FLAGS="${LD_FLAGS:?请从官方镜像 /app 的 go version -m 提取 -ldflags 后用 LD_FLAGS=... 重跑}"
CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH:-arm64}" \
  go build -trimpath -ldflags "$LD_FLAGS" -o "$HERE/codex-kagent-controller-fixed" ./core/cmd/controller
echo "已生成 $HERE/codex-kagent-controller-fixed"

#!/usr/bin/env bash
# 构建三个自建镜像：dsh-a2a（Actor 运行时）、adapter、controller-fix。
# dce CLI 与 skills 在构建期获取，不进 git（体积大 + 平台相关 + 会漂移）。
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env
require_vars IMAGE_REGISTRY IMAGE_TAG DSH_BASE_IMAGE DCE_SKILLS_VERSION
reject_placeholders IMAGE_REGISTRY DSH_BASE_IMAGE

BUILD_DIR="$REPO_ROOT/build"
CTX_DSH="$BUILD_DIR/ctx-dsh-a2a"
mkdir -p "$BUILD_DIR"

need docker
DOCKER="${DOCKER:-docker}"

# ── 1) 准备 dsh-a2a 的构建上下文 ─────────────────────────────
log "准备构建上下文: $CTX_DSH"
rm -rf "$CTX_DSH"
mkdir -p "$CTX_DSH"
cp "$REPO_ROOT"/images/dsh-a2a/*.js "$CTX_DSH"/
cp "$REPO_ROOT"/images/dsh-a2a/*.sh "$CTX_DSH"/
cp "$REPO_ROOT"/images/dsh-a2a/dce "$CTX_DSH"/dce
cp "$REPO_ROOT"/images/dsh-a2a/Dockerfile "$CTX_DSH"/

# 1a) dce CLI：按目标架构下载官方 release，校验 sha256
DCE_CLI_VERSION="${DCE_CLI_VERSION:-$DCE_SKILLS_VERSION}"
case "$(uname -m)" in
  arm64|aarch64) DCE_ARCH=arm64 ;;
  x86_64|amd64)  DCE_ARCH=amd64 ;;
  *) die "不支持的架构: $(uname -m)" ;;
esac
DCE_TARBALL="dce-${DCE_CLI_VERSION}-linux-${DCE_ARCH}.tar.gz"
DCE_URL="https://github.com/DaoCloud/daocloud-skills/releases/download/${DCE_CLI_VERSION}/${DCE_TARBALL}"
log "下载 dce CLI: $DCE_URL"
curl -fsSL --retry 3 -o "$BUILD_DIR/$DCE_TARBALL" "$DCE_URL" \
  || die "下载失败。离线环境请手动放到 $BUILD_DIR/$DCE_TARBALL 后重跑"
tar -xzf "$BUILD_DIR/$DCE_TARBALL" -C "$BUILD_DIR"
DCE_BIN="$(find "$BUILD_DIR" -maxdepth 2 -name dce -type f -perm -u+x | head -1)"
[ -n "$DCE_BIN" ] || die "压缩包里没找到 dce 可执行文件"
install -m 0755 "$DCE_BIN" "$CTX_DSH/dce-bin"

# 1b) skills：固定 tag 浅克隆，只取需要的几个
log "获取 skills @ $DCE_SKILLS_VERSION"
SKILLS_SRC="$BUILD_DIR/daocloud-skills"
rm -rf "$SKILLS_SRC"
git clone --depth 1 --branch "$DCE_SKILLS_VERSION" \
  https://github.com/DaoCloud/daocloud-skills "$SKILLS_SRC" 2>/dev/null \
  || die "clone 失败。离线环境请把仓库放到 $SKILLS_SRC"
mkdir -p "$CTX_DSH/skills"
for s in dce container-management-pod-diagnosis container-management-cluster-diagnosis; do
  [ -d "$SKILLS_SRC/skills/$s" ] || die "skills 目录里没有 $s"
  cp -R "$SKILLS_SRC/skills/$s" "$CTX_DSH/skills/"
done
ok "skills: $(ls "$CTX_DSH/skills" | tr '\n' ' ')"

# ── 2) 构建 ────────────────────────────────────────────────
build() {
  local name="$1" ctx="$2"; shift 2
  local ref; ref="$(image_ref "$name")"
  log "构建 $ref"
  "$DOCKER" build -t "$ref" "$@" "$ctx"
  ok "$ref"
}

build dsh-a2a "$CTX_DSH" --build-arg "DSH_BASE_IMAGE=$DSH_BASE_IMAGE"
build adapter "$REPO_ROOT/images/adapter"
build controller-fix "$REPO_ROOT/images/controller-fix"

# ── 3) 推送（可选） ────────────────────────────────────────
if [ "${PUSH_IMAGES:-false}" = "true" ]; then
  for n in dsh-a2a adapter controller-fix; do
    ref="$(image_ref "$n")"
    log "推送 $ref"
    "$DOCKER" push "$ref"
  done
fi

# ── 4) 输出 digest（BYO SandboxAgent 必须用 digest 固定） ──
log "镜像 digest（deploy 时会自动写入 SandboxAgent）"
for n in dsh-a2a adapter controller-fix; do
  ref="$(image_ref "$n")"
  d="$("$DOCKER" image inspect "$ref" --format '{{index .RepoDigests 0}}' 2>/dev/null || true)"
  printf '  %-16s %s\n' "$n" "${d:-<未推送，无 digest>}"
done

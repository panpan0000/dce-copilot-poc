#!/usr/bin/env bash
# 当集群不能出网（拉不到 ghcr.io / cr.kgateway.dev 等）时，用一台能出网的机器
# 把镜像搬过去：pull → save → scp → nerdctl load。
#
# 用法：
#   NODE=root@172.30.40.186 IMAGES="img1 img2" scripts/fetch-images.sh
#   NODE=root@172.30.40.186 scripts/fetch-images.sh --from-charts   # 从 helm chart 里自动提取
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env

NODE="${NODE:?需要 NODE=user@host（目标节点，需能 ssh）}"
PLATFORM="${TARGET_PLATFORM:-linux/amd64}"
NS_CONTAINERD="${NS_CONTAINERD:-k8s.io}"
WORK="${WORK:-/tmp/image-transfer}"
DOCKER="${DOCKER:-docker}"
mkdir -p "$WORK"

# --from-charts：渲染两个 chart，提取所有 image 行
if [ "${1:-}" = "--from-charts" ]; then
  log "从 helm chart 提取镜像清单"
  IMAGES="$(
    { helm template substrate oci://ghcr.io/kagent-dev/substrate/helm/substrate --version "$SUBSTRATE_VERSION" -n "$NS_SUBSTRATE" 2>/dev/null
      helm template kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent --version "$KAGENT_VERSION" -n "$NS_KAGENT" 2>/dev/null
      helm template kgateway oci://cr.kgateway.dev/kgateway-dev/charts/kgateway --version "$KGATEWAY_VERSION" -n "$NS_GATEWAY" 2>/dev/null
    } | grep -oE 'image: *"?[^"[:space:]]+' | sed 's/image: *"*//' | sort -u | grep -v '^$'
  )"
else
  IMAGES="${IMAGES:?需要 IMAGES="img1 img2" 或 --from-charts}"
fi

echo "$IMAGES" | sed 's/^/  /'
COUNT=$(echo "$IMAGES" | wc -l | tr -d ' ')
log "共 $COUNT 个镜像，开始搬运到 $NODE"

# 优先在节点上用国内镜像源拉取 + retag（不搬 tar）；失败再回退到本机搬运。
GHCR_MIRROR="${GHCR_MIRROR:-ghcr.nju.edu.cn}"
i=0
for img in $IMAGES; do
  i=$((i+1))
  case "$img" in *:*) ;; *) img="$img:latest";; esac
  # ⓪ 节点上已有就跳过
  if ssh -o ConnectTimeout=10 "$NODE" "nerdctl --namespace $NS_CONTAINERD images --format '{{.Repository}}:{{.Tag}}' | grep -qx '${img}'"; then
    ok "[$i/$COUNT] ${img}（已存在）"; continue
  fi

  # ① 节点侧镜像源
  case "$img" in
    ghcr.io/*)
      mimg="$GHCR_MIRROR/${img#ghcr.io/}"
      if ssh -o ConnectTimeout=10 "$NODE" "nerdctl --namespace $NS_CONTAINERD pull '$mimg' >/dev/null 2>&1 && nerdctl --namespace $NS_CONTAINERD tag '$mimg' '$img'"; then
        ok "[$i/$COUNT] ${img}（mirror）"; continue
      fi ;;
  esac

  # ② 回退：本机拉取后搬运
  tar="$WORK/$(echo "$img" | tr '/:' '__').tar"
  log "[$i/$COUNT] pull $img"
  "$DOCKER" pull --platform "$PLATFORM" "$img" >/dev/null || { warn "拉取失败，跳过: $img"; continue; }
  "$DOCKER" save "$img" -o "$tar"
  scp -o ConnectTimeout=10 "$tar" "$NODE:/tmp/" >/dev/null
  ssh -o ConnectTimeout=10 "$NODE" "nerdctl --namespace $NS_CONTAINERD load -i /tmp/$(basename "$tar") >/dev/null"
  rm -f "$tar"
  ok "[$i/$COUNT] $img"
done
ok "全部完成：$COUNT 个镜像已进入 $NODE 的 containerd（namespace=${NS_CONTAINERD}）"

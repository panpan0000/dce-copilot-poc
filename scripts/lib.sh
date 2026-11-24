# 公共 shell 库：加载配置、校验、日志。所有脚本 source 它。

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env}"
SECRET_FILE="${SECRET_FILE:-$REPO_ROOT/secrets/.env.secret}"

log()  { printf '\033[0;36m›\033[0m %s\n' "$*"; }
ok()   { printf '\033[0;32m✓\033[0m %s\n' "$*"; }
warn() { printf '\033[0;33m!\033[0m %s\n' "$*"; }
die()  { printf '\033[0;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

load_env() {
  [ -f "$ENV_FILE" ] || die "缺少 $ENV_FILE —— 先执行: cp config.env.example .env && \$EDITOR .env"
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  if [ -f "$SECRET_FILE" ]; then
    # shellcheck disable=SC1090
    . "$SECRET_FILE"
  fi
  set +a
}

# 必填项校验：缺哪个都明确报出来，不要等到 401 才发现
require_vars() {
  local missing=()
  for v in "$@"; do
    if [ -z "${!v:-}" ]; then missing+=("$v"); fi
  done
  if [ ${#missing[@]} -gt 0 ]; then
    die "配置缺项: ${missing[*]}（见 config.env.example）"
  fi
}

# 占位符检查：防止把 example 值直接拿去部署
reject_placeholders() {
  local bad=()
  for v in "$@"; do
    case "${!v:-}" in
      *example.com*|*xxxxxxxx*|*sk-xxxx*) bad+=("$v");;
    esac
  done
  if [ ${#bad[@]} -gt 0 ]; then
    die "以下配置还是模板值，请改成真实值: ${bad[*]}"
  fi
}

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"; }

image_ref() { printf '%s/%s:%s' "$IMAGE_REGISTRY" "$1" "$IMAGE_TAG"; }

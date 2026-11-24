#!/usr/bin/env bash
# 端到端用例：这是"能不能复刻"的唯一证据，README 不算。
# 用法: TOKEN=<用户JWT> make verify    或    make verify TOKEN=...
set -uo pipefail
. "$(dirname "$0")/lib.sh"
load_env
require_vars EXTENSION_GATEWAY_BASE
BASE="${VERIFY_BASE:-${EXTENSION_GATEWAY_BASE%/}/sandbox}"
# TOKEN 或 TOKEN_FILE（浏览器里 localStorage.getItem('ANAKIN_TOKEN')）
if [ -z "${TOKEN:-}" ] && [ -n "${TOKEN_FILE:-}" ] && [ -f "${TOKEN_FILE}" ]; then
  TOKEN="$(cat "${TOKEN_FILE}")"
fi
TOKEN="${TOKEN:-}"

PASS=0; FAIL=0; SKIP=0
res() { printf '  %-42s %s\n' "$1" "$2"; }
ok_() { PASS=$((PASS+1)); res "$1" "$(printf '\033[0;32mPASS\033[0m')"; }
no_() { FAIL=$((FAIL+1)); res "$1" "$(printf '\033[0;31mFAIL\033[0m')  $2"; }
sk_() { SKIP=$((SKIP+1)); res "$1" "$(printf '\033[0;33mSKIP\033[0m')  $2"; }

if [ -z "$TOKEN" ]; then
  warn "没有 TOKEN，跳过需要身份的用例（从浏览器 localStorage['ANAKIN_TOKEN'] 取）"
fi

echo "── 网关 ─────────────────────────────────"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$BASE/poc/whoami")
[ "$code" = "401" ] && ok_ "未带 token 被网关拒绝 (401)" || no_ "未带 token 被网关拒绝" "got $code"
[ -n "$TOKEN" ] || { sk_ "身份用例" "无 TOKEN"; }

"$REPO_ROOT/scripts/smoke.sh" 2>/dev/null || true

echo "── 身份与沙箱 ───────────────────────────"
if [ -n "$TOKEN" ]; then
  who=$(curl -s --max-time 15 -H "Authorization: Bearer $TOKEN" "$BASE/poc/whoami")
  case "$who" in
    *"not in the form of"*) sk_ "身份用例" "TOKEN 无效（需要真实 DCE JWT）"; TOKEN="";;
  esac
  [ -n "$TOKEN" ] || true
  uid=$(printf '%s' "$who" | sed -n 's/.*"username":"\([^"]*\)".*/\1/p')
  [ -n "$uid" ] && ok_ "whoami 解析到用户 ($uid)" || no_ "whoami 解析到用户" "$who"
fi

ask() { # $1=问题 $2=期望包含的正则 $3=用例名
  local body resp
  body=$(printf '{"message":%s,"conversation_id":"verify%s"}' \
    "$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$1")" "$RANDOM")
  resp=$(curl -s --max-time 200 -X POST -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' \
    -d "$body" "$BASE/poc/chat")
  if printf '%s' "$resp" | grep -Eq "$2"; then ok_ "$3"; else no_ "$3" "$(printf '%s' "$resp" | head -c 120)"; fi
}

if [ -n "$TOKEN" ]; then
  [ -n "$TOKEN" ] && ask "只回答 ok" 'ok|OK' "自然对话（非流式）"
  [ -n "$TOKEN" ] && ask "执行 shell: uname -s" 'Linux|linux' "沙箱内执行 shell"
  [ -n "$TOKEN" ] && ask "用 kubectl 看看可见命名空间的 pod" 'pod|Pod|命名空间|Forbidden' "kubectl（本人身份）"
  [ -n "$TOKEN" ] && ask "用 dce 列出我可见的 AI Lab notebook 实例，贴真实 HTTP 状态" 'dce|HTTP|404|200|工作空间' "DCE API（dce CLI）"
  [ -n "$TOKEN" ] && ask "看看 SRE 巡检报告" 'inspection|巡检|report|inspection_id' "SRE 巡检报告"

  [ -n "$TOKEN" ] || { sk_ "流式" "无有效 TOKEN"; }
  [ -n "$TOKEN" ] && echo "── 流式 ─────────────────────────────────"
  [ -n "$TOKEN" ] && sse=$(curl -sN --max-time 120 -X POST -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' \
    -d '{"message":"数到三","conversation_id":"verifystream"}' "$BASE/poc/chat/stream")
  [ -n "$TOKEN" ] && printf '%s' "$sse" | grep -q 'delta' && ok_ "流式返回 delta" || no_ "流式返回 delta" "无 delta"
  [ -n "$TOKEN" ] && printf '%s' "$sse" | grep -q '"done"' && ok_ "流式返回 done" || no_ "流式返回 done" "无 done"
fi

echo
printf '结果: %d pass · %d fail · %d skip\n' "$PASS" "$FAIL" "$SKIP"
[ "$FAIL" -eq 0 ] || exit 1

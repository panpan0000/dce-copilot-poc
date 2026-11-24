#!/usr/bin/env bash
# 生成扩展的 config.js（不让人改源码）并打出 zip。
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_env
require_vars EXTENSION_MATCH_ORIGIN EXTENSION_GATEWAY_BASE DCE_ORIGIN

OUT="$REPO_ROOT/build/extension"
rm -rf "$OUT"; mkdir -p "$OUT"
cp "$REPO_ROOT"/extension/{content.js,service_worker.js,panel.css} "$OUT"/

cat > "$OUT/config.js" <<CFG
// 由 scripts/extension.sh 生成，勿手改
globalThis.DCE_AGENT_CONFIG = {
  dceOrigin: "${EXTENSION_MATCH_ORIGIN}",
  gatewayBase: "${EXTENSION_GATEWAY_BASE}",
};
CFG

# manifest 用配置里的 origin 生成 matches / host_permissions
python3 - "$REPO_ROOT/extension/manifest.json" "$OUT/manifest.json" "$EXTENSION_MATCH_ORIGIN" "$EXTENSION_GATEWAY_BASE" <<'PY'
import json, sys
src, dst, origin, gw = sys.argv[1:5]
d = json.load(open(src))
d["host_permissions"] = [f"{origin}/*", f"{gw}/*"]
d["content_scripts"][0]["matches"] = [f"{origin}/*"]
d["content_scripts"][0]["js"] = ["config.js"] + [j for j in d["content_scripts"][0]["js"] if j != "config.js"]
json.dump(d, open(dst, "w"), indent=2, ensure_ascii=False)
PY

(cd "$OUT" && zip -qr "$REPO_ROOT/build/dce-copilot-extension.zip" .)
ok "扩展已生成: build/extension/ 与 build/dce-copilot-extension.zip"
echo "  chrome://extensions → 开发者模式 → 加载已解压的扩展程序 → $OUT"

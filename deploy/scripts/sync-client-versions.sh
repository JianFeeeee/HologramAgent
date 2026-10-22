#!/usr/bin/env bash
#
# 客户端版本与内核版本同步。
#
# 为什么要有这个脚本：内核的 internal/meta/meta.go Version 是唯一事实源，
# 而各客户端各有各的版本字段——GUI 在 package.json、鸿蒙在 AppScope/app.json5、
# waiter 走编译期注入。手工各改各的必然漂移（写这个脚本时的现状：内核 1.4.0、
# GUI 1.0.0、鸿蒙 1.1.1，三个号互不相干）。
#
# 用法：
#   bash deploy/scripts/sync-client-versions.sh            # 同步到内核当前版本
#   bash deploy/scripts/sync-client-versions.sh 1.4.0      # 同步到指定版本
#   bash deploy/scripts/sync-client-versions.sh --check    # 只校验，漂移则退出 1
#
# 同步目标：
#   cmd/gui/package.json                        version
#   cmd/ohos/HologramAgent/AppScope/app.json5       versionName + versionCode
#
# waiter 不在此列：它直接引用 internal/meta.Version（同一进程内编译），
# 没有第二份版本字段可漂。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

CHECK=0
VERSION=""
for arg in "$@"; do
  case "$arg" in
    --check) CHECK=1 ;;
    *) VERSION="$arg" ;;
  esac
done

# 未显式给版本时，从内核唯一事实源读。
if [ -z "$VERSION" ]; then
  VERSION="$(grep -oE 'Version = "[^"]+"' "$ROOT/internal/meta/meta.go" | head -1 | sed -E 's/.*"([^"]+)".*/\1/')"
fi
[ -n "$VERSION" ] || { echo "sync-client-versions: 无法确定版本号（internal/meta/meta.go 里没找到 Version）" >&2; exit 1; }

# versionCode 规则：X*1e6 + Y*1e3 + Z。鸿蒙要求 versionCode 单调递增的整数，
# 直接搬 semver 会丢信息，所以用主/次/补丁三段编码（1.4.0 → 1004000）。
CODE="$(python3 - "$VERSION" <<'PY'
import re, sys
m = re.match(r'^(\d+)\.(\d+)\.(\d+)', sys.argv[1])
if not m:
    sys.exit("sync-client-versions: 版本号必须是 X.Y.Z 形态，得到 %r" % sys.argv[1])
print(int(m.group(1)) * 1000000 + int(m.group(2)) * 1000 + int(m.group(3)))
PY
)"

GUI_PKG="$ROOT/cmd/gui/package.json"
OHOS_APP="$ROOT/cmd/ohos/HologramAgent/AppScope/app.json5"

DRIFT=0
note() { printf '  %-52s %s\n' "$1" "$2"; }

# ── GUI ──
gui_cur="$(python3 - "$GUI_PKG" <<'PY'
import json, sys
print(json.load(open(sys.argv[1]))["version"])
PY
)"
if [ "$gui_cur" != "$VERSION" ]; then
  DRIFT=1
  if [ "$CHECK" -eq 1 ]; then
    note "cmd/gui/package.json" "$gui_cur → 应为 $VERSION"
  else
    python3 - "$GUI_PKG" "$VERSION" <<'PY'
import json, sys
p, v = sys.argv[1], sys.argv[2]
d = json.load(open(p))
d["version"] = v
# indent=2 保留原格式；末尾补换行，避免 diff 噪声
with open(p, "w") as f:
    json.dump(d, f, indent=2, ensure_ascii=False)
    f.write("\n")
PY
    note "cmd/gui/package.json" "$gui_cur → $VERSION"
  fi
fi

# ── 鸿蒙 ──
ohos_name="$(grep -oE '"versionName"[[:space:]]*:[[:space:]]*"[^"]+"' "$OHOS_APP" | head -1 | sed -E 's/.*"([^"]+)"$/\1/')"
ohos_code="$(grep -oE '"versionCode"[[:space:]]*:[[:space:]]*[0-9]+' "$OHOS_APP" | head -1 | grep -oE '[0-9]+$')"
if [ "$ohos_name" != "$VERSION" ] || [ "$ohos_code" != "$CODE" ]; then
  DRIFT=1
  if [ "$CHECK" -eq 1 ]; then
    note "cmd/ohos AppScope/app.json5" "$ohos_name/$ohos_code → 应为 $VERSION/$CODE"
  else
    # app.json5 带注释，不是严格 JSON，用 sed 定点替换两个字段。
    sed -i -E "s/(\"versionCode\"[[:space:]]*:[[:space:]]*)[0-9]+/\1$CODE/" "$OHOS_APP"
    sed -i -E "s/(\"versionName\"[[:space:]]*:[[:space:]]*\")[^\"]+/\1$VERSION/" "$OHOS_APP"
    note "cmd/ohos AppScope/app.json5" "$ohos_name/$ohos_code → $VERSION/$CODE"
  fi
fi

echo "内核版本: $VERSION (versionCode $CODE)"
if [ "$CHECK" -eq 1 ]; then
  if [ "$DRIFT" -eq 1 ]; then
    echo "sync-client-versions: 客户端版本与内核不一致（见上）；跑 bash deploy/scripts/sync-client-versions.sh 同步" >&2
    exit 1
  fi
  echo "sync-client-versions: OK，客户端与内核版本一致"
fi

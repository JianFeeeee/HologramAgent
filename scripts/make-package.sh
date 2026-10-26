#!/usr/bin/env bash
# 制作 HomeAgent 复合安装包（含内核 OTA 工具）。
#
# 包结构（与既有 1.4.0-20261017 包保持一致，新增 homed-upgrade）：
#   bin/homed            内核（内置插件已编进去）
#   bin/depgraph.bin     启动依赖图（与 homed 必须同批，digest 绑定）
#   bin/waiter           CLI
#   bin/hmapdev          插件工具链（29MB，现场编译外部插件用）
#   bin/homed-upgrade    ★ 内核 OTA：探测 + 校验 + 备份 + 原子替换 + 回滚
#   sdk/                 公开 SDK 源码（hmapdev 装到 ~/.homeagent/hmapdev/sdk）
#   install.sh           安装脚本
#   SHA256SUMS           ★ 全量校验和（OTA 升级前必须验）
#
# 为什么带 SHA256SUMS：homed-upgrade 的第一步就是校验它。没有校验和，
# "包在传输中被截断/篡改"只能等替换完、服务起不来才发现。
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
VERSION=$(grep -oE 'Version = "[0-9.]+"' internal/meta/meta.go | head -1 | grep -oE '[0-9.]+')
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
STAMP=$(date +%Y%m%d)
NAME="HomeAgent-linux-x86_64-${VERSION}-${STAMP}"
OUT="$ROOT/build/package/$NAME"

echo "== 构建 =="
make build >/dev/null
make build-cli >/dev/null
CGO_ENABLED=0 go build -o "$ROOT/build/homed-upgrade" ./cmd/homed-upgrade/
# hmapdev 来自 SDK 仓
if [ ! -x "$ROOT/build/hmapdev" ]; then
	(cd third_party/homeagent-sdk/tools/hmapdev && go build -o "$ROOT/build/hmapdev" .)
fi

echo "== 组装 $NAME =="
rm -rf "$OUT"
mkdir -p "$OUT/bin"
install -m 0755 "$ROOT/build/homed" "$OUT/bin/homed"
install -m 0644 "$ROOT/build/depgraph.bin" "$OUT/bin/depgraph.bin"
install -m 0755 "$ROOT/build/waiter" "$OUT/bin/waiter"
install -m 0755 "$ROOT/build/hmapdev" "$OUT/bin/hmapdev"
install -m 0755 "$ROOT/build/homed-upgrade" "$OUT/bin/homed-upgrade"
# 只带 SDK 源码，排除构建产物。
SDK_SRC=third_party/homeagent-sdk
mkdir -p "$OUT/sdk"
(cd "$SDK_SRC" && tar -cf - \
	--exclude='./example/*/rawfile' \
	--exclude='./example/*/dist' \
	--exclude='./example/*/build' \
	--exclude='./example/*/node_modules' \
	--exclude='./build' \
	--exclude='./site_build' \
	--exclude='./.git' \
	--exclude='*.bin' \
	--exclude='*.hmap' \
	.) | (cd "$OUT/sdk" && tar -xf -)

cat >"$OUT/install.sh" <<'INSTALL'
#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
PREFIX="${PREFIX:-/usr/local}"
BINDIR="$PREFIX/bin"

echo "== 安装到 PREFIX=$PREFIX =="
install -d "$BINDIR"

# 先校验再安装：截断的 homed 装上去服务就起不来，而那时旧版已被覆盖。
echo "== 校验 SHA256SUMS =="
( cd "$HERE" && sha256sum -c --quiet SHA256SUMS )

# ★ homed 与 depgraph.bin 必须同一目录：内核启动按「可执行文件同目录」
#   找依赖图（缺失/不匹配 ⇒ 拒绝启动）。
install -m 0755 "$HERE/bin/homed"         "$BINDIR/homed"
install -m 0644 "$HERE/bin/depgraph.bin"  "$BINDIR/depgraph.bin"
install -m 0755 "$HERE/bin/waiter"        "$BINDIR/waiter"
install -m 0755 "$HERE/bin/hmapdev"       "$BINDIR/hmapdev"
install -m 0755 "$HERE/bin/homed-upgrade" "$BINDIR/homed-upgrade"

SDKVER="$(grep -oE 'Version = "[0-9.]+"' "$HERE/sdk/meta/meta.go" | head -1 | grep -oE '[0-9.]+')"
DEST="${HOME:-/root}/.homeagent/hmapdev/sdk/v${SDKVER}"
echo "== 安装 SDK $SDKVER → $DEST =="
rm -rf "$DEST"; mkdir -p "$(dirname "$DEST")"
cp -r "$HERE/sdk" "$DEST"
echo "v${SDKVER}" > "$(dirname "$DEST")/current"

echo
echo "完成。验证："
echo "  homed --version              # 内核（含 commit/build_time）"
echo "  homed-upgrade -pkg X.tar.gz  # 内核 OTA 探测（默认不写盘）"
echo "  waiter --version             # CLI"
echo "  hmapdev --version            # 插件工具链"
INSTALL
chmod +x "$OUT/install.sh"

cat >"$OUT/README.txt" <<README
HomeAgent ${VERSION} (commit ${COMMIT}, ${STAMP})
================================================

安装：  sudo ./install.sh
        PREFIX=/opt ./install.sh    # 装到别处

内核 OTA（只升级内核，不动插件）
--------------------------------
  homed-upgrade -pkg <新包.tar.gz>          # 探测（默认，不写盘）
  homed-upgrade -pkg <新包.tar.gz> -apply   # 执行

探测内容：当前/包内版本比较、depgraph 是否同批、插件兼容性预判。
执行流程：校验 → 预检 → 备份 → 原子替换 → 重启 → 版本复核 → 失败自动回滚。

插件与内核的兼容性**不由 OTA 处理**：换内核后与旧 SDK 编的插件会握手失败，
内核把它们持久禁用并写明原因（界面上可见），重编或手动启用即可恢复。
README
# 校验和必须在全部产物生成**之后**一次算完（边打边算会漏掉后生成的文件）
# 先写临时文件再 mv：避免"边扫描目录边写该目录下的文件"（shellcheck SC2094），
# 也避免中途失败留下一个**看起来完整**的校验和文件。
(cd "$OUT" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >/tmp/.ha-sums.$$)
mv /tmp/.ha-sums.$$ "$OUT/SHA256SUMS"

TAR="$ROOT/build/package/${NAME}.tar.gz"
tar -czf "$TAR" -C "$(dirname "$OUT")" "$NAME"
(cd "$(dirname "$TAR")" && sha256sum "$(basename "$TAR")" >"${TAR}.sha256")

echo
echo "包: $TAR"
echo "大小: $(du -h "$TAR" | cut -f1)"
echo "校验: $(cat "${TAR}.sha256")"
echo "文件数: $(wc -l <"$OUT/SHA256SUMS")"

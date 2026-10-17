#!/usr/bin/env bash
set -euo pipefail

# 本脚本位于 deploy/packaging/，故仓库根在上两级。
#
# v0.7.2 的根目录清理把 package/build.sh 移到 deploy/packaging/build.sh
# （深度 1 → 2 层），但这行的 ".." 没跟着改成 "../.."，于是 PROJECT_ROOT
# 变成了 <repo>/deploy：产物落进 deploy/build/、GUI 去找 deploy/cmd/gui。
# 跨平台构建从那次起一直是坏的（Makefile 的单平台 build 不走这里，所以没暴露）。
PROJECT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUILD_DIR="${PROJECT_ROOT}/build"
# ★★ VERSION 必须是**语义化版本**（x.y.z），不能是 git describe 的原样输出。
#
# 2026-10-17 实测事故：当前分支没有 tag，`git describe` 回退成分支名
#   backup/pre-identity-rewrite-20261005-51-g03d87a4e-dirty
# 它被注入 meta.Version ⇒ 插件握手的主版本判据变成
#   MajorVersion("backup/pre-...") = 整个字符串（没有 '.' 可截）
# 而插件端是 "4" ⇒ **所有子进程插件握手失败**：
#   proc: xxx.handshake: 主版本不匹配（内核 backup/pre-...，插件 SDK 1.4.0）
# 内核自己是活的，但 40 个插件一个都起不来 —— 看起来像"插件全坏了"。
#
# 权威来源是 internal/meta/meta.go 的 Version 常量（发版时人工确认）。
# git describe 只在**形如 vX.Y.Z-...** 时才可用（那说明确实在 tag 之后）。
_meta_version() {
  local mv
  mv="$(sed -n 's/^[[:space:]]*Version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
        "$PROJECT_ROOT/internal/meta/meta.go" 2>/dev/null | head -1)"
  case "$mv" in
    [0-9]*.[0-9]*.[0-9]*) printf '%s' "$mv"; return 0 ;;
  esac
  return 1
}
if [ -z "${VERSION:-}" ]; then
  _gd="$(git -C "$PROJECT_ROOT" describe --tags --dirty 2>/dev/null || true)"
  case "$_gd" in
    v[0-9]*.[0-9]*.[0-9]*) VERSION="${_gd#v}" ;;
    *)
      # ★ 不是 tag 描述 ⇒ 拒绝用它。退回 meta.Version（当前分支的权威版本）。
      if VERSION="$(_meta_version)"; then
        [ -n "$_gd" ] && echo "[BUILD] warn: git describe 给出非版本串（$_gd），改用 meta.Version=$VERSION" >&2
      else
        echo "[BUILD] ERROR: 拿不到合法版本号（git describe=$_gd，meta.Version 也读不出）。" >&2
        echo "        请显式传 VERSION=x.y.z 后重试。" >&2
        exit 1
      fi
      ;;
  esac
fi
# 最终守卫：非 x.y.z 一律拒绝构建（否则会打出一个"插件全部握手失败"的包）
case "$VERSION" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "[BUILD] ERROR: VERSION='$VERSION' 不是语义化版本 x.y.z；" \
          "它会被当作插件协议主版本，导致所有子进程插件握手失败。" >&2
     exit 1 ;;
esac
COMMIT="${COMMIT:-$(git -C "$PROJECT_ROOT" rev-parse --short HEAD 2>/dev/null || echo "unknown")}"
BUILD_TIME="${BUILD_TIME:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}"
GO="${GO:-$(command -v go 2>/dev/null || echo "go")}"
# ★★ 模块路径必须是**当前**的 github.com/...，不能是历史的 gitcode.com/...
#
# `-X` 的包路径写错时 Go **不报错、静默忽略** ⇒ 注入的值保持源码默认
# （Commit/BuildTime 变成字面量 "unknown"，Version 变成源码里的版本号）。
# 构建日志全绿，只有 `binary --version` 才能看出来 —— 于是发行包带着
# 错的溯源信息发出去。
#
# 实测（2026-10-17）：用旧路径构建 waiter，`--version` 输出
#   `waiter 1.4.0 (commit unknown, built unknown)`
# 而正确路径得到注入的值。
LDFLAGS="-X github.com/JianFeeeee/HomeAgent/internal/meta.Version=${VERSION} -X github.com/JianFeeeee/HomeAgent/internal/meta.Commit=${COMMIT} -X github.com/JianFeeeee/HomeAgent/internal/meta.BuildTime=${BUILD_TIME}"

# 版本与提交的**权威来源**是上面注入的 meta.Version / meta.Commit，不是 Go 自带的
# VCS 戳。后者不进 build cache key（Go 文档明确说明 VCS 变化不会触发重建），
# 命中缓存时会把上一次的 revision 一并带回来——实测发布分支的产物上就出现了
# 1715b5c（本机任何仓库都不存在的提交），用 `go version -m` 溯源会指向幽灵提交。
# 统一 -buildvcs=false：宁可没有这个信号，也不要一个错的。
# 溯源请用：`strings homed | grep -m1 '^<短 hash>$'`（meta.Commit 是字符串常量）。

TARGET="${1:-native}"
COMPONENT="${2:-all}"

# ---- platform matrix ----
# homed:   linux/amd64 + linux/arm64 (CGO), macOS native-only (no osxcross),
#          windows/amd64 (MinGW)
# waiter:  all platforms (CGO-free, raw terminal mode is a no-op on non-Linux)
# gui:     electron-builder handles cross-platform natively

case "$TARGET" in
  native)   GOOS="" GOARCH="" ;;
  linux/amd64)  GOOS=linux   GOARCH=amd64  CC="${CC:-}" ;;
  # arm64 必须同时给 CXX：gojieba 是 C++，缺 CXX 时 cgo 用宿主 g++ 编出
  # x86-64 的 .o，链接时报 "Relocations in generic ELF (EM: 183)"（183 = aarch64）。
  #
  # 此处曾有一条注释写着「arm64 刻意不设 CXX」，理由是设了会报
  # "file format not recognized"。那个判断是错的：那个报错的真因是
  # cmd/{homed,waiter}/*.syso（x86-64 COFF Windows 资源对象）被链进了目标，
  # 与 CXX 无关。四组对照：
  #   syso 在   + 无 CXX → Relocations in generic ELF (EM: 183)
  #   syso 在   + 有 CXX → 000000.o: file format not recognized
  #   syso 隐藏 + 无 CXX → Relocations in generic ELF (EM: 183)
  #   syso 隐藏 + 有 CXX → 成功，ELF aarch64
  # 本脚本的 hide_syso_for_target 已处理前一个条件，这里补上后一个。
  linux/arm64)  GOOS=linux   GOARCH=arm64  CC="${CC:-aarch64-linux-gnu-gcc}" CXX="${CXX:-aarch64-linux-gnu-g++}" ;;
  darwin/amd64) GOOS=darwin  GOARCH=amd64  CC="${CC:-}" ;;
  darwin/arm64) GOOS=darwin  GOARCH=arm64  CC="${CC:-}" ;;
  # Windows 必须同时给 CXX：gojieba 是 C++，缺 CXX 时 cgo 回退到宿主 g++，
  # 而宿主 g++ 不认 mingw 的 -mthreads，报 unrecognized command-line option。
  windows/amd64) GOOS=windows GOARCH=amd64 CC="${CC:-x86_64-w64-mingw32-gcc}" CXX="${CXX:-x86_64-w64-mingw32-g++}" ;;
  all)
    "$0" linux/amd64   "$COMPONENT"
    "$0" linux/arm64   "$COMPONENT"
    "$0" darwin/amd64  "$COMPONENT"
    "$0" darwin/arm64  "$COMPONENT"
    "$0" windows/amd64 "$COMPONENT"
    exit 0
    ;;
  *)
    echo "Unknown target: $TARGET"
    echo "Usage: $0 [native|linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|all] [all|homed|waiter|initconfig|gui|payload]"
    echo "       [all|homed|waiter|initconfig|gui]"
    exit 1
esac

if [ -n "${GOOS:-}" ]; then
  SUFFIX="${GOOS}_${GOARCH}"
  export GOOS GOARCH
fi
if [ -n "${CC:-}" ]; then
  export CC
fi
if [ -n "${CXX:-}" ]; then
  export CXX
fi
export CGO_ENABLED="${CGO_ENABLED:-1}"

mkdir -p "$BUILD_DIR"

# ---- .syso 隔离 ----
#
# cmd/{homed,waiter}/*.syso 是 Windows 资源对象（COFF，含图标/版本信息）。
# Go 会把同目录的 .syso 无条件链进任何目标，于是交叉编译到非 Windows 平台时：
#   - linux/arm64、darwin/arm64 报 "unknown ARM64 relocation type 3"
#   - 其他架构报 "file format not recognized"
# package-linux.sh 有 hide_syso()，但直接调本脚本时没有那层保护——
# 这正是 arm64 产物长期缺失的原因（曾被误判为缺 g++ 交叉编译器）。
SYSO_HIDDEN=()
hide_syso_for_target() {
  [ "${GOOS:-}" = "windows" ] && return 0
  local f
  for f in "$PROJECT_ROOT"/cmd/homed/*.syso "$PROJECT_ROOT"/cmd/waiter/*.syso; do
    [ -f "$f" ] || continue
    mv "$f" "$f.hidden"
    SYSO_HIDDEN+=("$f")
  done
}
restore_syso_for_target() {
  local f
  for f in "${SYSO_HIDDEN[@]:-}"; do
    [ -n "$f" ] && [ -f "$f.hidden" ] && mv "$f.hidden" "$f"
  done
  SYSO_HIDDEN=()
}
trap restore_syso_for_target EXIT
hide_syso_for_target

# ---- homed (CGO, sqlite3) ----
build_homed() {
  local out="$BUILD_DIR/homed${SUFFIX:+_$SUFFIX}"
  local plat="${GOOS:-linux}/${GOARCH:-amd64}"

  if [ "$GOOS" = "darwin" ] && [ "${CC:-}" = "" ] && [ "$(uname)" != "Darwin" ]; then
    echo "[SKIP] homed ${plat} — requires native macOS build (CGO + sqlite3, no osxcross)"
    return
  fi
  if [ "$GOOS" = "windows" ]; then
    out="${out}.exe"
  fi
  echo "[BUILD] homed ${plat} → $out"
  # Go 用 CC 驱动 CGO 编译与链接，用 CC 指定的交叉工具链来决定目标架构。
  # 必须同时 export CC 给 Go 的 CGO 代码生成器，否则 CGO_ENABLED=1 下的
  # 目标文件与 host 的 ld 不兼容（如 arm64 的 .o 给了 x86_64 的 ld）。
  #
  # HOMED_TAGS 默认带 onnxruntime：发行版**默认启用**本地向量空间。
  # 不带这个标签时 providers/chineseclip 与 providers/qwen3vl 仍会注册，
  # 但打开时报「requires build tag」并优雅降级（不静默假装成功）。
  # 需要极简构建时可显式 HOMED_TAGS= 关掉。
  #
  # 运行期还需要 libonnxruntime.so（provider 按 /opt/onnxruntime、
  # /usr/local/lib、/usr/lib 顺序查找）；缺失时同样是「日志里的明确错误 +
  # 降级」，不会假装启用。
  local _cc="${CC:-cc}"
  local _tags="${HOMED_TAGS-onnxruntime}"
  local -a _tagargs=()
  if [ -n "$_tags" ]; then _tagargs=(-tags "$_tags"); fi

  # ★★ homed 必须与**它自己的**启动依赖图一起产出（2026-10-17 实测缺陷）。
  #
  # 内核启动时比对「磁盘图 digest」vs「二进制内嵌 digest」，不一致或读不到
  # ⇒ **拒绝启动**（退出码 1）。本脚本原先既不生成图也不注入 digest
  # ⇒ 用 `deploy/packaging/build.sh` 打出的 homed 装到机器上**起不来**：
  #   kernelboot: 生产构建（内嵌 hash 非空）但读不到依赖图 …
  #
  # 注意这与 Makefile 是**两条独立构建路径**：只修 Makefile 不够。
  # 图必须由**同一次构建**产出（digest 一一对应），不能复用别的版本的图。
  local _graphgen="$BUILD_DIR/homed-depgraph${SUFFIX:+_$SUFFIX}"
  echo "  [graph] 生成启动依赖图（编译期定序）..."
  CGO_ENABLED=1 CC="$_cc" "$GO" build ${_tagargs[@]+"${_tagargs[@]}"} \
    -o "$_graphgen" ./cmd/homed-depgraph/
  local _graph_hash
  _graph_hash="$("$_graphgen" -digest)"
  "$_graphgen" -out "$BUILD_DIR/depgraph.bin"
  echo "  [graph] digest=$_graph_hash"

  CGO_ENABLED=1 CC="$_cc" "$GO" build -buildvcs=false -trimpath -installsuffix dynlink \
    ${_tagargs[@]+"${_tagargs[@]}"} \
    -ldflags "$LDFLAGS -X github.com/JianFeeeee/HomeAgent/internal/kernelboot.EmbeddedGraphHash=$_graph_hash" -o "$out" ./cmd/homed/
  echo "  OK ($(file "$out" | sed 's/.*: //') | $(du -h "$out" | cut -f1))"
}

# ---- waiter (cross-platform, CGO-free) ----
build_waiter() {
  local plat="${GOOS:-linux}/${GOARCH:-amd64}"
  local out="$BUILD_DIR/waiter${SUFFIX:+_$SUFFIX}"
  if [ "$GOOS" = "windows" ]; then out="${out}.exe"; fi

  echo "[BUILD] waiter ${plat} → $out"
  CGO_ENABLED=0 "$GO" build -buildvcs=false -trimpath -installsuffix dynlink \
    -ldflags "$LDFLAGS" -o "$out" ./cmd/waiter/
  echo "  OK ($(du -h "$out" | cut -f1))"
}

# ---- initconfig（必须 cgo：写 config.db 用的是 go-sqlite3）----
#
# 这里**必须** CGO_ENABLED=1。此前写的是 CGO_ENABLED=0，而 cmd/initconfig 通过
# database/sql 使用 mattn/go-sqlite3：CGO_ENABLED=0 时该库退化成 static_mock.go
# 里的桩，sql.Open 是懒的所以不报错、第一次 Exec 才失败；而 main.go 当时忽略
# 了所有错误——于是 initconfig 打印凭据、退出码 0、一个字节都没写进 config.db。
# 安装脚本把这份凭据写进 credentials.txt，用户照它登录必然失败，全程无报错。
#
# NSIS 安装包（installer.nsi）与 package-linux.sh 的 stage_variant 都引用它，
# 但此前 build.sh 从不构建它——Windows 安装包构建会直接失败在缺文件上。
build_initconfig() {
  local plat="${GOOS:-linux}/${GOARCH:-amd64}"
  local out="$BUILD_DIR/initconfig${SUFFIX:+_$SUFFIX}"
  if [ "$GOOS" = "windows" ]; then out="${out}.exe"; fi

  echo "[BUILD] initconfig ${plat} → $out"
  CGO_ENABLED=1 "$GO" build -buildvcs=false -trimpath -installsuffix dynlink \
    -ldflags "$LDFLAGS" -o "$out" ./cmd/initconfig/
  echo "  OK ($(du -h "$out" | cut -f1))"
}

# ---- linux-payload（给 Windows 安装器用的 Linux 包）----
#
# Windows 不再安装 homed.exe：homed 依赖 fd 继承 + 统一共享内存区的段内偏移
# 解引用，Windows 句柄模型无法表达（见 cmd/homed/platform_windows.go）。
# Windows 安装器改为引导到 WSL2，并把 **Linux 包**送进发行版里安装。
# 因此 Windows 安装包必须带上 Linux 产物——这一段就是把它暂存到
# build/linux-payload/（installer.nsi 从这里 File /r 打进安装包）。
#
# 复用 package-linux.sh 的产物，而不是在这里另行编译：WSL 里跑的就是普通
# linux/amd64，安装内容必须与 Linux 原生安装**完全一致**，否则又变成两个平台。
stage_linux_payload() {
  local src="$PROJECT_ROOT/dist/linux"
  local out="$BUILD_DIR/linux-payload"

  rm -rf "$out"
  mkdir -p "$out"

  local found=0
  for f in "$src"/*.deb "$src"/*.tar.gz; do
    [ -f "$f" ] || continue
    cp "$f" "$out/"
    found=$((found + 1))
  done

  if [ "$found" -eq 0 ]; then
    echo "[FAIL] build/linux-payload 为空：先运行 package-linux.sh 产出 dist/linux/*.deb|*.tar.gz" >&2
    echo "       （Windows 安装器会把这里的包送进 WSL 安装；空包等于装不上）" >&2
    return 1
  fi
  echo "[BUILD] linux-payload ← $found 个包"
  ls -1 "$out" | sed 's/^/  /'
}

# ---- gui (Electron) ----
#
# 输出目录必须用 --config.directories.output，**不能用 -o**：
# electron-builder 的 `-o` 是 `--mac`/`--macos` 的短别名（见 --help 的 Building 段），
# 不是 output。此前 `-o "$BUILD_DIR"` 被当成 macOS 的 target 列表，报
#   ⨯ Unknown target: ${HOME_DIR}/trueagent/build
# （路径被 lowercase 后去匹配 target 名表，所以错误信息里的路径是全小写的，
#  这也是它看起来像「路径错」而实际是「参数位置错」的原因）。
# v1.0.1 与 v1.0.3 两次发布都因此手工组装过 GUI。
build_gui() {
  if [ -n "${GOOS:-}" ] && [ "$GOOS" != "$("$GO" env GOOS)" ]; then
    echo "[SKIP] gui ${GOOS}/${GOARCH} — electron-builder handles cross-platform natively; run 'all' on CI host"
    return
  fi

  local gui_dir="$PROJECT_ROOT/cmd/gui"
  echo "[BUILD] gui → $BUILD_DIR/"

  if [ ! -d "$gui_dir/node_modules" ]; then
    echo "  npm install..."
    (cd "$gui_dir" && npm install --production)
  fi

  # 不传 --config：electron-builder 默认从 package.json 的 "build" 键读配置。
  # 传 --config package.json 会让它把**整个** package.json 当配置校验，
  # 于是 devDependencies / build / scripts 全被判为 "unknown property" 而失败。
  #
  # GUI 失败不中断整体构建：homed/waiter/initconfig 是发布的主体，
  # 而 GUI 依赖 electron 运行时下载（离线机器、arm64 缺缓存都会失败）。
  # set -e 下若不接住，一个可选组件会让整轮跨平台构建全废。
  if (cd "$gui_dir" && npx electron-builder \
      --linux --win --mac \
      --x64 --arm64 \
      -p never \
      --config.directories.output="$BUILD_DIR"); then
    echo "  OK"
  else
    echo "  WARN: gui 构建失败（可选组件，不影响 homed/waiter/initconfig）"
    echo "        Linux 包可用 deploy/packaging/package-linux.sh 内置的手工组装路径"
    return 0
  fi
}

# ---- dispatch ----
if [ "${GOOS:-}" = "windows" ]; then
  # Windows 目标：构建的**不是** homed——它已放弃 Windows 原生支持。
  # 需要的是：Linux 包（送进 WSL 安装）+ Windows 侧客户端（waiter CLI / GUI）。
  case "$COMPONENT" in
    all)   build_waiter; stage_linux_payload; build_gui ;;
    waiter) build_waiter ;;
    payload) stage_linux_payload ;;
    gui)   build_gui ;;
    homed|initconfig)
      echo "homed/initconfig 不再提供 Windows 原生构建：请用 WSL2（或用 linux/amd64 目标）。" >&2
      echo "原因见 cmd/homed/platform_windows.go。" >&2
      exit 1
      ;;
    *)
      echo "Unknown component: $COMPONENT"
      exit 1
      ;;
  esac
else
  case "$COMPONENT" in
    all)   build_homed; build_waiter; build_initconfig; build_gui ;;
    homed) build_homed ;;
    waiter) build_waiter ;;
    initconfig) build_initconfig ;;
    gui)   build_gui ;;
    *)
      echo "Unknown component: $COMPONENT"
      exit 1
      ;;
  esac
fi

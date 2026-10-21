#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUILD_DIR="${PROJECT_ROOT}/build"
DIST_DIR="${PROJECT_ROOT}/dist/linux"
# ★★ VERSION 解析必须与 build.sh 用**同一套规则**（2026-10-17 统一）。
#
# 原实现只看 git describe，而无 tag 分支上它返回**分支名**：
#   backup/pre-identity-rewrite-20261005-56-g98850e1
# 下面的守卫会拒绝它（正确），但要求人**手动传 VERSION** 才能打包 ——
# 而 build.sh 早已能自动回退到 meta.Version。两个脚本行为不一致的结果是：
# 同一个版本，走 build.sh 打得出来，走 package-linux.sh 打不出来。
#
# 更要紧的是：若有人为了"让它跑通"去掉守卫，分支名就会进 meta.Version，
# 而那是**插件协议主版本**的来源 ⇒ 22 个子进程插件全部握手失败（真事故）。
#
# ⇒ 统一为：git describe 只在形如 vX.Y.Z 时采用，否则回退 meta.Version。
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
      if VERSION="$(_meta_version)"; then
        [ -n "$_gd" ] && echo "[PKG] warn: git describe 给出非版本串（$_gd），改用 meta.Version=$VERSION" >&2
      else
        echo "[PKG] ERROR: 拿不到合法版本号（git describe=$_gd）。请显式传 VERSION=x.y.z。" >&2
        exit 1
      fi
      ;;
  esac
fi

# git describe 给出的是 v1.0.0-68-gba0b5a1-dirty 这类描述串，它不是合法的包版本：
# deb 的 Version 必须以数字开头，rpm 的 Version 不允许 '-'（那是版本/发布的分隔符）。
# 以前只有显式传 VERSION=1.0.3 才打得出来，默认路径一跑就死在 dpkg-deb 上——
# 而且死在 stage 之后，前面每条日志都是真的，只有最后一个产物没生成。
PKG_VERSION="${VERSION#v}"
case "$PKG_VERSION" in
  [0-9]*) ;;
  *) echo "ERROR: 包版本必须以数字开头（得到 '$VERSION'）。请显式设置 VERSION=x.y.z 后重试。" >&2; exit 1 ;;
esac
PKG_VERSION="$(printf '%s' "$PKG_VERSION" | sed -e 's/-/+/g')"
PACKAGE_ROOT="${PROJECT_ROOT}/deploy/packaging/linux"
GO="${GO:-$(command -v go 2>/dev/null || echo "go")}"

ARCH="${1:-amd64}"   # amd64 or arm64

# server/full 发行包默认带 Chinese-CLIP ONNX 产物与 ONNX Runtime。
# 二进制大资产不进 git：发布环境通过这两个目录提供已验证的产物；若缺失，
# server/full 打包必须明确失败，不能生成一个「默认启用但装完不能用」的假包。
CHINESECLIP_BUNDLE_DIR="${CHINESECLIP_BUNDLE_DIR:-$BUILD_DIR/model-assets/chinese-clip-vit-b16-onnx}"
ONNXRUNTIME_ASSET_DIR="${ONNXRUNTIME_ASSET_DIR:-$BUILD_DIR/runtime-assets/$ARCH}"
ONNXRUNTIME_LIB="${ONNXRUNTIME_LIB:-$ONNXRUNTIME_ASSET_DIR/libonnxruntime.so}"
ONNXRUNTIME_LICENSE="${ONNXRUNTIME_LICENSE:-$ONNXRUNTIME_ASSET_DIR/LICENSE}"
ONNXRUNTIME_NOTICES="${ONNXRUNTIME_NOTICES:-$ONNXRUNTIME_ASSET_DIR/ThirdPartyNotices.txt}"

# 打包 staging 会把 719MB 模型真的复制一份，临时目录必须落在构建目录所在的磁盘，
# 不能落在系统临时目录：本机 /tmp 是 9.8GB tmpfs，一次 full 包 staging 就能写满，
# 而且失败发生在 cp 进行到一半，报出来是 "No space left on device"——看上去像
# 资产／版本有问题，实际只是临时目录选错了文件系统。
STAGE_TMP="${BUILD_DIR}/.stage-tmp"

# electron 官方发布物用 x64/arm64 命名，而 Debian 用 amd64/arm64。
# 两者在 arm64 上恰好同名，amd64 上不同——此前缓存查找统一用 TAR_ARCH
# （amd64），于是 electron-v*-linux-x64.zip 永远命中不到，amd64 GUI 只能
# 靠"回退到 host node_modules"这条路组装。干净 worktree 里没有完整
# node_modules，GUI 就被静默跳过。故单独映射。
ACTION="${2:-all}"    # all, build, deb, tar, rpm

DEB_ARCH="$ARCH"
RPM_ARCH="$ARCH"
TAR_ARCH="$ARCH"
case "$ARCH" in
  amd64) DEB_ARCH="amd64"; RPM_ARCH="x86_64"; TAR_ARCH="amd64"; ELECTRON_ARCH="x64" ;;
  arm64) DEB_ARCH="arm64"; RPM_ARCH="aarch64"; TAR_ARCH="arm64"; ELECTRON_ARCH="arm64" ;;
  *) echo "Unknown arch: $ARCH (use amd64 or arm64)"; exit 1 ;;
esac

echo "=== HologramAgent Linux Packager ==="
echo "Version: $VERSION"
[ "$PKG_VERSION" = "$VERSION" ] || echo "Package: $PKG_VERSION (normalized for deb/rpm)"
echo "Arch:    $ARCH"
echo ""

# ---- prepare go.mod for Linux build: point SDK replace at the in-repo SDK ----
# SDK 仓库（含工具链/示例）以嵌套 git 仓库形式维护在 third_party/homeagent-sdk，
# 打包直接用该目录，不再 clone 到 /tmp。
prepare_gomod() {
  local gomod="$PROJECT_ROOT/go.mod"
  local sdk_local="$PROJECT_ROOT/third_party/homeagent-sdk"
  local sdk_clone="/tmp/homeagent-sdk"
  local patched=0

  if grep -q 'replace github.com/JianFeeeee/homeagentsdk' "$gomod"; then
    echo ">>> Updating go.mod: replacing Windows SDK path with in-repo SDK..."
    if [ -d "$sdk_local" ]; then
      sed -i.bak "s|^replace github.com/JianFeeeee/homeagentsdk => .*|replace github.com/JianFeeeee/homeagentsdk => ${sdk_local}|" "$gomod"
      patched=1
    elif [ ! -d "$sdk_clone" ]; then
      echo ">>> Cloning SDK to $sdk_clone (in-repo SDK missing)..."
      # 仓名用当前名 hologramagentsdk（旧名 homeagentsdk 仍 301 跳转，但别再用）。
      # 注意：下面几处 replace 的 module 路径**仍是** homeagentsdk —— 已发布
      # tag 的 go.mod 里 module 就是这个，改名后没变。
      git clone git@github.com:JianFeeeee/hologramagentsdk.git "$sdk_clone" 2>/dev/null || \
      git clone https://github.com/JianFeeeee/hologramagentsdk.git "$sdk_clone" 2>/dev/null || true
      if [ -d "$sdk_clone" ]; then
        sed -i.bak "s|^replace github.com/JianFeeeee/homeagentsdk => .*|replace github.com/JianFeeeee/homeagentsdk => ${sdk_clone}|" "$gomod"
        patched=1
      else
        echo "WARNING: Cannot clone SDK. Build may fail."
      fi
    else
      sed -i.bak "s|^replace github.com/JianFeeeee/homeagentsdk => .*|replace github.com/JianFeeeee/homeagentsdk => ${sdk_clone}|" "$gomod"
      patched=1
    fi
  fi
  return $patched
}

restore_gomod() {
  if [ -f "$PROJECT_ROOT/go.mod.bak" ]; then
    echo ">>> Restoring original go.mod..."
    mv "$PROJECT_ROOT/go.mod.bak" "$PROJECT_ROOT/go.mod"
  fi
}

# ---- temporarily move .syso files (Windows COFF objects) for Linux builds ----
hide_syso() {
  echo ">>> Hiding .syso files (Windows-only object files)..."
  for dir in homed waiter; do
    if [ -f "$PROJECT_ROOT/cmd/$dir/$dir.syso" ]; then
      mv "$PROJECT_ROOT/cmd/$dir/$dir.syso" "$PROJECT_ROOT/cmd/$dir/$dir.syso.bak"
    fi
  done
}

restore_syso() {
  for dir in homed waiter; do
    if [ -f "$PROJECT_ROOT/cmd/$dir/$dir.syso.bak" ]; then
      mv "$PROJECT_ROOT/cmd/$dir/$dir.syso.bak" "$PROJECT_ROOT/cmd/$dir/$dir.syso"
    fi
  done
}

# ensure both are always restored on exit
restore_all() { restore_gomod; restore_syso; rmdir "$STAGE_TMP" 2>/dev/null || true; }
trap restore_all EXIT

# ---- build Go binaries via existing build.sh ----
build_go() {
  echo ">>> Building Go binaries for linux/$ARCH..."

  prepare_gomod || true
  hide_syso

  local suffix="linux_${ARCH}"
  local homed_bin="$BUILD_DIR/homed_$suffix"
  local waiter_bin="$BUILD_DIR/waiter_$suffix"
  local initconfig_bin="$BUILD_DIR/initconfig_$suffix"

  # 先删旧产物：否则本次构建失败后，残留文件会让「产物存在」判据假绿。
  rm -f "$homed_bin" "$waiter_bin" "$initconfig_bin"

  bash "$PROJECT_ROOT/deploy/packaging/build.sh" "linux/$ARCH" "homed"
  test -x "$homed_bin"
  if ! go version -m "$homed_bin" | grep -Eq 'build[[:space:]]+-tags=.*onnxruntime'; then
    echo "ERROR: homed 不是 onnxruntime 构建，拒绝打 server/full 包：$homed_bin" >&2
    return 1
  fi

  bash "$PROJECT_ROOT/deploy/packaging/build.sh" "linux/$ARCH" "waiter"
  test -x "$waiter_bin"
  bash "$PROJECT_ROOT/deploy/packaging/build.sh" "linux/$ARCH" "initconfig"
  test -x "$initconfig_bin"

  echo "  homed:      $homed_bin ($(du -h "$homed_bin" | cut -f1), onnxruntime)"
  echo "  waiter:     $waiter_bin ($(du -h "$waiter_bin" | cut -f1))"
  echo "  initconfig: $initconfig_bin ($(du -h "$initconfig_bin" | cut -f1))"
  echo ""
}

# ---- build GUI (manual directory assembly, avoids electron-packager network issues) ----
#
# electron 运行时必须按**目标架构**取，不能用 host 的
# node_modules/electron/dist——那里永远是 host 架构（本机 x64）。
# v1.0.0 / v1.0.1 的 arm64 full/client 包都踩了这个坑：目录名带
# -arm64、homed/waiter 确实是 aarch64，但里面的 electron 是 x86-64，
# 在 arm64 机器上一启动就是 Exec format error（从未被交叉验证过）。
#
# 现在改为优先从 electron 缓存里取对应架构的 zip，并在最后做
# 一道强制校验：架构不符就删掉目录并跳过 GUI，宁可不发也不发坏包。
build_gui() {
  local gui_dir="$PROJECT_ROOT/cmd/gui"
  local gui_out="$BUILD_DIR/hologram-gui-linux-${TAR_ARCH}"

  if [ -d "$gui_out" ]; then
    echo ">>> GUI already built at $gui_out (delete to rebuild)"
    return
  fi

  echo ">>> Building GUI directory for linux/$ARCH..."

  # 判据是 electron 包本身在不在，而不是 node_modules 目录在不在。
  #
  # npm install 失败（离线、网络受限）会留下一个只有一两个条目的空壳
  # node_modules，目录存在但 electron 缺失。只看目录会以为"已安装"，
  # 于是 ever 读不到版本、缓存匹配退化、最后走到"host dist 也没有"而
  # 静默跳过 GUI——包名和目录名全都正确，只是没有 GUI，没有任何一步报错。
  if [ ! -f "$gui_dir/node_modules/electron/package.json" ]; then
    if [ -d "$gui_dir/node_modules" ]; then
      echo "  node_modules 存在但 electron 缺失（疑似上次 npm install 未完成）"
    fi
    echo "  npm install..."
    if ! (cd "$gui_dir" && npm install --production); then
      echo "  WARNING: npm install 失败——离线环境下这是预期的。"
      echo "           GUI 需要 cmd/gui/node_modules/electron 或 ~/.cache/electron 缓存。"
    fi
  fi

  # electron 版本优先从已安装的包里读，保证运行时与 app 依赖一致。
  # 读不到时退而从 package.json 的依赖声明里取数字部分（它可能写成
  # "^33.0.0" 这类范围，只用于给缓存匹配一个提示，匹配不上仍会走通配）。
  local ever
  ever=$(python3 -c "import json;print(json.load(open('$gui_dir/node_modules/electron/package.json'))['version'])" 2>/dev/null || true)
  if [ -z "$ever" ]; then
    ever=$(python3 -c "
import json, re
d = json.load(open('$gui_dir/package.json'))
spec = (d.get('devDependencies', {}) or {}).get('electron') or (d.get('dependencies', {}) or {}).get('electron') or ''
m = re.search(r'(\\d+(?:\\.\\d+)*)', spec)
print(m.group(1) if m else '')
" 2>/dev/null || true)
    if [ -n "$ever" ]; then
      echo "  electron 版本取自 package.json 依赖声明: $ever（非精确）"
    fi
  fi

  mkdir -p "$gui_out"

  # 优先：缓存里的目标架构 zip（~/.cache/electron/<hash>/electron-v<ver>-linux-<arch>.zip）
  #
  # ★ 这里必须 `|| true`：`find` 对**不存在的目录**返回退出码 1，而本脚本是
  #   `set -euo pipefail`，命令替换里的失败会让整个脚本当场退出。
  #   后果：任何**没有 ~/.cache/electron 的机器**（全新克隆、CI runner、
  #   其他开发机）跑到这里就死，且只留下一行「electron 版本取自 package.json」
  #   作为最后的输出，看不出真因。实测（2026-09-29，GitHub runner 与本地
  #   移走缓存后均复现）：build_go 全部成功，然后卡在这里静默退出。
  #   本机历史上之所以一直「能打包」，只是因为碰巧有那份缓存。
  local zip=""
  if [ -n "$ever" ]; then
    zip=$(find "$HOME/.cache/electron" -name "electron-v${ever}-linux-${ELECTRON_ARCH}.zip" 2>/dev/null | head -1 || true)
  fi
  if [ -z "$zip" ]; then
    zip=$(find "$HOME/.cache/electron" -name "electron-v*-linux-${ELECTRON_ARCH}.zip" 2>/dev/null | head -1 || true)
  fi

  if [ -n "$zip" ]; then
    echo "  electron runtime: $(basename "$zip")"
    unzip -q -o "$zip" -d "$gui_out"
  else
    # 回退：仅当目标架构 == host 架构时才能用 host 的 dist
    local host_arch
    case "$(uname -m)" in
      x86_64) host_arch=amd64 ;;
      aarch64|arm64) host_arch=arm64 ;;
      *) host_arch=unknown ;;
    esac
    if [ "$TAR_ARCH" != "$host_arch" ]; then
      echo "  WARNING: 缺 electron-v*-linux-${ELECTRON_ARCH}.zip 缓存，且目标架构与 host"
      echo "           ($host_arch) 不同——不能用 host 的 electron 冒充。跳过 GUI。"
      echo "           解法：下载 electron-v${ever:-<ver>}-linux-${ELECTRON_ARCH}.zip 到"
      echo "           ~/.cache/electron/<任意子目录>/ 后重跑。"
      rm -rf "$gui_out"
      return
    fi
    local electron_dir="$gui_dir/node_modules/electron/dist"
    if [ ! -f "$electron_dir/electron" ]; then
      echo "  WARNING: electron binary not found at $electron_dir. GUI will be skipped."
      rm -rf "$gui_out"
      return
    fi
    echo "  electron runtime: host node_modules (同架构 $host_arch)"
    cp -r "$electron_dir"/* "$gui_out/" 2>/dev/null
  fi

  mkdir -p "$gui_out/resources/app/node_modules"
  mkdir -p "$gui_out/resources/app/renderer"
  rm -f "$gui_out/resources/default_app.asar" 2>/dev/null

  # copy app source
  #
  # ★ 改成**按目录整体同步**，不再逐文件手列。
  #
  # 原实现手工 cp 七个文件，每加一个新文件就要记得补一行，漏了不会有
  # 任何报错，只会做出一个"装完缺东西"的包。已实际漏掉过：
  #   · renderer/mascot.svg      —— 源码里根本不存在（真名 mascot.webp）
  #   · icon.svg / icon.ico / icon-tray*.png —— 托盘与窗口图标
  #   · renderer/icon.svg
  #   · renderer/vendor/*         —— three/OrbitControls/marked/purify
  #     （这四个是本地化后的第三方库，缺了 Linux 发行版的星图直接坏）
  #
  # 整体同步还顺带保证 .hmap 之外的任何新增资源都会进包。
  cp "$gui_dir/main.js" "$gui_out/resources/app/"
  cp "$gui_dir/preload.js" "$gui_out/resources/app/"
  cp "$gui_dir/package.json" "$gui_out/resources/app/"
  cp "$gui_dir"/*.svg "$gui_out/resources/app/" 2>/dev/null || true
  cp "$gui_dir"/*.ico "$gui_out/resources/app/" 2>/dev/null || true
  cp "$gui_dir"/icon-tray*.png "$gui_out/resources/app/" 2>/dev/null || true

  # renderer 整目录同步（含 vendor/ 与全部静态资源）
  cp -r "$gui_dir/renderer/." "$gui_out/resources/app/renderer/"

  # ★ 必备文件校验：缺了就是"装完坏掉"的包，且不会有任何构建报错。
  # renderer/vendor 是本地化后的第三方库（three/OrbitControls/marked/purify），
  # 缺任何一份都会让星图或 Markdown 渲染在断网/离线环境下直接坏掉。
  local missing=""
  for _f in index.html app.js style.css vendor/three.min.js vendor/OrbitControls.js \
            vendor/marked.min.js vendor/purify.min.js; do
    [ -f "$gui_out/resources/app/renderer/$_f" ] || missing="$_f "
  done
  [ -f "$gui_out/resources/app/icon.svg" ] || missing="icon.svg "
  if [ -n "$missing" ]; then
    echo "  ERROR: GUI 资源缺失：$missing"
    echo "         宁可不发，也不发装了跑不起来的包。"
    rm -rf "$gui_out"
    return
  fi

  # production node_modules for app
  if [ -d "$gui_dir/node_modules" ]; then
    for mod in icojs; do
      if [ -d "$gui_dir/node_modules/$mod" ]; then
        cp -r "$gui_dir/node_modules/$mod" "$gui_out/resources/app/node_modules/"
      fi
    done
  fi

  # create desktop entry and symlink
  cat > "$gui_out/resources/app/hologram-gui.desktop" << DESKTOP
[Desktop Entry]
Name=HologramAgent
Comment=HologramAgent Desktop GUI
Exec=$gui_out/hologram-gui
Terminal=false
Type=Application
Categories=Utility;
Icon=$gui_out/resources/app/icon.svg
DESKTOP

  # create launcher script
  cat > "$gui_out/hologram-gui" << 'LAUNCHER'
#!/bin/sh
DIR="$(cd "$(dirname "$0")" && pwd)"
exec "$DIR/electron" "$DIR/resources/app" "$@"
LAUNCHER
  chmod +x "$gui_out/hologram-gui"
  chmod +x "$gui_out/electron"

  # 最后一道强制校验：electron 二进制的实际架构必须匹配目标架构。
  # 不做这步就会重现 v1.0.0/v1.0.1 的隐形坏包：包名、目录名、
  # homed/waiter 全对，只有 electron 是错架构，直到用户在 arm64 机器上
  # 双击才发现 Exec format error。
  local want_pat
  case "$TAR_ARCH" in
    amd64) want_pat="x86-64" ;;
    arm64) want_pat="aarch64" ;;
    *)     want_pat="" ;;
  esac
  if [ -n "$want_pat" ]; then
    local got
    got=$(file -b "$gui_out/electron" 2>/dev/null || echo "")
    if ! printf '%s' "$got" | grep -q "$want_pat"; then
      echo "  ERROR: electron 架构不符——期望 $want_pat，实际: ${got%%,*}"
      echo "         删除 GUI 目录并跳过（宁可不发，也不发装了跑不起来的包）。"
      rm -rf "$gui_out"
      return
    fi
  fi

  echo "  GUI built: $gui_out ($(du -sh "$gui_out" | cut -f1), $(file -b "$gui_out/electron" | cut -d, -f2 | tr -d ' '))"
  echo ""
}

# ---- stage files for a variant ----
stage_variant() {
  local variant="$1"
  local staging="$2"
  local suffix="linux_${ARCH}"

  echo ">>> Staging $variant..."

  mkdir -p "$staging/usr/bin"
  mkdir -p "$staging/etc/systemd/system"
  mkdir -p "$staging/var/lib/hologram"

  local initconfig_bin="$BUILD_DIR/initconfig_$suffix"

  # ★★ 启动依赖图必须与 homed **同目录**（2026-10-17 实测缺陷）。
  #
  # 内核启动时会比对
  #   「磁盘图的 digest」 vs 「二进制内嵌的 digest」
  # 读不到图 ⇒ 生产构建（内嵌 hash 非空）**直接拒绝启动**：
  #   kernelboot: 生产构建（内嵌 hash 非空）但读不到依赖图
  #               <dir>/depgraph.bin: no such file or directory
  #   ⇒ 退出码 1，内核完全起不来。
  #
  # 原先 stage_variant 只拷 homed/waiter ⇒ **每个 deb/tar 包里的 homed
  # 都起不来**。实测确认：只放 homed 不放图，启动即失败退出 1。
  #
  # 图由 `make build` 产出到 build/depgraph.bin（与 homed 同一次构建，
  # digest 一一对应；手工复制别的版本的图会 digest 不符，同样拒绝启动）。
  stage_depgraph() {
    local staging="$1"
    if [ -f "$BUILD_DIR/depgraph.bin" ]; then
      cp "$BUILD_DIR/depgraph.bin" "$staging/usr/bin/depgraph.bin"
      chmod 644 "$staging/usr/bin/depgraph.bin"
    else
      echo "ERROR: 缺少 $BUILD_DIR/depgraph.bin —— 内核与依赖图必须来自同一次构建。" >&2
      echo "       先跑 make build（它会生成图并注入 digest），再打包。" >&2
      return 1
    fi
  }

  # stage_hmapdev：插件工具链（init/build/package .hmap）
  #
  # 放在 /usr/bin 而不是 /usr/lib：它是使用者直接调用的命令，
  # 与 homed/waiter 同级更符合直觉（hmapdev 自身按 PATH 找 SDK store）。
  stage_hmapdev() {
    local staging="$1"
    local hv
    hv="$(ls "$BUILD_DIR"/hmapdev* 2>/dev/null | grep -vE '\.(exe)$' | head -1)"
    if [ -z "$hv" ]; then
      # 退而求其次：从 SDK 源码现编一个（CGO 无关，快）
      echo "  [hmapdev] build/ 下没有 hmapdev，尝试现编..."
      ( cd "$PROJECT_ROOT/third_party/homeagent-sdk/tools/hmapdev" && \
        CGO_ENABLED=0 go build -trimpath -o "$BUILD_DIR/hmapdev" . ) || {
        echo "ERROR: 无法获得 hmapdev（插件工具链），拒绝打包。" >&2
        return 1; }
      hv="$BUILD_DIR/hmapdev"
    fi
    install -m 0755 "$hv" "$staging/usr/bin/hmapdev"
    echo "  hmapdev:    $hv ($(du -h "$hv" | cut -f1))"
  }

  # stage_sdk：与本次工具链同源的 SDK 源码（含 sdk/ tools/ skills/ meta/）。
  #
  # 为什么要随包发：hmapdev 需要按插件声明的 sdk 版本解析依赖；没有本地 SDK
  # store 时它会去网上拉（内网/受限出口环境直接失败）。随包带一份 →
  # 「装完即可离线构建插件」，且版本与内核严格同源（防版本漂移）。
  stage_sdk() {
    local staging="$1"
    local sdk_src="$PROJECT_ROOT/third_party/homeagent-sdk"
    local sdk_dst="$staging/usr/lib/hologram/sdk"
    [ -d "$sdk_src" ] || { echo "ERROR: 找不到 SDK 源码 $sdk_src" >&2; return 1; }
    mkdir -p "$sdk_dst"
    for item in go.mod go.sum LICENSE README.md sdk meta tools skills; do
      [ -e "$sdk_src/$item" ] && cp -a "$sdk_src/$item" "$sdk_dst/"
    done
    # 不带陈旧工具链产物：那是上次构建留下的，可能与本次源码不符。
    rm -rf "$sdk_dst/tools/vscode-hmapdev/node_modules" 2>/dev/null || true
    local sv
    sv="$(grep -oE 'Version = "[0-9.]+"' "$sdk_src/meta/meta.go" 2>/dev/null | head -1 | grep -oE '[0-9.]+')"
    echo "  sdk:        $sdk_dst (v${sv:-?}, $(du -sh "$sdk_dst" | cut -f1))"
  }

  case "$variant" in
    full)
      cp "$BUILD_DIR/homed_$suffix" "$staging/usr/bin/homed"
      cp "$BUILD_DIR/waiter_$suffix" "$staging/usr/bin/waiter"
      stage_depgraph "$staging"
      cp "$PROJECT_ROOT/deploy/hologramagent.service" "$staging/etc/systemd/system/hologramagent.service"
      [ -f "$initconfig_bin" ] && cp "$initconfig_bin" "$staging/usr/bin/initconfig"
      stage_setup "$staging"
      stage_license "$staging"
      stage_multimodal_assets "$staging"
      stage_gui "$staging"
      ;;
    server)
      cp "$BUILD_DIR/homed_$suffix" "$staging/usr/bin/homed"
      cp "$BUILD_DIR/waiter_$suffix" "$staging/usr/bin/waiter"
      stage_depgraph "$staging"
      cp "$PROJECT_ROOT/deploy/hologramagent.service" "$staging/etc/systemd/system/hologramagent.service"
      [ -f "$initconfig_bin" ] && cp "$initconfig_bin" "$staging/usr/bin/initconfig"
      stage_setup "$staging"
      stage_license "$staging"
      stage_multimodal_assets "$staging"
      ;;
    toolchain)
      # 内核 + CLI + 依赖图 + 插件工具链 + SDK（开发者/自建一体化）
      cp "$BUILD_DIR/homed_$suffix" "$staging/usr/bin/homed"
      cp "$BUILD_DIR/waiter_$suffix" "$staging/usr/bin/waiter"
      stage_depgraph "$staging"
      stage_hmapdev "$staging"
      stage_sdk "$staging"
      cp "$PROJECT_ROOT/deploy/hologramagent.service" "$staging/etc/systemd/system/hologramagent.service"
      [ -f "$initconfig_bin" ] && cp "$initconfig_bin" "$staging/usr/bin/initconfig"
      stage_setup "$staging"
      stage_license "$staging"
      # ★★ onnx 运行库 + 模型必须进包（用户明确要求，2026-10-17）。
      # 否则装到干净机器上「默认启用但用不了」——本地向量空间会静默降级。
      stage_multimodal_assets "$staging"
      ;;
    client)
      cp "$BUILD_DIR/waiter_$suffix" "$staging/usr/bin/waiter"
      stage_license "$staging"
      stage_gui "$staging"
      ;;
  esac

  chmod 755 "$staging/usr/bin/"* 2>/dev/null || true
  echo "  OK"
}

stage_gui() {
  local staging="$1"
  local gui_src="$BUILD_DIR/hologram-gui-linux-${TAR_ARCH}"
  if [ -d "$gui_src" ]; then
    mkdir -p "$staging/usr/lib/hologram-gui"
    cp -r "$gui_src"/* "$staging/usr/lib/hologram-gui/"
    cat > "$staging/usr/bin/hologram-gui" << 'SCRIPT'
#!/bin/sh
exec /usr/lib/hologram-gui/hologram-gui "$@"
SCRIPT
    chmod 755 "$staging/usr/bin/hologram-gui"
  else
    echo "  WARNING: GUI not built, skipping GUI staging"
  fi
}

stage_setup() {
  local staging="$1"
  local setup_src="$PROJECT_ROOT/deploy/packaging/linux/setup.sh"
  if [ -f "$setup_src" ]; then
    mkdir -p "$staging/usr/lib/hologram"
    cp "$setup_src" "$staging/usr/lib/hologram/setup.sh"
    chmod 755 "$staging/usr/lib/hologram/setup.sh"
  fi
}

# 项目自身的许可：**所有变体**都要带（client 也分发 waiter 与 GUI）。
#
# deb 按 Debian 惯例给 /usr/share/doc/hologramagent/copyright（DEP-5 机器可读格式），
# 同时把 LICENSE 全文放进去；rpm 的许可走 fpm 的 --license 元数据。
# 与 stage_multimodal_assets 的 licenses/ 分工：那里放**第三方**（模型/运行库）的
# 许可全文，这里放本项目自己的。
stage_license() {
  local staging="$1"
  local docdir="$staging/usr/share/doc/hologramagent"
  mkdir -p "$docdir"
  cp "$PROJECT_ROOT/LICENSE" "$docdir/LICENSE"
  cat > "$docdir/copyright" <<'EOF'
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: HologramAgent
Source: https://github.com/JianFeeeee/HologramAgent

Files: *
Copyright: HologramAgent contributors
License: AGPL-3.0-only
 This program is free software: you can redistribute it and/or modify it under
 the terms of the GNU Affero General Public License as published by the Free
 Software Foundation, version 3 of the License.
 .
 This program is distributed in the hope that it will be useful, but WITHOUT
 ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
 FOR A PARTICULAR PURPOSE.  See the GNU Affero General Public License for more
 details.
 .
 You should have received a copy of the GNU Affero General Public License along
 with this program.  If not, see <https://www.gnu.org/licenses/>.
 .
 The license is AGPL-3.0-only: no later version may be chosen. Note the network
 clause (§13 Remote Network Interaction) — offering modified versions of this
 software to users over a network also requires offering them the source.
 .
 Full text: /usr/share/doc/hologramagent/LICENSE

Files: usr/lib/hologram/models/chinese-clip-vit-b16-onnx/*
Copyright: OFA-Sys / Chinese-CLIP authors
License: Apache-2.0
 Full text: /usr/share/doc/hologramagent/licenses/Chinese-CLIP-Apache-2.0.txt
 Comment: pre-trained model artifacts; NOT covered by this package's AGPL grant

Files: usr/lib/hologram/onnxruntime/*
Copyright: Microsoft Corporation
License: MIT
 Full text: /usr/share/doc/hologramagent/licenses/ONNX-Runtime-MIT.txt
 Comment: license texts and third-party notices under licenses/ONNX-Runtime-*
EOF
  chmod 644 "$docdir/LICENSE" "$docdir/copyright"
}

# server/full 的 ONNX 资产。模型与运行库是发行版能力的一部分，不是可选下载：
# 只要打 server/full 包，两者缺一就失败。client 包不运行 homed，故不携带。
stage_multimodal_assets() {
  local staging="$1"
  local model_dst="$staging/usr/lib/hologram/models/chinese-clip-vit-b16-onnx"
  local ort_dst="$staging/usr/lib/hologram/onnxruntime"
  local licenses="$staging/usr/share/doc/hologramagent/licenses"

  if [ ! -d "$CHINESECLIP_BUNDLE_DIR" ]; then
    echo "ERROR: Chinese-CLIP 产物目录不存在：$CHINESECLIP_BUNDLE_DIR" >&2
    echo "先运行 scripts/export_chineseclip_onnx.py，再通过 CHINESECLIP_BUNDLE_DIR 指向产物。" >&2
    return 1
  fi
  for f in TextEncoder.onnx VisionEncoder.onnx embed_config.json vocab.txt reference.json SHA256SUMS; do
    if [ ! -s "$CHINESECLIP_BUNDLE_DIR/$f" ]; then
      echo "ERROR: Chinese-CLIP 产物缺少或为空：$CHINESECLIP_BUNDLE_DIR/$f" >&2
      return 1
    fi
  done
  if ! (cd "$CHINESECLIP_BUNDLE_DIR" && sha256sum -c SHA256SUMS); then
    echo "ERROR: Chinese-CLIP SHA256SUMS 校验失败，拒绝打包。" >&2
    return 1
  fi

  if [ ! -s "$ONNXRUNTIME_LIB" ]; then
    echo "ERROR: ONNX Runtime 不存在：$ONNXRUNTIME_LIB" >&2
    echo "通过 ONNXRUNTIME_ASSET_DIR 或 ONNXRUNTIME_LIB 指向与目标架构匹配的资产。" >&2
    return 1
  fi
  for notice in "$ONNXRUNTIME_LICENSE" "$ONNXRUNTIME_NOTICES"; do
    if [ ! -s "$notice" ]; then
      echo "ERROR: ONNX Runtime 许可证资产缺失：$notice" >&2
      return 1
    fi
  done
  local runtime_desc
  runtime_desc=$(file -b "$ONNXRUNTIME_LIB")
  case "$ARCH" in
    amd64) printf '%s' "$runtime_desc" | grep -qE 'x86-64|x86_64' || {
      echo "ERROR: ONNX Runtime 架构不是 amd64：$runtime_desc" >&2; return 1; } ;;
    arm64) printf '%s' "$runtime_desc" | grep -qE 'aarch64|ARM aarch64' || {
      echo "ERROR: ONNX Runtime 架构不是 arm64：$runtime_desc" >&2; return 1; } ;;
  esac

  mkdir -p "$model_dst" "$ort_dst" "$licenses"
  cp -a "$CHINESECLIP_BUNDLE_DIR/." "$model_dst/"
  install -m 0755 "$ONNXRUNTIME_LIB" "$ort_dst/libonnxruntime.so"

  # 许可证随二进制分发：Chinese-CLIP = Apache-2.0；ONNX Runtime = MIT，
  # 同时携带其 ThirdPartyNotices（含 MKL/protobuf/zlib 等第三方条款）。
  cp /usr/share/common-licenses/Apache-2.0 "$licenses/Chinese-CLIP-Apache-2.0.txt"
  cp "$ONNXRUNTIME_LICENSE" "$licenses/ONNX-Runtime-MIT.txt"
  cp "$ONNXRUNTIME_NOTICES" "$licenses/ONNX-Runtime-ThirdPartyNotices.txt"
  cat > "$licenses/MODEL-SOURCES.txt" <<EOF
Chinese-CLIP ViT-B/16
  upstream: https://huggingface.co/OFA-Sys/chinese-clip-vit-base-patch16
  license: Apache-2.0
  exported-by: scripts/export_chineseclip_onnx.py
  dimensions: 512
  modalities: text,image

ONNX Runtime
  upstream: https://github.com/microsoft/onnxruntime
  license: MIT (see ONNX-Runtime-MIT.txt and ONNX-Runtime-ThirdPartyNotices.txt)
EOF

  echo "  ONNX assets: model=$(du -sh "$model_dst" | cut -f1) runtime=$(du -h "$ort_dst/libonnxruntime.so" | cut -f1)"
}

# ---- create .deb ----
build_deb() {
  local variant="$1"
  local staging="$2"
  local deb_dir="${DIST_DIR}/deb"
  mkdir -p "$deb_dir"

  local pkg_name="hologramagent-${variant}_${PKG_VERSION}_${DEB_ARCH}.deb"
  local deb_root
  deb_root="$(mktemp -d "$STAGE_TMP/deb.XXXXXX")"

  mkdir -p "$deb_root/DEBIAN"

  local control_file="$PACKAGE_ROOT/deb/control-${variant}"
  local installed_size_kb
  installed_size_kb=$(du -sk "$staging" | cut -f1)

  sed -e "s/VERSION_PLACEHOLDER/$PKG_VERSION/g" \
      -e "s/ARCH_PLACEHOLDER/$DEB_ARCH/g" \
      -e "s/INSTALLED_SIZE_PLACEHOLDER/$installed_size_kb/g" \
      "$control_file" > "$deb_root/DEBIAN/control"

  if [ -f "$PACKAGE_ROOT/deb/postinst" ]; then
    cp "$PACKAGE_ROOT/deb/postinst" "$deb_root/DEBIAN/postinst"
    chmod 755 "$deb_root/DEBIAN/postinst"
  fi

  if [ -f "$PACKAGE_ROOT/deb/prerm" ]; then
    cp "$PACKAGE_ROOT/deb/prerm" "$deb_root/DEBIAN/prerm"
    chmod 755 "$deb_root/DEBIAN/prerm"
  fi

  cp -r "$staging"/* "$deb_root/" 2>/dev/null || true

  echo ">>> Building .deb: $pkg_name"
  fakeroot dpkg-deb --build "$deb_root" "$deb_dir/$pkg_name" 2>/dev/null || \
    dpkg-deb --build "$deb_root" "$deb_dir/$pkg_name" 2>&1
  rm -rf "$deb_root"
  echo "  Created: $deb_dir/$pkg_name ($(du -h "$deb_dir/$pkg_name" | cut -f1))"
}

# ---- create combined .tar.gz (all binaries, no variant split) ----
build_tar() {
  local tar_dir="${DIST_DIR}/tar"
  mkdir -p "$tar_dir"

  local archive_name="hologramagent_${PKG_VERSION}_linux_${TAR_ARCH}.tar.gz"
  local archive_dir="hologramagent-${PKG_VERSION}-linux-${TAR_ARCH}"

  # build combined staging
  local staging
  staging="$(mktemp -d "$STAGE_TMP/tar.XXXXXX")"
  mkdir -p "$staging/usr/bin" "$staging/usr/lib/hologram" "$staging/etc/systemd/system"

  # copy all available binaries
  for bin in homed waiter initconfig; do
    local src="$BUILD_DIR/${bin}_linux_${TAR_ARCH}"
    [ -f "$src" ] && cp "$src" "$staging/usr/bin/$bin"
  done

  # setup script
  local setup_src="$PROJECT_ROOT/deploy/packaging/linux/setup.sh"
  [ -f "$setup_src" ] && cp "$setup_src" "$staging/usr/lib/hologram/setup.sh"
  cp "$PROJECT_ROOT/deploy/hologramagent.service" "$staging/etc/systemd/system/hologramagent.service"
  stage_license "$staging"
  stage_multimodal_assets "$staging"

  # GUI if available
  local gui_src="$BUILD_DIR/hologram-gui-linux-${TAR_ARCH}"
  if [ -d "$gui_src" ]; then
    mkdir -p "$staging/usr/lib/hologram-gui"
    cp -r "$gui_src"/* "$staging/usr/lib/hologram-gui/"
    cat > "$staging/usr/bin/hologram-gui" << 'SCRIPT'
#!/bin/sh
exec /usr/lib/hologram-gui/hologram-gui "$@"
SCRIPT
    chmod 755 "$staging/usr/bin/hologram-gui"
  fi

  chmod 755 "$staging/usr/bin/"* 2>/dev/null || true

  echo ">>> Building .tar.gz: $archive_name"
  (cd "$staging" && tar czf "$tar_dir/$archive_name" --transform "s|^\.|${archive_dir}|" .)
  echo "  Created: $tar_dir/$archive_name ($(du -h "$tar_dir/$archive_name" | cut -f1))"
  rm -rf "$staging"
}

# ---- create .rpm (via fpm if available) ----
build_rpm() {
  local variant="$1"
  local staging="$2"
  local rpm_dir="${DIST_DIR}/rpm"
  mkdir -p "$rpm_dir"

  local pkg_name="hologramagent-${variant}-${PKG_VERSION}-1.${RPM_ARCH}.rpm"

  # find fpm
  local fpm_bin="$(command -v fpm 2>/dev/null || true)"
  if [ -z "$fpm_bin" ]; then
    fpm_bin="$(find /home -name "fpm" -type f -path "*/bin/*" 2>/dev/null | head -1 || true)"
  fi

  if [ -z "$fpm_bin" ]; then
    echo "  SKIP .rpm: fpm not installed. Install it with: gem install fpm"
    echo "  Alternatively, build RPM on Fedora/RHEL using:"
    echo "    rpmbuild -ba deploy/packaging/linux/hologramagent.spec"
    return
  fi

  # find or extract rpmbuild (fpm needs it)
  local rpmbuild_dir="/tmp/rpmext"
  if [ ! -f "$rpmbuild_dir/usr/bin/rpmbuild" ]; then
    # try to extract from cached deb packages
    local rpm_deb
    # 同 build_gui：`find` 对不存在/无命中会返回 1，`set -euo pipefail` 下
    # 会让脚本当场退出（`|| true` 是给命令替换兜底，不是忽视错误）。
    rpm_deb="$(find /tmp -name "rpm_*.deb" -type f 2>/dev/null | head -1 || true)"
    if [ -z "$rpm_deb" ]; then
      rpm_deb="$(find "$PROJECT_ROOT" -name "rpm_*.deb" -type f 2>/dev/null | head -1 || true)"
    fi
    if [ -n "$rpm_deb" ]; then
      mkdir -p "$rpmbuild_dir"
      (cd "$rpmbuild_dir" && ar x "$rpm_deb" 2>/dev/null && \
        tar --no-same-permissions -xf data.tar.zst --zstd 2>/dev/null) || true
      for libdeb in /tmp/librpm*.deb; do
        [ -f "$libdeb" ] && (cd "$rpmbuild_dir" && ar x "$libdeb" 2>/dev/null && \
          tar --no-same-permissions -xf data.tar.zst --zstd 2>/dev/null) || true
      done
    fi
  fi

  if [ -f "$rpmbuild_dir/usr/bin/rpmbuild" ]; then
    export PATH="$rpmbuild_dir/usr/bin:$PATH"
    export LD_LIBRARY_PATH="$rpmbuild_dir/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
    export RPM_CONFIGDIR="$rpmbuild_dir/usr/lib/rpm"
  fi

  echo ">>> Building .rpm via fpm: $pkg_name"
  "$fpm_bin" -s dir -t rpm \
    -n "hologramagent-${variant}" \
    -v "$VERSION" \
    --iteration 1 \
    -a "$RPM_ARCH" \
    --description "HologramAgent ${variant^} package" \
    --url "https://github.com/trueagent/HologramAgent" \
    --license "AGPL-3.0-only" \
    -C "$staging" \
    -p "$rpm_dir/$pkg_name" \
    . 2>&1
  echo "  Created: $rpm_dir/$pkg_name"
}

# ---- main ----
main() {
  local target_arch="$ARCH"

  mkdir -p "$BUILD_DIR" "$STAGE_TMP"

  case "$ACTION" in
    all|build)
      build_go
      build_gui
      ;;
  esac

  if [ "$ACTION" = "build" ]; then
    echo ""
    echo "=== Build complete. Binaries in $BUILD_DIR ==="
    exit 0
  fi

  mkdir -p "$DIST_DIR"

  # 上次成功构建留下的校验和必须在本次开工前删掉：本次若中途失败，脚本直接退出、
  # 不重算 SHA256SUMS，旧的它会一直躺在 dist 里，看上去像在为这一批残缺产物背书。
  rm -f "$DIST_DIR/SHA256SUMS"

  for variant in full server client toolchain; do
    echo ""
    echo "=============================================="
    echo "  Packaging: $variant"
    echo "=============================================="

    local staging
    staging=$(mktemp -d "$STAGE_TMP/stage.XXXXXX")
    stage_variant "$variant" "$staging"

    case "$ACTION" in
      all|deb) build_deb "$variant" "$staging" ;;
    esac

    case "$ACTION" in
      all|rpm) build_rpm "$variant" "$staging" ;;
    esac

    rm -rf "$staging"
  done

  case "$ACTION" in
    all|tar) build_tar ;;
  esac

  echo ""
  echo "=== Done! Packages in: $DIST_DIR ==="
  echo ""
  echo "Summary:"
  # 只列**本批**产物：dist/ 会跨多次构建累积，用 find 全目录会让清单／SHA256SUMS
  # 带上历史版本的文件名——用户下载那种清单后 `sha256sum -c` 必然报缺失。
  # （v1.2.2 构建时就出现过：清单里混进了 1.2.0/1.2.1 的包名。）按本批版本号过滤。
  mapfile -t release_files < <(find "$DIST_DIR" -type f \( -name "*${PKG_VERSION}*.deb" -o -name "hologramagent_${PKG_VERSION}_*.tar.gz" -o -name "*${PKG_VERSION}*.rpm" \) 2>/dev/null | sort)
  for f in "${release_files[@]}"; do
    echo "  $(du -h "$f" | cut -f1)  $f"
  done
  # 全部包生成之后一次计算，避免边打边算漏掉后生成的产物。
  # 名字用**平铺名**（basename）：下载页的附件名就是平铺的，
  # 清单里若写 ./deb/xxx.deb，用户下载后 `sha256sum -c` 会找不到文件。
  if [ ${#release_files[@]} -gt 0 ]; then
    (
      cd "$DIST_DIR"
      # 哈希取**真实路径**，标签用**平铺名**：两者不能混（直接对 basename 求哈希会找不到文件）。
      for f in "${release_files[@]}"; do
        printf '%s  ./%s\n' "$(sha256sum "$f" | awk '{print $1}')" "$(basename "$f")"
      done | sort -k2 > SHA256SUMS
    )
    echo "  SHA256SUMS: $DIST_DIR/SHA256SUMS  （仅本批 ${#release_files[@]} 个产物，平铺名）"
  fi
}

main

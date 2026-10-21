#!/usr/bin/env bash
# SDK 文档站一键构建 + 部署（sdk.homeagent.jianfgit.xyz）
#
# 链路（全部实测确认，写在这里免得下次再摸一遍）：
#   本机 192.168.2.60
#     └─ nginx stream 按 ssl_preread SNI 把 443 透传 → 192.168.2.106:3080
#          └─ navi 容器内的 nginx（不是 portal-nginx，那个已 Exited 两周）
#               └─ server_name sdk.homeagent.jianfgit.xyz → root /app/data/sites/sdk
#                    └─ 宿主对应 /vol1/docker/navi-data/sites/sdk
#
# 两个容易踩的坑：
#   1. 构建**必须**走 tools/apidoc/build.sh，不能裸跑 mkdocs build。
#      裸跑会少掉整个 api/*.md 和 llms.txt —— 而 llms.txt 正是给 agent 用的入口。
#      正确产物 106 个文件，裸跑只有 78 个。
#   2. 登录 .106 只能用 admin@ + sudo -n，root@ 会被拒。
#      设备网关白名单（waiter.yaml 的 device_cmd_allowlist）现有 22 条：
#      ls pwd cat du df free ps ip uname uptime date hostname
#      find grep sed sort tr wc head tail stat file
#      —— 里面**没有 tar**（有 sed 也不能打包），所以走 SSH 直连。
#      ★ 早先这里写的是「只放行 ls/stat/find/cat」，那是 waiter 白名单
#        硬编码 18 条时的状况；2026-09-27 部署 7193446 后扩到 22 条，
#        结论（打不了包）不变但**理由已变**，别照旧文字理解。
#
# 用法：
#   ./deploy/scripts/deploy-sdk-site.sh                # 构建 + 部署 + 验证（两个站）
#   ./deploy/scripts/deploy-sdk-site.sh --check        # 只核对线上与本地产物差异，不动线上
#   ./deploy/scripts/deploy-sdk-site.sh --introduce    # 只部署 introduce 站（改 site/ 时用，跳过 SDK 站重建）
#   ./deploy/scripts/deploy-sdk-site.sh --rollback <备份目录名>            # 回滚 sdk 站
#   ./deploy/scripts/deploy-sdk-site.sh --rollback-introduce <备份目录名>  # 回滚 introduce 站

set -euo pipefail

SDK_DIR=${REPO}/third_party/homeagent-sdk
BUILD_DIR="$SDK_DIR/site_build"
HOST=192.168.2.106
SSH="ssh -n -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=no admin@$HOST"
SITES=/vol1/docker/navi-data/sites
TS=$(date +%Y%m%d-%H%M%S)
PKG_NAME=sdk-site-build-$TS.tar.gz
PKG=/tmp/$PKG_NAME

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }

# ── 比对工具：必须是**真能报出差异**的 diff ──
#
# 2026-09-29 实测踩到：本机 PATH 首位的 diff
# （/opt/huawei/harmonyos/ohos-sdk/linux/toolchains/diff）对**任何**输入都返回 0
# 且无输出。用它做判据的后果不是「偶尔报错」，而是**永久给出「逐字节一致」的假绿灯**——
# --check 永远说「无需部署」，站点改完再也不会被更新，而且看不出来。
#
# 所以：钉绝对路径 + 启动时自检一次。自检不过就直接退出，
# 而不是继续拿一个坏判据去做部署决定（「探针不红先怀疑探针」）。
DIFF=/usr/bin/diff
[ -x "$DIFF" ] || { printf '找不到可用的 %s\n' "$DIFF" >&2; exit 1; }
if "$DIFF" -q <(printf 'A\n') <(printf 'B\n') >/dev/null 2>&1; then
  printf '★ 判据自检失败：%s 对两个不同输入报告「无差异」。\n' "$DIFF" >&2
  printf '  这个 diff 是坏的（PATH 首位那个工具链自带的可能有问题），拒绝据此做部署判断。\n' >&2
  exit 1
fi

# ── introduce 站 ──
#
# 零构建：${REPO}/site/ 里就是成品（site/README.md 自称
# 「单文件、零构建、零依赖」），改完直接是产物，所以这条只需要同步。
#
# 部署时**故意不带上 README.md**：那是仓库里的说明文档，不是站点资源。
# 线上现有 2 个文件（index.html + assets/logo.svg），带 README 会多出一个
# 线上原本没有的文件，反而让 diff 比对永远不为零。
INTRO_SRC=${REPO}/site
INTRO_PKG_NAME=intro-site-$TS.tar.gz
INTRO_PKG=/tmp/$INTRO_PKG_NAME

intro_local_md5() {
  (cd "$INTRO_SRC" && find index.html assets -type f -print0 | sort -z | xargs -0 md5sum) | norm_md5
}

intro_remote_md5() {
  $SSH "cd $SITES/introduce && sudo -n find . -type f -print0 | sort -z | sudo -n xargs -0 md5sum" 2>/dev/null | norm_md5
}

do_check_introduce() {
  say "检查 introduce 站"
  local n files
  n=$(intro_local_md5 | wc -l)
  info "本机源: $n 个文件（已排除 README.md）"
  files=$($SSH "sudo -n find $SITES/introduce -type f 2>/dev/null | wc -l" 2>/dev/null)
  info "线上站:   $files 个文件"
  if "$DIFF" -q <(intro_local_md5) <(intro_remote_md5) >/dev/null 2>&1; then
    info "✓ introduce 线上与本机源逐字节一致"
    return 0
  fi
  info "✗ introduce 有差异："
  "$DIFF" <(intro_local_md5) <(intro_remote_md5) | head -20 || true
  return 1
}

do_deploy_introduce() {
  say "introduce：打包上传"
  tar -czf "$INTRO_PKG" -C "$INTRO_SRC" index.html assets
  local sz
  sz=$(du -h "$INTRO_PKG" | cut -f1)
  scp -q -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=no "$INTRO_PKG" "admin@$HOST:/tmp/" || {
    info "✗ 上传失败"; rm -f "$INTRO_PKG"; return 1; }
  rm -f "$INTRO_PKG"
  info "已上传 $sz"

  $SSH "
    set -e
    sudo -n cp -a $SITES/introduce $SITES/.intro-bak-$TS
    sudo -n mkdir -p $SITES/.intro-new-$TS
    sudo -n tar -xzf /tmp/$INTRO_PKG_NAME -C $SITES/.intro-new-$TS
    sudo -n chown -R root:root $SITES/.intro-new-$TS
    sudo -n rm -rf $SITES/.intro-old-$TS
    sudo -n mv $SITES/introduce $SITES/.intro-old-$TS
    sudo -n mv $SITES/.intro-new-$TS $SITES/introduce
    sudo -n rm -rf $SITES/.intro-old-$TS
    rm -f /tmp/$INTRO_PKG_NAME
    echo -n '现役站文件数: '; sudo -n find $SITES/introduce -type f | wc -l
  " 2>&1 | tail -3
  info "回滚点: $SITES/.intro-bak-$TS"

  local code
  code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' -k https://introduce.homeagent.jianfgit.xyz/)
  info "首页 http=$code（introduce 配了 try_files 回落，404 才是异常）"
  if "$DIFF" -q <(intro_local_md5) <(intro_remote_md5) >/dev/null 2>&1; then
    info "✓ introduce 线上与本机源逐字节一致"
  else
    info "✗ introduce 仍有差异："; "$DIFF" <(intro_local_md5) <(intro_remote_md5) | head -20; return 1
  fi
}

# md5 清单统一去掉路径前缀 ./，否则本地与线上排序后无法直接 diff。
# 归一化放在本机做：远端 sed 若嵌在 ssh 的双引号里，转义会被外层 shell 先吃掉。
norm_md5() { sed 's|  \./|  |'; }

# 本地产物 → md5 清单
local_md5() {
  (cd "$BUILD_DIR" && find . -type f -print0 | sort -z | xargs -0 md5sum) | norm_md5
}

# 线上产物 → md5 清单
remote_md5() {
  $SSH "cd $SITES/sdk && sudo -n find . -type f -print0 | sort -z | sudo -n xargs -0 md5sum" 2>/dev/null | norm_md5
}

do_check() {
  say "检查 $HOST 上的 sdk 站"
  local n files
  n=$(local_md5 | wc -l)
  info "本地产物: $n 个文件"
  files=$($SSH "sudo -n find $SITES/sdk -type f 2>/dev/null | wc -l" 2>/dev/null)
  info "线上站:   $files 个文件"
  if "$DIFF" -q <(local_md5) <(remote_md5) >/dev/null 2>&1; then
    info "✓ 线上与本地产物逐字节一致，无需部署"
  else
    info "✗ sdk 有差异，需部署。差异文件："
    "$DIFF" <(local_md5) <(remote_md5) | head -20 || true
  fi

  # introduce 始终检查：两个站是独立的，sdk 一致不代表 introduce 也一致
  do_check_introduce || true
}

do_deploy() {
  say "1/4 构建（$TS）"
  (cd "$SDK_DIR" && tools/apidoc/build.sh) >/tmp/sdk-build-$TS.log 2>&1 || {
    info "✗ 构建失败，日志：/tmp/sdk-build-$TS.log"; tail -20 /tmp/sdk-build-$TS.log; exit 1; }
  local n
  n=$(find "$BUILD_DIR" -type f | wc -l)
  info "产物 $n 个文件"
  [ "$n" -ge 100 ] || { info "✗ 产物只有 $n 个，八成是裸跑了 mkdocs（应调 build.sh）"; exit 1; }

  say "2/4 打包上传"
  tar -czf "$PKG" -C "$BUILD_DIR" .
  scp -q -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=no "$PKG" "admin@$HOST:/tmp/" || {
    info "✗ 上传失败"; exit 1; }
  info "已上传 $(du -h "$PKG" | cut -f1)"
  rm -f "$PKG"

  say "3/4 原子替换（先备份再换）"
  $SSH "
    set -e
    sudo -n cp -a $SITES/sdk $SITES/.sdk-bak-$TS
    sudo -n mkdir -p $SITES/.sdk-new-$TS
    sudo -n tar -xzf /tmp/$PKG_NAME -C $SITES/.sdk-new-$TS
    sudo -n chown -R root:root $SITES/.sdk-new-$TS
    sudo -n mv $SITES/sdk $SITES/.sdk-old-$TS
    sudo -n mv $SITES/.sdk-new-$TS $SITES/sdk
    echo -n '现役站文件数: '; sudo -n find $SITES/sdk -type f | wc -l
    # 注意：这里的 diff 是**远端**执行的（远端是健康的 /usr/bin/diff），
    # 不受本机 PATH 首位那个坏 diff 影响。
    if sudo -n diff -r -q $SITES/.sdk-bak-$TS $SITES/.sdk-old-$TS >/dev/null 2>&1; then
      sudo -n rm -rf $SITES/.sdk-old-$TS
      echo '已删重复的 .sdk-old（与 .sdk-bak 内容相同）'
    else
      echo '★ .sdk-old 与 .sdk-bak 内容不同，两份都保留，请人工确认'
    fi
    rm -f /tmp/$PKG_NAME
  " 2>&1 | tail -5
  info "回滚点: $SITES/.sdk-bak-$TS"

  say "4/4 验证"
  # 静态文件换完 nginx 直接吃新文件，不需要 reload。
  local code
  code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' -k https://sdk.homeagent.jianfgit.xyz/)
  info "首页 http=$code"
  for p in guide/scene-memory/ guide/parallel-tool-declaration/ llms.txt llms-full.txt; do
    printf '  %-34s ' "$p"
    curl -s -o /dev/null -m 10 -w 'http=%{http_code}\n' -k "https://sdk.homeagent.jianfgit.xyz/$p"
  done
  if "$DIFF" -q <(local_md5) <(remote_md5) >/dev/null 2>&1; then
    info "✓ 线上与本地产物逐字节一致"
  else
    info "✗ 仍有差异："; "$DIFF" <(local_md5) <(remote_md5) | head -20; return 1
  fi

  do_deploy_introduce
}

# 只跑 introduce 站：改 site/ 时用。
#
# 为何需要：do_deploy 会先把 SDK 文档站重建一遍（要跑 apidoc + mkdocs，产物 106 个文件、
# 十几 MB），而 site/ 的改动跟 SDK 站毫无关系。只动介绍页却重建整个文档站，
# 既慢又把一个本来没必要碰的线上站也卷进变更面。
do_deploy_introduce_only() {
  say "introduce 站：先比对再决定"
  # 一致就不动线上（越少触碰越好）；不一致才走「备份→原子替换→回验」
  if do_check_introduce; then
    info "✓ 线上已是最新，无需部署"
    return 0
  fi
  do_deploy_introduce
}

do_rollback() {
  local bak=${1:?用法: --rollback <备份目录名，如 .sdk-bak-20260927-223242>}
  say "回滚到 $bak"
  $SSH "
    set -e
    [ -d '$SITES/$bak' ] || { echo '找不到备份 $SITES/$bak'; exit 1; }
    sudo -n cp -a $SITES/sdk $SITES/.sdk-failed-$TS
    sudo -n rm -rf $SITES/sdk
    sudo -n cp -a $SITES/$bak $SITES/sdk
    echo -n '回滚后文件数: '; sudo -n find $SITES/sdk -type f | wc -l
  " 2>&1 | tail -3
  info "已回滚，失败版本留在 $SITES/.sdk-failed-$TS"
}

do_rollback_introduce() {
  local bak=${1:?用法: --rollback-introduce <备份目录名，如 .intro-bak-20260929-170000>}
  say "回滚 introduce 到 $bak"
  $SSH "
    set -e
    [ -d '$SITES/$bak' ] || { echo '找不到备份 $SITES/$bak'; exit 1; }
    sudo -n cp -a $SITES/introduce $SITES/.intro-failed-$TS
    sudo -n rm -rf $SITES/introduce
    sudo -n cp -a $SITES/$bak $SITES/introduce
    echo -n '回滚后文件数: '; sudo -n find $SITES/introduce -type f | wc -l
  " 2>&1 | tail -3
  info "已回滚，失败版本留在 $SITES/.intro-failed-$TS"
}

case "${1:-}" in
  --check)              do_check ;;
  --introduce)          do_deploy_introduce_only ;;
  --rollback)           do_rollback "${2:?用法: --rollback <备份目录名>}" ;;
  --rollback-introduce) do_rollback_introduce "${2:?用法: --rollback-introduce <备份目录名>}" ;;
  "")                   do_deploy ;;
  *)  echo "用法: $0 [--check | --introduce | --rollback <备份目录名> | --rollback-introduce <备份目录名>]"; exit 2 ;;
esac

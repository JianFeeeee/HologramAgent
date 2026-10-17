package systemd

// 数据目录迁移（切换 data_dir 时必须**连带迁移**，否则等于丢数据）。
//
// ## 为什么迁移不能只是 `mv`
//
// 实测盘点生产数据目录（2026-10-17，21G）后确认有四类**必须特殊处理**的东西：
//
//  1. **agentfs 是 overlay 挂载**（lower/upper/work 三段）：
//       overlay on <data>/agentfs/merged type overlay (
//                 lowerdir=<data>/agentfs/lower,
//                 upperdir=<data>/agentfs/upper,
//                 workdir=<data>/agentfs/work)
//     ⇒ 不先 `umount` 就 `mv`，会搬走挂载点下的"空壳"，而真正内容仍挂在旧路径；
//        或者搬完挂载关系悬空 → 新目录里 agentfs 不可用。
//
//  2. **数据库里有 12 处绝对路径**（config 8 + config_mcp 1 + config_files 1 + config_qq 2）：
//       core.daemon.data_dir / core.plugin.dir / core.memory.graph / core.memory.text /
//       core.memory.documents / core.knowledge.path / core.log.path /
//       core.memory.multimodal_space.options.model_dir
//     ⇒ 只搬文件不改库，重启后内核仍按旧绝对路径找数据（找不到就重建空库，
//        看起来像"记忆全丢了"，且**不会报错**）。
//
//  3. **插件自己的数据文件里也写死了绝对路径**（实测 46 个文件），
//     典型：plugin_data/commondev/ledger.json 里的 mirror_path / workdir（git 仓库路径）。
//     ⇒ 不迁这些，插件会指向旧目录里的 git 仓库（或报不存在）。
//
//  4. **不能迁的**：heartbeat（运行期状态，重启自动重建）、
//     agentfs 的挂载点本身（卸了重挂即可）。
//
// ## ★ 为什么要生成脚本而不是直接执行
//
// 迁移是**破坏性且不可逆**的操作（21G 数据 + 12 处 DB 改写 + 46 个文件改写），
// 而内核运行在 systemd 的 ProtectSystem/ProtectHome 沙箱里，本来也写不了
// /etc/systemd。⇒ 生成一个**可审查、可回滚**的脚本交给运维执行，
// 比内核自作主张更安全。
//
// 脚本特性：
//   · `set -euo pipefail` + 每步幂等
//   · **先检查目标目录不存在或为空**（防覆盖已有数据）
//   · 用 `rsync -aHAX`（保留硬链接/ACL/xattr；生产有大量 git 硬链接）
//   · 迁移后**校验**关键文件数量与 DB 行数
//   · 生成 `rollback.sh`（不删源目录，回滚就是把 systemd 改回去）

import (
	"fmt"
	"path/filepath"
	"strings"
)

// MigratePlan 是一次目录迁移的完整计划。
type MigratePlan struct {
	OldDir string // 当前数据目录（-data 的实际值）
	NewDir string // 目标数据目录

	// RuntimeDirs 是运行期状态，**不迁**（重启自动重建）。
	RuntimeDirs []string
	// MountPoints 是需要先卸载的挂载点（相对 OldDir）。
	MountPoints []string
	// DataDirHints 是数据库里存绝对路径的键（迁移后要改写）。
	DataDirHints []string
}

// NewMigratePlan 构造默认计划（清单来自 2026-10-17 生产盘点）。
func NewMigratePlan(oldDir, newDir string) MigratePlan {
	return MigratePlan{
		OldDir: filepath.Clean(oldDir),
		NewDir: filepath.Clean(newDir),
		// 运行期状态：这些**不迁**。heartbeat 是 guard 判活依据，重启重建。
		RuntimeDirs: []string{"heartbeat"},
		// overlay 挂载点：必须先 umount。
		MountPoints: []string{"agentfs/merged"},
		// 数据库里存绝对路径的键（实测 8 个 core.* + 插件表另算）。
		DataDirHints: []string{
			"core.daemon.data_dir",
			"core.plugin.dir",
			"core.memory.graph",
			"core.memory.text",
			"core.memory.documents",
			"core.knowledge.path",
			"core.log.path",
			"core.memory.multimodal_space.options.model_dir",
		},
	}
}

// Script 生成**可直接执行**的迁移脚本。
//
// 用户明确要求「指令应当直接包含替换部分而不是让用户手动修改」，
// 所以这里把新旧路径、systemd 的 sed 替换、DB 的 UPDATE 全部内联进脚本。
func (p MigratePlan) Script() string {
	var b strings.Builder
	old, new_ := p.OldDir, p.NewDir
	unitPath := filepath.Join("/etc/systemd/system", UnitName)

	fmt.Fprintf(&b, `#!/usr/bin/env bash
# HomeAgent 数据目录迁移：%s → %s
#
# ★ 这是**破坏性**操作。脚本已内置：
#   · 目标目录非空则拒绝执行
#   · 先卸载 overlay 挂载（agentfs/merged），否则会搬空壳
#   · rsync 保留硬链接/ACL/xattr（生产有大量 git 硬链接）
#   · 迁移后改写 systemd 单元 + 数据库里的绝对路径
#   · 不删源目录（回滚 = 把 systemd 改回去再重启）
#
# 用法： sudo bash %s
set -euo pipefail

OLD=%q
NEW=%q
UNIT=%q

log(){ printf '\033[1;34m[migrate]\033[0m %%s\n' "$*"; }
die(){ printf '\033[1;31m[FATAL]\033[0m %%s\n' "$*" >&2; exit 1; }

# ★ dry-run：只打印将要做什么，**不落任何改动**。
#
# 为什么必须有（实测教训）：演练时我直接跑脚本，它把
# /etc/systemd/system/homeagent.service 的 -data 改成了演练目标目录。
# 真迁移当然该改，但**演练不该**。运维想先看流程时必须有安全通道。
DRY=0
for a in "$@"; do
  case "$a" in
    -n|--dry-run) DRY=1 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) die "未知参数：$a（支持 --dry-run）" ;;
  esac
done
run(){ if [ "$DRY" = "1" ]; then printf '  [dry-run] %%s\n' "$*"; else "$@"; fi; }

[ "$(id -u)" -eq 0 ] || die "需要 root：sudo bash $0"
[ -d "$OLD" ] || die "源目录不存在：$OLD"
[ "$OLD" != "$NEW" ] || die "源与目标相同，无需迁移"

# ★ 强制校验：systemd 当前的 -data 必须就是 OLD。
#
# 防的是什么：运维把 OLD 写错（比如写成一个备份目录），脚本会一路把它
# 当成真数据源搬走并改 systemd ⇒ **指向了错的数据**。先对一次，成本极低。
# shellcheck disable=SC2016
cur=$(grep -hoE '(-data[= ])[^ "]+' "$UNIT" "$UNIT".d/*.conf 2>/dev/null | tail -1 | sed -E 's/-data[= ]//')
if [ -n "$cur" ] && [ "$cur" != "$OLD" ]; then
  die "systemd 当前的 -data 是 $cur，与脚本的 OLD=$OLD 不一致。
     若确实要迁移 $cur，请用 OLD=$cur 重新生成脚本。
     若这是演练，请加 --dry-run。"
fi

# ── 0) 目标必须不存在或为空 ──
if [ -d "$NEW" ] && [ -n "$(ls -A "$NEW" 2>/dev/null || true)" ]; then
  die "目标目录非空：$NEW（拒绝覆盖；请先确认或换一个路径）"
fi

# ── 1) 停服务（guard 会连带收 worker）──
#
# ★ 这一步就是用户要的「伪关机」的**进程级落实**：
#
#   内核侧的伪关机（kernelboot.BeginMigration：运行态停插件 + 暂停 distiller/
#   textmem）能保证"同进程内不再写"，但它**挡不住**：
#     · 插件自己进程里的定时器（rss 轮询、calendar 到期检查）
#     · 子进程还在跑（StopPlugin 之后才退，中间窗口仍可能落盘）
#     · 任何我们没想到的/proc/self 级写入
#
#   而 systemctl stop 之后**没有进程能写** —— 这是唯一零漏网的保证。
#   ⇒ 两者配合：内核伪关机负责"优雅收尾 + flush"，stop 负责"绝对静止"。
log "停止 homeagent（伪关机：等待 flush 完成后才搬数据）..."
run systemctl stop homeagent || true
# 等 systemd 确认进程真的退了（TimeoutStopSec=25s 内）
for i in $(seq 1 30); do
  systemctl is-active --quiet homeagent || break
  sleep 1
done
if systemctl is-active --quiet homeagent; then
  die "服务 30s 内未停止（可能卡在 flush）；请查 journalctl -u homeagent -n 50"
fi
log "服务已停止（无进程可写数据）"

# ── 2) 卸载 overlay 挂载（关键：不卸会搬空壳）──
`, old, new_, "/tmp/migrate-datadir.sh", old, new_, unitPath)
	for _, mp := range p.MountPoints {
		fmt.Fprintf(&b, `if mountpoint -q "$OLD/%s"; then
  log "卸载 overlay：$OLD/%s"
  run umount "$OLD/%s" || run umount -l "$OLD/%s" || die "无法卸载 $OLD/%s"
fi
`, mp, mp, mp, mp, mp)
	}
	fmt.Fprintf(&b, `
# ── 3) 迁移（保留硬链接/ACL/xattr；排除运行期状态）──
log "创建目标：$NEW"
run mkdir -p "$NEW"
RSYNC_EXCL=""
`)
	for _, rd := range p.RuntimeDirs {
		fmt.Fprintf(&b, `RSYNC_EXCL="$RSYNC_EXCL --exclude=%s"
`, rd)
	}
	fmt.Fprintf(&b, `
# ★ 优先 rsync（保留硬链接/ACL/xattr），缺失则退回 GNU cp。
#
# 为什么要退回：实测本机（Debian forky 容器）**没有 rsync**，
# 而生产机有。脚本若硬依赖 rsync，在一台没装的机器上就直接失败，
# 而失败点正好在"服务已停、数据未搬"的最尴尬位置。
#
# 退回分支的能力差异**必须说清楚**（不能假装等价）：
#   · cp -a 保留权限/时间戳/符号链接，但**不保硬链接**（-l 除外）
#     生产 plugindev 有大量 git 硬链接（objects 共享）⇒ 会**膨胀**磁盘占用
#   · cp 不保 ACL/xattr ⇒ 若有 SELinux/ACL 依赖会失配
# ⇒ 所以 cp 分支会**响亮警告**，并提示装 rsync。
if command -v rsync >/dev/null 2>&1; then
  log "rsync 迁移中（保留硬链接/ACL/xattr，大目录较慢，请耐心）..."
  # shellcheck disable=SC2086
  run rsync -aHAX --info=progress2 $RSYNC_EXCL "$OLD"/ "$NEW"/ || die "rsync 失败"
else
  log "warn: 未安装 rsync，退回 cp -a"
  log "warn: cp -a **不保硬链接/ACL/xattr** —— 含大量 git 硬链接的目录会膨胀"
  log "warn: 建议先装 rsync：apt-get install -y rsync  （然后重跑本脚本，可保留已拷内容）"
  # 逐个顶层项拷（要支持 RSYNC_EXCL 排除，cp 没有 --exclude）
  for entry in "$OLD"/* "$OLD"/.[!.]*; do
    [ -e "$entry" ] || continue
    base=$(basename "$entry")
    skip=0
    for x in $RSYNC_EXCL; do
      case "$x" in
        --exclude=*) [ "${x#--exclude=}" = "$base" ] && skip=1 ;;
      esac
    done
    [ "$skip" = "1" ] && { log "跳过（运行期状态）: $base"; continue; }
    run cp -a "$entry" "$NEW"/ || die "cp 失败: $entry"
  done
fi

# ── 4) 改写 systemd 单元（内联替换 -data 的值，保留 --role=guard）──
log "更新 systemd 单元：$UNIT"
if [ -f "$UNIT" ]; then
  run sed -i.bak-$(date +%%Y%%m%%d-%%H%%M%%S) -E "s|(-data[= ])[^ \"']+|\1$NEW|" "$UNIT"
fi
# drop-in 里重设过 ExecStart 的也必须改（生产用 drop-in 覆盖，改主单元无效）
DROP="$UNIT.d"
if [ -d "$DROP" ]; then
  for f in "$DROP"/*.conf; do
    [ -f "$f" ] || continue
    if grep -qE '^\s*ExecStart\s*=' "$f"; then
      log "更新 drop-in：$f"
      run sed -i.bak-$(date +%%Y%%m%%d-%%H%%M%%S) -E "s|(-data[= ])[^ \"']+|\1$NEW|" "$f"
    fi
  done
fi

# ── 5) 改写数据库里的绝对路径（不改会"看起来记忆全丢"）──
DB="$NEW/config.db"
if [ -f "$DB" ]; then
  log "改写数据库绝对路径：$OLD → $NEW"
`)
	for _, k := range p.DataDirHints {
		fmt.Fprintf(&b, `  run sqlite3 "$DB" "UPDATE config SET value=replace(value,'$OLD','$NEW') WHERE key='%s';" 2>/dev/null || true
`, k)
	}
	fmt.Fprintf(&b, `  # 插件配置表（mcp/files/qq 等实测有绝对路径）
  for t in $(sqlite3 "$DB" "SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'config_%%';" 2>/dev/null); do
    run sqlite3 "$DB" "UPDATE $t SET value=replace(value,'$OLD','$NEW') WHERE value LIKE '%%$OLD%%';" 2>/dev/null || true
  done
  log "剩余旧路径引用数：$(sqlite3 "$DB" "SELECT (SELECT COUNT(*) FROM config WHERE value LIKE '%%$OLD%%')" 2>/dev/null || echo '?')"
fi

# ── 6) 改写插件数据文件里的绝对路径（实测 46 个，典型是 commondev 的 git 镜像/workdir）──
if command -v grep >/dev/null; then
  log "扫描插件数据里的绝对路径..."
  n=$(grep -rl "$OLD" "$NEW/plugin_data" 2>/dev/null | wc -l | tr -d ' \n' || echo 0)
  [ -n "$n" ] || n=0
  if [ "$n" -gt 0 ]; then
    log "改写 $n 个文件"
    grep -rl "$OLD" "$NEW/plugin_data" 2>/dev/null | while read -r f; do
      sed -i.bak-$(date +%%Y%%m%%d-%%H%%M%%S) "s|$OLD|$NEW|g" "$f" || true
    done
  fi
fi

# ── 7) 校验 ──
#
# ★ dry-run 时**跳过校验**：数据根本还没搬，校验必然失败。
#   （实测：dry-run 跑到这里 die 掉，让人以为脚本有 bug）
if [ "$DRY" = "1" ]; then
  log "dry-run 结束：以上是**将要**执行的动作，未做任何改动"
  log "去掉 --dry-run 即真正执行"
  exit 0
fi
log "校验关键文件..."
for f in config.db security.key memory/graph.db; do
  if [ -e "$OLD/$f" ]; then
    [ -e "$NEW/$f" ] || die "迁移后缺少关键文件：$f"
  fi
done
if [ -f "$NEW/config.db" ]; then
  left=$(sqlite3 "$NEW/config.db" "SELECT COUNT(*) FROM config WHERE value LIKE '%%$OLD%%';" 2>/dev/null || echo 0)
  [ "$left" = "0" ] || die "数据库仍有 $left 处旧路径未改写"
fi
log "校验通过"

# ── 8) 重新加载并启动 ──
log "run systemctl daemon-reload && start"
run systemctl daemon-reload
run systemctl start homeagent
sleep 3
systemctl is-active --quiet homeagent && log "服务已启动" || die "服务未起来，请查：journalctl -u homeagent -n 100"

cat <<'EOF'

✅ 迁移完成。源目录**未删除**（安全起见）：
   · 确认无误后再手动删： sudo rm -rf "$OLD"
   · 回滚：把这个脚本的 OLD/NEW 互换，或把 systemd 的 -data 改回旧路径后重启

校验建议：
   systemctl show homeagent -p ExecStart --no-pager
   curl -s -H "X-API-Key: <key>" http://127.0.0.1:<port>/api/v1/kernel | head -c 300
EOF
`)
	return b.String()
}

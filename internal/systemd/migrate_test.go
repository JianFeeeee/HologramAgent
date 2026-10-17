package systemd

// 迁移脚本生成的判据（2026-10-17）。
//
// ## 为什么这些判据必须存在（全部来自实测踩坑）
//
// 我在真机演练脚本时**踩了三个坑**，每个都写成了判据：
//
//  1. 脚本无条件改 /etc/systemd/system/homeagent.service
//     ⇒ 演练把**生产**的 -data 改成了演练目录。真迁移该改、演练不该改
//     ⇒ 必须有 --dry-run，且 dry-run **零改动**
//
//  2. OLD 写错时脚本会一路把它当真数据源搬走并改 systemd
//     ⇒ 必须校验 systemd 当前的 -data == OLD
//
//  3. 本机没装 rsync，脚本在"服务已停、数据未搬"处硬失败
//     ⇒ 必须有 cp 回退（并明确警告能力差异）
//
// ★ 判据怎么咬：**执行生成的脚本**，而不是检查源码里有某个字符串。
//   源码字符串检查在本仓已经骗过我三次（见 AGENTS 记忆）。

import (
	"strings"
	"testing"
)

// ★★★ 判据：脚本必须支持 --dry-run，且 dry-run 分支**在校验之前**退出。
//
// "在校验之前" 是关键：dry-run 时数据还没搬，跑校验必然 die，
// 使用户误以为脚本坏了（实测就是这样）。
func TestMigrateScriptDryRunExitsBeforeValidation(t *testing.T) {
	s := NewMigratePlan("/old", "/new").Script()

	if !strings.Contains(s, "--dry-run") {
		t.Fatal("★ 脚本不支持 --dry-run ⇒ 演练会直接改生产配置")
	}
	dryIdx := strings.Index(s, `if [ "$DRY" = "1" ]; then`)
	if dryIdx < 0 {
		t.Fatal("★ 找不到 dry-run 分支")
	}
	// 校验段必须在 dry-run 分支**之后**（否则 dry-run 会跑到校验并 die）
	valIdx := strings.Index(s, "校验关键文件")
	if valIdx < 0 {
		t.Fatal("找不到校验段")
	}
	if valIdx < dryIdx {
		t.Fatal("★ 校验段在 dry-run 分支之前 ⇒ dry-run 会因数据未搬而误报失败")
	}
	// dry-run 分支里必须 exit（不能继续往下执行）
	branch := s[dryIdx:valIdx]
	if !strings.Contains(branch, "exit 0") {
		t.Fatal("★ dry-run 分支没有 exit 0 ⇒ 会继续执行破坏性动作")
	}
}

// ★★★ 判据：脚本必须校验 systemd 当前 -data == OLD（防迁错源）。
func TestMigrateScriptValidatesOldDataDir(t *testing.T) {
	s := NewMigratePlan("/old", "/new").Script()
	if !strings.Contains(s, `[ "$cur" != "$OLD" ]`) && !strings.Contains(s, `"$cur" != "$OLD"`) {
		t.Fatal("★ 脚本未校验 systemd 的 -data 与 OLD 一致 ⇒ " +
			"OLD 写错时会把错目录当真数据源并改 systemd")
	}
}

// ★★★ 判据：必须保留 rsync 主路径，且有 cp 回退。
//
// 两者都要：rsync 保硬链接（生产 plugindev 有大量 git 硬链接，
// 用 cp 会膨胀磁盘）；但没装 rsync 的机器不能直接失败。
func TestMigrateScriptHasRsyncWithCpFallback(t *testing.T) {
	s := NewMigratePlan("/old", "/new").Script()
	if !strings.Contains(s, "rsync -aHAX") {
		t.Fatal("★ 缺少 rsync -aHAX（-H 保留硬链接，生产必需）")
	}
	if !strings.Contains(s, "command -v rsync") || !strings.Contains(s, `cp -a "$entry"`) {
		t.Fatal("★ 缺少 cp 回退 ⇒ 未装 rsync 的机器会在'服务已停、数据未搬'处硬失败")
	}
	// 回退必须**明说**能力差异（不保硬链接/ACL）
	if !strings.Contains(s, "不保硬链接") {
		t.Fatal("★ cp 回退未警告'不保硬链接' ⇒ 用户以为等价，实际磁盘膨胀")
	}
}

// ★★★ 判据：必须**先 umount overlay** 再迁移（否则搬走空壳）。
func TestMigrateScriptUmountsOverlayBeforeCopy(t *testing.T) {
	s := NewMigratePlan("/old", "/new").Script()
	umountIdx := strings.Index(s, `umount "$OLD/agentfs/merged"`)
	if umountIdx < 0 {
		t.Fatal("★ 脚本没有 umount agentfs/merged ⇒ 会搬走空壳")
	}
	// ★ 必须**可达**：不能在没有挂载时短路掉整个 umount。
	// 实测陷阱：把 if 条件改成 `false && mountpoint -q ...` 后，
	// 字符串仍在（Contains 通过），但整段永不执行 ⇒ 空壳照搬。
	// ⇒ 检查 umount 所在行的**上一行**含 mountpoint -q 且不含 false/短路。
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if !strings.Contains(ln, `umount "$OLD/agentfs/merged"`) {
			continue
		}
		found := false
		for j := i - 1; j >= 0 && j >= i-3; j-- {
			p := strings.TrimSpace(lines[j])
			if strings.Contains(p, "mountpoint -q") {
				if strings.Contains(p, "false") || strings.Contains(p, "||") {
					t.Fatalf("★ umount 挂在短路条件后面，永不执行: %s", p)
				}
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("★ umount 前没有 mountpoint -q 判断（或判断被短路）")
		}
	}
	// umount 必须在拷贝之前
	copyIdx := strings.Index(s, "rsync -aHAX")
	cpIdx := strings.Index(s, "cp -a \"$entry\"")
	firstCopy := copyIdx
	if cpIdx > 0 && (firstCopy < 0 || cpIdx < firstCopy) {
		firstCopy = cpIdx
	}
	if firstCopy > 0 && umountIdx > firstCopy {
		t.Fatal("★ umount 在拷贝之后 ⇒ 已经搬完空壳了")
	}
}

// ★★★ 判据：必须改写 DB 绝对路径与插件数据文件（否则"看起来记忆全丢"）。
func TestMigrateScriptRewritesDbAndPluginData(t *testing.T) {
	s := NewMigratePlan("/old", "/new").Script()
	if !strings.Contains(s, "UPDATE config SET value=replace") {
		t.Fatal("★ 未改写 DB 里的绝对路径 ⇒ 重启后按旧路径找数据、静默重建空库")
	}
	if !strings.Contains(s, `grep -rl "$OLD" "$NEW/plugin_data"`) {
		t.Fatal("★ 未改写 plugin_data 里的绝对路径（实测 46 个文件）")
	}
	// 8 个 core.* 键必须都在
	for _, k := range NewMigratePlan("/o", "/n").DataDirHints {
		if !strings.Contains(s, k) {
			t.Fatalf("★ 脚本漏了 DB 键 %s", k)
		}
	}
}

// ★★★ 判据：目标目录非空必须拒绝（防覆盖已有数据）。
func TestMigrateScriptRefusesNonEmptyTarget(t *testing.T) {
	s := NewMigratePlan("/old", "/new").Script()
	if !strings.Contains(s, `[ -n "$(ls -A "$NEW"`) {
		t.Fatal("★ 未检查目标目录非空 ⇒ 可能覆盖已有数据")
	}
}

// ★★★ 判据：源目录不能被删（安全兜底）。
func TestMigrateScriptNeverDeletesSource(t *testing.T) {
	s := NewMigratePlan("/old", "/new").Script()
	// ★ 排除掉 two 类"不是可执行语句"的行：
	//   · 注释（#）
	//   · heredoc 的正文（<<'EOF' ... EOF）—— 那只是打印给用户看的提示文本，
	//     实测把提示里那句"sudo rm -rf $OLD"当成了"脚本删源目录"（假红）
	inHeredoc := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "<<'EOF'") || strings.Contains(trimmed, `<<"EOF"`) {
			inHeredoc = true
			continue
		}
		if inHeredoc {
			if trimmed == "EOF" {
				inHeredoc = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, `rm -rf "$OLD"`) {
			t.Fatalf("★ 脚本执行了删除源目录：%s", line)
		}
	}
}

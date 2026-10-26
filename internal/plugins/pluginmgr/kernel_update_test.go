package pluginmgr

import (
	"strings"
	"testing"
)

// ── plugin_update / kernel_update 的入口层判据 ────────────────
//
// 这里只测**入口层**（参数解析、范围选择、结果形状）；
// 底层逻辑（Check / Upgrade / DoUpgrade）的判据在各自包里：
//   internal/update   — 拉源码、编译、版本比较
//   internal/update  — 版本比较、解包、替换、回滚
// 入口层若长出业务逻辑，就违背了"三端共享一个底层"的架构约束。

// 判据 1：plugin_update 检查阶段（apply=false）不要求 plugins 参数，
// 且结果含 updatable / no_repo 两类 —— 两者必须分开。
//
// 混在一起的后果：用户无法区分"没更新"和"根本不能自动更新"（内置插件）。
func TestPluginUpdate_CheckShape(t *testing.T) {
	p := &Plugin{pluginDir: t.TempDir()} // 空目录：没有已装插件
	out, err := p.pluginUpdate(false, "")
	if err != nil {
		t.Fatalf("检查不应失败: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("返回应为 map，实际 %T", out)
	}
	if m["apply"] != false {
		t.Errorf("apply 应为 false，实际 %v", m["apply"])
	}
	if _, ok := m["updatable"]; !ok {
		t.Error("结果应有 updatable 字段")
	}
	if _, ok := m["no_repo"]; !ok {
		t.Error("结果应有 no_repo 字段（未声明仓库的插件单独列出）")
	}
}

// 判据 2：apply=true 但没给 plugins ⇒ 明确报错，不默认全升。
//
// 默认全升的危险：一次误触就把所有插件换了。
func TestPluginUpdate_ApplyRequiresScope(t *testing.T) {
	p := &Plugin{pluginDir: t.TempDir()}
	out, err := p.pluginUpdate(true, "")
	if err != nil {
		t.Fatalf("应返回可呈现的错误而不是 error: %v", err)
	}
	m := out.(map[string]interface{})
	if _, hasErr := m["error"]; !hasErr {
		t.Errorf("apply=true 缺 plugins 时必须报错，实际: %v", m)
	}
}

// 判据 3：kernel_update 检查阶段不依赖包存在，返回版本比较结果。
func TestKernelUpdate_CheckShape(t *testing.T) {
	p := &Plugin{pluginDir: t.TempDir()}
	out, err := p.kernelUpdateCheck()
	if err != nil {
		t.Fatalf("检查不应失败: %v", err)
	}
	m := out.(map[string]interface{})
	if m["apply"] != false {
		t.Errorf("apply 应为 false，实际 %v", m["apply"])
	}
	// current/latest/reason 三者在"读不到本机版本"时也要给
	if m["reason"] == nil {
		t.Error("结果应有 reason 字段（说明能否比较/为何不能）")
	}
}

// 判据 4：kernel_update apply 没给来源 ⇒ 明确报错（不许"猜一个默认动作"）。
//
// 旧判据要求错误里出现 homed-upgrade —— 那是"agent 不该在推理中发起长下载"
// 时期的限制。现在支持 release=true 自动取包（下载放在内核侧统一实现），
// 所以判据改为：必须说清"两种来源二选一"。
func TestKernelUpdate_ApplyRequiresSource(t *testing.T) {
	p := &Plugin{pluginDir: t.TempDir()}
	out, err := p.kernelUpdateApply("", false)
	if err != nil {
		t.Fatalf("应返回可呈现的错误: %v", err)
	}
	m := out.(map[string]interface{})
	msg := fmtSprint(m["error"])
	if !strings.Contains(msg, "pkg") {
		t.Errorf("错误应说明需要 pkg，实际: %v", msg)
	}
	if !strings.Contains(msg, "Release") && !strings.Contains(msg, "release") {
		t.Errorf("错误应说明可从 Release 自动更新，实际: %v", msg)
	}
}

// 判据 5：kernel_update apply 的 pkg 是目录/不存在 ⇒ 拒绝。
func TestKernelUpdate_ApplyRejectsBadPkg(t *testing.T) {
	p := &Plugin{pluginDir: t.TempDir()}
	out, _ := p.kernelUpdateApply(t.TempDir(), false) // 目录不是包
	m := out.(map[string]interface{})
	if _, hasErr := m["error"]; !hasErr {
		t.Errorf("目录不应被当作安装包，实际: %v", m)
	}
}

func fmtSprint(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

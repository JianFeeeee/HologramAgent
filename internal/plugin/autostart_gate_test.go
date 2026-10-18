package plugin

// 外部插件的三种启动形态（2026-10-18 补）。
//
// ## 缺陷
//
// `plugin.json` 的 `auto_start` 字段一路写得出来（hmapdev 打包会写），
// 但 **Load 的目录扫描从不读它**：`loadOne` 里只有持久禁用门与运行态门，
// 没有 auto_start 门。也就是说这个字段在外部插件上**完全不生效** ——
// 声明 `auto_start: false` 的插件照样开机自启。
//
// （内置模块那条走 kernelboot/scheduler.go 的 p.AutoStart，与外部插件是两条路，
//  所以这个缺陷只在外部插件上。）
//
// ## 三态语义（必须都成立）
//
//	自动启动       auto_start 缺省/true  + 启动扫描 ⇒ 拉起
//	启用但不自启   auto_start=false      + 启动扫描 ⇒ 跳过
//	               同上                  + plugin_start ⇒ 拉起（显式启动绕过）
//	禁用           持久禁用清单     + 任何路径 ⇒ 拒绝（连 plugin_start 也拒）
//
// ★ "启用但不自启" 必须能被 plugin_start 拉起：false 的含义是
// "别在开机时拉起我"，不是"永远不许启动"。少了这个区分，
// 按需加载的插件就再也起不来了 —— 比不声明更糟。
//
// ## 判据方式
//
// 真起一个 Registry + 临时插件目录（含 plugin.json），断言三种情况的加载结果。
// 不用字符串匹配：那是"符号存在"，不是"行为正确"。

import (
	"os"
	"path/filepath"
	"testing"

	isdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

// writeProcPlugin 造一个最小的"外部插件目录"。
//
// 只写 plugin.json：本判据关心的是**是否被允许加载**，
// 而不是加载后能不能跑（那由 proc 层判据负责）。
// loadOne 在 auto_start 门处就会返回，不会走到真正的进程启动。
func writeProcPlugin(t *testing.T, root, name string, autoStart *bool) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := `{"name":"` + name + `","version":"0.0.1","entry":"plugin.bin"`
	if autoStart != nil && !*autoStart {
		m += `,"auto_start":false`
	}
	m += "}"
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ★★★ 判据：auto_start=false 在启动扫描时被跳过，但显式启动能真的加载起来。
//
// ## 这一版为什么必须"真加载成功"
//
// 第一版只断言 loadOneOpt 返回 false/true。但 **false 有两个来源**：
//
//	① 被 auto_start 门拦住（正确）
//	② 根本没有 plugin.bin，加载失败（噪音）
//
// 两者无法区分 ⇒ 删掉 auto_start 门后判据照样"通过"（变异不咬，实测）。
//
// 修法：用 RegisterNative 注册一个**真能 Start 的**假插件，
// 这样 loadOneOpt 返回 true 就必然意味着"门没拦它"，
// 返回 false 就必然意味着"门拦了它"。语义唯一。
func TestAutoStartFalseSkipsScanButAllowsExplicitStart(t *testing.T) {
	root := t.TempDir()
	f := false
	dir := writeProcPlugin(t, root, "ondemand", &f)

	newReg := func() (*Registry, *fakeLifecyclePlugin) {
		r := NewRegistry()
		r.SetPluginDir(root)
		p := &fakeLifecyclePlugin{name: "ondemand"}
		r.RegisterNative("ondemand", func(name string, cfg map[string]interface{}) (isdk.Plugin, error) {
			p.name = name
			return p, nil
		})
		return r, p
	}

	// ① 启动扫描路径（explicitStart=false）⇒ 必须被 auto_start 门拦住
	r1, p1 := newReg()
	if r1.loadOneOpt(dir, "ondemand", false) {
		t.Fatal("★ auto_start=false 的插件在启动扫描时被加载了 —— " +
			"该字段在外部插件上不生效（这是原始缺陷）")
	}
	if p1.starts != 0 {
		t.Fatalf("★ 被拦住了但仍调用了 Start %d 次（门的位置不对）", p1.starts)
	}

	// ② 显式启动路径（explicitStart=true）⇒ 必须真的加载并 Start
	r2, p2 := newReg()
	if !r2.loadOneOpt(dir, "ondemand", true) {
		t.Fatal("★ 显式启动（plugin_start）也被 auto_start 门拦住了 —— " +
			"声明 false 是\"别开机自启\"，不是\"永远不许启动\"；" +
			"按需插件会再也起不来")
	}
	if p2.starts != 1 {
		t.Fatalf("★ 显式启动没有真的调用 Start（starts=%d）", p2.starts)
	}
}

// ★★★ 判据：auto_start 缺省（未写）⇒ 启动扫描照常加载（不能误伤存量插件）。
func TestAutoStartDefaultTrueStillLoads(t *testing.T) {
	root := t.TempDir()
	dir := writeProcPlugin(t, root, "defaulton", nil)

	r := NewRegistry()
	r.SetPluginDir(root)

	if !r.shouldAutoStart(dir, "defaulton") {
		t.Fatal("★ 未声明 auto_start 的插件被判为不自启 —— " +
			"存量插件会全部不启动（灾难性）")
	}
}

// ★★★ 判据：auto_start=true 显式声明 ⇒ 自启。
func TestAutoStartExplicitTrueLoads(t *testing.T) {
	root := t.TempDir()
	tr := true
	dir := writeProcPlugin(t, root, "explicit", &tr)

	r := NewRegistry()
	r.SetPluginDir(root)

	if !r.shouldAutoStart(dir, "explicit") {
		t.Fatal("★ auto_start=true 的插件被判为不自启")
	}
}

// ★★★ 判据：manifest 读不到时**默认自启**（宁可多启，不要静默不启动）。
func TestAutoStartMissingManifestDefaultsTrue(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nomanifest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	r.SetPluginDir(root)
	if !r.shouldAutoStart(dir, "nomanifest") {
		t.Fatal("★ 读不到 plugin.json 时判为不自启 —— " +
			"清单损坏会让插件静默不启动，比多启动一个更难查")
	}
}

package plugin

// 插件**运行态 vs 持久态**分离的判据（2026-10-16）。
//
// 设计见 docs/zh/kernel-modularization-design.md §2.6.7。
//
// ## 这组判据守的是什么
//
// 用户指出"只有 reload" —— 实测确认现有 API 全都是
// "改运行态 **顺带** 改持久态"：
//
//	DisablePlugin → cfgReg.AddDisabledPlugin     （持久禁用）
//	Enable        → cfgReg.RemoveDisabledPlugin  （解除持久禁用）
//
// 于是 agent **无法**"只是现在关掉它"而不影响开机自启。
// 这组判据要求：运行态操作**绝不**触碰持久状态。
//
// ## 判据必须测真实加载路径
//
// 本仓教训（重复 10+ 次）：只测 helper 会假绿。
// 这里用**真工厂 + 真 loadOne**（经 Registry.Load），并断言
// ① 持久配置真的没被写 ② 插件真的被停/起。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	isdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

// fakeLifecyclePlugin 记录生命周期调用（真工厂产出的插件）。
type fakeLifecyclePlugin struct {
	name   string
	starts int
	stops  int
}

func (p *fakeLifecyclePlugin) Name() string { return p.name }
func (p *fakeLifecyclePlugin) Start(s *isdk.PluginSDK) error {
	p.starts++
	return nil
}
func (p *fakeLifecyclePlugin) Stop() error {
	p.stops++
	return nil
}

// newRuntimeStateReg 建一个可真实加载 `alpha` 的 Registry（含临时数据目录）。
func newRuntimeStateReg(t *testing.T) (*Registry, string, *fakeLifecyclePlugin) {
	t.Helper()
	dir := t.TempDir()
	plgDir := filepath.Join(dir, "plugins")
	if err := os.MkdirAll(plgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	r.SetPluginDir(plgDir)
	r.SetDataDir(dir)
	// ★ 必须装一个**真** cfgReg：持久禁用状态就存在它里面，
	//   而 isDisabled 在 cfgReg==nil 时恒返回 false（判据会假绿）。
	//   这是"必须测真实入口"的又一具体落点。
	reg := internalConfig.NewConfigRegistry(filepath.Join(dir, "config.db"))
	t.Cleanup(func() { reg.Close() })
	r.SetConfigRegistry(reg)

	fp := &fakeLifecyclePlugin{name: "alpha"}
	r.RegisterNative("alpha", func(name string, cfg map[string]interface{}) (isdk.Plugin, error) {
		return fp, nil
	})
	return r, dir, fp
}

// ★★★ 判据：**StopPlugin 不写持久禁用配置**（运行态 ≠ 持久态）。
//
// 这是本组最核心的一条。若实现里误调了 DisablePlugin（或它内部的
// AddDisabledPlugin），用户下次开机会发现"我只是关了一下，怎么不启动了"。
func TestStopPluginDoesNotTouchPersistentState(t *testing.T) {
	r, dir, fp := newRuntimeStateReg(t)
	if err := r.Load(dir + "/plugins"); err != nil {
		t.Fatal(err)
	}
	if fp.starts == 0 {
		t.Fatal("前置失败：插件未被真实加载")
	}

	if err := r.StopPlugin("alpha"); err != nil {
		t.Fatalf("StopPlugin: %v", err)
	}

	// ① 真的停了
	if fp.stops == 0 {
		t.Fatal("★ StopPlugin 没有真的停掉插件（只改了标记）")
	}
	// ② 运行态门置位
	if !r.IsPluginRuntimeStopped("alpha") {
		t.Fatal("StopPlugin 后应处于运行态关闭")
	}
	// ③ ★ 持久态**未变**：IsPluginDisabled 必须仍为 false
	if r.IsPluginDisabled("alpha") {
		t.Fatal("★ StopPlugin 写了持久禁用 —— 用户下次开机会发现插件不自启了" +
			"（这正是要修的行为）")
	}
}

// ★★★ 判据：**StartPlugin 不解除持久禁用**（且被持久禁用时拒绝）。
//
// 反向的误用：若 StartPlugin 调了 Enable（它内部 RemoveDisabledPlugin），
// 就会把用户"明确禁止这个插件"的决定悄悄改掉。
func TestStartPluginRefusesPersistentlyDisabled(t *testing.T) {
	r, dir, _ := newRuntimeStateReg(t)
	if err := r.Load(dir + "/plugins"); err != nil {
		t.Fatal(err)
	}

	// 持久禁用（模拟用户的明确决定）
	if err := r.DisablePlugin("alpha", "test"); err != nil {
		t.Fatalf("DisablePlugin: %v", err)
	}
	if !r.IsPluginDisabled("alpha") {
		t.Fatal("前置失败：应已持久禁用")
	}

	// ★ StartPlugin 必须**拒绝**（不能绕过用户的明确决定）。
	//
	// ★ 断言错误**原因**，不能只看 err != nil：
	//   去掉我那句显式检查后，loadOne 里还有一道 isDisabled 门会挡住 ⇒
	//   StartPlugin 仍返回错误（"启动插件失败"）⇒ 只查非 nil 就**空转**了。
	//   必须要求原因指向"持久禁用"，才能区分"按预期拒绝"与"因别的原因失败"。
	err := r.StartPlugin("alpha")
	if err == nil {
		t.Fatal("★ StartPlugin 加载了被持久禁用的插件 —— 绕过了用户的明确决定")
	}
	if !strings.Contains(err.Error(), "持久禁用") {
		t.Fatalf("★ StartPlugin 拒绝的原因不是\"持久禁用\"（判据空转，无法区分真正原因）: %v", err)
	}
	// 且必须仍处于持久禁用（不得被这次尝试清掉）
	if !r.IsPluginDisabled("alpha") {
		t.Fatal("★ StartPlugin 悄悄清除了持久禁用标记")
	}
}

// ★★★ 判据：**StopPlugin → StartPlugin 能往返**（运行态关闭可被清除）。
func TestStopThenStartRoundTrip(t *testing.T) {
	r, dir, fp := newRuntimeStateReg(t)
	if err := r.Load(dir + "/plugins"); err != nil {
		t.Fatal(err)
	}

	if err := r.StopPlugin("alpha"); err != nil {
		t.Fatal(err)
	}
	if !r.IsPluginRuntimeStopped("alpha") {
		t.Fatal("应处于运行态关闭")
	}

	// 启动：应清门并重新加载
	startsBefore := fp.starts
	if err := r.StartPlugin("alpha"); err != nil {
		t.Fatalf("StartPlugin: %v", err)
	}
	if r.IsPluginRuntimeStopped("alpha") {
		t.Fatal("★ StartPlugin 没有清掉运行态门（下次 loadOne 会被它挡住）")
	}
	if fp.starts <= startsBefore {
		t.Fatal("★ StartPlugin 没有真的重新加载")
	}
	// 持久态全程未变
	if r.IsPluginDisabled("alpha") {
		t.Fatal("往返过程动了持久态")
	}
}

// ★★★ 判据：**运行态关闭不持久**（重启后插件正常加载）。
//
// 这是有意设计：运行态关闭 = "这一次不跑"，不是"以后都不跑"。
// 若它持久，agent 关掉一个插件后就再也没法让它随内核起来。
func TestRuntimeStopIsNotPersistent(t *testing.T) {
	r, dir, _ := newRuntimeStateReg(t)
	if err := r.Load(dir + "/plugins"); err != nil {
		t.Fatal(err)
	}
	if err := r.StopPlugin("alpha"); err != nil {
		t.Fatal(err)
	}
	if !r.IsPluginRuntimeStopped("alpha") {
		t.Fatal("前置失败")
	}

	// 模拟"重启"：新建一个 Registry（不共享任何内存状态）
	r2, dir2, fp2 := newRuntimeStateReg(t)
	_ = dir2
	r2.SetPluginDir(r.PluginDir())
	r2.RegisterNative("alpha", func(name string, cfg map[string]interface{}) (isdk.Plugin, error) {
		return fp2, nil
	})
	if r2.IsPluginRuntimeStopped("alpha") {
		t.Fatal("★ 运行态关闭跨实例保留了 —— 它必须是会话级的（重启即复位）")
	}
	// 重启后应能正常加载
	if err := r2.Load(r.PluginDir()); err != nil {
		t.Fatal(err)
	}
	if fp2.starts == 0 {
		t.Fatal("★ 重启后被运行态关闭过的插件未能加载 —— " +
			"agent 关掉它之后就再也起不来了（正是要修的毛病）")
	}
}

// ★★★ 判据：**持久禁用仍走原来的门**（不能因新门而失效）。
//
// 持久禁用优先于运行态：被持久禁用的插件即使开始未运行态关闭，也不得加载。
func TestPersistentDisableStillBlocksLoad(t *testing.T) {
	r, dir, fp := newRuntimeStateReg(t)
	if err := r.DisablePlugin("alpha", "test"); err != nil {
		t.Fatalf("DisablePlugin: %v", err)
	}
	if err := r.Load(dir + "/plugins"); err != nil {
		t.Fatal(err)
	}
	if fp.starts != 0 {
		t.Fatal("★ 持久禁用的插件被加载了（旧门失效）")
	}
}

// ★★★ 判据：**pluginmgr 与 webui 不能被关闭/禁用**（防自锁）。
//
// 为何必须在内核层面拦：agent 可以犯错，而这两个操作一旦执行，
// agent **无法从自己这侧恢复**（要启动 pluginmgr 得先有 pluginmgr）。
func TestRefuseSelfLockOnCriticalPlugins(t *testing.T) {
	r := NewRegistry()

	// ★ 判据要断言三件事，不能只看 err != nil：
	//
	// 我第一次只查 `err == nil` ⇒ **空转**：去掉守卫后 StopPlugin 仍会报
	// "plugin pluginmgr 未在运行"（因为没注册工厂），判据照样通过。
	// 错误**原因**不同却无法区分，是判据失焦。
	//
	// 正确的断言：
	//   ① 返回错误 ② 错误信息点明"拒绝"（自锁守卫的措辞）
	//   ③ **状态未被改动**（这才是自锁的实质危害）
	checkRefused := func(t *testing.T, err error, what string) {
		t.Helper()
		if err == nil {
			t.Fatalf("★ 允许%s pluginmgr —— agent 无法从自己这侧恢复", what)
		}
		if !strings.Contains(err.Error(), "拒绝") {
			t.Fatalf("★ %s 返回的错误不是自锁守卫给的（原因不明确，判据空转）: %v", what, err)
		}
	}

	checkRefused(t, r.StopPlugin("pluginmgr"), "运行态关闭")
	// ③ 实质危害：门**不得**被置位
	if r.IsPluginRuntimeStopped("pluginmgr") {
		t.Fatal("★ 自锁守卫没拦住：pluginmgr 已被标记运行态关闭（它再也启动不回来了）")
	}

	checkRefused(t, r.DisablePlugin("pluginmgr", "agent"), "持久禁用")
	// ③ 更糟的实质危害：持久禁用不得被写入
	if r.IsPluginDisabled("pluginmgr") {
		t.Fatal("★ 自锁守卫没拦住：pluginmgr 已被**持久禁用**（重启也不恢复）")
	}

	checkRefused(t, r.StopPlugin("webui"), "运行态关闭")

	// 普通插件**不受**自锁守卫影响（守卫不能不问对象）（守卫不能误伤）
	err := r.StopPlugin("someplugin")
	if err == nil {
		t.Fatal("未安装的普通插件应返回错误")
	}
	if strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("★ 自锁守卫误伤了普通插件: %v", err)
	}
}

// ★★★ 判据：**StopPlugin 摸掉注册面**（否则旧闭包指向已死实例）。
//
// 与 ReloadOne/Disable 同一处置：不摸的后果是模型仍能看到旧工具并调用，
// 只能拿到 ErrProcessExited（通道/工具永久指向死实例）。
func TestStopPluginDetachesRegistrationSurface(t *testing.T) {
	r, dir, _ := newRuntimeStateReg(t)
	if err := r.Load(dir + "/plugins"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	_, hadChannels := r.pluginChannels["alpha"]
	_, hadSDK := r.sdkRefs["alpha"]
	r.mu.Unlock()

	if err := r.StopPlugin("alpha"); err != nil {
		t.Fatal(err)
	}

	r.mu.Lock()
	_, stillLoaded := r.plugins["alpha"]
	_, stillSDK := r.sdkRefs["alpha"]
	r.mu.Unlock()
	if stillLoaded {
		t.Fatal("★ StopPlugin 后插件仍在注册表里")
	}
	if stillSDK {
		t.Fatal("★ StopPlugin 后 sdkRefs 未清 —— 旧 SDK 引用指向已死实例")
	}
	// 记录一下前置状态（避免 unused 警告同时留下诊断信息）
	t.Logf("前置: channels=%v sdk=%v", hadChannels, hadSDK)
}

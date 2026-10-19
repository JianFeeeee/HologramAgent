package plugins

import (
	"testing"

	"github.com/JianFeeeee/HomeAgent/internal/plugin"
)

// TestTimerRemovedFromKernel 是一条**负向断言**：timer 已从内核内置插件移出，
// 改由外部 hmap 插件提供。
//
// 为什么需要它：同包的 TestIntegrationAllPluginsRegister 只断言「该有的工具都在」
// （正向清单）。正向清单对「多出来」是瞎的 —— 把 internal/plugins/timer 加回 all.go、
// blank import 一恢复，那条测试依然全绿。于是「移除」这件事无从守护，只能靠人记住。
// 这条测试把「不该有的不能有」变成机器判据。
//
// 三个层面各断一次，因为它们的注册路径互相独立、任一处漏掉都能单独复活 timer：
//   - 工具（timer_set/list/cancel）：用户与 LLM 直接可见的能力面；
//   - inputch（timer）：定时器到点经它注入 agent，是中断的来源面；
//   - 工厂与元信息：plugin 注册表层面，决定 WebUI 插件列表与 IsBuiltinPlugin 判定。
//
// 注意：本包的测试二进制经 all.go 的 blank import **链入了全部内置插件**，
// 所以这里看到的注册状态就是内核的真实装配结果，不是某个手工构造的假环境。
func TestTimerRemovedFromKernel(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	// ① 工具面：timer_set / timer_list / timer_cancel 都不应再注册。
	for _, name := range []string{"timer_set", "timer_list", "timer_cancel"} {
		if env.stageHost.HasTool(name) {
			t.Errorf("工具 %q 仍被内核注册 —— timer 应已从内置插件移出", name)
		}
	}

	// ② 输入面：inputch "timer" 不应再存在（它是定时器注入中断的通道）。
	if ch, ok := env.iom.LookupInputChannel("timer"); ok {
		t.Errorf("inputch %q 仍被注册（Plugin=%q）—— timer 应已从内置插件移出",
			ch.Name, ch.Plugin)
	}

	// ③ 注册表面：工厂与元信息都不应再有 timer。
	if _, ok := plugin.GetPluginMeta("timer"); ok {
		t.Error("插件元信息 \"timer\" 仍被注册 —— timer 应已从内置插件移出")
	}
	if env.pluginReg.IsBuiltinPlugin("timer") {
		t.Error("timer 仍被判定为内置插件（存在编译期工厂）—— 应改由外部 hmap 插件提供")
	}
}

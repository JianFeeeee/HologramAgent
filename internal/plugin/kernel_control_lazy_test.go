package plugin

// 内核控制通道**晚绑定**的判据。
//
// 现场（2026-10-26 真机）：WebUI 重启按钮点击返回 503。
// 日志：POST /api/v1/kernel/restart auth=session status=503
//
// 根因不是授权，而是**时序**：webui 由 plugin-host 在装配期拉起，
// 而 main() 里 SetKernelControl 那一行还没执行 ⇒ 插件 Start 时拿到 nil。
// 靠"调整顺序"修不了（依赖图不保证 main 里的语句顺序），
// 只能让插件拿**解析器**，调用那一刻再取真实通道。

import (
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

type fakeCtl struct {
	restarts  int
	shutdowns int
}

func (f *fakeCtl) RequestKernelRestart(reason string) error {
	f.restarts++
	return nil
}
func (f *fakeCtl) RequestKernelShutdown(reason string) error {
	f.shutdowns++
	return nil
}

// ① 先给通道、后注入 ⇒ 调用时仍能成功（这是真机那条修复的核心）。
func TestLazyKernelControlWorksWhenInjectedLater(t *testing.T) {
	r := NewRegistry()
	ctl := kernelControlFor("webui", r) // 此时 registry 里还没有通道
	if ctl == nil {
		t.Fatal("按授权表 webui 应拿到控制通道（晚绑定）")
	}

	// 注入前调用 ⇒ 明确报不可用（不能静默成功）
	if err := ctl.RequestKernelRestart("before"); err == nil {
		t.Fatal("未注入时应报错，不能假装成功")
	}

	fake := &fakeCtl{}
	r.SetKernelControl(fake)
	if err := ctl.RequestKernelRestart("after"); err != nil {
		t.Fatalf("注入后应成功，实际: %v", err)
	}
	if fake.restarts != 1 {
		t.Fatalf("应调用真实通道 1 次，实际 %d", fake.restarts)
	}
}

// ② 晚绑定**不放宽授权**：未获授权的插件仍然拿不到通道。
func TestLazyKernelControlRespectsAuthorization(t *testing.T) {
	r := NewRegistry()
	if ctl := kernelControlFor("some-random-plugin", r); ctl != nil {
		t.Fatal("未授权插件不应拿到控制通道")
	}
	// pluginmgr 只有重启权限：关机必须被拒
	r.SetKernelControl(&fakeCtl{})
	pm := kernelControlFor("pluginmgr", r)
	if pm == nil {
		t.Fatal("pluginmgr 应有仅重启通道")
	}
	if err := pm.RequestKernelShutdown("x"); err == nil {
		t.Fatal("pluginmgr 不应能关机")
	}
}

var _ sdk.KernelControlAPI = lazyKernelControl{}

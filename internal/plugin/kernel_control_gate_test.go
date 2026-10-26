package plugin

// 内核控制通道的**授权边界**判据（2026-10-17）。
//
// ## 为什么这条判据必须存在
//
// 重启/关机通道能让**整个服务停下来**，而且关机后不会自己回来
// （guard 显式 systemctl stop 压制了 Restart=always）。
//
// 我做这个功能时第一版是"有就发"：`KernelControl: r.kernelCtl` 无条件
// 传给**所有**内置插件。那意味着任何一个内置插件（包括将来新加的）
// 都能一键关掉整个服务 —— 攻击面 = 全部内置插件。
//
// ⇒ 必须按插件名白名单授予，且这条边界要有判据钉住。

import (
	"context"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// fakeKernelCtl 记录收到的请求（判据只关心"能不能拿到/调用"）。
type fakeKernelCtl struct {
	restartCalls  int
	shutdownCalls int
}

func (f *fakeKernelCtl) RequestKernelRestart(reason string) error {
	f.restartCalls++
	return nil
}
func (f *fakeKernelCtl) RequestKernelShutdown(reason string) error {
	f.shutdownCalls++
	return nil
}

// ★★★ 判据：只有白名单里的插件能拿到内核控制通道。
func TestKernelControlOnlyForAllowlistedPlugins(t *testing.T) {
	r := NewRegistry()
	fake := &fakeKernelCtl{}
	r.SetKernelControl(fake)

	if got := r.kernelControlFor("webui"); got == nil {
		t.Fatal("★ webui 应拿到内核控制通道（它是有鉴权的管理界面）")
	}
	// 典型的内置插件都不该拿到**完整**通道（能关机）
	for _, name := range []string{"cmd", "files", "memo", "healthcheck", "timer"} {
		if got := r.kernelControlFor(name); got != nil {
			t.Fatalf("★ 内置插件 %q 拿到了内核控制通道 —— "+
				"任何插件都能关机整个服务，攻击面失控", name)
		}
	}

	// ★★★ pluginmgr 只拿到**仅重启**通道（kernelRestartAllowed）：
	//
	//	为什么需要它：kernel_update 已经把新二进制换好，不重启等于升级
	//	永远不生效，而 pending 标记会在下次启动时被判"未确认"并回滚。
	//	为什么不能给完整通道：关机后服务不会自己回来（压制 Restart=always），
	//	而重启几秒后 guard 会自己拉回来 —— 两者危险程度差一个量级。
	pm := r.kernelControlFor("pluginmgr")
	if pm == nil {
		t.Fatal("★ pluginmgr 应拿到**仅重启**通道（否则 kernel_update 换完二进制却不重启）")
	}
	if err := pm.RequestKernelShutdown("t"); err == nil {
		t.Fatal("★★★ pluginmgr 竟然能关机整个服务 —— 攻击面失控（只该授予重启）")
	}
	if fake.shutdownCalls != 0 {
		t.Fatalf("★ 关机请求被转发到底层通道（%d 次）—— 包装没有拦住", fake.shutdownCalls)
	}
	if err := pm.RequestKernelRestart("t"); err != nil {
		t.Fatalf("pluginmgr 应能重启: %v", err)
	}
	if fake.restartCalls != 1 {
		t.Fatalf("重启请求未到达底层通道（%d）", fake.restartCalls)
	}
	// 未知/外部插件名
	if got := r.kernelControlFor("some_third_party"); got != nil {
		t.Fatal("★ 未登记插件拿到了内核控制通道")
	}
}

// ★★★ 判据：内核未装配控制通道时，白名单插件也只拿到 nil（不 panic）。
func TestKernelControlNilWhenNotWired(t *testing.T) {
	r := NewRegistry()
	// 故意不 SetKernelControl（模拟非 systemd 环境/未启用）
	if got := r.kernelControlFor("webui"); got != nil {
		t.Fatal("★ 通道未装配时不该返回非 nil")
	}
}

// ★★★ 判据：SDK 侧 KernelControl() 未注入时返回 nil，且调用方判 nil 不 panic。
func TestSDKKernelControlIsNilSafe(t *testing.T) {
	s := sdk.New("probe", sdk.SDKConfig{})
	if kc := s.KernelControl(); kc != nil {
		t.Fatal("未注入时 KernelControl() 应为 nil")
	}
	// 注入后能拿到
	fake := &fakeKernelCtl{}
	s2 := sdk.New("probe2", sdk.SDKConfig{KernelControl: fake})
	if s2.KernelControl() == nil {
		t.Fatal("注入后应能拿到")
	}
	if err := s2.KernelControl().RequestKernelRestart("t"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if fake.restartCalls != 1 {
		t.Fatalf("fake 未收到请求（%d）", fake.restartCalls)
	}
}

var _ = context.Background

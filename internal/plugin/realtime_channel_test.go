package plugin

// 判据：插件通过**普通通道注册接口**声明 rt 能力（`ChannelDef.Realtime`）。
//
// ## 为什么走普通接口而不是新增一对便捷方法
//
// 本仓 SDK 的通道注册只有两个入口（`RegisterInputChannel` /
// `RegisterOutputChannel`），属性一律由 `ChannelDef` 承载 —— 与
// `cli` / `webui` / `remotedevice` 等所有既有插件的写法完全一致：
//
//	_ = s.RegisterInputChannel("cli", sdk.ChannelDef{})
//	s.RegisterOutputChannel("cli", 1, "CLI 终端", sdk.ChannelDef{}, handler)
//
// 加一对 `RegisterRealtimeInputChannel` / `RegisterRealtimeOutputChannel`
// 会多出**第二套写法**：同一个能力两种声明方式，必然漂移
// （一处改了、另一处没改，且不报错）。所以 rt 能力就走 `ChannelDef.Realtime`。
//
// ## 这两条判据钉的是什么
//
// SDK 声明 → 内核能力位的映射在**两个不同的地方**（输入侧在
// `ChannelRegistry.Register`，输出侧在 `Registry.regOutput`）。
// 只做一侧就会出现"inputch 声明成 rt 了、outputch 没有"⇒
// `AssignRealtime(outputch)` 被拒 ⇒ 插件作者以为"我声明了 rt"却划不上。
// 所以两侧各一条，删掉任一侧的映射都会红。

import (
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// TestRealtimeChannel_InputSideMapsFromChannelDef 钉住输入侧映射：
// 插件用 ChannelDef{Realtime:true} 声明后，登记表里的通道必须带 CapRT。
func TestRealtimeChannel_InputSideMapsFromChannelDef(t *testing.T) {
	reg := agentIO.NewChannelRegistry()

	// 走**普通**接口（不存在的便捷方法不该被使用）。
	if err := reg.Register(agentIO.InputChannel{
		Name: "voice-in", Plugin: "demo",
		Def: agentIO.ChannelDef{Realtime: true},
	}); err != nil {
		t.Fatalf("注册: %v", err)
	}

	ch, ok := reg.Lookup("voice-in")
	if !ok {
		t.Fatal("通道未登记")
	}
	if !ch.Capabilities.Supports(agentIO.CapRT) {
		t.Fatalf("Def.Realtime=true 但 Capabilities=%v 缺 CapRT —— "+
			"插件声明的 rt 通道无法被 AssignRealtime 划拨", ch.Capabilities)
	}
}

// TestRealtimeChannel_OutputSideMapsFromChannelDef 钉住输出侧映射：
// 插件用 RegisterOutputChannel(..., ChannelDef{Realtime:true}, ...) 声明后，
// 该 device 的 OutputCapabilities 必须带 CapRT。
//
// ★ 这条钉的是 `Registry.regOutput` 里的映射 —— 它和输入侧是**两处代码**。
// 删掉那一处，本判据红而输入侧那条仍绿，正好定位到"只改了一半"。
func TestRealtimeChannel_OutputSideMapsFromChannelDef(t *testing.T) {
	r := NewRegistry()
	iom := agentIO.NewIOManager()
	r.SetIOManager(iom)
	sd := r.buildSDK("demo")

	var called bool
	handler := func(args map[string]interface{}) (interface{}, error) {
		called = true
		return "ok", nil
	}

	// 走**普通** RegisterOutputChannel（与 cli/webui 等既有插件同一写法），
	// rt 能力只靠 ChannelDef.Realtime 表达。
	if err := sd.RegisterOutputChannel("voice", 1, "实时通话输出",
		sdk.ChannelDef{Realtime: true}, handler); err != nil {
		t.Fatalf("注册输出通道: %v", err)
	}

	dev := iom.GetDevice("voice")
	if dev == nil {
		t.Fatal("输出通道未注册成 device")
	}
	gotCaps := dev.OutputCapabilities()
	if !gotCaps.Supports(agentIO.CapRT) {
		t.Fatalf("ChannelDef.Realtime=true 但 OutputCapabilities=%v 缺 CapRT —— "+
			"AssignRealtime(outputch) 会被拒", gotCaps)
	}
	// caps 原本声明的文本能力不能被覆盖掉。
	if !gotCaps.Supports(agentIO.CapText) {
		t.Fatalf("补 CapRT 时把原有的 CapText 弄丢了：%v", gotCaps)
	}

	// handler 仍可执行（映射不能破坏 device 行为）。
	if _, err := dev.Execute("voice", map[string]interface{}{"text": "hi"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !called {
		t.Fatal("handler 未被调用")
	}
}

// TestRealtimeChannel_NonRealtimeUnchanged 钉住**没有**副作用：
// 不声明 Realtime 的通道，abilities 里不得凭空出现 CapRT。
//
// 这条防"补映射时顺手把所有通道都标成 rt"——那会让 rt 校验形同虚设。
func TestRealtimeChannel_NonRealtimeUnchanged(t *testing.T) {
	reg := agentIO.NewChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{
		Name: "plain", Plugin: "demo", Def: agentIO.ChannelDef{},
	}); err != nil {
		t.Fatalf("注册: %v", err)
	}
	ch, _ := reg.Lookup("plain")
	if ch.Capabilities.Supports(agentIO.CapRT) {
		t.Fatalf("未声明 Realtime 的通道却带上了 CapRT（%v）—— rt 校验会形同虚设",
			ch.Capabilities)
	}

	r2 := NewRegistry()
	iom2 := agentIO.NewIOManager()
	r2.SetIOManager(iom2)
	sd2 := r2.buildSDK("demo2")
	if err := sd2.RegisterOutputChannel("plainout", 1, "普通输出",
		sdk.ChannelDef{}, func(map[string]interface{}) (interface{}, error) { return "ok", nil }); err != nil {
		t.Fatalf("注册输出通道: %v", err)
	}
	if dev := iom2.GetDevice("plainout"); dev == nil {
		t.Fatal("device 未注册")
	} else if dev.OutputCapabilities().Supports(agentIO.CapRT) {
		t.Fatal("未声明 Realtime 的输出通道却带上了 CapRT")
	}
}

package pluginhost

// tools_ready 的发布判据。
//
// 语义边界（用户裁定）：seq 自动运行**不依赖 agent** —— 它只是一段工具调用
// 序列，返回值排队等 agent 起来处理即可。所以事件的正确时机是
// "全部插件加载完、工具都注册了"（ks.PluginsReady = true 处），
// 而不是 agent.Start() 之后。
//
// 放在 kernelmod 而不是 main：时机语义属于内核；外包给编排层会让
// 换个入口（e2e / 测试 / 别的发行版 main）就不一致。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
)

// 没有 EventBus 时必须**响亮报错而不是静默返回** ——
// 静默失败会让"为什么开机序列没跑"完全无从查起。
func TestPublishToolsReadyWithoutBusDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("publishToolsReady 在 nil EventBus 下不能 panic：%v", r)
		}
	}()
	publishToolsReady(nil)
	publishToolsReady(&kernelboot.KernelState{})
}

// 有 EventBus 时必须真的发布一条 tools_ready，且带 plugin_count。
func TestPublishToolsReadyPublishes(t *testing.T) {
	bus := events.NewBus()
	var got []*events.Event
	bus.Subscribe(events.EventToolsReady, func(e *events.Event) { got = append(got, e) })

	ks := &kernelboot.KernelState{EventBus: bus}
	publishToolsReady(ks)

	if len(got) != 1 {
		t.Fatalf("应发布恰好一条 tools_ready，实际 %d 条", len(got))
	}
	if got[0].Source != "plugin-host" {
		t.Fatalf("Source 应为 plugin-host，实际 %q", got[0].Source)
	}
	if _, ok := got[0].Payload["plugin_count"]; !ok {
		t.Fatal("payload 应含 plugin_count")
	}
}

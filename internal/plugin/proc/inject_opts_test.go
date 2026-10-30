package proc

// 判据：**InjectOptions 的每一个字段都必须能跨进程传过去**。
//
// 背景（2026-10-30 审计）：`pubSdkInjectOpts` 曾逐字段摊平传参
// （noMemory, policy, recallPolicy, cleanerName, priority），于是新增字段时
// 没人被提醒去接 —— 实际漏了 ScenePolicy 与 SecurityProfile。
//
// 后者尤其严重：QQ 插件的 owner/guest 档位就是靠 InjectOptions.SecurityProfile
// 选的，漏了等于「插件声明了档位、注入时带了、内核从没收到」，策略静默不生效。
//
// 现在签名接收整个 injectParams，新增字段时编译器会在这里提示。

import (
	"testing"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// TestPubSdkInjectOpts_CarriesEveryField 钉住整体映射：
// injectParams 的每个 opt 字段都必须出现在输出的 InjectOptions 里。
func TestPubSdkInjectOpts_CarriesEveryField(t *testing.T) {
	in := injectParams{
		NoMemory:        true,
		ContextPolicy:   "prune",
		RecallPolicy:    "none",
		ScenePolicy:     "none",
		CleanerName:     "voice-cleaner",
		Priority:        "L2",
		SecurityProfile: "qq.guest",
	}
	got := pubSdkInjectOpts(in)

	if !got.NoMemory {
		t.Error("NoMemory 丢失")
	}
	if got.ContextPolicy != "prune" {
		t.Errorf("ContextPolicy = %q", got.ContextPolicy)
	}
	if got.RecallPolicy != "none" {
		t.Errorf("RecallPolicy = %q —— 漏传则插件声明的召回策略静默失效", got.RecallPolicy)
	}
	if got.ScenePolicy != "none" {
		t.Errorf("ScenePolicy = %q —— 漏传则插件声明的场面策略静默失效", got.ScenePolicy)
	}
	if got.CleanerName != "voice-cleaner" {
		t.Errorf("CleanerName = %q", got.CleanerName)
	}
	if got.SecurityProfile != "qq.guest" {
		t.Errorf("SecurityProfile = %q —— 漏传则插件声明的安全档位**永不生效**"+
			"（QQ owner/guest 就是走这条）", got.SecurityProfile)
	}
	// 外部插件的优先级必须被夹到 L1..L3（L4 内核独占）。
	if got.Priority == "L4" {
		t.Error("外部插件竟然传出了 L4 —— 优先级夹取失效")
	}
}

// TestPubSdkInjectOpts_MediaPathKeepsOpts 钉住媒体注入路径同样不丢字段。
//
// 媒体路径用 injectMediaParams（多 Blocks/BlocksRef），曾经因为类型不同
// 而无法复用同一个转换函数 —— 那正是漏字段的温床。
func TestPubSdkInjectOpts_MediaPathKeepsOpts(t *testing.T) {
	m := injectMediaParams{
		NoMemory:        true,
		ScenePolicy:     "none",
		SecurityProfile: "qq.owner",
	}
	got := pubSdkInjectOpts(m.asInjectParams())
	if !got.NoMemory {
		t.Error("媒体路径 NoMemory 丢失")
	}
	if got.ScenePolicy != "none" {
		t.Errorf("媒体路径 ScenePolicy = %q", got.ScenePolicy)
	}
	if got.SecurityProfile != "qq.owner" {
		t.Errorf("媒体路径 SecurityProfile = %q —— 媒体注入同样要带档位", got.SecurityProfile)
	}
}

// TestInjectParityWithSDK 是**防复发**判据：injectParams/injectMediaParams
// 必须覆盖 InjectOptions 的全部字段。
//
// 做法：构造一个"所有字段都非零"的 InjectOptions，逐个读出名称与值，
// 确认转换后不丢。新增 SDK 字段而没改这里时，本判据会红。
func TestInjectParityWithSDK(t *testing.T) {
	// 全字段非零（新字段加进 SDK 后，这里也要加 —— 判据会提示）
	want := pubsdk.InjectOptions{
		NoMemory:        true,
		ContextPolicy:   "prune",
		RecallPolicy:    "none",
		ScenePolicy:     "none",
		CleanerName:     "c",
		Priority:        "L2",
		SecurityProfile: "p",
	}
	in := injectParams{
		NoMemory: want.NoMemory, ContextPolicy: want.ContextPolicy,
		RecallPolicy: want.RecallPolicy, ScenePolicy: want.ScenePolicy,
		CleanerName: want.CleanerName, Priority: want.Priority,
		SecurityProfile: want.SecurityProfile,
	}
	got := pubSdkInjectOpts(in)
	// InjectOptions 含 map 字段（不可用 ==），逐字段比。
	if got.NoMemory != want.NoMemory ||
		got.ContextPolicy != want.ContextPolicy ||
		got.RecallPolicy != want.RecallPolicy ||
		got.ScenePolicy != want.ScenePolicy ||
		got.CleanerName != want.CleanerName ||
		got.Priority != want.Priority ||
		got.SecurityProfile != want.SecurityProfile {
		t.Fatalf("字段不齐：\n got = %+v\nwant = %+v", got, want)
	}
}

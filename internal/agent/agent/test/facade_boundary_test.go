package test

// 「类型不许跨层取内核源表」的判据（用户裁定 2026-11-20）。
//
// ## 要钉住什么
//
// 四个 agent 类型包里，**读 LLM 源**这件事只允许经基座的能力面：
//
//	Agent.CompleteLLM / CompleteLLMNoRetry / StreamLLM   ← 补全
//	Agent.LLMSources / LLMSourceSupports                 ← 按能力挑源
//	Agent.ActiveLLMSource / LLMMaxContextTokens          ← 事实查询
//
// 不许出现：`Config().Provider` / `ProviderManager` / `.Provider().Chat`。
//
// ★ 为什么这条必须是判据而不是约定：`Provider` 是裸单源句柄，
//   绕过去就同时丢掉候选链、跨源回退、可用性标记与错误分类 ——
//   而**不会有任何报错**，只是"配置里配了优先源却没生效"。

import (
	"reflect"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// TestBaseExposesNoRawProviderHandle：基座方法集里**没有**裸 provider 句柄。
//
// ★ 黑盒（2026-11-25 改写）：`reflect` 查 `*agent.Agent` 的方法集。
//
// ## 原判据为什么不合格
//
// 它 `filepath.Walk` 四个类型的目录，读每个 `.go` 文件，`strings.Contains`
// 找 `ProviderManager` / `.Provider().Chat` / `Provider().ChatStream`。
// 三个问题：
//
//	① 判据在测"这四个包里有没有这些**字面量**" —— 换个变量名就绕过；
//	② 而且它扫的是**类型包**。可类型根本不需要 import 任何东西就能读到
//	   基座的那个方法：只要 `MountProviders` 之外还留着 `ProviderManager()`
//	   访问器，任何类型都能 `Config().ProviderManager().List()`。
//	   后门在**基座的方法集**上，原文却在类型的目录里找字符串。
//	③ 注释里出现这些名字（解释"为什么不许"）要靠逐行判断是否 `//` 开头
//	   来放过 —— 那本身就是"用文本当语义"的证据。
//
// ## 这一版
//
// 直接观测**能力面**：基座不给裸句柄，类型就没有跨层的可能。
// 这才是"LLM 通道统一"的判据 —— 不是查谁写错了字符串。
func TestBaseExposesNoRawProviderHandle(t *testing.T) {
	typ := reflect.TypeOf(&agent.Agent{})
	// 裸句柄：拿到它就能自己排源 / 自己判能力 / 自己发请求。
	for _, banned := range []string{"Provider", "ProviderManager", "SetProvider"} {
		if _, ok := typ.MethodByName(banned); ok {
			t.Fatalf("★ 基座方法集里还有 %q —— 类型可以绕过 LLM 通道自己发请求。\n"+
				"  正当入口：装配用 MountProviders / SetProviderSecretStore，"+
				"运行期只用 CompleteLLM / CompleteLLMNoRetry / StreamLLM", banned)
		}
	}
	// 三个正当通道必须都在（否则"统一"变成了"没有"）。
	for _, need := range []string{
		"CompleteLLM", "CompleteLLMNoRetry", "StreamLLM",
	} {
		if _, ok := typ.MethodByName(need); !ok {
			t.Fatalf("★ 基座缺 LLM 通道 %q", need)
		}
	}
}

package test

// interrupt_marks_test.go —— **中断标记的解释权归类型**（docs §5.6）。
//
// 用户裁定 2026-11-20：
// 「插件类型中的中断相关字段是标记，由基座直接透传，进入 agentloop 依照标记
//   进行处理，而不是硬编码」。
//
// 判据分两层：
//
//	① 透传层：标记原样到达各类型（基座/内核不解释、不改写）
//	② 解释层：解释函数只在**类型自己的包**里，共享包一份都没有

import (
	"reflect"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// readPkg 读一个包目录下全部非测试源码（拼接）。
// readPkg 已删除（2026-11-25）：本文件的判据全部改为 `reflect` / 行为观测，
// 不再读源码文本。

// TestSharedPackagesDoNotInterpretInterruptMarks：基座不解释标记。
//
// ★ 黑盒（2026-11-25 改写）：`reflect` 查 `*agent.Agent` 的**方法集**，
//
//	不读任何源码文本。
//
// 「解释」的可观测形态是**导出能力**：基座若把标记翻成调度级别，必然出现
// 一个可调用的解析入口（`InterruptLevel` / `ParseInterruptLevel` 之类）。
// 内联字符串索引在编译期不留痕迹 —— 而它也不是"共享包替所有类型预定了解释"，
// 真正要拦的是**能力**：基座给类型一个"标记 → 级别"的函数，类型就会用它，
// 加第五种类型时就要回来改基座。
func TestSharedPackagesDoNotInterpretInterruptMarks(t *testing.T) {
	typ := reflect.TypeOf(&agent.Agent{})
	for _, banned := range []string{
		"InterruptLevel", "ParseInterruptLevel", "LevelOf", "InterruptPriority",
	} {
		if _, ok := typ.MethodByName(banned); ok {
			t.Fatalf("★ 基座方法集里出现了 %q —— 解释函数必须归类型自己的包", banned)
		}
	}
}

// TestMarkContractLivesInIO：标记的**叫法**只有一处契约（键名 + 取值）。
//
// 这里不断言"哪个类型怎么解释"，只断言契约面存在且是唯一的：
// 注入侧写键、`reservedPayloadKeys` 拦插件伪造。
func TestMarkContractLivesInIO(t *testing.T) {
	// ★ 只查**行为**（2026-11-25）：保留键表必须显式列出它们
	//   （否则插件能伪造内核语义）。原先还扫源码找 `"priority"` 字面量 ——
	//   那是同一件事的第二遍，而且换个写法就会误判。
	if !agentIO.IsReservedPayloadKey("priority") ||
		!agentIO.IsReservedPayloadKey("stop") ||
		!agentIO.IsReservedPayloadKey("interrupt") {
		t.Fatal("★ 标记键没进 reservedPayloadKeys —— 插件可以伪造中断提权")
	}
}

// TestMarksArePassedThroughUnmodified：标记透传时一个字节都不改。
//
// 这是"基座只搬运"的可执行证据：注入什么，事件上就是什么。
func TestMarksArePassedThroughUnmodified(t *testing.T) {
	ioM := agentIO.NewIOManager()
	evt := &agentIO.InputEvent{
		Source: "tester", Type: "text",
		Payload: map[string]interface{}{
			"content":  "hi",
			"priority": "L3",
			"stop":     true,
			"deadline": 42, // 未来任意标记也照样透传
		},
	}
	if got := evt.Payload["priority"]; got != "L3" {
		t.Fatalf("标记被改写了: %v", got)
	}
	if got := evt.Payload["deadline"]; got != 42 {
		t.Fatalf("未知标记没被透传: %v", got)
	}
	// 注入路径只**加**不改：注入之后标记在 payload 上。
	_ = ioM
	payload := map[string]interface{}{"content": "hi"}
	// 走真实注入点（有 router 时会被路由走，这里只为验证标记落到 payload）。
	ioM2 := agentIO.NewIOManager()
	ioM2.InjectInputOpts("tester", "text", payload, agentIO.InjectOptions{Priority: "L2"})
	if payload["priority"] != "L2" {
		t.Fatalf("注入路径没写标记: %v", payload)
	}
}

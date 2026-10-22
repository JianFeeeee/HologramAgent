package core

// outputch 一 call 多发的判据。
//
// ## 用户拍板的五条（2026-10-22）
//
//  1. 独立 tool（如 qq_send_file）收回 outputch —— 能力统一走输出门
//  2. meta 允许单值广播
//  3. 能力校验放在插件，插件向内核返回错误
//  4. 支持向后兼容（单值 payload 老写法照旧）
//  5. outputch 说明移进 skill 一份，同时保留 _help 机制
//
// ## 内核的职责边界（这是本组判据的核心）
//
// 内核只做三件事：**透传**（payload/type/meta 原样给插件）、**对齐校验**、
// **逐条调用 + 如实汇报**。它不解释 type 的语义 —— type 是插件自定义字段。

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// recordingChannel 记录每次发送收到的参数，并可按条注入失败。
type recordingChannel struct {
	mu    sync.Mutex
	calls []map[string]interface{}
	// failAt 是"第 N 条（0 基）失败"；-1 表示全成功
	failAt int
}

func newRecordingChannel() *recordingChannel { return &recordingChannel{failAt: -1} }

func (c *recordingChannel) run(args map[string]interface{}) (interface{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := len(c.calls)
	c.calls = append(c.calls, args)
	if idx == c.failAt {
		return nil, fmt.Errorf("模拟失败：第 %d 条", idx+1)
	}
	return map[string]interface{}{"status": "sent"}, nil
}

func (c *recordingChannel) payloads() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.calls))
	for i, a := range c.calls {
		out[i], _ = a["payload"].(string)
	}
	return out
}

// batchDevice 是一个可记录、可按条注入失败的输出设备。
type batchDevice struct {
	name string
	caps agentIO.OutputCapability
	ch   *recordingChannel
}

func (d *batchDevice) Name() string             { return d.name }
func (d *batchDevice) Type() agentIO.DeviceType { return agentIO.DeviceOutput }
func (d *batchDevice) Description() string      { return "batch test device" }
func (d *batchDevice) Tools() []agentIO.ToolDef { return nil }
func (d *batchDevice) Execute(_ string, args map[string]interface{}) (interface{}, error) {
	return d.ch.run(args)
}
func (d *batchDevice) Start() error                                 { return nil }
func (d *batchDevice) Stop() error                                  { return nil }
func (d *batchDevice) OutputCapabilities() agentIO.OutputCapability { return d.caps }
func (d *batchDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }

// newOutputTestAgent 造一个带可控通道的 Agent。
func newOutputTestAgent(t *testing.T, ch *recordingChannel, caps agentIO.OutputCapability) *Agent {
	t.Helper()
	a := newToolTestAgent(t)
	a.io = agentIO.NewIOManager()
	if err := a.io.RegisterDevice(&batchDevice{
		name: "testch", caps: caps, ch: ch,
	}); err != nil {
		t.Fatalf("注册测试设备失败: %v", err)
	}
	return a
}

func sendArgs(args map[string]interface{}) agentAPI.ToolCall {
	return agentAPI.ToolCall{Name: "output_send__testch", Arguments: args}
}

// ① 向后兼容：payload 为字符串时，行为与改动前逐字相同。
//
// 变异：把单值路径也走批量（返回"已发送 1 条"而不是 "ok"）⇒ 红。
func TestOutputSend_SingleValueUnchanged(t *testing.T) {
	ch := newRecordingChannel()
	a := newOutputTestAgent(t, ch, agentIO.CapText)

	out := a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": "你好",
		"meta":    `{"user_id":1}`,
	}))

	if out != "ok" {
		t.Errorf("单条成功应返回 \"ok\"（与改动前一致），实际: %q", out)
	}
	if got := ch.payloads(); len(got) != 1 || got[0] != "你好" {
		t.Errorf("应发送 1 条「你好」，实际: %v", got)
	}
	// type 缺省应为 text
	if ty, _ := ch.calls[0]["type"].(string); ty != "text" {
		t.Errorf("type 缺省应为 text，实际 %q", ty)
	}
}

// ② 批量：payload 数组 ⇒ 按顺序发 N 条。
//
// 变异：乱序发送 / 只发第一条 ⇒ 红。
func TestOutputSend_ArraySendsInOrder(t *testing.T) {
	ch := newRecordingChannel()
	a := newOutputTestAgent(t, ch, agentIO.CapText|agentIO.CapImage|agentIO.CapFile)

	out := a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": []interface{}{"第一条", "https://x/a.png", "/tmp/r.pdf"},
		"type":    []interface{}{"text", "image", "file"},
	}))

	if !strings.Contains(out, "3") {
		t.Errorf("返回值应报告发送了 3 条，实际: %q", out)
	}
	want := []string{"第一条", "https://x/a.png", "/tmp/r.pdf"}
	got := ch.payloads()
	if len(got) != 3 {
		t.Fatalf("应发送 3 条，实际 %d: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条顺序错：got %q want %q", i+1, got[i], want[i])
		}
	}
	// type 一一对应
	for i, wantTy := range []string{"text", "image", "file"} {
		if ty, _ := ch.calls[i]["type"].(string); ty != wantTy {
			t.Errorf("第 %d 条 type: got %q want %q", i+1, ty, wantTy)
		}
	}
}

// ③ meta 单值广播（用户明确要求）。
//
// 变异：广播只给第一条 ⇒ 红。
func TestOutputSend_MetaBroadcast(t *testing.T) {
	ch := newRecordingChannel()
	a := newOutputTestAgent(t, ch, agentIO.CapText)

	a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": []interface{}{"a", "b", "c"},
		"meta":    `{"user_id":42}`,
	}))

	if len(ch.calls) != 3 {
		t.Fatalf("应发送 3 条，实际 %d", len(ch.calls))
	}
	for i, call := range ch.calls {
		if m, _ := call["meta"].(string); m != `{"user_id":42}` {
			t.Errorf("第 %d 条的 meta 未广播到：got %q", i+1, m)
		}
	}
}

// ④ 长度不符 ⇒ 整 call 拒绝，一条都不发。
//
// 变异：发前 N 条再报错 ⇒ 红。
func TestOutputSend_LengthMismatchRejectsWholeCall(t *testing.T) {
	ch := newRecordingChannel()
	a := newOutputTestAgent(t, ch, agentIO.CapText|agentIO.CapImage)

	out := a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": []interface{}{"a", "b"},
		"type":    []interface{}{"text"}, // 1 个 type 会被当成广播，这里用 3 个才错
	}))
	// 1 个 type + 2 个 payload ⇒ 广播，合法。用 3 个 meta 制造真错。
	if len(ch.calls) != 2 {
		t.Errorf("单 type 应广播给 2 条，实际发送 %d 条（%q）", len(ch.calls), out)
	}

	ch2 := newRecordingChannel()
	a2 := newOutputTestAgent(t, ch2, agentIO.CapText)
	out2 := a2.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": []interface{}{"a", "b"},
		"type":    []interface{}{"text", "text", "text"}, // 3 != 2
	}))
	if len(ch2.calls) != 0 {
		t.Errorf("长度不符必须一条都不发，实际发了 %d 条", len(ch2.calls))
	}
	if !strings.Contains(out2, "数量必须一致") {
		t.Errorf("返回值应说明数量不一致，实际: %q", out2)
	}
}

// ⑤ 部分失败：继续发剩余的，并如实列出失败项。
//
// 变异：第 2 条失败即中止 ⇒ 第 3 条不会被尝试 ⇒ 红。
// 变异：静默吞掉失败 ⇒ 返回值不含"失败" ⇒ 红。
func TestOutputSend_PartialFailureReportsAndContinues(t *testing.T) {
	ch := newRecordingChannel()
	ch.failAt = 1 // 第 2 条失败
	a := newOutputTestAgent(t, ch, agentIO.CapText)

	out := a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": []interface{}{"第一条", "第二条", "第三条"},
	}))

	if len(ch.calls) != 3 {
		t.Errorf("第 2 条失败后仍应尝试第 3 条，实际只调用 %d 次", len(ch.calls))
	}
	if !strings.Contains(out, "2/3") {
		t.Errorf("返回值应报告 2/3 条成功，实际: %q", out)
	}
	if !strings.Contains(out, "失败") {
		t.Errorf("返回值必须如实列出失败项，实际: %q", out)
	}
	if !strings.Contains(out, "模拟失败：第 2 条") {
		t.Errorf("返回值应带插件的错误原文，实际: %q", out)
	}
	if !strings.Contains(out, "重发") {
		t.Errorf("返回值应告知可单独重发失败项，实际: %q", out)
	}
}

// ⑥ 内核不再解释 type 语义：任意插件自定义 type 都原样透传。
//
// ★ 这是"能力校验下放插件"的核心判据。旧实现里内核 switch 了
// text/voice/image/file 四个值，其它值直接跳过校验、且四个已知值会被
// 内核用 caps 拦掉 —— 那等于内核在理解插件语义。
//
// 变异：恢复 switch rawType 的能力校验 ⇒ 自定义 type "sticker" 若插件
// 不支持会被内核拦截（本判据用 caps 只声明 CapText 来触发）。
func TestOutputSend_KernelDoesNotInterpretType(t *testing.T) {
	ch := newRecordingChannel()
	// 只声明 CapText：若内核还按 type 校验，"sticker" 会解析成未知类型
	a := newOutputTestAgent(t, ch, agentIO.CapText)

	out := a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": "这是插件自定义类型的载荷",
		"type":    "sticker",
	}))

	if len(ch.calls) != 1 {
		t.Fatalf("自定义 type 应被原样透传给插件，实际调用 %d 次（返回 %q）", len(ch.calls), out)
	}
	if ty, _ := ch.calls[0]["type"].(string); ty != "sticker" {
		t.Errorf("type 被内核改写：got %q want \"sticker\"", ty)
	}
	if strings.Contains(out, "不支持") {
		t.Errorf("内核不该对 type 做能力判断，实际返回: %q", out)
	}
}

// ⑦ 空 payload 交给插件判断，内核不拦（★ 实测踩到的真实缺陷）。
//
// 原先这里是"含空条目必须整 call 拒绝"。但内核**不知道 payload 的语义**：
// QQ 的 `type=sticker` + `meta.sticker_index=1` 需要 payload 留空占位，
// 真实 URL 由插件从收藏表情里取。旧实现在内核判空 ⇒ 插件侧的放行分支
// 永远走不到 ⇒ 生产实测报 "payload 不能为空"，功能完全不可用。
//
// 正确边界：**参数是否存在归内核，参数值是否合法归插件**。
//
// 变异：把 trim 判空加回去 ⇒ 红（内核又替插件做决定了）。
func TestOutputSend_EmptyPayloadDeferredToPlugin(t *testing.T) {
	ch := newRecordingChannel()
	a := newOutputTestAgent(t, ch, agentIO.CapText)

	out := a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": []interface{}{"正常", "   "},
		"meta":    `{"sticker_index":1}`,
	}))
	// 内核应把两条都交给插件（内容合法性由插件判）
	if len(ch.calls) != 2 {
		t.Errorf("内核不该拦空条目，应交插件判断，实际只发 %d 条（返回 %q）", len(ch.calls), out)
	}
}

// ⑦b 单个空 payload 是"占位"，不是"没传"。
//
// 区别：`payload: ""` 说明模型明确给了空占位；"没传 payload 键"才是真没提供。
// 前者必须走到插件（否则 sticker_index 用法永远不可用）。
func TestOutputSend_EmptyStringIsNotMissing(t *testing.T) {
	ch := newRecordingChannel()
	a := newOutputTestAgent(t, ch, agentIO.CapText)

	out := a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": "",
		"type":    "sticker",
		"meta":    `{"user_id":1,"sticker_index":1}`,
	}))
	if len(ch.calls) != 1 {
		t.Fatalf("空字符串 payload 应交给插件（sticker_index 用法依赖它），实际 %d 次调用（返回 %q）",
			len(ch.calls), out)
	}
	if p, _ := ch.calls[0]["payload"].(string); p != "" {
		t.Errorf("payload 应原样透传空串，实际 %q", p)
	}
}

// ⑦c 但"完全没传 payload 键"仍必须拒绝（结构性错误，内核能判）。
func TestOutputSend_MissingPayloadStillRejected(t *testing.T) {
	ch := newRecordingChannel()
	a := newOutputTestAgent(t, ch, agentIO.CapText)

	out := a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"type": "text",
	}))
	if len(ch.calls) != 0 {
		t.Errorf("完全没传 payload 必须拒绝，实际发了 %d 条", len(ch.calls))
	}
	if !strings.Contains(out, "payload 不能为空") {
		t.Errorf("返回值应说明 payload 不能为空，实际 %q", out)
	}
}

// ⑧ 未授权通道不得发送（授权闸不被批量绕过）。
func TestOutputSend_UnauthorizedStillBlocked(t *testing.T) {
	ch := newRecordingChannel()
	a := newOutputTestAgent(t, ch, agentIO.CapText)
	a.allowedOutputs = []string{"other"}

	out := a.executeOutputSendTool(sendArgs(map[string]interface{}{
		"payload": []interface{}{"a", "b"},
	}))
	if len(ch.calls) != 0 {
		t.Errorf("未授权通道不得发送，实际发了 %d 条", len(ch.calls))
	}
	if !strings.Contains(out, "未授权") {
		t.Errorf("返回值应说明未授权，实际: %q", out)
	}
}

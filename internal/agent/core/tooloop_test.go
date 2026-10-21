package core

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	sdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// appendPlaceholder 复刻 process() 循环顶部的补位逻辑。
func appendPlaceholder(msgs []agentAPI.Message, replyOnly bool) []agentAPI.Message {
	msgs = dropContinuationPlaceholders(msgs)
	if last := msgs[len(msgs)-1]; last.Role == "assistant" || last.Role == "tool" {
		msgs = append(msgs, agentAPI.Message{Role: "user", Content: continuationFor(replyOnly)})
	}
	return msgs
}

func countPlaceholders(msgs []agentAPI.Message) int {
	n := 0
	for _, m := range msgs {
		if isContinuationPlaceholder(m) {
			n++
		}
	}
	return n
}

// 占位是核心插入的传输层附加物，不是用户发言——它不能随轮次线性累积。
//
// 旧实现每轮无条件追加而从不移除，跑 N 轮 prompt 里就叠了 N 条一模一样的
// “继续”，把前缀上下文（含记忆注入）往后挤。
func TestPlaceholderDoesNotAccumulate(t *testing.T) {
	msgs := []agentAPI.Message{{Role: "user", Content: "用户请求"}}

	const rounds = 20
	for turn := 0; turn < rounds; turn++ {
		msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: "调用工具"})
		msgs = append(msgs, agentAPI.Message{Role: "tool", Content: "结果"})

		// 交替普通工具轮 / 纯发送轮，确保两种文案都参与去重。
		msgs = appendPlaceholder(msgs, turn%2 == 1)

		if n := countPlaceholders(msgs); n != 1 {
			t.Fatalf("第 %d 轮后占位数=%d，期望恰好 1 条（旧实现会累积到 %d 条）", turn+1, n, turn+1)
		}
	}

	// 末尾那一轮是纯发送轮，留下的应是“允许收尾”的文案。
	if last := msgs[len(msgs)-1]; last.Content != replyDeliveredPlaceholder {
		t.Fatalf("最后应是回复已交付的文案，实际: %q", last.Content)
	}
}

// 首轮 system/真实用户输入结尾不补位：补了会覆盖实际用户输入。
func TestPlaceholderNotAppendedOnFirstTurn(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "system", Content: "系统说明"},
		{Role: "user", Content: "真实用户输入"},
	}
	got := appendPlaceholder(msgs, false)
	if len(got) != 2 {
		t.Fatalf("首轮不应补位，得到 %d 条: %+v", len(got), got)
	}
	if got[1].Content != "真实用户输入" {
		t.Fatalf("真实用户输入被覆盖: %q", got[1].Content)
	}
}

// 内容相近的真实用户消息不能被当作占位删掉。
func TestDropOnlyExactPlaceholder(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "user", Content: continuationPlaceholder + "补充"},
		{Role: "user", Content: replyDeliveredPlaceholder + "补充"},
		{Role: "user", Content: continuationPlaceholder},
	}
	got := dropContinuationPlaceholders(msgs)
	if len(got) != 2 {
		t.Fatalf("只应删掉精确匹配的那条，得到 %d 条: %+v", len(got), got)
	}
}

// 纯输出通道调用之后的补位不能再是「请继续」。
//
// 异步通道的回复只能经 output_send__* 交付，所以模型「已完成回复」的形式就是
// 一个工具调用；紧跟一句「请继续」会被读成「还要再做一步」，而能做的
// 「一步」恰好还是再发一条消息。（生产实测：单轮 34 次发送、514 秒）
func TestContinuationForReplyDoesNotPushToContinue(t *testing.T) {
	reply := continuationFor(true)
	if reply == continuationPlaceholder {
		t.Fatal("回复已交付后不应再补「请继续」，会驱动重复发送")
	}
	if !strings.Contains(reply, "纯文本") || !strings.Contains(reply, "结束") {
		t.Fatalf("应明确告知可返回纯文本收尾，实际: %q", reply)
	}

	if got := continuationFor(false); got != continuationPlaceholder {
		t.Fatalf("普通工具轮补位应保持不变，实际: %q", got)
	}
}

// 只有真正的发送动作算「交付回复」；_help 是查询用法。
func TestIsOutputDeliveryTool(t *testing.T) {
	cases := map[string]bool{
		"output_send__qq":      true,
		"output_send__webui":   true,
		"output_send__qq_help": false,
		"output_list_channels": false,
		"cmd_run":              false,
		"qq_get_message":       false,
	}
	for name, want := range cases {
		if got := isOutputDeliveryTool(name); got != want {
			t.Errorf("isOutputDeliveryTool(%q) = %v, want %v", name, got, want)
		}
	}
}

// 插件在 before_toolcall 里给出的拒绝理由必须原样进入工具结果。
// 若被通用文案覆盖，模型不知道「为什么被拒 / 能不能重试」，会反复重试同一个调用。
func TestDenialReasonReachesModel(t *testing.T) {
	reason := "QQ 权限策略拒绝私人资源工具 calendar_list；请不要重试"
	ctx := &sdk.StageContext{Response: &reason}
	if got := denialResultText(ctx, "calendar_list"); got != reason {
		t.Fatalf("拒绝理由被丢弃，实际: %q", got)
	}

	// 插件没给理由时退回通用文案（保持既有行为）。
	if got := denialResultText(&sdk.StageContext{}, "calendar_list"); !strings.Contains(got, "已被插件拒绝") {
		t.Fatalf("无理由时应退回通用文案，实际: %q", got)
	}
	// 空白理由不算理由。
	blank := "   "
	if got := denialResultText(&sdk.StageContext{Response: &blank}, "x"); !strings.Contains(got, "已被插件拒绝") {
		t.Fatalf("空白理由应退回通用文案，实际: %q", got)
	}
}

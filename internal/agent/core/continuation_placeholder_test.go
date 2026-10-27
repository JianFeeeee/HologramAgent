package core

// 「本批全是输出通道发送」之后补的占位文案必须同时给出**两种授权**。
//
// 现场（2026-10-27，用户原话："那你去啊？说完就停了"）：
// 模型经 output_send__qq 说了一句"我这就去办"，内核随后补的占位只有
// 终止许可（"若你的回复已完成，直接返回纯文本即可结束本轮"）⇒ 模型照做，
// 本轮结束，活没干。这不是模型偷懒，是内核把它劝退了。
//
// 但也不能只写"继续"：注释里记着一次事故 —— 通用「请继续」被读成
// "再发一条消息"，生产实测单轮 34 次 output_send__qq、持续 514 秒。
//
// 所以判据是两条同时成立：
//   ① 明确"没做完就继续做"（允许继续调工具）
//   ② 明确"全部完成就收尾，且不要再发第二条消息"（护栏不削弱）

import (
	"strings"
	"testing"
)

func TestReplyDeliveredPlaceholderAllowsBothContinuingAndStopping(t *testing.T) {
	p := replyDeliveredPlaceholder
	if !strings.Contains(p, "没做完") || !strings.Contains(p, "继续做完") {
		t.Fatalf("占位文案必须允许「未做完就继续」，实际: %q", p)
	}
	if !strings.Contains(p, "结束本轮") {
		t.Fatalf("占位文案必须保留终止许可（否则模型会无限发送），实际: %q", p)
	}
	if !strings.Contains(p, "不要再发第二条消息") {
		t.Fatalf("占位文案必须明确禁止重复发送（防 34 连发事故复发），实际: %q", p)
	}
	if strings.Contains(p, "无需再调用任何工具") {
		t.Fatalf("不得出现「无需再调用任何工具」——那会把「承诺后未执行」也一并劝退，实际: %q", p)
	}
	if continuationFor(true) != p {
		t.Fatal("replyOnly 时必须使用本条占位")
	}
	if continuationFor(false) != continuationPlaceholder {
		t.Fatal("非 replyOnly 时必须用通用续写占位")
	}
}

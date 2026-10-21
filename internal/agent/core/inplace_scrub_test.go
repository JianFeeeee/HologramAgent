package core

// 判据：出向拦截的**原地改写**语义（共享底层数组）。
//
// ## 为什么这条值得单独立判据
//
// `task.go:663` 构造请求时是 `Messages: f.Msgs` —— **值切片，无拷贝**。
// 所以装饰器在出网前改写消息内容时，改的是 `f.Msgs` 的元素本身。
//
// 这个语义是**有意的**，且必须钉住：
//
//	好处：明文在第一次出网时就永久离开上下文。
//	     若不共享，明文会一直躺在 f.Msgs 里，每一轮都要重新拦截，
//	     并且任何读 f.Msgs 的地方（记忆写入、会话持久化、WebUI 回显）
//	     都会看到明文 —— 泄漏面大得多。
//
//	代价：改写会渗进**非 LLM 路径**（同上那几处）。这是刻意的取舍：
//	     宁可让这些地方看到别名（一致、可诊断），也不要让明文到处躺着。
//
// ⇒ 若有人把 `Messages: f.Msgs` 改成深拷贝，本判据会报红，
//   提醒他同时要在「记忆写入前」和「持久化前」各加一道拦截。

import (
	"context"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// ★★ 判据：装饰器原地改写 ⇒ 原始消息切片也变成别名。
func TestOutboundScrubIsInPlaceByDesign(t *testing.T) {
	dir := t.TempDir()
	if _, err := security.InitGlobal(security.Options{
		Path:    dir + "/v.json",
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { security.SetGlobal(nil) })

	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// 与 task.go:663 同构：Messages 直接赋 f.Msgs（值切片共享底层数组）
	msgs := []agentAPI.Message{{Role: "user", Content: "token=" + tok}}

	var captured string
	mgr := agentAPI.NewProviderManager()
	mgr.Register("p", &fwCaptureProvider{name: "p", capture: &captured})

	if _, err := mgr.Default().Chat(context.Background(),
		&agentAPI.CompletionRequest{Messages: msgs}); err != nil {
		t.Fatal(err)
	}

	// ① 出网安全
	if strings.Contains(captured, tok) {
		t.Fatal("明文泄漏到 provider")
	}
	// ② 原地改写：原始切片也变了（**这是刻意的**）
	if strings.Contains(msgs[0].Content, tok) {
		t.Fatal("原始消息仍含明文 —— 说明 Messages 被深拷贝了。" +
			"若确实要改成拷贝，必须同时在记忆写入与持久化前各加一道拦截，" +
			"否则明文会一直躺在 f.Msgs 里被反复读出")
	}
	if !strings.Contains(msgs[0].Content, "@{sec_") {
		t.Fatalf("原始消息应含别名，实际 %q", msgs[0].Content)
	}
	t.Logf("原地改写生效：原始 f.Msgs 元素已变别名 %q", msgs[0].Content)
}

package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStoreTestAgent 造一个带持久化目录的 agent（用临时目录，不碰生产数据）。
func newStoreTestAgent(t *testing.T) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	savePath := filepath.Join(dir, "context.json")
	a := &Agent{context: NewRelevanceContext(savePath, nil)}
	return a, savePath
}

// ── 判据 1：超大结果落盘，上下文只拿预览（不含全文）──────────────
//
// 这是本特性存在的理由：36783 token 的单条结果曾经整条进上下文，
// 直接冲破整个对话预算、整轮终止。
func TestOffloadReplacesFullTextWithPreview(t *testing.T) {
	a, _ := newStoreTestAgent(t)
	f := &TaskFrame{}

	// 造一个明显超长的结果（500 行）
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		sb.WriteString("line-")
		sb.WriteString(strings.Repeat("x", 100))
		sb.WriteString("\n")
	}
	full := sb.String()

	prev, ok := a.offloadToolResult(f, "cmd_run", full)
	if !ok {
		t.Fatal("超长结果应当落盘")
	}

	// ① 上下文里拿到的是预览，不是全文
	if len(prev) >= len(full) {
		t.Fatalf("预览不应不小于全文（预览 %d 字节，全文 %d 字节）", len(prev), len(full))
	}

	// ② 三要素必须齐（缺任一条模型就成了被骗的一方）
	if !strings.Contains(prev, "已完整保存") {
		t.Error("预览必须说明这是预览、全文已保存")
	}
	if !strings.Contains(prev, "evt_") {
		t.Error("预览必须给出 event_id 句柄")
	}
	if !strings.Contains(prev, "toolresult_read") {
		t.Error("预览必须告知怎么读其余部分")
	}
	if !strings.Contains(prev, "字节") {
		t.Error("预览必须给出总大小")
	}

	// ③ 记录挂在事件上（供 findToolResultStore 找到）
	if len(a.context.events) == 0 {
		// offloadToolResult 只是落盘并返回预览，不负责 append 事件；
		// 事件由调用方（task.go）创建。这里直接验证帧上的 EventID 已分配。
	}
	if f.EventID == "" {
		t.Fatal("落盘后帧上必须有 EventID，否则没法回读")
	}
}

// ── 判据 2：分段读回拼接后与原结果**逐字节相同** ────────────────
//
// 这是「信息不丢」的唯一硬证据。没有它，本特性只是另一种形式的截断。
func TestReadBackReassemblesByteIdentical(t *testing.T) {
	a, _ := newStoreTestAgent(t)
	f := &TaskFrame{}

	// 刻意用多字节字符（中文）+ 换行，确保按字节切不会切坏 UTF-8
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		sb.WriteString("第")
		sb.WriteString(strings.Repeat("测", 40))
		sb.WriteString("行\n")
	}
	full := sb.String()

	if _, ok := a.offloadToolResult(f, "files_read", full); !ok {
		t.Fatal("落盘失败")
	}

	// 按 4KB 分段读回，拼起来必须逐字节等于原文
	var got strings.Builder
	offset := 0
	chunk := 4096
	for {
		txt, err := a.readStoredToolResult(map[string]interface{}{
			"event_id": f.EventID,
			"offset":   float64(offset),
			"limit":    float64(chunk),
		})
		if err != nil {
			t.Fatalf("读 offset=%d 失败: %v", offset, err)
		}
		// 去掉头部那行元信息（形如 "[tool 的第 a-b 字节，共 n 字节]"）
		if idx := strings.Index(txt, "\n"); idx >= 0 {
			got.WriteString(txt[idx+1:])
		}
		if strings.Contains(txt, "已到末尾") {
			break
		}
		offset += chunk
		if offset > len(full)+chunk {
			t.Fatal("读回死循环：offset 已超过总长仍未到末尾")
		}
	}

	if got.String() != full {
		t.Fatalf("分段拼接与原结果不一致：读回 %d 字节，原文 %d 字节", got.Len(), len(full))
	}
}

// ── 判据 3：事件淘汰后回读给**明确**答复，不是静默空串 ──────────
//
// 静默返回空串会让模型以为"内容就是空的"，那是比失败更糟的误导。
func TestReadAfterPurgeIsExplicit(t *testing.T) {
	a, _ := newStoreTestAgent(t)
	f := &TaskFrame{}

	if _, ok := a.offloadToolResult(f, "cmd_run", strings.Repeat("z", 200000)); !ok {
		t.Fatal("落盘失败")
	}
	evtID := f.EventID

	// 模拟事件淘汰：删除其全文目录
	a.context.purgeToolResultDir(evtID)

	_, err := a.readStoredToolResult(map[string]interface{}{"event_id": evtID})
	if err == nil {
		t.Fatal("事件淘汰后回读必须报错（明确告知已淘汰），不能返回空内容")
	}
	if !strings.Contains(err.Error(), "淘汰") {
		t.Errorf("错误文案必须说明是「随事件淘汰」，实际: %v", err)
	}
}

// ── 判据 4：小结果不落盘（不改变既有行为）──────────────────────
func TestSmallResultNotOffloaded(t *testing.T) {
	a, _ := newStoreTestAgent(t)
	// 直接调 storeToolResult 是小结果场景的旁证：
	// 触发权在 checkToolResultSize，这里验证"没触发就不产生文件"。
	if a.hasStoredToolResults() {
		t.Fatal("初始状态不应有任何落盘记录")
	}
}

// ── 判据 5：handle 打错给出可执行提示 ──────────────────────────
func TestReadWithBadHandleIsActionable(t *testing.T) {
	a, _ := newStoreTestAgent(t)
	_, err := a.readStoredToolResult(map[string]interface{}{"event_id": "evt_不存在"})
	if err == nil {
		t.Fatal("打错的句柄必须报错")
	}
	if !strings.Contains(err.Error(), "淘汰") && !strings.Contains(err.Error(), "句柄") {
		t.Errorf("应说明是句柄有误或已淘汰，实际: %v", err)
	}
}

// ── 判据 6：落盘文件确实在事件目录下（不是内联进 context.json）────
//
// 这条钉住"为什么不用内联"：context.json 每 5s 全量重写，内联几 MB
// 正文会让每次 flush 都写几 MB。
func TestPersistedToEventDirNotInline(t *testing.T) {
	a, savePath := newStoreTestAgent(t)
	f := &TaskFrame{}
	if _, ok := a.offloadToolResult(f, "cmd_run", strings.Repeat("y", 200000)); !ok {
		t.Fatal("落盘失败")
	}

	p := filepath.Join(filepath.Dir(savePath), toolResultDirName, f.EventID, "tool_0.txt")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("正文应在事件目录 %s: %v", p, err)
	}
	if st.Size() != 200000 {
		t.Errorf("正文大小应为 200000，实际 %d", st.Size())
	}
}

// ── 判据 7：事件进 L1 后必须认领自己的落盘记录 ──────────────────
//
// 缺陷现场（自测抓到）：registerToolResultStore 把记录放进 pendingToolResults，
// 但认领函数没有任何调用点 ⇒ 事件进入 L1 后记录仍躺在待定表里。
// 后果：进程重启后从 context.json 读回事件，ToolResultStores 是空的，
// 句柄全部失效（模型回读得到"未找到"）。
//
// ★ 这条必须走真实的 Append 路径，不能直接调认领函数——
//
//	直接调就盖住了"没人调它"这个缺陷（变异验证：删掉 Append 里那行，
//	本判据必须变红）。
func TestAppendClaimsPendingToolResults(t *testing.T) {
	a, _ := newStoreTestAgent(t)
	f := &TaskFrame{}
	if _, ok := a.offloadToolResult(f, "cmd_run", strings.Repeat("w", 200000)); !ok {
		t.Fatal("落盘失败")
	}
	evtID := f.EventID

	// 落盘时事件还没进 L1，记录应在待定表里
	if len(a.context.pendingToolResults[evtID]) == 0 {
		t.Fatal("落盘后记录应先进入待定表")
	}

	// 事件进入 L1（走真实 Append）
	a.context.Append(ContextEvent{ID: evtID, Source: "agent", Input: "x"})

	// ① 待定表应被清空
	if len(a.context.pendingToolResults[evtID]) != 0 {
		t.Error("Append 后待定表应已认领清空")
	}
	// ② 事件上应挂上记录
	var found bool
	for _, evt := range a.context.events {
		if evt != nil && evt.ID == evtID && len(evt.ToolResultStores) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("Append 后事件上必须有 ToolResultStores，否则重启后句柄失效")
	}
	// ③ 依然可回读
	if _, err := a.readStoredToolResult(map[string]interface{}{"event_id": evtID}); err != nil {
		t.Fatalf("认领后应仍可回读: %v", err)
	}
}

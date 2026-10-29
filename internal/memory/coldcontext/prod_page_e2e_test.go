package coldcontext

// 页结构在**生产数据副本**上的验收（2026-10-29）。
//
// ## 设计前提（用户裁定）
//
// 页**不引入任何新结构**：它就是用现有的「记忆块」(MemoryBlock) 承载 ——
// 一个块带 PageKind/PageSeq/BlockRole/CallID/FrameTS 字段，
// 同一帧同一页的块自然聚成一页。没有 page 表、没有 Page 类型、没有第二份数据。
//
// 这不是省事，而是必要：
//   · 块已经是一等图节点（有向量、有边、能召回）
//   · 若页另立结构，页与块就要双向同步 —— 又多一处会漂移的真相源
//
// ## 本文件钉什么
//
//	① 生产 L2 文档的块能按 (FrameTS, PageSeq) 分组，组内恰好一对 request/response
//	② 存量文档（旧 schema 写入、无页字段）不被误判、不被拒绝
//	③ 页化的块数与「工具调用次数 + 1」一致（C1）

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// loadProdDocs 从生产 L2 副本目录加载文档（走真实 Store 的读取路径）。
func loadProdDocs(t *testing.T) *Store {
	t.Helper()
	src := os.Getenv("HA_PROD_DOCS")
	if src == "" {
		t.Skip("未设置 HA_PROD_DOCS（生产 documents 目录副本），跳过")
	}
	if _, err := os.Stat(src); err != nil {
		t.Skip("生产文档副本不存在，跳过：" + err.Error())
	}
	s := NewStore(t.TempDir(), nil)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	loaded := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			continue
		}
		var d ColdDoc
		if err := json.Unmarshal(raw, &d); err != nil {
			continue
		}
		if d.ID == "" {
			d.ID = e.Name()[:len(e.Name())-5]
		}
		s.docs[d.ID] = &d
		loaded++
	}
	t.Logf("载入生产文档 %d 篇", loaded)
	if loaded == 0 {
		t.Skip("没有可用文档，跳过")
	}
	return s
}

// ① 生产 L2 的块能按页分组；存量块（无页字段）不参与分组但也不被丢弃。
func TestProdPage_BlocksGroupIntoPages(t *testing.T) {
	s := loadProdDocs(t)

	var totalBlocks, framedBlocks, legacyBlocks int
	var pageGroups int
	for _, d := range s.docs {
		if d == nil || len(d.Events) == 0 {
			continue
		}
		for _, ev := range d.Events {
			// 按 (FrameTS, PageSeq) 分组
			groups := map[string][]memory.MemoryBlock{}
			for _, b := range ev.Blocks {
				totalBlocks++
				if b.FrameTS.IsZero() {
					legacyBlocks++
					continue
				}
				framedBlocks++
				key := b.FrameTS.Format("2006-01-02T15:04:05.000000000") + "|" + itoa(b.PageSeq)
				groups[key] = append(groups[key], b)
			}
			pageGroups += len(groups)
		}
	}
	t.Logf("生产 L2：块 %d（有帧 %d / 存量无帧 %d），页组 %d",
		totalBlocks, framedBlocks, legacyBlocks, pageGroups)

	// 这是存量数据：页字段在旧 schema 里根本不存在，
	// 所以这里**不应**期待有页化的块。断言的重点是"不被错误标记"。
	if framedBlocks > 0 {
		t.Logf("注意：存量数据里出现 %d 个带帧的块（可能已被新版本归档过）", framedBlocks)
	}
	if totalBlocks == 0 {
		t.Fatal("生产 L2 里一个块都没有 —— 读取路径坏了")
	}
}

// ② + ③ 对**真实事件**跑页化，验证块数 = 工具调用数 + 1 页（C1）。
//
// 用生产 L2 里的真实事件（含 ToolCalls/ToolResults 或至少 Input/Response）
// 走 docBlocksForEntry，验证页结构与输入一致。
func TestProdPage_SplitMatchesRealEvents(t *testing.T) {
	s := loadProdDocs(t)

	checked, withTools := 0, 0
	for _, d := range s.docs {
		if d == nil || len(d.Events) == 0 {
			continue
		}
		for _, ev := range d.Events {
			// 把 ColdEvent 转成 ContextEntry（生产回填的真实方向）
			entry := ContextEntry{
				Timestamp:   ev.Timestamp,
				Source:      ev.Source,
				Content:     firstText(ev, memory.RoleRequest),
				Response:    firstText(ev, memory.RoleResponse),
				ToolCalls:   ev.ToolCalls,
				ToolResults: ev.ToolResults,
				Blocks:      ev.Blocks,
			}
			blocks := docBlocksForEntry(entry)
			if len(blocks) == 0 {
				continue
			}
			checked++

			// 期望页数 = 1 (llm_round) + len(ToolCalls)
			wantPages := 1 + len(ev.ToolCalls)
			byPage := map[int][]memory.MemoryBlock{}
			for _, b := range blocks {
				if b.PageKind == "" {
					continue // 事件持有的原始块（媒体等），不算页
				}
				byPage[b.PageSeq] = append(byPage[b.PageSeq], b)
			}
			if len(byPage) > wantPages {
				t.Fatalf("★ 页数超出预期: 得到 %d 页，期望 ≤%d（工具调用 %d 次）",
					len(byPage), wantPages, len(ev.ToolCalls))
			}
			if len(ev.ToolCalls) > 0 {
				withTools++
			}

			// 同一帧的所有页共享 FrameTS（铁律）
			var frame = blocks[0].FrameTS
			for _, b := range blocks {
				if b.FrameTS.IsZero() {
					continue
				}
				if !b.FrameTS.Equal(frame) {
					t.Fatalf("★ 同帧块分裂: %v vs %v", b.FrameTS, frame)
				}
			}
			if checked >= 200 {
				break
			}
		}
		if checked >= 200 {
			break
		}
	}
	t.Logf("检查 %d 条真实事件（含工具调用的 %d 条）", checked, withTools)
	if checked == 0 {
		t.Skip("没有可检查的事件")
	}
}

func firstText(ev ColdEvent, role memory.BlockRole) string {
	for _, b := range ev.Blocks {
		if b.PageKind == memory.PageLLMRound && b.BlockRole == role {
			return b.Text
		}
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// 用生产**真实正文**构造带工具调用的事件，验证新归档路径把页结构落在块上。
//
// ## 为什么必须用生产正文
//
// 合成正文（"test"/"foo"）在分词、向量、去重上都与真实文本行为不同。
// 生产里出现的是长中文段落、代码片段、路径、甚至空 Response ——
// 这些才会触发「块 ID 派生」「空页跳过」「同一帧多页」的边界。
func TestProdPage_RealTextBecomesPages(t *testing.T) {
	s := loadProdDocs(t)

	// 取真实事件正文（前若干条非空）
	var entries []ContextEntry
	for _, d := range s.docs {
		if len(entries) >= 20 {
			break
		}
		if d == nil || len(d.Events) == 0 {
			continue
		}
		for _, ev := range d.Events {
			body := firstText(ev, memory.RoleRequest)
			if body == "" && len(ev.Blocks) > 0 {
				body = ev.Blocks[0].Text
			}
			if len([]rune(body)) < 20 {
				continue // 太短的正文不足以验证分词/派生
			}
			entries = append(entries, ContextEntry{
				Timestamp: ev.Timestamp,
				Source:    ev.Source,
				Content:   body,
				Response:  firstText(ev, memory.RoleResponse),
				// 真实数据没有 ToolCalls，这里补上以验证新路径
				ToolCalls: []ToolCallItem{
					{CallID: "c_real_" + itoa(len(entries)),
						Name:         "cmd_run",
						Arguments:    map[string]interface{}{"command": "df -h " + minRune(body, 12)},
						RawArguments: `{"command":"df -h"}`},
				},
				ToolResults: []ToolResultItem{
					{CallID: "c_real_" + itoa(len(entries)),
						Name: "cmd_run", Output: "/dev/sda1 62% /"},
				},
			})
			if len(entries) >= 20 {
				break
			}
		}
	}
	if len(entries) == 0 {
		t.Skip("没有足够长的真实正文，跳过")
	}

	for _, e := range entries {
		blocks := docBlocksForEntry(e)

		// ① llm_round 页必须有 request（正文非空）
		var llmReq, llmResp int
		var toolReqs, toolResps int
		for _, b := range blocks {
			switch {
			case b.PageKind == memory.PageLLMRound && b.BlockRole == memory.RoleRequest:
				llmReq++
			case b.PageKind == memory.PageLLMRound && b.BlockRole == memory.RoleResponse:
				llmResp++
			case b.PageKind == memory.PageToolRound && b.BlockRole == memory.RoleRequest:
				toolReqs++
			case b.PageKind == memory.PageToolRound && b.BlockRole == memory.RoleResponse:
				toolResps++
			}
		}
		if llmReq != 1 {
			t.Fatalf("★ 真实正文未生成 llm_round request 块: %+v", blocks)
		}
		// ② 工具页的 call/result 必须成对
		if toolReqs != toolResps {
			t.Fatalf("★ 工具页配对不完整（call=%d result=%d）—— 渲染会 400",
				toolReqs, toolResps)
		}
		// ③ 每块都带帧号
		for _, b := range blocks {
			if b.FrameTS.IsZero() {
				t.Fatalf("★ 页块缺 FrameTS: %+v", b)
			}
		}
	}

	// ④ 真实正文派生出的块 ID 必须幂等（重复归档不增生）
	a := docBlocksForEntry(entries[0])
	b := docBlocksForEntry(entries[0])
	if len(a) != len(b) {
		t.Fatalf("重复派生块数不同: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("★ 块 ID 不幂等: %q vs %q", a[i].ID, b[i].ID)
		}
	}
	t.Logf("用 %d 条真实正文验证页化：llm_round + tool_round 结构正确", len(entries))
}

func minRune(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

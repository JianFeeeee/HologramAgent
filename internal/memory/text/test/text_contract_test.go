package test

// internal/memory/text 的黑盒判据：New/Start → Append → RecentEvents/Replay/Stats。
// 用真实临时目录，不碰 m.current/m.encoder。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/text"
)

func newMemory(t *testing.T) *text.Memory {
	t.Helper()
	m := text.New(t.TempDir())
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.Stop)
	return m
}

// TestAppendThenRecentEvents：Append 后 RecentEvents 能读回内容。
func TestAppendThenRecentEvents(t *testing.T) {
	m := newMemory(t)
	if err := m.Append(text.Event{Source: "qq", Input: "你好", Response: "在"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	evts, err := m.RecentEvents(5)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("★ RecentEvents = %d 条，期望 1", len(evts))
	}
	if evts[0].Input != "你好" || evts[0].Response != "在" {
		t.Fatalf("★ 内容不符：%+v", evts[0])
	}
}

// TestReplayVisitsAppendedEvents：Replay 回调收到已写入的事件。
func TestReplayVisitsAppendedEvents(t *testing.T) {
	m := newMemory(t)
	_ = m.Append(text.Event{Source: "webui", Input: "A"})
	_ = m.Append(text.Event{Source: "webui", Input: "B"})
	inputs := map[string]bool{}
	if err := m.Replay(func(e text.Event) error {
		inputs[e.Input] = true
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !inputs["A"] || !inputs["B"] {
		t.Fatalf("★ Replay 没收到全部事件：%v", inputs)
	}
}

// TestFileCountAndStats：写后文件数/统计输出可用。
func TestFileCountAndStats(t *testing.T) {
	m := newMemory(t)
	_ = m.Append(text.Event{Source: "s", Input: "x"})
	if got := m.FileCount(); got < 1 {
		t.Fatalf("★ FileCount = %d，期望 ≥1", got)
	}
	if st := m.Stats(); st == nil {
		t.Fatal("★ Stats() 返回 nil")
	}
}

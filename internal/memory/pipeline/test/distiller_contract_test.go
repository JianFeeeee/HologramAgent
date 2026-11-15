package test

// internal/memory/pipeline 的模块级黑盒判据：Append 原始记录 → 蒸馏批次
// 经公开 SetSplitter/Start/Stop → 图记忆可查询到的三元组，以及原始记录
// 落盘/重启回读与暂停语义。不碰 d.records 等私有字段。

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/pipeline"
)

type spySplitter struct{ calls atomic.Int32 }

func (s *spySplitter) Split(_ context.Context, record string) ([]memory.Triple, error) {
	s.calls.Add(1)
	return []memory.Triple{{
		Subject:      "billing服务",
		Relation:     "端口",
		Object:       "8499",
		SentenceText: record,
	}}, nil
}

type failSplitter struct{}

func (failSplitter) Split(context.Context, string) ([]memory.Triple, error) {
	return nil, context.DeadlineExceeded
}

func newHarness(t *testing.T) (*pipeline.Distiller, *memory.GraphDB, *spySplitter) {
	t.Helper()
	dir := t.TempDir()
	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	spy := &spySplitter{}
	d := pipeline.NewDistiller(g, dir, pipeline.DistillerConfig{
		Interval: 20 * time.Millisecond, RetentionDays: 7, BatchSize: 10,
	})
	d.SetSplitter(spy)
	return d, g, spy
}

// assertFactRecalledEventually 等待「蒸馏产出可查」，而不是**立刻**断言。
//
// ## 为什么要等（一次真实的 CI 偶发）
//
// `spySplitter.Split` 里的 `calls.Add(1)` 发生在**拆分返回之前**，而三元组
// 落库是随后的 `d.db.Commit(...)`。因此「观察到 calls>0」只说明 Split 进去了，
// 不说明 Commit 回来了 —— 差多少取决于调度。
//
// 2026-11-15 CI 实测（`406e9cf` 的 Race detector 门）：
//
//	--- FAIL: TestAppendThenDistillWritesQueryableFact (0.03s)
//	    distilled fact not queryable: &{Entities:[] Relations:[]}
//
// 本地连跑 8 次全绿 —— 典型的判据自身竞态：本判据想验证的是
// 「Append → 蒸馏 → **最终**可查」这个契约，不是「Split 返回那一刻就已在库里」。
//
// ## 为什么不能改成「先等库里有、再看 calls」
//
// 那会把判据变成「等 10 秒看结果」，`calls` 就失去意义了。这里的顺序保留
// 「先确认 splitter 真的被调用过」（注入的拆分器没被调用 = 蒸馏没跑，
// 与「跑了但还没落库」是两件事），再在**有限时间**内等落库。
func assertFactRecalledEventually(t *testing.T, g *memory.GraphDB, deadline time.Time) {
	t.Helper()
	var lastGot string
	for {
		got, err := g.Recall([]string{"billing服务"}, nil, 2, "")
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		for _, r := range got.Relations {
			if r.SourceName == "billing服务" && r.RelationType == "端口" && r.TargetName == "8499" {
				return
			}
		}
		lastGot = fmt.Sprintf("%+v", got)
		if !time.Now().Before(deadline) {
			t.Fatalf("distilled fact not queryable within budget: %s", lastGot)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAppendThenDistillWritesQueryableFact(t *testing.T) {
	d, g, spy := newHarness(t)
	d.Append("s1", "user", "billing服务端口是8499")
	d.Start()
	t.Cleanup(d.Stop)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if spy.calls.Load() > 0 {
			// Split 已被调用 ⇒ 蒸馏确实跑起来了。落库是它的下一步，
			// 给一段明确预算等它完成（而不是零等待断言）。
			assertFactRecalledEventually(t, g, deadline)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("distiller never invoked the injected splitter")
}

func TestRawRecordsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := pipeline.DistillerConfig{Interval: time.Hour, RetentionDays: 7, BatchSize: 10}

	first := pipeline.NewDistiller(nil, dir, cfg)
	first.Append("s1", "user", "第一条原始记录")
	first.Start()
	first.Stop()

	second := pipeline.NewDistiller(nil, dir, cfg)
	second.Start()
	defer second.Stop()
	// Start 载入磁盘上的原始记录后，未蒸馏记录必须可读到。
	got := second.GetRecentRecords(10)
	second.Stop()
	found := false
	for _, r := range got {
		if r.Content == "第一条原始记录" && r.Role == "user" && r.SessionID == "s1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("raw record not restored after restart: %+v", got)
	}
}

func TestSplitterFailureDoesNotFabricateFacts(t *testing.T) {
	dir := t.TempDir()
	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	d := pipeline.NewDistiller(g, dir, pipeline.DistillerConfig{
		Interval: 20 * time.Millisecond, RetentionDays: 7, BatchSize: 10,
	})
	d.SetSplitter(failSplitter{})
	d.Append("s1", "user", "会失败的记录")
	d.Start()
	time.Sleep(200 * time.Millisecond)
	d.Stop()

	// 拆分器失败时必须回退而不是伪造事实：图记忆里不能出现这条记录编出来的关系。
	got, err := g.Recall([]string{"会失败的记录"}, nil, 2, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, r := range got.Relations {
		t.Fatalf("failed batch fabricated a fact: %+v", r)
	}
}

func TestPauseResumeAndStopAreObservable(t *testing.T) {
	d, _, _ := newHarness(t)
	d.Append("s1", "user", "record")
	d.Start()
	if err := d.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := d.Pause(); err != nil {
		t.Fatalf("Pause must be idempotent: %v", err)
	}
	if err := d.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if d.Stopped() {
		t.Fatal("reported stopped before Stop")
	}
	d.Stop()
	if !d.Stopped() {
		t.Fatal("Stop did not report stopped immediately")
	}
}

package test

// internal/memory/pipeline 的模块级黑盒判据：Append 原始记录 → 蒸馏批次
// 经公开 SetSplitter/Start/Stop → 图记忆可查询到的三元组，以及原始记录
// 落盘/重启回读与暂停语义。不碰 d.records 等私有字段。

import (
	"context"
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

func assertFactRecalled(t *testing.T, g *memory.GraphDB) {
	t.Helper()
	got, err := g.Recall([]string{"billing服务"}, nil, 2, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, r := range got.Relations {
		if r.SourceName == "billing服务" && r.RelationType == "端口" && r.TargetName == "8499" {
			return
		}
	}
	t.Fatalf("distilled fact not queryable: %+v", got)
}

func TestAppendThenDistillWritesQueryableFact(t *testing.T) {
	d, g, spy := newHarness(t)
	d.Append("s1", "user", "billing服务端口是8499")
	d.Start()
	t.Cleanup(d.Stop)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if spy.calls.Load() > 0 {
			assertFactRecalled(t, g)
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

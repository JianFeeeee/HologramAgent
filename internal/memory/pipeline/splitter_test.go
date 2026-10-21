package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/distill"
)

// newTestDistiller 建一个带真实图库的 Distiller（nil db 的那种测不了入库）。
func newTestDistiller(t *testing.T) (*Distiller, func()) {
	t.Helper()
	db, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	d := NewDistiller(db, t.TempDir(), DistillerConfig{Interval: time.Hour, BatchSize: 50})
	d.ctx, d.cancel = context.WithCancel(context.Background())
	return d, func() {
		d.cancel()
		db.Close()
	}
}

func nowForTest() time.Time { return time.Now() }

// relationCount 读活动关系数。
func relationCount(t *testing.T, d *Distiller) int {
	t.Helper()
	data, err := d.db.Introspect()
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if n, ok := data["relation_count"].(int); ok {
		return n
	}
	return -1
}

// stubSplitter 记录调用参数，返回预置结果。
type stubSplitter struct {
	called  int
	gotText string
	triples []memory.Triple
	err     error
	callErr error
}

func (s *stubSplitter) Split(_ context.Context, record string) ([]memory.Triple, error) {
	s.called++
	s.gotText = record
	if s.callErr != nil {
		return nil, s.callErr
	}
	return s.triples, s.err
}

// ★ splitter 注入后应被真正使用（否则接了等于没接）。
func TestDistillBatch_优先用Splitter(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()

	want := []memory.Triple{{Subject: "第183批", Relation: "停机时长", Object: "7分"}}
	stub := &stubSplitter{triples: want}
	d.SetSplitter(stub)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "sess", Role: "user",
		Content: "第183批周三凌晨1点·停机7分", CreatedAt: nowForTest()}})
	if !ok {
		t.Fatal("distillBatch 应成功")
	}
	if stub.called != 1 {
		t.Fatalf("splitter 应被调用 1 次，实际 %d", stub.called)
	}
	if !contains(stub.gotText, "停机7分") {
		t.Errorf("splitter 未收到记录内容：%q", stub.gotText)
	}
	// 三元组应真的进了图
	if n := relationCount(t, d); n == 0 {
		t.Error("splitter 产出的三元组未入库")
	}
}

// splitter 返回空但成功 = 「这条记录没有可拆字段」，应视为成功
// （蒸馏标记完成，不重试）。
func TestDistillBatch_splitter空结果视为成功(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	stub := &stubSplitter{triples: nil}
	d.SetSplitter(stub)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "s", Role: "user",
		Content: "第129~143批均仅评审通过"}})
	if !ok {
		t.Fatal("splitter 返回 0 条应视为成功（无字段可拆是正常结果）")
	}
	if stub.called != 1 {
		t.Fatalf("splitter 应被调用，实际 %d", stub.called)
	}
}

// ★ splitter 报错 → 回退 jieba 路，且 distillBatch 仍成功（不停摆）。
func TestDistillBatch_splitter失败回退(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	stub := &stubSplitter{err: errors.New("model unavailable")}
	d.SetSplitter(stub)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "s", Role: "user",
		Content: "第183批周三凌晨1点·停机7分"}})
	if !ok {
		t.Fatal("splitter 失败后应回退而非整体失败")
	}
	if stub.called != 1 {
		t.Fatalf("splitter 应被尝试过 1 次，实际 %d", stub.called)
	}
}

// 未注入 splitter 时行为不变（向后兼容）。
func TestDistillBatch_无splitter走原路径(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	d.SetSplitter(nil)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "s", Role: "user",
		Content: "第183批周三凌晨1点·停机7分"}})
	if !ok {
		t.Fatal("未注入 splitter 时应仍能成功（走既有 jieba 路）")
	}
}

// ★ 变异自证：若 extractTriples 忽略 splitter 恒走 jieba，
// 「优先用 splitter」这条测试必须变红。
func TestExtractTriples_变异_忽略splitter则失败(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	stub := &stubSplitter{triples: []memory.Triple{{Subject: "x"}}}
	d.SetSplitter(stub)

	got := d.extractTriples("第183批 停机7分", "")
	if len(got) != 1 || got[0].Subject != "x" {
		t.Fatalf("extractTriples 应返回 splitter 的结果，实际 %+v", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// blockSplitter 实现 BlockSplitter（多了 Blocks 方法）。
type blockSplitter struct {
	stubSplitter
	payload  *distill.BlockPayload
	blockErr error
	calls    int
}

func (b *blockSplitter) Blocks(_ context.Context, record string) (*distill.BlockPayload, error) {
	b.calls++
	if b.blockErr != nil {
		return nil, b.blockErr
	}
	if b.payload != nil {
		return b.payload, nil
	}
	return &distill.BlockPayload{
		Sentence: record,
		Fields: []distill.FieldBlock{
			{Dimension: "停机时长", Value: "4分"},
		},
	}, nil
}

// ★ 块路径优先：实现 BlockSplitter 时，产出应落成块（不是 entities）。
func TestDistillBatch_块路径优先(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	bs := &blockSplitter{}
	d.SetSplitter(bs)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "s", Role: "user",
		Content: "第112批 停机4分"}})
	if !ok {
		t.Fatal("distillBatch 应成功")
	}
	if bs.calls == 0 {
		t.Fatal("应走块路径")
	}
	blocks, err := d.db.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) == 0 {
		t.Fatal("块路径应写入块")
	}
	// ★ 关键：块路径**不写旧表 entities**。
	//
	// ★ 判据口径变了（2026-10-04）：原来断言「Recall 返回 0 个实体」。
	//   那是在 Recall 仍读旧表时的写法 —— Recall 切块之后它必然返回块
	//   （那正是块路径该做的事），于是判据恒红。
	//
	//   判据的**意图**是「没有走旧表双写」，所以直接查旧表本身：
	//   与实现无关，且比间接断言可靠。
	legacy, err := d.db.LegacyEntities(0, 1)
	if err != nil {
		t.Fatalf("LegacyEntities: %v", err)
	}
	if len(legacy) != 0 {
		t.Errorf("块路径不该写 entities（旧表 %d 个）: %+v", len(legacy), legacy)
	}
}

// ★ 块路失败时落回 Triple 路，而不是让整批重试。
func TestDistillBatch_块路失败落回Triple(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	bs := &blockSplitter{
		stubSplitter: stubSplitter{triples: []memory.Triple{{Subject: "s", Relation: "r", Object: "o"}}},
		blockErr:     errors.New("model timeout"),
	}
	d.SetSplitter(bs)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "s", Role: "user",
		Content: "第112批 停机4分"}})
	if !ok {
		t.Fatal("块路失败应落回 Triple 路并成功，而非整批失败（那会无限重试）")
	}
}

// 不带 BlockSplitter 能力（只有 Split）时走旧路，不 panic。
func TestDistillBatch_仅Split能力走旧路(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	st := &stubSplitter{triples: []memory.Triple{{Subject: "s", Relation: "r", Object: "o"}}}
	d.SetSplitter(st)

	if ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "s", Role: "user",
		Content: "第112批 停机4分"}}); !ok {
		t.Fatal("应成功")
	}
	if st.called == 0 {
		t.Fatal("应走 Triple 路")
	}
}

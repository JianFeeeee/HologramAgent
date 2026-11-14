package test

// internal/memory 的黑盒判据：只用导出函数/方法，输入 → 可观察输出。
// 不读源码、不碰 GraphDB 内部字段、不用 ForTest 钩子。

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// ─────────── 纯函数：输入文本 → 输出 ───────────

func TestCleanText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  你好  ", "你好"},
		{"，你好", "你好"},
		{"，，你好", "你好"}, // 连续剥两次前缀
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := memory.CleanText(c.in); got != c.want {
			t.Errorf("★ CleanText(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestChannelScene(t *testing.T) {
	cases := []struct{ in, want string }{
		{"qq", "chan:qq"},
		{"webui", "chan:webui"},
		{"", ""},
	}
	for _, c := range cases {
		if got := memory.ChannelScene(c.in); got != c.want {
			t.Errorf("★ ChannelScene(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestFilterNoiseTriples：噪音实体在主语或宾语位置都要被过滤掉。
func TestFilterNoiseTriples(t *testing.T) {
	good := memory.Triple{Subject: "billing服务", Relation: "端口", Object: "8499"}
	badSub := memory.Triple{Subject: "", Relation: "是", Object: "X"}
	badObj := memory.Triple{Subject: "X", Relation: "是", Object: "这个"}
	in := []memory.Triple{good, badSub, badObj}
	out := memory.FilterNoiseTriples(in)
	if len(out) != 1 || out[0].Subject != "billing服务" {
		t.Fatalf("★ FilterNoiseTriples = %+v，期望只留 billing 那条", out)
	}
	if len(in) != 3 {
		t.Fatal("★ 过滤修改了输入切片")
	}
}

// TestSentenceBlockIDIsStable：同文本得同一 ID。
func TestSentenceBlockIDIsStable(t *testing.T) {
	const s = "用户偏好在晚上处理构建任务"
	a := memory.SentenceBlockID(s)
	b := memory.SentenceBlockID(s)
	if a == "" || a != b {
		t.Fatalf("★ SentenceBlockID 不稳定：%q vs %q", a, b)
	}
	if c := memory.SentenceBlockID(s + " "); c == "" {
		t.Fatal("★ 带空白的句子没得到 ID")
	}
}

func TestNewSentenceBlockFields(t *testing.T) {
	now := time.Now()
	b := memory.NewSentenceBlock("  原句内容  ", now, now)
	if b.Text != "原句内容" {
		t.Fatalf("★ Text = %q，期望已 Trim", b.Text)
	}
	if b.ID != memory.SentenceBlockID("  原句内容  ") {
		t.Fatalf("★ ID = %q，与 SentenceBlockID 不一致", b.ID)
	}
}

// TestBuildCentroid：中心向量的均值必须等于输入的算术均值。
func TestBuildCentroid(t *testing.T) {
	vecs := [][]float64{{1, 2, 3}, {3, 4, 5}}
	centroid, _ := memory.BuildCentroid(vecs)
	if centroid == nil {
		t.Fatal("★ BuildCentroid 返回 nil")
	}
	mean := centroid.Mean()
	if len(mean) != 3 {
		t.Fatalf("★ Mean 维度 = %d，期望 3", len(mean))
	}
	for i, want := range []float64{2, 3, 4} {
		if mean[i] != want {
			t.Fatalf("★ Mean[%d]=%v，期望 %v", i, mean[i], want)
		}
	}
}

// ─────────── GraphDB：真实构造 → 写入 → 可观察输出 ───────────

func newDB(t *testing.T) *memory.GraphDB {
	t.Helper()
	g, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// TestCommitThenRecallAfterReopen verifies that a committed fact is durable and
// queryable through the memory module, rather than merely producing graph keys.
func TestCommitThenRecallAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	g, err := memory.NewGraphDB(path)
	if err != nil {
		t.Fatal(err)
	}
	tri := memory.Triple{
		Subject: "billing服务", Relation: "端口", Object: "8499",
		SentenceText: "billing服务端口是8499",
	}
	if _, _, err := g.Commit([]memory.Triple{tri}, "contract-session", 1); err != nil {
		_ = g.Close()
		t.Fatalf("Commit: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := memory.NewGraphDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.Recall([]string{"billing服务"}, nil, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, relation := range result.Relations {
		if relation.SourceName == tri.Subject && relation.RelationType == tri.Relation && relation.TargetName == tri.Object {
			return
		}
	}
	t.Fatalf("committed fact missing after reopen: %+v", result)
}

// TestReadOnlyHandleRejectsWrite：OpenGraphDBReadOnly 必须在 SQLite 层拒写。
//
// 这是「子 agent 改不了主记忆」的结构性保证，不是靠调用方自觉。
func TestReadOnlyHandleRejectsWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	main, err := memory.NewGraphDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()

	ro, err := memory.OpenGraphDBReadOnly(path)
	if err != nil {
		t.Fatalf("OpenGraphDBReadOnly: %v", err)
	}
	defer ro.Close()
	// 实体名要合法（否则会被 validEntityName 提前拒掉，走不到 SQL 层）
	_, _, err = ro.Commit([]memory.Triple{
		{Subject: "billing服务", Relation: "端口", Object: "8499",
			SentenceText: "billing服务端口是8499"},
	}, "contract", 1)
	if err == nil {
		t.Fatal("★ 只读句柄接受了写入 —— 子 agent 能改主记忆")
	}
}

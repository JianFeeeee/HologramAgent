package test

// internal/plugins/seq 的黑盒判据：Parse/Store 的导出接口，输入 → 输出。
// 不读序列文件内部格式、不碰 s.dir 之外的状态。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugins/seq"
)

// TestParseValidSequence：合法 JSON 解析出名字与组。
func TestParseValidSequence(t *testing.T) {
	// tools 在 JSON 里是**字符串**（若干以 ';' 分隔的 JSON 对象），
	// 不是数组 —— 这是 Parse 的真实输入契约。
	data := []byte(`{
		"name":"daily",
		"description":"每日检查",
		"boot":true,
		"groups":[{"name":"g1","tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"echo ok\"}} ;"}]
	}`)
	s, err := seq.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Name != "daily" || !s.Boot {
		t.Fatalf("★ Parse 结果不符：%+v", s)
	}
	if len(s.Groups) != 1 || s.Groups[0].Name != "g1" {
		t.Fatalf("★ Groups 丢失：%+v", s.Groups)
	}
}

// TestParseRejectsUnknownField：拼错字段必须报错，不静默丢弃。
func TestParseRejectsUnknownField(t *testing.T) {
	// 带上合法 group：否则"没有任何 group"也会报错，
	// 判据会假绿（断言的其实是任意错误，不是未知字段错误）。
	_, err := seq.Parse([]byte(`{
		"name":"x","paralll":true,
		"groups":[{"name":"g","tools":"{\"tool\":\"cmd_run\"} ;"}]
	}`))
	if err == nil {
		t.Fatal("★ 未知字段被静默接受")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("★ 报错来自别处，不是未知字段：%v", err)
	}
}

// TestParseRejectsInvalidJSON：坏 JSON 报错。
func TestParseRejectsInvalidJSON(t *testing.T) {
	if _, err := seq.Parse([]byte(`{not json`)); err == nil {
		t.Fatal("★ 坏 JSON 被接受")
	}
}

// TestStoreRoundTrip：Save → List/Load → Delete。
func TestStoreRoundTrip(t *testing.T) {
	s := seq.NewStore(t.TempDir())
	in := &seq.Sequence{Name: "nightly", Description: "夜间"}
	if err := s.Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	list := s.List()
	if len(list) != 1 || list[0] != "nightly" {
		t.Fatalf("★ List = %v，期望 [nightly]", list)
	}
	out, err := s.Load("nightly")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Name != "nightly" || out.Description != "夜间" {
		t.Fatalf("★ Load 内容不符：%+v", out)
	}
	if err := s.Delete("nightly"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("★ Delete 后 List = %v", got)
	}
}

// TestLoadMissingReportsError：读不存在的序列报错。
func TestLoadMissingReportsError(t *testing.T) {
	if _, err := seq.NewStore(t.TempDir()).Load("ghost"); err == nil {
		t.Fatal("★ 读不存在的序列却成功")
	}
}

// TestCheckNewRejectsSelfRecursion：序列不能调用自身（无限递归）。
func TestCheckNewRejectsSelfRecursion(t *testing.T) {
	s := seq.NewStore(t.TempDir())
	bad := &seq.Sequence{Name: "loop", Groups: []seq.Group{{
		Name:  "g1",
		Tools: []seq.ToolCall{{Tool: "seq_call", Args: map[string]interface{}{"target": "#loop"}}},
	}}}
	if err := s.CheckNew(bad); err == nil {
		t.Fatal("★ 自递归序列被接受")
	}
}

// TestCheckNewAcceptsValidCall：指向本序列内已存在 group 的调用应通过。
func TestCheckNewAcceptsValidCall(t *testing.T) {
	s := seq.NewStore(t.TempDir())
	ok := &seq.Sequence{Name: "chain", Groups: []seq.Group{
		{Name: "a", Tools: []seq.ToolCall{{Tool: "seq_call",
			Args: map[string]interface{}{"target": "b"}}}},
		{Name: "b"},
	}}
	if err := s.CheckNew(ok); err != nil {
		t.Fatalf("★ 合法 group 调用被拒：%v", err)
	}
}

// ---- 跨序列调用图：零覆盖 ----

// TestCheckGraphRejectsMissingTarget：调用了不存在的序列必须报错。
//
// ★ 为什么这条重要：`seq_call` 的 target 可以是**另一条序列**。写错名字
// 时如果不报错，运行时到点才失败 —— 而那可能是半夜的定时任务。
//
// 生产调用点：`handlers.go:77` 在每次 Save 之后复查一次。
func TestCheckGraphRejectsMissingTarget(t *testing.T) {
	s := seq.NewStore(t.TempDir())
	if err := s.Save(&seq.Sequence{Name: "caller", Groups: []seq.Group{{
		Name:  "g",
		Tools: []seq.ToolCall{{Tool: "seq_call", Args: map[string]interface{}{"target": "#ghost"}}},
	}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.CheckGraph(); err == nil {
		t.Fatal("★ 调用了不存在的序列却通过校验")
	} else if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("★ 报错没指出缺的是哪条（用户无从修复）：%v", err)
	}
}

// TestCheckGraphRejectsCrossSequenceCycle：两条序列互调必须被检测出来。
//
// 这是跨序列的环 —— 单条序列的自递归由 CheckNew 挡，但 A→B→A 只有
// 图级检查能看见。放任它的后果是运行时无限递归直到栈溢出。
func TestCheckGraphRejectsCrossSequenceCycle(t *testing.T) {
	s := seq.NewStore(t.TempDir())
	// A → B
	if err := s.Save(&seq.Sequence{Name: "a", Groups: []seq.Group{{
		Name:  "g",
		Tools: []seq.ToolCall{{Tool: "seq_call", Args: map[string]interface{}{"target": "#b"}}},
	}}}); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	// B → A（此刻环形成）
	if err := s.Save(&seq.Sequence{Name: "b", Groups: []seq.Group{{
		Name:  "g",
		Tools: []seq.ToolCall{{Tool: "seq_call", Args: map[string]interface{}{"target": "#a"}}},
	}}}); err != nil {
		t.Fatalf("Save b: %v", err)
	}

	err := s.CheckGraph()
	if err == nil {
		t.Fatal("★ a → b → a 的环没被检测出来（运行时会无限递归）")
	}
	// 错误必须带环路径，否则用户不知道是哪一圈
	if !strings.Contains(err.Error(), "→") {
		t.Fatalf("★ 报错没给出环路径（无法定位）: %v", err)
	}
}

// TestCheckGraphAcceptsAcyclicChain：无环的多层调用链必须通过。
//
// 与上面两条成对 —— 只测"拒绝环"会放过"拒绝一切跨序列调用"的实现。
func TestCheckGraphAcceptsAcyclicChain(t *testing.T) {
	s := seq.NewStore(t.TempDir())
	chain := []struct{ name, target string }{
		{"level1", "#level2"},
		{"level2", "#level3"},
		{"level3", ""},
	}
	for _, c := range chain {
		seqDef := &seq.Sequence{Name: c.name, Groups: []seq.Group{{Name: "g"}}}
		if c.target != "" {
			seqDef.Groups[0].Tools = []seq.ToolCall{{
				Tool: "seq_call", Args: map[string]interface{}{"target": c.target},
			}}
		}
		if err := s.Save(seqDef); err != nil {
			t.Fatalf("Save %s: %v", c.name, err)
		}
	}
	if err := s.CheckGraph(); err != nil {
		t.Fatalf("★ 无环调用链被判成非法：%v", err)
	}
}

// TestDeleteUpdatesTheGraph：删除序列后，指向它的引用必须变成"目标不存在"。
//
// ★ 这条钉的是"边随节点一起移除"。若只删文件不删边，CheckGraph 会永远
// 报一条已经不存在的序列，而这个错误再也修不掉（用户只能看到
// "调用了不存在的序列 ghost"，但 ghost 其实早已删除）。
func TestDeleteUpdatesTheGraph(t *testing.T) {
	s := seq.NewStore(t.TempDir())
	if err := s.Save(&seq.Sequence{Name: "target", Groups: []seq.Group{{Name: "g"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Save(&seq.Sequence{Name: "caller", Groups: []seq.Group{{
		Name:  "g",
		Tools: []seq.ToolCall{{Tool: "seq_call", Args: map[string]interface{}{"target": "#target"}}},
	}}}); err != nil {
		t.Fatalf("Save caller: %v", err)
	}
	if err := s.CheckGraph(); err != nil {
		t.Fatalf("保存完成后本该合法：%v", err)
	}

	if err := s.Delete("target"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 现在 caller 指向一个不存在的目标 —— 必须报出来，而不是沉默。
	err := s.CheckGraph()
	if err == nil {
		t.Fatal("★ 目标被删后，指向它的引用没有被报出来（图没随删除更新）")
	}
	if !strings.Contains(err.Error(), "target") {
		t.Fatalf("★ 报错没指出悬空引用指向谁：%v", err)
	}
}

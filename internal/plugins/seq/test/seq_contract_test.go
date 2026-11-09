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

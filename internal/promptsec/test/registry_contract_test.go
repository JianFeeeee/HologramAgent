package test

// internal/promptsec 的黑盒判据：NewRegistry → Declare → Render/Snapshot/Names。
// 输入 Section → 输出渲染文本。不碰 r.sections。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/promptsec"
)

func sec(owner, name, content string, order int) promptsec.Section {
	return promptsec.Section{Owner: owner, Section: owner + "." + name, Content: content, Order: order}
}

// TestDeclareThenRender：声明两段，Render 按 Order 输出且含正文。
func TestDeclareThenRender(t *testing.T) {
	r := promptsec.NewRegistry()
	if err := r.Declare(sec("alpha", "late", "后面这段", 20)); err != nil {
		t.Fatal(err)
	}
	if err := r.Declare(sec("beta", "early", "前面这段", 10)); err != nil {
		t.Fatal(err)
	}
	out := r.Render()
	if !strings.Contains(out, "前面这段") || !strings.Contains(out, "后面这段") {
		t.Fatalf("★ Render 缺正文：%q", out)
	}
	if strings.Index(out, "前面这段") > strings.Index(out, "后面这段") {
		t.Fatalf("★ Order 未生效：%q", out)
	}
}

// TestDeclareRejectsMismatchedOwner：段名前缀与 Owner 不符必须报错。
func TestDeclareRejectsMismatchedOwner(t *testing.T) {
	r := promptsec.NewRegistry()
	cases := []promptsec.Section{
		{Owner: "alpha", Section: "beta.x", Content: "v"},
		{Owner: "", Section: "alpha.x", Content: "v"},
		{Owner: "alpha", Section: "alpha", Content: "v"},
		{Owner: "alpha", Section: "alpha.", Content: "v"},
	}
	for _, s := range cases {
		if err := r.Declare(s); err == nil {
			t.Fatalf("★ 非法声明被接受：%+v", s)
		}
	}
	if got := r.Render(); got != "" {
		t.Fatalf("★ 非法声明后 Render 非空：%q", got)
	}
}

// TestDeclareIsIdempotent：同段名覆盖，不产生两份。
func TestDeclareIsIdempotent(t *testing.T) {
	r := promptsec.NewRegistry()
	_ = r.Declare(sec("a", "x", "旧内容", 1))
	_ = r.Declare(sec("a", "x", "新内容", 1))
	if got := r.Names(); len(got) != 1 {
		t.Fatalf("★ Names = %v，期望 1 段", got)
	}
	out := r.Render()
	if strings.Contains(out, "旧内容") || !strings.Contains(out, "新内容") {
		t.Fatalf("★ 覆盖未生效：%q", out)
	}
}

// TestEmptyContentRemovesSection：空正文 = 撤销声明。
func TestEmptyContentRemovesSection(t *testing.T) {
	r := promptsec.NewRegistry()
	_ = r.Declare(sec("a", "x", "有内容", 1))
	_ = r.Declare(sec("a", "x", "", 1))
	if got := r.Names(); len(got) != 0 {
		t.Fatalf("★ 空正文未撤销：%v", got)
	}
}

// TestRemoveOwnerOnlyDropsThatOwner：撤销一个 owner 不影响其他 owner。
func TestRemoveOwnerOnlyDropsThatOwner(t *testing.T) {
	r := promptsec.NewRegistry()
	_ = r.Declare(sec("a", "x", "A", 1))
	_ = r.Declare(sec("b", "x", "B", 2))
	r.RemoveOwner("a")
	out := r.Render()
	if strings.Contains(out, "A") || !strings.Contains(out, "B") {
		t.Fatalf("★ RemoveOwner 误删/漏删：%q", out)
	}
}

// TestSnapshotReturnsAllSections：Snapshot 输出反映当前声明。
func TestSnapshotReturnsAllSections(t *testing.T) {
	r := promptsec.NewRegistry()
	_ = r.Declare(sec("a", "x", "A", 1))
	_ = r.Declare(sec("b", "y", "B", 2))
	if got := r.Snapshot(); len(got) != 2 {
		t.Fatalf("★ Snapshot = %d 段，期望 2", len(got))
	}
}

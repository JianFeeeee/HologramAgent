package test

// internal/kstate 的黑盒判据：只用 NewSyncBus / Mount / Get / State 的导出方法，
// 输入 → 可观察输出。不读源码、不用 MountForTest。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// TestSyncBusReadyLifecycle：声明 → 就绪 → IsReady/Snapshot/AllReady/Missing。
func TestSyncBusReadyLifecycle(t *testing.T) {
	b := kstate.NewSyncBus()
	b.MarkDeclared([]string{"db", "http", "memory"})
	b.MarkReady("db")
	b.MarkReady("http")
	if !b.IsReady("db") || !b.IsReady("http") {
		t.Fatal("★ 已 MarkReady 的设施报告未就绪")
	}
	if b.IsReady("memory") {
		t.Fatal("★ 未就绪的设施报告就绪")
	}
	if b.AllReady([]string{"db", "http"}) != true {
		t.Fatal("★ AllReady 对全部就绪的集合应返回 true")
	}
	if b.AllReady([]string{"db", "memory"}) {
		t.Fatal("★ AllReady 对含未就绪项的集合返回了 true")
	}
	if missing := b.Missing([]string{"db", "http", "memory"}); len(missing) != 1 || missing[0] != "memory" {
		t.Fatalf("★ Missing = %v，期望 [memory]", missing)
	}
}

// TestSyncBusReadyInOrderIsStable：就绪顺序可复现，重复标记不重复记录。
func TestSyncBusReadyInOrderIsStable(t *testing.T) {
	b := kstate.NewSyncBus()
	b.MarkReady("a")
	b.MarkReady("a")
	b.MarkReady("b")
	got := b.ReadyInOrder()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("★ ReadyInOrder = %v，期望 [a b]", got)
	}
}

// TestSyncBusKnownVsReady：声明过但未就绪 ≠ 未声明。
func TestSyncBusKnownVsReady(t *testing.T) {
	b := kstate.NewSyncBus()
	b.MarkDeclared([]string{"db"})
	if !b.Known("db") {
		t.Fatal("★ 声明过的设施 Known=false")
	}
	if b.IsReady("db") {
		t.Fatal("★ 只声明未就绪却报告 IsReady")
	}
	if b.Known("ghost") {
		t.Fatal("★ 未声明的设施 Known=true")
	}
}

// TestSyncBusKernelComplete：内核完成标记是可观察输出。
func TestSyncBusKernelComplete(t *testing.T) {
	b := kstate.NewSyncBus()
	if b.KernelComplete() {
		t.Fatal("★ 初始就报告内核已完成")
	}
	b.MarkKernelComplete()
	if !b.KernelComplete() {
		t.Fatal("★ MarkKernelComplete 后仍报告未完成")
	}
}

// TestSyncBusSnapshotIsCopy：改快照不得影响总线。
func TestSyncBusSnapshotIsCopy(t *testing.T) {
	b := kstate.NewSyncBus()
	b.MarkReady("db")
	snap := b.Snapshot()
	if !snap["db"] {
		t.Fatal("★ Snapshot 缺 db")
	}
	snap["db"] = false
	snap["ghost"] = true
	if !b.IsReady("db") {
		t.Fatal("★ 外部改快照影响了总线内部")
	}
	if b.IsReady("ghost") {
		t.Fatal("★ 外部往快照塞值影响了总线内部")
	}
}

// TestMountGet：Mount → Get 拿到同一份状态（模块最核心的输入→输出）。
func TestMountGet(t *testing.T) {
	s := &kstate.State{DataDir: "/contract/tmp"}
	kstate.Mount(s)
	if got := kstate.Get(); got != s {
		t.Fatalf("★ Get() = %v，期望刚 Mount 的那份", got)
	}
}

// TestStateCloseAllInvokesClosers：AddCloser 注册的清理必须被调用一次。
func TestStateCloseAllInvokesClosers(t *testing.T) {
	s := kstate.New()
	n := 0
	s.AddCloser(func() { n++ })
	s.CloseAll()
	if n != 1 {
		t.Fatalf("★ CloseAll 后 closer 调用 %d 次，期望 1", n)
	}
}

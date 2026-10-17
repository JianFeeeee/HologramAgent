package proc

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestProcess_OnExitCompletesBeforeExitedCloses 钉死一条**顺序不变量**：
//
//	Exited() 通道关闭时，onExit 回调必须**已经返回**。
//
// ============================ 为什么这是一条安全不变量 ============================
// onExit（内核侧 Plugin.handleExit）会调 Host.ReclaimOwner 回收残留共享槽
// —— 那要读共享内存区域。而任何等待者（Stop/Kill/CallContext/Alive）看到
// Exited() 关闭就会认为「完全收尾」，进而释放资源（Host.Close 会 freeShm
// 解除整块 mmap）。
//
// 若 Exited() 先于 onExit 返回而关闭，就会出现：
//
//	等待者 → 释放映射 → onExit 仍在读那块内存 → SIGSEGV
//
// 这正是 2026-09-25 全量测试偶发崩溃的根因（栈见 process.go 的 markExited 注释）。
//
// ============================ 为什么用原子标志而非 channel ============================
// 要断言的是「关闭**之前**回调已完成」这一 happened-before 关系。
// 用一个在回调里置位的原子量 + 在收到关闭信号后立刻读它：
//   - 修复前：关闭先发生，回调尚未跑 ⇒ 读到 false ⇒ 判红
//   - 修复后：回调先跑完再关闭 ⇒ 读到 true ⇒ 判绿
//
// 用 atomic 而非普通 bool 是为了让「回调的写」与「测试的读」之间
// 有明确的同步语义（否则是数据竞态，-race 下会报）。
func TestProcess_OnExitCompletesBeforeExitedCloses(t *testing.T) {
	bin := buildTestPlugin(t, "crashplugin.go")

	var onExitDone atomic.Bool
	exitCh := make(chan struct{})

	p, err := Spawn("exitorder", bin, Options{
		Handler: noopHandler,
		OnExit: func(name string, err error) {
			// 模拟 handleExit 里的 ReclaimOwner：真实实现要读共享内存，
			// 这里用一个短暂延迟把「回调还在跑」这个窗口放大到可观测。
			// 关键：置位发生在**回调返回之前**。
			time.Sleep(50 * time.Millisecond)
			onExitDone.Store(true)
			close(exitCh)
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	// 触发插件 panic 自杀
	if _, err := p.Call(MethodToolInvoke, ToolInvokeParams{Name: "boom"}); err == nil {
		t.Error("崩溃插件应返回错误")
	}

	// 等 Exited() 关闭 —— 此后任何等待者都会认为「可以安全 unmap」
	select {
	case <-p.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("10s 内未观测到进程退出")
	}

	// ★ 核心断言：Exited() 已关闭时，onExit 必须已经跑完。
	if !onExitDone.Load() {
		t.Fatal("顺序违例：Exited() 已关闭，但 onExit 尚未返回。\n" +
			"  后果：等待者（Stop/Kill/Host.Close）会立刻 freeShm 解除映射，\n" +
			"  而 onExit 里的 ReclaimOwner 仍要读共享内存 ⇒ SIGSEGV。\n" +
			"  修法：markExited 中 close(p.exited) 必须放在 onExit 之后。")
	}

	// 顺带确认回调确实被调用过（而非因 bug 整个跳过）
	select {
	case <-exitCh:
	default:
		t.Fatal("onExit 未在 Exited() 关闭前完成")
	}
}

package core

// 判据：`cancel` 之后调度循环**不再取新任务**（2026-10-18 修的真缺陷）。
//
// ## 缺陷
//
// `schedulerLoop` 原来只在"队列空"的分支里 select `ctx.Done()`：
//
//	if kind == nextNone {
//		select { ... case <-a.ctx.Done(): return }
//	}
//
// 于是**只要队列里有任务，循环就永远不看 ctx.Done** —— 一直取、一直执行，
// 直到队列被取空才可能退出。
//
// 后果（生产路径 `reclaim`/`destroy`）：销毁流程先取残余任务、再销毁子，
// 而取完到销毁之间子仍在运行，继续把队列里的任务取走执行 —— 那些任务的
// 结果**永远无人认领**（子随即被销毁），且日志无痕迹：既不在"残余清单"里，
// 也没跑到出结果。
//
// ## 判据必须造出"队列非空时 cancel"这个确切场景
//
// 我第一版判据犯过两个错，都记在这里：
//
//  1. 加了 `scheduler.freeze` 绕过 —— 调度器本来就有停止语义（ctx），
//     再引入一个状态位会让"谁负责停"含糊，且忘了解除就永久僵住。已删。
//  2. 判据写成「先 Start、再 cancel、然后入队」。但那时队列是**空的**，
//     循环正好走 nextNone 分支看到 ctx.Done 退出 —— 于是**撤掉产品修复
//     判据依然通过**（实测 30/30 全绿）。那是假绿，测的是"空队列能退出"，
//     而不是真正的缺陷（"非空队列不会退出"）。
//
// 现在的做法：让**第一条任务正在执行且阻塞住**（provider 卡在 Chat 里），
// 于是循环停在 executeNewTask 内；此时再塞入若干任务并 cancel、放行。
// 若修复在位 ⇒ 循环处理完当前任务后看到 ctx.Done 就退出，队列里的新任务
// **一条都不会被执行**。若修复不在 ⇒ 它会继续把队列取空。

import (
	"context"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// blockingProvider 的 Chat 会一直阻塞，直到 release 被关闭。
//
// 用途：把调度循环**钉在 executeNewTask 里**，从而在"队列非空 + 任务正在跑"
// 这个确切时刻做 cancel，复现原缺陷的场景。
type blockingProvider struct {
	entered chan struct{} // 第一次进入 Chat 时关闭（供判据等待）
	release chan struct{} // 关闭后 Chat 返回
}

func (p *blockingProvider) Name() string { return "blocking" }
func (p *blockingProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	select {
	case <-p.entered:
	default:
		close(p.entered)
	}
	<-p.release
	return &agentAPI.CompletionResponse{Content: "ok"}, nil
}
func (p *blockingProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, context.Canceled
}
func (p *blockingProvider) MaxContextTokens() int { return 8192 }

// ★★★ 判据：队列非空时 cancel ⇒ 剩余任务一条都不被执行。
func TestSchedulerStopsTakingTasksAfterCancelWithNonEmptyQueue(t *testing.T) {
	prov := &blockingProvider{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	a := New(AgentConfig{
		ID:              "sched-nonempty",
		Provider:        prov,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
		DataDir:         t.TempDir(),
	})
	defer a.Stop()

	// ① 第一条：让循环进去执行并卡在 Chat 里。
	a.sched.enqueue(newInputTask(&agentIO.InputEvent{
		RequestID: "running", Source: "cli",
		Payload: map[string]interface{}{"content": "hi"},
	}))
	a.Start()

	select {
	case <-prov.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider 未被调用 —— 调度循环没跑起来")
	}

	// ② 此刻：循环卡在 executeNewTask（running 非空），再塞 5 条进队列。
	const pending = 5
	for i := 0; i < pending; i++ {
		a.sched.enqueue(newInputTask(&agentIO.InputEvent{
			RequestID: "pending", Source: "qq",
			Payload: map[string]interface{}{"content": "x"},
		}))
	}

	// ③ 在这个时刻 cancel（**队列非空** —— 这正是原缺陷的场景），然后放行。
	a.cancel()
	close(prov.release)

	// ④ 给它足够时间：修复在位时循环处理完当前任务即退出；
	//    修复缺失时它会把 5 条全部取走执行。
	time.Sleep(1200 * time.Millisecond)

	if got := a.sched.queueLen(); got != pending {
		t.Fatalf("★ 队列非空时 cancel 后仍取走了 %d 条（剩 %d / %d）——\n"+
			"  调度循环没有在取任务前检查 ctx.Done。\n"+
			"  生产后果：reclaim/destroy 取完残余到销毁之间，子继续执行那些任务，\n"+
			"  结果随销毁永久丢失且日志无痕迹。",
			pending-got, got, pending)
	}
}

// ★★★ 判据：FinishResident 把「停止调度 → 回收残余 → 销毁」做成一次事务。
//
// 它防的是：调用方拆成 ApplyResidual + Destroy 两步时，中间那段窗口里
// 子仍在收新任务。断言"返回后子已从登记表移除"，即销毁确实在同一事务内完成。
func TestFinishResidentIsAtomic(t *testing.T) {
	root, main := newRootWithoutSchedulerLoop(t)
	defer main.Close()
	defer root.Stop()

	info, err := root.SpawnResident(ResidentOptions{
		ID: "res-fin", TempPath: root.residentTempPath("res-fin"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := root.FinishResident(info.ID, ResidualKeep, nil); err != nil {
		t.Fatalf("FinishResident 失败: %v", err)
	}

	root.residentMu.Lock()
	_, still := root.residents[info.ID]
	root.residentMu.Unlock()
	if still {
		t.Fatal("★ FinishResident 之后子仍在登记表 —— 销毁没被纳入同一事务，" +
			"取残余到销毁之间的窗口仍存在")
	}
}

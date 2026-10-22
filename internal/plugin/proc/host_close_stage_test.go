package proc

import (
	"sync"
	"testing"
	"time"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// TestHost_CloseWaitsForInflightStage 钉死不变量：
//
//	Host.Close() 必须在 unmap 共享段**之前**等正在进行的 stage 收尾。
//
// 为什么这是安全不变量：stage 数据面与事件环是**两条独立写者**。
// beginStage 的首进者会持 stageMu 调 Segment.WriteAll（写共享段），
// 而它不在事件环订阅里 —— 退订阅管不到它。
//
// 实测（2026-10-08，连续两次部署复现）：Host.Close 只退了事件环就 freeShm，
// 于是 stage 线程继续写已 unmap 的段 ⇒ SIGSEGV，栈正是
//
//	stageCoordinator.enter → Segment.WriteAll → writeLocal → descOffset
//
// 这是 runtime 致命错误，recover 捕不到，整个 homed 被杀。后果还包括
// 部署验证门失效：分不清「新版本崩了」还是「旧版本关闭时崩」。
//
// 判据设计：让一个 stage 持锁（模拟正在写段），Close 在另一 goroutine 里跑。
// 若 Close 不等，它会在 stage 释放前就完成（并已 unmap）→ 测试红。
func TestHost_CloseWaitsForInflightStage(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}

	// 进入一个 stage（首进者）：此时 stageMu 被持有，等价于「正在写共享段」。
	sc := &pubsdk.StageContext{}
	coord, err := host.beginStage(sc)
	if err != nil {
		t.Fatalf("beginStage: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- host.Close() }()

	// Close 必须被挡住：它得等 stage 退出，不能提前 unmap。
	select {
	case <-closeDone:
		t.Fatal("Host.Close 未等 stage 收尾就返回：\n" +
			"  它会在 stage 仍写共享段时 freeShm ⇒ use-after-free\n" +
			"  ⇒ SIGSEGV（runtime fatal，recover 捕不到，homed 被杀）")
	case <-time.After(150 * time.Millisecond):
		// 正确：Close 还阻塞在等 stageMu
	}

	// 让 stage 收尾 → Close 应当随即完成。
	if err := host.endStage(coord); err != nil {
		t.Fatalf("endStage: %v", err)
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stage 已退出，Host.Close 仍未完成（可能死锁）")
	}
}

// TestHost_BeginStageAfterCloseRejected 钉住：
//
//	Close 开始后不得再接受新 stage。
//
// 为什么需要：光「等正在进行的 stage」还不够 —— Close 释放 stageMu 之后、
// 或 freeShm 之后，仍可能有新 stage 进场（agent 的收尾路径）。
// 那时它会照写已释放的段，同样是 use-after-free。
func TestHost_BeginStageAfterCloseRejected(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := host.beginStage(&pubsdk.StageContext{}); err == nil {
				t.Error("Close 之后 beginStage 仍成功：它会写已 unmap 的共享段 ⇒ use-after-free")
			}
		}()
	}
	wg.Wait()
}

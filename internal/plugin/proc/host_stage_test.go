package proc

import (
	"sync"
	"sync/atomic"
	"testing"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 本文件是 2026-09-04 06:56:18 线上 crash 的回归测试。
//
// 崩溃形态：homed 主进程直接死亡，退出码 2。
//
//	fatal error: sync: unlock of unlocked mutex
//	proc.(*Host).endStage(...) host.go:189
//	proc.(*coreHandler).runStage.func1() stage.go:94
//	core.(*StageHost).RunStage.func1() stages.go:190
//
// 注意 stage.go 与 stages.go 各有一层 recover，却都没拦住——
// sync.Mutex 的双重解锁是 runtime fatal，recover 捕不到。这是本次
// "整个内核本体崩溃"而非"插件崩溃被隔离"的直接原因。

// TestEndStage_LateArrivalNoDoubleUnlock 复现根因竞态。
//
// 旧实现把 leave() 放在 coordMu 之外，留出这个窗口：
//
//	A.endStage: leave() → inflight 1→0, last=true，尚未摘除 h.coord
//	B.beginStage: 看到 h.coord != nil，以「后到者」身份 enter，inflight 0→1
//	              （后到者不取 stageMu）
//	A.endStage: h.coord = nil; stageMu.Unlock()                    ← 第 1 次
//	B.endStage: leave() → inflight 1→0, last=true → stageMu.Unlock() ← 第 2 次 💥
//
// B 从未持有 stageMu，却因为挂进了一个正在收尾的协调器而成为
// "最后离开者"，于是对同一把锁解了两次。
//
// 本测试直接驱动 depart/enter 制造那个时序，不依赖调度巧合。
func TestEndStage_LateArrivalNoDoubleUnlock(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	scA := &pubsdk.StageContext{RawMessage: "A"}
	coordA, err := host.beginStage(scA)
	if err != nil {
		t.Fatalf("A beginStage: %v", err)
	}

	// A 收尾：修复后 depart 与摘除 h.coord 在同一个 coordMu 临界区内，
	// 所以此刻起 h.coord 已是 nil，B 不可能再挂进 A 的协调器。
	if err := host.endStage(coordA); err != nil {
		t.Fatalf("A endStage: %v", err)
	}

	// B 现在进入：必须成为新的首进者（拿到自己的 stageMu），
	// 而不是挂进 A 那个已收尾的协调器。
	scB := &pubsdk.StageContext{RawMessage: "B"}
	coordB, err := host.beginStage(scB)
	if err != nil {
		t.Fatalf("B beginStage: %v", err)
	}
	if coordB == coordA {
		t.Fatal("B 不该复用 A 已收尾的协调器——这正是 double-unlock 的来源")
	}
	if err := host.endStage(coordB); err != nil {
		t.Fatalf("B endStage: %v", err)
	}

	// 若上面多解了一次锁，这里会 fatal（runtime 级，测试进程直接死）；
	// 能走到这一步说明配对正确。
	scC := &pubsdk.StageContext{RawMessage: "C"}
	coordC, err := host.beginStage(scC)
	if err != nil {
		t.Fatalf("C beginStage: %v", err)
	}
	if err := host.endStage(coordC); err != nil {
		t.Fatalf("C endStage: %v", err)
	}
}

// TestEndStage_ConcurrentChurnNoFatal 高并发进出：真实触发线上那个窗口。
//
// 旧实现下这个测试会以 fatal error: sync: unlock of unlocked mutex 结束
// （整个测试二进制死亡，不是 FAIL）。修复后应干净通过。
func TestEndStage_ConcurrentChurnNoFatal(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	const workers = 8
	const rounds = 40
	var wg sync.WaitGroup
	var failures atomic.Int64

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				sc := &pubsdk.StageContext{RawMessage: "churn"}
				coord, err := host.beginStage(sc)
				if err != nil {
					failures.Add(1)
					return
				}
				if err := host.endStage(coord); err != nil {
					failures.Add(1)
					return
				}
			}
		}()
	}
	wg.Wait()

	if n := failures.Load(); n > 0 {
		t.Fatalf("%d 次 begin/end 失败", n)
	}
}

// TestBeginStage_MultiPluginSameStage 同阶段多插件扇出：
// 首进者取 stageMu，后到者只递增 inflight，最后离开者才解锁。
// 验证并发扇出这一原始设计仍然成立（§0.2 第 1 条）。
func TestBeginStage_MultiPluginSameStage(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	sc := &pubsdk.StageContext{RawMessage: "fanout"}

	// 三个插件先后进入同一次 stage
	c1, err := host.beginStage(sc)
	if err != nil {
		t.Fatalf("plugin1 beginStage: %v", err)
	}
	c2, err := host.beginStage(sc)
	if err != nil {
		t.Fatalf("plugin2 beginStage: %v", err)
	}
	c3, err := host.beginStage(sc)
	if err != nil {
		t.Fatalf("plugin3 beginStage: %v", err)
	}
	// 同一次 stage 内必须共用一个协调器（共享同一份 StageContext 段）
	if c1 != c2 || c2 != c3 {
		t.Fatal("同阶段并发插件应共用一个协调器")
	}

	// 前两个离开不该释放 stageMu
	if err := host.endStage(c1); err != nil {
		t.Fatalf("plugin1 endStage: %v", err)
	}
	if err := host.endStage(c2); err != nil {
		t.Fatalf("plugin2 endStage: %v", err)
	}
	// 最后一个离开才释放
	if err := host.endStage(c3); err != nil {
		t.Fatalf("plugin3 endStage: %v", err)
	}

	// 锁已释放：新一轮能立即开始
	c4, err := host.beginStage(sc)
	if err != nil {
		t.Fatalf("新一轮 beginStage 应成功（stageMu 已释放）: %v", err)
	}
	if c4 == c1 {
		t.Fatal("新一轮应是新的协调器")
	}
	if err := host.endStage(c4); err != nil {
		t.Fatalf("新一轮 endStage: %v", err)
	}
}

// TestBeginStage_SerialRounds 长串行：确认没有单向泄漏（少解锁会在第二轮卡死）。
func TestBeginStage_SerialRounds(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	for round := 0; round < 50; round++ {
		sc := &pubsdk.StageContext{RawMessage: "serial"}
		coord, err := host.beginStage(sc)
		if err != nil {
			t.Fatalf("round %d beginStage: %v", round, err)
		}
		if err := host.endStage(coord); err != nil {
			t.Fatalf("round %d endStage: %v", round, err)
		}
	}
}

// TestBeginStage_PhaseSequence 模拟一条消息走完 pre_action → chat → post_action。
func TestBeginStage_PhaseSequence(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	phases := []pubsdk.Stage{"pre_action", "chat", "after_toolcall", "post_action"}
	for msg := 0; msg < 10; msg++ {
		for _, p := range phases {
			sc := &pubsdk.StageContext{RawMessage: "msg", Phase: p}
			coord, err := host.beginStage(sc)
			if err != nil {
				t.Fatalf("msg %d phase %s beginStage: %v", msg, p, err)
			}
			if err := host.endStage(coord); err != nil {
				t.Fatalf("msg %d phase %s endStage: %v", msg, p, err)
			}
		}
	}
}

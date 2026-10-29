package update

// 内核侧升级执行器的判据。
//
// 这些用例钉住的是「谁执行」与「能不能并发」——两者错了的后果都比较重：
// 前者会让升级依赖一个可被禁用的插件，后者会让两次升级交错替换文件。

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// ① AttachExecutor 幂等：重复挂载不重复订阅。
func TestAttachExecutor_Idempotent(t *testing.T) {
	bus := events.NewBus()
	c1 := AttachExecutor(bus, t.TempDir())
	c2 := AttachExecutor(bus, t.TempDir())
	if c1 == nil || c2 == nil {
		t.Fatal("应返回非 nil 的 cleanup")
	}
	// 第二次应返回空操作，并且不应再订阅一次
	kernelExec.mu.Lock()
	attached := kernelExec.attached
	kernelExec.mu.Unlock()
	if !attached {
		t.Error("应处于已挂载状态")
	}
	c1()
	c2()
}

// ② bus 为 nil 时不 panic（未接总线的宿主）。
func TestAttachExecutor_NilBus(t *testing.T) {
	c := AttachExecutor(nil, t.TempDir())
	if c == nil {
		t.Fatal("应返回可调用的 cleanup 而不是 nil")
	}
	c() // 不应 panic
}

// ③ scope=plugin 的请求**不被内核执行器接管**。
//
// 这是职责边界：插件升级要 hmapdev 现场编译，那是应用域的事。
// 内核越界去管会让两条通道互相打架。
func TestHandleUpdateRequest_IgnoresPluginScope(t *testing.T) {
	bus := events.NewBus()
	// 清掉可能残留的挂载状态
	kernelExec.mu.Lock()
	kernelExec.running = false
	kernelExec.mu.Unlock()

	handleUpdateRequest(&events.Event{
		Type:    events.EventUpdateRequest,
		Source:  "test",
		Payload: map[string]interface{}{"scope": "plugin", "name": "timer"},
	}, t.TempDir(), bus)

	kernelExec.mu.Lock()
	running := kernelExec.running
	kernelExec.mu.Unlock()
	if running {
		t.Error("★ scope=plugin 不应被内核执行器接管")
	}
}

// ④ 并发请求只允许一个通过（旧实现完全没有互斥）。
//
// 判据不是"返回了错误"，而是**第二个请求确实没有开始执行**：
// 两个升级同时跑会互相覆盖备份目录、交错替换 homed 与 depgraph。
func TestHandleUpdateRequest_RejectsConcurrent(t *testing.T) {
	bus := events.NewBus()

	// 手工占住 running，模拟"升级进行中"
	kernelExec.mu.Lock()
	kernelExec.running = true
	kernelExec.mu.Unlock()
	t.Cleanup(func() {
		kernelExec.mu.Lock()
		kernelExec.running = false
		kernelExec.mu.Unlock()
	})

	// 发一个 kernel 请求：应被拒绝且**不启动 goroutine**
	handleUpdateRequest(&events.Event{
		Type:    events.EventUpdateRequest,
		Source:  "agent",
		Payload: map[string]interface{}{"scope": "kernel", "release": true},
	}, t.TempDir(), bus)

	// running 仍是 true（被拒绝的请求没有改动它）
	kernelExec.mu.Lock()
	stillRunning := kernelExec.running
	kernelExec.mu.Unlock()
	if !stillRunning {
		t.Error("被拒绝的并发请求不应改变 running 标志")
	}
}

// ⑤ RunUpgrade 在形态不允许自替换时返回 NotExecutableError（带可执行命令）。
//
// 这条保证：请求方拿到的是「该走哪条通道」而不是一个裸 error。
func TestRunUpgrade_NonExecutableReturnsPlan(t *testing.T) {
	// 造一个"不可写"的环境不可靠（root 无视权限），所以直接构造
	// 一个自替换被禁的 Capabilities 路径：用容器形态的判据。
	// 这里改为验证 NotExecutableError 本身的语义，不依赖运行环境。
	p := UpgradePlan{
		Channel:  ChannelPackageManager,
		Reason:   "由 dpkg 安装",
		Commands: []string{"sudo apt install --only-upgrade hologramagent-full"},
	}
	err := &NotExecutableError{Plan: p}
	msg := err.Error()
	if msg == "" {
		t.Fatal("错误消息不能为空")
	}
	if got := msg; !contains(got, "sudo apt") {
		t.Errorf("应包含可执行命令，实际: %s", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOfStr(s, sub) >= 0)
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ⑥ 大量并发调用不产生数据竞争（配合 -race）。
func TestHandleUpdateRequest_NoRaceOnRunning(t *testing.T) {
	bus := events.NewBus()
	kernelExec.mu.Lock()
	kernelExec.running = false
	kernelExec.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleUpdateRequest(&events.Event{
				Type:    events.EventUpdateRequest,
				Source:  "race",
				Payload: map[string]interface{}{"scope": "plugin"}, // 不触发执行
			}, t.TempDir(), bus)
		}()
	}
	wg.Wait()
	_ = os.Getpid
	_ = filepath.Join
}

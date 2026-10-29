package update

// 内核侧的**升级执行者**：订阅 update_request 并真正执行升级。
//
// ## 为什么执行权必须在内核，而不是某个插件
//
// 升级会替换内核二进制并让进程消失。这个动作的执行者必须满足三条：
//
//	① 不会被用户禁用（插件可以 auto_start:false / disable）
//	② 不依赖插件进程活着（插件崩了请求就丢了）
//	③ 知道自己的 dataDir / binPath（内核才有 ks.DataDir 这个权威值）
//
// 只有内核模块同时满足。而 pluginmgr 作为插件，三条全不满足。
//
// ## 为什么是「延迟挂载」而不是在 Init 里订阅
//
// update 模块必须在**第 0 层**（回滚判定要早于一切模块），但事件总线
// 由 kernelapi 层产出 —— 第 0 层时它还不存在。
//
// 所以分两段：
//
//	Init（第 0 层）    : 回滚判定 + 能力探测（不需要总线）
//	Attach（总线就绪后）: 订阅 update_request，接管执行
//
// Attach 由调用方（cmd/homed）在 kernelapi 层完成后调用。它幂等：
// 重复调用不会重复订阅。

import (
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// executor 是内核侧的升级执行器实例（进程级单例）。
type executor struct {
	mu       sync.Mutex
	attached bool
	unsub    func()

	// running 防止并发升级：两个请求同时进来会互相覆盖备份目录、
	// 交错替换 homed 与 depgraph。（旧实现完全没有互斥。）
	running bool
}

var kernelExec = &executor{}

// AttachExecutor 挂载内核侧升级执行器（幂等）。
//
// bus 为空时什么也不做（未接总线的宿主，如部分单测）。
// 返回的 cleanup 用于关停时退订。
func AttachExecutor(bus *events.Bus, dataDir string) func() {
	if bus == nil {
		return func() {}
	}
	kernelExec.mu.Lock()
	if kernelExec.attached {
		kernelExec.mu.Unlock()
		return func() {}
	}
	kernelExec.attached = true
	kernelExec.unsub = bus.Subscribe(events.EventUpdateRequest, func(evt *events.Event) {
		handleUpdateRequest(evt, dataDir, bus)
	})
	kernelExec.mu.Unlock()

	log.Printf("[update] kernel-side upgrade executor attached (data_dir=%s)", dataDir)
	return func() {
		kernelExec.mu.Lock()
		defer kernelExec.mu.Unlock()
		if kernelExec.unsub != nil {
			kernelExec.unsub()
			kernelExec.unsub = nil
		}
		kernelExec.attached = false
	}
}

// handleUpdateRequest 处理一条 update_request。
//
// ## 语义分工
//
//	scope=kernel → **本模块**执行（内核升级是内核的职责）
//	scope=plugin → 交给插件通道（pluginmgr 自己订阅处理）
//
// 内核侧只接管 kernel，不越界去管插件 —— 插件升级要用 hmapdev 现场编译，
// 那是应用域的事。
//
// ## 为什么异步
//
// 升级会替换二进制并请求重启，本进程随后消失。事件处理器是**同步投递**的
// （events.Bus.Publish 直接调 handler），在处理器里同步做完会让发布方卡住
// 直到进程被杀。
func handleUpdateRequest(evt *events.Event, dataDir string, bus *events.Bus) {
	if evt == nil || evt.Payload == nil {
		return
	}
	scope, _ := evt.Payload["scope"].(string)
	if scope != "kernel" {
		return // 插件升级不归内核执行器
	}
	pkgPath, _ := evt.Payload["pkg"].(string)
	release, _ := evt.Payload["release"].(bool)

	kernelExec.mu.Lock()
	if kernelExec.running {
		kernelExec.mu.Unlock()
		log.Printf("[update] 拒绝并发升级请求（来源 %s）：已有升级在进行中", evt.Source)
		publishResult(bus, "kernel", "", false, "", "", false,
			"已有升级正在进行中，本次请求被拒绝（避免备份与替换交错）")
		return
	}
	kernelExec.running = true
	kernelExec.mu.Unlock()

	go func() {
		defer func() {
			kernelExec.mu.Lock()
			kernelExec.running = false
			kernelExec.mu.Unlock()
		}()
		if err := RunUpgrade(pkgPath, release, dataDir, bus); err != nil {
			log.Printf("[update] upgrade failed (source=%s): %v", evt.Source, err)
		}
	}()
}

// RunUpgrade 执行一次内核升级（内核侧入口，含形态分派）。
//
// 流程：解析来源 → 形态闸门 → 取包 → Apply（写 pending + 原子替换）。
// **不重启**：重启由调用方走 KernelControlAPI（restartCh → exit 42）。
func RunUpgrade(pkgPath string, release bool, dataDir string, bus *events.Bus) error {
	src, err := ParseSource(pkgPath, release)
	if err != nil {
		publishResult(bus, "kernel", "", false, "", "", false, err.Error())
		return err
	}

	bus = eventBusOr(bus)
	binPath := KernelBinaryPath()

	// ★ 形态闸门：先判断能不能自己升，再决定要不要下载 880MB。
	capab := Detect(binPath, dataDir)
	plan := PlanUpgrade(capab, pkgPath)
	if !plan.Executable {
		publishResult(bus, "kernel", "", false, "", "", false, plan.Reason)
		return &NotExecutableError{Plan: plan}
	}

	workDir, err := os.MkdirTemp("", "kernel-upgrade-*")
	if err != nil {
		publishResult(bus, "kernel", "", false, "", "", false, err.Error())
		return fmt.Errorf("建临时目录: %w", err)
	}
	defer os.RemoveAll(workDir)

	root, _, err := src.Resolve(workDir)
	if err != nil {
		publishResult(bus, "kernel", "", false, "", "", false, err.Error())
		return err
	}
	newDG := FindFile(root, "depgraph.bin")

	_, err = Apply(root, binPath, newDG, dataDir, bus)
	return err
}

// NotExecutableError 表示本机形态不允许自替换（调用方应转成可执行提示）。
type NotExecutableError struct {
	Plan UpgradePlan
}

func (e *NotExecutableError) Error() string {
	if len(e.Plan.Commands) > 0 {
		return e.Plan.Reason + "；请执行: " + e.Plan.Commands[0]
	}
	return e.Plan.Reason
}

package seq

// 开机序列：系统就绪时自动执行标记了 boot 的序列。
//
// ## 为什么这是 seq 最核心的用途（用户原话）
//
// 「seq 最核心的是定时复用，比如开机执行一段序列」
//
// 单次任务里编序列往往**不划算**：先 seq_create 写 JSON 再 seq_run，
// 比直接调工具多两步（e2e 实测：多文件读取场景模型直接并行 files_read
// 就是最优解，包成序列纯亏）。真正省事的是**同一套流程反复跑** ——
// 而开机是最典型的周期性触发点。
//
// ## 时序（为什么订阅 tools_ready 而不是在 Start 里跑）
//
// 插件 Start 的先后由依赖图决定，此刻：
//   · 序列要调的工具可能属于还没拉起的插件
//   · agent 循环还没开始，投递的输入无人接收
//
// plugin-host 在 `ks.PluginsReady = true`（全部插件含内置都已加载）
// 之后发布 tools_ready，那一刻**工具才全部可调**。
// 本文件只订阅那一个事件 —— 不需要等 agent 起来。

import (
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// subscribeBoot 订阅 tools_ready，在该时刻执行所有 boot 序列。
//
// 订阅 tools_ready 而不是"agent 就绪"：seq 自动运行**不依赖 agent** ——
// 它只是一段工具调用序列，返回值进队列等 agent 起来后处理即可。
// 绑在 agent 启动上会把时机人为推后，且依赖编排层的判断。
//
// 返回退订函数（Stop 时调用）。订阅失败不阻断插件启动：
// 工具本身仍可用，只是开机序列不会跑 —— 但必须留日志，
// 否则"为什么开机没执行"完全无从查起。
func (p *Plugin) subscribeBoot() func() {
	if p.sdk == nil {
		return func() {}
	}
	unsub := p.sdk.Subscribe(events.EventToolsReady, func(evt *events.Event) {
		// 事件在总线 goroutine 上分发，不能阻塞它：开机序列可能跑很久。
		go p.runBootSequences()
	})
	return unsub
}

// runBootSequences 依次执行所有 boot 序列。
//
// 串行执行（不是并发）：开机序列通常彼此独立，但并发跑会让日志交错、
// 且若两条序列争用同一资源，失败原因极难定位。开机不是性能敏感路径。
//
// 单条失败不影响后续序列 —— 一条坏了不该让其余开机动作全部失效。
func (p *Plugin) runBootSequences() {
	names := p.store.List()
	var boots []string
	for _, n := range names {
		sq, err := p.store.Load(n)
		if err != nil {
			// 坏序列不阻断：store 里可能有历史遗留文件
			log.Printf("[seq] boot scan skip %q: %v", n, err)
			continue
		}
		if sq.Boot {
			boots = append(boots, n)
		}
	}
	if len(boots) == 0 {
		return
	}

	log.Printf("[seq] tools_ready: running %d boot sequence(s): %v", len(boots), boots)
	for _, name := range boots {
		sq, err := p.store.Load(name)
		if err != nil {
			log.Printf("[seq] boot %q load failed: %v", name, err)
			continue
		}
		// 开机序列无外部入参（nil）：组的 when/args 按既有语义取零值。
		res, err := p.runSequence(sq, nil, nil)
		if err != nil {
			log.Printf("[seq] boot %q failed: %v", name, err)
			continue
		}
		// runSequence 返回渲染后的摘要（interface{}），含失败标记时如实转述。
		log.Printf("[seq] boot %q done (%d groups): %v", name, len(sq.Groups), res)
	}
}

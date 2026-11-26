package rtchild

// intercept.go —— **rt 自己的**输入分拣（用户裁定 2026-11-20）。
//
// ## rt 的语义与另外两类根本不同
//
//	主 agent   四级抢占阶梯（L4 抢占 / L3 挂起 / L2 优先 / L1 队尾）
//	驻留子     高优先级入队（不挂起）
//	rt         **没有级别概念**：任何输入都覆盖当前轮（最新者胜）
//
// 所以 rt 不能借主 agent 的 InterceptLoop —— 那个循环一进门就按
// `priority` 判级别、进中断队列、请求抢占。rt 拿到同样的标记，
// 该做的是"覆盖单槽 + 取消当前轮"，而不是"排到第几级"。
//
// ## 标记在这里怎么被解释（只属于 rt）
//
//	stop == true   ⇒ 清槽（丢掉当前轮的活）+ 取消当前生成
//	priority 非空  ⇒ 中断：同样覆盖（L 值对 rt 无意义 —— 它没有阶梯）
//	其余           ⇒ 覆盖（rt 没有队列，"先排队"这个概念不存在）

import (
	"log"
	"runtime/debug"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// InterceptLoop 是 rt 的输入分拣循环。
//
// rt 的**主驱动**是 `rtLoop`（由 SpawnRTChild 起）；本循环只处理
// "同一个 inputch 上来的输入该落到 rt 的哪个槽"。
func (m *M) InterceptLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[rtchild] interceptLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go m.InterceptLoop()
		}
	}()
	for {
		select {
		case evt := <-m.A.IO().InputChan():
			m.dispatchRTInput(evt)
		case <-m.A.Ctx().Done():
			return
		}
	}
}

// dispatchRTInput 把一条输入按 **rt 的语义**处置。
//
// ★ 所有判定都在这一个函数里（rt 自己一份），不借任何共享判定。
func (m *M) dispatchRTInput(evt *agentIO.InputEvent) {
	if evt == nil {
		return
	}
	rc := m.rtByInputChannel(evt.OutputChannel)
	if rc == nil {
		// 找不到对应的 rt 子：**不静默处理**（谁都不该假装接下这条输入）。
		// rt 没有"根兜底"这条路径 —— 它的输入只属于具体某一通电话。
		m.A.Logf("rt input dropped: no rt child owns channel %q (source=%s request=%s)",
			evt.OutputChannel, evt.Source, evt.RequestID)
		return
	}

	marks := agentIO.MarksOf(evt.Payload)

	// ★ rt 的"停止"：清空槽（丢掉这一轮的活）+ 取消当前生成。
	//
	// 与主 agent 的 stop 不同：主 agent 登记"短路配额"让排队队列逐条短路；
	// rt 没有队列可短路，它的 stop 就是"丢掉当前轮，等新输入"。
	if marks.Flag("stop") {
		rc.slot.Clear()
		if cancel := rc.takeCurCancel(); cancel != nil {
			cancel()
		}
		m.A.Logf("rt stop requested on channel %s (slot cleared)", evt.OutputChannel)
		return
	}

	// ★ 其余一律**覆盖**（priority 的值对 rt 无意义，只表示"这是新输入"）。
	rc.pushInput(evt)
}

// rtByInputChannel 按 inputch 名字反查 rt 实例。
//
// rt 的通道是**成对**给的（input+output），而 input 名是它自己的键。
func (m *M) rtByInputChannel(ch string) *rtChild {
	if ch == "" {
		return nil
	}
	for _, id := range m.RTChildren() {
		if rc := m.RTChildOf(id); rc != nil && rc.input == ch {
			return rc
		}
	}
	return nil
}

package agent

// statussection.go —— **状态面也走注册表**（用户裁定 2026-11-18，§0 第 12 条）。
//
// ## 为什么不是给 KernelStatus 加字段
//
// 实测（`4ed18fb4`）：`KernelStatus` 20 个字段里 **9 个零填充点**
// （`Memory/Knowledge/Documents/TextMemory/Social/LLM/Context/Runtime/Tracker`
// 在 `collectKernelStatus` 里确实有代码，但生产装配下它们的句柄全为 nil ⇒
// 恒为零值），另外 3 个（`Scheduler`/`Residents`/`ONNX`）装的是**类型专属结构**。
//
// 这与 `Hooks.GraphMemory` 是同一个病：基座假设所有 agent 都有这些面，
// 用一个大结构体表达"三形态 + 一无"。字段越多，假设越空。
//
// ## 正确的形态：谁持有数据，谁注册
//
//	type StatusSection struct { Key string; Payload any }
//	Agent.RegisterStatusSection(StatusSection) bool
//	KernelStatus.Sections map[string]json.RawMessage   // 按 key 取，消费方自己解码
//
// 基座只留**真正公共**的：`Uptime/StartTime/Build/AgentID/Tools/Channels/
// InputChannels/Plugins/Usage`（以及 `runtime`/`llm` 两个与 agent 无关的段）。
//
// ## 这条同时回答「context 段的数字由谁填」
//
// 类型自己填（在自己 `Initialize` 里注册 `"context"` 段），
// **不是**内核 eventloop 顺手填，也不是状态面现算 ——
// "什么时候算、算什么"是类型策略。

import (
	"encoding/json"
	"log"
	"sync"
)

// StatusSection 是一个**由持有者注册**的状态段。
//
// Key 是消费方取用的键（`"context"` / `"scheduler"` / `"memory"` / `"resident"` …）；
// Payload 是任意结构，基座**不解释**它（序列化成 json.RawMessage 交给消费方解码）。
type StatusSection struct {
	Key string
	// Payload 可以是立即序列化的值，也可以是 `func() any`（延迟到取状态时求值）。
	//
	// ★ 为什么允许函数：状态面每 3 秒被轮询一次，而有些段要现场读句柄
	//   （"当前的 scheduler 队列深度"）。传函数让持有者自己决定求值时机，
	//   基座不在错误的时刻算它。
	Payload any
}

// statusSections 是**本 agent** 注册的状态段（装配期写入，运行期只读）。
type statusSections struct {
	mu       sync.RWMutex
	sections map[string]StatusSection
}

// RegisterStatusSection 注册/覆盖一个状态段（**装配期**调用）。
//
// 重复注册同一 key ⇒ 后到者覆盖并留日志：类型重载（软重启）会重新注册，
// 覆盖是常规操作；但同名**不同来源**意味着装配点写错了，日志要能看出来。
func (a *Agent) RegisterStatusSection(s StatusSection) bool {
	if a == nil || s.Key == "" {
		return false
	}
	a.statusSecMu.Lock()
	defer a.statusSecMu.Unlock()
	if a.statusSections == nil {
		a.statusSections = map[string]StatusSection{}
	}
	if old, dup := a.statusSections[s.Key]; dup {
		log.Printf("[agent] status section %q re-registered (old payload type %T)", s.Key, old.Payload)
	}
	a.statusSections[s.Key] = s
	return true
}

// CollectStatusSections 把已注册的段序列化成交给状态面的 map。
//
// 求值失败（panic / JSON 不可序列化）的段**逐条**记日志并跳过 ——
// 状态面不能因为一段坏了就整份不可用（它恰是排障时唯一的观察窗）。
func (a *Agent) CollectStatusSections() map[string]json.RawMessage {
	if a == nil {
		return nil
	}
	a.statusSecMu.RLock()
	sections := make(map[string]StatusSection, len(a.statusSections))
	for k, v := range a.statusSections {
		sections[k] = v
	}
	a.statusSecMu.RUnlock()
	if len(sections) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(sections))
	for k, s := range sections {
		raw := encodeSection(k, s)
		if raw == nil {
			continue
		}
		out[k] = raw
	}
	return out
}

// encodeSection 求值 + 序列化一个段（失败返回 nil 并留痕）。
func encodeSection(key string, s StatusSection) (raw json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] status section %q panicked (skipped): %v", key, r)
			raw = nil
		}
	}()
	payload := s.Payload
	if fn, ok := payload.(func() any); ok {
		payload = fn()
	}
	if payload == nil {
		return nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[agent] status section %q: marshal failed (skipped): %v", key, err)
		return nil
	}
	return b
}

// RegisterSection 实现 sdk.StatusAPI：把 agent 的状态段也挂到 SDK 的口上。
//
// ★ 为什么两处都要有：agent 侧一条（本 agent 自己的段，随实例走），
//
//	SDK 侧一条（**插件**注册的段，全局）。`GetKernelStatus` 把两者合进
//	同一份快照，消费方无从察觉差异 —— 那正是"谁持有数据谁注册"的效果。
func (a *Agent) RegisterSection(key string, provider func() any) {
	a.RegisterStatusSection(StatusSection{Key: key, Payload: provider})
}

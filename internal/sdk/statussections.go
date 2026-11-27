package sdk

import (
	"encoding/json"
	"sort"
	"sync"
)

// statussections.go —— **插件侧**的状态段注册表（用户裁定 2026-11-18）。
//
// ## 为什么插件需要自己的一条注册表
//
// A-1 把 `KernelStatus` 收成了 `Sections map[string]json.RawMessage`，
// 原则是「谁持有数据谁注册」。agent 侧有了 `RegisterStatusSection`，
// 但**插件**不是 agent（它不参与 eventloop），所以它需要另一条路。
//
// 一条全局注册表就够：段是**本进程**的运行事实（知识库条目数、索引状态…），
// 与"哪个 agent"无关。`GetKernelStatus` 只负责把它合进快照。
//
// ## 契约
//
//   - 装配期/Start 时注册；重复 key = 后者覆盖（热重载要能换实现）。
//   - provider 在**读取时**调用（拿到的是当下事实，不是注册时的快照）。
//   - 未注册的 key 不出现（消费方按 key 取，取不到就是没有）。
var (
	statusSectionsMu sync.RWMutex
	statusSections   = map[string]func() any{}
)

// RegisterStatusSection 注册（或覆盖）一个插件状态段。
func RegisterStatusSection(key string, provider func() any) {
	if key == "" || provider == nil {
		return
	}
	statusSectionsMu.Lock()
	defer statusSectionsMu.Unlock()
	statusSections[key] = provider
}

// UnregisterStatusSection 撤销一个段（插件 Stop 时调用；幂等）。
func UnregisterStatusSection(key string) {
	statusSectionsMu.Lock()
	defer statusSectionsMu.Unlock()
	delete(statusSections, key)
}

// CollectPluginStatusSections 取全部插件段的 JSON 快照。
func CollectPluginStatusSections() map[string]json.RawMessage {
	statusSectionsMu.RLock()
	defer statusSectionsMu.RUnlock()
	if len(statusSections) == 0 {
		return nil
	}
	keys := make([]string, 0, len(statusSections))
	for k := range statusSections {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]json.RawMessage, len(keys))
	for _, k := range keys {
		if data, err := json.Marshal(statusSections[k]()); err == nil {
			out[k] = data
		}
	}
	return out
}

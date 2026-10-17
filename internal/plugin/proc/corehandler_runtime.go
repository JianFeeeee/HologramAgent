package proc

import (
	"encoding/json"
	"fmt"

	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// handlePluginMgr 处理插件管理：reloadOne / listLoaded / isDisabled。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handlePluginMgr(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 插件管理（原 case 48/49/50）----
	case MethodPluginReloadOne:
		pm := h.sdk.PluginMgr()
		if pm == nil {
			return nil, errUnavailable("plugin manager")
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, pm.ReloadOne(p.Name)
	case MethodPluginListLoaded:
		pm := h.sdk.PluginMgr()
		if pm == nil {
			return nil, errUnavailable("plugin manager")
		}
		list := pm.ListLoadedPlugins()
		if list == nil {
			list = []string{}
		}
		return map[string]interface{}{"plugins": list}, nil
	case MethodPluginIsDisabled:
		pm := h.sdk.PluginMgr()
		if pm == nil {
			return nil, errUnavailable("plugin manager")
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return map[string]interface{}{"disabled": pm.IsPluginDisabled(p.Name)}, nil
	case MethodPluginMgrStop:
		pm := h.sdk.PluginMgr()
		if pm == nil {
			return nil, errUnavailable("plugin manager")
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, pm.StopPlugin(p.Name)
	case MethodPluginMgrStart:
		pm := h.sdk.PluginMgr()
		if pm == nil {
			return nil, errUnavailable("plugin manager")
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, pm.StartPlugin(p.Name)
	case MethodPluginMgrIsRuntimeStopped:
		pm := h.sdk.PluginMgr()
		if pm == nil {
			return nil, errUnavailable("plugin manager")
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return map[string]interface{}{"runtime_stopped": pm.IsPluginRuntimeStopped(p.Name)}, nil

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// handleStageLocks 处理共享段锁仲裁（§3.7）。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleStageLocks(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 共享段锁仲裁（新增，§3.7）----
	case MethodStageLock:
		if h.locks == nil {
			return nil, fmt.Errorf("stage.lock: 锁仲裁未就绪")
		}
		return nil, h.locks.acquire(h.name)
	case MethodStageUnlock:
		if h.locks == nil {
			return nil, fmt.Errorf("stage.unlock: 锁仲裁未就绪")
		}
		return nil, h.locks.release(h.name)

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// handleEvents 处理事件订阅（§3.6，子进程下首次真正可用）。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleEvents(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 事件订阅（原 case 23/24，子进程下首次真正可用，§3.6）----
	case MethodEventsSubscribe:
		var p struct {
			Types []pubsdk.EventType `json:"types"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if h.evtRing == nil {
			return nil, fmt.Errorf("%s: 事件环未就绪", method)
		}
		// 订阅请求来自子进程——handler 直接注册到 Bus，
		// 事件经 EventRing 写入环后由子进程消费。
		//
		// ★ 用 tracked 版本：订阅会被登记，Host.Close 在内核关停时统一退订。
		// 必须如此——这些 handler 写共享内存，而 Host.Close 会 munmap 整块区域；
		// 未退订的 handler 在关停后会写已解除映射的内存 ⇒ SIGSEGV。
		h.evtRing.EvtRingSubscribeTracked(p.Types)
		return nil, nil

	case MethodEventsUnsubscribe:
		// 事件环的订阅没有**按插件**持久化句柄（取消函数由 Subscribe 返回，
		// 但子进程不保存，故无法精确撤销单个插件的订阅）。
		// 当前设计：子进程 Stop 时由内核统一清理——具体落点是
		// Host.Close → evtCloser.Close 退订全部 tracked 订阅。
		// 因此这里仍是 no-op；但「统一清理」现在是真的有实现，
		// 不再是只写在注释里的承诺。
		return nil, nil

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// handleArena 处理共享槽池分配/释放（内部传输层，见 protocol.go）。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleArena(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 共享槽池（内部传输层，见 protocol.go 注释）----
	case MethodArenaAlloc:
		var p ArenaAllocParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		ref, err := h.arenaAlloc(p.Size)
		if err != nil {
			return nil, err
		}
		return ArenaAllocResult{Ref: ref}, nil

	case MethodArenaFree:
		var p ArenaFreeParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, h.arenaFree(p.Ref)

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

package agent

// substates.go —— **类型状态分区表**的存取（基座只存 `any`，不认识形状）。
//
// §6.2 #3 #4：`rtState` / `residentState` / `workerState` / `mainState` 全是
// **类型状态**。基座只提供格子，形状由类型包自己定义。
//
// ★ 为什么不是一张 `map[string]any`：map 的键是运行期字符串，拼错不报错
//   （`Sub("wrok")` 静默返回 nil）。用固定常量 + 显式字段，拼错是编译错误。

import "sync"

type subStates struct {
	mu    sync.RWMutex
	rt    any
	arch  any
	res   any
	wrk   any
	mn    any
	extra map[string]any
}

func (s *subStates) get(key string) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch key {
	case SubKeyRT:
		return s.rt
	case SubKeyArchive:
		return s.arch
	case SubKeyResident:
		return s.res
	case SubKeyWorker:
		return s.wrk
	case SubKeyMain:
		return s.mn
	}
	return s.extra[key]
}

func (s *subStates) set(key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch key {
	case SubKeyRT:
		s.rt = v
		return
	case SubKeyArchive:
		s.arch = v
		return
	case SubKeyResident:
		s.res = v
		return
	case SubKeyWorker:
		s.wrk = v
		return
	case SubKeyMain:
		s.mn = v
		return
	}
	if s.extra == nil {
		s.extra = map[string]any{}
	}
	s.extra[key] = v
}

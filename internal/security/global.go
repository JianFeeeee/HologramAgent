package security

// 进程级单例与两个入向填充入口。
//
// ## 依赖方向（重要）
//
//	internal/security        ← 纯逻辑，不 import 任何 agent 包
//	    ↑
//	internal/agent/api       provider 装饰器（出向改写）
//	internal/agent/core      工具参数/载荷填充（入向回填）
//
// 是 DAG，**没有 import 环**。所以装饰器可以直接用内核的 Provider 接口，
// 不必做接口投影。（初版误以为会成环而写了一层投影接口，已删除。）
//
// ## 为何单例必须是进程级
//
// 已核：主 / 子 / 驻留子 agent 共享同一个 ProviderManager
//（`resident.go:185`；manager 全进程只建一份 `bootstrap.go:249`）。
// 所以变量库挂在 provider 层天然就是进程级 —— **不需要跨 agent 同步**。

import (
	"log"
	"sync"
)

var (
	globalMu sync.RWMutex
	global   *Store
)

// InitGlobal 装配进程级单例。**必须在任何 agent 启动前调用**。
func InitGlobal(opt Options) (*Store, error) {
	s := New(opt)
	if err := s.Load(); err != nil {
		return nil, err
	}
	globalMu.Lock()
	global = s
	globalMu.Unlock()
	return s, nil
}

// SetGlobal 直接装入一个已构造的 Store（测试与特殊装配用）。
func SetGlobal(s *Store) {
	globalMu.Lock()
	global = s
	globalMu.Unlock()
}

// Global 取进程级单例；未初始化时返回 nil。
//
// ★ 为何返回 nil 而不报错：调用点散布在内核热路径上，
// 未启用时必须**零成本跳过**。所有调用方都写成
// `if s := Global(); s != nil { ... }`。
func Global() *Store {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global
}

// Enabled 报告安全模块是否已启用。
func Enabled() bool {
	s := Global()
	return s != nil && s.enabled
}

// ---------------- 入向填充（插件无感知的关键） ----------------

// FillToolArgs 把一个工具调用的参数里的 `@{别名}` 换回明文。
//
// 这是**内核工具链的入口**（`core.StageHost.ExecuteTool`）：
// 插件收到的参数已经填好，它完全不知道有替换这回事。
//
// 返回 (填充处数, 用到的别名)。未初始化时返回 (0, nil)。
func FillToolArgs(tool string, args map[string]interface{}) (int, []string) {
	s := Global()
	if s == nil || args == nil {
		return 0, nil
	}
	n, used := s.FillMap(args)
	if n > 0 {
		log.Printf("[security] tool %s backfilled %d argument(s) (aliases %v)", tool, n, used)
	}
	return n, used
}

// FillPayload 把一段文本载荷里的 `@{别名}` 换回明文。
//
// 用于**设备输出通道**（`core` 的 `dev.Execute`）：发给设备的指令同理
// 需要真实凭据，而设备侧对替换无感知。
func FillPayload(where, text string) string {
	s := Global()
	if s == nil || text == "" {
		return text
	}
	out, n, used := s.Fill(text)
	if n > 0 {
		log.Printf("[security] %s payload backfilled %d place(s) (aliases %v)", where, n, used)
	}
	return out
}

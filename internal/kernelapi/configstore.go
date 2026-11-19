package kernelapi

import "time"

// configstore.go —— **配置管理器**的契约（基座注入，各 agent 经它读写配置）。
//
// ## 用户裁定
//
// > 「基座需要注入配置管理器啊，然后各个 agent 可以通过配置管理器读取/写入配置」
//
// ## 它解决什么（这是本轮真正的问题）
//
// 改造前：`kernelmod/agent` 在装配期把配置**读成快照**塞进 `AgentConfig`
// （DistillInterval / MaxContextSize / CtxTuning / Offload …），基座再把它们
// 拷进 `mainState`。后果三条：
//
//	① 配置改了要重启内核才生效（快照是死的）
//	② 同一份值有两个落点（`ConfigRegistry` 的真值与 `mainState` 的副本），
//	   实测已漂移过一次（`core.agent.max_context_size` 注册了却从没被读到）
//	③ 这些"值"被迫住在 `mainState` 里，于是 `mainState` 搬不走
//
// 现在：基座注入配置管理器，**用到时才读**。配置是**内核持有的事实**，
// 不是 agent 的私有状态。
//
// ## 为什么是窄接口而不是 *config.ConfigRegistry
//
// `internal/config` import 了 `security`，而基座**不许** import security
// （§10 不变量）。所以只在这一层声明基座真正用到的那几个方法。

// ConfigStore 是配置管理器给基座/agent 的只读 + 可写面。
type ConfigStore interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetBool(key string, def bool) bool
	GetDuration(key string, def time.Duration) time.Duration
	// Set 写入配置（agent 通过它改自己的运行参数，如 offload 开关）。
	Set(key string, value interface{}) error
}

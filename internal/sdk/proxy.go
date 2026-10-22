// Package sdk —— 反代声明的内核侧登记表。
//
// 契约定义在公开 SDK（pubsdk.ProxyDef）。本文件只做两件事：
//
//  1. 把公开类型重新导出，让内核侧调用方只需 import 本包。
//  2. 维护**内置插件**的声明表（它们没有 plugin.json，扫目录发现不了）。
//
// 外部插件的声明由反代层直接读 plugin.json 得到，不经过这里 —— 那份声明
// 是静态的、插件没启动也可见，而内置插件的声明只能运行期拿到。两种来源在
// 反代层合并（见 webui.buildProxyTable）。
package sdk

import (
	"strings"
	"sync"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// ProxyDef 是一条反代声明（见 pubsdk.ProxyDef 的完整文档，含「单一入口原则」）。
// 与 ConfigDef / ChannelDef / ToolDef 同族：SDK 定契约，内核实现行为。
type ProxyDef = pubsdk.ProxyDef

// 鉴权与 Host 规范化的常量/函数转发（调用方不必两处 import）。
const (
	// ProxyAuthAgent 表示由内核统一保护（门户会话或 X-API-Key）。
	ProxyAuthAgent = pubsdk.ProxyAuthAgent
	ProxyAuthNone  = pubsdk.ProxyAuthNone

	// ProxyAuthAgentLegacy 是旧值 "homeagent"：存量 plugin.json 里写的仍是它。
	// 只用于兼容读取；判断时一律先过 EffectiveProxyAuth 归一化。
	ProxyAuthAgentLegacy = pubsdk.ProxyAuthAgentLegacy

	// ProxyAuthHomeAgent 是 ProxyAuthAgent 的旧名，仅为存量源码保留。
	//
	// Deprecated: 用 ProxyAuthAgent。
	ProxyAuthHomeAgent = pubsdk.ProxyAuthHomeAgent
)

// ValidateProxyDef 校验一条声明，返回人类可读的错误（合法时为空）。
func ValidateProxyDef(d ProxyDef) string { return pubsdk.ValidateProxyDef(d) }

// EffectiveProxyAuth 返回生效的鉴权模式（空串与旧值都归一化为 ProxyAuthAgent）。
func EffectiveProxyAuth(auth string) string { return pubsdk.EffectiveProxyAuth(auth) }

// ValidProxyAuth 判断鉴权取值是否合法。
func ValidProxyAuth(auth string) bool { return pubsdk.ValidProxyAuth(auth) }

// NormalizeProxyHost 由插件名推导默认的 Host 标签。
func NormalizeProxyHost(plugin string) string { return pubsdk.NormalizeProxyHost(plugin) }

// ValidProxyHostLabel 校验子域名标签是否合法（DNS label 规则）。
func ValidProxyHostLabel(label string) bool { return pubsdk.ValidProxyHostLabel(label) }

// 内置插件（编译进内核、无 plugin.json）的反代声明表。
//
// 版本号 builtinProxyVer 每次变更自增。反代层据此判断缓存的路由表是否过期 ——
// 比每次请求都重新聚合一遍便宜得多。
var (
	builtinProxyMu   sync.RWMutex
	builtinProxyVer  int64
	builtinProxyDefs = map[string][]ProxyDef{}
)

// RegisterBuiltinProxy 登记一个内置插件的服务声明（由 RegisterProxy 转发）。
//
// ProxyDef.Name 由调用方保证非空（RegisterProxy 会在缺失时兜底为 "service"）。
func RegisterBuiltinProxy(plugin, name string, d ProxyDef) {
	if plugin == "" || strings.TrimSpace(d.Target) == "" {
		return
	}
	if d.Name == "" {
		d.Name = name
	}
	if d.Name == "" {
		d.Name = "service"
	}
	builtinProxyMu.Lock()
	defer builtinProxyMu.Unlock()
	// 同名重复登记（如自动重启后再次 Start）视为刷新，不重复累积。
	list := builtinProxyDefs[plugin]
	for i := range list {
		if list[i].Name == d.Name {
			list[i] = d
			builtinProxyVer++
			return
		}
	}
	builtinProxyDefs[plugin] = append(list, d)
	builtinProxyVer++
}

// ClearBuiltinProxyDefs 清除某插件的声明（插件停止/卸载时调用）。
func ClearBuiltinProxyDefs(plugin string) {
	builtinProxyMu.Lock()
	defer builtinProxyMu.Unlock()
	if _, ok := builtinProxyDefs[plugin]; ok {
		delete(builtinProxyDefs, plugin)
		builtinProxyVer++
	}
}

// BuiltinProxyDefs 返回内置声明的快照（plugin → defs）。
func BuiltinProxyDefs() map[string][]ProxyDef {
	builtinProxyMu.RLock()
	defer builtinProxyMu.RUnlock()
	out := make(map[string][]ProxyDef, len(builtinProxyDefs))
	for k, v := range builtinProxyDefs {
		cp := make([]ProxyDef, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// BuiltinProxyVersion 是声明表的版本号，供反代层判断缓存是否过期。
func BuiltinProxyVersion() int64 {
	builtinProxyMu.RLock()
	defer builtinProxyMu.RUnlock()
	return builtinProxyVer
}

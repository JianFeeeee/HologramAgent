package pluginhost

// 把**真实的** plugin.Registry 接到 kernelboot.PluginQuiescer（伪关机用）。
//
// ## 为什么需要适配器而不是让 kernelboot 直接依赖 plugin
//
// kernelboot 是装配内核，plugin 是插件系统 —— 后者依赖前者（模块声明依赖），
// 反向 import 就成环。⇒ 用一个小接口 + 适配器隔开。
//
// ## ★ 为什么是 StopPlugin/StartPlugin，不是 StopAll
//
// 三态生命周期早已存在（自动启动 / 启用但不自动启动 / 禁用），而
// `StopPlugin`/`StartPlugin` 是**运行态**操作：
//
//	· 不写 disabled_plugins ⇒ 不改用户决定
//	· 不置 shuttingDown    ⇒ 之后还能重新加载（**可逆**）
//	· 会摘掉注册面         ⇒ 停完模型看不到也调不到（彻底）
//
// `StopAll` 完全相反：置 shuttingDown 后插件**永久无法再加载**，
// 那是进程退出用的。用它做伪关机 = 关了就起不来。
//
// ## 为什么跳过自身与 UI
//
// `refuseSelfLock` 已经拒绝了 pluginmgr（自锁，停了就没法再启）与 webui
// （停了 UI 就失联，迁移进度看不见）。这里**主动跳过**而不是去撞错误：
// 迁移期间我们需要 UI 报告进度，撞错只会让日志充满噪声。

import "github.com/JianFeeeee/HomeAgent/internal/plugin"

// RegistryQuiescer 适配 *plugin.Registry → kernelboot.PluginQuiescer。
type RegistryQuiescer struct{ Reg *plugin.Registry }

// StopPluginRuntime 运行态停掉一个插件（不改持久状态）。
func (r RegistryQuiescer) StopPluginRuntime(name string) error {
	return r.Reg.StopPlugin(name)
}

// StartPluginRuntime 运行态拉起一个插件。
func (r RegistryQuiescer) StartPluginRuntime(name string) error {
	return r.Reg.StartPlugin(name)
}

// RunningPlugins 返回当前在运行的插件名。
//
// ★ 排除 pluginmgr 与 webui：前者停了无法再启（自锁），后者是迁移进度的
// 唯一可视通道。这与 refuseSelfLock 的口径一致，但在这里**提前排除**
// 而不是靠报错兜底（报错会进入 BeginMigration 的回滚路径，把正常迁移搞挂）。
func (r RegistryQuiescer) RunningPlugins() []string {
	// Registry.List() 返回 r.plugins 的键集 = **当前已加载并运行**的插件
	// （StopPlugin 会从该 map 删除 ⇒ 停掉的自然不在其中）。
	all := r.Reg.List()
	out := make([]string, 0, len(all))
	for _, n := range all {
		if n == "pluginmgr" || n == "webui" {
			continue
		}
		out = append(out, n)
	}
	return out
}

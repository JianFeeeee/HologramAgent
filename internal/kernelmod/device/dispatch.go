package device

// device 模块对内置工具路由的**对外入口**。
//
// ## 为什么路由表本体在 internal/toolroute
//
// 依赖方向限制：agent/core 需要调用路由，而 kernelmod/device 会 import
// kernelboot（→ agent/core）。若路由表放在本包，core 就会 import 本包，
// 形成 core → device → kernelboot → core 的**循环**。
//
// 所以：路由表放低层包（toolroute，只依赖 agent/api），
// 本包保留对外入口 —— 模块的所有权仍然在这里（它是 device 模块的
// 第一个被搬出来的部件），只是实现下沉以避免环。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/toolroute"
)

// Dispatch 按工具名选出族处理器并执行（device 模块的对外入口）。
func Dispatch(h api.BuiltinToolHandlers, tc api.ToolCall, turnScenes []string) (string, bool) {
	return toolroute.Dispatch(h, tc, turnScenes)
}

package agent

// extslot.go —— **通用扩展槽**（docs §6）。
//
// ## 它是什么
//
//	内核持有【槽】—— 通用，不是某个能力专用
//	      ↑
//	被选中为主 agent 的类型，在 Initialize 里注册：
//	   · 一批**工具**（名字 / schema / 标记）
//	   · 它们的**处理钩子**（注册时逻辑即交出）
//	      ↓
//	这些工具进工具面，**任何 agent 都能调用**
//	      ↓
//	内核一视同仁处理：同路由 / 同安全闸 / 同并发判据 / 同超时 / 同脱敏
//	      ↓
//	调那个已交出的钩子 → 返回 map
//
// ## 三条硬规则（docs §6.2）
//
//  1. **只有被选中为主 agent 的那个类型才注册** —— 其他类型根本不注册
//  2. **处理逻辑在注册时交出**（钩子交出去，不回主 agent 的 agentloop）
//  3. 主 agent 在 `admin_child` 创建子 agent 时用参数**划定暴露哪些槽内工具**
//
// ## 为什么钩子交出去就不越界
//
// 「读谁的私有状态，就归谁执行」仍然成立 —— 槽内工具能被内核执行，
// 是因为钩子在注册时已经交出去了。内核只按统一的执行链调它，
// 不知道它背后读的是谁的图库。

import (
	"fmt"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// RegisterExtSlot 把一组槽内工具交给内核（主 agent 的 Initialize 调它）。
//
// 登记进**同一张工具表**：于是它们在工具面里出现、被 `DispatchTool` 分派、
// 受同一个安全闸与并发判据约束 —— 与插件工具没有任何区别。
// 调用它们也只走 `RunToolBatch`（工具调用的**唯一**入口），不另开路径。
//
// ★ 归属记成本 agent 的 id：槽内工具是"它交出来的能力"，
//
//	而不是内核自带的（`Owner: "core"`）。
func (a *Agent) RegisterExtSlot(tools []registry.Tool) error {
	if a == nil {
		return fmt.Errorf("agent: RegisterExtSlot 需要非 nil 的 agent")
	}
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("agent %s: 槽内工具缺名字", a.id)
		}
		if t.Handle == nil {
			return fmt.Errorf("agent %s: 槽内工具 %q 缺处理钩子（注册时必须交出逻辑）", a.id, t.Name)
		}
		t.Owner = string(a.id)
		// ★ 标记为槽工具：执行走内核设施，归属只用于溯源与暴露范围。
		t.Slot = true
		// ★ 同名槽内工具**覆盖**：多个主 agent 实例（测试 / 探针 / 重启）
		//   交的是**同一能力**（graph_recall 就是主 agent 的图记忆召回）。
		//   "后注册者赢"是这里唯一合理的语义 —— 拒绝会让"第二个实例起不来"，
		//   而它们本来就不该并存（一个内核一个主 agent）。
		if err := registry.RegisterToolReplace(t); err != nil {
			return fmt.Errorf("agent %s: 注册槽内工具 %q 失败: %w", a.id, t.Name, err)
		}
	}
	a.Logf("registered %d extension slot tool(s)", len(tools))
	return nil
}

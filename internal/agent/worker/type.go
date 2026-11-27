package worker

// type.go —— **工作式子类型**的注册项（Descriptor）。
//
// ## 为什么它必须是注册项（而不是"父调一个工具就起 goroutine"）
//
// 用户反复裁定：父子通信只有两条协议
//
//	父 → 子   admin_child(type, action, meta, arg) + admin_child_help + admin_child_list
//	子 → 父   CallParent(level, kind, text, meta)
//
// 而旧的 `spawn_child` 走的是第三条：父调一个工具 → worker 自己起 goroutine。
// 后果（都是真缺陷，不是口味问题）：
//
//	· 不进 `child.Registry` / `InstanceManager` ⇒ 不持久化（重启丢）
//	· `admin_child_list` 看不到它
//	· `DeclareChildPolicy` 没有它（子执行体**无约束**）
//	· `StopAll` 收不掉它（主 agent 退出即孤儿）
//
// 现在创建工作式子 = `admin_child(type="worker", action="create", arg={task, ...})`
// → 本包的 `Construct` → 走内核骨架建实例。
//
// ## 工作式子**不是**长期驻留的
//
// 它是一次性局部执行体：跑完就退役。所以它不注册长期运行期循环
// （`RuntimeHooks.Start/Stop` 只做最小接线）。

import (
	"fmt"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// TypeName 是本类型的注册名（`admin_child(type="worker", ...)` 用它）。
const TypeName = "worker"

func init() {
	if err := registry.Register(workerDescriptor()); err != nil {
		panic("agent/worker: " + err.Error())
	}
}

func workerDescriptor() registry.Descriptor {
	return registry.Descriptor{
		Meta: registry.Meta{
			Type:        TypeName,
			Description: "一次性局部子 agent：在独立上下文里完成一件具体任务后退役（不常驻、不接通道）",
			AgentCap:    agentIO.CapText,
		},
		Actions: []registry.Action{
			{
				Name:        "inspect",
				Description: "查看某个工作式子任务的进度或最终产出（结果幂等：重复查询返回同一份）",
				Args: map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "任务 id（create 时返回）"},
				},
				Required: []string{"id"},
			},
		},
		// Initialize：装配本类型的运行时钩子（与其余三个内置类型同一路子）。
		//
		// ★ 此前 worker **没有** Initialize —— 它能跑是因为恰好不需要装
		//   任何服务。那是"靠巧合工作"：一旦它需要一个钩子（例如恢复），
		//   就会静默没有，而且没有人会发现。
		Initialize: func(instance any) error {
			a, _ := instance.(*agent.Agent)
			if a == nil {
				return fmt.Errorf("worker: 实例不是 *agent.Agent（装配错误）")
			}
			h := a.Hooks()
			// 工作式子是一次性执行体：不跨重启保留上下文（明确策略）。
			h.Runtime.Restore = func(_ *agent.Agent, _ string) error { return nil }
			a.SetHooks(h)
			return nil
		},
		Construct: func(host registry.Host, arg map[string]interface{}) (string, error) {
			m := Of(hostRoot(host))
			if m == nil {
				return "", fmt.Errorf("worker: 宿主不是实例管理器")
			}
			return m.spawnFromAdmin(arg)
		},
		Admin: func(host registry.Host, action string, arg map[string]interface{}) (string, error) {
			m := Of(hostRoot(host))
			if m == nil {
				return "", fmt.Errorf("worker: 宿主不是实例管理器")
			}
			return m.adminWorker(action, arg)
		},
		Help: func(host registry.Host) string {
			return workerHelp()
		},
	}
}

// hostRoot 从 `registry.Host` 取根 agent（实例管理器的宿主）。
//
// ★ 与 resident/rtchild 的 `Of(host)` 同一形态：注册表只给 Host 接口，
//
//	类型自己断言回实例管理器再取根 agent —— 注册表不认识任何具体类型。
func hostRoot(host registry.Host) *agent.Agent {
	im, ok := host.(*agent.InstanceManager)
	if !ok || im == nil {
		return nil
	}
	root, _ := im.Root().(*agent.Agent)
	return root
}

// workerHelp 是 `admin_child_help(type="worker")` 的正文。
func workerHelp() string {
	return strings.TrimSpace(`
工作式子（worker）= 一次性局部执行体。

【创建】admin_child(type="worker", action="create", arg={...})
  task        要它完成的任务描述（必填；要完整、含必要背景）
  max_turns   最大工具轮数（默认 5，范围 1-30）
  channel     完成通知回到哪个通道（默认 cli）

【查看】admin_child(type="worker", action="inspect", arg={id})
  结果**幂等**：重复查询返回同一份（不会"读到即删"）。

【生命周期】
  · 不进 inputch 队列、不接通道 —— 它只跑这一件任务
  · 完成后由它自己 CallParent 上报（父侧看到的是正常上报，不需要父侧另存表）
  · 主 agent 退出时由内核 StopAll 统一收掉（不留孤儿）
`)
}

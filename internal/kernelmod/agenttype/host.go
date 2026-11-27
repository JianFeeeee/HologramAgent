package agenttype

// host.go —— 插件类型组装器（**唯一的适配点**）。
//
// 中立面（`pubsdk.AgentTypeDef`）↔ 内核实类型（`agent.Agent` / `registry.Descriptor`）
// 只在这里转换。插件永远看不到内核类型。

import (
	"fmt"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// Host 是组装器实例（无状态：每个类型注册时按需组装）。
type Host struct{}

// New 构造组装器（装配期一次）。
func New() *Host { return &Host{} }

// Assemble 把插件声明组装成内核注册项并登记。
//
// 返回值 = 拒绝原因（同名已注册 / 声明不完整 / Spec 非法）。
// **不静默成功**：静默成功会让"插件以为注册了、实际没有"变成查不出的谜。
func (h *Host) Assemble(plugin string, raw any) error {
	def, ok := raw.(pubsdk.AgentTypeDef)
	if !ok {
		return fmt.Errorf("agenttype: 载荷不是 sdk.AgentTypeDef（拿到 %T）—— 插件不认识的形状", raw)
	}
	if def.Type == "" {
		return fmt.Errorf("agenttype: 类型名不能为空")
	}
	if def.Loop == nil {
		return fmt.Errorf("agenttype: 类型 %q 缺少 Loop（没有它这个类型不知道怎么思考）", def.Type)
	}
	if err := validateSpec(def.Type, def.Spec); err != nil {
		return err
	}
	capBits, err := agentCapOf(def)
	if err != nil {
		return err
	}
	return registry.Register(toDescriptor(def, capBits))
}

// agentCapOf 把插件声明的**中立能力名**映射成内核能力位。
//
// ★ 这是**唯一**的转换点（与 `toDescriptor` 同一纪律）：插件看不到位类型，
//
//	内核不认识插件字符串。空声明 = 只要求基础文本能力。
//	拼错的能力名**响亮拒绝**（静默忽略会让"我声明了 rt 但划不进 rt 通道"变成谜）。
func agentCapOf(def pubsdk.AgentTypeDef) (agentIO.OutputCapability, error) {
	if len(def.Spec.Capabilities) == 0 {
		return agentIO.CapText, nil
	}
	var bits agentIO.OutputCapability
	for _, name := range def.Spec.Capabilities {
		bit, ok := agentIO.CapabilityByName(name)
		if !ok {
			return 0, fmt.Errorf("agenttype: 类型 %q 声明了未知能力 %q（可用: %s）",
				def.Type, name, agentIO.CapabilityNames())
		}
		bits |= bit
	}
	return bits, nil
}

// validateSpec 校验 Spec 的取值（拼错会静默退化，所以必须拦）。
func validateSpec(typeName string, spec pubsdk.AgentTypeSpec) error {
	switch spec.Scheduler {
	case "", pubsdk.SchedulerNone, pubsdk.SchedulerStandard:
	default:
		return fmt.Errorf("agenttype: 类型 %q 的 Scheduler=%q 非法（可用 none|standard）", typeName, spec.Scheduler)
	}
	switch spec.Context {
	case "", pubsdk.ContextNone, pubsdk.ContextWindow, pubsdk.ContextRelevance:
	default:
		return fmt.Errorf("agenttype: 类型 %q 的 Context=%q 非法（可用 none|window|relevance）", typeName, spec.Context)
	}
	switch spec.Memory {
	case "", pubsdk.MemoryNone, pubsdk.MemoryLight, pubsdk.MemoryFull:
	default:
		return fmt.Errorf("agenttype: 类型 %q 的 Memory=%q 非法（可用 none|light|full）", typeName, spec.Memory)
	}
	return nil
}

// toDescriptor 是中立声明 → 内核注册项的转换。
func toDescriptor(def pubsdk.AgentTypeDef, cap agentIO.OutputCapability) registry.Descriptor {
	actions := make([]registry.Action, 0, len(def.Actions))
	for _, a := range def.Actions {
		actions = append(actions, registry.Action{
			Name: a.Name, Description: a.Description,
			Args: a.Args, Required: a.Required,
		})
	}
	return registry.Descriptor{
		Meta: registry.Meta{
			Type:          def.Type,
			Description:   def.Description,
			NeedsInputCh:  def.NeedsInputCh,
			NeedsOutputCh: def.NeedsOutputCh,
			AgentCap:      cap,
		},
		Actions: actions,
		Construct: func(host registry.Host, arg map[string]interface{}) (string, error) {
			return construct(def, host, arg)
		},
		Initialize: func(instance any) error {
			return initialize(def, instance)
		},
		Admin: func(host registry.Host, action string, arg map[string]interface{}) (string, error) {
			return admin(def, host, action, arg)
		},
		Help: func(host registry.Host) string {
			return help(def, host)
		},
	}
}

// 下面三个是适配体（实现在 runtime.go / construct.go）。
var (
	construct  func(def pubsdk.AgentTypeDef, host registry.Host, arg map[string]interface{}) (string, error)
	initialize func(def pubsdk.AgentTypeDef, instance any) error
	admin      func(def pubsdk.AgentTypeDef, host registry.Host, action string, arg map[string]interface{}) (string, error)
	help       func(def pubsdk.AgentTypeDef, host registry.Host) string
)

// Unregister 撤销一个类型（仍有存活实例 ⇒ 拒绝并列出 id）。
//
// 存活实例由实例管理器给出（与事件路径一致）。
func (h *Host) Unregister(typeName string, alive []string) error {
	return registry.Unregister(typeName, alive)
}

var _ = agent.TypeNameMain

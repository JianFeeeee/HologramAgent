package registry

// eventbus.go —— **类型注册的 EventBus 入口**（用户裁定 2026-11-18）。
//
// ## 用户原话
//
// > 「注册子 agent 类型的接口只对内置插件暴露，内置插件通过 eventbus，
// >    向 agent 基座（或者说是 agent 管理器）发布行为动态注册撤销新的 agent 类型」
//
// ## 为什么载荷能携带真正的 Go 函数值（方案 B）
//
// 内置插件是**线程化**的：与内核同一进程、同一地址空间。它们拿到的是
// `sdk.SDK` 的方法集，可以把自己的 Go 函数直接放进事件载荷。
// 跨进程插件（lua / proc）只能送字符串 —— 它们**在结构上**就注册不出类型
// （Construct 为空即被拒），不需要额外手段去挡。
//
// 所以"只对内置插件暴露"不是一句约定，而是两个机制叠加的**结果**：
//
//	① security.EventPolicy 允许发布这两条事件（否则内置插件也发不出）
//	② 订阅侧再查 IsBuiltinPlugin（运行期事实，静态策略表表达不了）
//
// ## 为什么订阅侧还要再查一次
//
// 若某个外部插件能发同名字符串事件，它会得到一个"Construct 是 nil"的
// 空注册 —— 那是一次无意义的越界，但**不该被静默接受**：报错能让人看见
// 有人在试探边界。而更重要的理由：IsBuiltinPlugin 是运行期事实（插件可以
// 在运行期被换掉），静态白名单无法表达它。

import "fmt"

// 事件类型名（与 security.EventTypeRegister / 公开 SDK 逐字一致）。
const (
	EventTypeRegister   = "agent.type.register"
	EventTypeUnregister = "agent.type.unregister"
)

// EventPayloadKey 是把 Descriptor 塞进事件载荷时用的键。
//
// ★ 为什么用固定键而不是把 Descriptor 摊平进载荷：事件载荷的**顶层键**
//
//	有数量上限（security.EventPolicy.MaxPayloadKeys），而 Descriptor 的
//	Actions 是个列表 —— 摊平会把"一个注册"变成几十个键。固定键也让
//	"载荷结构"与"策略上限"互不牵连。
const EventPayloadKey = "descriptor"

// RegisterRequest 是一条注册请求（内置插件构造后放进事件载荷）。
//
// ★ 与 Descriptor 分开的理由：需要额外携带**声明者**，以便错误文案
//
//	能指出是谁注册的、以及撤销时校验"只能撤自己注册的"。
type RegisterRequest struct {
	// Plugin 是声明者（内置插件名）。
	Plugin string
	// Descriptor 是类型注册项（含真正的 Go 函数值）。
	Descriptor Descriptor
}

// HandleRegisterEvent 处理一条 `agent.type.register` 事件。
//
// 返回 (声明者, 中立类型声明, 错误)。声明与错误分两路：调用方要先验
// 声明者再交给组装器（两件事的责任不同）。
//
// ★ 本函数**不组装**：它只做"能不能收"的判定（内置插件 + 载荷形状）。
//
//	组装（造实例 / 挂回调）在内核模块 `kernelmod/agenttype` —— registry
//	是纯注册表，不能 import agent 实现（那会让注册表变成基座的一部分）。
func HandleRegisterEvent(eventType string, payload map[string]interface{}, isBuiltin func(string) bool) (string, any, error) {
	plugin := str(payload, "plugin")
	if plugin == "" {
		return "", nil, fmt.Errorf("registry: 注册事件缺少 plugin 字段（无法判定声明者）")
	}
	if isBuiltin != nil && !isBuiltin(plugin) {
		// ★ 这一道是**运行期事实**：跨进程插件送不进函数值，但可能发同名事件。
		return "", nil, fmt.Errorf("registry: 插件 %q 不是内置插件（线程化），无权注册子 agent 类型", plugin)
	}
	def, err := decodeRegisterPayload(payload)
	if err != nil {
		return "", nil, err
	}
	return plugin, def, nil
}

// HandleUnregisterEvent 处理一条 `agent.type.unregister` 事件。
//
// alive 是该类型**当前存活实例**的 id 列表（由实例管理器给出）。
// 非空 ⇒ 拒绝并列出 id（撤销会让实例的管理/销毁实现变成悬空调用）。
func HandleUnregisterEvent(payload map[string]interface{}, alive []string) error {
	typeName := str(payload, "type")
	if typeName == "" {
		return fmt.Errorf("registry: 撤销事件缺少 type 字段")
	}
	return Unregister(typeName, alive)
}

// decodeRegisterPayload 从事件载荷里取出**类型声明**。
//
// ★ 载荷里必须是**真正的 Go 值**（不是 map 反序列化来的）：函数值无法从
// JSON 还原。拿到的若是 map，说明发布方是跨进程插件 —— 明确拒绝并说明原因，
// 而不是静默当空注册。
//
// ★ 这里收的是**中立声明**（`pubsdk.AgentTypeDef`），不是内核的 Descriptor：
//
//	插件不认识内核类型，组装（含造实例）由 `kernelmod/agenttype` 做。
//	（判据 `TestPayloadIsNotDeserialized` 钉住"拒绝 map 载荷"。）
func decodeRegisterPayload(payload map[string]interface{}) (any, error) {
	raw, ok := payload[EventPayloadKey]
	if !ok {
		return nil, fmt.Errorf("registry: 注册事件缺少 %q 载荷", EventPayloadKey)
	}
	if _, isMap := raw.(map[string]interface{}); isMap {
		return nil, fmt.Errorf(
			"registry: %q 载荷是 map（反序列化产物）—— 跨进程插件无法携带 Go 函数值，"+
				"只有内置插件（线程化）能注册子 agent 类型", EventPayloadKey)
	}
	return raw, nil
}

// NewRegisterPayload 构造一条注册事件的载荷（内置插件用）。
func NewRegisterPayload(plugin string, d Descriptor) map[string]interface{} {
	return map[string]interface{}{
		"plugin":        plugin,
		EventPayloadKey: d,
	}
}

// NewUnregisterPayload 构造一条撤销事件的载荷。
func NewUnregisterPayload(plugin, typeName string) map[string]interface{} {
	return map[string]interface{}{
		"plugin": plugin,
		"type":   typeName,
	}
}

// str 取字符串字段（去空白）。
func str(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return trim(s)
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// RegisterTypesFrom 是**装配期**的批量注册入口（内置类型模块用 init()）。
//
// ★ 与事件路径的关系：内置类型（resident / rt / 其他）在包的 init() 里
//
//	直接调它，不必绕事件总线 —— 它们在编译期就存在，"运行期登记"这件事
//	对它们没有意义。而**运行期**新增的类型（插件提供的）才走事件。
//	两条路最终都到 Register，所以只有一张表。
func RegisterTypesFrom(plugin string, ds ...Descriptor) error {
	for _, d := range ds {
		if err := Register(d); err != nil {
			return fmt.Errorf("%s: %w", plugin, err)
		}
	}
	return nil
}

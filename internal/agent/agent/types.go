package agent

// types.go —— agent **类型注册表**（用户裁定 2026-11-17）。
//
// ## 用户原话
//
// > 「同时每个 agent 都有对应的实例化调用，没有被调用就没有这个 agent 的实例，
// >    内核启动后自动实例化一个主 agent」
//
// ## 由此确定的两条规则
//
//  1. **类型必须先注册**（`RegisterType`），并且**只能经实例化调用**
//     （`Instantiate`）产生实例。没有那条调用 ⇒ 该类型根本没有实例。
//     基座里不存在"顺手 new 一个"的路径：`New` 是**构造原语**，类型层
//     （main / resident / rtchild / worker）各自用它在自己的工厂里组装，
//     并把工厂登记到本表。
//  2. **内核启动时自动实例化一个主 agent**：`kernelmod/agent` 在 `Init`
//     里 `Instantiate("main", …)` 正好一次。主 agent 不是特例 —— 它和
//     驻留子 / rtchild 走**同一张表、同一个调用**。
//
// ## 为什么注册表放在 agent 包而不是 kernelapi
//
// 工厂的签名要收 `AgentConfig`（agent 包的公开类型）。放进 kernelapi 就得
// 让接口包认识一个具体类型 —— 那正是"接口包混合实现"的形态。
// 注册表本身是纯数据（名字 → 工厂），它属于 agent 模块，而不是内核设施表。
//
// 对插件暴露的那条路走 SDK + EventBus（插件经 host 声明"我要一个这类子 agent"，
// 存续由插件负责），见 docs/zh/resident-subagent-design.md 的通道语义。

import (
	"fmt"
	"sort"
	"sync"
)

// Factory 是某类 agent 的**实例化调用**（组装基座 + 注册该类型的回调）。
//
// 参数是调用方给的构造参数；返回已装配完成的实例（含已挂的 hooks）。
type Factory func(cfg AgentConfig) (*Agent, error)

var (
	typeMu       sync.RWMutex
	typeRegistry = map[string]Factory{}
	typeOrder    []string
)

// RegisterType 登记一类 agent 的实例化调用（装配期一次，通常在包 init 里）。
//
// 同名重复登记会被拒并返回错误：两个工厂同名意味着"到底哪个生效"取决于
// 调用顺序 —— 那是不可判定的故障。
func RegisterType(name string, f Factory) error {
	if name == "" {
		return fmt.Errorf("agent: RegisterType with empty name")
	}
	if f == nil {
		return fmt.Errorf("agent: RegisterType(%q) with nil factory", name)
	}
	typeMu.Lock()
	defer typeMu.Unlock()
	if _, dup := typeRegistry[name]; dup {
		return fmt.Errorf("agent: type %q already registered", name)
	}
	typeRegistry[name] = f
	typeOrder = append(typeOrder, name)
	return nil
}

// Instantiate 是**唯一的实例化入口**：按类型名调用对应工厂。
//
// 未注册的类型返回错误（不静默降级，也不隐式创建）。
func Instantiate(name string, cfg AgentConfig) (*Agent, error) {
	typeMu.RLock()
	f, ok := typeRegistry[name]
	typeMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("agent: type %q is not registered — no factory call was made for it, "+
			"so this agent type has no instance by design (see types.go)", name)
	}
	a, err := f(cfg)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, fmt.Errorf("agent: type %q factory returned nil", name)
	}
	return a, nil
}

// RegisteredTypes 返回已登记的 agent 类型名（排序后，便于诊断与断言）。
func RegisteredTypes() []string {
	typeMu.RLock()
	defer typeMu.RUnlock()
	out := make([]string, len(typeOrder))
	copy(out, typeOrder)
	sort.Strings(out)
	return out
}

// IsRegistered 报告某类型是否已登记（诊断用）。
func IsRegistered(name string) bool {
	typeMu.RLock()
	defer typeMu.RUnlock()
	_, ok := typeRegistry[name]
	return ok
}

// TypeNameMain 是**主 agent** 的类型名（内核启动时自动实例化这一次）。
const TypeNameMain = "main"

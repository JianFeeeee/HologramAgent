// Package device 是 device 模块：**工具数据面的统一入口**。
//
// ## 定位（用户裁定 2026-10-26）
//
// > toolcall 部分作为一个完整的 device 子模块，管理全部的 tool、inputch 和
// > outputch。
//
// 第一阶段（本文件）先建立**正式模块 + 数据面对象 + KernelState 挂载点**，
// 工具执行仍委托 agent —— 行为零变化，但边界成立、安全检查有唯一落点。
// 第二阶段再把 tooldefs/toolcall 的实现迁进来（见 plan.md §七）。
//
// 为什么不一锅端：搬 2000+ 行工具实现的同时还有 1000+ 判据在跑，
// 任何一处不一致都无从定位。先把**边界与闸门**立起来，再搬实现。
//
// ## 分层位置
//
//	config → security → memory/knowledge → stagehost → agent → device → pluginhost
//
// ## 安全纪律
//
// device 不实现安全逻辑，也不持有 raw security.Store：
// 只通过 ks.Security 的 Runtime 过境。任何"顺手直接调 store"的写法都是
// 绕过闸门（判据见 internal/security/gate_test.go）。
package device

import (
	"fmt"
	"log"
	"sort"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	sec "github.com/JianFeeeee/HologramAgent/internal/security"
)

// Module 提供设备/工具数据面。
type Module struct{}

func (Module) Name() string { return "device" }

func (Module) Requires() []string {
	return []string{kernelmod.FacAgent, kernelmod.FacSecurity}
}

func (Module) Provides() []string { return []string{kernelmod.FacDevice} }

func (Module) Init(ks *kernelboot.KernelState) error {
	if ks.Agent == nil {
		return fmt.Errorf("device module: ks.Agent is nil (requires %s)", kernelmod.FacAgent)
	}
	if ks.Security == nil {
		// 安全模块必须已装配（配置关闭时是 Disabled Runtime，**不是** nil）。
		// nil 表示装配顺序错了 —— 那会让本模块"看起来装好了但没有闸门"。
		return fmt.Errorf("device module: ks.Security is nil - the security module must be " +
			"assembled first (when disabled it installs a Disabled Runtime, not nil)")
	}

	rt := New(ks.Agent, ks.Security)
	ks.Device = rt
	ks.Agent.MountDevice(rt)

	if ks.EventBus != nil {
		ks.EventBus.Publish(&events.Event{
			Type:   events.EventDeviceReady,
			Source: "kernelmod/device",
			Payload: map[string]interface{}{
				"tools":   rt.ToolCount(),
				"secured": rt.Secured(),
			},
		})
	}
	log.Printf("[device] ready: %d tools, security=%v", rt.ToolCount(), rt.Secured())
	return nil
}

func init() { kernelmod.Register(Module{}) }

// Runtime 是 device 数据面。
type Runtime struct {
	host api.ToolHost
	sec  *sec.Runtime
}

// New 构造 device 数据面。
func New(host api.ToolHost, s *sec.Runtime) *Runtime {
	return &Runtime{host: host, sec: s}
}

// Security 返回安全运行时（只读，供过境用；不是变量库本身）。
func (r *Runtime) Security() *sec.Runtime {
	if r == nil {
		return nil
	}
	return r.sec
}

// Secured 报告安全审查是否真的生效（状态面用）。
func (r *Runtime) Secured() bool { return r != nil && r.sec != nil && r.sec.Enabled() }

// Tools 返回全部工具定义（按名字排序，便于人读与稳定输出）。
func (r *Runtime) Tools() []api.ToolDefinition {
	if r == nil || r.host == nil {
		return nil
	}
	out := r.host.ToolDefinitions()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToolCount 返回工具数。
func (r *Runtime) ToolCount() int { return len(r.Tools()) }

// Execute 执行一次工具调用，并对**返回值**做出向边界处理。
//
// ★ 这是 device 的执行入口，但**不是**安全审查的唯一位置：
//   - 参数校验 / 工具授权 / 设备授权 / 超时 / panic 恢复 → ToolHost（agent/core）
//   - 明文 → 别名（出向）、别名 → 明文（执行前回填） → agent 的 stage 管道
//   - 返回值清洗（入向）→ 本函数与 agent 的 defer 双保险
//
// 为什么入向要在两处都做：agent 的 defer 覆盖全部 return 出口（含 panic），
// 而本函数覆盖"经 device 的调用"。少一处就可能有一条路径不洗。
func (r *Runtime) Execute(name string, args map[string]interface{}) (string, error) {
	if r == nil || r.host == nil {
		return "", fmt.Errorf("device: tool host is not mounted")
	}
	out, err := r.host.ExecuteTool(name, args)
	if err != nil {
		return out, err
	}
	if r.sec != nil {
		cleaned, n := r.sec.ScrubInbound(sec.Inspection{
			Direction: sec.DirectionInbound,
			Stage:     "tool_result",
			Tool:      name,
		}, out)
		if n > 0 {
			return cleaned, nil
		}
	}
	return out, nil
}

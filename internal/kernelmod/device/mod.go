// Package device 是 device 模块：**工具数据面的统一入口**。
//
// ## 定位（用户裁定 2026-10-26）
//
// > toolcall 部分作为一个完整的 device 子模块，管理全部的 tool、inputch 和
// > outputch。
//
// 第一阶段（本文件）先建立**正式模块 + 数据面对象 + KernelState 挂载点**，
// 工具执行仍委托 agent —— 行为零变化，但边界成立、安全检查有唯一落点。
// 第二阶段再把 tooldefs/toolcall 的实现迁进来（阶段划分见
// docs/zh/kernel-modularization-design.md）。
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
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"log"
	"sort"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sec "github.com/JianFeeeee/HologramAgent/internal/security"
)

// Module 提供设备/工具数据面。
type Module struct{}

func (Module) Name() string { return "device" }

// Requires 声明能开工的前置。
//
// ★ **不再依赖 `FacAgent`**（用户裁定 2026-11-27）。此前它依赖 agent 只为了
// `host.MountDevice(rt)` —— 把数据面**推**给 agent。那条路径的消费方为零
// （`a.Device()` 全仓 0 处调用），却让 device 倒挂到基座之上，而 `FacDevice`
// 反过来也没人取。
//
// 现在方向是**拉**：agent 从 `FacDevice` 取自己的虚拟设备管理器。
func (Module) Requires() []string {
	return []string{
		kernelmod.FacKernelIO,       // IOManager（tool 面 + ch 面）
		kernelmod.FacSecurity,       // 安全闸（工具授权 / 路径 / 网络 / 审查）
		kernelapi.FacPluginRegistry, // plghost（插件可见性）
		kernelmod.FacToolReg,        // 插件工具表
	}
}

// Provides 声明本模块产出的设施。
//
// ★ `FacToolExec` 也从本模块产出（用户裁定 2026-11-27）：
//
//	「device 就应该是那个执行工具的模块」
//
// 此前它由一个独立的 `kernelmod/toolexec` 平行提供 —— 那让"工具由谁执行"
// 有两个答案，而 device 反而退化成一个把数据面推给 agent、却**无人使用**的空壳。
// 工具执行链（路由三步 / 超时 / 并发 / panic 恢复 / 脱敏 / 安全闸）本来就是
// device 的职责（它是 tool + ch 的抽象），现在归位。
func (Module) Provides() []string {
	return []string{kernelmod.FacDevice, kernelapi.FacToolExec}
}

func (Module) Init(ks *kstate.State) error {
	// 四件设施：缺任何一件都响亮失败（不静默降级 —— 那会变成
	// "看起来装好了但没有闸门/没有工具表"）。
	iom, ok := kernelmod.MustValueAny(ks, kernelapi.FacKernelIO).(*agentIO.IOManager)
	if !ok || iom == nil {
		return fmt.Errorf("device module: facility %q 未提供（IO 是 device 的资源池）", kernelapi.FacKernelIO)
	}
	secImpl, ok := ks.Require(kernelapi.FacSecurity)
	if !ok || secImpl == nil {
		// 安全模块必须已装配（配置关闭时是 Disabled Runtime，**不是** nil）。
		return fmt.Errorf("device module: facility %q 未提供 —— security 必须先生成"+
			"（关闭时是 Disabled Runtime，不是 nil）", kernelapi.FacSecurity)
	}
	secRT, _ := secImpl.(*security.Runtime)

	regImpl, ok := ks.Require(kernelapi.FacPluginRegistry)
	if !ok || regImpl == nil {
		return fmt.Errorf("device module: facility %q 未提供 —— plugin-host 必须先生成", kernelapi.FacPluginRegistry)
	}
	pluginReg, _ := regImpl.(*plugin.Registry)

	tools, ok := kernelmod.MustValueAny(ks, kernelapi.FacToolReg).(*kit.ToolRegistry)
	if !ok || tools == nil {
		return fmt.Errorf("device module: facility %q 未提供", kernelapi.FacToolReg)
	}

	dev := NewDevice(iom, pluginReg, tools, NewExecutor(iom, secRT.AsGate(), tools))
	ks.Provide(kernelapi.FacDevice, dev)
	ks.Provide(kernelapi.FacToolExec, dev.Executor())

	if busImpl, ok := ks.Require(kernelapi.FacEventBus); ok {
		if bus, _ := busImpl.(kernelapi.EventBus); bus != nil {
			bus.Publish(&events.Event{
				Type:   events.EventDeviceReady,
				Source: "kernelmod/device",
				Payload: map[string]interface{}{
					"tools":   len(dev.Tools()),
					"secured": secRT != nil && secRT.Enabled(),
				},
			})
		}
	}
	log.Printf("[device] ready: tool+ch manager (tools=%d, secured=%v)", len(dev.Tools()), secRT != nil && secRT.Enabled())
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

// ── 设施访问的薄适配（把 concrete 类型包成执行链的窄接口） ──

type ioAccess struct{ m *agentIO.IOManager }

func (a ioAccess) IO() *agentIO.IOManager { return a.m }

type securityAccess struct{ g kernelapi.SecurityGate }

func (a securityAccess) SecurityGate() kernelapi.SecurityGate { return a.g }

type toolAccess struct{ r *kit.ToolRegistry }

func (a toolAccess) ToolRegistry() *kit.ToolRegistry { return a.r }

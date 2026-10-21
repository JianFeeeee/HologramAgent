package sdk

import (
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// toolImpl 桥接 StageHost（插件工具）与 IOManager（设备/通道工具）。
type toolImpl struct {
	stageHost ToolSource
	iom       *agentIO.IOManager
}

func NewTool(sh ToolSource, iom *agentIO.IOManager) ToolAPI {
	return &toolImpl{stageHost: sh, iom: iom}
}

func (t *toolImpl) GetToolDefs() []ToolDef {
	if t.stageHost == nil {
		return nil
	}
	return t.stageHost.GetToolDefs()
}

func (t *toolImpl) GetAllTools() []ToolDef {
	if t.iom == nil {
		return nil
	}
	defs := t.iom.GetAllTools()
	out := make([]ToolDef, 0, len(defs))
	for _, d := range defs {
		// ⚠️ ParallelSafe 必须一并带出：它决定该工具能否被并发执行。
		// 此前这里漏了它 ⇒ 插件看到的设备工具一律"不可并发"，
		// 设备工具的并发声明等于对插件不可见。
		out = append(out, ToolDef{
			Name:         d.Name,
			Description:  d.Description,
			Parameters:   d.Parameters,
			ParallelSafe: d.ParallelSafe,
		})
	}
	return out
}

func (t *toolImpl) ExecuteTool(name string, args map[string]interface{}) (interface{}, error) {
	if t.stageHost != nil {
		if def := t.stageHost.ToolDef(name); def != nil {
			return t.stageHost.ExecuteTool(name, args)
		}
	}
	if t.iom != nil {
		ret, err := t.iom.ExecuteTool(name, args)
		if err == nil {
			return ret, nil
		}
		// io 没有该工具 ⇒ 试内置工具（方案 B）。
		// ⚠️ 只在「确实不存在」时才继续；io 的**执行失败**必须如实上抛，
		// 否则会把「设备离线」误报成「工具不存在」，让调用方按 missing
		// 策略跳过——这与 P3 修过的父 io 吞错误是同一族陷阱。
		if !agentIO.IsToolNotFound(err) {
			return nil, err
		}
	}
	// ② 内核内置工具
	if hasBuiltin(name) {
		return builtinExec(name, args)
	}
	return nil, agentIO.ToolNotFound(name)
}

var _ ToolAPI = (*toolImpl)(nil)

// ToolDefByName 按名字查任一来源的工具声明（StageHost 优先，再查 IO 设备）。
//
// 用途：插件在**运行前**判断目标工具是否存在、是否并发安全。工具是动态
// 注册的，"不存在"是常态（插件未加载/已卸载/崩溃），因此查不到一律返回
// nil 交由调用方按"不存在"处理——不得 panic。
func (t *toolImpl) ToolDefByName(name string) *ToolDef {
	if t == nil || name == "" {
		return nil
	}
	if t.stageHost != nil {
		if def := t.stageHost.ToolDef(name); def != nil {
			return def
		}
	}
	if t.iom != nil {
		if def, ok := t.iom.ToolDefOf(name); ok {
			return &ToolDef{
				Name:         def.Name,
				Description:  def.Description,
				Parameters:   def.Parameters,
				ParallelSafe: def.ParallelSafe,
			}
		}
	}
	// ③ 内核内置工具（方案 B）：此前这里直接返回 nil ⇒ 插件既查不到也调不了
	//    memory_*/knowledge_*/doc_*/person_*。
	for _, d := range builtinDefs() {
		if d.Name == name {
			return &ToolDef{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Parameters,
				// ⚠️ 内置工具**默认不声明并发安全**：它们含 SQLite 写与召回，
				// 且部分依赖 per-agent 状态（驻留子是轻量内核，memory 为 nil）。
				// 保守默认 = 不并发，与它们今天的行为一致。
				ParallelSafe: false,
			}
		}
	}
	return nil
}

// CanUse 报告「执行该工具是否被授权」（D4）。
//
// 实现要点：**必须与 core.executeToolCallInner 里的那道闸同源同语义**，
// 否则两条路径判定不同，本身就是漏洞。判据 core.TestCanUseAgreesWithInnerPath
// 逐例比对两条路径的结论。
//
// 判据只有一条：设备类工具按 `device/<id>` 查当前 agent 的 allowedOutputs。
//
// ⚠️ device_id 缺失时**放行**（fail-open）——这是内核现状
// （core 的 TestDeviceToolAuth_* 依赖它）。两处行为已由
// TestCanUseMatchesInnerFailOpenOnMissingDeviceID 钉住；若将来要改成
// fail-closed，**必须两处同时改**，否则两条路径不一致。
func (t *toolImpl) CanUse(toolName string, args map[string]interface{}) bool {
	if t == nil || t.iom == nil {
		return true // 拿不到设备视图 ⇒ 不拦（与内核无 io 时的行为一致）
	}
	// 非设备工具不受此闸影响：闸的作用域必须窄，否则会把所有工具锁死。
	if _, isDeviceTool := t.iom.DeviceOfTool(toolName); !isDeviceTool {
		return true
	}
	id, _ := args["device_id"].(string)
	if id == "" {
		return true // fail-open，与内核一致
	}
	return t.canUseDevice(id)
}

// canUseDevice 是**可注入**的授权查询。
//
// 为何不直接在 toolImpl 里调 Agent：toolImpl 在 internal/sdk 包，
// 而 IsOutputAllowed 是 core.*Agent 的方法（core 反向依赖 sdk，
// sdk 不能依赖 core）。故内核在装配时把查询函数注入进来。
var deviceAuthQuery func(deviceID string) bool

// SetDeviceAuthQuery 注入"设备是否已授权"的查询（由内核在装配时调用）。
//
// 传 nil 表示尚未注入 ⇒ CanUse 对设备工具**放行**（保持存量行为不变）。
func SetDeviceAuthQuery(fn func(deviceID string) bool) { deviceAuthQuery = fn }

func (t *toolImpl) canUseDevice(deviceID string) bool {
	if deviceAuthQuery == nil {
		return true
	}
	return deviceAuthQuery(deviceID)
}

// builtinExec 执行内核内置工具。
//
// 放在本文件是因为要用 agentIO.ErrToolNotFound（sdk/tool.go 未导入 agentIO）。
// 未注入 provider 时返回**类型化**的 not-found，使调用方（如 seq 的
// missing 策略）能把它与「插件执行失败」区分开 —— 二者的处置完全不同。
func builtinExec(name string, args map[string]interface{}) (string, error) {
	if builtinProv == nil {
		return "", agentIO.ToolNotFound(name)
	}
	return builtinProv.Exec(name, args)
}

// hasBuiltin 报告该名字是否是可见的内置工具。
func hasBuiltin(name string) bool {
	for _, d := range builtinDefs() {
		if d.Name == name {
			return true
		}
	}
	return false
}

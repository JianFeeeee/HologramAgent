package agent

// composition.go —— 类型模块**组合基座**时的句柄读入口。
//
// ## 形态（用户裁定 2026-11-17 / 2026-11-19）
//
// 类型 `type M struct{ A *agent.Agent }`，需要基座什么就直接调 `m.A.Xxx()`。
// 这里只有**字段读入口**：没有 `Host` 接口、没有 `a.X → h.X` 的映射层。
//
// ## 什么该在这里
//
// 只有**基座/内核设施**：IO、LLM 提供者、图记忆装配面、数据目录、
// 子 agent 骨架、错误分类。四类 agent 都要用，不属于任何类型私有。
//
// 与它们相对：主 agent 的**私有装配**（记忆整理面 / 媒体 / 插件 / 人格 /
// 预算记账）不是设施，整体归 `mainagent`，基座一格里都不留。
//
// ★ 这里**没有** `Scheduler()`：调度器是 agentloop 的内部实现，
//   基座只负责把输入交给 agent 实例的输入口（io 单通道 + priority 标签）。

import (
	"context"
	"os"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// IO 返回本 agent 的 IO 管理器（输入入口 / 输出设备 / 通道登记表）。
func (a *Agent) IO() *agentIO.IOManager {
	if a == nil {
		return nil
	}
	return a.io
}

// ★ 2026-11-25 删除 `Provider()` / `SetProvider()` / `ProviderManager()`。
//
// 三个都是**裸句柄访问器**，零生产调用，但它们的存在本身就是后门：
// 类型只要写一句 `Config().ProviderManager().List()` 就能自己排源、自己
// 判能力，绕开候选链 / 跨源回退 / 可用性标记 / 错误分类 —— 而且不会报错。
//
// 正当入口各有一处：
//
//	装配     MountProviders(p, pm)        挂载模型源（§9.6 硬边界 2）
//	装配     SetProviderSecretStore(st)   变量库注入（本函数保留）
//	运行期   CompleteLLM / StreamLLM      类型只用这三个
//
// 判据：`TestBaseExposesNoRawProviderHandle`（reflect 方法集）。
// SetProviderSecretStore 把变量库注入源表（装配侧调用）。
//
// ★ 由基座转发而不是让类型拿 `ProviderManager()` 自己 SetSecretStore：
//
//	"源表要变量库"这件事是内核装配的内部细节，不该经类型的手。
func (a *Agent) SetProviderSecretStore(s kernelapi.SecretStore) {
	if a == nil || a.providerManager == nil {
		return
	}
	a.providerManager.SetSecretStore(s)
}

// PreferredProvider 返回本 agent 优先使用的源名（空 = 继承父 / 默认）。
//
// ★ 现读配置（docs §2：`PreferredProvider` 是配置快照，不存字段）。
// 空值语义：本 agent 没声明 ⇒ 继承父 / 走总默认源。
func (a *Agent) PreferredProvider() string {
	if a == nil {
		return ""
	}
	a.semanticMu.RLock()
	declared := a.preferredProvider
	a.semanticMu.RUnlock()
	if declared != "" {
		return declared
	}
	return a.CfgString(CfgKeyMainProvider, "")
}

// SystemPrompt 返回系统提示词**原始素材**（子继承父的那份）。
//
// ★ 这是素材，不是渲染结果：渲染在类型自己的包里做，渲染好的**完整上下文**
//
//	才交回基座。
func (a *Agent) SystemPrompt() string {
	if a == nil {
		return ""
	}
	// 父声明的优先（子继承父的那份），否则现读配置。
	a.semanticMu.RLock()
	declared := a.systemPrompt
	a.semanticMu.RUnlock()
	if declared != "" {
		return declared
	}
	return a.CfgString(CfgKeySystemPrompt, "")
}

// StreamLLM 暴露流式补全（类型的唯一流式入口）。
//
// 类型的 agentloop **阻塞地**消费这条流直到 Done —— 那一轮结束才进下一轮。
func (a *Agent) StreamLLM(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return a.streamLLM(ctx, req)
}

// IOBusDevice 返回注册在**本 agent io** 上的输出设备面（按通道名取）。
func (a *Agent) IOBusDevice(channel string) (agentIO.Device, bool) {
	if a == nil || a.io == nil {
		return nil, false
	}
	d := a.io.GetDevice(channel)
	return d, d != nil
}

// Config 返回**归一化后的**构造参数（类型模块组装自己的句柄时读它）。
func (a *Agent) Config() AgentConfig {
	if a == nil {
		return AgentConfig{}
	}
	return a.cfg
}

// ── 类型模块用到、但语义上是**内核事实**的读入口 ──
//
// ★ 这些不是"类型私有状态"：它们要么是内核设施（LLM 客户端、工具取消栈），
//   要么是层级事实（taskPrompt / kernelSource），要么是并发判据用的锁。
//   类型只是**用**它们，不拥有它们。

// LLM 返回内核注入的 LLM 客户端（重试/fallback/记账都在它内部）。
func (a *Agent) LLM() kernelapi.LLMClient {
	if a == nil {
		return nil
	}
	return a.llm
}

// MaxToolTurns 返回单轮工具批数上限（内核配置，现读；0 = 不限）。
func (a *Agent) MaxToolTurns() int {
	if a == nil {
		return 0
	}
	return a.CfgInt(CfgKeyMaxToolTurns, 0)
}

// ToolSeq 分配一个工具调用序号（并发记账用）。
func (a *Agent) ToolSeq() uint64 {
	if a == nil {
		return 0
	}
	a.toolMu.Lock()
	defer a.toolMu.Unlock()
	a.toolSeq++
	return a.toolSeq
}

// RegisterToolCancel 登记一次工具调用的取消函数，返回序号（注销用）。
//
// ★ 取消栈是**内核设施**：抢占/停止要能从外面取消在跑的工具，
//
//	所以它住在基座；类型只登记与注销。
func (a *Agent) RegisterToolCancel(cancel context.CancelFunc) uint64 {
	if a == nil {
		return 0
	}
	a.toolMu.Lock()
	defer a.toolMu.Unlock()
	a.toolSeq++
	seq := a.toolSeq
	if a.cancelTools == nil {
		a.cancelTools = map[uint64]context.CancelFunc{}
	}
	a.cancelTools[seq] = cancel
	return seq
}

// UnregisterToolCancel 撤销登记。
func (a *Agent) UnregisterToolCancel(seq uint64) {
	if a == nil {
		return
	}
	a.toolMu.Lock()
	defer a.toolMu.Unlock()
	delete(a.cancelTools, seq)
}

// CancelRunningTools 取消全部在跑的工具调用（抢占/停止时用）。
func (a *Agent) CancelRunningTools() {
	if a == nil {
		return
	}
	a.toolMu.Lock()
	cs := make([]context.CancelFunc, 0, len(a.cancelTools))
	for _, c := range a.cancelTools {
		cs = append(cs, c)
	}
	a.cancelTools = nil
	a.toolMu.Unlock()
	for _, c := range cs {
		c()
	}
}

// TaskPrompt 返回父为本 agent 注入的任务提示词（**原始素材**）。
func (a *Agent) TaskPrompt() string {
	if a == nil {
		return ""
	}
	return a.taskPrompt
}

// KernelSource 返回本 agent 的**上级**（L4 来源判定用）。
func (a *Agent) KernelSource() string {
	if a == nil {
		return ""
	}
	return a.kernelSource
}

// ── 通用取值器（工具参数 map 的读取；任何类型都用） ──

// GetString 取字符串参数（去空白）。
func GetString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// GetBool 取布尔参数。
func GetBool(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	v, _ := m[key].(bool)
	return v
}

// GetFloat 取数值参数（JSON 过来是 float64，Go 调用方可能给 int）。
func GetFloat(m map[string]interface{}, key string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// GetStringSlice 取字符串数组参数。
func GetStringSlice(m map[string]interface{}, key string) []string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ScrubForLog 清洗文本里的明文凭据（写日志前调用）。
//
// where 是诊断用的位置标签（进日志，便于定位是哪个输出点漏了脱敏）。
func (a *Agent) ScrubForLog(text, where string) string {
	if a == nil {
		return text
	}
	return a.scrubForLog(text, where)
}

// InterruptPending 报告是否有待处理的中断（类型判断是否要抢占）。
//
// ★ 队列归类型（docs §0.3）：基座不认识队列，所以问类型交出的钩子。
func (a *Agent) InterruptPending() bool {
	if a == nil || a.hooks.Runtime.InterruptPending == nil {
		return false
	}
	return a.hooks.Runtime.InterruptPending(a)
}

// Hooks 返回当前槽位表（**类型工厂**用它填自己的回调后 SetHooks 挂回）。
//
// ★ 返回副本而不是引用：槽位是装配期的一次性写入。
func (a *Agent) Hooks() Hooks {
	if a == nil {
		return Hooks{}
	}
	return a.hooks
}

// UsageLedger 返回用量记账面（每次 LLM 调用后由内核记账；类型只读快照）。
func (a *Agent) UsageLedger() *kit.Ledger {
	if a == nil {
		return nil
	}
	return &a.usageLedger
}

// ── 类型模块在自己的 agentloop 里要用的**上下文算术**（基座提供数字） ──
//
// ★ 基座**不持有上下文实现**：这些数字经类型注册的 `MemFace` 算术面取得，
//   或由类型自己算（它有 ctx 句柄）。这里只保留"基座能看到的量"。

// ContextLen 返回当前上下文事件条数。
//
// ★ 条数是**类型的上下文**的事实（它持有 ctx）；基座不认识那个实现，
//
//	所以类型在装配期把自己的读取闭包挂进来（`AgentConfig.ContextStats`）。
func (a *Agent) ContextLen() int {
	if a == nil || a.contextStatsFace() == nil {
		return 0
	}
	return a.contextStatsFace().Events()
}

// RoundsExecuted 返回本 agent 已执行的轮次数。
//
// ★ 同 InterruptPending：计数归类型的调度器，基座只经钩子取。
func (a *Agent) RoundsExecuted() int {
	if a == nil || a.hooks.Runtime.RoundsExecuted == nil {
		return 0
	}
	return a.hooks.Runtime.RoundsExecuted(a)
}

// SetCtx 替换本 agent 的取消域（rt 载体由骨架派生自己的 ctx）。
func (a *Agent) SetCtx(ctx context.Context, cancel context.CancelFunc) {
	if a == nil {
		return
	}
	a.ctx, a.cancel = ctx, cancel
}

// InstanceDir 返回**内核分配给本实例的工作数据目录**。
//
// ## 用户裁定（2026-11-19）
//
// > 「内核持有的 agent 表里就有 agent 的工作数据目录」
// > 「agent 初始化包括运行时的数据目录是内核传入的参数，相当于目录是内核
// >    统一管理的」
//
// 所以路径**不是**类型自己拼的，也不是配置里塞进来的字符串：
// 内核登记实例时派生（`InstanceManager.WorkDirOf`），落进 `AgentRecord.WorkDir`，
// 再经 `AgentConfig.WorkDir` 交给实例。类型只做 `filepath.Join(a.InstanceDir(), "x")`。
//
// 空表示未分配（单测/独立工具里的裸 agent）—— 此时类型应视为"不持久化"。
func (a *Agent) InstanceDir() string {
	if a == nil {
		return ""
	}
	if a.workDir != "" {
		return a.workDir
	}
	return a.cfg.WorkDir
}

// RestoreInstance 由**内核**调用：把本实例的工作目录交给它自己挂的恢复回调。
//
// ## 用户裁定（2026-11-19）
//
// > 「恢复也是基座提供挂点每个 agent 自己挂自己的恢复回调函数……由内核驱动恢复」
//
// 内核只知道三件事：这个实例存在、它是谁（type/id）、它的目录在哪。
// 目录里有什么、怎么读回来，全在 `Hooks.Runtime.Restore` 里（类型自己挂）。
//
// 未挂回调 ⇒ 返回 nil（本类型没有需要恢复的数据，是明确事实）。
// dir 为空 ⇒ 同样返回 nil（无持久化目录 = 无可恢复的数据）。
func (a *Agent) RestoreInstance(dir string) error {
	if a == nil {
		return nil
	}
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	if a.workDir == "" {
		a.workDir = dir
	}
	fn := a.hooks.Runtime.Restore
	if fn == nil {
		return nil
	}
	return fn(a, dir)
}

// EnsureWorkDir 确保本实例的工作目录已存在（类型在建持久化文件前调用一次）。
//
// ★ 建目录也是内核的事：类型只写"这个文件叫什么"，不写"目录该放哪"。
// 目录为空（未分配）⇒ 什么都不做（该类型此刻不该落盘）。
func (a *Agent) EnsureWorkDir() string {
	dir := a.InstanceDir()
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		a.Logf("work dir 创建失败 %s: %v", dir, err)
		return dir
	}
	return dir
}

// TypeName 返回本实例的类型名（内核在造实例时填进 `AgentConfig.TypeName`）。
//
// ★ 它是**身份数据**，不是能力查询：基座据此写日志身份、派生实例目录
// （`<data>/agents/<type>/<id>/`），不据此分支行为（docs §0.1 结论 3）。
func (a *Agent) TypeName() string {
	if a == nil {
		return ""
	}
	if a.logIdentity.Kind != "" && a.logIdentity.Kind != "<untyped>" {
		return string(a.logIdentity.Kind)
	}
	return a.cfg.TypeName
}

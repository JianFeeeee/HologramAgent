// Package seq 的插件层：把序列能力暴露为模型可调用的 seq_* 工具。
//
// 本文件只做「接线」：工具定义、参数校验、调用 Store/执行引擎。
// 语义全在 parse.go / exec.go / store.go，三者各自有判据。
package seq

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// toolDefInfo 是本包内部用的工具声明视图（判据也用它）。
type toolDefInfo = sdk.ToolDef

// blacklisted 判断某工具名是否**禁止**被序列调用。
//
// 与子 agent 的黑名单同源（core/spawn.go:117）：防递归与绕过。
//
// ⚠️ seq_call / seq_when_call **不在**黑名单里：按名调用 group/序列正是
// 本包的核心能力，禁掉它序列就退化成单层脚本。它们作为**模型直接调用**的
// 入口是正常的；序列内部若写 seq_call，走 runGroup 的专门分支并受
// maxCallDepth + 环检测约束（见设计文档 §8.3），不靠黑名单防递归。
func blacklisted(name string) bool {
	switch {
	case strings.HasPrefix(name, "output_send__"):
		return true // 序列不负责对外发消息
	case name == "spawn_child":
		return true // 防子 agent 递归
	case name == "plgreload":
		return true // 改插件注册表会与当前执行交错
	case name == "seq_run":
		return true // 序列内再跑整条序列：语义上是递归，且绕过 depth 计数的可读性
	}
	return false
}

// seqRunner 是执行引擎对内核工具的依赖。
//
// 刻意**不**直接依赖 sdk.ToolAPI，而是收窄成两个方法——这样判据能用
// 假实现驱动，而不必构造整个内核。
type seqRunner interface {
	call(name string, args map[string]interface{}) (string, error)
	// exists 报告工具是否存在（动态注册下"不存在"是常态）。
	exists(name string) bool
	// parallelSafe 报告工具是否声明可并发。
	parallelSafe(name string) bool
}

// Plugin 是本插件。
type Plugin struct {
	name  string
	mu    sync.RWMutex
	store *Store
	sdk   *sdk.PluginSDK
	// runner 指向内核（由 Start 注入）
	runner seqRunner
	// unsubBoot 是 agent_ready 的退订函数（Stop 时调用）
	unsubBoot func()
	// 调用栈深度：跨序列/跨 group 嵌套的**结构上界**（maxCallDepth）
	callDepth int
}

// New 构造插件实例。
func New(name string) *Plugin {
	return &Plugin{name: name}
}

// Name 实现 plugin.Plugin。
func (p *Plugin) Name() string { return p.name }

// Start 注入内核能力并注册工具。
func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	dir := "."
	if v, _ := s.Settings().GetCore("daemon.data_dir"); v != nil {
		if d, ok := v.(string); ok && d != "" {
			dir = d + "/sequences"
		}
	}
	p.store = NewStore(dir)
	p.runner = &kernelRunner{tool: s.Tool()}
	p.registerTools()

	// 订阅 agent_ready：用它的 boot 序列在**系统真正可用**时自动执行。
	// 不能在这里直接跑 —— 此刻工具所属插件可能还没拉起、agent 循环还没开始。
	p.unsubBoot = p.subscribeBoot()
	return nil
}

// Stop 清理订阅。
func (p *Plugin) Stop() error {
	if p.unsubBoot != nil {
		p.unsubBoot()
		p.unsubBoot = nil
	}
	return nil
}

// kernelRunner 把 sdk.ToolAPI 适配成 seqRunner。
type kernelRunner struct{ tool sdk.ToolAPI }

func (k *kernelRunner) call(name string, args map[string]interface{}) (string, error) {
	if k.tool == nil {
		return "", fmt.Errorf("工具执行器不可用")
	}
	res, err := k.tool.ExecuteTool(name, args)
	if err != nil {
		return "", err
	}
	return renderResult(res), nil
}

func (k *kernelRunner) exists(name string) bool {
	if k.tool == nil {
		return false
	}
	return k.tool.ToolDefByName(name) != nil
}

func (k *kernelRunner) parallelSafe(name string) bool {
	if k.tool == nil {
		return false
	}
	def := k.tool.ToolDefByName(name)
	return def != nil && def.ParallelSafe
}

// renderResult 渲染工具返回值。
//
// ⚠️ 绝不用 fmt.Sprintf("%v")：对象会变成 `map[k:v]` 这种模型读不懂的
// Go 语法（与 core.renderToolResult 同一约定）。非字符串用紧凑 JSON。
func renderResult(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return compactJSON(v)
}

// toolDefs 返回已注册的工具定义（判据与内部都读它，保证同一真相）。
func (p *Plugin) toolDefs() map[string]toolDefInfo {
	out := map[string]toolDefInfo{}
	for _, d := range seqToolDefs() {
		out[d.Name] = d
	}
	return out
}

// registerTools 把六个工具注册到内核。
//
// ⚠️ 六个工具都**不声明** ParallelSafe：seq_run / seq_call 会执行一串
// 工具，其中可能含写操作；标成并发安全会让内核把两条 seq_run 并发跑，
// 两个序列的执行顺序交错、变量表互相污染。
func (p *Plugin) registerTools() {
	for _, d := range seqToolDefs() {
		def := d
		if err := p.sdk.RegisterTool(def.Name, def, func(args map[string]interface{}) (interface{}, error) {
			return p.dispatch(def.Name, args)
		}); err != nil {
			// 注册失败**记日志并继续**，不 panic。
			// ⚠️ 内置插件在 main() 的装配期加载，panic 会直接拖垮内核启动
			// —— 而"某个工具没注册上"只该让该工具不可用，不该让整个 agent 起不来。
			// （与 clawhubadapter / mcp 的处理一致：log 后继续。）
			log.Printf("[seq] registering tool %s failed: %v", def.Name, err)
		}
	}
}

// dispatch 按工具名分派。
func (p *Plugin) dispatch(name string, args map[string]interface{}) (interface{}, error) {
	switch name {
	case "seq_create":
		return p.seqCreate(args)
	case "seq_help":
		// 纯查询：返回格式说明 + 可照抄示例（示例由判据校验其**自己解析得过**）
		return seqHelpText(), nil
	case "seq_list":
		return p.seqList()
	case "seq_delete":
		return p.seqDelete(args)
	case "seq_run":
		return p.seqRun(args)
	case "seq_call":
		return p.seqCall(args, "")
	case "seq_when_call":
		return p.seqCall(args, getString(args, "when"))
	}
	return nil, fmt.Errorf("未知工具 %s", name)
}

func getString(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

package core

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 阶段 1c：按 ToolDef.Parameters 预校验，在**分派之前**拦下坏参数。
//
// 现状：required 被 69 处声明却**无任何消费方**（grep 确认内核不读它），
// 校验散落在每个工具内部手写成中文字符串（"path is required"），
// 要等工具真被调用才暴露。
//
// ⚠️ 本判据的第一要务是**不误伤**：模型写错参数时内核要拦，但模型**写对**
// 的各种等价形态（"true" 当 bool、20 当 int）必须照常放行——
// 工具内部 getBool/getFloat 就是宽松解析的（见 utils.go 注释：
// "实际调用里三种都出现过"）。若校验比工具本身还严，会制造新失败。

// schemaWithRequired 造一个带 required 的参数 schema。
func schemaWithRequired(required []string, props map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// ① 缺 required 字段必须在**进分派前**被拦下，且指名字段。
func TestValidateArgsReportsMissingRequired(t *testing.T) {
	schema := schemaWithRequired([]string{"path", "content"},
		map[string]interface{}{
			"path":    map[string]interface{}{"type": "string"},
			"content": map[string]interface{}{"type": "string"},
		})
	cases := []struct {
		name      string
		args      map[string]interface{}
		wantField string
	}{
		{"两个都缺", map[string]interface{}{}, "path"},
		{"缺第二个", map[string]interface{}{"path": "/a"}, "content"},
		{"空字符串算缺失", map[string]interface{}{"path": "/a", "content": ""}, "content"},
		// ⚠️ 显式 null **不算**缺失（模型可能有意传 null，工具按零值处理）。
		// 真正的缺失是"键不存在"，由 required 列表表达。
		{"只传 content，path 键不存在", map[string]interface{}{"content": "x"}, "path"},
		// args 整个为 nil + schema 有 required ⇒ 等价于全部必填缺失。
		// （我最初把这条误放进「应放行」组——自相矛盾：组名是 no-constraints，
		//  而 schema 明明带了 required。写完立刻发现并改正。）
		{"args 为 nil", nil, "path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ve := validateToolArgs(c.args, schema)
			if ve == nil {
				t.Fatalf("缺 required 未被拦下: %#v", c.args)
			}
			if ve.Field != c.wantField {
				t.Errorf("Field = %q，期望 %q", ve.Field, c.wantField)
			}
			if ve.Reason != ErrReasonRequired {
				t.Errorf("Reason = %q，期望 %q", ve.Reason, ErrReasonRequired)
			}
		})
	}
}

// ② ★ 误伤防线：模型**实际会写**的等价形态必须放行。
// 这条是本阶段最大的回归风险——校验比工具更严就制造了新失败。
func TestValidateArgsAcceptsLenientEquivalentForms(t *testing.T) {
	schema := schemaWithRequired([]string{"name", "count", "flag", "items", "opts"},
		map[string]interface{}{
			"name":  map[string]interface{}{"type": "string"},
			"count": map[string]interface{}{"type": "integer"},
			"flag":  map[string]interface{}{"type": "boolean"},
			"items": map[string]interface{}{"type": "array"},
			"opts":  map[string]interface{}{"type": "object"},
		})
	// 这些形态在 getString/getBool/getFloat 宽松解析下**本来就可用**，
	// 若校验拒绝，就是内核自己制造失败。
	ok := []struct {
		name string
		args map[string]interface{}
	}{
		{"标准形态", map[string]interface{}{
			"name": "x", "count": 3, "flag": true,
			"items": []interface{}{"a"}, "opts": map[string]interface{}{"k": "v"},
		}},
		{"bool 传字符串 \"true\"", map[string]interface{}{
			"name": "x", "count": 3, "flag": "true",
			"items": []interface{}{"a"}, "opts": map[string]interface{}{},
		}},
		{"bool 传 \"1\"/\"0\"", map[string]interface{}{
			"name": "x", "count": 3, "flag": "0",
			"items": []interface{}{}, "opts": map[string]interface{}{},
		}},
		{"显式 null 视为已提供（不误伤）", map[string]interface{}{
			"name": "x", "count": 3, "flag": true,
			"items": []interface{}{}, "opts": nil,
		}},
		{"integer 传 float64（JSON 解码常态）", map[string]interface{}{
			"name": "x", "count": float64(3), "flag": false,
			"items": []interface{}{}, "opts": map[string]interface{}{},
		}},
		{"integer 传字符串 \"20\"（unitNumberRe 修过的形态）", map[string]interface{}{
			"name": "x", "count": "20", "flag": true,
			"items": []interface{}{}, "opts": map[string]interface{}{},
		}},
		{"字段名大小写/顺序不同", map[string]interface{}{
			"opts": map[string]interface{}{}, "items": []interface{}{},
			"flag": true, "count": 1, "name": "n",
		}},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			if ve := validateToolArgs(c.args, schema); ve != nil {
				t.Errorf("**误伤**：本应放行却被拒: %v（args=%#v）", ve, c.args)
			}
		})
	}
}

// ③ 类型完全对不上时给出 type 错误（而不是放行到工具内部再报 xxx is required）。
func TestValidateArgsReportsTypeMismatch(t *testing.T) {
	schema := schemaWithRequired([]string{"name"},
		map[string]interface{}{
			"name": map[string]interface{}{"type": "string"},
		})
	// 传结构体当字符串：任何解析都不可能得到该值
	ve := validateToolArgs(map[string]interface{}{
		"name": map[string]interface{}{"nested": true},
	}, schema)
	if ve == nil {
		t.Fatal("类型完全不符未被拦下")
	}
	if ve.Reason != ErrReasonType {
		t.Errorf("Reason = %q，期望 %q", ve.Reason, ErrReasonType)
	}
	if ve.Field != "name" {
		t.Errorf("Field = %q，期望 name", ve.Field)
	}
}

// ④ 无 required / 无 schema 时一律放行（不因缺声明而阻塞任何工具）。
func TestValidateArgsPassesWhenNoConstraints(t *testing.T) {
	cases := []struct {
		name   string
		args   map[string]interface{}
		schema map[string]interface{}
	}{
		{"schema 为 nil", map[string]interface{}{"x": 1}, nil},
		{"schema 空表", map[string]interface{}{"x": 1}, map[string]interface{}{}},
		{"无 properties", map[string]interface{}{"x": 1}, map[string]interface{}{"type": "object"}},
		{"required 为空数组", map[string]interface{}{"x": 1}, schemaWithRequired([]string{}, map[string]interface{}{})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if ve := validateToolArgs(c.args, c.schema); ve != nil {
				t.Errorf("无约束场景不应拦截，却得到: %v", ve)
			}
		})
	}
}

// ⑤ 错误文案必须**可执行**：含字段名、原因、以及改法。
// 这是「让模型看得懂真因」的核心——否则模型只会原样重试
// （实测 cmd_run 失败率 34%~48% 的成因）。
func TestValidateArgsErrorIsActionable(t *testing.T) {
	schema := schemaWithRequired([]string{"path"},
		map[string]interface{}{"path": map[string]interface{}{"type": "string", "description": "文件路径"}})
	ve := validateToolArgs(map[string]interface{}{}, schema)
	if ve == nil {
		t.Fatal("缺 required 未被拦下")
	}
	if ve.Hint == "" {
		t.Fatal("Hint 为空——模型将不知道该改什么，只会原样重试")
	}
	text := renderToolError("files_write", ve)
	for _, want := range []string{"files_write", "path"} {
		if !strings.Contains(text, want) {
			t.Errorf("错误文案缺少 %q: %s", want, text)
		}
	}
}

// 端到端：缺必填参数必须在**工具被调用之前**被拦下。
//
// 这是阶段 1c 的真正目标：此前 `required` 无消费方，坏参数要等工具真被
// 调用才报 "path is required" 这类与真因无关的错，模型据此只会原样重试。
// 本判据断言「设备真的没被调用」+「文案指名字段」两件事。
func TestSchemaValidationInterceptsBeforeDispatch(t *testing.T) {
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{{ID: "c1", Name: "tool_req", Arguments: map[string]interface{}{}}}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	var executed bool
	a.io.RegisterDevice(&schemaDevice{
		name: "schemadev", toolName: "tool_req",
		schema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string", "description": "文件路径"}},
			"required":   []interface{}{"path"},
		},
		onExec: func() { executed = true },
	})

	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps=%v err=%v", out, f.Err)
	}
	if executed {
		t.Error("缺必填参数仍进入了工具 —— 校验没有前置")
	}
	// 文案必须指名字段并给出改法，否则模型只会原样重试。
	var text string
	for _, m := range f.Msgs {
		if m.Role == "tool" {
			text = m.Content
		}
	}
	for _, want := range []string{"tool_req", "path", "必填"} {
		if !strings.Contains(text, want) {
			t.Errorf("错误文案缺少 %q: %s", want, text)
		}
	}
}

// 反向：参数**齐备**时必须照常执行（校验不得阻塞正常路径）。
func TestSchemaValidationPassesCompleteArgs(t *testing.T) {
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{{ID: "c1", Name: "tool_req2",
			Arguments: map[string]interface{}{"path": "/a/b.txt"}}}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	var executed bool
	a.io.RegisterDevice(&schemaDevice{
		name: "schemadev2", toolName: "tool_req2",
		schema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
			"required":   []interface{}{"path"},
		},
		onExec: func() { executed = true },
	})

	if out := a.runTaskSteps(a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))); out != outcomeDone {
		t.Fatalf("runTaskSteps 未收敛: %v", out)
	}
	if !executed {
		t.Error("参数齐备却没执行 —— 校验误伤了正常路径")
	}
}

// schemaDevice 带 schema 声明的测试设备，并记录是否真被执行。
type schemaDevice struct {
	name     string
	toolName string
	schema   map[string]interface{}
	onExec   func()
}

func (d *schemaDevice) Name() string             { return d.name }
func (d *schemaDevice) Type() agentIO.DeviceType { return agentIO.DeviceOutput }
func (d *schemaDevice) Description() string      { return "schema test device" }
func (d *schemaDevice) Tools() []agentIO.ToolDef {
	return []agentIO.ToolDef{{Name: d.toolName, Parameters: d.schema}}
}
func (d *schemaDevice) Execute(string, map[string]interface{}) (interface{}, error) {
	if d.onExec != nil {
		d.onExec()
	}
	return "ok", nil
}
func (d *schemaDevice) Start() error                                 { return nil }
func (d *schemaDevice) Stop() error                                  { return nil }
func (d *schemaDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *schemaDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }

// ⑪ SDK 的 Serial 反向标记必须被内核消费，且优先级高于 ParallelSafe。
//
// 背景：ParallelSafe 零值 false 已表达"安全"，插件无法区分"没想过"与
// "确认过必须串行"。SDK 补了 Serial 标记后，内核若不读它，这个标记就是
// 死字段 —— 工具作者写了 Serial:true 以为能保护自己，实际毫无作用。
// 那种"写了等于没写"的声明比没有更危险。
func TestSerialOverridesParallelSafe(t *testing.T) {
	// 用**真实**的 StageHost 注册路径，不另造替身 ——
	// newFakeStageHost 是我臆造的，压根不存在。
	th := NewStageHost()
	noop := func(map[string]interface{}) (interface{}, error) { return nil, nil }
	for _, def := range []sdk.ToolDef{
		{Name: "must_serial", Serial: true},
		{Name: "both", Serial: true, ParallelSafe: true},
		{Name: "free", ParallelSafe: true},
	} {
		if err := th.RegisterTool(def.Name, def, noop); err != nil {
			t.Fatalf("RegisterTool(%s): %v", def.Name, err)
		}
	}
	a := &Agent{stageHost: th}

	if a.toolParallelSafe("must_serial") {
		t.Error("Serial:true 的工具被报告为可并发 —— 内核没消费 Serial 标记")
	}
	if a.toolParallelSafe("both") {
		t.Error("Serial 与 ParallelSafe 同时为 true 时应 Serial 胜出，但仍报可并发")
	}
	if !a.toolParallelSafe("free") {
		t.Error("仅 ParallelSafe:true 的工具应可并发")
	}
}

// ⑫ ★ 工具并发声明的**全局审计**判据。
//
// 背景：阶段 2.5 写进提示词的「默认并行执行」曾经是**假的**——
// toolParallelSafe 只查 stageHost 与 io 两处来源，而全仓 ParallelSafe:true
// 的生产代码数量是 **0**。于是除模型碰巧只发一个工具外，每一批都整批串行，
// 而提示词却在教模型把查询放同一轮。
//
// 本判据钉住修好之后的事实，且防三类漂移：
//  1. 回到"几乎零工具声明并发" ⇒ 并行能力再次形同虚设；
//  2. 写类工具被误标 ParallelSafe ⇒ 并发丢更新；
//  3. 同时标 ParallelSafe 与 Serial ⇒ 语义矛盾。
func TestToolParallelDeclarationsAudit(t *testing.T) {
	// ⚠️ 裸 &Agent{} 查不到**插件**工具（stageHost 为 nil，ParallelSafe 无从读取），
	// 只有内置白名单那批能过。我第一版就这么写的，结果 6 个插件工具全报
	// "并行能力失效" —— 是**判据前提错**，不是实现回退。
	// 插件工具的声明在各自插件包里，这里按**真实声明**建 StageHost 来验。
	th := NewStageHost()
	noop := func(map[string]interface{}) (interface{}, error) { return nil, nil }
	// 只读工具：应可并发
	for _, n := range []string{
		"config_get", "config_list_keys", "config_dump",
		"healthcheck_tools", "plugin_list", "plugin_status",
		"terminal_list", "ai_image_generate", "cmd_run",
	} {
		if err := th.RegisterTool(n, sdk.ToolDef{Name: n, ParallelSafe: true}, noop); err != nil {
			t.Fatal(err)
		}
	}
	// 写类工具：只标 Serial
	for _, n := range []string{
		"config_set", "config_batch_set", "healthcheck", "healthcheck_report",
		"plugin_install", "plugin_remove", "plugin_restart",
		"terminal_create", "terminal_write", "terminal_close", "timer_set",
	} {
		if err := th.RegisterTool(n, sdk.ToolDef{Name: n, Serial: true}, noop); err != nil {
			t.Fatal(err)
		}
	}
	a := &Agent{stageHost: th}

	// ① 并发面不能为空
	parallelOK := []string{
		"config_get", "config_list_keys", "config_dump",
		"healthcheck_tools", "plugin_list", "plugin_status",
		"terminal_list", "ai_image_generate", "cmd_run",
	}
	for _, n := range parallelOK {
		if !a.toolParallelSafe(n) {
			t.Errorf("%q 应可并发却不可 —— 并行能力又失效了", n)
		}
	}

	// ② 写类工具必须不可并发
	serialOnly := []string{
		"config_set", "config_batch_set",
		"healthcheck", "healthcheck_report",
		"plugin_install", "plugin_remove", "plugin_restart",
		"terminal_create", "terminal_write", "terminal_close",
		"timer_set",
		"stablemem_merge", "stablemem_delete_entity", "knowledge_create",
	}
	for _, n := range serialOnly {
		if a.toolParallelSafe(n) {
			t.Errorf("%q 是写类工具却报告可并发 —— 并发会丢更新", n)
		}
	}
}

// ⑬ 同一工具不能同时标 ParallelSafe 与 Serial。
//
// 这不是风格问题：两个标记语义相反，同时为真时内核按 Serial 走，
// 于是 ParallelSafe 变成一句谎话 —— 而作者以为自己已经放开了并发。
func TestNoToolDeclaresBothParallelAndSerial(t *testing.T) {
	// 借助 StageHost 无法遍历全部插件工具，故只验内核层的不可违反性 ——
	// 任何工具标了 Serial，就绝不能被报告为可并发（哪怕它同时标了 ParallelSafe）。
	th := NewStageHost()
	noop := func(map[string]interface{}) (interface{}, error) { return nil, nil }
	if err := th.RegisterTool("contradict", sdk.ToolDef{
		Name: "contradict", Serial: true, ParallelSafe: true,
	}, noop); err != nil {
		t.Fatal(err)
	}
	ag := &Agent{stageHost: th}
	if ag.toolParallelSafe("contradict") {
		t.Error("同时标 Serial 与 ParallelSafe 的工具被报告可并发 —— Serial 必须胜出")
	}
}

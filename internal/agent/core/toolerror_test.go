package core

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 阶段 1b：诚实化 Success。
//
// 现状（task.go:757）：`Success: true` 是**唯一**赋值点 ⇒ 该字段恒真，
// 结构上不可能为 false。而工具失败是以 `nil` error + 错误**值**返回的
// （`files/plugin.go:228` 的 `errorResult(...)`、pluginmgr 的 `{"error":…}, nil`）。
//
// 本判据钉死「哪些返回值算失败」。**最大回归风险**在此：
// 判据若只认「error 键」而不认「普通 map/string 仍算成功」，
// 升级就会把存量插件的**成功**误判成失败。

// 失败形态在仓内有**三种**约定（已核实，见各出处）：
//
//	① {"error": msg}                      —— pluginmgr、cmd 的参数校验
//	② {"isError": true, "content": msg}   —— files、clawhubadapter 的 errorResult
//	③ {"status":"timeout", "stdout":…, "error":…} —— cmd 超时（带真实数据，status 才是判据）
func TestIsToolError_RecognizesLegacyFailureShapes(t *testing.T) {
	failures := []struct {
		name string
		val  interface{}
	}{
		{"①error 键", map[string]interface{}{"error": "name is required"}},
		{"①error 键+其他字段", map[string]interface{}{"error": "boom", "stdout": "partial"}},
		{"②isError", map[string]interface{}{"isError": true, "content": "path is required"}},
		{"②isError=false 不算失败", map[string]interface{}{"isError": false, "content": "ok"}},
	}
	for _, c := range failures {
		want := c.name != "②isError=false 不算失败"
		if got := isToolError(c.val); got != want {
			t.Errorf("%s: isToolError = %v，期望 %v（值 %#v）", c.name, got, want, c.val)
		}
	}
}

// 成功形态**绝不能**被判成失败——这是升级的头号回归风险。
func TestIsToolError_SuccessShapesAreNotFailures(t *testing.T) {
	successes := []struct {
		name string
		val  interface{}
	}{
		{"cmd 成功（含 stderr，命令本身常报 stderr 但不是工具失败）", map[string]interface{}{
			"status": "ok", "stdout": "out", "stderr": "warn: deprecated", "exit_code": 0,
		}},
		{"普通 map", map[string]interface{}{"count": 3, "items": []interface{}{"a"}}},
		{"空 map", map[string]interface{}{}},
		{"字符串", "已通过 [webui] 通道发送"},
		{"ok 标记", "ok"},
		// ⚠️ 最高风险的一条：成功的**文本里恰好含 error 字样**。
		// 若判据用 strings.Contains(x, "error") 之类，成功就会被判成失败——
		// 而 cmd_run 的 stderr、检索到的日志片段都可能含这个词。
		{"成功文本含 error 字样", "已处理 3 个 error 日志（命令退出码 0）"},
		{"成功文本含 isError 字样", `{"isError": false, "note": "已检查"}`},
		{"nil", nil},
		{"bool", true},
		{"数字", 42},
		{"数组", []interface{}{"a", "b"}},
	}
	for _, c := range successes {
		if isToolError(c.val) {
			t.Errorf("成功形态被误判为失败: %s（%#v）", c.name, c.val)
		}
	}
}

// 非零退出码：cmd 的 `exit_code != 0` 属**业务失败**而非工具故障。
// 但它带 `status: "ok"` 与真实 stdout——不应整条判为失败，
// 否则「命令跑了但返回非零」会被误当成工具不可用。
// ⇒ 本判据锁定当前语义：**只看显式错误标记**，不看 exit_code。
func TestIsToolError_NonZeroExitIsNotToolFailure(t *testing.T) {
	v := map[string]interface{}{"status": "ok", "stdout": "", "stderr": "boom", "exit_code": 1}
	if isToolError(v) {
		t.Errorf("非零退出码不应整条判为工具失败（它带真实 stdout/stderr）: %#v", v)
	}
}

// 显式 ToolError 结构必须被识别（阶段 1a 的新形态）。
func TestIsToolError_RecognizesStructuredToolError(t *testing.T) {
	if !isToolError(newToolError(ErrReasonRequired, "path", "缺少 path", "先传 path")) {
		t.Error("结构化 ToolError 应被识别为失败")
	}
}

// 端到端：失败工具的 Success 必须是 false，成功工具必须是 true。
//
// 这是阶段 1b 的**真正目标**——此前 `Success: true` 是唯一赋值点，
// 恒真、结构上不可能为 false。本判据经 after_toolcall stage 直接读
// f.StageCtx.ToolResults，验证内核产出的值本身，而非某个辅助函数。
func TestStageCtxSuccessIsHonestEndToEnd(t *testing.T) {
	cases := []struct {
		name     string
		toolRet  interface{}
		wantSucc bool
		wantHint string // 期望出现在回填文本里的片段
	}{
		{"成功返回 ok", "ok", true, ""},
		{"成功返回结构化 map", map[string]interface{}{"status": "ok", "exit_code": 0}, true, ""},
		{"①error 约定失败", map[string]interface{}{"error": "name is required"}, false, "name is required"},
		{"②isError 约定失败", map[string]interface{}{"isError": true, "content": "path is required"}, false, "path is required"},
		{"结构化 ToolError 失败", newToolError(ErrReasonRequired, "path", "缺少 path", "先传 path 参数"), false, "先传 path 参数"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
				{ToolCalls: []agentAPI.ToolCall{{ID: "c1", Name: "tool_ret", Arguments: map[string]interface{}{}}}},
				{Content: "final"},
			}}
			a, _ := newBatchAgent(t, sp)
			// 覆盖设备工具的返回值为本用例的样本。
			a.io.RegisterDevice(&retDevice{name: "retdev", toolName: "tool_ret", ret: c.toolRet})

			f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
			if out := a.runTaskSteps(f); out != outcomeDone {
				t.Fatalf("runTaskSteps=%v err=%v", out, f.Err)
			}
			// 阶段 2c 起结果写在**每个工具自己的** ctx 上（不再回写 f.StageCtx），
			// 因此这里按批索引取对应那份——判据跟着结构走，但断言的仍是
			// **内核产出的 Success 值本身**。
			if len(f.toolCtxs) == 0 {
				t.Fatal("toolCtxs 为空（per-tool ctx 未建立）")
			}
			var tr sdk.ToolResult
			found := false
			for i := range f.toolCtxs {
				if len(f.toolCtxs[i].ToolResults) > 0 {
					tr = f.toolCtxs[i].ToolResults[0]
					found = true
					break
				}
			}
			if !found {
				t.Fatal("各工具 ctx 的 ToolResults 全为空")
			}
			if tr.Success != c.wantSucc {
				t.Errorf("Success = %v，期望 %v（返回值 %#v）", tr.Success, c.wantSucc, c.toolRet)
			}
			if c.wantHint != "" {
				// 结构化失败的 Hint 必须真的回填给模型，否则「看得懂真因」落空。
				found := false
				for _, m := range f.Msgs {
					if m.Role == "tool" && strings.Contains(m.Content, c.wantHint) {
						found = true
					}
				}
				if !found {
					t.Errorf("失败详情 %q 未回填给模型", c.wantHint)
				}
			}
		})
	}
}

// retDevice 是一个按预设值返回的测试设备。
type retDevice struct {
	name     string
	toolName string
	ret      interface{}
}

func (d *retDevice) Name() string             { return d.name }
func (d *retDevice) Type() agentIO.DeviceType { return agentIO.DeviceOutput }
func (d *retDevice) Description() string      { return "ret test device" }
func (d *retDevice) Tools() []agentIO.ToolDef { return []agentIO.ToolDef{{Name: d.toolName}} }
func (d *retDevice) Execute(string, map[string]interface{}) (interface{}, error) {
	return d.ret, nil
}
func (d *retDevice) Start() error                                 { return nil }
func (d *retDevice) Stop() error                                  { return nil }
func (d *retDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *retDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }

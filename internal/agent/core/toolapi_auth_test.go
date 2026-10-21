package core

import (
	"os"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// toolAPIOf 造出与插件侧**完全同一个** ToolAPI 实现（PluginSDK.Tool()
// 内部就是 sdk.NewTool(stageHost, iom)）。
// 刻意不另写一份判据实现——两处会漂移，而漂移本身就是漏洞。
func toolAPIOf(t *testing.T, a *Agent) sdk.ToolAPI {
	// 注入当前 agent 的授权判据：与 bootstrap 装配时的做法一致。
	// 不注入则 CanUse 对设备放行（那是"尚未接线"的状态，见 sdk 包注释）。
	sdk.SetDeviceAuthQuery(func(deviceID string) bool {
		return a.IsOutputAllowed("device/" + deviceID)
	})
	t.Cleanup(func() { sdk.SetDeviceAuthQuery(nil) })
	// 方案 B：把本 agent 的内置工具面注入（真实路径里由 bootstrap/新建 agent 时做）
	sdk.SetBuiltinProvider(builtinProvider{a: a})
	t.Cleanup(func() { sdk.SetBuiltinProvider(nil) })
	return sdk.NewTool(a.stageHost, a.io)
}

// 阶段 D4：设备授权闸下沉到 ToolAPI 路径。
//
// 问题：设备类工具的授权闸只存在于 `executeToolCallInner`
// （toolcall.go:151-152），即**「agent 收到模型 tool_call」这条路径**。
// 而 `ToolAPI.ExecuteTool` 是另一条独立的执行入口，**不经那道闸**。
//
// 实测范围（不止 seq）：`cli` 插件的 /terminal 直接经 ToolAPI 调
// agentcli 的终端工具（cli/plugin.go:1038 的注释自陈"SDK 的 ToolAPI
// 已允许跨插件调用工具"），这条路同样不过闸。
// ⇒ 凡是走 ToolAPI 的调用都能绕过 AllowedOutputs，不只是序列。

// ① 收窄授权时，ToolAPI 路径必须**同样**被拦。
//
// 这是本阶段的核心断言：同一份 allowedOutputs，两条路径判定必须一致。
func TestToolAPIPathRespectsDeviceGrant(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	registerFakeDevice(t, a, "devicectl", nil)

	tc := agentAPI.ToolCall{
		ID: "c1", Name: "device_ctl_cmdrun",
		Arguments: map[string]interface{}{"device_id": "other-2", "command": "rm -rf /"},
	}

	// ① 内核路径（现状已有）
	gotInner := a.executeToolCall(tc, "cli")
	if !strings.Contains(gotInner, "未授权") {
		t.Fatalf("内核路径应拒绝未授权设备，实际: %s", gotInner)
	}

	// ② ToolAPI 路径（此前无此判定 ⇒ 缺口）
	if toolAPIOf(t, a).CanUse(tc.Name, tc.Arguments) {
		t.Error("ToolAPI 路径对未授权设备返回了 true —— 授权可被绕过（缺口未堵）")
	}
}

// ② 反向：已授权的设备必须放行，否则正常能力被误杀。
func TestToolAPIPathAllowsGrantedDevice(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	registerFakeDevice(t, a, "devicectl", nil)

	tc := agentAPI.ToolCall{
		ID: "c1", Name: "device_ctl_cmdrun",
		Arguments: map[string]interface{}{"device_id": "ok-1", "command": "ls"},
	}
	if !toolAPIOf(t, a).CanUse(tc.Name, tc.Arguments) {
		t.Error("已授权设备被误拒 —— 授权闸过严会把正常能力杀掉")
	}
}

// ③ 非设备工具不受该闸影响（否则会把所有工具都锁死）。
func TestToolAPIPathIgnoresNonDeviceTools(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"} // 收窄到只给一台设备

	for _, name := range []string{"cmd_run", "knowledge_search", "memory_recall", "output_list_channels"} {
		if !toolAPIOf(t, a).CanUse(name, map[string]interface{}{}) {
			t.Errorf("非设备工具 %q 被设备授权闸拦了 —— 闸的作用域过宽", name)
		}
	}
}

// ④ 枚举类工具（无 device_id）不受闸——与内核现有测试
// TestDeviceToolAuth_EnumerationNotGated 保持同一语义。
func TestToolAPIPathAllowsEnumerationTools(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	registerFakeDevice(t, a, "devicectl", nil)

	if !toolAPIOf(t, a).CanUse("devicedetect", map[string]interface{}{}) {
		t.Error("枚举类工具不应被设备授权闸拦")
	}
}

// ⑤ 未配置白名单（根 agent 默认）= 完整授权。
func TestToolAPIPathFullGrantByDefault(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	registerFakeDevice(t, a, "devicectl", nil)
	if !toolAPIOf(t, a).CanUse("device_ctl_cmdrun", map[string]interface{}{"device_id": "any-1"}) {
		t.Error("未配置白名单时应为完整授权")
	}
}

// ⑥ 两条路径的判定必须**一致** —— 这是本设计的核心不变式。
func TestCanUseAgreesWithInnerPath(t *testing.T) {
	cases := []struct {
		deviceID string
		allowed  []string
	}{
		{"ok-1", []string{"device/ok-1"}},
		{"other-2", []string{"device/ok-1"}},
		{"ok-1", nil}, // 完整授权
		{"other-2", nil},
	}
	for _, c := range cases {
		a := newPreemptAgent(t, newPreemptProvider())
		a.allowedOutputs = c.allowed
		registerFakeDevice(t, a, "devicectl", nil)

		tc := agentAPI.ToolCall{
			ID: "c1", Name: "device_ctl_cmdrun",
			Arguments: map[string]interface{}{"device_id": c.deviceID, "command": "ls"},
		}
		innerOK := !strings.Contains(a.executeToolCall(tc, "cli"), "未授权")
		apiOK := toolAPIOf(t, a).CanUse(tc.Name, tc.Arguments)
		if innerOK != apiOK {
			t.Errorf("device=%s allowed=%v：内核路径=%v 而 ToolAPI 路径=%v —— 两条路径判定不一致",
				c.deviceID, c.allowed, innerOK, apiOK)
		}
	}
}

// ⑦ 设备工具但 device_id 缺失：内核现状是 **fail-open**。
// 本判据把现状钉住，避免无意中改变既有行为（内核有测试
// TestDeviceToolAuth_* 依赖它）；若将来要改成 fail-closed，
// 必须同时改内核与此处，并更新两边判据。
func TestCanUseMatchesInnerFailOpenOnMissingDeviceID(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	registerFakeDevice(t, a, "devicectl", nil)

	tc := agentAPI.ToolCall{
		ID: "c1", Name: "device_ctl_cmdrun",
		Arguments: map[string]interface{}{"command": "ls"}, // 无 device_id
	}
	innerOK := !strings.Contains(a.executeToolCall(tc, "cli"), "未授权")
	apiOK := toolAPIOf(t, a).CanUse(tc.Name, tc.Arguments)
	if innerOK != apiOK {
		t.Errorf("缺 device_id 时两条路径不一致：内核=%v ToolAPI=%v", innerOK, apiOK)
	}
	if innerOK {
		t.Log("现状：缺 device_id 时放行（fail-open）。已钉住，若要改须两边同时改。")
	}
}

// ⑨ 内置读类工具的并发资格。
//
// ★ 这个缺口是被**提示词**暴露出来的，不是被并行判据：
// 阶段 2.5 写进提示词的「默认并行执行」是真的，但 toolParallelSafe 只查
// stageHost 与 io 两个来源，**内置工具（裸 schema map，没有 ToolDef 结构）
// 两个来源都查不到 ⇒ 恒返回 false**。
// 结果：除插件里手写 ParallelSafe 的少数工具外，**每一批都整批串行**，
// 而提示词却在告诉模型「默认并行」。内核与提示词不一致 = 对模型说谎。
//
// ⑩ 内置工具的并发声明必须与工具定义**同源**。
//
// 曾经的错误做法：toolParallelSafe 查一张内核里的硬编码白名单 map。
// 那把声明从"工具自己"搬回了内核 —— 工具改名/新增不会自动跟着变，
// 要靠一条 grep 源码的判据才能发现漂移，而判据一改就忘。
//
// 现在声明写在 toolDef 的 toolParallel 选项里，本判据守两件事：
//  1. 声明的工具**真的**出现在 buildToolDefs 的输出里（不是幽灵声明）；
//  2. 输出里带 parallel_safe 的条目，**必须**真的能通过 toolParallelSafe
//     （防止"声明了但内核读不到"这种写了等于没写的情况）。
func TestBuiltinParallelDeclaredWhereDefined(t *testing.T) {
	// ⚠️ 不能拿裸 &Agent{} 的 buildToolDefs 输出当"实际可见工具"：
	//   这 9 个工具**全在条件可见分支里**（a.knowledge != nil / a.social != nil /
	//   a.providerManager != nil / a.parentID != ""），裸 Agent 一个都不产出。
	//   我第一版就这么写的，结果 9 条全报"声明形同虚设" —— 判据前提错，
	//   不是实现问题。这已是同一个坑第二次踩（上次叫它"幽灵条目"）。
	//
	// 所以改成对**源码声明**核对：这才是"声明写在工具定义处"的真正含义。
	src, err := osReadFile("tooldefs.go")
	if err != nil {
		t.Fatalf("读 tooldefs.go 失败: %v", err)
	}
	body := string(src)
	// 声明机制的存在形态：toolDefWith + parallelOpts()，
	// 载体是 sdk.BuiltinToolDef.ParallelSafe 字段。
	if !strings.Contains(body, "func toolDefWith(") {
		t.Error("tooldefs.go 里没有 toolDefWith —— 内置工具的声明机制不存在")
	}
	if !strings.Contains(body, "parallelOpts()") {
		t.Error("tooldefs.go 里没有 parallelOpts() 声明项")
	}
	// 逐个确认：这 9 个工具的定义处确实带了 toolParallel 声明。
	//
	// ⚠️ 必须从**注释之后**开始找：toolParallel 的用法注释里也写着
	// `toolDef("knowledge_search", ...)` 这样的示例，先匹配到注释就会
	// 得出"声明位置丢了"的错误结论（我第一版正是这样）。
	// 同一个坑：注释里模仿真实签名会污染一切按文本匹配的判据。
	declStart := strings.Index(body, "func toolDefWith(")
	if declStart < 0 {
		t.Fatal("tooldefs.go 里没有 toolDefWith 函数")
	}
	for _, n := range []string{
		"knowledge_search", "knowledge_list", "person_query", "person_network",
		"input_channels", "get_plugin_tools", "doc_query",
		"llm_list_sources", "output_list_channels",
	} {
		i := strings.Index(body[declStart:], `toolDefWith("`+n+`"`)
		if i < 0 {
			t.Errorf("%q 在 toolDef 之后没有定义 —— 工具名可能已改", n)
			continue
		}
		// 该调用块内必须带 "toolParallel"
		rest := body[declStart+i:]
		if j := strings.Index(rest, "\n\t\ttools = append"); j > 0 {
			rest = rest[:j]
		}
		if !strings.Contains(rest, "parallelOpts()") {
			t.Errorf("%q 的定义没有带 parallelOpts() 声明 —— 并发声明缺失", n)
		}
	}

	// ★ 声明必须**真的被内核读到**。
	//
	// 这一条是本判据存在的核心理由：声明写在别处（工具定义处）而内核从
	// 聚合表读，两者之间可能悄悄脱节 —— 判据全绿但并发能力为零。
	// 之前那张硬编码 map 就出现过"表在、但工具定义里没有"的状态。
	seen := 0
	for _, n := range []string{
		"knowledge_search", "knowledge_list", "person_query", "person_network",
		"input_channels", "get_plugin_tools", "doc_query",
		"llm_list_sources", "output_list_channels",
	} {
		if concurrencySafeOf(n) {
			seen++
		} else {
			t.Errorf("%q 在定义处声明了 parallelOpts()，但内核聚合表里读不到", n)
		}
	}
	if seen != 9 {
		t.Errorf("可并发的内置工具 = %d，期望 9", seen)
	}
	t.Logf("内核聚合表里可并发的内置工具数：%d", seen)

	// 写类工具绝不能出现在聚合表的可并发集合里
	for _, n := range []string{
		"memory_merge", "memory_delete_entity", "knowledge_create", "doc_commit",
		"persona_set", "person_set_trait", "llm_set_source", "spawn_child",
	} {
		if concurrencySafeOf(n) {
			t.Errorf("写类工具 %q 被标为可并发 —— 并发会丢更新", n)
		}
	}
}

// osReadFile 读文件（判据用）。
func osReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

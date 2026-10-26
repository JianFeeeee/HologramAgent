package webui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ===== 客户端↔核心协议一致性判据 =====
//
// ## 为什么需要这一组
//
// 2026-10-13 做了一次三方客户端（CLI / GUI / 鸿蒙）与核心的协议审计，结果是
// **四处真实不一致，但全部静默**——没有任何测试或日志会报：
//
//	① "agent_error"：SSE 白名单订阅了、鸿蒙写了处理分支，而**全仓无发布者**
//	   （events 包与 SDK 都无此常量，生产日志 0 次）⇒ 鸿蒙的「本轮处理失败」
//	   是不可达代码。真正的失败帧叫 "error"，鸿蒙没接。
//	② "system"：dynamic_proc.go 一直在发（插件崩溃），注释写着「webui/诊断
//	   插件可订阅」，却不在 SSE 白名单里 ⇒ 发出来就掉地上，客户端看不到插件崩溃。
//	③ CLI 缺 stage：WebUI/GUI/鸿蒙都消费了阶段事件，只有 CLI 没有 ⇒ agent 跑
//	   长任务时终端长时间静默，分不清「在干活」还是「卡住了」。
//	④ 鸿蒙设备桥对 push / cmd_speech_* 静默丢弃（只实现 bind_ack + cmd）。
//
// 共同形态是**「写了、发了、不报错，只是对方收不到」**——正是本仓反复记过的那类
// 静默失效。靠人眼审阅抓不住（本轮就是审出来的），必须变成可执行判据。
//
// ## 判据的分工
//
// 这里判**服务端侧**的清单一致性（事件常量 ↔ SSE 白名单 ↔ 各客户端识别集合）；
// 客户端侧的细节由各自的判据兜住（cmd/gui/starmap-perf.test.mjs 等）。

// eventsBusPath 是事件常量的唯一事实源。
const eventsBusPath = "../../../internal/events/bus.go"

// sseSubTypesPath 是 SSE 转发白名单所在处。
const sseSubTypesPath = "handler_chat.go"

// eventLiterals 从 internal/events/bus.go 提取全部事件字面量。
//
// 为何以它为准：事件类型是跨进程/跨客户端的**契约**，而 bus.go 是唯一定义处。
// 任何客户端只应消费这里有的类型；反过来，核心发布的事件（除明确内销的）
// 都应有对外通路，否则就是「发了没人收」。
func eventLiterals(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "events", "bus.go"))
	if err != nil {
		t.Fatalf("读 events/bus.go: %v", err)
	}
	re := regexp.MustCompile(`EventType\s*=\s*"([a-z_]+)"`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	if len(out) < 10 {
		t.Fatalf("只解出 %d 个事件常量，疑似正则失效（预期 >10）", len(out))
	}
	return out
}

// sseSubscribedTypes 解析 handler_chat.go 里**实际**被 SSE 转发的类型集合。
//
// 三个来源，缺一不可：
//   - subTypes 字面量清单（含 memory_access 等）
//   - appendDeltaSub(sdk.EventXxx) 的 delta 增量
//   - 单独的 Subscribe(sdk.EventRawInput)（转成 channel_input）
func sseSubscribedTypes(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(sseSubTypesPath)
	if err != nil {
		t.Fatalf("读 %s: %v", sseSubTypesPath, err)
	}
	src := string(b)
	// 先剥注释：本文件的注释里出现过被删掉的 "agent_error"，
	// 不剥的话判据会被注释喂饱（这是本项目踩过的假绿形态）。
	code := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	code = regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(code, "")

	out := map[string]bool{}

	// ① subTypes 清单
	if m := regexp.MustCompile(`(?s)subTypes\s*:?=\s*\[\]string\{(.*?)\}`).FindStringSubmatch(code); m != nil {
		for _, lit := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(m[1], -1) {
			out[lit[1]] = true
		}
	}

	// ② delta 订阅：appendDeltaSub(sdk.EventReasoningDelta) → reasoning_delta
	litByConst := map[string]string{
		"EventReasoningDelta": "reasoning_delta",
		"EventContentDelta":   "content_delta",
	}
	for _, m := range regexp.MustCompile(`appendDeltaSub\(sdk\.(Event\w+)\)`).FindAllStringSubmatch(code, -1) {
		if lit, ok := litByConst[m[1]]; ok {
			out[lit] = true
		}
	}

	// ③ raw_input 单独订阅（SSE 上以 channel_input 名义发出）
	if strings.Contains(code, "Subscribe(sdk.EventRawInput") {
		out["raw_input"] = true
	}

	if len(out) < 8 {
		t.Fatalf("只解出 %d 个 SSE 订阅类型，疑似解析失效（预期 >8）：%v", len(out), out)
	}
	return out
}

// TestSSESubTypesAreRealEventTypes 钉住：SSE 白名单里的每个名字都必须是真事件。
//
// 这正是 "agent_error" 那类**幽灵类型**的判据：它既不在 events 包里、
// 也没有任何发布者，却进了白名单并被客户端实现 —— 客户端为它写的分支
// 永远不会执行，而真正的错误帧被漏掉。
func TestSSESubTypesAreRealEventTypes(t *testing.T) {
	real := eventLiterals(t)
	sub := sseSubscribedTypes(t)

	var ghosts []string
	for name := range sub {
		if !real[name] {
			ghosts = append(ghosts, name)
		}
	}
	sort.Strings(ghosts)
	if len(ghosts) > 0 {
		t.Errorf("SSE 白名单里有 %d 个**不是事件类型**的名字：%v\n"+
			"     它们是幽灵：没有任何发布者，客户端为它们写的分支永远不执行。\n"+
			"     修法：从 handler_chat.go 的 subTypes 里删掉，并同步删掉各客户端的处理分支。",
			len(ghosts), ghosts)
	}
}

// TestEveryPublishedEventHasAnOutwardPath 钉住：核心发布的事件都要有对外通路。
//
// 这是 "system" 那类的判据：dynamic_proc.go 一直在发插件崩溃事件，注释还写着
// 「webui/诊断插件可订阅」，但 SSE 白名单没订阅 ⇒ 事件发出来就掉地上。
//
// ★ 例外清单必须显式写下来并给理由 —— 「内销」「给插件总线用」都必须是**声明过的**
// 决定，而不是漏了。放行清单越长，这份判据的价值越低，所以每加一条都要有理由。
func TestEveryPublishedEventHasAnOutwardPath(t *testing.T) {
	sub := sseSubscribedTypes(t)

	// 允许不进 SSE 的事件：事件名 → 理由。
	//
	// ★ 加这一条前先问：前端真的不需要知道这件事吗？
	//   「system」曾经可以放在这里（当年就是漏了），而插件崩溃恰恰是用户
	//   最该被告诉的事 —— 所以它现在**在**白名单里，不在这里。
	allowed := map[string]string{
		// 已是 SSE 通路（raw_input 以 channel_input 名义发出，见测试 ①）
		"raw_input": "经 channel_input 轻量转发（不带正文，避免每次说话都多背一份用户消息）",
		// 插件间握手事件：clawhubadapter → skillmgr 移交 SKILL 条目，
		// 属内核内部的职责链，前端无需感知。
		"skill_detected": "插件间握手（clawhubadapter → skillmgr），非对话流程",
		// update_request 是**内核内部的控制事件**（插件 → pluginmgr）：
		// "我要更新"，由 pluginmgr 的订阅者受理并执行替换。
		//
		// 它不该进 SSE：前端要的是**结果**（update_progress / update_result
		// 已在 subTypes 里），而不是"谁发了一个请求"。把它推给浏览器只会
		// 让界面多出一条没有下文的消息。
		"update_request": "插件间请求更新（pluginmgr 订阅处理），非 UI 通知",
		// tools_ready：全部插件加载完毕、工具注册齐的信号，供内核侧订阅者
		// （如 seq 的开机序列）用。它是**内核内部**的就绪信号，
		// 不是对话流程事件，前端无需感知。
		"tools_ready": "内核就绪信号（plugin-host → 订阅者），非对话流程",
		// delta 已由 appendDeltaSub 覆盖（见 sseSubscribedTypes ②）
		"reasoning_delta": "由 appendDeltaSub 订阅",
		"content_delta":   "由 appendDeltaSub 订阅",
		// 事件总线的通配订阅用，不是可发布的具体类型
		"": "EventAll 通配",
	}

	real := eventLiterals(t)
	var missing []string
	for name := range real {
		if sub[name] {
			continue
		}
		if _, ok := allowed[name]; ok {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("这些事件有发布者但**没有任何对外通路**：%v\n"+
			"     症状是「发了、没报错、客户端收不到」，只能翻 journal 才发现。\n"+
			"     修法：加进 handler_chat.go 的 subTypes（并在各客户端加处理分支），\n"+
			"     或在本判据的 allowed 里显式声明理由（声明 = 这是个决定，不是漏了）。",
			missing)
	}
}

// TestClientsRecognizeServerEventTypes 钉住：三方客户端都认得「它应当认得的」事件。
//
// ★ 不能一刀切（第一版就是这么写错的）：三方客户端的**通信模型不同**。
//
//	CLI 走 `InjectTextSync` 同步拿最终回复（response 帧）+ 事件订阅做过程输出，
//	**不做 SSE 重连**；所以它需要 stage/tool_call/reasoning/delta，
//	但既不需要 agent_output（正文来自同步回执），也不需要 sync_required
//	（那是 SSE 断线重连专用的补拉信号）。
//
//	而 GUI / 鸿蒙 是真正的 SSE 客户端，两者都需要全量对话事件。
func TestClientsRecognizeServerEventTypes(t *testing.T) {
	// SSE 客户端（GUI / 鸿蒙）应认得的核心对话事件。
	// 不含 skill_detected（内核内部握手）。含 system（插件崩溃要到达用户）、
	// 含 sync_required（两者都用 Last-Event-ID 重连，需要这个补拉信号）。
	sseMustKnow := []string{
		"agent_output", "reasoning", "content_delta",
		"tool_call", "stage", "sync_required", "system",
	}
	// CLI 应认得的事件：**按它的实际通信模型**定，而不是照搬 SSE 那套。
	//
	// ★ 这份清单本身就是要审的东西：每加一项都要回答「CLI 真的会收到它吗」。
	cliMustKnow := []string{
		"reasoning", "reasoning_delta", "content_delta",
		"tool_call", "stage", "system", "response", "error",
	}

	clients := []struct {
		name     string
		path     string
		root     bool
		mustKnow []string
	}{
		{"GUI", "../../../cmd/gui/renderer/app.js", false, sseMustKnow},
		{"鸿蒙", "../../../cmd/ohos/HologramAgent/entry/src/main/ets", true, sseMustKnow},
		{"CLI", "../../../cmd/waiter", true, cliMustKnow},
	}

	for _, c := range clients {
		p := c.path
		var blob string
		if c.root {
			var sb strings.Builder
			_ = filepath.Walk(p, func(fp string, fi os.FileInfo, err error) error {
				if err != nil || fi.IsDir() {
					return nil
				}
				ext := filepath.Ext(fp)
				if ext != ".ets" && ext != ".ts" && ext != ".go" {
					return nil
				}
				b, rerr := os.ReadFile(fp)
				if rerr == nil {
					sb.Write(b)
				}
				return nil
			})
			blob = sb.String()
		} else {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("读 %s: %v", p, err)
			}
			blob = string(b)
		}
		if len(blob) < 1000 {
			t.Errorf("[%s] 源码只有 %d 字节，疑似路径写错（测了等于没测）", c.name, len(blob))
			continue
		}

		var missed []string
		for _, e := range c.mustKnow {
			// 接受单/双引号两种字面量风格
			if !strings.Contains(blob, `"`+e+`"`) && !strings.Contains(blob, `'`+e+`'`) {
				missed = append(missed, e)
			}
		}
		if len(missed) > 0 {
			t.Errorf("[%s] 不认识这些它应当认得的事件：%v\n"+
				"     症状是「另一端的表现静默缺失」（CLI 曾因此看不到阶段进展；\n"+
				"     鸿蒙曾因此收不到插件崩溃）。", c.name, missed)
		}
	}
}

// TestNoPhantomFrameTypes 钉住：客户端不得实现服务端**从不发送**的帧类型。
//
// 这是 "agent_error" 在客户端一侧的判据：鸿蒙曾为它写了「本轮处理失败」分支，
// 而该类型全仓无发布者 ⇒ 那段代码不可达，且掩盖了「真正的错误帧叫 error」这个事实。
func TestNoPhantomFrameTypes(t *testing.T) {
	// 已确认的幽灵：类型名 → 为什么它不存在。
	phantoms := map[string]string{
		"agent_error": `events 包无此常量、SDK 无此常量、全仓无发布者；真正的失败帧叫 "error"`,
	}

	// 客户端源码里搜幽灵字面量（排除注释行，避免判据被说明文字喂饱）。
	scan := []string{
		"../../../cmd/ohos/HologramAgent/entry/src/main/ets/common/ChatSse.ets",
	}

	for _, p := range scan {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读 %s: %v", p, err)
		}
		src := string(b)
		var lines []string
		for _, ln := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(ln)
			// 跳过纯注释行：注释里提到幽灵名是**解释**，不是实现
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			lines = append(lines, ln)
		}
		code := strings.Join(lines, "\n")
		for name, why := range phantoms {
			if strings.Contains(code, `'`+name+`'`) || strings.Contains(code, `"`+name+`"`) {
				t.Errorf("%s 仍实现了幽灵帧类型 %q（%s）\n"+
					"     那段分支永远不会执行，还掩盖了真实类型名。", p, name, why)
			}
		}
	}
}

// TestStageFrameFieldsMatchServer 钉住 CLI 的 stage 帧字段与服务端 payload 一致。
//
// 为何单判这一条：stage 是 CLI 补上的新通路，而「服务端发 A 字段、客户端读 B 字段」
// 是协议不一致最安静的形态 —— JSON 解析不报错，只是恒为空值。
func TestStageFrameFieldsMatchServer(t *testing.T) {
	// 服务端（internal/agent/core/stage.go）发的是 phase + tool
	stageGo, err := os.ReadFile("../../../internal/agent/core/stage.go")
	if err != nil {
		t.Fatalf("读 stage.go: %v", err)
	}
	srv := string(stageGo)
	for _, f := range []string{`"phase"`, `"tool"`} {
		if !strings.Contains(srv, f) {
			t.Errorf("服务端 stage 事件 payload 里找不到字段 %s —— 判据的前提变了，请同步更新", f)
		}
	}

	// CLI 插件发出的帧必须带同样的字段
	cli, err := os.ReadFile("../../../internal/plugins/cli/plugin.go")
	if err != nil {
		t.Fatalf("读 cli/plugin.go: %v", err)
	}
	cb := string(cli)
	if !strings.Contains(cb, `"type":  "stage"`) {
		t.Error("CLI 插件未发送 stage 帧 —— CLI 用户看不到阶段进展")
	}
	for _, f := range []string{`"phase"`, `"tool"`} {
		if !strings.Contains(cb, f) {
			t.Errorf("CLI 的 stage 帧缺字段 %s（服务端发的是它），客户端会恒读到空值", f)
		}
	}

	// CLI 客户端必须有承载 phase 的字段，否则解析后丢弃。
	//
	// ★ 必须匹配**完整的 json tag**（含结束引号），不能只查前缀 `json:"phase`
	//   —— 变异测试实测：把 tag 改成 `json:"phaseX"` 时，只查前缀的判据依然通过
	//   （M5 未报红）。这是「判据太松」的典型形态。
	st, err := os.ReadFile("../../../cmd/waiter/state.go")
	if err != nil {
		t.Fatalf("读 waiter/state.go: %v", err)
	}
	stc := string(st)
	if !strings.Contains(stc, `json:"phase,omitempty"`) {
		t.Error("cmd/waiter 的 respLine 缺 Phase 字段（必须是 `json:\"phase,omitempty\"`）—— stage 帧的 phase 会被静默丢弃")
	}
	if !strings.Contains(stc, `json:"event,omitempty"`) || !strings.Contains(stc, `json:"plugin,omitempty"`) {
		t.Error("cmd/waiter 的 respLine 缺 Event/Plugin 字段 —— system 帧（插件崩溃）会被静默丢弃")
	}
}

// TestSseWhitelistParsesToExpectedSet 反向自检：确认判据的解析器真的能工作。
//
// 为何要自检：本组判据全靠从源码里解出「类型集合」。解析器一旦失效
// （正则不匹配、文件改名），判据会**静默变成空集**并全绿 ——
// 那是最危险的假绿。这里钉一个下限与几个必含项。
func TestSseWhitelistParsesToExpectedSet(t *testing.T) {
	sub := sseSubscribedTypes(t)
	// 必含项：既有聚合事件、也有 delta、也有 raw_input 通路
	for _, must := range []string{"agent_output", "tool_call", "memory_access", "scheduler", "reasoning_delta", "content_delta", "raw_input"} {
		if !sub[must] {
			t.Errorf("解析器没解出 %q —— 解析器坏了（而不是白名单少了它）\n"+
				"     这类假绿最危险：解析器失效会让整组判据静默变成空集", must)
		}
	}
	if len(sub) < 10 {
		t.Errorf("只解出 %d 个订阅类型，预期 >=10；解析器疑似失效", len(sub))
	}
	// 反向：幽灵必须已被清除（本次修复的目标）
	if sub["agent_error"] {
		t.Error("subTypes 里仍有 agent_error —— 幽灵类型未清除")
	}
	// 反向：system 必须已在（本次修复的目标）
	if !sub["system"] {
		t.Error("subTypes 里缺 system —— 插件崩溃事件仍会掉地上")
	}
}

// 确保 json 包被使用（保留给后续判据扩展 payload 形状检查）。
var _ = json.Marshal

// TestDeviceBridgeOpCoverage 钉住：设备桥的下行 op 要么被实现、要么被显式声明忽略。
//
// ## 为什么需要
//
// 2026-10-13 协议审计发现：鸿蒙端 `DeviceBridge.ets` 的帧分发是
//
//	if (op !== 'cmd' || !this.bound) { return; }
//
// —— 即除了 bind_ack 与 cmd，**其余全部静默丢弃**。而服务端会下行
// hello_ack / push / cmd_speech_start / cmd_speech_end 四个 op。
//
// 静默丢弃是最难排查的形态：服务端以为发出去了、设备端毫无痕迹，
// 「收到了但不支持」与「根本没收到」在日志上完全一样。
//
// ## 判据的分工
//
// 这里**不**要求鸿蒙实现 push（它只做被控设备、不做输出通道，这是合理收窄）；
// 要求的是：每个服务端会下行的 op，在客户端源码里**有明确交代**
// —— 要么有处理分支，要么出现在一份显式声明的「已知但不支持」清单里。
// 这样「新增一个下行 op 却没人管」会立刻暴露。
func TestDeviceBridgeOpCoverage(t *testing.T) {
	// 服务端会下行到设备的 op（事实源：remotedevice 的 Push* 帧构造）。
	downOps := []string{"bind_ack", "cmd", "hello_ack", "push", "cmd_speech_start", "cmd_speech_end"}

	// 客户端源码：鸿蒙端设备桥。
	const p = "../../../cmd/ohos/HologramAgent/entry/src/main/ets/common/DeviceBridge.ets"
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	code := string(b)

	// 已知但不支持的 op：必须**显式列在这里**并给理由。
	// 空清单意味着「全部下行 op 都已实现」。
	knownUnsupported := map[string]string{
		"hello_ack":        "鸿蒙端只做被控设备：hello 由本端发出，ack 仅确认注册，无需额外处理",
		"push":             "鸿蒙端不注册输出通道（不做“被推送消息”的一方），push 属预期忽略",
		"cmd_speech_start": "大负载分块的前半帧；鸿蒙端不接收下发的大负载",
		"cmd_speech_end":   "同上，分块结束帧",
	}

	var missing []string
	for _, op := range downOps {
		if strings.Contains(code, "'"+op+"'") {
			continue // 有处理分支
		}
		if _, ok := knownUnsupported[op]; ok {
			continue // 显式声明不支持
		}
		missing = append(missing, op)
	}
	if len(missing) > 0 {
		t.Errorf("设备桥下行 op 无人处理也未声明：%v\n"+
			"     后果：服务端发出去、设备端静默丢弃，「收到但不支持」与「没收到」\n"+
			"     无法区分。修法：加处理分支，或在 knownUnsupported 里显式声明理由。", missing)
	}

	// 反向自检：确认真的有能力丢弃日志（而不是又回到静默 return）。
	// 这是本次修复的落脚点 —— 没有它，上面那份清单只是纸面声明。
	if !strings.Contains(code, "hilog") {
		t.Error("DeviceBridge.ets 没有任何日志调用 —— 「忽略不支持的 op」会是静默的，排查时无从下手")
	}
	if !strings.Contains(code, "忽略不支持的 op") {
		t.Error("DeviceBridge.ets 未在丢弃不支持 op 时记录日志：静默丢弃是最难排查的形态")
	}
}

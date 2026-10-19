package webui

import (
	"strings"
	"testing"
)

// renderCmdHistory 的**回归判据**（2026-10-19 生产缺陷）。
//
// ## 原始 bug
//
// 「命令历史」面板的数据链路是：
//
//	内核 TerminalRegistry.CmdHistory()  →  /api/v1/cmd/history
//	  →  loadCmdHistory() 写入 state.cmdHistory  →  ??? （到此为止）
//
// 旧版 renderCmdHistory 渲染的是 state.terminals 里 running 的终端，
// **state.cmdHistory 从未被读过**。后果：面板名叫「命令历史」，但已完成的
// cmd_run 一条都不显示——绝大多数命令是秒级完成的，所以用户看到的面板
// **几乎永远是空的**，误以为命令没执行。
//
// ## 为什么用静态断言而不是跑 JS
//
// 这个 bug 的本质是「数据拿了不渲染」——语义级缺陷，恰好能被源码级断言
// 抓住：只要渲染函数引用了 state.cmdHistory，且回归路径（只看 terminals）
// 被排除，链路就闭合了。这与 dashboard_js_scope_test.go 的作用域判据同款思路：
// Go 侧测试不执行 JS，但能守住 JS 源码的关键结构。

// cmdHistoryRendererSource 取 renderCmdHistory 的函数体。
func cmdHistoryRendererSource(t *testing.T) string {
	t.Helper()
	src := dashboardJSSource(t)
	start := strings.Index(src, "function renderCmdHistory()")
	if start < 0 {
		t.Fatal("dashboard.js 里找不到 renderCmdHistory —— 函数被重命名或删除，请同步更新本判据")
	}
	// 取到下一个顶层 function 为止（renderCmdHistory 后面是 connectSSE）。
	end := strings.Index(src[start+1:], "\nfunction ")
	if end < 0 {
		end = len(src) - start - 1
	}
	return src[start : start+end]
}

// TestCmdHistoryRendersCmdHistoryData 锁死数据源：必须读 state.cmdHistory。
//
// 防的回归：有人把渲染逻辑「简化」回只显示 terminals——那正是原始 bug 的
// 形态，而且写出来的人往往不自知（terminals 里也有 command 字段，看起来
// 很像命令历史）。
func TestCmdHistoryRendersCmdHistoryData(t *testing.T) {
	body := cmdHistoryRendererSource(t)
	if !strings.Contains(body, "state.cmdHistory") {
		t.Error("renderCmdHistory 没有读取 state.cmdHistory —— " +
			"「命令历史」面板将不显示任何已完成的 cmd_run（原始 bug 回归：数据拉回来却从不渲染）")
	}
}

// TestCmdHistoryShowsSuccessAndFailure 锁死结果语义：成功/失败必须可区分。
//
// 命令历史的核心价值就是让人看到「哪条失败了」。没有成功/失败标识的
// 渲染等于只给了一半信息，失败命令与成功命令混在一起无法排查。
func TestCmdHistoryShowsSuccessAndFailure(t *testing.T) {
	body := cmdHistoryRendererSource(t)
	// 成功与失败要有不同的视觉标识（badge 颜色或等价物），两者都要出现。
	hasOK := strings.Contains(body, "badge-green")
	hasFail := strings.Contains(body, "badge-red")
	if !hasOK || !hasFail {
		t.Errorf("renderCmdHistory 缺少成功/失败的视觉区分（badge-green=%v badge-red=%v）—— "+
			"命令历史的价值就在区分成败，混在一起等于没渲染", hasOK, hasFail)
	}
	// 退出码必须参与判定（exit_code 或等价字段），否则无法区分「status 缺失」的条目。
	if !strings.Contains(body, "exit_code") && !strings.Contains(body, "status") {
		t.Error("renderCmdHistory 没有使用 exit_code/status —— 成败判定缺少依据")
	}
}

// TestCmdHistoryEscapesOutput 锁死 XSS 面：命令输出必须经 escHtml。
//
// 命令输出是**外部内容**：cmd_run 跑的是任意命令，输出里可以有
// <script>、属性注入等。渲染时不过 escHtml 就是把执行结果直接
// 插进 DOM —— 一次 cat 恶意文件就能在 WebUI 里执行任意 JS。
func TestCmdHistoryEscapesOutput(t *testing.T) {
	body := cmdHistoryRendererSource(t)
	// 输出段（stdout/stderr 拼接处）必须走 escHtml。
	if !strings.Contains(body, "escHtml(out") && !strings.Contains(body, "escHtml(c.stdout") {
		t.Error("renderCmdHistory 渲染命令输出时没有调用 escHtml —— " +
			"输出是外部内容，直接 innerHTML 插入是 XSS 注入点")
	}
	// 命令文本同样来自外部（参数里可带 HTML），也要转义。
	if !strings.Contains(body, "escHtml(c.command") {
		t.Error("renderCmdHistory 渲染命令文本时没有调用 escHtml —— 命令参数同样可注入 HTML")
	}
}

// TestCmdHistoryKeepsRunningTerminals 锁死旧行为里**有效**的部分：
// 运行中的终端仍要显示。
//
// 原始 bug 的旧实现虽然数据源错了，但「显示运行中的终端」这件事本身
// 是对的（长命令确实需要能在这里看到）。重写时必须保留，不能顺手删掉。
func TestCmdHistoryKeepsRunningTerminals(t *testing.T) {
	body := cmdHistoryRendererSource(t)
	if !strings.Contains(body, "running") {
		t.Error("renderCmdHistory 不再显示运行中的终端 —— " +
			"长命令（下载/渲染/构建）需要在命令历史面板可见，这是旧实现里正确的一半")
	}
}

// TestCmdHistoryLoadWritesState 锁死链路上游：loadCmdHistory 必须写 state。
//
// 渲染函数对了、但加载函数不写 state，链路同样断。这条判据保证
// 「/api/v1/cmd/history → state.cmdHistory」这一段不被改坏。
func TestCmdHistoryLoadWritesState(t *testing.T) {
	src := dashboardJSSource(t)
	start := strings.Index(src, "async function loadCmdHistory()")
	if start < 0 {
		t.Fatal("dashboard.js 里找不到 loadCmdHistory")
	}
	end := strings.Index(src[start+1:], "\nfunction ")
	if end < 0 {
		end = len(src) - start - 1
	}
	body := src[start : start+end]
	if !strings.Contains(body, "state.cmdHistory") {
		t.Error("loadCmdHistory 没有把响应写入 state.cmdHistory —— " +
			"渲染端永远拿到空数组，命令历史面板将恒为空")
	}
	if !strings.Contains(body, "/cmd/history") {
		t.Error("loadCmdHistory 没有请求 /cmd/history —— 数据源被换掉时必须同步更新本判据")
	}
}

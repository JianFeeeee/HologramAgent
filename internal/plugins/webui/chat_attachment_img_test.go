package webui

// 聊天图片附件的 HTML 必须**能被浏览器正确切分**（2026-10-30）。
//
// ## 缺陷现场
//
// 生产截图：图片明明加载成功了（img 元素在、src 正确），界面上却多出一行
// 蓝色文字「图片加载失败"/>」。
//
// 根因是内联事件属性里用了反斜杠转义：
//
//	onerror="this.parentElement.innerHTML='<span class=\"att-err\">图片加载失败</span>'"
//
// **HTML 属性值里没有反斜杠转义** —— 那个 \" 就是引号本身，属性在
// `<span class=` 之后就闭合了。浏览器切出来的是：
//
//	onerror = "this.parentElement.innerHTML='<span class="
//
// 剩下的 `att-err">图片加载失败</span>'"/>` 变成**文本节点**直接显示。
// 而那个 onerror 因为语法错误永远不会执行 —— 真失败时反而什么都不显示。
//
// ## 为什么这条判据值得写
//
// 这种缺陷**不会让任何构建或测试失败**：JSON 合法、JS 语法合法、
// 字符串拼接合法。它只在浏览器里错，而且症状（"加载成功却显示加载失败"）
// 指向的方向与真因（引号归属）完全不同。
//
// 判据做法：按正则把 HTML 里所有 `name="value"` 属性切出来，
// 断言每个值内部**不含裸引号**，并断言页面里没有残留的 `"/>` 文本片段。

import (
	"regexp"
	"strings"
	"testing"
)

// attrRe 匹配 HTML 属性。用它模拟浏览器"走到下一个引号就闭合"的行为。
var attrRe = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)="([^"]*)"`)

// TestChatAttachmentImgHasNoBrokenInlineHandler 钉住附件图片的 HTML 形态。
func TestChatAttachmentImgHasNoBrokenInlineHandler(t *testing.T) {
	js := stripJSComments(dashboardJS(t))
	if !strings.Contains(js, "chat-attachment-img") {
		t.Fatal("dashboardJS 里找不到 chat-attachment-img —— 附件图片渲染被改名或删了？")
	}

	// ① 内联 onerror 不能再出现：它是本缺陷的载体。
	//
	//    真正的失败处理已改为**捕获阶段的事件委托**（error 事件不冒泡，
	//    只能在捕获阶段拿到）。所以这里断言"没有内联 onerror"是合理的收紧，
	//    而不是"不许处理错误"。
	if strings.Contains(js, `onerror="this.parentElement`) {
		t.Error("★ 附件图片仍在用内联 onerror —— " +
			"HTML 属性不支持反斜杠转义，属性会被提前闭合、剩余部分变成可见文本。" +
			"请用捕获阶段的 error 事件委托（见 renderChat 里的 addEventListener(\"error\", ..., true)）")
	}

	// ② 失败占位必须走 DOM API，不能拼 innerHTML 字符串。
	//
	//    innerHTML 拼接是这条缺陷的来源（要在字符串里同时处理 HTML 引号、
	//    JS 引号、CSS 选择器）。用 createElement + textContent 只需处理数据。
	if !strings.Contains(js, `span.className = "att-err"`) {
		t.Error("找不到 .att-err 的 DOM 构造 —— 失败占位应改用 createElement/textContent")
	}

	// ③ 必须有捕获阶段的 error 委托（缺了它，图片失败时用户看到破图无提示）。
	//
	//    注意：error 事件不冒泡，`addEventListener("error", fn)` 不带 true
	//    是**收不到** img 加载失败的 —— 这条注释是为了让改的人别再踩。
	if !regexp.MustCompile(`addEventListener\(\s*"error"`).MatchString(js) {
		t.Error("缺少 error 事件监听 —— 图片加载失败时不会显示占位")
	}
}

// TestDashboardNoEscapedQuoteInHTMLAttributes 是**通用**的同类缺陷扫描。
//
// 它不针对某一行，而是扫整个 dashboard.js：凡是形如 `="...\"..."` 的
// 拼接，都会在浏览器里被提前闭合。
//
// 这条判据的价值在于：下次别人再写内联事件（onclick/onerror/onload…）
// 并试图用 \" 转义时，这里会红，而不是等到生产上出现一行诡异的可见文本。
func TestDashboardNoEscapedQuoteInHTMLAttributes(t *testing.T) {
	js := stripJSComments(dashboardJS(t))
	// 找 `="` 之后到下一个 `"` 之间出现反斜杠转义引号的片段。
	// 注意要排除 JS 字符串里的普通转义（如 "a\"b" 作为纯数据），
	// 所以只匹配**紧跟在 HTML 属性名后面**的形态：空白 + 名字 + = "
	bad := regexp.MustCompile(`[a-zA-Z-]+="[^"]*\\"`)
	if m := bad.FindString(js); m != "" {
		t.Errorf("★ 发现 HTML 属性内用反斜杠转义引号（浏览器会提前闭合属性）：\n  %s\n"+
			"HTML 属性值里没有转义序列 —— 需要嵌套引号时改用单引号包属性值，"+
			"或改用 DOM API / 事件委托。", m)
	}
}

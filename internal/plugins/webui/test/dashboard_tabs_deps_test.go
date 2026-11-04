package test

// 判据：「插件」页签的「服务入口」必须真的被请求到数据。
//
// ## 要判的是什么（2026-11-04 生产实测）
//
// 「插件」页签的「服务入口」卡片永远显示「暂无被反代的插件服务」，
// 而 /api/v1/proxy/services **一次都没被请求过**（journalctl 实测：
// 该端点只有我手工 curl 的那一条 401，用户正常使用期间零请求）。
//
// 根因：`renderPlugins()` 第一行就调 `renderProxyServicesCard()`，
// 而后者读 `state.proxyServices`；但 TABS 表里
//
//	plugins: { fetch: ["status","kernel","plugins","runtime"], ... }   ← 没有 proxyServices
//
// 于是 `state.proxyServices` 永远是 undefined ⇒ `svcs = []` ⇒ 空卡片。
// 只有先点进「对话」页（那里 fetch 了 proxyServices）再回「插件」才能看到。
//
// ## 为什么钉「三条事实」而不是做静态调用图
//
// 我第一版写了个递归调用图提取器（render 根 → 展开同文件调用 → 收集
// `state.X` 读点 → 比对 TABS.fetch）。它在实测中**完全失效**：
//
//  1. 中文注释里的引号（「」与 '）会破坏括号/字符串配平扫描，
//     `escHtml` 的函数体被误判成覆盖到文件末尾 ⇒ renderAdapters 的
//     "调用链"里出现 renderKernel / renderOneSettings / renderAll……
//  2. 分发器 `starmapFetchBlock` 的 switch 覆盖全部块名，展开它会让
//     任何调过它的 render 都被判成「缺十几块数据」。
//  3. 按需加载的 state（messages / chatOffset / starmapInit）不属于
//     TABS.fetch，误报成缺口。
//
// 结论：**JS 的静态依赖分析在这个文件上不可靠**。可靠的是钉住本缺陷
// 的**结构性链条**——三件必须同时成立的事，每件都是一次廉价字符串检查：
//
//	A. renderPlugins 确实调用了 renderProxyServicesCard
//	B. renderProxyServicesCard 确实读 state.proxyServices
//	C. TABS.plugins.fetch 确实含 "proxyServices"
//
// 任何一环断了都不会再有"空卡片"这种静默故障：A 断 ⇒ 卡片不再渲染
// （显而易见的 UI 变化，评审会看见）；B 断 ⇒ 读不到数据（同样明显）。
// C 是本次故障的那一环，也是唯一纯粹"看不见"的一环。
//
// 更通用的依赖覆盖检查需要真正的 JS 语义分析（或在 harness 里记录
// fetch 调用序列并与页签切换做配对断言）。那属于测试补强，另行安排；
// 这里刻意只保留一条不会误报、不会假绿的判据。

import (
	"strings"
	"testing"
)

// funcBody 取顶层 `function name(` 的花括号体（配平扫描）。
//
// 只在**函数体内部**做配平，遇到不平衡就返回空串让调用方判失败——
// 宁可判「找不到」也不要基于错误范围给出假结论。
func funcBody(js, name string) string {
	marker := "function " + name + "("
	idx := strings.Index(js, marker)
	if idx == -1 {
		return ""
	}
	// 从开括号起配平参数表。marker 已包含那个 '('，所以深度从 1 起。
	i := idx + len(marker)
	depth := 1
	for ; i < len(js); i++ {
		switch js[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 {
			break
		}
	}
	if i >= len(js) {
		return ""
	}
	// 跳过返回类型/空白，定位函数体 '{'。
	for ; i < len(js) && js[i] != '{'; i++ {
		if js[i] == ';' || js[i] == '=' {
			return "" // 不是声明，是别的形态
		}
	}
	if i >= len(js) {
		return ""
	}
	start := i
	d := 0
	inStr := byte(0)
	for ; i < len(js); i++ {
		c := js[i]
		if inStr != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			inStr = c
		case '{':
			d++
		case '}':
			d--
			if d == 0 {
				return js[start : i+1]
			}
		}
	}
	return ""
}

// TestProxyServicesReachesPluginsTab 是本次实测缺陷的回归判据。
//
// 变异验证：把 TABS.plugins.fetch 里的 "proxyServices" 删掉 ⇒ 立刻红。
func TestProxyServicesReachesPluginsTab(t *testing.T) {
	js := dashboardJSSource(t)

	// —— A. renderPlugins 调用 renderProxyServicesCard ——
	plug := funcBody(js, "renderPlugins")
	if plug == "" {
		t.Fatal("找不到 renderPlugins 的函数体 —— 解析器与源码形态不同步，本判据此刻是假绿")
	}
	if !strings.Contains(plug, "renderProxyServicesCard(") {
		t.Fatalf("renderPlugins 不再调用 renderProxyServicesCard()。\n" +
			"若这是刻意移除，请同时删掉本判据；若是重构漏了，服务入口卡片就消失了。")
	}

	// —— B. renderProxyServicesCard 读取 state.proxyServices ——
	card := funcBody(js, "renderProxyServicesCard")
	if card == "" {
		t.Fatal("找不到 renderProxyServicesCard 的函数体 —— 同上，假绿风险")
	}
	if !strings.Contains(card, "state.proxyServices") {
		t.Fatalf("renderProxyServicesCard 不再读 state.proxyServices —— " +
			"要么它换了数据源（本判据需同步），要么卡片会永远为空")
	}

	// —— C. TABS.plugins.fetch 含 proxyServices ——
	tabs := parseTabs(t, js)
	for _, tab := range tabs {
		if tab.name != "plugins" {
			continue
		}
		for _, f := range tab.fetch {
			if f == "proxyServices" {
				return // 三环皆成立
			}
		}
		t.Fatalf("TABS.plugins.fetch 缺少 \"proxyServices\"。\n"+
			"症状（2026-11-04 生产实测）：「服务入口」卡片永远显示"+
			"「暂无被反代的插件服务」，即使插件确实声明了 proxies ——"+
			"因为 /api/v1/proxy/services 根本没被请求。\n"+
			"renderPlugins 第一行调用 renderProxyServicesCard()，后者读 state.proxyServices；"+
			"renderPlugins 是 TABS.plugins.render，所以 proxyServices 必须在该页签的 fetch 里。\n"+
			"当前 fetch = %v", tab.fetch)
	}
	t.Fatal("TABS 里找不到 plugins 页签 —— 解析器与源码形态不同步，本判据此刻是假绿")
}

// tabsSpec 是从 dashboard.js 源码解析出的 TABS 条目。
type tabsSpec struct {
	name    string
	fetch   []string
	renders []string
}

// parseTabs 用**花括号配平扫描**解析 TABS 对象。
//
// 为什么不用正则切条目（实测踩过）：单行条目
// `starmap: { fetch: [], render: [...] },` 的右花括号在同一行，
// 非贪婪正则于是吞到**下一个**多行条目末尾 —— `starmap` 把紧随其后的
// `plugins` 整块吞进自己体内，症状是 plugins 页签"不存在"。
func parseTabs(t *testing.T, js string) []tabsSpec {
	t.Helper()
	start := strings.Index(js, "var TABS = {")
	if start == -1 {
		t.Fatal("找不到 var TABS")
	}
	openBrace := start + strings.Index(js[start:], "{")

	// 配平 TABS 主体。
	d := 0
	end := -1
	for i := openBrace; i < len(js); i++ {
		switch js[i] {
		case '{':
			d++
		case '}':
			d--
			if d == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		t.Fatal("TABS 花括号配平失败")
	}
	obj := js[openBrace+1 : end]

	var out []tabsSpec
	i := 0
	for i < len(obj) {
		// 跳过空白与逗号/换行，停在下一个标识符。
		for i < len(obj) && (obj[i] == ' ' || obj[i] == '\t' || obj[i] == '\n' || obj[i] == '\r' || obj[i] == ',') {
			i++
		}
		k := i
		for k < len(obj) && (obj[k] == '_' || obj[k] >= 'a' && obj[k] <= 'z' ||
			obj[k] >= 'A' && obj[k] <= 'Z' || obj[k] >= '0' && obj[k] <= '9') {
			k++
		}
		if k == i {
			i++
			continue
		}
		name := obj[i:k]
		r := k
		for r < len(obj) && (obj[r] == ' ' || obj[r] == '\t' || obj[r] == '\n' || obj[r] == '\r') {
			r++
		}
		if r >= len(obj) || obj[r] != ':' {
			i = k
			continue
		}
		r++
		for r < len(obj) && (obj[r] == ' ' || obj[r] == '\t' || obj[r] == '\n' || obj[r] == '\r') {
			r++
		}
		if r >= len(obj) || obj[r] != '{' {
			i = k
			continue
		}
		dd := 0
		close_ := -1
		for j := r; j < len(obj); j++ {
			switch obj[j] {
			case '{':
				dd++
			case '}':
				dd--
				if dd == 0 {
					close_ = j
				}
			}
			if close_ >= 0 {
				break
			}
		}
		if close_ < 0 {
			t.Fatalf("条目 %q 花括号配平失败", name)
		}
		body := obj[r : close_+1]
		spec := tabsSpec{name: name}
		if f := quotedList(body, "fetch:"); f != nil {
			spec.fetch = f
		}
		if rr := quotedList(body, "render:"); rr != nil {
			spec.renders = rr
		}
		out = append(out, spec)
		i = close_ + 1
	}
	if len(out) == 0 {
		t.Fatal("TABS 解析出 0 个页签 —— 解析器与源码形态不同步")
	}
	return out
}

// quotedList 取 `key: ["a", "b"]` 里的字符串列表。
func quotedList(body, key string) []string {
	idx := strings.Index(body, key)
	if idx == -1 {
		return nil
	}
	lb := strings.Index(body[idx:], "[")
	if lb == -1 {
		return nil
	}
	lb += idx
	rb := strings.Index(body[lb:], "]")
	if rb == -1 {
		return nil
	}
	inner := body[lb+1 : lb+rb]
	var out []string
	for _, p := range strings.Split(inner, ",") {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

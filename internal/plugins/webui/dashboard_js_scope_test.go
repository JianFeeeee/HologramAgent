package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dashboard.js 的**作用域**判据：所有被跨函数调用的 render* 必须是顶层声明。
//
// ## 要判的是什么（2026-10-05 生产实测白屏根因）
//
//	Uncaught (in promise) ReferenceError: renderChatUsage is not defined
//	    at starmapFetchBlock (dashboard.js:3830)   ← 顶层函数
//	    at async Promise.all (index 2)
//	    at async renderAll (dashboard.js:3878)
//
// renderChatUsage 定义在 dashboard.js:4475，**缩进是一个 tab**，
// 位置在 connectSSE()（4342 行）内部 ⇒ 它只在 connectSSE 执行期间处于作用域内。
//
// 而 starmapFetchBlock 是**顶层函数**，在 renderAll 里与其它 fetch 并发跑，
// 那时 connectSSE 早已返回 ⇒ renderChatUsage 是 undefined ⇒ ReferenceError。
//
// ★ 为什么这会导致「大量页面白屏」而不是「少一个数字」：
//
//	renderAll 里 `await Promise.all(needs.map(starmapFetchBlock))`
//	**没有 try/catch**，而 fetch 列表里 overview/plugins/kernel 三个页签
//	都含 "kernel" ⇒ 任何一页触发它，Promise.all 整体 reject，
//	**后面的 spec.render.forEach 根本不执行** ⇒ 整页空白。
//
//	renderAll 明明给 render 步骤写了 `typeof window[fn] === "function"` 守卫、
//	也给星图写了 try/catch，唯独漏了 fetch 这一步 —— 而它跑在 render 之前。
//
// ★ 为什么这类 bug 能活下来：
//   - Go 侧测试只验 dashboard.html/js/css 的**组装**（TestDashboardAssetsSplit），
//     不验 JS 语义 ⇒ 编译、vet、单测全绿。
//   - 它只在**运行期、且 state.kernel.usage 存在**（即 /kernel 返回了用量）时触发。
//     新装实例 usage 为 0/null 时不炸 ⇒ 装完看着好好的，跑几天后才白屏。
//
// 判据做法：用 node 在沙箱里把 dashboard.js 跑一遍，抓未捕获异常。
// node 不在时 t.Skip（CI 里 GO 环境可能没有 node）。

// dashboardJSSource 取出待测脚本文本（测试内复用以免重复读 embed）。
func dashboardJSSource(t *testing.T) string {
	t.Helper()
	b, err := dashboardFS.ReadFile("dashboard.js")
	if err != nil {
		t.Fatalf("读 dashboard.js: %v", err)
	}
	return string(b)
}

// TestDashboardJS_HasNoReferenceErrorInStrictMode 用 node 跑真脚本抓 ReferenceError。
//
// ★ 为什么必须真跑而不是 grep「函数名出现过几次」：
//
//	文本上 renderChatUsage 出现了 2 次（定义 + 调用），grep 查不出作用域问题。
//	作用域只能在真执行时暴露 —— 这是本仓反复记的「测真实行为而非形态」。
func TestDashboardJS_HasNoReferenceErrorInStrictMode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("无 node，跳过 JS 语义判据")
	}
	js := dashboardJSSource(t)
	// 触发 kernel 分支里的 renderChatUsage 调用：造一个带 usage 的 /kernel 响应，
	// 然后调 starmapFetchBlock("kernel")。
	//
	// ★★ 桩必须建在 dashboard.js **之前**（否则整个脚本不执行，判据变成「测不到」）：
	//   ① 文件顶部有立即执行的 IIFE（initAppearance / 主题应用），
	//      它们访问 localStorage / document.body / window.innerWidth / matchMedia。
	//   ② 文件末尾有 setInterval(renderAll, 15000) 与 setInterval(sync…, 30000)，
	//      **不接管 setInterval 的话 node 永不退出** —— 实测表现为
	//      exit 0 且 stdout 全空，看起来像「脚本没报错」，实则根本没跑到断言。
	//      这条踩坑本身就是判据设计的坑：静默 hang 会被误读成通过。
	harness := `
const __caught = [];
process.on('uncaughtException', (e) => { __caught.push(String(e)); });
process.on('unhandledRejection', (e) => { __caught.push('Rejection: ' + String(e)); });
const __store = {};
globalThis.localStorage = {
  getItem: (k) => (k in __store ? __store[k] : null),
  setItem: (k, v) => { __store[k] = String(v); },
  removeItem: (k) => { delete __store[k]; },
};
globalThis.matchMedia = () => ({ matches: false, addListener(){}, removeListener(){}, addEventListener(){}, removeEventListener(){} });
globalThis.innerWidth = 1400;
globalThis.innerHeight = 900;
globalThis.addEventListener = () => {};
globalThis.requestAnimationFrame = (f) => setTimeout(f, 16);
globalThis.cancelAnimationFrame = () => {};
// ★ 接管定时器：否则永不退出（见上）。
globalThis.setInterval = () => 0;
globalThis.clearInterval = () => {};
const __mkEl = () => ({
  style: {}, dataset: {}, children: [],
  classList: { add(){}, remove(){}, toggle(){}, contains(){ return false; } },
  setAttribute(){}, getAttribute(){ return null; },
  appendChild(c){ this.children.push(c); return c; },
  removeChild(){}, insertAdjacentHTML(){}, remove(){}, addEventListener(){},
  removeEventListener(){}, querySelector(){ return null; }, querySelectorAll(){ return []; },
  getBoundingClientRect(){ return { width: 0, height: 0, left: 0, top: 0 }; },
  focus(){}, click(){}, closest(){ return null; },
});
globalThis.document = {
  documentElement: { setAttribute(){}, classList:{ add(){}, remove(){}, toggle(){} }, style:{ setProperty(){} } },
  head: __mkEl(), body: __mkEl(),
  createElement: __mkEl, createTextNode: (t) => ({ textContent: t }),
  // ★ getElementById 必须返回**可用元素**而不是 null：
  //   dashboard.js 末尾有顶层调用 renderConfigDisabled()（7129 行），
  //   它对 getElementById 的结果写 .innerHTML。返回 null ⇒ TypeError
  //   ⇒ node 在跑到断言之前就退出，**stdout 全空、exit 0**，
  //   看起来像「没报错」，实则一条断言都没执行 —— 静默假绿。
  getElementById: () => __mkEl(),
  querySelector: () => __mkEl(), querySelectorAll: () => [],
  getElementsByClassName: () => [], getElementsByTagName: () => [],
  addEventListener(){}, removeEventListener(){}, cookie: '',
};
globalThis.window = globalThis;
globalThis.__ = (zh) => zh;
globalThis.navigator = { language: 'zh-CN', userAgent: 'node', clipboard: { writeText: async () => {} } };
globalThis.location = { href: 'http://x/', origin: 'http://x', protocol: 'http:', hostname: 'x', reload(){}, assign(){} };
globalThis.history = { replaceState(){}, pushState(){} };
globalThis.EventSource = class { constructor(){} addEventListener(){} close(){} };
` + js + `
// ① 让 api() 直接返回带 usage 的 kernel，避开网络。
// ★ content-type 必须是 application/json：api()（dashboard.js:414）先看
//   r.headers.get("content-type")，含 "json" 才走 r.json()；
//   否则走 r.text() ⇒ state.kernel 变成字符串 ⇒ kernel 分支的
//   "if (state.kernel && state.kernel.usage)" 为假 ⇒ 压根走不到那一行，
//   判据变成「测不到」（实测 state.kernel = ""）。
globalThis.fetch = async () => ({
  ok: true, status: 200,
  json: async () => ({ usage: { total: 42, prompt: 10, completion: 32 } }),
  text: async () => '',
  headers: { get: (k) => (String(k).toLowerCase() === 'content-type' ? 'application/json' : null) },
});
(async () => {
  try {
    await starmapFetchBlock('kernel');
  } catch (e) {
    console.log('THROWN:' + String((e && e.stack) ? e.stack.split('\\n')[0] : e));
    process.exitCode = 3;
    return;
  }
  await new Promise((r) => setTimeout(r, 60));
  if (__caught.length) {
    console.log('CAUGHT:' + JSON.stringify(__caught));
    process.exitCode = 3;
  } else {
    console.log('OK');
  }
})();
`
	// ★ 必须写临时文件而不是 node -e：dashboard.js 有 500KB+，
	//   内联进命令行会撞 ARG_MAX（实测 "argument list too long"）。
	tmp := filepath.Join(t.TempDir(), "harness.js")
	if err := os.WriteFile(tmp, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("node", tmp).CombinedOutput()
	o := string(out)
	t.Logf("node 输出: %s", strings.TrimSpace(o))
	// ★ 任何形式的运行期异常都算失败，包括 harness 自己报出的 THROWN:。
	//
	//   先前这里只认 "CAUGHT:"，而 harness 是用 try/catch 包住
	//   starmapFetchBlock 并打印 "THROWN:" 的 ⇒ 真实缺陷被判成
	//   「非本次目标」而 Skip，**又是一次假绿**。
	//   判据自己的报告格式不能成为它的免罪符。
	if strings.Contains(o, "THROWN:") || strings.Contains(o, "CAUGHT:") {
		t.Fatalf("dashboard.js 运行期抛错（页面会白屏）:\n%s", strings.TrimSpace(o))
	}
	if err != nil {
		if strings.Contains(o, "OK") {
			return
		}
		t.Fatalf("node 执行失败（判据自身的问题，不能 Skip ——\n"+
			"Skip 会把「测不到」伪装成「通过」）：%v\n%s", err, strings.TrimSpace(o))
	}
	if !strings.Contains(o, "OK") {
		t.Fatalf("node 跑完但没到断言点（又是静默假绿）:\n%s", strings.TrimSpace(o))
	}
}

// TestDashboardJS_RenderHelpersAreTopLevel 静态钉死作用域：
// 凡出现在 TABS[].render 或被 starmapFetchBlock 调用的 render*，缩进必须是 0。
func TestDashboardJS_RenderHelpersAreTopLevel(t *testing.T) {
	js := dashboardJSSource(t)
	lines := strings.Split(js, "\n")

	// 找出所有形如 `\t*function NAME(` 的声明，记录缩进深度。
	depth := map[string]int{}
	for _, l := range lines {
		t2 := strings.TrimLeft(l, "\t")
		if !strings.HasPrefix(t2, "function ") && !strings.HasPrefix(t2, "async function ") {
			continue
		}
		rest := t2[strings.Index(t2, "function ")+len("function "):]
		name := strings.TrimSpace(strings.SplitN(rest, "(", 2)[0])
		name = strings.TrimPrefix(name, "async ")
		depth[name] = len(l) - len(t2) // tab 数
	}

	// 这些必须顶层：TABS 里声明的 render 名 + starmapFetchBlock 里直接调用的。
	mustTop := map[string]string{
		"renderChatUsage": "starmapFetchBlock 的 kernel 分支调用它（dashboard.js:3830）",
		"renderOverview":  "TABS.overview.render",
		"renderPlugins":   "TABS.plugins.render",
		"renderKernel":    "TABS.kernel.render",
		"renderChat":      "TABS.chat.render",
		"renderAdapters":  "TABS.adapters.render",
	}
	for name, why := range mustTop {
		d, ok := depth[name]
		if !ok {
			t.Errorf("%s 未找到声明（%s）", name, why)
			continue
		}
		if d != 0 {
			// 报告行号
			line := 0
			for i, l := range lines {
				t2 := strings.TrimLeft(l, "\t")
				if strings.HasPrefix(t2, "function "+name+"(") ||
					strings.HasPrefix(t2, "async function "+name+"(") {
					line = i + 1
					break
				}
			}
			t.Errorf("%s 声明在缩进 %d 处（dashboard.js:%d），必须顶层（缩进 0）——"+
				"否则顶层函数调用它时是 undefined：%s", name, d, line, why)
		}
	}
}

// TestRenderAll_GuardsEveryStep 钉住 renderAll 的每一步都有兜底。
//
// ★ 这是白屏能扩大的**结构性原因**：fetch 那一步裸奔，
//
//	一个 ReferenceError 就让整页什么都不渲染。
func TestRenderAll_GuardsEveryStep(t *testing.T) {
	js := dashboardJSSource(t)
	start := strings.Index(js, "async function renderAll(")
	if start == -1 {
		t.Fatal("找不到 renderAll")
	}
	// 取到下一个顶层 "\n}\n" 为止。
	end := strings.Index(js[start:], "\n}\n")
	if end == -1 {
		t.Fatal("renderAll 提取失败")
	}
	body := js[start : start+end]

	// Promise.all 那一步必须有容错。
	//
	// ★ 两种等价写法都算合格，不要把判据钉死成某一种：
	//
	//	① 逐项 catch：needs.map(n => starmapFetchBlock(n).catch(...))
	//	② 整体 try/catch：try { await Promise.all(...) } catch {}
	//
	//   先前这条判据只认 ②（找字面 "try {"），而修法用的是 ① ⇒
	//   修好了判据却变红。**判据必须钉「语义」不钉「写法」**，
	//   否则它会惩罚正确的实现。
	pa := strings.Index(body, "Promise.all")
	if pa == -1 {
		t.Fatal("renderAll 里没有 Promise.all，结构变了？")
	}
	before := body[max(0, pa-400):pa]
	after := body[pa:min(len(body), pa+900)]

	// ① 逐项 catch：在 map 回调里对每个 promise 接 .catch(。
	//    窗口要够大：真实写法里 .catch 距 Promise.all 约 200+ 字符
	//    （中间隔着 needs.map(function (n) { … })），窗口太小会漏判。
	hasPerItemCatch := strings.Contains(after, ").catch(") ||
		strings.Contains(after, ").catch(function") ||
		strings.Contains(after, ".catch((e)") ||
		strings.Contains(after, ".catch(function")
	// ② 整体 try/catch：Promise.all 前后有 try { … } catch。
	hasTryCatch := (strings.Contains(before, "try {") || strings.Contains(before, "try{")) &&
		strings.Contains(after, "catch")

	if !hasPerItemCatch && !hasTryCatch {
		t.Errorf("renderAll 的 Promise.all（fetch 步骤）没有容错 ——" +
			"任一 fetch 块抛错会让 Promise.all 整体 reject，**后面的 spec.render 根本不执行**" +
			" ⇒ 整页白屏（生产实测：starmapFetchBlock(\"kernel\") 里的 ReferenceError" +
			" 连带 overview/plugins/kernel 三页全白）。\n" +
			"★ 合格写法二选一：逐项 .catch(...)，或整体 try/catch。" +
			"参考同函数里 spec.render 已有的 typeof 守卫 —— 那一步一直有兜底，" +
			"唯独跑在它之前的 fetch 步骤没有。")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestDashboardJS_NoWriteOnlyGlobals 钉住「只写不读的全局变量」这类静默失效。
//
// ★ 为什么需要（2026-10-08 实际踩到）：`starmapDirty = true` 在 dashboard.js
// 里出现两处，但**没有任何地方读它**，而且它连声明都没有（赋的是隐式全局）。
// 后果是**新写入的记忆块永远不会出现在星图上** —— 后端的 memory_access
// 事件一路发到这里、标志位也置了，就是没人消费，整条链路静默断在这里。
//
// 这类 bug 的特征：不报错、不抛异常、UI 上看不出异常（图只是“不长”），
// 既有的作用域判据（真跑 node）也抓不到 —— 因为隐式全局赋值在非严格模式下
// 完全合法。所以必须用静态判据单独钉。
func TestDashboardJS_NoWriteOnlyGlobals(t *testing.T) {
	raw := dashboardJSSource(t)
	// ★ 必须先剥掉注释再做词频统计：注释里经常提到变量名（本项目尤其如此，
	//   注释大量引用 `starmapDirty = true` 这类代码片段），不剥会让「出现次数」
	//   虚高，把真正只写不读的变量算成有读。
	js := stripJSComments(raw)

	// 顶层 `var X` 声明过的名字。
	declRe := regexp.MustCompile(`\bvar\s+([A-Za-z_$][\w$]*)`)
	declared := map[string]bool{}
	for _, m := range declRe.FindAllStringSubmatch(js, -1) {
		declared[m[1]] = true
	}

	// 只关心布尔标志：赋 true/false 的全局。这类最典型的失效形态就是
	// 「置了标志但没人消费」——功能看着接好了，实际没接。
	for name := range declared {
		nameRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		occur := len(nameRe.FindAllString(js, -1))
		writeRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*=\s*(true|false)\b`)
		writes := len(writeRe.FindAllString(js, -1))
		if writes == 0 {
			continue // 不是布尔标志，不在本判据范围
		}
		// 每次出现要么是一次写入、要么是一次读取。
		// ⇒ 读取次数 = 出现次数 - 写入次数。等于 0 即「只写不读」。
		reads := occur - writes
		if reads <= 0 {
			t.Errorf("全局布尔标志 %s 只被写入、从未被读取（出现 %d 次，写入 %d 次）。\n"+
				"     这类标志位属于**静默失效**：置了标志但无人消费，功能看似接好了实际没接。\n"+
				"     修法：补上消费点，或删掉这个字段。", name, occur, writes)
		}
	}
}

func stripJSComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inLine, inBlock := false, false
	var inStr byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inLine {
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
			continue
		}
		if inBlock {
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if c == '\n' {
			b.WriteByte(c)
			continue
		}
		if inStr != 0 {
			b.WriteByte(c)
			if c == '\\' {
				if i+1 < len(s) {
					b.WriteByte(s[i+1])
					i++
				}
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			// 反引号也是字符串定界符，但它可跨行 —— 本判据只统计标识符，
			// 跨行模板串里出现标识符属正常，此处按普通字符串处理即可。
			inStr = c
			b.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			inLine = true
			i++
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			inBlock = true
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

package proc

// proc 桥**同步一致性**的判据（2026-10-16）。
//
// ## 为什么需要这组判据
//
// 我不久前给公开 SDK 的 `PluginMgrAPI` 加了 3 个方法（StopPlugin/
// StartPlugin/IsPluginRuntimeStopped），当时以为"仅新增，非必须声明，
// 低风险"。**实测发现有两处会漏**：
//
//  ① `internal/plugin/proc` 的方法白名单（4 处：常量 / 能力表 / Handle 分派 /
//     case 体）—— 漏任何一处 ⇒ 子进程插件调这个方法得到"未知 method"
//  ② `hmapdev` 的 `proc_main.go.tmpl`（生成 plugin.bin 的模板）——
//     漏 ⇒ **新生成的插件编译不过**（`procPluginMgr` 不满足新接口）
//
// 这两处都不是"仅新增"能覆盖的：同进程插件（.so）直接调 SDK 不受影响，
// 而子进程插件走桥 ⇒ **不对称**，正是最容易漏的地方。
//
// 判据必须**自动跟随** SDK 接口（不能手工维护清单 —— 手写清单在"新增
// 忘了登记"时同样不会报，守卫形同虚设；本仓已有先例）。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// publishedPluginMgrMethods 从**公开 SDK 源码**提取 PluginMgrAPI 的方法名。
//
// ★ 必须从 SDK 源码提取，不能手写清单：手写清单在"SDK 加了方法但忘了
// 桥接"时**也不会包含新方法** ⇒ 判据照样绿（这就是要防的失效模式）。
func publishedPluginMgrMethods(t *testing.T, sdkPath string) []string {
	t.Helper()
	src, err := os.ReadFile(sdkPath)
	if err != nil {
		t.Fatalf("读公开 SDK: %v", err)
	}
	// 抓 `type PluginMgrAPI interface { ... }` 块
	re := regexp.MustCompile(`(?s)type PluginMgrAPI interface \{(.*?)\n\}`)
	m := re.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("未在公开 SDK 里找到 PluginMgrAPI 接口 —— 接口改名了？本判据需修正")
	}
	body := m[1]
	// 方法行：`Name(args) ret`
	mre := regexp.MustCompile(`(?m)^\s*([A-Z][A-Za-z0-9_]*)\([^)]*\)\s+[A-Za-z\[\]\*\.]+`)
	var out []string
	for _, mm := range mre.FindAllStringSubmatch(body, -1) {
		out = append(out, mm[1])
	}
	if len(out) < 4 {
		t.Fatalf("只从 PluginMgrAPI 提取到 %d 个方法 —— 正则失效（本判据会假绿）: %v", len(out), out)
	}
	return out
}

// ★★★ 判据：**SDK 上每个 PluginMgrAPI 方法都必须在 proc 桥里可达**。
//
// 可达 = 能力表里有它（capabilityOf 能查到）且值非空。
// 这挡住了"SDK 加了方法但忘记登记能力" ⇒ 那种方法会被按 CapCore 放行，
// 或者直接"未知 method"。
func TestBridgeCoversEveryPublishedPluginMgrMethod(t *testing.T) {
	sdkPath := filepath.Join("..", "..", "..", "third_party", "homeagent-sdk", "sdk", "plugin.go")
	methods := publishedPluginMgrMethods(t, sdkPath)

	// SDK 方法名 → proc method 值（命名约定：StopPlugin → plugin.mgrStop）
	// ★ 这里的映射表本身也要有守卫：下面断言"每个 SDK 方法都能找到映射"，
	//   且映射出的值必须真的在能力表里 ⇒ 漏一个就报红。
	wantValues := map[string]string{
		"ReloadOne":              "plugin.reloadOne",
		"ListLoadedPlugins":      "plugin.listLoaded",
		"IsPluginDisabled":       "plugin.isDisabled",
		"StopPlugin":             "plugin.mgrStop",
		"StartPlugin":            "plugin.mgrStart",
		"IsPluginRuntimeStopped": "plugin.mgrIsRuntimeStopped",
	}

	for _, m := range methods {
		val, ok := wantValues[m]
		if !ok {
			t.Errorf("★ SDK 的 PluginMgrAPI 新增了方法 %s，但本判据的映射表没有它 —— "+
				"必须同时：① 在 protocol.go 加 method 常量 ② 登记能力 ③ 加 Handle 分派 "+
				"④ 加 case 体 ⑤ 更新 hmapdev 模板", m)
			continue
		}
		if _, ok := capabilityOf(val); !ok {
			t.Errorf("★ %s（%s）未登记能力归属 —— 子进程插件调用会被拒或绕过权限", m, val)
		}
	}
	// 反向：映射表里不该有 SDK 已删除的方法（防改名后只改一处）
	for name := range wantValues {
		found := false
		for _, m := range methods {
			if m == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("映射表里的 %s 在 SDK 的 PluginMgrAPI 里不存在（改名/删除后漏改？）", name)
		}
	}
}

// ★★★ 判据：**Handle 的分派表覆盖全部 case 体**。
//
// protocol.go 的常量、corehandler.go 的 case 标签、corehandler_runtime.go 的
// case 体是**三处**必须同步的地方。漏任意一处 ⇒ "未知 method" 或死代码。
//
// 这里检查"每个 plugin.* 常量都出现在某个 Handle 分派组里"。
func TestBridgeDispatchesEveryPluginMethod(t *testing.T) {
	proto, err := os.ReadFile("protocol.go")
	if err != nil {
		t.Fatal(err)
	}
	disp, err := os.ReadFile("corehandler.go")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile("corehandler_runtime.go")
	if err != nil {
		t.Fatal(err)
	}

	re := regexp.MustCompile(`(?m)^\s*(MethodPlugin[A-Za-z0-9_]+)\s*=\s*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(string(proto), -1)
	if len(matches) < 5 {
		t.Fatalf("只提取到 %d 个 plugin.* 常量 —— 正则失效", len(matches))
	}

	// 内核→插件方向的 3 个不走 Handle 的 plugin.* 常量
	kernelToPlugin := map[string]bool{
		"MethodPluginInit":  true,
		"MethodPluginStart": true,
		"MethodPluginStop":  true,
	}

	for _, m := range matches {
		name, value := m[1], m[2]
		if kernelToPlugin[name] {
			continue
		}
		// ① 必须在 Handle 分派里出现
		if !strings.Contains(string(disp), name) {
			t.Errorf("★ %s (%s) 未出现在 Handle 分派里 —— 子进程插件调用会得到\"未知 method\"",
				name, value)
			continue
		}
		// ② 必须在某个 case 体里被处理（否则是空分派 ⇒ 静默 nil）
		if !strings.Contains(string(body), "case "+name+":") {
			t.Errorf("★ %s (%s) 在分派里出现但没有对应的 case 体 —— 会被静默吞掉", name, value)
		}
	}
}

// ★★★ 判据：**hmapdev 模板实现了 SDK 的全部 PluginMgrAPI 方法**。
//
// 漏 ⇒ 新生成的 plugin.bin **编译不过**（procPluginMgr 不满足接口）。
// 这是最晚才发现的一环（编译期才报，且只在用户执行 hmapdev build 时暴露）。
func TestHmapdevTemplateImplementsAllPluginMgrMethods(t *testing.T) {
	sdkPath := filepath.Join("..", "..", "..", "third_party", "homeagent-sdk", "sdk", "plugin.go")
	methods := publishedPluginMgrMethods(t, sdkPath)

	tmplPath := filepath.Join("..", "..", "..", "third_party", "homeagent-sdk",
		"tools", "hmapdev", "templates", "proc_main.go.tmpl")
	src, err := os.ReadFile(tmplPath)
	if err != nil {
		// ★ 文件缺失必须 **skip 而不是 fail**（2026-10-17 CI 实测）：
		//
		// `third_party/homeagent-sdk/tools/` 在 .gitignore 里（SDK 仓自治，
		// 本仓不跟踪）。所以：
		//   · 本机（完整 SDK 检出）有该文件 ⇒ 判据能跑
		//   · CI（只 checkout 本仓）没有该文件 ⇒ 原本 Fatalf ⇒ **CI 必红**
		//
		// 我第一次提交时只在本机验证，没看 CI，于是这个判据让
		// "Go build / vet / test" 和 "Build linux/amd64" 双红，把 PR 卡住。
		//
		// skip 不是放弃检查：跨仓一致性由 **SDK 仓自己的判据**守着
		// （third_party/homeagent-sdk/tools/hmapdev/scaffold_stdout_test.go
		//  等），那边是模板的真实归属地。本判据在本机开发时仍然生效。
		t.Skipf("跳过：SDK 仓的 tools/ 不在本仓跟踪范围（.gitignore），"+
			"跨仓一致性由 SDK 仓判据守护。缺文件: %v", err)
	}
	// 只看 procPluginMgr 的方法块。
	//
	// ★ 用**行扫描**而不是正则切块：正则的边界在遇到嵌套/注释时容易提前结束
	// （实测撞了两次：先是 `(\n// |\z)` 被段落注释截断，再是 `\n(type |...)`
	// 仍不够稳）。行扫描的语义直白且不受内容影响。
	block := extractTypeBlock(t, string(src), "type procPluginMgr struct{}")
	if !strings.Contains(block, "func (procPluginMgr) ReloadOne(") {
		t.Fatal("提取到的 procPluginMgr 块不含 ReloadOne ⇒ 边界错误，判据会假绿")
	}

	for _, name := range methods {
		// 期望存在 `func (procPluginMgr) <Name>(`
		if !strings.Contains(block, "func (procPluginMgr) "+name+"(") {
			t.Errorf("★ hmapdev 模板的 procPluginMgr 缺少 %s —— "+
				"用它生成的 plugin.bin **编译不过**（不满足 SDK 接口）", name)
		}
	}
}

// extractTypeBlock 从源码里取出以 header 开头、到下一个顶层声明为止的块。
//
// 行扫描实现：遇到下一个处于**第 0 列**的 `type `/`func `/`var `/`const ` 即结束。
// 这比正则稳，也不需要理解内容（注释/嵌套括号都不影响）。
func extractTypeBlock(t *testing.T, src, header string) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	start := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == header {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("未找到 %q", header)
	}
	_ = start
	var out []string
	for i := start; i < len(lines); i++ {
		if i > start {
			ln := lines[i]
			// 结束条件：下一个**顶层类型/变量/常量声明**。
			//
			// ★ 不能把第 0 列的 `func` 当结束：方法声明（带接收者）也是
			//   第 0 列 `func` —— 初版这样写 ⇒ 块在第一个方法处就结束
			//   ⇒ 所有方法都"缺失"（判据假红，实测撞到）。
			//   区分点：方法声明是 `func (recv)`，普通函数是 `func Name(`。
			isOtherTopDecl := strings.HasPrefix(ln, "type ") ||
				strings.HasPrefix(ln, "var ") || strings.HasPrefix(ln, "const ")
			if isOtherTopDecl {
				break
			}
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n")
}

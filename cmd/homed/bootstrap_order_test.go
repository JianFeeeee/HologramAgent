package main

// 启动顺序的**结构性判据**（2026-10-16）。
//
// ## 为什么必须有这条
//
// 实测故障（我方引入）：安全模块原先装配在 `cfg := cfgReg.ToConfig()` **之后**。
// 于是 `ToConfig()` 在**没有主密钥**的情况下读 `core.llm.*.api_key` ——
// 密文解不开 ⇒ key 变成空串；而 `initLLMProviders(cfg, …)` 用的正是这份
// 早期快照 ⇒ **所有 LLM 调用 401 invalid api key**。
//
// 症状极具误导性：日志里只有 `Dump 解密 core.llm.api_key 失败: security: 无可用主密钥`，
// 而 401 看起来像"key 配错了"。实际是纯粹的**启动顺序**问题 ——
// 查 key 值、改 key、重配 provider 全都无效。
//
// ## 为什么用行号比较而不是行为测试
//
// `main()` 无法在单测里跑（要完整数据目录、要真密钥、要起一堆子系统）。
// 而这里要守的是一个**结构性不变量**：「安全模块的装配点必须早于配置快照点」。
// 行号比较恰好精确表达这个不变量 —— 比字符串包含匹配强得多
// （后者在"注释里提到 ToConfig"时就会假绿或假红）。
//
// 这是本仓"判据要测真实顺序，而不是测某个字符串出现过"的一个正例：
// 判据断言的是**两个真实调用点的相对位置**。

import (
	"os"
	"strings"
	"testing"
)

// ★★★ 判据：安全模块（InitConfigCodec）必须装配在 cfg := cfgReg.ToConfig() 之前。
//
// 违反的后果：LLM key 解不开 ⇒ 全 provider 401，而症状指向"key 配错"。
func TestSecurityModuleInitializesBeforeConfigSnapshot(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读不到 main.go: %v", err)
	}
	src := string(b)

	// 找**真实的调用点**：行内出现调用名，且该行不是注释。
	//
	// 为什么用 Contains 而不是 HasPrefix：真实形式有三种
	//   cfg := cfgReg.ToConfig()
	//   if _, cerr := security.InitConfigCodecWithOldKeys(...); cerr != nil {
	//   key, keySrc, kerr := security.LoadOrCreateKey(...)
	// 用 HasPrefix 会漏掉 if/赋值形态（第一版就是这么漏的）。
	// 注释行仍要排除 —— 否则本文件顶部的说明注释会被当成装配点。
	lineOf := func(call string) int {
		for i, ln := range strings.Split(src, "\n") {
			t := strings.TrimSpace(ln)
			if strings.HasPrefix(t, "//") {
				continue
			}
			if strings.Contains(ln, call) {
				return i + 1
			}
		}
		return -1
	}

	// ★ 锚点已随"内核模块化"改写（2026-10-16）：
	//   `cfg := cfgReg.ToConfig()` → `ks.Cfg = cfgReg.ToConfig()`
	// （快照现在直接存进 KernelState 的公共结构体，用户设计要求）。
	//
	// ★ 为什么匹配 "cfgReg.ToConfig()" 而不是整行：
	// 赋值目标从 `cfg` 变成了 `ks.Cfg`，将来还可能再变。
	// 判据要守的是**"ToConfig() 这个调用点的相对位置"**，
	// 而不是某次具体的赋值写法 —— 锚在调用名上更稳且仍是真实调用点
	// （lineOf 已排除注释行）。
	snapLine := lineOf("cfgReg.ToConfig()")
	if snapLine < 0 {
		t.Fatal("找不到 `cfgReg.ToConfig()`（启动顺序不变量失去锚点）—— " +
			"若方法改名/搬走，请把锚点更新到新的真实调用点，" +
			"**不要**删掉这条判据：它守的是「主密钥必须先于配置快照」")
	}
	// 安全模块的**生效点**是 InitConfigCodecWithOldKeys（配置加密子密钥派生）
	secLine := lineOf("security.InitConfigCodecWithOldKeys(")
	if secLine < 0 {
		secLine = lineOf("security.InitConfigCodec(")
	}
	if secLine < 0 {
		t.Fatal("找不到 InitConfigCodec 调用（安全模块装配点）")
	}
	// 密钥装载点也要在快照之前（两者都在安全块里）
	keyLine := lineOf("security.LoadOrCreateKey(")

	if secLine > snapLine {
		t.Errorf("★ 安全模块装配（第 %d 行）在配置快照（第 %d 行）**之后** —— "+
			"ToConfig() 会在无主密钥时读 core.llm.*.api_key，key 变空 ⇒ 全 provider 401。"+
			"必须把整个安全块前移到 ToConfig() 之前", secLine, snapLine)
	}
	if keyLine < 0 {
		t.Fatal("找不到 LoadOrCreateKey 调用")
	}
	if keyLine > snapLine {
		t.Errorf("★ 主密钥装载（第 %d 行）在配置快照（第 %d 行）之后 —— 同上",
			keyLine, snapLine)
	}
	// 且 provider 注册必须用**快照之后**的 cfg（这条锚定"快照确实被使用了"，
	// 防止有人把 ToConfig() 挪到别处却没人发现）
	// ★ 锚点已随"内核模块化"改写（2026-10-16）：provider 注册搬进 llm 模块，
	// 由 `RunKernelModules` 触发 ⇒ 新的等价锚点是它。
	// 不变量不变：provider 注册必须晚于配置快照（否则拿到空 key）。
	asmLine := lineOf("RunKernelModules(")
	if asmLine > 0 && asmLine < snapLine {
		t.Errorf("★ 内核模块装配（第 %d 行）早于配置快照（第 %d 行）—— "+
			"llm 模块会拿到空 key 注册 provider", asmLine, snapLine)
	}

	// ★ **第二个读取点**：SetLLMSnapshotFile 启动即快照 core.llm.*（会读密文）。
	// 第一版判据只盯了 ToConfig() ⇒ 把它前移后判据变绿，但线上**照旧报**
	// `listPrefix 解密 core.llm.api_key 失败: 无可用主密钥` ——
	// 因为真正早于安全模块的是**这个**调用（main.go 里在 SeedDefaults 之后立刻执行）。
	// 这正是本会话反复吃到的"判据只覆盖一个出口"：修一个点，另一个点照样漏。
	snapFileLine := lineOf("cfgReg.SetLLMSnapshotFile(")
	if snapFileLine < 0 {
		t.Fatal("找不到 SetLLMSnapshotFile 调用")
	}
	if snapFileLine < secLine {
		t.Errorf("★ SetLLMSnapshotFile（第 %d 行）早于安全模块装配（第 %d 行）—— "+
			"启动即快照会读 core.llm.* 密文，主密钥还没装 ⇒ 密文解不开、快照写成空值，"+
			"且日志刷 '无可用主密钥'。必须把安全块前移到它之前", snapFileLine, secLine)
	}
}

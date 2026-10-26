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

	// ★ 2026-10-26 起安全装配搬进正式内核模块 internal/kernelmod/security。
	// 判据必须继续守**真实的装配点**，所以把模块源码一起读进来：
	// 否则锚点（LoadOrCreateKey / InitConfigCodecWithOldKeys）会"找不到"，
	// 而那种失败会被误读成"安全模块没了"。不变量本身不变：
	// 主密钥必须先于任何读 core.llm.* 的操作。
	if mb, merr := os.ReadFile("../../internal/kernelmod/security/mod.go"); merr == nil {
		src += "\n" + string(mb)
	} else {
		t.Fatalf("读不到 kernelmod/security/mod.go（安全模块的实现文件）: %v", merr)
	}
	lineOfMain := func(call string) int {
		for i, ln := range strings.Split(string(b), "\n") {
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
	// 装配入口现在是 main.go 里的 securitymod.Prepare(...)：它必须早于快照。
	prepLine := lineOfMain("securitymod.Prepare(")

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
	// ★ 2026-10-26：模块内的调用是 sec.InitConfigCodecWithOldKeys(...)，
	// 裸源码读取（非 AST）匹配不到包名，故按函数名匹配。
	secLine := lineOf("InitConfigCodecWithOldKeys(")
	if secLine < 0 {
		secLine = lineOf("InitConfigCodec(")
	}
	if secLine < 0 {
		t.Fatal("找不到 InitConfigCodec 调用（安全模块装配点）")
	}
	// 密钥装载点也要在快照之前（两者都在安全块里）
	keyLine := lineOf("LoadOrCreateKey(")

	// 真正的"生效点"是 main.go 里对安全模块 Prepare() 的调用 ——
	// 密钥装载与子密钥派生都在它内部完成。它必须在快照之前。
	if prepLine <= 0 {
		t.Fatalf("找不到 securitymod.Prepare( 调用（安全模块装配入口）")
	}
	if prepLine > snapLine {
		t.Errorf("★ 安全模块装配（main.go 第 %d 行）在配置快照（第 %d 行）**之后** —— "+
			"ToConfig() 会在无主密钥时读 core.llm.*.api_key，key 变空 ⇒ 全 provider 401。"+
			"必须把 securitymod.Prepare() 前移到 ToConfig() 之前", prepLine, snapLine)
	}
	// 且模块内部**确实**装了密钥与子密钥（防止有人把 Prepare 改成空壳而判据仍绿）。
	if secLine <= 0 || keyLine <= 0 {
		t.Errorf("★ 安全模块实现里找不到密钥装配（InitConfigCodec=%d LoadOrCreateKey=%d）—— "+
			"Prepare() 退化成空壳，安全审查将不生效", secLine, keyLine)
	}
	// ★ 行号只在**同一文件内**可比。密钥装载与子密钥派生现在都在
	// kernelmod/security.Prepare() 内部，所以"主密钥先于快照"由上面
	// prepLine < snapLine 保证；这里只需确认模块**确实**装了它们
	//（防止 Prepare() 退化成空壳而判据仍绿）。
	if keyLine <= 0 {
		t.Fatal("安全模块实现里找不到 LoadOrCreateKey 调用（Prepare 可能被改成空壳）")
	}

	// 且 provider 注册必须用**快照之后**的 cfg（这条锚定"快照确实被使用了"，
	// 防止有人把 ToConfig() 挪到别处却没人发现）
	// 不变量不变：provider 注册必须晚于配置快照（否则拿到空 key）。
	asmLine := lineOfMain("RunKernelModules(")
	if asmLine > 0 && asmLine < snapLine {
		t.Errorf("★ 内核模块装配（第 %d 行）早于配置快照（第 %d 行）—— "+
			"llm 模块会拿到空 key 注册 provider", asmLine, snapLine)
	}

	// ★ **第二个读取点**：SetLLMSnapshotFile 启动即快照 core.llm.*（会读密文）。
	// 第一版判据只盯了 ToConfig() ⇒ 把它前移后判据变绿，但线上**照旧报**
	// `listPrefix 解密 core.llm.api_key 失败: 无可用主密钥` ——
	// 因为真正早于安全模块的是**这个**调用。
	// 这正是"判据只覆盖一个出口"：修一个点，另一个点照样漏。
	snapFileLine := lineOfMain("cfgReg.SetLLMSnapshotFile(")
	if snapFileLine < 0 {
		t.Fatal("找不到 SetLLMSnapshotFile 调用")
	}
	if snapFileLine < prepLine {
		t.Errorf("★ SetLLMSnapshotFile（第 %d 行）早于安全模块装配（第 %d 行）—— "+
			"启动即快照会读 core.llm.* 密文，主密钥还没装 ⇒ 密文解不开、快照写成空值，"+
			"且日志刷 '无可用主密钥'。必须把 securitymod.Prepare() 前移到它之前",
			snapFileLine, prepLine)
	}
}

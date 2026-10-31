package all

// 装配级安全判据（2026-10-30）。
//
// ## 为什么必须新开一个文件、而不是加在 security_toolgate_test.go 里
//
// 那边 4 条判据全绿了**整整一轮发布**，而生产里的闸门根本没接上。原因很简单：
//
//	它们自己调了 a.SetSecurity(...)。
//
// 判据把缺失的那根线补上了，所以永远绿 —— 测的是「接线之后行为对」，
// 而不是「生产有没有接线」。这是「机制 ≠ 接线」最纯粹的样本。
//
// 本文件钉的是**装配**：
//
//	kernelmod.Registered() → kernelboot.Build → kernelboot.Run
//	  → security.Init 产出 ks.Security
//	  → agent.Init 把 ks.Security 装进 Agent
//	  → 真实工具执行链读到它（securityDecisionFor → CheckTool）
//
// ★ 变异要求（每条都在实现上验过，见文件末尾的变异表）：
//   ① 从 agent.Requires() 删掉 FacSecurity   ⇒ 判据 1 红
//   ② 从 buildAgent 的 AgentConfig 删 Security ⇒ 判据 2 红
//   ③ 从 resident/rtchild 的 AgentConfig 删 Security ⇒ 判据 3 红
//   ④ 从 New() 删掉 `security: cfg.Security` ⇒ 判据 2 红
//   ⑤ 从 agent.Init 删掉 nil 检查 ⇒ 判据 4 红

import (
	"strings"
	"testing"

	agentcore "github.com/JianFeeeee/HologramAgent/internal/agent/core"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// ───────────────── 判据 1：agent 模块**声明**了 security 依赖 ─────────────────

// ★★★ 判据：`agent` 模块必须在 Requires() 里声明 FacSecurity。
//
// ## 为什么不能靠"层号碰巧对"来代替这条判据
//
// 即使不声明，security 也在第 1 层、agent 在第 4 层 ⇒ `ks.Security` 恰好
// 已被填好，运行期照样能读到。**这正是这个缺陷藏了整轮的原因。**
//
// 但那个"恰好"是偶然的：security 只依赖 config，所以天然在前面。一旦
// security 将来加了自己的依赖（比如判某些策略要先读 llm 配置），它就会被推到
// agent 之后 —— 那时 agent 读到一个 nil 的 ks.Security，而**图不会拦**，
// 因为图根本不知道 agent 需要它。
//
// 所以这条判据断言的是**声明**，不是顺序。与 TestHiddenDependencyOnnxDeclared
// 同型（那条也明确写了"不能用层号断言"）。
func TestAgentDeclaresSecurityDependency(t *testing.T) {
	var matched *kernelboot.Module
	var requires []string
	for _, m := range kernelmod.Registered() {
		if m.Name() != "agent" {
			continue
		}
		requires = m.Requires()
		matched = &m
	}
	if matched == nil {
		t.Fatal("★ agent 模块没注册")
	}
	for _, r := range requires {
		if r == kernelmod.FacSecurity {
			return
		}
	}
	t.Fatalf("★ agent 模块未声明对 %q 的依赖（当前 Requires=%v）。\n"+
		"  后果：buildAgent 读 ks.Security 就成了**隐式顺序依赖** ——\n"+
		"  今天恰好成立（security 在第 1 层），但那是运气不是设计，\n"+
		"  而隐式顺序依赖正是本仓模块化设计要消灭的形态。\n"+
		"  修法：kernelmod/agent 的 Requires() 加 kernelmod.FacSecurity。",
		kernelmod.FacSecurity, requires)
}

// ───────────────── 判据 2：装配后 Agent 的执行链真的读到它 ─────────────────

// ★★★ 判据：走真实登记表装配整机后，给 `ks.Security` 配的黑名单
// **必须**能拦住 Agent 的工具执行链。
//
// ## 与前一代判据的区别（本文件存在的全部理由）
//
// 旧判据：`a := newAgent(); a.SetSecurity(rt); assert(a.securityDecisionFor(...))`
//
//	⇒ 自己补线，永远绿。
//
// 本判据：不碰 SetSecurity，只用**模块装配**产出的 ks.Agent 与 ks.Security，
//
//	并且走**真实执行入口** `ExecuteTool`（即 api.ToolHost，生产里
//	device 与插件工具面都通过它进来）。
//
// ## 为什么用"黑名单里的**不存在**的工具"
//
// 闸门在 `executeToolCallInner` 里位于**分派之前**（参数校验之后）。
// 所以拿一个未注册的工具名去试：
//
//	· 闸门生效 ⇒ 得到 "not permitted"
//	· 闸门失效 ⇒ 落到分派，得到 "does not exist"
//
// 两者文本完全不同，无从混淆。这样判据不需要注册探针工具、不需要造插件，
// 也不会因为工具不存在而"碰巧"通过。
func TestAgentAssemblyWiresSecurity(t *testing.T) {
	m := bootMachine(t)
	ks := m.ks

	if ks.Security == nil {
		t.Fatal("★ security 模块没产出 ks.Security（装配链断了）")
	}
	if ks.Agent == nil {
		t.Fatal("★ agent 模块没产出 ks.Agent")
	}

	const deniedTool = "boot_probe_denied_tool"

	// ① 先证明这个工具名**没被注册**（否则下面"not permitted"可能来自别处）。
	out, _ := ks.Agent.ExecuteTool(deniedTool, nil)
	if strings.Contains(out, "not permitted") {
		t.Fatalf("前置不成立：未配黑名单时就已被拒（%q）—— 检查环境里的实际策略", out)
	}

	// ② 关键一步：给**模块化产出**的 ks.Security 装黑名单。
	//    如果 Agent 里拿的是另一个实例（或 nil），它看不到这条 ⇒ 判据红。
	ks.Security.SetPolicy(security.Policy{ToolDeny: []string{deniedTool}})

	out, _ = ks.Agent.ExecuteTool(deniedTool, nil)
	if !strings.Contains(out, "not permitted") {
		t.Fatalf("★ 已给 ks.Security 配 %q 黑名单，Agent 的执行链却没拦住（实际返回 %q）。\n"+
			"  可能原因（按概率）：\n"+
			"    ① buildAgent 没把 ks.Security 传进 AgentConfig.Security\n"+
			"    ② New() 没把 cfg.Security 写进 a.security\n"+
			"    ③ 执行链上少了 securityDecisionFor 这道闸\n"+
			"  三者任一都会让策略表正确而完全无效，且日志里一条拒绝都没有。",
			deniedTool, out)
	}

	// ③ 反向：未命中的工具必须放行。
	//    否则是"全拒"型假绿（闸门写死了），而不是接线正确。
	if out, _ := ks.Agent.ExecuteTool("boot_probe_unlisted_tool", nil); strings.Contains(out, "not permitted") {
		t.Fatalf("未命中黑名单的工具被误拒（%q）—— 判定逻辑坏了，不是接线问题", out)
	}
}

// ───────────────── 判据 4：装配失败时响亮，而不是静默放行 ─────────────────

// ★★★ 判据：`ks.Security == nil` 时 agent 模块必须**拒绝装配**。
//
// 防的失败方式：把 nil 一路传下去 —— Agent 装好了、日志写着 "kernel ready"、
// 工具照常执行，只是策略永远不生效。本项目已经踩过一次这个坑
// （"策略表存在、判定函数正确、日志里一条拒绝都没有"）。
//
// 注意区分：配置显式关闭安全模块时装的是 **Disabled Runtime（非 nil）**，
// 所以本判据不会误伤"用户选择不用安全模块"这个合法场景。
//
// 实现要点：用**真实模块的 Init**，不是一个复制品。bootMachine 已把整机跑过
// 一遍（所有依赖真实就绪），这里借它的 KernelState 复制一份、只破坏一个前提。
// agent.Init 的 nil 检查在所有副作用之前（第一个 if），所以本判据不产生资源。
func TestAgentAssemblyRefusesNilSecurity(t *testing.T) {
	m := bootMachine(t)
	base := m.ks
	if base.Security == nil {
		t.Fatal("前置失败：整机装配没产出 Security")
	}

	var agentMod kernelboot.Module
	for _, mod := range kernelmod.Registered() {
		if mod.Name() == "agent" {
			agentMod = mod
		}
	}
	if agentMod == nil {
		t.Fatal("★ agent 模块没注册")
	}

	// ★ 不能复制 KernelState（它含 sync.Mutex，复制即 vet 报错，且真的不安全）。
	// 改为新建一个：把**真实状态里已就绪的全部依赖**逐字段搬过去，
	// 唯独 Security 留 nil —— 这样 Init 只会因为这一个前提失败。
	//
	// 为什么这样做而不是直接用 base：Init 成功时会覆盖 base.Agent，
	// 破坏后续判据的上下文。新建一份是干净的。
	var probe kernelboot.KernelState
	probe.Cfg = base.Cfg
	probe.CfgReg = base.CfgReg
	probe.DataDir = base.DataDir
	probe.AgentWorkDir = base.AgentWorkDir
	probe.CLISocket = base.CLISocket
	probe.HTTPAddr = base.HTTPAddr
	probe.Ctx = base.Ctx
	probe.Memory = base.Memory
	probe.LLMProvider = base.LLMProvider
	probe.LLMManager = base.LLMManager
	probe.Tracker = base.Tracker
	probe.DocStore = base.DocStore
	probe.Knowledge = base.Knowledge
	probe.TextMemory = base.TextMemory
	probe.StageHost = base.StageHost
	probe.PluginRegistry = base.PluginRegistry
	probe.EventBus = base.EventBus
	probe.Embedder = base.Embedder
	probe.MultimodalSpace = base.MultimodalSpace
	probe.MultimodalProviderName = base.MultimodalProviderName
	probe.MultimodalErr = base.MultimodalErr
	probe.BaseAPIKey = base.BaseAPIKey
	probe.Security = nil // ← 唯一被破坏的前提

	err := agentMod.Init(&probe)
	if err == nil {
		t.Fatal("★ ks.Security 为 nil 时 agent 模块居然装配成功了 —— " +
			"nil 会被一路传到 securityDecisionFor，那里恒放行且不留任何日志。\n" +
			"  必须在 Init 里响亮失败（与 device 模块对 ks.Security 的检查同型）。")
	}
	if !strings.Contains(err.Error(), "security") {
		t.Fatalf("失败信息必须点名缺失的设施（便于运维一眼定位），实际: %v", err)
	}
}

// ───────────────── 判据 4：父为子**声明**策略，而不是子继承父的权限 ─────────────────

// ★★★ 判据：父创建子 agent 时，父声明的工具策略必须**真的限制住那个子**，
// 且**只影响那个子**（不是全局）。
//
// ## 为什么这版取代了上一版（用户裁定 2026-10-30）
//
// 上一版的判据断言「子继承父的 `a.security`」—— 那只保证了**机制**到位，
// 却把权限模型写错成了「子 = 父的完整权限」。
//
// 正确模型是 **父为子声明策略**：
//
//	父：能调全部工具（受全局策略约束）
//	子：父在创建时决定它能调什么 —— 内核持有并执行，子不能自己扩大
//
// 这不是新机制：`security.CheckToolFor(Principal, tool)` 与
// `PrincipalPolicy{AllowTools, DenyTools}` 早就在树里，只是**零生产调用方**
// （与工具策略闸、ScanContent 同型：机制完整、接线缺失）。
//
// ## 本判据钉三件事
//
//	① 父声明的 deny 真的拦住**子**
//	② 同一条策略**不影响父**（不是全局策略）
//	③ 子销毁后策略被撤销（同名重建不能继承上一代父的约束）
func TestParentDeclaresChildPolicy(t *testing.T) {
	rt := security.NewRuntime(nil)

	// 父：parentID 为空 ⇒ 走 CheckTool（只受全局策略约束）。
	const forbidden = "resident_denied_tool"
	parent := agentcore.New(agentcore.AgentConfig{
		ID:       "main",
		Provider: nil,
		Security: rt,
	})
	if d := parent.ProbeSecurityDecision(forbidden); !d.Allowed {
		t.Fatalf("父不应受限（它是内核自身，此工具不受任何全局策略限制）: %s", d.Reason)
	}

	// 父为子声明策略：允许 probe_ok_*，禁止 forbidden。
	childID := "policy-probe-child"
	if err := parent.ProbeDeclareChildPolicy(childID, []string{"probe_ok_*"}, []string{forbidden}); err != nil {
		t.Fatalf("父为子声明策略失败: %v", err)
	}

	// ① 子被判住：子 parentID 非空 ⇒ 走 CheckToolFor(Principal{self})
	child := agentcore.New(agentcore.AgentConfig{
		ID:       types.AgentID(childID),
		ParentID: "main",
		Provider: nil,
		Security: rt, // 同一个 Runtime 实例（它只是机制，权限靠主体声明）
	})
	if d := child.ProbeSecurityDecision(forbidden); d.Allowed {
		t.Fatalf("★ 父已为子 %q 声明禁止 %q，子的判定却仍是 Allowed —— "+
			"主体策略没有被工具执行链读到（CheckToolFor 未接线）", childID, forbidden)
	}
	if d := child.ProbeSecurityDecision("other_tool"); d.Allowed {
		t.Fatal("★ 父给了 probe_ok_* 白名单，other_tool 却仍被放行 —— 白名单未生效")
	}
	if d := child.ProbeSecurityDecision("probe_ok_1"); !d.Allowed {
		t.Fatalf("白名单内的工具被误拒（%q）—— 判定逻辑坏了，不是接线问题", d.Reason)
	}

	// ② 同一条策略不影响父（主体隔离）。
	if d := parent.ProbeSecurityDecision(forbidden); !d.Allowed {
		t.Fatal("★ 子策略泄漏成了全局策略 —— 父也被禁了。主体隔离失效")
	}

	// ③ 撤销后子恢复（同名重建不能继承上一代父的约束）。
	parent.ProbeRevokeChildPolicy(childID)
	if d := child.ProbeSecurityDecision(forbidden); !d.Allowed {
		t.Fatal("★ 策略已撤销，子却仍被禁 —— 主体表以调用方给的名字为键，" +
			"不清理就会让同名重建的新一代继承上一代父的约束（与 rtchild 留档串线同型）")
	}
}

// ★★★ 判据：**销毁路径必须撤销策略**（两处对称：单数与复数）。
//
// 这条单列，因为本仓反复吃亏在"单数路径改了、复数路径漏了"：
// `StopRTChild` 注销入站登记而 `StopRTChildren` 漏了，就是这么来的。
//
// 用源码断言：真销毁需要完整记忆栈/rt 通道，而"前置不满足就 skip"
// 的判据等于没有（本仓已有同型先例：TestHiddenDependencyOnnxDeclared）。
func TestChildTeardownRevokesPolicy(t *testing.T) {
	cases := []struct{ file, fn, why string }{
		{"../../agent/core/resident.go", "func (a *Agent) teardownResidentWith(",
			"驻留子销毁必须撤销父为它声明的策略"},
		{"../../agent/core/rtchild.go", "func (a *Agent) StopRTChild(",
			"rtchild 单数停止必须撤销策略"},
		{"../../agent/core/rtchild.go", "func (a *Agent) StopRTChildren(",
			"rtchild 批量停止必须与单数路径对称地撤销策略"},
	}
	for _, c := range cases {
		src := readMod(t, c.file)
		i := strings.Index(src, c.fn)
		if i < 0 {
			t.Fatalf("%s 里找不到 %s —— 符号改名了？判据需要跟着改", c.file, c.fn)
		}
		body := src[i:]
		if end := strings.Index(body, "\n}\n"); end > 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "revokeChildPolicy") {
			t.Errorf("★ %s 里没有 revokeChildPolicy —— %s。\n"+
				"  后果：主体表以调用方给的名字为键，回收后同名重建会继承\n"+
				"  上一代父留下的约束（而那个父可能已经不在了），且表单调增长。",
				c.fn, c.why)
		}
	}
}

// ★★★ 判据：创建路径必须在**子开始执行前**声明策略。
//
// 顺序错（比如在 go carrier.rtLoop 之后声明）会留下一个"无约束执行"窗口：
// 子一接到输入就会跑工具，那时策略可能还没进内核。
func TestPolicyDeclaredBeforeChildStarts(t *testing.T) {
	cases := []struct{ file, declAnchor, startAnchor, name string }{
		{"../../agent/core/resident.go", "declareChildPolicy(opts.ID", "child.Start()", "驻留子"},
		{"../../agent/core/rtchild.go", "declareChildPolicy(opts.ID", "go carrier.rtLoop(rc)", "rtchild"},
	}
	for _, c := range cases {
		src := readMod(t, c.file)
		di := strings.Index(src, c.declAnchor)
		si := strings.Index(src, c.startAnchor)
		if di < 0 || si < 0 {
			t.Fatalf("%s: 找不到锚点（decl=%v start=%v）", c.file, di >= 0, si >= 0)
		}
		if di > si {
			t.Errorf("★ %s：策略声明在 `%s` **之后** —— 存在一个「子已开始执行、策略尚未生效」的窗口",
				c.name, c.startAnchor)
		}
	}
}

// ───────────────── 判据 5：策略语义只能在 security 内部 ─────────────────

// ★★★ 判据：`agent/core` **不得**再做策略决策。
//
// ## 收敛前它做了什么（2026-10-31）
//
//	security_profile.go 里 214 行，其中绝大部分是策略逻辑：
//	  · 自己从 payload 抠 `security_profile` 字段
//	  · 自己判断 "parentID != "" → 走 CheckToolFor，否则 CheckTool"
//	  · 自己拼 `PrincipalPolicy{AllowTools:…, DenyTools:…}` 字面量
//	  · 自己写 `Kind: "resident"` **声明自己的主体类别**
//
// 最后一条是提权口子：调用方说自己是内部主体，就享受内部豁免
// （不过 HardPrivateTools）。类别必须由 security 从**事实**推出。
//
// ## 本判据用什么钉
//
// 源码断言（跨包行为断言做不到"禁止某种写法"）。三类越界各一条正则：
//
//	① `payload["security_profile"]` 出现在 agent/core ⇒ 解析回落调用方
//	② `Kind:` 字面量出现在 agent/core 的安全文件 ⇒ 调用方声明类别
//	③ `CheckTool(` / `CheckToolFor(` 直接出现在 agent/core ⇒ 越级选链
//
// ★ 例外：`security_profile.go` 的**注释**里会提到这些名字（解释历史）。
// 所以逐行判断，跳过纯注释行。
func TestAgentCoreHoldsNoPolicySemantics(t *testing.T) {
	src := readMod(t, "../../agent/core/security_profile.go")

	// 会被禁止的代码形态（不含注释）。
	banned := []struct{ pattern, why string }{
		{`payload["security_profile"]`,
			"档位字段名是 security 的协议，解析必须留在 security.BeginRound"},
		{`Kind:`,
			"主体类别必须由 security 从事实（有无父）推出，调用方不得声明"},
		{`.CheckToolFor(`,
			"选哪条判定链是策略决策，调用方只该调 Round.CheckTool"},
		{`PrincipalPolicy{`,
			"策略结构由 security 组装，调用方只该传 allow/deny 列表"},
	}
	for _, b := range banned {
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // 注释里解释历史是允许的
			}
			if strings.Contains(line, b.pattern) {
				t.Errorf("★ agent/core/security_profile.go:%d 出现 %q —— %s\n\t%s",
					i+1, b.pattern, b.why, trimmed)
			}
		}
	}
}

// ★★★ 判据：主体类别由 security 从**事实**推出，且**不信**调用方声明。
//
// 收敛前 `Kind: "resident"` 由 agent 写死 —— 它同时意味着"我是内部"
// 与"我不受收窄"，把两件不同的事绑在一个字符串上。现在分类规则只有一处
// （`classifyPrincipal`），本判据钉住三条映射。
func TestPrincipalKindIsDerivedBySecurity(t *testing.T) {
	rt := security.NewRuntime(nil)

	// ① 有父 ⇒ 子执行体（内部豁免，但可被父收窄）
	if p := rt.ClassifyPrincipalForTest(security.RoundInput{AgentID: "kid", IsChild: true}); p.Kind != security.KindSubAgent {
		t.Fatalf("有父的执行体应归为 KindSubAgent，实际 %q", p.Kind)
	} else if !p.IsInternal() || !p.IsSubAgent() {
		t.Fatal("KindSubAgent 必须同时满足 IsInternal（豁免 HardPrivate）与 IsSubAgent（可被收窄）")
	}

	// ② 无 id ⇒ 内核自身
	if p := rt.ClassifyPrincipalForTest(security.RoundInput{}); p.Kind != security.KindInternal {
		t.Fatalf("无 id 应归为 KindInternal，实际 %q", p.Kind)
	}

	// ③ 有 id 无父 ⇒ 用户
	if p := rt.ClassifyPrincipalForTest(security.RoundInput{AgentID: "qq-user-1"}); p.Kind != security.KindUser {
		t.Fatalf("经插件通道进来的应归为 KindUser，实际 %q", p.Kind)
	}

	// ④ 枚举不接受自由字符串：拼错必须能被识别（而不是静默变成用户主体）
	if _, ok := security.ParsePrincipalKind("residnet"); ok {
		t.Fatal("★ 拼错的主体类别被当成合法值 —— 拼写错误会静默漂移权限语义")
	}
	if k, ok := security.ParsePrincipalKind("resident"); !ok || k != security.KindSubAgent {
		t.Fatal("旧值 resident 应兼容映射为 KindSubAgent（不破坏既有调用方）")
	}
}

// ★★★ 判据：轮次边界在**全部输入路径**上都被声明。
//
// 收敛把判定逻辑收到了一处，但"一轮何时开始/结束"只有各路径自己知道。
// 本仓已有三条独立输入路径，漏一条就是"那类 agent 永远没有档位"：
//
//	主 agent + 驻留子  → runInputTask（驻留子有自己的 Agent 实例）
//	rtchild           → rtRunOnce（不走调度器，单独一条）
//
// 用源码断言：真跑三条路径需要完整记忆栈/rt 通道，
// 而"前置不满足就 skip"的判据等于没有（与 TestChildTeardownRevokesPolicy 同一理由）。
func TestEveryInputPathDeclaresRound(t *testing.T) {
	cases := []struct{ file, fn, why string }{
		{"../../agent/core/task.go", "func (a *Agent) runInputTask(",
			"主 agent 与驻留子共用的输入入口"},
		{"../../agent/core/rtchild.go", "func (a *Agent) rtRunOnce(",
			"rtchild 不走调度器、不经过 runInputTask，是独立的一条输入路径"},
	}
	for _, c := range cases {
		src := readMod(t, c.file)
		i := strings.Index(src, c.fn)
		if i < 0 {
			t.Fatalf("%s 里找不到 %s —— 符号改名了？判据需要跟着改", c.file, c.fn)
		}
		body := src[i:]
		if end := strings.Index(body, "\n}\n"); end > 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "beginSecurityRound(") {
			t.Errorf("★ %s 里没有 beginSecurityRound —— %s 缺少安全轮次边界。\n"+
				"  后果：这条路径上的输入永远不会激活插件声明的档位，\n"+
				"  而它在**其他**路径上是生效的 ⇒ 表现为「时灵时不灵」，最难查。",
				c.fn, c.why)
		}
		if !strings.Contains(body, "End()") {
			t.Errorf("★ %s 里没有 End() —— 档位会在轮次结束后泄漏到下一轮，"+
				"guest 的收窄误伤后续输入（方向相反就是提权）", c.fn)
		}
	}
}

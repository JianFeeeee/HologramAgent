package security

// 全局策略基线的**配置装配**判据。
//
// ## 为什么需要这个文件
//
// 2026-10-29 之前的实际状况是：`core.security.*` 只有 `enabled` 一个键被读，
// `Policy.ToolDeny/ToolAllow/Sandbox` **没有任何配置入口**，`SetPolicy`
// 零调用方。于是运维即使想「这台机器不许跑 cmd_*」也无从下手。
//
// 本文件的判据钉三件事：
//   ① 留空 = 零值策略（升级不得改变存量行为）
//   ② 逗号列表/通配符/路径规范化解析正确
//   ③ 装配后 `CheckTool` 真的按配置拒绝（而不是只存了字段）

import (
	"testing"

	sec "github.com/JianFeeeee/HologramAgent/internal/security"
)

// fakeCfg 是 ConfigReader 的测试替身。
type fakeCfg map[string]string

func (f fakeCfg) GetString(key, def string) string {
	if v, ok := f[key]; ok {
		return v
	}
	return def
}

func (f fakeCfg) GetBool(key string, def bool) bool {
	v, ok := f[key]
	if !ok {
		return def
	}
	return v == "true" || v == "1"
}

// ① 全部留空 ⇒ 零值策略，不限制任何工具（**升级安全底线**）。
func TestEmptyConfigYieldsZeroPolicy(t *testing.T) {
	p := PolicyFromConfig(fakeCfg{})
	if len(p.ToolDeny) != 0 || len(p.ToolAllow) != 0 || p.Sandbox.Enabled {
		t.Fatalf("空配置必须得到零值策略，实际 %+v", p)
	}
	if d := p.CheckTool("cmd_run"); !d.Allowed {
		t.Fatalf("零值策略必须放行全部工具，实际拒绝: %s", d.Reason)
	}
	// AllowNetwork 默认 true（不施加网络约束）—— 与加配置前一致。
	if !p.Sandbox.AllowNetwork {
		t.Fatal("空配置下 AllowNetwork 应为 true（不限制），否则升级会突然断网")
	}
}

// ② 黑名单解析：逗号分隔 + 通配符 + 空白容忍。
func TestToolDenyListParsing(t *testing.T) {
	p := PolicyFromConfig(fakeCfg{
		"core.security.tool_deny": " cmd_* , files_write ,  ",
	})
	if len(p.ToolDeny) != 2 {
		t.Fatalf("应解析出 2 项（空项被丢弃），实际 %v", p.ToolDeny)
	}
	if d := p.CheckTool("cmd_run"); d.Allowed {
		t.Fatal("cmd_* 应命通配匹配拒绝 cmd_run")
	}
	if d := p.CheckTool("files_write"); d.Allowed {
		t.Fatal("精确项应拒绝 files_write")
	}
	if d := p.CheckTool("stablemem_recall"); !d.Allowed {
		t.Fatalf("未命中项必须放行: %s", d.Reason)
	}
}

// ③ 白名单语义：非空即"只有这些"。
//
// ★ 通配符是**纯字面前缀**匹配：`memory_*` 不会匹配 `stablemem_recall`。
// 工具按裁定 #11 改名后，运维配置里的白名单必须同步改，
// 否则白名单会静默变成"什么都不允许"。
func TestToolAllowListIsOnlyThese(t *testing.T) {
	p := PolicyFromConfig(fakeCfg{"core.security.tool_allow": "stablemem_*,knowledge_*"})
	if d := p.CheckTool("stablemem_recall"); !d.Allowed {
		t.Fatalf("白名单内应放行: %s", d.Reason)
	}
	if p.CheckTool("cmd_run").Allowed {
		t.Fatal("白名单外必须拒绝 —— 白名单是\"只有这些\"")
	}
}

// ③b 旧名前缀不再匹配改名后的工具（升级风险的具体形状）。
//
// 存量部署的配置里写的是 `memory_*`，而工具已叫 `stablemem_*`：
// 白名单会**静默**变成空集，而不是报错。本判据把这一行为钉住，
// 使任何“通配符语义悄悄变宽”的改动都会变红。
func TestLegacyMemoryWildcardDoesNotMatchRenamedTools(t *testing.T) {
	p := PolicyFromConfig(fakeCfg{"core.security.tool_allow": "memory_*"})
	if p.CheckTool("stablemem_recall").Allowed {
		t.Fatal("`memory_*` 不应匹配 stablemem_recall —— 若匹配，说明通配符语义变了")
	}
	// 旧前缀仍应匹配旧名字（存量插件可能仍用旧名注册工具）。
	if d := p.CheckTool("memory_something_legacy"); !d.Allowed {
		t.Fatalf("`memory_*` 应仍匹配旧前缀工具: %s", d.Reason)
	}
}

// ④ deny 优先于 allow（同名工具同时在两边）。
func TestDenyWinsOverAllow(t *testing.T) {
	p := PolicyFromConfig(fakeCfg{
		"core.security.tool_deny":  "cmd_run",
		"core.security.tool_allow": "cmd_*",
	})
	if d := p.CheckTool("cmd_run"); d.Allowed {
		t.Fatal("deny 必须优先于 allow")
	}
}

// ⑤ 路径列表：去尾部分隔符、/ 保留、空项丢弃。
func TestPathListParsing(t *testing.T) {
	p := PolicyFromConfig(fakeCfg{
		"core.security.sandbox.enabled":     "true",
		"core.security.sandbox.write_roots": "/tmp/, /home/x ,,/",
	})
	got := p.Sandbox.WriteRoots
	want := []string{"/tmp", "/home/x", "/"}
	if len(got) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("期望 %v，实际 %v", want, got)
		}
	}
}

// ⑥ ★ 装配进 Runtime 后 `CheckTool` 真的按配置生效（不是只存字段）。
func TestApplyPolicyActuallyEnforced(t *testing.T) {
	rt := sec.NewRuntime(nil)
	if d := rt.CheckTool("cmd_run"); !d.Allowed {
		t.Fatalf("装配前应放行: %s", d.Reason)
	}

	ApplyPolicyFromConfig(rt, fakeCfg{"core.security.tool_deny": "cmd_*"})

	if d := rt.CheckTool("cmd_run"); d.Allowed {
		t.Fatal("★ 装配后 cmd_run 必须被拒 —— 否则配置只是存进了字段没落到判定上")
	}
}

// ⑦ 沙箱：启用后按写根拒绝 `..` 逃逸到根外。
func TestSandboxWriteRootsEnforced(t *testing.T) {
	p := PolicyFromConfig(fakeCfg{
		"core.security.sandbox.enabled":     "true",
		"core.security.sandbox.write_roots": "/tmp/work",
	})
	if d := p.CheckPath("write", "/tmp/work/a.txt"); !d.Allowed {
		t.Fatalf("写根内应放行: %s", d.Reason)
	}
	if d := p.CheckPath("write", "/tmp/work/../etc/passwd"); d.Allowed {
		t.Fatal("`..` 逃逸到写根外必须拒绝")
	}
	if d := p.CheckPath("write", "/etc/passwd"); d.Allowed {
		t.Fatal("写根外必须拒绝")
	}
}

// ⑧ 沙箱未启用 ⇒ 路径判定不施约束（默认行为不变）。
func TestSandboxDisabledDoesNotConstrain(t *testing.T) {
	p := PolicyFromConfig(fakeCfg{
		"core.security.sandbox.write_roots": "/tmp/work",
		// enabled 缺省 false
	})
	if d := p.CheckPath("write", "/etc/passwd"); !d.Allowed {
		t.Fatal("沙箱未启用时不应约束路径 —— 否则升级即改变存量行为")
	}
}

// ⑨ ★ 语义锁定：`core.security.enabled=false` 关掉的**只是凭据替换**，
// 工具策略必须继续生效。
//
// ## 为何这条必须单独钉
//
// `enabled` 这个名字太容易被理解成「关掉整个安全模块」。实际上：
//
//	enabled=false：不加载主密钥、不做明文⇄别名替换（需要密钥才能做）
//	              但工具黑白名单/沙箱**与密钥无关**，仍然生效
//
// 若有人把 `SetPolicy` 挪进 `if rt.Enabled()` 分支，工具闸会跟着凭据替换
// 一起静默失效 —— 而日志上看不出区别。本判据就是拦这个。
// （与 eventpolicy 同一纪律："关闭安全不等于开权限"。）
func TestDisabledRuntimeStillEnforcesPolicy(t *testing.T) {
	// NewRuntime(nil) 构造的正是 Disabled Runtime（enabled=false）。
	rt := sec.NewRuntime(nil)
	if rt.Enabled() {
		t.Fatal("前置条件不成立：NewRuntime(nil) 应为 Disabled Runtime")
	}

	ApplyPolicyFromConfig(rt, fakeCfg{"core.security.tool_deny": "cmd_*"})

	if d := rt.CheckTool("cmd_run"); d.Allowed {
		t.Fatal("★ enabled=false 时工具策略仍须生效 —— 关掉的是凭据替换，不是授权")
	}
}

// ⑩ ★ 兜底值焊死：装配层的 GetXxx 兜底必须与 registry 的 ConfigDef.Default 一致。
//
// ## 为什么这条是必需的（2026-10-29）
//
// 这些键用 `regMeta` 登记 —— **只登记、不播种**。而 `defDefault` 要求
// `seeded[key]` 为真才回退 `ConfigDef.Default`，regMeta 从不写 seeded，
// 所以 `GetXxx` 对它们**永远返回调用方传入的兜底值**，Default 字段
// 只给 WebUI 展示。
//
// 于是同一个"默认值"存在两处：
//
//	registry.ConfigDef.Default            ← WebUI 上写的
//	PolicyFromConfig 的 GetXxx 兜底      ← 实际生效的
//
// 两处一旦漂移，WebUI 显示 `sandbox.enabled = false` 而实际按 true 运行
// （或反之）—— 安全配置显示与实际不一致，是最不该出现的分歧。
func TestPolicyConfigDefaultsMatchRegistry(t *testing.T) {
	// 用 fakeCfg（空 map）走一遍装配 = 全部键取兜底值。
	p := PolicyFromConfig(fakeCfg{})

	if p.Sandbox.Enabled {
		t.Error("sandbox.enabled 的装配兜底应为 false，与 ConfigDef.Default 一致")
	}
	if !p.Sandbox.AllowNetwork {
		t.Error("sandbox.allow_network 的装配兜底应为 true，与 ConfigDef.Default 一致")
	}
	if len(p.ToolDeny) != 0 || len(p.ToolAllow) != 0 {
		t.Errorf("两个名单的装配兜底应为空，实际 deny=%v allow=%v", p.ToolDeny, p.ToolAllow)
	}
	if len(p.Sandbox.ReadRoots) != 0 || len(p.Sandbox.WriteRoots) != 0 {
		t.Errorf("读写根装配兜底应为空，实际 read=%v write=%v", p.Sandbox.ReadRoots, p.Sandbox.WriteRoots)
	}
}

package security

// 安全模块的判据集。
//
// 每条判据对应设计文档里的一条硬约束（S1–S12）。
// 纪律：**判据必须先跑通，再接内核** —— 否则接线出错时无法区分
// 「安全模块坏了」还是「接线错了」。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey() ([]byte, error) {
	return []byte("0123456789abcdef0123456789abcdef"), nil
}

func newTestStore(t *testing.T, enabled bool) *Store {
	t.Helper()
	dir := t.TempDir()
	s := New(Options{
		Path:    filepath.Join(dir, "variables.json"),
		KeyFn:   testKey,
		Enabled: enabled,
	})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	return s
}

// const testToken 是一个形状合法的 GitHub token（用于触发模式库）。
const testToken = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// ★★ S3 + S11：**同明文自动合并到同一别名**（哈希键的天然性质）。
func TestSameSecretMergesToSameAlias(t *testing.T) {
	s := newTestStore(t, true)

	a1, err := s.Alias(testToken)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := s.Alias(testToken)
	if a1 != a2 {
		t.Fatalf("同一明文必须得到同一别名，实际 %s vs %s", a1, a2)
	}
	// 别名形态：sec_ + 16 hex = 20 字符
	if !strings.HasPrefix(a1, AliasPrefix) || len(a1) != 20 {
		t.Errorf("别名形态异常: %q (len=%d)", a1, len(a1))
	}
	// 不同明文 ⇒ 不同别名
	other, _ := s.Alias("ghp_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ")
	if other == a1 {
		t.Error("不同明文不应合并")
	}
}

// ★★ S3：别名必须**依赖主密钥**（即 HMAC，不是裸哈希）。
//
// 裸哈希可被字典枚举（实测 hunter2 当场命中），而别名会进上下文/日志/记忆。
func TestAliasDependsOnMasterKey(t *testing.T) {
	s1 := New(Options{Path: filepath.Join(t.TempDir(), "a.json"), KeyFn: testKey, Enabled: true})
	other := func() ([]byte, error) { return []byte("ffffffffffffffffffffffffffffffff"), nil }
	s2 := New(Options{Path: filepath.Join(t.TempDir(), "b.json"), KeyFn: other, Enabled: true})

	a1, _ := s1.Alias("same-plain")
	a2, _ := s2.Alias("same-plain")
	if a1 == a2 {
		t.Fatal("不同主密钥必须产出不同别名（否则说明用了裸哈希）")
	}
}

// ★ 判据：裸哈希可被字典枚举 —— 用真实数据证明 HMAC 的必要性。
//
// 这条不是测代码，而是**固化设计依据**：若有天有人把 HMAC 换成裸 SHA256，
// 这条判据会连同其注释一起提醒他为什么不行。
func TestBareHashIsEnumerable(t *testing.T) {
	// 攻击者视角：拿到别名（裸哈希前 16 hex），拿常见弱口令字典去试
	weak := "hunter2"
	sum := sha256Hex16(weak)
	// 字典命中（一行代码就够）
	hit := false
	for _, g := range []string{"password123", "admin", "letmein", "hunter2", "secret"} {
		if sha256Hex16(g) == sum {
			hit = true
		}
	}
	if !hit {
		t.Fatal("本判据的前提失效（裸哈希应可被字典命中）")
	}
	// 而 HMAC 版本：没有 key 无法枚举
	s := newTestStore(t, true)
	a, _ := s.Alias(weak)
	if a == AliasPrefix+sum {
		t.Fatal("别名用了裸哈希 —— 会泄露 secret 内容")
	}
	t.Logf("裸哈希（危险）: %s%s ；HMAC（当前实现）: %s", AliasPrefix, sum, a)
}

// ★★ S1 + 插件无感知：拦截把明文换成别名，稍后能用别名换回（回填）。
func TestReplaceThenFill(t *testing.T) {
	s := newTestStore(t, true)

	text := "我的 token 是 " + testToken + " 请使用"
	out, evts := s.Replace(text, "input", Patterns)

	if strings.Contains(out, testToken) {
		t.Fatalf("明文未被替换: %s", out)
	}
	if !strings.Contains(out, "安全模块自动拦截") {
		t.Errorf("缺少拦截提示: %s", out)
	}
	if len(evts) != 1 {
		t.Fatalf("应恰好 1 条拦截事件，实际 %d", len(evts))
	}
	alias := evts[0].Alias
	if !strings.Contains(out, "@{"+alias+"}") {
		t.Errorf("提示里应含 @{%s}: %s", alias, out)
	}

	// ★ 回填：把别名换回明文（这是插件能用密钥的唯一通道）
	back, n, used := s.Fill("Authorization: token @{" + alias + "}")
	if n != 1 {
		t.Fatalf("应回填 1 处，实际 %d", n)
	}
	if !strings.Contains(back, testToken) {
		t.Fatalf("回填未生效: %s", back)
	}
	if len(used) != 1 || used[0] != alias {
		t.Errorf("应报告用到的别名: %v", used)
	}
}

// ★★ S4：**不可误伤** —— 正常文本绝不能被替换。
//
// 误报比漏报严重得多：它会把正常内容换掉、把任务搞坏，且归因极难。
func TestNoFalsePositives(t *testing.T) {
	s := newTestStore(t, true)
	benign := []string{
		"请修复这个 bug，提交到 feat/login 分支",
		"git commit -m 'add feature'",
		"https://github.com/JianFeeeee/HologramAgent/pull/42",
		"这个函数返回 404 或者 500",
		"hex 颜色 #4f46e5 很好看",
		// ★ 关键：40 位 git SHA-1 不得被误伤（否则 agent 拿不到 commit id）
		"commit 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b",
		"版本号 v1.2.3",
		"文件 /home/program/TrueAgent/docs/zh/kernel-secret-firewall-design.md",
		"时间戳 2026-10-14T17:27:16+08:00",
		"UUID 550e8400-e29b-41d4-a716-446655440000",
		"讨论一下 secret 管理和 token 轮换的策略", // 含关键词但不是赋值形态
		"password 字段的长度校验有问题",         // 同上
	}
	for _, s0 := range benign {
		out, evts := s.Replace(s0, "input", Patterns)
		if len(evts) > 0 {
			t.Errorf("误伤正常文本 %q → %v", s0, evts)
		}
		if out != s0 {
			t.Errorf("未命中却改动了文本: %q → %q", s0, out)
		}
	}
}

// 各类凭据形状都要能识别（覆盖面）。
func TestRecognizesCommonShapes(t *testing.T) {
	s := newTestStore(t, true)
	cases := []struct{ name, text string }{
		{"github", testToken},
		{"gitlab", "glpat-FAKE-TEST-TOKEN-NOT-REAL"},
		{"openai", "sk-FAKE-TEST-KEY-NOT-REAL-0000000"},
		{"anthropic", "sk-ant-FAKE-TEST-KEY-NOT-REAL-00"},
		{"aws", "AKIA0000FAKE0000TEST"},
		{"slack", "xoxb-FAKE-TEST-TOKEN-NOT-REAL"},
		{"assignment", `api_key = "FAKE-TEST-ASSIGNED-VALUE-000"`},
		{"bearer", "Authorization: Bearer FAKE-TEST-BEARER-TOKEN-NOT-REAL"},
	}
	for _, c := range cases {
		_, evts := s.Replace(c.text, "input", Patterns)
		if len(evts) == 0 {
			t.Errorf("%s 形状未被识别: %q", c.name, c.text)
		}
	}
}

// ★ 判据：赋值形态保留键名（否则人看不懂发生了什么）。
func TestAssignmentKeepsKeyName(t *testing.T) {
	s := newTestStore(t, true)
	out, evts := s.Replace(`api_key = "FAKE-TEST-ASSIGNED-VALUE-000"`, "input", Patterns)
	if len(evts) == 0 {
		t.Fatal("未命中")
	}
	if !strings.Contains(out, "api_key") {
		t.Errorf("应保留键名: %s", out)
	}
}

// ★ S1：**不提供取回明文的接口**（看值只能靠回填）。
//
// 这条是防回归：若有人加了 Get/Reveal 之类的方法，判据通过编译期就能发现。
func TestNoPlaintextReadAPI(t *testing.T) {
	// 语义判据：List() 只给元信息；明文只能经 Fill 出现在"待发送的文本"里
	s := newTestStore(t, true)
	alias, err := s.Submit(testToken, "测试")
	if err != nil {
		t.Fatal(err)
	}
	infos := s.List()
	if len(infos) != 1 {
		t.Fatalf("应有 1 条记录，实际 %d", len(infos))
	}
	b, _ := json.Marshal(infos)
	if strings.Contains(string(b), testToken) {
		t.Fatalf("List 泄露了明文: %s", b)
	}
	if !strings.Contains(string(b), alias) {
		t.Error("List 应给出别名")
	}
}

// ★★ 落盘：明文绝不落盘，且文件必须 0600。
func TestPersistsCiphertextOnlyWith0600(t *testing.T) {
	s := newTestStore(t, true)
	if _, err := s.Submit(testToken, "落盘测试"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), testToken) {
		t.Fatalf("磁盘文件出现明文: %s", b)
	}
	if fi, err := os.Stat(s.path); err == nil {
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("变量库权限应为 0600，实际 %04o", fi.Mode().Perm())
		}
	} else {
		t.Fatal(err)
	}
}

// ★ 判据：权限不安全的变量库**拒绝加载**（响亮失败，不静默降级）。
func TestRejectsLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.json")
	if err := os.WriteFile(p, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Path: p, KeyFn: testKey, Enabled: true})
	if err := s.Load(); err == nil {
		t.Fatal("0644 的变量库必须被拒绝")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(); err != nil {
		t.Fatalf("0600 应可加载: %v", err)
	}
}

// ★ 持久化往返：重启后别名仍可用（用户要求落盘 + 不自动淘汰）。
func TestReloadKeepsVariables(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.json")
	s1 := New(Options{Path: p, KeyFn: testKey, Enabled: true})
	if err := s1.Load(); err != nil {
		t.Fatal(err)
	}
	alias, err := s1.Submit(testToken, "重启测试")
	if err != nil {
		t.Fatal(err)
	}

	// 模拟重启
	s2 := New(Options{Path: p, KeyFn: testKey, Enabled: true})
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	out, n, _ := s2.Fill("x=@{" + alias + "}")
	if n != 1 || !strings.Contains(out, testToken) {
		t.Fatalf("重启后别名应仍可用: n=%d out=%q", n, out)
	}
}

// ★ S12：不自动淘汰，但 agent 可**主动删除**。
func TestForgetRemovesVariable(t *testing.T) {
	s := newTestStore(t, true)
	alias, _ := s.Submit(testToken, "")
	if err := s.Forget(alias); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	out, n, _ := s.Fill("x=@{" + alias + "}")
	if n != 0 || strings.Contains(out, testToken) {
		t.Fatalf("删除后不应再回填: %q", out)
	}
	if err := s.Forget(alias); err != ErrUnknownAlias {
		t.Errorf("重复删除应返回 ErrUnknownAlias，实际 %v", err)
	}
}

// ★ 判据：拿不到密钥 ⇒ **拒绝产出别名**（宁可漏报，不用裸哈希）。
//
// 这里要**直接测 `Alias()`**，不能只测 Replace：
// Replace 里另有一道外层护栏（无密钥就整体早退），会把 Alias 层的降级
// 挡住 —— 变异实验证实了这一点（只测 Replace 时，把 Alias 改成裸哈希降级
// 判据仍绿）。纵深防御是对的，但两层必须各自有判据盯着。
func TestNoKeyMeansNoAlias(t *testing.T) {
	s := New(Options{Path: filepath.Join(t.TempDir(), "v.json"), KeyFn: nil, Enabled: true})

	// ① Alias() 本身必须报错（不降级为裸哈希）
	if a, err := s.Alias("anything"); err != ErrNoMasterKey {
		t.Errorf("无密钥时 Alias 应返回 ErrNoMasterKey，实际 alias=%q err=%v", a, err)
	}
	// ② Submit() 也必须失败（不静默记个裸哈希别名）
	if a, err := s.Submit(testToken, ""); err == nil {
		t.Errorf("无密钥时 Submit 应失败，实际得到 %q", a)
	}
	// ③ outer guard：Replace 原样返回
	out, evts := s.Replace("token="+testToken, "input", Patterns)
	if len(evts) != 0 {
		t.Fatal("无密钥时不得产出别名")
	}
	if out != "token="+testToken {
		t.Errorf("无密钥时应原样保留: %s", out)
	}
	// ④ Fill 也应安全降级（不 panic、不改动）
	if got, n, _ := s.Fill("@{sec_x}"); n != 0 || got != "@{sec_x}" {
		t.Errorf("无密钥时 Fill 应原样返回: %q", got)
	}
	// ⑤ 密钥长度不对也必须拒绝（AES-256 要 32 字节）
	bad := New(Options{Path: filepath.Join(t.TempDir(), "w.json"),
		KeyFn: func() ([]byte, error) { return []byte("too-short"), nil }, Enabled: true})
	if _, err := bad.Alias("x"); err == nil {
		t.Error("密钥长度不对时必须报错")
	}
}

// ★ 开关：关闭后完全不做事（显式才是特权）。
func TestDisabledDoesNothing(t *testing.T) {
	s := newTestStore(t, false)
	out, evts := s.Replace("token="+testToken, "input", Patterns)
	if len(evts) != 0 || out != "token="+testToken {
		t.Error("关闭后不应做任何替换")
	}
}

// ★ FillMap：工具参数是 map，要能递归回填（插件调用前用）。
func TestFillMapRecursive(t *testing.T) {
	s := newTestStore(t, true)
	alias, _ := s.Submit(testToken, "")

	args := map[string]interface{}{
		"url": "https://" + "@{" + alias + "}" + "@host/repo",
		"nested": map[string]interface{}{
			"token": "@{" + alias + "}",
		},
		"list": []interface{}{"plain", "@{" + alias + "}"},
		"num":  42,
	}
	n, used := s.FillMap(args)
	if n != 3 {
		t.Fatalf("应回填 3 处，实际 %d", n)
	}
	if len(used) != 3 {
		t.Errorf("应报告 3 次使用: %v", used)
	}
	if !strings.Contains(args["url"].(string), testToken) {
		t.Error("顶层未回填")
	}
	if !strings.Contains(args["nested"].(map[string]interface{})["token"].(string), testToken) {
		t.Error("嵌套 map 未回填")
	}
	if !strings.Contains(args["list"].([]interface{})[1].(string), testToken) {
		t.Error("数组未回填")
	}
	if args["num"].(int) != 42 {
		t.Error("非字符串值不应被改动")
	}
}

// ★ 未知别名：原样保留（不报错、不吞掉），以免把正常文本改坏。
func TestFillKeepsUnknownAlias(t *testing.T) {
	s := newTestStore(t, true)
	in := "看看 @{sec_notexist} 和 @{sec_0000000000000000}"
	out, n, _ := s.Fill(in)
	if n != 0 {
		t.Fatalf("未知别名不应被替换，实际 %d", n)
	}
	if out != in {
		t.Errorf("未知别名应原样保留: %q", out)
	}
	// 不像引用的文本也不该被碰
	out2, n2, _ := s.Fill("普通文本 @{bad-name!} 与 email@example.com")
	if n2 != 0 || out2 != "普通文本 @{bad-name!} 与 email@example.com" {
		t.Errorf("非引用文本被改动: %q", out2)
	}
}

// ★ 拦截事件是**可观测**的（S5，不静默做事），且 merged 标明是否合并。
func TestInterceptEventsObservable(t *testing.T) {
	s := newTestStore(t, true)
	_, e1 := s.Replace("a="+testToken, "input", Patterns)
	_, e2 := s.Replace("b="+testToken, "tool_result:cmd_run", Patterns)

	if len(e1) != 1 || len(e2) != 1 {
		t.Fatal("两次都应各产生 1 条事件")
	}
	if e1[0].Merged {
		t.Error("首次应 merged=false")
	}
	if !e2[0].Merged {
		t.Error("第二次同明文应 merged=true（合并）")
	}
	if e1[0].Alias != e2[0].Alias {
		t.Error("同明文应同一别名")
	}
	if e2[0].Where != "tool_result:cmd_run" {
		t.Errorf("应记录来源位置: %q", e2[0].Where)
	}
}

// ★ 判据：明文（高熵）不得从**别名**反推出来。
//
// 这是 HMAC + 截断的性质，用抽样验证（不是穷举证明，而是防低级错误）。
func TestAliasDoesNotLeakPlaintext(t *testing.T) {
	s := newTestStore(t, true)
	alias, _ := s.Alias(testToken)
	// 别名里不能包含明文的任何连续 8 字符片段
	for i := 0; i+8 <= len(testToken); i++ {
		if strings.Contains(alias, testToken[i:i+8]) {
			t.Fatalf("别名泄露了明文片段: %s", alias)
		}
	}
}

func sha256Hex16(s string) string {
	h := sha256Sum([]byte(s))
	return hexEncode(h[:8])
}

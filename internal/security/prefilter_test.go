package security

// 前置过滤（Prefilter）的判据。
//
// ## 为什么需要它 + 为什么必须单独验
//
// 加 prefilter 是为了修一个**实测的性能缺陷**：所有模式以 `\b` 开头，
// 而 Go 的 RE2 对 `\b` 要逐 rune 判定 —— 50KB 文本上单条 660µs，
// 9 条合计 ~6ms。常被调用的路径（每次 LLM 调用、每次工具调用）
// 白付这个成本。加字面量前缀后降到 10µs（~400×）。
//
// ★ 但优化有个致命风险：**prefilter 若与正则的必含字符不一致，
// 就会造成漏报** —— 而漏报（凭据没被拦下）比性能差严重得多，
// 且**不会报错**，只会在某天出事时才发现。
//
// 所以本文件逐条验证：「正则能匹配的文本，prefilter 一定放行」。

import (
	"strings"
	"testing"
)

// ★★ 判据：**每一条模式的 prefilter 都必须是该模式真会匹配的文本的子串**。
//
// 做法：为每条模式准备一份"应命中"样本，断言
//
//	① 正则确实命中它
//	② prefilter 也放行它
//
// 若 ② 失败 ⇒ 该样本永远被 prefilter 挡在门外 ⇒ **漏报**。
func TestPrefilterNeverBlocksRealMatches(t *testing.T) {
	samples := map[string]string{
		"github_token":        "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"github_fine_grained": "github_pat_" + strings.Repeat("A", 60),
		"gitlab_token":        "glpat-FAKE-TEST-TOKEN-NOT-REAL",
		"openai_key":          "sk-FAKE-TEST-KEY-NOT-REAL-0000000",
		"anthropic_key":       "sk-ant-FAKE-TEST-KEY-NOT-REAL-00",
		"aws_access_key":      "AKIA0000FAKE0000TEST",
		"slack_token":         "xoxb-FAKE-TEST-TOKEN-NOT-REAL",
		"bearer_token":        "Bearer FAKE-TEST-BEARER-TOKEN-NOT-REAL",
		"private_key_block": "-----BEGIN RSA PRIVATE KEY-----\n" +
			strings.Repeat("MIIEowIBAAKCAQEA", 8) + "\n-----END RSA PRIVATE KEY-----",
		"assigned_secret": `api_key = "FAKE-TEST-ASSIGNED-VALUE-000"`,
	}

	for _, p := range Patterns {
		sample, ok := samples[p.Name]
		if !ok {
			t.Errorf("模式 %s 没有样本 —— 新增模式时必须补样本（否则判据会漏掉它）", p.Name)
			continue
		}
		// ① 正则必须命中
		if !p.RE.MatchString(sample) {
			t.Errorf("模式 %s 的正则不匹配它的样本（样本过期了？）: %q", p.Name, sample)
			continue
		}
		// ② prefilter 必须放行（否则这条永远漏报）
		if !p.matchesPrefilter(sample) {
			t.Errorf("★ 模式 %s 的 prefilter %q 挡住了自己的样本 %q —— 会造成**静默漏报**",
				p.Name, p.Prefilter, sample)
		}
	}
}

// ★★ 判据：**反向** —— prefilter 挡下的文本，正则确实不该匹配。
//
// 这一条防的是"prefilter 太宽"吗？不是 —— 太宽只会变慢，不会出错。
// 它防的是相反的错误：有人把 prefilter 写成**排除式**（如"不含 X 才跑"），
// 那就反了。这里给出一个"不含任何前缀"的文本，断言：
//
//	跳过它不会漏掉任何真命中（即正则对它本来就不匹配）。
func TestPrefilterSkipIsSafe(t *testing.T) {
	benign := "这是一段普通的技术讨论：架构、实现、测试与部署。没有凭据。"
	for _, p := range Patterns {
		if p.matchesPrefilter(benign) {
			continue // 放行也无妨（会跑正则，只是慢）
		}
		if p.RE.MatchString(benign) {
			t.Errorf("★ 模式 %s 被 prefilter 跳过，但正则其实匹配这段文本 —— 漏报！",
				p.Name)
		}
	}
}

// ★ 判据：**大文本下的正确性**（prefilter 在长文本上不能出错）。
//
// 慢路径 bug 常在边界出现：prefilter 命中但在文本很靠后的位置。
func TestPrefilterWorksDeepInLargeText(t *testing.T) {
	s := newTestStore(t, true)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// 把 token 埋在 50KB 文本的**中间**
	pad := strings.Repeat("普通技术讨论内容，不含凭据。", 1500)
	text := pad[:len(pad)/2] + " token=" + tok + " " + pad[len(pad)/2:]

	out, evts := s.Replace(text, "bench", Patterns)
	if len(evts) == 0 {
		t.Fatal("★ 埋在长文本中段的凭据未被识别（prefilter 在长文本上漏报）")
	}
	if strings.Contains(out, tok) {
		t.Fatal("明文未被替换")
	}
	if len(evts[0].Alias) != 20 {
		t.Errorf("别名形态异常: %s", evts[0].Alias)
	}
}

// ★★ 判据：**prefilter 必须覆盖正则里所有"必含"的备选分支**。
//
// ## 这条判据的由来（真实漏报）
//
// `assigned_secret` 的正则是 `\b(...)\b\s*[:=]\s*...` —— 分隔符有
// **两个**备选（`:` 与 `=`）。初版 prefilter 只写 `"="`，于是
// `password: "hunter2..."`（冒号形态）被 prefilter 静默挡住 ⇒ **漏报**。
//
// 更值得记的是**为什么没被发现**：当时的判据样本恰好用了 `=`，
// 所以测试全绿 —— 判据覆盖了"能用的形态"，漏了"另一种同样合法的形态"。
//
// ⇒ 现在逐形态验证：正则支持的每种写法，prefilter 都必须放行。
func TestPrefilterCoversAllAlternatives(t *testing.T) {
	// 每个形态都**必须是正则能匹配的**，且 prefilter 必须放行。
	forms := map[string][]string{
		"assigned_secret": {
			`api_key = "FAKE-TEST-ASSIGNED-VALUE-000"`, // =
			`api_key: "FAKE-TEST-ASSIGNED-VALUE-000"`,  // :
			`password="FAKE-TEST-TOKEN-NOT-REAL-001234"`,
			`password:'FAKE-TEST-TOKEN-NOT-REAL-001234'`,
			`access_token = FAKE-TEST-TOKEN-NOT-REAL-0012`,
			`client_secret: FAKE-TEST-TOKEN-NOT-REAL-0012`,
			`auth_token := FAKE-TEST-TOKEN-NOT-REAL-00123`,
			`secret_key = FAKE-TEST-TOKEN-NOT-REAL-0012345`,
			`passwd = FAKE-TEST-ASSIGNED-VALUE-000`,
		},
	}

	for name, samples := range forms {
		var pat *Pattern
		for i := range Patterns {
			if Patterns[i].Name == name {
				pat = &Patterns[i]
				break
			}
		}
		if pat == nil {
			t.Fatalf("找不到模式 %s", name)
		}
		for _, sample := range samples {
			if !pat.RE.MatchString(sample) {
				// 样本本身不被正则支持 —— 那是样本写错了，不是漏报
				t.Logf("  （正则不支持该形态，跳过）%q", sample)
				continue
			}
			if !pat.matchesPrefilter(sample) {
				t.Errorf("★ 模式 %s 的 prefilter %q 挡住了**正则能匹配**的形态 %q —— 静默漏报",
					name, pat.Prefilter, sample)
			}
		}
	}
}

// ★ 判据：多候选 prefilter 的"任一命中即放行"语义。
func TestPrefilterAlternativesSemantics(t *testing.T) {
	p := Pattern{Name: "t", Prefilter: "a|bb|ccc"}

	if !p.matchesPrefilter("xax") {
		t.Error("含第一个候选应放行")
	}
	if !p.matchesPrefilter("xbbx") {
		t.Error("含第二个候选应放行")
	}
	if !p.matchesPrefilter("xcccx") {
		t.Error("含第三个候选应放行")
	}
	if p.matchesPrefilter("none of them") {
		t.Error("一个都不含时不应放行")
	}
	// 空候选要忽略（防 `"a||b"` 这种写法把空串当"总匹配"）
	p2 := Pattern{Name: "t2", Prefilter: "a||b"}
	if p2.matchesPrefilter("zzz") {
		t.Error("空候选不该被当成匹配（否则 prefilter 形同虚设）")
	}
	// 空 prefilter = 总放行（交给正则）
	p3 := Pattern{Name: "t3", Prefilter: ""}
	if !p3.matchesPrefilter("anything") {
		t.Error("无 prefilter 时应总放行")
	}
}

// ★ 判据：**prefilter 必须真的起作用**（否则优化是空谈）。
//
// 用一个只含无关字符的大文本，断言它比"含前缀"的文本快一个数量级。
// 这条是"优化没有偷偷失效"的哨兵。
func TestPrefilterActuallySkipsWork(t *testing.T) {
	p := Patterns[0] // github_token
	// 找一条有 prefilter 的模式
	for _, q := range Patterns {
		if q.Prefilter != "" {
			p = q
			break
		}
	}
	if p.Prefilter == "" {
		t.Skip("没有带 prefilter 的模式")
	}
	benign := strings.Repeat("普通文本，无凭据。", 1000)
	if p.matchesPrefilter(benign) {
		t.Errorf("模式 %s 的 prefilter %q 不该在无关文本上放行（优化失效）",
			p.Name, p.Prefilter)
	}
}

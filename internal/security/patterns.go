package security

// 敏感信息模式库。
//
// ## 设计原则：**宁可漏，不可误伤**（文档 S4）
//
// 误报比漏报严重得多：它会把正常文本换成别名提示、把任务搞坏，
// 而且现象会出现在很后面（agent 拿到错误的参数），归因极难。
// 所以每条规则都要求**强特征**（固定前缀 + 长度 + 字符集），不用泛化启发式。
//
// ## 覆盖范围
//
// 主流平台的访问令牌 + 常见凭据形态（赋值语句、私钥块、认证头）。

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// Pattern 是一条识别规则。
type Pattern struct {
	Name string
	RE   *regexp.Regexp

	// Prefilter 是**必含的字面量**：不含它就一定不匹配，可直接跳过 RE。
	//
	// ★ 为什么需要（实测的性能缺陷）：本文件所有模式都以 `\b` 开头，
	// 而 Go 的 RE2 对 `\b` 要逐 rune 做 Unicode 词边界判定 ——
	// 实测 50KB 文本上**单条模式 660µs**，9 条合计 ~6ms；
	// 而同样形状但去掉 `\b` 的模式只需 **1.6µs（快 400 倍）**。
	//
	// `\b` 不能删（它防 `xghp_xxx` 这类误伤，见 boundary_need_test.go），
	// 所以改为**先用 O(n) 的字面量包含检查快速排除**：
	// 绝大多数文本根本不含 `ghp_`/`glpat-` 等前缀，连正则都不用跑。
	//
	// 空串表示"没有可用前缀"，那就直接跑正则（这类模式本身很快）。
	//
	// ★ 支持**多个候选**（用 `|` 分隔，任一中即放行）——
	// 因为有些模式的正则本身有多个备选分支（如 assigned_secret 的
	// `[:=]`）。初版只允许单个前缀，于是把 `:` 当 prefilter 后
	// `api_key = "..."` 就被挡在外面 ⇒ **漏报**。
	//
	// 规则：prefilter 的候选集**必须覆盖**正则里所有"必含"的字面量分支。
	// `TestPrefilterCoversAllAlternatives` 逐个形态验证这一点。
	Prefilter string
}

// prefilterAlternatives 把 Prefilter 拆成候选列表。
func (p Pattern) prefilterAlternatives() []string {
	if p.Prefilter == "" {
		return nil
	}
	return strings.Split(p.Prefilter, "|")
}

// matchesPrefilter 报告文本是否**可能**匹配该模式。
//
// 返回 false ⇒ 一定不匹配，可安全跳过正则。
// 返回 true  ⇒ 可能匹配，需跑正则确认。
//
// ★ 保守方向必须是"宁可漏判为可能"：前缀检查只用来**排除**，
// 不能用来**认定** —— 认定仍交给正则（S4：宁可漏，不可误伤）。
func (p Pattern) matchesPrefilter(text string) bool {
	alts := p.prefilterAlternatives()
	if len(alts) == 0 {
		return true // 没有前缀可依，交给正则
	}
	for _, a := range alts {
		if a != "" && strings.Contains(text, a) {
			return true
		}
	}
	return false
}

// Patterns 是内置模式库。
//
// 排序无要求（每条独立替换）。
var Patterns = []Pattern{
	// —— 平台访问令牌（强前缀，几乎不会误伤）——
	// GitHub: ghp_/gho_/ghu_/ghs_/ghr_ + 36 位
	{Name: "github_token", RE: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36}\b`), Prefilter: "gh"},
	// GitHub fine-grained
	{Name: "github_fine_grained", RE: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{60,}\b`), Prefilter: "github_pat_"},
	// GitLab
	{Name: "gitlab_token", RE: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`), Prefilter: "glpat-"},
	// OpenAI
	{Name: "openai_key", RE: regexp.MustCompile(`\bsk-(proj-)?[A-Za-z0-9_-]{20,}\b`), Prefilter: "sk-"},
	// Anthropic
	{Name: "anthropic_key", RE: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`), Prefilter: "sk-ant-"},
	// AWS Access Key ID
	{Name: "aws_access_key", RE: regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), Prefilter: "A"},
	// Slack
	{Name: "slack_token", RE: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), Prefilter: "xox"},
	// 认证头里的长 token
	{Name: "bearer_token", RE: regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-]{30,}`), Prefilter: "earer"},

	// —— 私钥块（多行；单独一条，因为长度阈值不同）——
	{Name: "private_key_block", RE: regexp.MustCompile(
		`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----[\s\S]{64,}?-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`), Prefilter: "-----BEGIN"},

	// —— 赋值语句里的长密钥 ——
	//
	// ⚠️ 这条最容易误伤，所以限制得很死：
	//   · 键名必须是明确的凭据词（api_key / token / password …）
	//   · 值必须有 16+ 位、且是常见的密钥字符集
	// （像 `version = "1.2.3"`、`name = "hello"` 都不会命中）
	// assigned_secret 没有**单一**字面量前缀（它匹配多种键名）。
	//
	// ★ prefilter 必须覆盖正则里的**两种**分隔符（`:` 与 `=`）——
	// 初版只写 `"="`，于是 `password: "..."`（冒号形态）被 prefilter
	// 静默挡住 ⇒ **漏报**。这个 bug 是"盘点什么没被拦"时发现的：
	// 判据样本恰好用了 `=`，所以测试全绿而真实场景有洞。
	//
	// 现在用 `":"` 作 prefilter（冒号在 JSON/YAML/日志里更常见），
	// 并由 TestPrefilterCoversAllAlternatives 逐个分隔符验证。
	{Name: "assigned_secret", Prefilter: ":|=",
		RE: regexp.MustCompile(
			`(?i)\b(api[_-]?key|access[_-]?token|auth[_-]?token|secret[_-]?key|client[_-]?secret|password|passwd)\b\s*[:=]\s*["']?([A-Za-z0-9/+_\-]{16,})["']?`)},
}

// 注意：**刻意不包含**"裸的 40 位 hex"（Gitea token 形状）。
//
// 为什么：40 位 hex 与 git SHA-1（commit id）**形态完全相同**。
// 把 commit hash 换成别名提示会直接把任务搞坏（agent 拿不到正确的 SHA）。
// 这是"宁可漏，不可误伤"的典型取舍：
//   · 漏掉：Gitea token 在裸文本里没被拦（但它通常会出现在赋值语句里，被上一条覆盖）
//   · 误伤：每个 commit id 都变成别名提示 —— 后果严重得多

// ---------------- base64 辅助（集中一处，便于审计） ----------------

func encodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func decodeB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

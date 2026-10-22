package seq

import (
	"fmt"
	"strings"
	"testing"
)

// 阶段 P4：seq_* 工具与插件装配。
//
// 这一层的判据关注**可观测契约**（给模型看的东西对不对），
// 执行语义已由 P1/P2/P3 的判据覆盖。

// ① 六个工具全部注册，且都带可用的 description 与参数 schema。
//
// 工具是**模型可见面**：description 缺失或为空白 ⇒ 模型不知道何时该用它。
func TestAllSixSeqToolsRegistered(t *testing.T) {
	p := newTestPlugin(t)
	want := []string{
		"seq_create", "seq_list", "seq_delete", "seq_run", "seq_call", "seq_when_call",
		"seq_help", // 格式说明与可照抄示例（真机实跑后加：模型踩格式坑各试 1~3 次）
	}
	defs := p.toolDefs()
	for _, name := range want {
		def, ok := defs[name]
		if !ok {
			t.Errorf("工具 %s 未注册（已注册：%v）", name, keysOf(defs))
			continue
		}
		if strings.TrimSpace(def.Description) == "" {
			t.Errorf("工具 %s 的 description 为空 —— 模型无从判断何时使用", name)
		}
		if def.Parameters == nil {
			t.Errorf("工具 %s 缺参数 schema", name)
			continue
		}
		if typ, _ := def.Parameters["type"].(string); typ != "object" {
			t.Errorf("工具 %s 的 schema type = %q，期望 object", name, typ)
		}
	}
	if len(defs) != len(want) {
		t.Errorf("应恰好注册 %d 个工具，实际 %d（%v）—— 多余的导出工具会稀释工具面", len(want), len(defs), keysOf(defs))
	}
}

// ② seq_create 的 schema 必须声明 required，否则模型会漏传。
func TestSeqCreateSchemaDeclaresRequired(t *testing.T) {
	p := newTestPlugin(t)
	def := p.toolDefs()["seq_create"]
	if def.Parameters == nil {
		t.Fatal("seq_create 缺 schema")
	}
	req, _ := def.Parameters["required"].([]string)
	if len(req) == 0 {
		t.Fatalf("seq_create 未声明 required：模型会漏传 name/groups/file")
	}
	got := map[string]bool{}
	for _, r := range req {
		got[r] = true
	}
	if !got["name"] {
		t.Error("required 应包含 name")
	}
}

// ③ seq_create 的 description 必须说明 groups 与 file **二选一**。
//
// 这是最容易让模型犯错的地方：两个都传或都不传该怎么办，必须写清。
func TestSeqCreateExplainsMutuallyExclusiveInput(t *testing.T) {
	p := newTestPlugin(t)
	desc := p.toolDefs()["seq_create"].Description
	for _, want := range []string{"groups", "file"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description 未提到 %s：%s", want, desc)
		}
	}
	if !strings.Contains(desc, "二选一") && !strings.Contains(desc, "或") {
		t.Errorf("description 未说明 groups 与 file 是二选一：%s", desc)
	}
}

// ④ 六个工具都**不得**声明并发安全。
//
// seq_run / seq_call 会执行**一串**工具，其中可能含写操作；把它们标成
// 并发安全，会让内核把两条 seq_run 并发跑起来 ⇒ 两个序列的执行顺序
// 交错、变量表互相污染。
func TestSeqToolsAreNotParallelSafe(t *testing.T) {
	p := newTestPlugin(t)
	for name := range p.toolDefs() {
		def := p.toolDefs()[name]
		if def.ParallelSafe {
			t.Errorf("工具 %s 声明了 ParallelSafe —— seq 会执行一串工具，并发会污染执行序列", name)
		}
	}
}

// ⑤ seq_run 的 description 必须说明"按 groups 数组顺序执行"。
//
// 组的执行顺序是**语义**的一部分：条件依赖前面的槽，顺序反了结果就错。
func TestSeqRunExplainsOrdering(t *testing.T) {
	p := newTestPlugin(t)
	desc := p.toolDefs()["seq_run"].Description
	if !strings.Contains(desc, "顺序") {
		t.Errorf("seq_run 未说明组按顺序执行：%s", desc)
	}
}

// ⑥ 黑名单：序列**内部**不得调用这些工具（与子 agent 的黑名单同源，防递归）。
//
// ⚠️ 这里曾有一个我自己的设计矛盾：判据原先把 `seq_call` / `seq_when_call`
// 也要求进黑名单，但「按名调用 group/序列」恰恰是本包的核心能力——
// 若禁掉它，序列就退化成单层脚本，功能归零。
//
// 分层澄清（这也是正确语义）：
//
//	· `seq_call` / `seq_when_call` 作为**模型直接调用**的入口是正常的
//	  （模型可以单独调某个 group），不算黑名单；
//	· 序列**内部**若写 seq_call，那是按名组合，走 runGroup 的专门分支
//	  并受 maxCallDepth 约束（§8.3 的环检测与深度上界），
//	  不靠黑名单防递归。
//	· 真正要禁的是：对外发消息（output_send__）、起子 agent（spawn_child）、
//	  改插件表（plgreload）、以及再次 seq_run 整条序列（会绕过深度计数的语义）。
func TestSeqBlacklistCoversRecursionRisks(t *testing.T) {
	for _, name := range []string{
		"output_send__qq", "output_send__x", "spawn_child", "plgreload", "seq_run",
	} {
		if !blacklisted(name) {
			t.Errorf("黑名单未覆盖 %q —— 序列能调它就是递归/绕过风险", name)
		}
	}
	// seq_call / seq_when_call 必须**不在**黑名单，否则按名调用能力归零
	for _, name := range []string{"seq_call", "seq_when_call"} {
		if blacklisted(name) {
			t.Errorf("黑名单误伤了 %q —— 它是序列组合的核心能力（递归由 maxCallDepth + 环检测负责）", name)
		}
	}
	// 普通工具不应被误伤
	for _, name := range []string{"cmd_run", "knowledge_search", "output_list_channels"} {
		if blacklisted(name) {
			t.Errorf("黑名单误伤了正常工具 %q", name)
		}
	}
}

func keysOf(m map[string]toolDefInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- 测试替身 ----

// newTestPlugin 造一个**不依赖内核**的 Plugin。
//
// 刻意不走 Start（那需要真实 *sdk.PluginSDK）：本组判据只测
// 「工具定义与可观测契约」，不测执行语义（后者由 exec/store 的判据覆盖）。
func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	return &Plugin{name: "seq", store: NewStore(t.TempDir())}
}

// ⑦ seq_help 必须存在，且**只返回文本**（与仓内 output_send__*_help 同范式）。
//
// 动机来自真机实跑：模型在写序列时踩了三个坑，各试了 1~3 次才改对
//
//	① tools 漏末尾的 ';'      → 「末尾缺少 ';'」
//	② group 的 in 传成字符串   → 重试 3 次
//	③ as 指向未声明的 out 槽    → 静态校验拦下
//
// 这三处的**格式细节**都适合集中在一处可查的地方，而不是散在六个工具
// 描述里（描述有长度限制，细节写不进去）。
func TestSeqHelpRegistered(t *testing.T) {
	p := newTestPlugin(t)
	def, ok := p.toolDefs()["seq_help"]
	if !ok {
		t.Fatalf("seq_help 未注册（已注册：%v）", keysOf(p.toolDefs()))
	}
	if strings.TrimSpace(def.Description) == "" {
		t.Error("seq_help 的 description 为空 —— 模型不知道该什么时候查它")
	}
	if def.ParallelSafe {
		t.Error("seq_help 不该声明并发安全（它是纯查询）")
	}
}

// ⑧ ★ seq_help 的内容必须覆盖真机踩过的**每一个**坑。
//
// 判据从"坑"出发而非从"我打算写什么"出发：下面每一项都对应一次真实失败。
func TestSeqHelpCoversRealPitfalls(t *testing.T) {
	p := newTestPlugin(t)
	out, err := p.dispatch("seq_help", map[string]interface{}{})
	if err != nil {
		t.Fatalf("seq_help 失败: %v", err)
	}
	text, _ := out.(string)
	if strings.TrimSpace(text) == "" {
		t.Fatal("seq_help 返回空")
	}
	need := []struct{ key, want string }{
		{"①tools 是字符串且 ; 结尾", ";"},
		{"②in/out 是对象", "对象"},
		{"③as 必须在 out 声明", "out"},
		{"④groups 与 file 二选一", "file"},
		{"⑤组内并行组间串行", "并行"},
		{"⑥when 条件", "when"},
	}
	for _, n := range need {
		if !strings.Contains(text, n.want) {
			t.Errorf("seq_help 缺少要点「%s」（应含 %q）", n.key, n.want)
		}
	}
}

// ⑨ seq_help 必须给一个**可直接照抄**的完整例子。
//
// 实跑里模型是照着自己理解拼 JSON 的，踩了两次格式坑。
// 一个正确样例比三段描述更有用。
func TestSeqHelpIncludesCopyableExample(t *testing.T) {
	p := newTestPlugin(t)
	out, err := p.dispatch("seq_help", map[string]interface{}{})
	if err != nil {
		t.Fatalf("seq_help 失败: %v", err)
	}
	text, _ := out.(string)
	if !strings.Contains(text, `"groups"`) {
		t.Fatalf("seq_help 未包含示例: %s", truncateForMsg(text, 300))
	}
	// 例子必须能被本包自己的解析器接受 —— 判据直接拿它过一遍 Parse。
	_ = err
	if err := validateHelpExample(text); err != nil {
		t.Errorf("seq_help 里的示例**自己解析不过**（模型照抄必然失败）: %v", err)
	}
}

func truncateForMsg(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// validateHelpExample 从 seq_help 文本里抽出示例并交给**本包自己的解析器**校验。
//
// 为什么要这么判：seq_help 是**给模型照抄的**。如果示例本身解析不过
// （例如 tools 少一个 ';'、in 写成了字符串），模型照抄必然失败 —— 而这类
// bug 从"文本里有没有某个词"是看不出来的。
//
// 做法：从文本里取第一个含 `"groups"` 的 JSON 对象（花括号配平扫描），
// 直接喂给 Parse。
func validateHelpExample(help string) error {
	// ⚠️ 两个坑（都踩过）：
	//  1. 不能用 strings.Index(help, `{"name"`)：帮助文本的「格式要点」里
	//     也有一段 `{"name":…, "groups":[…]}` 示意（有意写的），先命中它
	//     会截到非示例的片段，报出莫名其妙的 invalid character。
	//  2. 基准必须统一。下面全程在**同一个**子串 base 上做偏移，
	//     绝不把 base 的下标拿去切 help。
	const marker = "【可照抄的完整示例】"
	mi := strings.Index(help, marker)
	if mi < 0 {
		return fmt.Errorf("help 里缺少【可照抄的完整示例】小节")
	}
	base := help[mi+len(marker):]

	// 找第一行"整行就是一个 JSON 对象"的内容
	var line string
	for _, l := range strings.Split(base, "\n") {
		t := strings.TrimSpace(l)
		t = strings.Trim(t, "`")
		if strings.HasPrefix(t, `{"name"`) {
			line = t
			break
		}
	}
	if line == "" {
		return fmt.Errorf("示例小节里找不到一整行的 JSON 序列")
	}

	// 在 base 上定位该行，再做括号配平扫描（全程同一基准）
	off := strings.Index(base, line)
	depth := 0
	inStr := false
	esc := false
	for i := off; i < len(base); i++ {
		c := base[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				_, err := Parse([]byte(base[off : i+1]))
				if err != nil {
					return fmt.Errorf("示例解析失败: %w（示例前 120 字：%s）",
						err, truncateForMsg(base[off:min(i+1, off+120)], 120))
				}
				return nil
			}
		}
	}
	return fmt.Errorf("示例 JSON 括号未配平")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ⑩ ★ 真机实跑发现的最高频坑：`in` 传空字符串 ""（而非对象 {}）。
//
// 两轮对照实验（隔离实例，各 5 次与 3 次 seq_create 尝试）里，
// 模型**都在同一处错了两次**：
//
//	① 实验 A：in 传 "" → 连续 2 次失败
//	② 实验 B（先查 seq_help）：in 仍传 "" → 又连续 2 次失败
//
// 模型自述：「我把『无入参』理解成『不传这个字段』，结果序列化成了字符串」。
//
// ⇒ 现有文案虽已可执行（"无入参请写 {}，不要写成字符串"），
// 但**位置太深**：埋在「格式要点」第 2 条里，模型读到了仍会错。
// 本判据要求这条在**前两屏内**且**用醒目措辞**出现。
func TestHelpWarnsEmptyStringInProminently(t *testing.T) {
	p := newTestPlugin(t)
	out, err := p.dispatch("seq_help", map[string]interface{}{})
	if err != nil {
		t.Fatalf("seq_help 失败: %v", err)
	}
	text, _ := out.(string)

	// ① 必须明确说"不要写成字符串"（这是模型实际犯的错）
	if !strings.Contains(text, "不要写成字符串") {
		t.Errorf("seq_help 未警示『in 不要写成字符串』（实跑中模型在此错了 4 次）")
	}
	// ② 这条必须出现在**前 400 字**内 —— 埋太深就会被跳过（实跑证明）
	head := text
	if len(head) > 400 {
		head = head[:400]
	}
	if !strings.Contains(head, "不要写成字符串") {
		t.Errorf("『in 不要写成字符串』未出现在前 400 字内（实跑证明埋太深会被忽略）")
	}
	// ③ 必须给出正确写法，让模型无需推断
	if !strings.Contains(text, "{}") {
		t.Error("未给出正确写法 {}")
	}
}

// ⑪ ★ 同样的坑必须也出现在 seq_create 自己的描述里 ——
// 模型可能不查 help 就直接建序列。
func TestSeqCreateDescWarnsAboutIn(t *testing.T) {
	p := newTestPlugin(t)
	desc := p.toolDefs()["seq_create"].Description
	if !strings.Contains(desc, "in") {
		t.Errorf("seq_create 描述未提及 in：%s", desc)
	}
	// 描述里至少要点明 groups[].in 是对象
	if !strings.Contains(desc, "对象") && !strings.Contains(desc, "{}") {
		t.Errorf("seq_create 描述未说明 groups[].in 应为对象：%s", desc)
	}
}

// Package seq 实现「工具序列」：把可复用的多步工具流程固化为可命名、
// 可复用、可删除的对象。
//
// 边界：本包是**插件**，不是内核。内核只提供并行执行这一项基础设施
// （见 core 的 batchRunnable），序列的全部语义——分组、具名槽、条件、
// 调用图——都在本包内自建，不要求内核开任何新接口。
//
// 能力边界与设计文档 docs/zh/toolcall-contract-and-sequence-design.md §7/§8
// 对应。核心不变量：
//  1. 存的是**解析后的 AST**，执行期不再碰原始文本
//  2. 一切静默降级都视为缺陷：格式错、槽未声明、目标不存在都必须**报错**
package seq

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Sequence 是一条序列（已解析、已校验的 AST）。
type Sequence struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Groups      []Group `json:"groups"`

	// Boot 标记该序列在**系统就绪时自动执行一次**（每次进程启动一次）。
	//
	// 这是序列最核心的用途之一（用户原话："seq 最核心的是定时复用，
	// 比如开机执行一段序列"）。单次任务里编序列往往不划算 —— 先 create
	// 再 run 比直接调工具多两步；真正省事的是**同一套流程反复跑**，
	// 而开机是最典型的周期性触发点。
	//
	// 执行时机：订阅 agent_ready 事件（内核在 kernel assembly → 插件调度
	// → agent.Start() 之后发布），因此执行时工具已全部注册、agent 已能
	// 接受输入。
	//
	// 声明方式：seq_create 传 boot:true，或序列文件里写 "boot": true。
	Boot bool `json:"boot,omitempty"`
}

// Group 是一组工具：**组内并行、组间串行**，且拥有独立签名。
type Group struct {
	// Name 是签名名，全局唯一，可被 seq_call 按名调用。
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// In 是入参声明 {键: 类型}；组内用 $args.<键> 读取。
	In map[string]string `json:"in,omitempty"`
	// Out 是出参声明 {键: 类型}；组内用 as:<键> 写入。
	Out map[string]string `json:"out,omitempty"`
	// When 是条件屏障（默认 true），**只可读 $args.***。
	When string `json:"when,omitempty"`
	// Parallel 为 false 时组内退化为串行（逃生舱）。
	Parallel bool `json:"parallel"`
	// Missing 声明「目标工具不存在」时的行为：fail（默认）/ skip / degrade。
	Missing string `json:"missing,omitempty"`
	// Timeout 是本组墙钟上限（如 "30s"）。
	Timeout string `json:"timeout,omitempty"`
	// OnError: abort（默认）/ continue / retry
	OnError string     `json:"on_error,omitempty"`
	Retries int        `json:"retries,omitempty"`
	Tools   []ToolCall `json:"tools"`
}

// ToolCall 是组内的一个工具调用。
type ToolCall struct {
	Tool string                 `json:"tool"`
	Args map[string]interface{} `json:"args,omitempty"`
	// As 是写入本组 out 具名槽的键名（不含 $ 前缀）。
	As string `json:"as,omitempty"`
	// Fallback 是 missing=degrade 时使用的兜底值（紧凑 JSON 文本）。
	Fallback string `json:"fallback,omitempty"`
}

// missing 取带默认值的缺失策略。
func (g Group) missingPolicy() string {
	if g.Missing == "" {
		return "fail"
	}
	return g.Missing
}

// 合法取值表（错误文案要列出合法值，而不是当默认值蒙过去）。
var (
	validMissing = []string{"fail", "skip", "degrade"}
	validOnError = []string{"abort", "continue", "retry"}
)

// rawGroup 是 JSON 解码的中间形态：tools 是**字符串**。
type rawGroup struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	In          map[string]string `json:"in"`
	Out         map[string]string `json:"out"`
	When        string            `json:"when"`
	Parallel    *bool             `json:"parallel"`
	Missing     string            `json:"missing"`
	Timeout     string            `json:"timeout"`
	OnError     string            `json:"on_error"`
	Retries     int               `json:"retries"`
	Tools       string            `json:"tools"`
}

type rawSeq struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Groups      []rawGroup `json:"groups"`
	// Boot 必须在这里声明，否则 Parse 的 DisallowUnknownFields 会把
	// "boot": true 当成**拼写错误**拒绝 —— 开机序列根本存不进去。
	// （这正是 DisallowUnknownFields 的价值：漏声明字段会立刻报错，
	//   而不是静默忽略后让人纳闷"为什么开机不执行"。）
	Boot bool `json:"boot"`
}

// Parse 把序列文本解析为 AST 并完成**全部静态校验**。
//
// 校验在此处一次做完（而不是留到执行期），因为 group 拥有独立签名：
// 具名槽、写错的目标、同名 as 都能**构建期**发现——这正是具名槽相对
// 自动编号（$0/$1）的全部价值。
func Parse(data []byte) (*Sequence, error) {
	// DisallowUnknownFields：拼错 parallel 必须是**报错**，不能静默取默认。
	// 参照 internal/plugin/manifest.go 记的教训（该仓无此选项，字段被静默丢弃）。
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var raw rawSeq
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("序列 JSON 解析失败: %w", friendlyJSONError(err))
	}

	if strings.TrimSpace(raw.Name) == "" {
		return nil, fmt.Errorf("序列缺少 name")
	}
	if len(raw.Groups) == 0 {
		return nil, fmt.Errorf("序列 %q 没有任何 group", raw.Name)
	}

	seq := &Sequence{Name: raw.Name, Description: raw.Description, Boot: raw.Boot}
	seenGroup := map[string]bool{}
	for gi, rg := range raw.Groups {
		g, err := buildGroup(rg)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个 group: %w", gi+1, err)
		}
		if seenGroup[g.Name] {
			return nil, fmt.Errorf("group 名 %q 重复（它是签名名，必须唯一才能按名调用）", g.Name)
		}
		seenGroup[g.Name] = true
		seq.Groups = append(seq.Groups, g)
	}
	return seq, nil
}

// buildGroup 解析并校验单个 group。
func buildGroup(rg rawGroup) (Group, error) {
	if strings.TrimSpace(rg.Name) == "" {
		return Group{}, fmt.Errorf("缺少 group 名")
	}
	g := Group{
		Name:        rg.Name,
		Description: rg.Description,
		In:          rg.In,
		Out:         rg.Out,
		When:        rg.When,
		Parallel:    true, // 默认并行
		Missing:     rg.Missing,
		Timeout:     rg.Timeout,
		OnError:     rg.OnError,
		Retries:     rg.Retries,
	}
	if rg.Parallel != nil {
		g.Parallel = *rg.Parallel
	}
	if g.When == "" {
		g.When = "true"
	}
	if g.In == nil {
		g.In = map[string]string{}
	}
	if g.Out == nil {
		g.Out = map[string]string{}
	}
	if g.Missing != "" && !containsStr(validMissing, g.Missing) {
		return Group{}, fmt.Errorf("group %q 的 missing=%q 非法，合法取值：%s",
			g.Name, g.Missing, strings.Join(validMissing, "/"))
	}
	if g.OnError != "" && !containsStr(validOnError, g.OnError) {
		return Group{}, fmt.Errorf("group %q 的 on_error=%q 非法，合法取值：%s",
			g.Name, g.OnError, strings.Join(validOnError, "/"))
	}

	tools, err := splitToolList(rg.Tools)
	if err != nil {
		return Group{}, fmt.Errorf("group %q 的 tools: %w", g.Name, err)
	}
	if len(tools) == 0 {
		return Group{}, fmt.Errorf("group %q 没有任何工具", g.Name)
	}

	// 槽校验 + 组内 $args 引用校验
	asCount := map[string]int{}
	for ti, t := range tools {
		if strings.TrimSpace(t.Tool) == "" {
			return Group{}, fmt.Errorf("group %q 第 %d 个工具缺少 tool 字段", g.Name, ti+1)
		}
		if t.As != "" {
			if _, ok := g.Out[t.As]; !ok {
				return Group{}, fmt.Errorf(
					"group %q 第 %d 个工具的 as=%q 未在 out 中声明（out 现有：%s）",
					g.Name, ti+1, t.As, keyList(g.Out))
			}
			asCount[t.As]++
		}
		// args 里只能引用已声明的入参
		for k, v := range t.Args {
			for _, ref := range argRefs(v) {
				if _, ok := g.In[ref]; !ok {
					return Group{}, fmt.Errorf(
						"group %q 第 %d 个工具的 args.%s 引用了未声明的入参 $args.%s（in 现有：%s）",
						g.Name, ti+1, k, ref, keyList(g.In))
				}
			}
		}
	}
	// 非 array 槽被同名 as 写多次 ⇒ 组内并发时数据竞争
	for slot, n := range asCount {
		if n > 1 && !isArrayType(g.Out[slot]) {
			return Group{}, fmt.Errorf(
				"group %q 的 out 槽 %q（类型 %s）被 as 写了 %d 次；组内并行下同名写入是数据竞争。"+
					"若要累加请把该槽声明为 array",
				g.Name, slot, g.Out[slot], n)
		}
	}
	g.Tools = tools
	return g, nil
}

// splitToolList 把 `;` 分隔的 tools 字符串解析为若干工具调用。
//
// ⚠️ `;` **仅在 brace/bracket 深度为 0 且不在字符串内**时才是分隔符。
// 这一点是必需的、不是装饰：线上实测模型写出的 command 参数里**大量**含分号
// （见 core/stream_accumulate_test.go 里取自日志原文的 fixture），裸切分会把
// 一条命令切成六个工具。被 `{…}` 包裹后，命令里的分号在字符串内 ⇒ 天然无歧义。
func splitToolList(s string) ([]ToolCall, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("没有工具——需要至少一个 `{\"tool\":\"...\",\"args\":{...}} ;` 形式的工具")
	}
	var pieces []string
	depth := 0
	inStr := false
	esc := false
	start := 0

	flush := func(end int) error {
		p := strings.TrimSpace(s[start:end])
		if p == "" {
			return fmt.Errorf("位置 %d：空的工具（连续或多余的分号）", end)
		}
		pieces = append(pieces, p)
		return nil
	}

	for i, r := range s {
		switch {
		case esc:
			esc = false
		case r == '\\' && inStr:
			esc = true
		case r == '"':
			inStr = !inStr
		case inStr:
			// 字符串内不参与深度计算
		case r == '{' || r == '[':
			depth++
		case r == '}' || r == ']':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("位置 %d：多余的 %q", i, r)
			}
			if depth == 0 {
				// 回到顶层：其后必须紧跟 ';'，否则下一个工具会被**静默吞掉**
				nxt := strings.TrimSpace(s[i+1:])
				if nxt != "" && !strings.HasPrefix(nxt, ";") {
					return nil, fmt.Errorf(
						"位置 %d：`{…}` 之后缺少 ';' 分隔符（不留分隔符会让下一个工具被静默吞掉）", i+1)
				}
			}
		case r == ';' && depth == 0:
			if err := flush(i); err != nil {
				return nil, err
			}
			start = i + 1
		}
	}
	if inStr {
		return nil, fmt.Errorf("字符串未闭合（引号不成对）")
	}
	if depth != 0 {
		return nil, fmt.Errorf("结构未闭合（括号深度 %d）", depth)
	}
	if tail := strings.TrimSpace(s[start:]); tail != "" {
		return nil, fmt.Errorf("末尾缺少 ';'：最后一个工具未被分隔")
	}
	if len(pieces) == 0 {
		return nil, fmt.Errorf("没有任何工具")
	}

	out := make([]ToolCall, 0, len(pieces))
	for i, p := range pieces {
		var tc ToolCall
		d := json.NewDecoder(strings.NewReader(p))
		d.DisallowUnknownFields()
		if err := d.Decode(&tc); err != nil {
			return nil, fmt.Errorf("第 %d 个工具解析失败: %w（内容：%s）", i+1, err, trunc(p, 120))
		}
		if strings.TrimSpace(tc.Tool) == "" {
			return nil, fmt.Errorf("第 %d 个工具缺少 tool 字段", i+1)
		}
		out = append(out, tc)
	}
	return out, nil
}

// argRefs 收集一个 args 值里出现的所有 $args.<键> 引用。
// 深度遍历（args 本身可能是嵌套对象/数组）。
func argRefs(v interface{}) []string {
	var out []string
	var walk func(interface{})
	walk = func(x interface{}) {
		switch t := x.(type) {
		case string:
			out = append(out, parseArgRefs(t)...)
		case map[string]interface{}:
			for _, vv := range t {
				walk(vv)
			}
		case []interface{}:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(v)
	return out
}

// parseArgRefs 从一段文本里取出全部 $args.<键>。
func parseArgRefs(s string) []string {
	const marker = "$args."
	var out []string
	rest := s
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			return out
		}
		rest = rest[i+len(marker):]
		end := 0
		for end < len(rest) && isRefChar(rest[end]) {
			end++
		}
		if end == 0 {
			// 形如 "$args." 后面没有键名 —— 不是合法引用，跳过避免死循环
			continue
		}
		out = append(out, rest[:end])
		rest = rest[end:]
	}
}

func isRefChar(b byte) bool {
	return b == '_' || b == '-' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func isArrayType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return t == "array" || strings.HasPrefix(t, "array<") || strings.HasPrefix(t, "[]")
}

func keyList(m map[string]string) string {
	if len(m) == 0 {
		return "（无）"
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// friendlyJSONError 把 encoding/json 的原始报错翻译成**模型可执行**的话。
//
// 为什么必须翻译（真机实跑证据）：模型把 group 的 `in` 传成字符串 "{}"
// 时，原始报错是
//
//	json: cannot unmarshal string into Go struct field rawSeq.groups.0.in
//	      of type map[string]string
//
// 这句话说的是"事实"（string 解不成 map），不是"该怎么做"
// （in 应该写成对象 {"键":"类型"}）。模型为此重试了 **3 次**才改对。
// 残留的 `rawSeq` / `Go struct field` 更是 Go 内部实现细节，
// 对模型无意义且会误导它去猜一个叫 rawSeq 的东西。
//
// 这与本仓反复吃亏的那类问题同源：`20s` 少引号 → 静默降级 →
// cmd_run 失败率 34%。**报事实不报改法，模型只能猜。**
func friendlyJSONError(err error) error {
	// ① 未知字段：拼写错误最常见，且必须显式（DisallowUnknownFields 已启用）
	if strings.Contains(err.Error(), "unknown field") {
		return fmt.Errorf("%w（注意字段名拼写；每个 group 允许的字段为 "+
			"name/description/in/out/when/parallel/missing/timeout/on_error/retries/tools）", err)
	}

	// ② 类型不匹配：给出该字段**应该**是什么
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		field := shortFieldName(te.Field)
		switch field {
		case "in", "out":
			return fmt.Errorf("group 的 %s 应写成**对象**，形如 {\"键\":\"类型\"}"+
				"（键=参数名，值=类型如 string/bool/integer/array）；"+
				"收到的是 %s —— 无入参请写 {}，不要写成字符串", field, jsonKind(te.Value))
		case "tools":
			return fmt.Errorf("group 的 tools 应写成**字符串**（内容是若干以 ';' 分隔的 JSON 对象），"+
				"而不是数组；例如 \"{\\\"tool\\\":\\\"cmd_run\\\",\\\"args\\\":{},\\\"as\\\":\\\"x\\\"} ;\"；"+
				"收到的是 %s", jsonKind(te.Value))
		case "groups":
			return fmt.Errorf("groups 应是数组，形如 [ {…}, {…} ]；收到的是 %s", jsonKind(te.Value))
		case "name", "description", "when", "missing", "timeout", "on_error":
			return fmt.Errorf("%s 应写成字符串；收到的是 %s", field, jsonKind(te.Value))
		default:
			return fmt.Errorf("%s 的类型不对（应为 %s，收到 %s）",
				field, goTypeName(te.Type), jsonKind(te.Value))
		}
	}
	return err
}

// shortFieldName 把 "rawSeq.groups.0.in" 缩成 "in"。
//
// 剥掉 Go 内部类型名（rawSeq）：那是本包的实现细节，模型无从得知，
// 照抄反而会去猜"rawSeq 是什么"。
func shortFieldName(f string) string {
	if f == "" {
		return "某个字段"
	}
	if i := strings.LastIndex(f, "."); i >= 0 {
		f = f[i+1:]
	}
	// 去掉数组下标：groups.0.in → in（上面已取最后一段，兜底再剥一次）
	f = strings.TrimSuffix(f, "]")
	return f
}

// jsonKind 描述**收到的** JSON 值长什么样（用模型看得懂的说法）。
func jsonKind(v string) string {
	switch v {
	case "string":
		return "字符串"
	case "array":
		return "数组"
	case "object":
		return "对象"
	case "number":
		return "数字"
	case "bool":
		return "布尔值"
	}
	return v
}

// goTypeName 去掉包名前缀（map[string]string → map）。
func goTypeName(t reflect.Type) string {
	if t == nil {
		return "未知"
	}
	if t.Kind() == reflect.Map {
		return "对象"
	}
	if t.Kind() == reflect.Slice {
		return "数组"
	}
	s := t.String()
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return s
}

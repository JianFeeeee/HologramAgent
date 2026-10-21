package distill

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/pkg/generation"
)

// fakeGen 返回固定文本的假 provider，用于把「模型行为」与「本包逻辑」分开测。
type fakeGen struct {
	text      string
	truncated bool
	schema    string
	err       error
	info      generation.Info
	// sawPrompt/sawSchema 记录请求，用于断言提示词与 schema 真的传出去了。
	sawPrompt string
	sawSchema string
	sawTemp   float64
}

func (f *fakeGen) Generate(_ context.Context, r generation.Request) (generation.Response, error) {
	f.sawPrompt = r.Prompt
	f.sawSchema = r.JSONSchema
	f.sawTemp = r.Temperature
	if f.err != nil {
		return generation.Response{}, f.err
	}
	return generation.Response{Text: f.text, Truncated: f.truncated}, nil
}

func (f *fakeGen) Info() generation.Info {
	if f.info.Model == "" {
		return generation.Info{Model: "fake", SupportsJSONSchema: true}
	}
	return f.info
}

func (f *fakeGen) Close() {}

const rec = "第183批 告警规则9条·值班手册第4版·容量预警70%·排期10月"

// ★ 正常路径：字段 → 三元组，主语锚定，值原样。
func TestSplit_字段转三元组(t *testing.T) {
	g := &fakeGen{text: `{"fields":[
		{"name":"告警规则数","value":"9条"},
		{"name":"值班手册版本","value":"第4版"},
		{"name":"容量预警","value":"70%"}]}`}
	e := NewExtractor(g, nil)

	tr, err := e.Split(context.Background(), rec)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(tr) != 3 {
		t.Fatalf("期望 3 条三元组，实际 %d：%+v", len(tr), tr)
	}
	for _, x := range tr {
		if x.Subject != "第183批" {
			t.Errorf("主语应为 第183批，实际 %q", x.Subject)
		}
		if !strings.Contains(rec, x.Object) {
			t.Errorf("值 %q 不在原文里 —— 闸门失效", x.Object)
		}
		if x.SentenceText != rec {
			t.Errorf("应保留原句以便回到原文")
		}
	}
	// 提示词与 schema 真的传出去了（这是小模型能用的唯一机制）
	if !strings.Contains(g.sawSchema, `"fields"`) {
		t.Errorf("JSONSchema 未传出，字段约束丢失：%q", g.sawSchema)
	}
	if !strings.Contains(g.sawPrompt, rec) {
		t.Errorf("记录未进入提示词")
	}
	if g.sawTemp != 0 {
		t.Errorf("温度应为 0（拆分要可复现），实际 %v", g.sawTemp)
	}
}

// ★ 闸门 1：值不在原文 → 丢弃。这是唯一的幻觉防线。
func TestSplit_值不在原文则丢弃(t *testing.T) {
	g := &fakeGen{text: `{"fields":[
		{"name":"告警规则数","value":"9条"},
		{"name":"容量预警","value":"999%"},
		{"name":"排期","value":"12月"}]}`}
	tr, err := NewExtractor(g, nil).Split(context.Background(), rec)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(tr) != 1 || tr[0].Object != "9条" {
		t.Fatalf("只应保留原样值 9条，实际 %+v", tr)
	}
}

// ★ 闸门 2：维度名归一。不归一就建不了边。
func TestSplit_维度名归一(t *testing.T) {
	g := &fakeGen{text: `{"fields":[
		{"name":"停机","value":"7分"},
		{"name":"停机时间","value":"9分"},
		{"name":"停机时长","value":"4分"}]}`}
	tr, err := NewExtractor(g, nil).Split(context.Background(), "第200批 停机7分")
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	rels := map[string]bool{}
	for _, x := range tr {
		rels[x.Relation] = true
	}
	if !rels["停机时长"] {
		t.Fatalf("三种同义写法应归一为「停机时长」，实际 %v", rels)
	}
}

// ★ 闸门 3：无主语 → 不写脏三元组。
func TestSplit_无主语不产出(t *testing.T) {
	g := &fakeGen{text: `{"fields":[{"name":"容量预警","value":"70%"}]}`}
	tr, err := NewExtractor(g, nil).Split(context.Background(), "就那么回事")
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(tr) != 0 {
		t.Fatalf("无批次号作主语时不应产出三元组（宁可少记不写脏），实际 %+v", tr)
	}
}

// 「没有可拆字段」是正常结果，不是错误 —— 调用方要能区分它与「模型没跑」。
func TestSplit_空字段不是错误(t *testing.T) {
	g := &fakeGen{text: `{"fields":[]}`}
	tr, err := NewExtractor(g, nil).Split(context.Background(), "第129~143批均仅评审通过")
	if err != nil {
		t.Fatalf("空字段不应报错：%v", err)
	}
	if len(tr) != 0 {
		t.Fatalf("期望 0 条，实际 %d", len(tr))
	}
}

// provider 不支持 schema 时必须显式失败，不能静默降级成自由文本。
func TestSplit_不支持schema显式失败(t *testing.T) {
	g := &fakeGen{
		text: `{"fields":[]}`,
		info: generation.Info{Model: "no-schema", SupportsJSONSchema: false},
	}
	_, err := NewExtractor(g, nil).Split(context.Background(), rec)
	if err == nil || !errors.Is(err, generation.ErrSchemaUnsupported) {
		t.Fatalf("应报 ErrSchemaUnsupported，实际 %v", err)
	}
}

// 模型调用失败 → 报错（调用方据此回退 jieba 路径或重试）。
func TestSplit_模型失败透传错误(t *testing.T) {
	g := &fakeGen{err: errors.New("connection refused")}
	_, err := NewExtractor(g, nil).Split(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("应透传模型错误，实际 %v", err)
	}
}

// 非法 JSON → 报错；截断要标出来（调用方可选择重试而非丢弃）。
func TestSplit_非法JSON与截断(t *testing.T) {
	g := &fakeGen{text: `{"fields":[{"name":"a"`}
	_, err := NewExtractor(g, nil).Split(context.Background(), rec)
	if err == nil {
		t.Fatal("非法 JSON 应报错")
	}

	g2 := &fakeGen{text: `{"fields":[{"name":"a","value":"1"}`, truncated: true}
	_, err2 := NewExtractor(g2, nil).Split(context.Background(), rec)
	if err2 == nil || !strings.Contains(err2.Error(), "truncated") {
		t.Fatalf("截断应标明（便于调用方重试），实际 %v", err2)
	}
}

// 字段名超长（模型把整句当字段名）→ 丢弃。
func TestSplit_字段名超长丢弃(t *testing.T) {
	long := "第183批周三凌晨1点·停机4分·回滚v2.29.5·灰度10%观察48分后放50%"
	g := &fakeGen{text: `{"fields":[{"name":"` + long + `","value":"4分"}]}`}
	tr, err := NewExtractor(g, nil).Split(context.Background(), rec)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(tr) != 0 {
		t.Fatalf("超长字段名应丢弃，实际 %+v", tr)
	}
}

// defaultSubject 覆盖多种批次形态。
func TestDefaultSubject(t *testing.T) {
	cases := map[string]string{
		"第183批 告警规则9条":       "第183批",
		"第128/130/131批同为33日": "第128/130/131批",
		"第129~143批共十五个版本":    "第129~143批",
		"没有批次号的纯句子":          "",
		"":                   "",
	}
	for in, want := range cases {
		if got := defaultSubject(in); got != want {
			t.Errorf("defaultSubject(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// 归一表里每个别名都要真的归一到规范名（防止有人加词时写错方向）。
func TestNormalizeDimension_别名表方向正确(t *testing.T) {
	for alias, canonical := range dimensionAliases {
		if got := NormalizeDimension(alias); got != canonical {
			t.Errorf("NormalizeDimension(%q)=%q，期望 %q", alias, got, canonical)
		}
		// 规范名自身也必须稳定（幂等）
		if got := NormalizeDimension(canonical); got != canonical {
			t.Errorf("规范名 %q 不幂等：NormalizeDimension=%q", canonical, got)
		}
	}
}

// ★ 归一的**包含式路径**：模型输出带后缀的字段名（「停机时长(分钟)」）。
// 上一版测试只覆盖精确匹配，去掉精确分支后仍能通过 —— 包含式分支当时被
// `_ = canonical` 死代码掩盖着。这条测试专门盯它。
func TestNormalizeDimension_包含式最长匹配(t *testing.T) {
	cases := map[string]string{
		"停机时长(分钟)":    "停机时长",
		"值班手册第4版":     "值班手册版本",
		"回滚版本v2.29.5": "回滚版本",
		"未知维度XYZ":     "未知维度XYZ", // 不命中 → 原样返回，不丢弃
	}
	for in, want := range cases {
		if got := NormalizeDimension(in); got != want {
			t.Errorf("NormalizeDimension(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// ★ 自环过滤：字段名或值等于主语时丢弃。
//
// 起因（实测）：提示词里写「批次号是主语，不要单独列成字段」这条**负向指令**
// 会让 qwen3:1.7b 把所有字段都当成批次号相关而丢弃（4 字段 → 0 字段）。
// 去掉该指令后它改为主动拆出「批次号」字段，于是产生
// (第183批) -[批次号]-> 第183批 这种自环。过滤必须放在 Go 侧，
// 不能指望模型听懂否定指令。
func TestSplit_过滤主语自环(t *testing.T) {
	g := &fakeGen{text: `{"fields":[
		{"name":"批次号","value":"第183批"},
		{"name":"批号","value":"第183批"},
		{"name":"容量预警","value":"70%"}]}`}
	tr, err := NewExtractor(g, nil).Split(context.Background(), rec)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	for _, x := range tr {
		if x.Subject == x.Object {
			t.Errorf("自环未过滤: %+v", x)
		}
		if x.Relation == "第183批" || x.Relation == "批次号" || x.Relation == "批号" {
			t.Errorf("主语被当成字段名: %+v", x)
		}
	}
	if len(tr) != 1 || tr[0].Object != "70%" {
		t.Fatalf("只应保留非自环字段，实际 %+v", tr)
	}
}

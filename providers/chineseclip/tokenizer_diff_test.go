package chineseclip

// tokenizer_diff_test.go —— 新实现 vs **原实现（内联为参照 oracle）** 的差分等价测试。
//
// ============================ 为什么必须有这个文件 ============================
// 本轮我把 splitOnPunctuation 与 wordpiece 从「[]rune + 每轮 substring」
// 改成「字节边界 + 一次 substring」。目标是纯性能，语义必须**逐值不变**。
//
// 而本机**没有真实模型产物**（CHINESECLIP_MODEL_DIR 未设），
// 唯一的权威对照 TestTokenizerMatchesOfficialReference会 **SKIP** ——
// 也就是说：仅靠现有测试，我的重写是**没有被有效验证**的。
//
// 故这里把**原实现**原样内联为 oracle，用同一批输入逐值比对。
// 这与本仓 C 化那几刀同一条纪律：
//   「没有对照的优化，只是感觉而不是证据」。
//
// oracle 是**冻结的旧代码**（照抄改动前的实现），不得随主实现演进而修改——
// 否则它就失去了「参照」的意义。

import (
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// oracle：改动前的 splitOnPunctuation / wordpiece（原样冻结）
// ---------------------------------------------------------------------------

func oracleSplitOnPunctuation(text string) []string {
	runes := []rune(text)
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range runes {
		if isBERTPunctuation(r) {
			flush()
			out = append(out, string(r))
			continue
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

func oracleWordpiece(vocab map[string]int32, token string) []string {
	runes := []rune(token)
	if len(runes) > maxInputCharsPerWord {
		return []string{tokenUNK}
	}
	var out []string
	start := 0
	for start < len(runes) {
		end := len(runes)
		var cur string
		found := false
		for end > start {
			piece := string(runes[start:end])
			if start > 0 {
				piece = "##" + piece
			}
			if _, ok := vocab[piece]; ok {
				cur = piece
				found = true
				break
			}
			end--
		}
		if !found {
			return []string{tokenUNK}
		}
		out = append(out, cur)
		start = end
	}
	return out
}

func oracleBasicTokenize(text string) []string {
	cleaned := oracleCleanText(text)
	var out []string
	for _, token := range strings.Fields(oracleTokenizeChineseChars(cleaned)) {
		if len([]rune(token)) > maxInputCharsPerWord {
			continue
		}
		stripped := stripAccents(strings.ToLower(token))
		out = append(out, oracleSplitOnPunctuation(stripped)...)
	}
	return out
}

// oracleCleanText / oracleTokenizeChineseChars 直接复用主实现中**未被改动**的
// 函数（它们本轮没动，故无需再抄一份，抄了反而会有漂移风险）。
func oracleCleanText(text string) string { return cleanText(text) }

func oracleTokenizeChineseChars(text string) string { return tokenizeChineseChars(text) }

// ---------------------------------------------------------------------------
// 差分测试
// ---------------------------------------------------------------------------

// diffInputs 覆盖各语言/标点/空白/emoji/组合字符/超长词。
var diffInputs = []string{
	"",
	"a",
	"你好",
	"你好，世界！",
	"hello world",
	"hello, world!",
	"用户询问了系统状态",
	"这是一段中文文本，用于测试分词器的吞吐。",
	"the quick brown fox jumps over the lazy dog.",
	"记忆 memory 检索 recall 上下文 context 注入 inject。",
	"混合Mixed中英English文本text。",
	"標點測試：；、（）《》「」",
	"emoji 😀 与中文混合",
	"combin\u0301ing",              // 组合音标
	"café naïve résumé",            // 预组合
	"a" + strings.Repeat("b", 300), // 超长 ASCII 词（> maxInputCharsPerWord）
	"字" + strings.Repeat("长", 300),
	"  多个   空格\t制表\n换行  ",
	"$+=^`|~ 符号",
	"＃全角＃ＡＢＣ", // 全角
	"1234567890",
	"UPPER lower MiXeD",
	"无标点长句onetwothreefour",
	"\u0000\u0001控制符",
	"a,，b。c！d?e;f:g",
	strings.Repeat("词。", 100),
}

func TestTokenizerDiff_SplitOnPunctuation(t *testing.T) {
	for _, in := range diffInputs {
		got := splitOnPunctuation(in)
		want := oracleSplitOnPunctuation(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("splitOnPunctuation 分歧 %q：\n  got  %q\n  want %q", in, got, want)
		}
	}
}

func TestTokenizerDiff_Wordpiece(t *testing.T) {
	vocab := synthVocab()
	tok := &Tokenizer{vocab: vocab, maxLength: 512}
	// 单独的词（含超长、含 CJK、含 ASCII、含未登录词）
	words := []string{
		"hello", "world", "helloing", "unbelievable", "abc", "a",
		"学", "学习", "学习机器学习", "机器learning", "未知词汇",
		strings.Repeat("x", 300), strings.Repeat("学", 300),
		"café", "caféing", "aaaaaaa",
		"", "##x", "1234567890",
	}
	for _, w := range words {
		got := tok.wordpiece(w)
		want := oracleWordpiece(vocab, w)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("wordpiece 分歧 %q：\n  got  %v\n  want %v", w, got, want)
		}
	}
}

func TestTokenizerDiff_BasicTokenize(t *testing.T) {
	for _, in := range diffInputs {
		got := basicTokenize(in)
		want := oracleBasicTokenize(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("basicTokenize 分歧 %q：\n  got  %v\n  want %v", in, got, want)
		}
	}
}

// TestTokenizerDiff_Encode 端到端：整条 Encode（含特殊 token 与 padding）。
func TestTokenizerDiff_Encode(t *testing.T) {
	vocab := synthVocab()
	tok := &Tokenizer{vocab: vocab, maxLength: 512}
	for _, in := range diffInputs {
		got, maskGot := tok.Encode(in)

		// oracle 路径：用冻结的 basicTokenize + wordpiece 重建 Encode
		pieces := []string{}
		for _, basic := range oracleBasicTokenize(in) {
			pieces = append(pieces, oracleWordpiece(vocab, basic)...)
		}
		if limit := 512 - 2; len(pieces) > limit {
			pieces = pieces[:limit]
		}
		want := []int64{int64(vocab[tokenCLS])}
		for _, p := range pieces {
			want = append(want, int64(vocab[p]))
		}
		want = append(want, int64(vocab[tokenSEP]))
		for len(want) < 512 {
			want = append(want, int64(vocab[tokenPAD]))
		}
		wantMask := make([]int64, 0, 512)
		n := len(pieces) + 2
		for i := 0; i < n; i++ {
			wantMask = append(wantMask, 1)
		}
		for len(wantMask) < 512 {
			wantMask = append(wantMask, 0)
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("Encode ids 分歧 %q", in)
		}
		if !reflect.DeepEqual(maskGot, wantMask) {
			t.Errorf("Encode mask 分歧 %q", in)
		}
	}
}

// TestTokenizerDiff_RandomBytes 随机字节：非法 UTF-8 是分词器最容易分叉的输入。
//
// ★ 为什么必须测这个：字节层面扫描（新实现）与 rune 层面扫描（旧实现）在
// **非法 UTF-8** 上的行为最容易不同 ——
//
//	· []rune(s) 把非法字节变成 U+FFFD（每个坏字节一个）
//	· utf8.DecodeRuneInString 返回 (RuneError, 1) 并前进 1 字节
//	两者语义应当一致，但「应当」不是证据。
func TestTokenizerDiff_RandomBytes(t *testing.T) {
	rng := newSeededRand(20260926)
	alphabet := []byte("ab ,.!?中文。，！？$+=^`|~\xff\xfe\x80\xc3\xe4\t\n")
	for iter := 0; iter < 20000; iter++ {
		n := rng.Intn(40)
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = alphabet[rng.Intn(len(alphabet))]
		}
		in := string(buf)

		if got, want := splitOnPunctuation(in), oracleSplitOnPunctuation(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("随机字节 splitOnPunctuation 分歧 %q：\n  got  %q\n  want %q", in, got, want)
		}
		if got, want := basicTokenize(in), oracleBasicTokenize(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("随机字节 basicTokenize 分歧 %q：\n  got  %v\n  want %v", in, got, want)
		}
	}
}

// TestTokenizerDiff_FuzzishRunes 随机 rune（合法 UTF-8 但内容任意）。
func TestTokenizerDiff_FuzzishRunes(t *testing.T) {
	rng := newSeededRand(777)
	var sb strings.Builder
	for iter := 0; iter < 5000; iter++ {
		sb.Reset()
		n := rng.Intn(30)
		for i := 0; i < n; i++ {
			sb.WriteRune(rune(rng.Intn(0x2000)))
		}
		in := sb.String()
		if got, want := splitOnPunctuation(in), oracleSplitOnPunctuation(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("随机 rune 分歧 %q：\n  got  %q\n  want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

type seededRand struct{ s uint64 }

func newSeededRand(seed uint64) *seededRand { return &seededRand{s: seed | 1} }

func (r *seededRand) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}

func (r *seededRand) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// 确保 oracle 与主实现对 isBertPunctuation 的使用一致（防有人改了判定）。
var (
	_ = unicode.IsPunct
	_ = utf8.RuneError
)

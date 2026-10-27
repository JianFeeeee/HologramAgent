package chineseclip

// tokenizer_bench_test.go —— 分词器热路径基准（判定 C 化是否值得）。
//
// ============================ 为什么不依赖真实模型 ============================
// LoadTokenizer 只需要 vocab.txt + maxLength，不需要 ONNX 产物；
// 本基准用**合成词表**（结构与真实词表同形：含 ## 续接前缀与 CJK 字符），
// 于是无需 CHINESECLIP_MODEL_DIR 即可在任意机器复现。
//
// ★ 同时**不假设「C 更快」**，而是先测出成本分布：
//   分词流水线有 5 段（cleanText / tokenizeChineseChars / splitOnPunctuation
//   / stripAccents+ToLower / wordpiece），哪一段占大头要**测**出来。
//   三刀教训：C 化的收益判据必须先有数据支撑。

import (
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// synthVocab 造一个与 BERT 中文词表同形的词表。
// 结构还原要点：单字 + ## 续接 + 少量多字词 + 4 个特殊 token。
func synthVocab() map[string]int32 {
	v := make(map[string]int32, 32768)
	add := func(p string) {
		if _, ok := v[p]; !ok {
			v[p] = int32(len(v))
		}
	}
	for _, s := range []string{tokenCLS, tokenSEP, tokenPAD, tokenUNK} {
		add(s)
	}
	// ASCII 词与 ## 续接
	words := []string{"hello", "world", "user", "query", "memory", "agent",
		"ing", "er", "ed", "s", "ly", "tion", "##ing", "##er", "##ed"}
	for _, w := range words {
		add(w)
	}
	// 常用单字（含中英）
	singles := []string{"的", "了", "是", "在", "我", "你", "他", "们", "这", "那",
		"a", "b", "c", "x", "y", "z", "0", "1", "2"}
	for _, s := range singles {
		add(s)
	}
	// 双字词（让 wordpiece 有机会一次命中）
	for i := 0; i < 512; i++ {
		add(string(rune('A'+i%26)) + string(rune('a'+i/26)))
	}
	return v
}

func benchTok(b *testing.B) *Tokenizer {
	b.Helper()
	return &Tokenizer{vocab: synthVocab(), maxLength: 512}
}

// benchInputs 覆盖真实分布：短查询 / 中文长句 / 英文长文 / 混合 / 超长。
var benchInputs = map[string]string{
	"short_zh":    "用户询问了系统状态",
	"short_en":    "what is the system status",
	"mid_zh":      strings.Repeat("这是一段中文文本，用于测试分词器的吞吐。", 10),
	"mid_en":      strings.Repeat("the quick brown fox jumps over the lazy dog. ", 10),
	"mixed":       strings.Repeat("记忆 memory 检索 recall 上下文 context 注入 inject。", 8),
	"long_zh":     strings.Repeat("长文本。", 200),
	"punct_heavy": strings.Repeat("你好，世界！这是一个测试。", 20),
}

// BenchmarkTokenizerEncode 整体分词（Encode 全流程）。
func BenchmarkTokenizerEncode(b *testing.B) {
	tok := benchTok(b)
	for name, in := range benchInputs {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tok.Encode(in)
			}
		})
	}
}

// BenchmarkTokenizerStages 分解流水线各段，用于定位真正的热点。
//
// ★ 判据：若 wordpiece 占比远高于其它段，则「O(n²) 字符串分配」是主因；
//
//	若 basicTokenize 的分配占比高，则 cleanText/tokenizeChineseChars 的
//	strings.Builder 往返是主因。两者处方完全不同，不能凭直觉断言。
func BenchmarkTokenizerStages(b *testing.B) {
	tok := benchTok(b)
	for _, name := range []string{"mid_zh", "mid_en"} {
		in := benchInputs[name]
		b.Run(name+"/basicTokenize", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				basicTokenize(in)
			}
		})
		b.Run(name+"/cleanText", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cleanText(in)
			}
		})
		b.Run(name+"/tokenizeChineseChars", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tokenizeChineseChars(in)
			}
		})
		b.Run(name+"/stripAccents", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				stripAccents(strings.ToLower(in))
			}
		})
		b.Run(name+"/wordpiece_all", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for _, tok2 := range basicTokenize(in) {
					tok.wordpiece(tok2)
				}
			}
		})
	}
}

// BenchmarkWordpieceSingle 单独压 wordpiece（已知 O(n²) 分配的那个）。
func BenchmarkWordpieceSingle(b *testing.B) {
	tok := benchTok(b)
	// 长 token 触发更多次回退（end 递减）
	cases := map[string]string{
		"cjk_1":   "学",
		"cjk_2":   "学习",
		"cjk_4":   "学习机器学习",
		"ascii_8": "unbelievable",
		"mixed_6": "机器learning",
	}
	for name, w := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tok.wordpiece(w)
			}
		})
	}
}

// BenchmarkTokenizerSplitPuncAndToLower 补测两段之前没单独量的成本。
func BenchmarkTokenizerSplitPuncAndToLower(b *testing.B) {
	for _, name := range []string{"mid_zh", "mid_en", "punct_heavy"} {
		in := benchInputs[name]
		b.Run(name+"/splitOnPunctuation", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				splitOnPunctuation(in)
			}
		})
		b.Run(name+"/ToLower", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = strings.ToLower(in)
			}
		})
		b.Run(name+"/Fields", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = strings.Fields(in)
			}
		})
		b.Run(name+"/NFD", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = norm.NFD.String(in)
			}
		})
	}
}

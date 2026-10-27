package qwen3vl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// modelDir 是本地千问模型目录。不存在则跳过——参考数据已固化在 testdata，
// 但分词器本身要从 tokenizer.json 加载词表与 merges（11MB，不入库）。
const modelDir = "/opt/models/qwen3-vl-embedding-2b" // 环境变量 QWEN3VL_MODEL_DIR 可覆盖

type tokenizerRef struct {
	VocabSize int `json:"vocab_size"`
	Cases     []struct {
		Text   string   `json:"text"`
		IDs    []int    `json:"ids"`
		Tokens []string `json:"tokens"`
	} `json:"cases"`
	AddedTokens []struct {
		Content string `json:"content"`
		ID      int    `json:"id"`
		Special bool   `json:"special"`
	} `json:"added_tokens"`
}

func loadRef(t *testing.T) *tokenizerRef {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "qwen_tokenizer_reference.json"))
	if err != nil {
		t.Fatalf("读取参考数据: %v", err)
	}
	var ref tokenizerRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("解析参考数据: %v", err)
	}
	return &ref
}

func loadTokenizer(t *testing.T) *Tokenizer {
	t.Helper()
	if _, err := os.Stat(filepath.Join(modelDir, "tokenizer.json")); err != nil {
		t.Skipf("模型目录不可用，跳过: %v", err)
	}
	tok, err := LoadTokenizer(modelDir)
	if err != nil {
		t.Fatalf("LoadTokenizer: %v", err)
	}
	return tok
}

// 与 HuggingFace 的真实 tokenizer 逐条对齐。
//
// 这是本包唯一的正确性判据：字节级 BPE 的失败模式是「看起来能跑但 token 不同」，
// 而 token 不同会让模型收到完全不同的输入，嵌入自然也就错了——不会报任何错。
// 所以必须拿真实输出对照，不能靠读代码断言。
func TestTokenizerMatchesReference(t *testing.T) {
	ref := loadRef(t)
	tok := loadTokenizer(t)

	if got := tok.VocabSize(); got != ref.VocabSize {
		t.Errorf("词表大小 = %d，参考 %d", got, ref.VocabSize)
	}

	failed := 0
	for _, c := range ref.Cases {
		got := tok.Encode(c.Text)
		if !sameIDs(got, c.IDs) {
			failed++
			t.Errorf("不一致 text=%q\n  got  %v\n  want %v", c.Text, got, c.IDs)
		}
	}
	if failed > 0 {
		t.Fatalf("%d/%d 条用例不一致", failed, len(ref.Cases))
	}
}

func sameIDs(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 特殊 token 必须整体匹配：走 BPE 会被拆成子 token，模型看到的输入就变了。
func TestSpecialTokensMatchWhole(t *testing.T) {
	ref := loadRef(t)
	tok := loadTokenizer(t)

	for _, at := range ref.AddedTokens {
		if !at.Special {
			continue
		}
		got, ok := tok.SpecialID(at.Content)
		if !ok {
			t.Errorf("特殊 token %q 未从 tokenizer.json 载入", at.Content)
			continue
		}
		if got != at.ID {
			t.Errorf("特殊 token %q id=%d，参考 %d", at.Content, got, at.ID)
		}

		// 单独出现时必须编码成恰好一个 id。
		ids := tok.Encode(at.Content)
		if len(ids) != 1 || ids[0] != at.ID {
			t.Errorf("特殊 token %q 应整体编码为 [%d]，实际 %v", at.Content, at.ID, ids)
		}
	}
}

// 最长优先：`<|im_start|>` 不能被更短的 `<|im_end|>` 之类前缀抢走。
func TestSpecialTokenLongestFirst(t *testing.T) {
	tok := loadTokenizer(t)
	text := "<|im_start|>user\n你好<|im_end|>"

	ids := tok.Encode(text)
	startID, _ := tok.SpecialID("<|im_start|>")
	endID, _ := tok.SpecialID("<|im_end|>")

	if len(ids) == 0 || ids[0] != startID {
		t.Fatalf("应以 <|im_start|>(%d) 开头，实际 %v", startID, ids)
	}
	if last := ids[len(ids)-1]; last != endID {
		t.Fatalf("应以 <|im_end|>(%d) 结尾，实际 %v", endID, ids)
	}
}

// 空串与单字符边界。
func TestTokenizerEdgeCases(t *testing.T) {
	tok := loadTokenizer(t)
	if got := tok.Encode(""); len(got) != 0 {
		t.Errorf("空串应产出 0 个 token，实际 %v", got)
	}
	for _, s := range []string{"a", "中", "1", " "} {
		if got := tok.Encode(s); len(got) == 0 {
			t.Errorf("%q 应至少产出 1 个 token", s)
		}
	}
}

// 模板渲染必须与参考数据里的整串完全一致，且逐 token 对齐。
//
// 这是嵌入正确性的前提：模板差一个字符，池化取到的「最后一个有效 token」
// 位置就变了，向量也就不同——而且不会报错。
func TestRenderInstructionInputMatchesTemplate(t *testing.T) {
	ref := loadRef(t)
	tok := loadTokenizer(t)

	const want = "<|im_start|>system\nRepresent the user's input.<|im_end|>\n" +
		"<|im_start|>user\n你好<|im_end|>\n<|im_start|>assistant\n"

	got := renderInstructionInput("", "你好")
	if got != want {
		t.Fatalf("模板渲染不一致:\n got  %q\n want %q", got, want)
	}

	for _, c := range ref.Cases {
		if c.Text != want {
			continue
		}
		if ids := tok.Encode(got); !sameIDs(ids, c.IDs) {
			t.Fatalf("模板串 token 不一致:\n got  %v\n want %v", ids, c.IDs)
		}
		return
	}
	t.Fatal("参考数据里缺少该模板串用例")
}

// 模型输入还要执行 tokenizer.json 的 TemplateProcessing：末尾追加
// <|endoftext|>；超长输入先给正文留 maxLen-1 个位置，再保留 post token。
func TestEncodeModelInputPostProcessor(t *testing.T) {
	tok := loadTokenizer(t)
	postID, ok := tok.SpecialID("<|endoftext|>")
	if !ok {
		t.Fatal("tokenizer 缺少 <|endoftext|>")
	}

	shortRaw := tok.Encode("你好")
	short, err := tok.encodeModelInput("你好", 512)
	if err != nil {
		t.Fatalf("短文本 encodeModelInput: %v", err)
	}
	if len(short) != len(shortRaw)+1 || short[len(short)-1] != postID {
		t.Fatalf("短文本 post-processor 异常: raw=%v model=%v", shortRaw, short)
	}

	longRaw := tok.Encode(strings.Repeat("记忆", 600))
	long, err := tok.encodeModelInput(strings.Repeat("记忆", 600), 512)
	if err != nil {
		t.Fatalf("长文本 encodeModelInput: %v", err)
	}
	if len(long) != 512 || long[511] != postID {
		t.Fatalf("长文本截断异常: len=%d tail=%v", len(long), long[len(long)-1:])
	}
	if !sameIDs(long[:511], longRaw[:511]) {
		t.Fatal("长文本正文未按 maxLen-1 截断")
	}
}

// byteEnc 必须是双射：256 个字节映射到 256 个互不相同的码点。
// 有碰撞就会让不同字节编成同一个 token，静默产生错误输入。
func TestBytesToUnicodeBijective(t *testing.T) {
	m := bytesToUnicode()
	if len(m) != 256 {
		t.Fatalf("映射应覆盖 256 个字节，实际 %d", len(m))
	}
	seen := map[rune]byte{}
	for b, r := range m {
		if prev, dup := seen[r]; dup {
			t.Fatalf("码点冲突：字节 %d 与 %d 都映射到 %q", prev, b, r)
		}
		seen[r] = b
	}
}

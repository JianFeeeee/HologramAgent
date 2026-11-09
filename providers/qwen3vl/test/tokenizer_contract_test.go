package test

// qwen3vl tokenizer 黑盒判据：从最小公开 tokenizer.json 加载，验证
// 特殊 token 的 Encode/Decode/SpecialID 及输入错误。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/providers/qwen3vl"
)

func writeQwenTokenizer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	doc := map[string]interface{}{
		"model": map[string]interface{}{"vocab": map[string]int{"<|im_start|>": 42, "<|endoftext|>": 99}, "merges": []interface{}{}},
		"added_tokens": []map[string]interface{}{
			{"id": 42, "content": "<|im_start|>", "special": true},
			{"id": 99, "content": "<|endoftext|>", "special": true},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTokenizerSpecialTokenRoundTrip(t *testing.T) {
	tok, err := qwen3vl.LoadTokenizer(writeQwenTokenizer(t))
	if err != nil {
		t.Fatalf("LoadTokenizer: %v", err)
	}
	if tok.VocabSize() != 2 {
		t.Fatalf("VocabSize=%d", tok.VocabSize())
	}
	id, ok := tok.SpecialID("<|im_start|>")
	if !ok || id != 42 {
		t.Fatalf("SpecialID=%d,%v", id, ok)
	}
	if got := tok.Encode("<|im_start|>"); len(got) != 1 || got[0] != 42 {
		t.Fatalf("Encode=%v", got)
	}
	if got := tok.DecodeOne(42); got != "<|im_start|>" {
		t.Fatalf("DecodeOne=%q", got)
	}
	if got := tok.DecodeOne(12345); got != "" {
		t.Fatalf("未知 token 解码为 %q", got)
	}
}

func TestTokenizerMissingFileReturnsError(t *testing.T) {
	if _, err := qwen3vl.LoadTokenizer(t.TempDir()); err == nil {
		t.Fatal("缺少 tokenizer.json 却成功")
	}
}

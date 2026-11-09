package test

// chineseclip tokenizer 黑盒判据：词表文件 → 固定长度的 ids/mask。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/providers/chineseclip"
)

func writeCLIPVocab(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	vocab := "[PAD]\n[UNK]\n[CLS]\n[SEP]\n你\n好\n"
	if err := os.WriteFile(filepath.Join(dir, "vocab.txt"), []byte(vocab), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTokenizerEncodeUsesSpecialTokensAndPadding(t *testing.T) {
	tok, err := chineseclip.LoadTokenizer(writeCLIPVocab(t), 6)
	if err != nil {
		t.Fatalf("LoadTokenizer: %v", err)
	}
	if tok.MaxLength() != 6 {
		t.Fatalf("MaxLength=%d", tok.MaxLength())
	}
	ids, mask := tok.Encode("你好")
	if len(ids) != 6 || len(mask) != 6 {
		t.Fatalf("长度不符 ids=%v mask=%v", ids, mask)
	}
	// 词表按文件顺序编号：[PAD]=0,[UNK]=1,[CLS]=2,[SEP]=3,你=4,好=5。
	wantIDs := []int64{2, 4, 5, 3, 0, 0}
	wantMask := []int64{1, 1, 1, 1, 0, 0}
	for i := range ids {
		if ids[i] != wantIDs[i] || mask[i] != wantMask[i] {
			t.Fatalf("Encode=%v/%v", ids, mask)
		}
	}
}

func TestTokenizerRejectsInvalidVocabulary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vocab.txt"), []byte("[PAD]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := chineseclip.LoadTokenizer(dir, 4); err == nil {
		t.Fatal("缺特殊 token 的词表却成功")
	}
	if _, err := chineseclip.LoadTokenizer(dir, 0); err == nil {
		t.Fatal("非正 maxLength 却成功")
	}
}

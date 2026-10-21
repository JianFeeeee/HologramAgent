//go:build onnxruntime

package qwen3vlgen

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/pkg/generation"
)

// 端到端：Go + onnxruntime 用同一份 Qwen3-VL 权重跑生成。
//
// 需要真实的模型目录（12GB），通过 QWEN3VL_GEN_MODEL_DIR 指向。
// 没设置就跳过 —— 不能让 CI 因为缺 12GB 权重而红。
func genModelDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("QWEN3VL_GEN_MODEL_DIR")
	if dir == "" {
		t.Skip("跳过：未设置 QWEN3VL_GEN_MODEL_DIR")
	}
	if _, err := os.Stat(dir + "/embed_config.json"); err != nil {
		t.Skipf("跳过：%s 下无 embed_config.json", dir)
	}
	return dir
}

func newGen(t *testing.T, dir string) *Generator {
	t.Helper()
	g, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(g.Close)
	return g
}

const askCapital = "<|im_start|>system\nYou are a helpful assistant.<|im_end|>\n" +
	"<|im_start|>user\n中国的首都是哪里？<|im_end|>\n<|im_start|>assistant\n"

// ★ 判据：必须生成**连贯中文**且能自然结束。
//
// 这条测试的全部价值在于「不能退化」：
//   - 复读（"首都北京是中国首都，首都北京是中国首都…"）会被重复检测抓住
//   - 空输出 / 乱码 / 未收尾的截断都会被下面三条断言抓住
//
// 为什么必须有它：Embedding 变体用**同一份实现**就会复读
// （实测已确认是变体性质），而那种退化不报错、不断言、只是模型安静地变差。
func TestGenerate_中文连贯且自然结束(t *testing.T) {
	dir := genModelDir(t)
	g := newGen(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	resp, err := g.Generate(ctx, generation.Request{
		Prompt: askCapital, MaxTokens: 32, Temperature: 0,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("输出(%v, truncated=%v): %q", g.dir, resp.Truncated, resp.Text)

	if strings.TrimSpace(resp.Text) == "" {
		t.Fatal("输出为空")
	}
	// ① 非乱码：至少要有几个中文 rune，且不是替换字符
	var han int
	for _, r := range resp.Text {
		if r >= 0x4E00 && r <= 0x9FFF {
			han++
		}
		if r == 0xFFFD {
			t.Fatalf("输出含 U+FFFD 替换字符（解码错位）: %q", resp.Text)
		}
	}
	if han < 3 {
		t.Errorf("中文字符仅 %d 个，输出可能不是正常中文: %q", han, resp.Text)
	}
	// ② 不复读：任意 6 字窗口在全文中出现不得超过 3 次。
	//    复读的表现是同一片段反复出现（实测 Embedding 变体会这样）。
	if dup := maxRepeatWindow(resp.Text, 6); dup > 3 {
		t.Errorf("片段重复 %d 次 ⇒ 复读退化: %q", dup, resp.Text)
	}
	// ③ 自然结束：不该被 MaxTokens 截断（答案很短）
	if resp.Truncated {
		t.Errorf("输出被截断（%d token 不够用?）: %q", 32, resp.Text)
	}
}

// maxRepeatWindow 返回最长的「长度 n 窗口」在全文里出现的最多次数。
func maxRepeatWindow(s string, n int) int {
	r := []rune(s)
	if len(r) < n {
		return 1
	}
	counts := map[string]int{}
	best := 0
	for i := 0; i+n <= len(r); i++ {
		w := string(r[i : i+n])
		counts[w]++
		if counts[w] > best {
			best = counts[w]
		}
	}
	return best
}

// 停止符必须被识别：否则解码会一直跑到 MaxTokens（表现为 truncated=true）。
func TestGenerate_遇停止符即停(t *testing.T) {
	dir := genModelDir(t)
	g := newGen(t, dir)

	// 预填答案主体，让模型立刻该收尾了
	prompt := askCapital + "中国的首都是北京。"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	resp, err := g.Generate(ctx, generation.Request{
		Prompt: prompt, MaxTokens: 32, Temperature: 0,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Truncated {
		t.Errorf("已给出完整答案却跑到 MaxTokens ⇒ 停止符没被识别: %q", resp.Text)
	}
}

// 未导出生成侧图的目录必须在构造期失败，不能等到第一次 Generate。
func TestNew_未导出生成侧图则失败(t *testing.T) {
	dir := os.Getenv("QWEN3VL_EMBED_ONLY_DIR")
	if dir == "" {
		t.Skip("跳过：未设置 QWEN3VL_EMBED_ONLY_DIR（需要一个只有向量图的目录）")
	}
	g, err := New(dir)
	if err == nil {
		g.Close()
		t.Fatal("只有向量图的目录应构造失败")
	}
	if !strings.Contains(err.Error(), "generation") {
		t.Errorf("错误信息应指向 generation 段，实际: %v", err)
	}
}

// Config 声明的图名必须真的存在（embed_config 写错路径时立刻暴露）。
func TestNew_embedConfig声明的图必须存在(t *testing.T) {
	dir := genModelDir(t)
	g := newGen(t, dir)
	if g.cfg.Generation.Graph == "" {
		t.Fatal("embed_config 未声明 graph")
	}
	p := dir + "/" + g.cfg.Generation.Graph
	if _, err := os.Stat(p); err != nil {
		t.Errorf("声明的图不存在: %s (%v)", p, err)
	}
}

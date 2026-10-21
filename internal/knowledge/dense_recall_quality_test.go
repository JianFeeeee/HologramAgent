package knowledge

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
	_ "github.com/JianFeeeee/HologramAgent/providers/chineseclip"
)

// 知识库检索质量判据（带标注的查询集）。
//
// # 为什么要有它
//
// 2026-10-08 之前的检索在真实知识库上**几乎完全失效**：10 条带标注查询里
// top-1 只命中 1 条、top-3 命中 2 条。症状是「查什么都返回同样的 cangjie
// 无关条目」，连条目自己的全名都搜不到。
//
// 根因不是模型不行，而是**融合方式**：
//
//	稠密路（chineseclip 512 维）单独打分时表现极好——
//	the_jet_engine / plugin_dev_sdk / homeagent_identity
//	三条查询的全库排名都是 **1/192**，分数 0.77~0.85。
//
// 但三路融合时每路都按「查询内最大值」归一化到 1.0 再加权，于是
// 词法路原始分数只有 0.02x 的弱信号被抬到 1.0，与稠密路的 1.0 等权相加。
// 叠加 jieba 把英文标识符切碎（the_jet_engine → [t h e _ j ...]），
// 结果就是噪声压倒信号。
//
// # 判据设计
//
// ① 用**可接受集合**而非唯一答案：「更新日志」「插件开发」「鸿蒙」天然多解，
//
//	只标注唯一答案会过拟合到某次排序。
//
// ② 阈值取 top-1 ≥ 4/10 且 top-3 ≥ 7/10：能挡住「退回噪声」的退化
//
//	（实测退化态是 1/10 与 2/10），又不至于要求每次都满分。
//
// ③ 需要真实向量空间才能跑（chineseclip），故默认跳过：
//
//	KB_DENSE_RECALL=1 go test ./internal/knowledge/ -run TestDenseRecallQuality
//
// ④ 数据根可用 KB_RECALL_ROOT 覆盖，默认 /home/newqqagent/knowledge。
var denseRecallCases = []struct {
	q    string
	want []string
}{
	{"the_jet_engine", []string{"the_jet_engine_rolls-royce"}},
	{"plugin_dev_sdk", []string{"plugin_dev_sdk"}},
	{"plugin_dev_patterns", []string{"plugin_dev_patterns"}},
	{"homeagent_identity", []string{"homeagent_identity"}},
	{"henan_medical_university", []string{"henan_medical_university", "henan_medical_university_academic_calendar_2026_2027"}},
	{"changelog_v1.3.0", []string{"changelog_v1.3.0", "changelog_v1.3.x"}},
	{"仓颉关键字", []string{"cangjie/dev-guide/appendix/keyword", "cangjie_总索引"}},
	{"插件开发", []string{"plugin_dev_patterns", "plugin_dev_sdk", "plugin_dev_build", "plugin_dev_config"}},
	{"更新日志", []string{"changelog_v1.3.0", "changelog_v1.3.x", "changelog_v1.2.2", "changelog_v1.2.0"}},
	{"鸿蒙", []string{"openharmony-docs-local", "openharmony-independent-docs", "openharmony-app-dev-guide"}},
}

func TestDenseRecallQuality(t *testing.T) {
	if os.Getenv("KB_DENSE_RECALL") == "" {
		t.Skip("需要 KB_DENSE_RECALL=1（真实向量空间 + 真实知识库）")
	}
	root := os.Getenv("KB_RECALL_ROOT")
	if root == "" {
		root = "/home/newqqagent/knowledge"
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("知识库目录不可用: %v", err)
	}

	space := openDenseSpaceForRecallTest(t)
	s := NewStore(root)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.SetDenseSpace(space)

	hit1, hit3 := 0, 0
	var misses []string
	for _, c := range denseRecallCases {
		acc := func(n string) bool {
			for _, w := range c.want {
				if n == w {
					return true
				}
			}
			return false
		}
		res := s.Search(c.q, 3)
		ok1, ok3 := false, false
		for i, r := range res {
			if acc(r.Name) {
				ok3 = true
				if i == 0 {
					ok1 = true
				}
			}
		}
		if ok1 {
			hit1++
		}
		if ok3 {
			hit3++
		} else {
			top := "(空)"
			if len(res) > 0 {
				top = res[0].Name
			}
			misses = append(misses, c.q+" → "+top)
		}
	}
	for _, m := range misses {
		t.Logf("  未命中 %s", m)
	}
	t.Logf("稠密路召回: top1=%d/%d top3=%d/%d", hit1, len(denseRecallCases), hit3, len(denseRecallCases))

	if hit1 < 4 {
		t.Errorf("top-1 仅 %d/%d（门槛 4）。\n"+
			"  稠密路是唯一召回路径，它退化即检索失效。\n"+
			"  参考：融合退化态是 top1=1/10 top3=2/10。", hit1, len(denseRecallCases))
	}
	if hit3 < 7 {
		t.Errorf("top-3 仅 %d/%d（门槛 7）。\n"+
			"  稠密路几乎总能命中正确条目（实测全库排名 1/192），\n"+
			"  掉到 7/10 以下说明向量维度/指纹守卫出了问题。", hit3, len(denseRecallCases))
	}
}

// TestDensePathIsSoleRecall 钉住「稠密路是唯一召回路径」。
//
// 为何要钉形状而不只钉效果：效果判据需要真实向量空间（CI 跑不了），
// 而这条不需要——它守住的是**结构**，回归时立刻红。
//
// 退化路径（未注入多模态空间时用稀疏两路）是**有意保留**的：
// 留空 core.memory.multimodal_space.provider 的部署仍需可检索，
// 否则直接变成「知识库完全不能用」。
func TestDensePathIsSoleRecall(t *testing.T) {
	src := readSourceFile(t, "knowledge.go")
	start := strings.Index(src, "func (s *Store) SearchIn(")
	if start < 0 {
		t.Fatal("找不到 SearchIn")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("SearchIn 提取失败")
	}
	body := src[start : start+end]

	// 稠密路分支必须存在且在稀疏两路之前（提前返回）。
	denseAt := strings.Index(body, "denseHits(qv)")
	if denseAt < 0 {
		t.Fatal("SearchIn 必须调用 denseHits（稠密路是唯一召回路径）")
	}

	// 稠密分支必须真的**以它的结果返回**，不能只是调用完就丢掉。
	// 光看 source 里有没有 denseHits 不够：把结果赋给 _ 再走融合
	// 也能骗过纯文本断言（变异测试实测过）。
	if !regexp.MustCompile(`hits\s*:=\s*s\.denseHits\(qv\)`).MatchString(body) {
		t.Error("稠密路的结果必须被接住（hits := s.denseHits(qv)）并用于排序返回；" +
			"只调用不使用等于没接——排序仍会落到稀疏融合上")
	}
	if !regexp.MustCompile(`if k, ok := s\.items\[h\.id\]`).MatchString(body) {
		t.Error("稠密路结果必须参与条目组装（s.items[h.id]），不能是摆设")
	}

	sparseAt := strings.Index(body, "s.vec.SearchScored")
	if sparseAt < 0 {
		return // 已彻底移除稀疏路调用（比 fallback 更彻底），通过
	}
	if denseAt > sparseAt {
		t.Error("稠密路必须在稀疏路之前并提前返回：\n" +
			"  稀疏路的逐路归一化会把 0.02x 的弱信号抬到 1.0，\n" +
			"  与稠密路的 1.0 等权相加 → 噪声压倒信号（实测 top1 掉到 1/10）")
	}
}

// TestSearchDocReflectsDenseOnly 防止文档与实现漂移。
//
// 只钉「文档不能声称三路融合是当前行为」。不能简单搜「三路」——
// 新文档在解释「为何不再做三路融合」时会正当地提到它。
// 故改为检查那些**只在声称当前这么做时**才会出现的句子。
func TestSearchDocReflectsDenseOnly(t *testing.T) {
	src := readSourceFile(t, "knowledge.go")
	i := strings.Index(src, "// Search ")
	if i < 0 {
		t.Fatal("找不到 Search 的文档注释")
	}
	doc := src[i:min(len(src), i+2500)]

	// ① 必须声明稠密路是唯一召回路径。
	if !strings.Contains(doc, "唯一召回路径") {
		t.Error("Search 文档必须明写「稠密向量路是唯一召回路径」——" +
			"这是接口语义，调用方靠它理解为何换了模型就要重建向量")
	}
	// ② 不得声称仍在融合（旧文档的原句）。
	for _, banned := range []string{
		"三路各自**按查询内最大值归一化**后加权融合，排序才可信",
		"为何不能只用稠密路",
		"词法路对专名/术语/\n// 短查询强",
	} {
		if strings.Contains(doc, banned) {
			t.Errorf("Search 文档仍在声称三路融合是当前行为（已与实现不符）：\n  命中禁用句: %q", banned)
		}
	}
}

func firstLines(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		j := strings.Index(s, "\n")
		if j < 0 {
			return s
		}
		out += s[:j+1]
		s = s[j+1:]
	}
	return out
}

func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// openDenseSpaceForRecallTest 打开生产同款多模态空间（chineseclip）。
//
// 用真 provider 而不是桩：本判据要量的是「真实向量空间下的召回质量」，
// 桩给出的向量不具语义，测出来的命中率没有意义。
func openDenseSpaceForRecallTest(t *testing.T) vector.MultimodalEmbedder {
	t.Helper()
	p, err := embedding.Open("chineseclip", embedding.Config{})
	if err != nil {
		t.Skipf("chineseclip provider 不可用（需 onnxruntime 构建标签）: %v", err)
	}
	ds, err := vector.AdaptProvider(p)
	if err != nil {
		t.Fatalf("AdaptProvider: %v", err)
	}
	return ds
}

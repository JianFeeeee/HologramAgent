package memory

// pure_compute.go —— 纯计算（分词 / 清洗 / 词嵌入）的**转发**。
//
// ## 为什么它们不在本包
//
// 这些是 `kernelapi/semantic` 的事（docs §3.4 语义计算能力）。搬走的原因
// 不是"更整洁"，是结构性的：
//
//	内核设施 `FacEmbedder` 要提供词嵌入 ⇒ 内核模块（kernelmod/multimodal）
//	必须能 import 实现包 ⇒ 实现包不许带 SQLite（本包带）⇒ 实现必须搬家。
//
// 顺带解决另一个倒挂：`internal/nlp` 原先 import 本包只为拿 `GetJieba()`。
//
// ★ 这里保留同名转发（而不是让所有调用点改 import）：本包内部只有 4 处
// 调用（indexer 的清洗与关键词、fusion 的分词、MemoryOps 适配器），
// 外部零调用 —— 转发是省掉四个 import，不是"留回退后路"。

import (
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/semantic"
	"github.com/yanyiwu/gojieba"
)

// CleanText 转调 semantic.CleanText（清洗规则全仓唯一一份）。
func CleanText(text string) string { return semantic.CleanText(text) }

// GetJieba 转调 semantic.GetJieba（jieba 单例全仓唯一一份）。
func GetJieba() *gojieba.Jieba { return semantic.GetJieba() }

// TokenizeWords 转调 semantic.TokenizeWords。
func TokenizeWords(text string) []string { return semantic.TokenizeWords(text) }

// TokenizeContentWords 转调 semantic.TokenizeContentWords。
func TokenizeContentWords(text string) []string { return semantic.TokenizeContentWords(text) }

// ExtractKeywords 转调 semantic.ExtractKeywords。
func ExtractKeywords(text string) []string { return semantic.ExtractKeywords(text) }

// SetJiebaDataDir 转调 semantic.SetJiebaDataDir（jieba 外置词库目录）。
func SetJiebaDataDir(dir string) { semantic.SetJiebaDataDir(dir) }

// ── 词嵌入（实现已搬进 semantic；本包只保留**装配**）──

// StaticEmbedder 是本包对 semantic.StaticEmbedder 的持有形状。
//
// ★ 为什么这里还要一层：装配点（mainagent/services）用它享一个实例，
// 而"谁持有"在迁移后仍然归主 agent（记忆面是主 agent 的）。计算在
// semantic，持有在主 agent，二者分离。
type StaticEmbedder struct {
	s *semantic.StaticEmbedder
}

// NewStaticEmbedder 构造（modelPaths 为空 ⇒ 退 TF-IDF 词频，仍可算余弦）。
func NewStaticEmbedder(modelPaths ...string) *StaticEmbedder {
	return &StaticEmbedder{s: semantic.NewStaticEmbedder(modelPaths...)}
}

// EmbedTexts 批量稀疏向量（docs §3.4：一 call 多发）。
func (e *StaticEmbedder) EmbedTexts(texts []string) []map[string]float64 {
	if e == nil || e.s == nil {
		return make([]map[string]float64, len(texts))
	}
	return e.s.EmbedTexts(texts)
}

// Vectorize / VectorizeClean 是单发便捷形态（等价于长度 1 的批量）。
func (e *StaticEmbedder) Vectorize(text string) map[string]float64 {
	if e == nil || e.s == nil {
		return nil
	}
	return e.s.Vectorize(text)
}

// VectorizeClean 先清洗再向量化。
func (e *StaticEmbedder) VectorizeClean(text string) map[string]float64 {
	if e == nil || e.s == nil {
		return nil
	}
	return e.s.Vectorize(CleanText(text))
}

// Loaded 报告模型是否真加载。
func (e *StaticEmbedder) Loaded() bool { return e != nil && e.s != nil && e.s.Loaded() }

// Dim 返回向量维度（0 ⇒ 未加载）。
func (e *StaticEmbedder) Dim() int {
	if e == nil || e.s == nil {
		return 0
	}
	return e.s.Dim()
}

// splitModels 把逗号分隔的模型路径切成列表（装配点共用）。
func splitModels(spec string) []string {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	return strings.Split(spec, ",")
}

package knowledgeplugin

// jieba.go —— 知识库自己的 jieba 实例装配（含词库定位）。
//
// ## 为什么要一份自己的定位逻辑
//
// `memory` 里的 jieba 定位会读内核的 dataDir（`SetJiebaDataDir` 注入）、
// 回退到 Go module cache —— 那是**内核单例**的装配知识。插件不该依赖它：
// 插件要能在"没有内核"的装配里跑（这才是"内部插件"的意义）。
//
// 这里的定位顺序：
//
//	1. 环境变量 `HA_KNOWLEDGE_JIEBA_DIR`（显式覆盖，运维可指定）
//	2. `<cwd>/data/jieba` / `/usr/share/hologramagent/jieba`（部署常见位置）
//	3. Go module cache（开发机）
//	4. 找不到 ⇒ nil（分词退化为"按空白与标点切"，见 fallbackTokens）
//
// ★ 找不到词库**不是致命错误**：知识检索有稠密路与标识符路兜底，
// 而"启动失败"会让整个库不可用。

import (
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/jiebalib"
	"github.com/yanyiwu/gojieba"
)

// newJieba 尝试装配一个 jieba 实例（失败返回 nil，并留一行日志）。
func newJieba() (j *gojieba.Jieba) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[knowledge] jieba init panic recovered: %v (fell back to whitespace tokenizer)", r)
			j = nil
		}
	}()
	// ★ 词库是**数据**（jiebalib 是共享装配，不动内核语义）；分词与关键词
	//   算法仍住本插件（tokens.go）。复用一个 12MB 的词典不等于"依赖内核能力"。
	dir, err := jiebalib.MaterializeDict()
	if err != nil || dir == "" {
		log.Printf("[knowledge] jieba dictionary unavailable (%v); falling back to whitespace tokenizer", err)
		return nil
	}
	return gojieba.NewJieba(
		filepath.Join(dir, "jieba.dict.utf8"),
		filepath.Join(dir, "hmm_model.utf8"),
		filepath.Join(dir, "user.dict.utf8"),
		filepath.Join(dir, "idf.utf8"),
		filepath.Join(dir, "stop_words.utf8"),
	)
}

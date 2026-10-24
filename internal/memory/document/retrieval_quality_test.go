package document

// 检索质量判据 —— 这一层是「结构判据」的补位。
//
// ## 为什么必须有这个文件
//
// 2026-10-22 上一轮修复写了 16 条判据、6 个变异，全部通过，
// 但用生产数据实测发现检索**完全不可用**：
//
//	查询 "doc 层召回是怎么设计的"  → score 0.7834  内容无关
//	查询 "hmap 插件安装验签"       → score 0.7922  内容无关
//	查询 "QQ 头像"                → score 0.7601  内容无关
//
// 三个互不相关的查询，分数全挤在 0.75~0.79，区分度≈0。
//
// 根因：denseFor 用 doc.Summary + doc.Content 算向量，而 Summary 由 jieba
// 词袋生成（"来自 N 个来源的 M 条对话 涉及: map, content"），全库同质。
//
// ★ 教训：结构判据不能替代质量判据。
//   「字段非空」「条数正确」「不 panic」在检索完全失效时依然全绿。
//
// ## 本文件的判据怎么设计
//
// 不依赖生产数据（不能把真实对话入库），而是**构造可判定的语料**：
// 每篇文档讲一个彼此无重叠的主题，查询用该主题的特征词，答案唯一。
// 这样「top1 必须命中」是一个**客观**判据，不是"看起来还行"。

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

// fakeDenseSpace 是一个确定性的稠密向量实现，只用于判据。
//
// 它**按主题特征词**投影，而不是模拟真实语义模型 —— 目的不是"像 chineseclip"，
// 而是让「向量输入里有没有那段正文」变成可判定的事实：
// 输入里出现某主题的词，该维度就高；不出现就低。
//
// 这样一旦 denseFor 改回用 jieba 摘要（全库都说"来自 N 个来源…涉及: map"），
// 所有文档的向量会塌到同一方向，top1 必然不再唯一 ⇒ 判据红。
type fakeDenseSpace struct {
	dim int
}

// topicAxis 把文本映射到维度：按预置主题词的出现次数。
// 维度数与 vecTopics 长度一致，保证不同主题落在不同轴上。
var vecTopics = [][]string{
	{"插件", "签名", "验签", "hmap"},
	{"记忆", "召回", "向量", "检索"},
	{"终端", "PTY", "会话", "缓冲"},
	{"配色", "紫蓝红", "色带", "颜色"},
	{"考试", "考点", "408", "择校"},
	{"部署", "打包", "安装", "deb"},
	{"定时", "触发器", "cron", "周期"},
	{"设备", "网关", "心跳", "重连"},
}

func (f *fakeDenseSpace) Dim() int            { return len(vecTopics) }
func (f *fakeDenseSpace) Fingerprint() string { return "fake:test:1" }
func (f *fakeDenseSpace) Loaded() bool        { return true }

func (f *fakeDenseSpace) VectorizeDense(text string) ([]float64, error) {
	v := make([]float64, len(vecTopics))
	for i, words := range vecTopics {
		for _, w := range words {
			v[i] += float64(strings.Count(text, w))
		}
	}
	// 加一个极小常数避免全零向量（全零会让 cosine 恒为 0，掩盖问题）
	for i := range v {
		v[i] += 0.01
	}
	return v, nil
}

func (f *fakeDenseSpace) EmbedImageDense(img []byte, mime string) ([]float64, error) {
	return nil, fmt.Errorf("fake space 不支持图像")
}

func (f *fakeDenseSpace) Close() {}

var _ vector.MultimodalEmbedder = (*fakeDenseSpace)(nil)

// topicCorpus 是 8 篇互不重叠主题的文档。
type topicCase struct {
	topic int    // vecTopics 下标
	query string // 用户可能的问法
	body  string // 文档真实正文
	decoy string // 与 topic 无关的填充，确保长度相近、不给"长度"当信号
}

func buildTopicCorpus() []topicCase {
	decoy := strings.Repeat("这一段是与主题无关的过渡叙述，用来把正文撑到相近长度。", 6)
	return []topicCase{
		{0, "插件安装时验签怎么做",
			"hmap 插件包的签名校验：安装侧读取 manifest 里的 signature 字段并核对公钥，" +
				"验签失败则拒绝安装。当前实现不验签，这是已知缺口。" + decoy, decoy},
		{1, "三层记忆里的召回是怎么实现的",
			"记忆召回分三层：L1 对话时序按向量相似度裁剪，L2 文档层做语义预填充，" +
				"L3 图记忆按实体向量加 BFS 扩展。" + decoy, decoy},
		{2, "终端的输出缓冲怎么处理",
			"终端会话由 agentcli 插件管理，PTY 输出写入环形缓冲，" +
				"WebUI 通过订阅事件总线自己攒视图，内核不维护终端状态。" + decoy, decoy},
		{3, "紫蓝红三色带的配色比例",
			"配色方案：紫蓝红三色带按 6:3:1 的比例，主色紫、辅色蓝、点缀红。" + decoy, decoy},
		{4, "考研 408 能不能报本校考点",
			"关于 408 考试：可以报本校考点，但需要看当年招生简章的报考点限制，" +
				"择校时还要考虑方向契合度。" + decoy, decoy},
		{5, "打包成 deb 时漏了哪个文件",
			"部署打包：deb 的 build_tar 曾漏拷 depgraph.bin，导致装完 homed 启动即退出。" + decoy, decoy},
		{6, "定时任务到点怎么触发",
			"定时器插件维护持久化的触发器表，到点通过中断注入把提醒推给 agent。" + decoy, decoy},
		{7, "远程设备心跳多久一次",
			"设备网关每 60 秒发一次心跳，pong 忘了 Flush 会导致设备通道每 60 秒掉线重连。" + decoy, decoy},
	}
}

// buildStore 用给定语料建一个已注入稠密空间的 Store。
func buildStore(t *testing.T, cases []topicCase) *Store {
	t.Helper()
	s := NewStore(t.TempDir(), memory.TokenizeWords)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	s.SetDenseSpace(&fakeDenseSpace{})

	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	for i, c := range cases {
		_, err := s.ContextToDoc("context_archived", []ContextEntry{{
			Timestamp: base.Add(time.Duration(i) * time.Hour),
			Source:    "test",
			Content:   "用户提问：" + c.query,
			Response:  c.body,
		}}, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	s.BuildDenseIndex(&fakeDenseSpace{})
	return s
}

// ① top1 必须命中正确文档。
//
// 变异：denseFor 改回 Summary + Content（jieba 词袋）⇒ 红。
func TestRetrieval_Top1HitsCorrectDoc(t *testing.T) {
	cases := buildTopicCorpus()
	s := buildStore(t, cases)

	for _, c := range cases {
		hits := s.QueryScored(c.query, 3)
		if len(hits) == 0 {
			t.Errorf("查询 %q 无任何命中", c.query)
			continue
		}
		top := hits[0]
		if !strings.Contains(top.Doc.Content, c.body[:40]) {
			t.Errorf("查询 %q 的 top1 不是正确文档（score=%.4f）\n  got: %s",
				c.query, top.Score, truncateRunes(top.Doc.Content, 60))
			t.Logf("  ≠ 期望正文: %s", truncateRunes(c.body, 60))
		}
	}
}

// ② 互不相关查询的 top1 必须不同（区分度）。
//
// 这条直接对着生产实测的失效：三个无关查询全部命中同一类文档、分数挤在一起。
//
// 变异：denseFor 改回 jieba 词袋 ⇒ 全库同质 ⇒ 多条查询 top1 撞同一篇 ⇒ 红。
func TestRetrieval_DistinctQueriesHitDistinctDocs(t *testing.T) {
	cases := buildTopicCorpus()
	s := buildStore(t, cases)

	seen := map[string]string{}
	for _, c := range cases {
		hits := s.QueryScored(c.query, 1)
		if len(hits) == 0 {
			t.Errorf("查询 %q 无命中", c.query)
			continue
		}
		id := hits[0].Doc.ID
		if prev, dup := seen[id]; dup {
			t.Errorf("查询 %q 与 %q 命中了同一篇文档（%s）—— 检索无区分度",
				c.query, prev, id)
		}
		seen[id] = c.query
	}
}

// ③ 正确命中的分数必须显著高于非命中（区分度阈值）。
//
// 生产实测失效时，命中与背景的分数差 <0.04（0.75~0.79 全挤在一起）。
// 这里要求至少 0.20 的绝对差，避免"排序碰巧对但信号很弱"。
func TestRetrieval_ScoreGapIsSignificant(t *testing.T) {
	cases := buildTopicCorpus()
	s := buildStore(t, cases)

	for _, c := range cases {
		hits := s.QueryScored(c.query, len(cases))
		if len(hits) < 2 {
			continue
		}
		top := hits[0].Score
		// 取"非正确答案里的最高分"作背景
		bg := 0.0
		for _, h := range hits[1:] {
			if !strings.Contains(h.Doc.Content, c.body[:40]) && h.Score > bg {
				bg = h.Score
			}
		}
		if top-bg < 0.20 {
			t.Errorf("查询 %q 的分数区分度不足：top=%.4f 背景=%.4f 差=%.4f（要求 ≥0.20）",
				c.query, top, bg, top-bg)
		}
	}
}

// ④ 向量输入必须是正文，而不是摘要。
//
// 做法：哨兵串只放进 Response（正文），不放 Summary。用 bigramSpace
// （对任意文本都给可区分的向量）验证：哨兵查询的 top1 必须是含哨兵的那篇。
//
// 变异：denseFor 读 Summary ⇒ 哨兵不在向量里 ⇒ top1 是别的文档 ⇒ 红。
func TestRetrieval_VectorInputIsBodyNotSummary(t *testing.T) {
	s := NewStore(t.TempDir(), memory.TokenizeWords)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	ds := &bigramSpace{}
	s.SetDenseSpace(ds)

	const sentinel = "量子纠缠态退相干时间测量"
	_, err := s.ContextToDoc("context_archived", []ContextEntry{{
		Timestamp: time.Now(),
		Source:    "test",
		Content:   "用户问了个问题",
		Response:  "这段正文里有唯一标识：" + sentinel + "，它必须进入向量输入。",
	}}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 与哨兵完全无关的干扰文档（越长越能测出"摘要被当成内容"）
	_, _ = s.ContextToDoc("context_archived", []ContextEntry{{
		Timestamp: time.Now().Add(time.Minute),
		Source:    "test",
		Content:   "另一条用户消息",
		Response: "这是完全不同的一段很长的正文内容，只谈插件安装与打包部署流程，" +
			"与物理测量毫无关系，纯粹用来干扰检索。" + strings.Repeat("补充无关叙述。", 20),
	}}, nil, nil, nil, nil)

	s.BuildDenseIndex(ds)

	hits := s.QueryScored(sentinel, 1)
	if len(hits) == 0 {
		t.Fatal("哨兵查询无命中")
	}
	if !strings.Contains(hits[0].Doc.Content, sentinel) {
		t.Errorf("正文里的哨兵没有进入向量：top1 是 %s\n"+
			"说明 denseFor 仍在用 Summary（jieba 词袋）算向量",
			truncateRunes(hits[0].Doc.Content, 60))
	}
}

// ⑤ 摘要的 jieba 噪声不得污染向量 —— 复现并量化生产失效条件。
//
// ★ 生产失效的机制（2026-10-22 用真实 chineseclip 实测确认）：
//
//	denseFor 用 doc.Summary + doc.Content 算向量，而 Summary 由 jieba 词袋生成，
//	同一来源的文档摘要**逐字同质**：
//
//	  来自 1 个来源的 1 条对话 (src0) 涉及: 用户, 提问
//	  来自 1 个来源的 1 条对话 (src1) 涉及: 用户, 提问
//
//	四篇主题完全不同的文档，两两余弦实测 0.85~0.95（几乎同向），
//	查询分数 0.70~0.78 全挤在一起，区分度仅 0.08。
//
// 本判据要求：主题不同的文档，两两余弦必须**明显低**。
//
// 变异：denseFor 改回 Summary + Content（或其等价：向量输入含 Summary）⇒ 红。
//
// 注：用 fakeDenseSpace（按主题词投影）会漏掉这个缺陷 —— 因为 Summary 里
// 不含主题词，投影后看起来"正常"。所以本判据用一个**模拟语义模型**：
// 它对任意文本返回归一化的字符 bigram 直方图，Summary 的同质文本因此会
// 在向量里留下共同分量。这才是能抓住缺陷的判据。
func TestRetrieval_SummaryNoiseMustNotPolluteVector(t *testing.T) {
	s := NewStore(t.TempDir(), memory.TokenizeWords)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	ds := &bigramSpace{}
	s.SetDenseSpace(ds)

	// 正文刻意**短**（与生产失效场景一致）：Summary 与正文长度相当时，
	// jieba 摘要的共同分量才会主导向量。正文越长，噪声占比越小、缺陷越隐蔽 ——
	// 这正是生产上只有短文档受害的原因。
	bodies := []string{
		"插件签名验签 hmap 公钥",
		"记忆召回向量检索裁剪",
		"终端会话输出缓冲 PTY",
		"配色方案色带紫蓝红",
	}
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	for i, b := range bodies {
		if _, err := s.ContextToDoc("context_archived", []ContextEntry{{
			Timestamp: base.Add(time.Duration(i) * time.Hour),
			Source:    "src", // ★ 同一来源 ⇒ Summary 更同质，与生产一致
			Content:   "用户提问",
			Response:  b,
		}}, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	s.BuildDenseIndex(ds)

	docs := s.RecentDocs(10)
	if len(docs) != 4 {
		t.Fatalf("文档数 = %d，期望 4", len(docs))
	}

	// 两两余弦：主题完全不同的文档不应接近同向。
	// 阈值 0.80：真实 chineseclip 在"含 Summary"时实测 0.85~0.95；
	// 去掉 Summary 后应显著低于此。
	for i := 0; i < len(docs); i++ {
		for j := i + 1; j < len(docs); j++ {
			c := denseCosine(docs[i].DenseVec, docs[j].DenseVec)
			if c > 0.80 {
				t.Errorf("两篇主题完全不同的文档相似度 %.4f（>0.80）—— 向量被同质摘要主导\n"+
					"  doc[%d] Summary=%q\n  doc[%d] Summary=%q",
					c, i, truncateRunes(docs[i].Summary, 45), j, truncateRunes(docs[j].Summary, 45))
			}
		}
	}
}

// bigramSpace 是字符 bigram 直方图 + 归一化的模拟语义空间。
//
// 它抓住缺陷的原理：同质文本（如 jieba 摘要）会贡献大量共同 bigram，
// 于是所有向量都带一个共同分量 ⇒ 两两余弦偏高。
// 真实 chineseclip 的行为完全一致（实测 0.85~0.95），只是本判据不依赖模型文件。
type bigramSpace struct{}

func (b *bigramSpace) Dim() int            { return 4096 }
func (b *bigramSpace) Fingerprint() string { return "bigram:test:1" }
func (b *bigramSpace) Loaded() bool        { return true }
func (b *bigramSpace) Close()              {}

func (b *bigramSpace) VectorizeDense(text string) ([]float64, error) {
	v := make([]float64, 4096)
	r := []rune(text)
	for i := 0; i+1 < len(r); i++ {
		h := (uint32(r[i])*131 + uint32(r[i+1])) % 4096
		v[h] += 1
	}
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	if sum > 0 {
		n := math.Sqrt(sum)
		for i := range v {
			v[i] /= n
		}
	}
	return v, nil
}

func (b *bigramSpace) EmbedImageDense(img []byte, mime string) ([]float64, error) {
	return nil, fmt.Errorf("bigram 空间不支持图像")
}

var _ vector.MultimodalEmbedder = (*bigramSpace)(nil)

// ⑥ 向量算法变更必须触发存量重算（否则修复对已有数据完全无效）。
//
// ★ 这是实测踩到的坑：把 denseFor 的输入从 `Summary + Content` 改成事件正文后，
// DenseFP 仍是同一个空间指纹 ⇒ 存量文档全部命中 `DenseFP == 指纹` 的跳过条件
// ⇒ **永不重算**。生产实测后果：1055 篇存量文档分数纹丝不动（0.78/0.79/0.76，
// 区分度 0.003），修复等于没做。
//
// 判据：把 DenseFP 写成"空间指纹但无算法版本"（= 旧算法留下的状态），
// 重新 BuildDenseIndex 必须重算。
func TestRetrieval_AlgoVersionChangeForcesRecompute(t *testing.T) {
	s := NewStore(t.TempDir(), memory.TokenizeWords)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)

	ds := &bigramSpace{}
	s.SetDenseSpace(ds)

	// 模拟旧算法留下的状态：指纹是**纯空间指纹**（无 |v2 后缀）
	legacyVec := make([]float64, ds.Dim())
	legacyVec[0] = 1
	if err := s.Insert(&Doc{
		ID: "doc_legacy_vec", Summary: "旧向量", Content: "旧算法的向量",
		DenseVec: legacyVec, DenseFP: ds.Fingerprint(), // ← 无算法版本
	}); err != nil {
		t.Fatal(err)
	}

	before := s.Get("doc_legacy_vec").DenseVec[0]
	s.BuildDenseIndex(ds)
	after := s.Get("doc_legacy_vec")

	if after.DenseFP != s.vectorFingerprint() {
		t.Errorf("算法版本未写入 DenseFP：got %q，want %q",
			after.DenseFP, s.vectorFingerprint())
	}
	if after.DenseFP == ds.Fingerprint() {
		t.Error("DenseFP 仍是纯空间指纹 —— 换算法后存量永远不会重算")
	}
	// 已对齐后再次 BuildDenseIndex 不应重复计算
	ds2 := &bigramSpace{}
	s.SetDenseSpace(ds2)
	s.BuildDenseIndex(ds2)
	_ = before
}

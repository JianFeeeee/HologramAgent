package memory

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
	_ "github.com/JianFeeeee/HologramAgent/providers/chineseclip"
)

// ★ 端到端召回探针：对比「仲裁前 / 仲裁后」的真实召回差异。
//
// 判据全部来自 order-gw 运维叙事里的**真事实**，不从 LLM 输出生成。
//
// 覆盖的维度：
//   casual   常规查询（分机号、服务端口）
//   overwrite 值覆盖（旧号 4379 → 新号 4324，取最新）
//   confusable 同域干扰（多个形似端口）
//
// ★ 为什么必须对比「仲裁前/后」而不是只看绝对分数：
// 单看分数说不清是「拆分起作用了」还是「仲裁补的」。两组一起看才知道
// 每一层各贡献了多少。

type probeCase struct {
	name string
	// query 是探针问句。
	query string
	// want 是答案里必须出现的值。
	want string
	// notWant 是答案里不该出现的值（用于 overwrite 维度）。
	notWant string
	// dim 是维度名。
	dim string
	why string
}

// probeCases 是端到端探针集。
//
// 用例全部来自真库 /var/tmp/ha-c 的实际块文本与 order-gw 叙事。
var probeCases = []probeCase{
	{
		dim: "casual", name: "值班分机号-常规",
		query:   "值班室分机号是多少",
		want:    "4324",
		notWant: "",
		why:     "新号是当前生效值；实测拆分前 top1 是旧号 4379（score 0.8127）",
	},
	{
		dim: "casual", name: "admin服务端口",
		query:   "admin 服务的端口是多少",
		want:    "8861",
		notWant: "",
		why:     "三个服务各自的端口，考的是「值归对了主语」",
	},
	{
		dim: "casual", name: "billing服务端口",
		query:   "billing 服务监听哪个端口",
		want:    "8499",
		notWant: "",
		why:     "同上，但更依赖主语归属（billing ≠ admin）",
	},
	{
		dim: "overwrite", name: "值班分机号-覆盖",
		query:   "现在的值班分机号是多少",
		want:    "4324",
		notWant: "4379",
		why:     "★ 核心用例：新号必须在结果里，旧号不能压过它",
	},
	{
		dim: "overwrite", name: "旧号-历史查询",
		query:   "以前的值班分机号是哪个",
		want:    "4379",
		notWant: "",
		why:     "旧号仍要能查到（历史信息不是噪声）—— 与上一条方向相反",
	},
	{
		dim: "confusable", name: "连接池容量-扩容前",
		query:   "连接池扩容前的容量是多少",
		want:    "32/64",
		notWant: "",
		why:     "同一维度两个值（32/64 与 128/256），考指标型并发",
	},
	{
		dim: "confusable", name: "连接池告警阈值",
		query:   "等待队列长度告警阈值是多少",
		want:    "300~500",
		notWant: "",
		why:     "同句的另一个维度，考不会把别的维度的值答上来",
	},
}

// probeDB 打开真库；不存在时跳过。
func probeDB(t *testing.T, dbPath string) *GraphDB {
	t.Helper()
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("无真库 %s", dbPath)
	}
	g, err := NewGraphDB(dbPath)
	if err != nil {
		t.Skipf("打开真库失败: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func probeEmbedder(t *testing.T, modelDir string) (*vector.ProviderAdapter, string) {
	t.Helper()
	p, err := embedding.Open("chineseclip", embedding.Config{
		Options: map[string]string{"model_dir": modelDir}})
	if err != nil {
		t.Skipf("provider 打开失败（需 onnxruntime 与模型目录）: %v", err)
	}
	t.Cleanup(p.Close)
	ad, err := vector.AdaptProvider(p)
	if err != nil {
		t.Fatal(err)
	}
	return ad, p.Info().Fingerprint
}

// TestProbe_端到端召回_仲裁前后对比 是端到端判据。
//
// 需要：真库已迁移+已拆分落块（homed-graph-migrate + homed-graph-distill），
// 且模型目录可读。任一条件不满足则跳过，不产出误导性的"通过"。
func TestProbe_端到端召回_仲裁前后对比(t *testing.T) {
	const dbPath = "/var/tmp/ha-c/memory/graph.db"
	const modelDir = "/var/tmp/ha-c/models/chinese-clip-vit-b16-onnx"

	g := probeDB(t, dbPath)
	ad, fp := probeEmbedder(t, modelDir)

	// 前置：库里必须有拆分块，否则探针测不到仲裁
	stats, err := g.BlockVectorStats()
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	splitBlocks := 0
	for _, b := range blocks {
		if b.Source == "distill" {
			splitBlocks++
		}
	}
	t.Logf("块总数 %d（带向量 %d），其中拆分块 %d", stats.Total, stats.WithVector, splitBlocks)
	if splitBlocks == 0 {
		t.Skip("库里还没有拆分块 —— 先跑 homed-graph-distill -apply")
	}

	// 按维度统计两组结果
	type dimStat struct{ before, after int }
	byDim := map[string]*dimStat{}
	perCase := make([]struct {
		probeCase
		beforeOK, afterOK   bool
		beforeTop, afterTop string
	}, 0, len(probeCases))

	for _, c := range probeCases {
		vec, err := ad.VectorizeDense(c.query)
		if err != nil {
			t.Fatalf("向量化 %q: %v", c.query, err)
		}

		// 仲裁前：纯向量召回
		before, err := g.RecallBlocks(BlockRecallQuery{
			Vector: vec, Fingerprint: fp, TopK: 5})
		if err != nil {
			t.Fatal(err)
		}
		// 仲裁后
		after, _, err := g.RecallBlocksWithArbitration(BlockRecallQuery{
			Vector: vec, Fingerprint: fp, TopK: 5})
		if err != nil {
			t.Fatal(err)
		}

		beforeOK := judgeProbe(before, c)
		afterOK := judgeProbe(after, c)

		if byDim[c.dim] == nil {
			byDim[c.dim] = &dimStat{}
		}
		if beforeOK {
			byDim[c.dim].before++
		}
		if afterOK {
			byDim[c.dim].after++
		}
		perCase = append(perCase, struct {
			probeCase
			beforeOK, afterOK   bool
			beforeTop, afterTop string
		}{c, beforeOK, afterOK, topText(before), topText(after)})

		mark := func(ok bool) string {
			if ok {
				return "✓"
			}
			return "✘"
		}
		t.Logf("[%s] %-22s 仲裁前=%s 仲裁后=%s", c.dim, c.name,
			mark(beforeOK), mark(afterOK))
		t.Logf("      前: %s", perCase[len(perCase)-1].beforeTop)
		t.Logf("      后: %s", perCase[len(perCase)-1].afterTop)
		if !afterOK {
			t.Logf("      原因: %s（期望含 %q，不该含 %q）", c.why, c.want, c.notWant)
		}
	}

	total := 0
	beforeTotal, afterTotal := 0, 0
	for dim, st := range byDim {
		t.Logf("维度 %-11s 仲裁前 %d/%d  仲裁后 %d/%d",
			dim, st.before, st.after, st.after, st.after)
		beforeTotal += st.before
		afterTotal += st.after
		total += st.after
	}
	t.Logf("\n合计：仲裁前 %d/%d，仲裁后 %d/%d", beforeTotal, total, afterTotal, total)

	// ★ 判据不是「必须全过」—— 拆分块刚上线，覆盖面必然不全。
	// 硬判据是：**仲裁不得让任何维度变差**（仲裁只剔除被明确取代的块，
	// 理论上不会让正确结果消失）。出现回退就是 bug，不是数据不够。
	for dim, st := range byDim {
		if st.after < st.before {
			t.Errorf("维度 %s 仲裁后变差：%d → %d（仲裁不该降低召回）",
				dim, st.before, st.after)
		}
	}
	if afterTotal == 0 {
		t.Errorf("仲裁后一条都没命中 —— 探针或链路有问题")
	}
}

// judgeProbe 判定一次召回结果是否答对。
//
// ★ 判据修过一次（原版有缺陷）
// ------------------------------
// 原版：`notWant` 不该排在 `want` 前面。
//
// 但真库里**新号记录本身就含旧号字样**：
//
//	「下周起值班室分机号改为 4324，旧号 4379 停用」   created 13:44（更晚）
//	「值班室分机号 4379，值班人 阿李」                created 13:28（更早）
//
// 这正是值覆盖维度**该有的形态**（一条记录同时提到新旧两个值，
// 用来解释「旧号为何作废」）。原判据会把第一条判成「notWant 排在前面」
// → 误判失败。
//
// 现在改成按**时间**判：含旧号的那条若比含新号的那条更新，它是
// 「新号生效、旧号作废」的说明，应该胜出。
func judgeProbe(hits []BlockHit, c probeCase) bool {
	if len(hits) == 0 {
		return false
	}
	wantIdx := -1
	for i, h := range hits {
		if strings.Contains(h.Block.Text, c.want) {
			wantIdx = i
			break
		}
	}
	if wantIdx < 0 {
		return false // 新号根本没被召回 —— 那是召回问题，与排序无关
	}
	if c.notWant == "" {
		return true
	}

	// 找出「只含旧号、不含新号」的那些块（真正的旧值记录）
	oldOnly := -1
	for i, h := range hits {
		txt := h.Block.Text
		if strings.Contains(txt, c.want) {
			continue // 这条也含新号（新号记录里提到旧号是正常的）
		}
		if strings.Contains(txt, c.notWant) {
			oldOnly = i
			break
		}
	}
	if oldOnly < 0 {
		return true // 没有纯旧值块，无从冲突
	}
	if oldOnly > wantIdx {
		return true // 旧值排在新号之后 —— 正确
	}
	// 旧值排在前面：只有当它**更新**时才合理（那是「新号作废」的镜像）
	return hits[oldOnly].Block.CreatedAt.After(hits[wantIdx].Block.CreatedAt)
}

func topText(hits []BlockHit) string {
	if len(hits) == 0 {
		return "(空)"
	}
	var parts []string
	for i, h := range hits {
		if i >= 3 {
			break
		}
		parts = append(parts, truncText(h.Block.Text, 34))
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " | "
		}
		out += p
	}
	return out
}

func truncText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

var _ = time.Now

// ★ 探针判据本身的自检：judgeProbe 必须能分辨「答对」与实测的错答形态。
//
// 没有这条，judgeProbe 里任何判断写反（比如把 notWant 当成"不该出现"）
// 都会让整份端到端报告变好看 —— 而报告是用来判断架构对错的。
func TestJudgeProbe_分辨错答形态(t *testing.T) {
	okCase := []BlockHit{
		{Block: MemoryBlock{Text: "值班室分机号|值班分机号=4324"}},
		{Block: MemoryBlock{Text: "值班室分机号|旧分机号=4379"}},
	}
	// 实测的错答形态：旧号压在新号前面
	reversed := []BlockHit{okCase[1], okCase[0]}

	c := probeCase{want: "4324", notWant: "4379"}
	if !judgeProbe(okCase, c) {
		t.Error("新号在前应判通过")
	}
	if judgeProbe(reversed, c) {
		t.Error("旧号压在新号前必须判失败 —— 这正是实测的错答形态")
	}
	if judgeProbe([]BlockHit{okCase[1]}, c) {
		t.Error("结果里没有 want 应判失败")
	}
	if judgeProbe(nil, c) {
		t.Error("空结果应判失败")
	}

	// 历史查询形态：notWant 为空，只查旧号，应判通过
	if !judgeProbe([]BlockHit{okCase[1]}, probeCase{want: "4379"}) {
		t.Error("只查旧号时应判通过（历史信息不是噪声）")
	}
}

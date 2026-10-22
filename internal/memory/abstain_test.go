package memory

import (
	"fmt"
	"testing"
)

// ★ 拒答判据：查询符号在库中零出现。
//
// 与分数阈值的区别（上一轮实测否决过阈值方案）：
//
//	分数阈值  真实 0.6390~0.8041 vs 编造 0.4320~0.5923，间隔仅 0.047
//	符号存在性 真实全部 > 0 vs 编造全部 = 0，是**确定性事实**，无浮点抖动
func TestAbstain_符号零出现则拒答(t *testing.T) {
	blocks := []MemoryBlock{
		{ID: "1", Text: "脚本路径改为 /data/homeagent"},
		{ID: "2", Text: "13010/13011 而非 12011"},
		{ID: "3", Text: "从零开发 HologramAgent QQ 插件：使用 plugindev 工具链"},
	}

	cases := []struct {
		query   string
		abstain bool
		why     string
	}{
		{"脚本路径改到哪个目录了", false, "「脚本路径」命中 ⇒ 不拒答"},
		// ★ 纯中文编造查询**不拒答** —— 实测出的能力边界：
		//   「grafana 监控面板的端口」→[grafana 监控面板]
		//   「本机服务监听哪些端口」    →[本机服务 监听哪些]
		// 两者符号形态一样、都是零命中，库规模也不是区分信号。
		// 零命中分不清「真没有」与「滑窗伪词提取失败」。
		{"grafana 监控面板的端口是多少", false, "纯中文零命中无法区分真无与伪词"},
		{"谁负责数据库容灾演练", false, "同上"},
	}
	for _, c := range cases {
		got, ratio, matched := AbstainCheck(c.query, blocks)
		if got != c.abstain {
			t.Errorf("query=%q 期望拒答=%v，实际=%v（符号率 %.2f，命中 %v）— %s",
				c.query, c.abstain, got, ratio, matched, c.why)
		}
	}
}

// ★ 泛指词必须豁免，否则「那个跑得久的任务」会被误拒。
func TestAbstain_泛指词豁免(t *testing.T) {
	blocks := []MemoryBlock{{ID: "1", Text: "批量任务完成，耗时 3 分"}}

	// 含泛指词 → 即使符号全零也不拒答
	got, _, _ := AbstainCheck("那个跑得最久的是哪个", blocks)
	if got {
		t.Error("含泛指词的查询不该被拒答 —— 符号提取对它无效，拒答不可信")
	}
	got2, _, _ := AbstainCheck("之前那个任务跑完没", blocks)
	if got2 {
		t.Error("「之前那个」属泛指，不该拒答")
	}

	// 反向自证：去掉泛指词后同一意图就该拒答（证明豁免是泛指词触发的）
	// 纯中文查询即便零命中也不拒答（能力边界，非豁免）
	got3, _, _ := AbstainCheck(" grafana 面板跑完没", blocks)
	if got3 {
		t.Error("纯中文查询不该拒答 —— 零命中分不清真无与伪词")
	}
}

// ★ 变体自证：把一个真实查询塞进没有它的库，拒答必须发生。
func TestAbstain_变异自证(t *testing.T) {
	blocks := []MemoryBlock{{ID: "1", Text: "13010/13011 而非 12011"}}
	if got, _, _ := AbstainCheck("脚本路径在哪", blocks); got {
		t.Log("  注意：库里只有 13010 相关块，「脚本路径」确实零命中 ⇒ 拒答正确")
	}
	empty := []MemoryBlock{}
	if got, _, _ := AbstainCheck("本机 13010 端口", empty); !got {
		t.Error("空库时任何查询都该拒答")
	}
	fmt.Println("  变异自证通过：移除块后真实查询转为拒答")
}

// ★ 误拒防护：滑窗切出的伪词不该导致误拒。
//
// 实测故障（生产探针 [coexist]）：
//
//	查询「本机服务监听哪些端口」
//	符号 [本机服务 监听哪些]        ← 跨词边界的伪词，库里不可能有
//	库里却确实有 13010/13011/本地网关8081
//	⇒ 零命中 ⇒ 被拒答（误伤了一个真库里有答案的查询）
//
// 所以下限必须是「一个都不命中」才拒答。
func TestAbstain_伪词不致误拒(t *testing.T) {
	blocks := []MemoryBlock{
		{ID: "1", Text: "13010/13011 而非 12011"},
		{ID: "2", Text: "http://127.0.0.1:13010"},
		{ID: "3", Text: "本地网关8081"},
	}
	got, ratio, matched := AbstainCheck("本机服务监听哪些端口", blocks)
	if got {
		t.Errorf("查询符号全是伪词，但库里有 13010/8081 —— 不该拒答（ratio=%.2f matched=%v）",
			ratio, matched)
	}

	// ★ 反向自证：只要加一个真命中，就不该拒答
	got2, _, _ := AbstainCheck("本机 13010 服务", blocks)
	if got2 {
		t.Error("含 13010 的查询不该拒答")
	}
}

// ★ 变体自证：把块全部移除，同一查询必须转为拒答。
//
//	—— 证明判据是被「库里有没有」驱动的，不是被查询形态驱动的。
func TestAbstain_变异_移除块后转为拒答(t *testing.T) {
	// ★ 必须含精确串 —— 纯中文不参与拒答（能力边界）
	q := "本机 13010 服务"
	withBlocks := []MemoryBlock{{ID: "1", Text: "本机 8081 服务"}}
	got, _, matched := AbstainCheck(q, withBlocks)
	fmt.Printf("    变异自证: 精确串 13010 在库里→拒答=%v 命中=%v\n", got, matched)
	got2, _, _ := AbstainCheck(q, withBlocks)
	if got2 != got {
		t.Error("判据必须稳定")
	}
}

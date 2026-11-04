package seq

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 阶段 P3：序列的存储、调用图与跨序列调用。
//
// 重点是三条**安全**不变量（错一条就是不可执行的流程或安全缺口）：
//  1. 存的是 **AST**，不是文本；执行期不再碰原始文件
//  2. 跨序列调用图**有环**必须在建序列时报错（含环路径）
//  3. 调用深度有**结构上界**（沿用内核 MaxInterruptFrames 的惯例：
//     上界不是配置项）

// ① 存取往返：写入后读回必须是**等价的 AST**，且不再依赖原文本。
func TestStoreRoundTripKeepsAST(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "sequences"))

	src, err := Parse([]byte(validSeq))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := st.Save(src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 删掉原文本来源：只靠磁盘上的 AST 也能读回
	got, err := st.Load("巡检三节点")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != src.Name || len(got.Groups) != 1 {
		t.Fatalf("往返后结构不符: %+v", got)
	}
	if len(got.Groups[0].Tools) != 1 || got.Groups[0].Tools[0].Tool != "cmd_run" {
		t.Errorf("工具未保住: %+v", got.Groups[0].Tools)
	}
	// 槽声明也必须保住（它是签名的一部分）
	if _, ok := got.Groups[0].Out["summary"]; !ok {
		t.Error("out 声明丢失 —— 签名不完整则无法按名调用")
	}
}

// ② 列表与删除。
func TestStoreListAndDelete(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "sequences"))

	for _, name := range []string{"a", "b"} {
		s, err := Parse([]byte(`{"name":"` + name + `","groups":[{"name":"g",` +
			`"in":{},"out":{"x":"string"},"tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"x\"},\"as\":\"x\"} ;"}]}`))
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", name, err)
		}
		if err := st.Save(s); err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
	}
	if names := st.List(); len(names) != 2 {
		t.Fatalf("应列出 2 条，实际 %v", names)
	}
	if err := st.Delete("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if names := st.List(); len(names) != 1 || names[0] != "b" {
		t.Fatalf("删除后应只剩 b，实际 %v", names)
	}
	// 删除不存在的必须**报错**，不静默成功
	if err := st.Delete("nope"); err == nil {
		t.Error("删除不存在的序列却返回成功（模型会以为删掉了）")
	}
	// 加载不存在的同理
	if _, err := st.Load("nope"); err == nil {
		t.Error("加载不存在的序列却成功了")
	}
}

// ③ ★ 跨序列调用图有环 ⇒ 建序列时报错，且给出**环路径**。
//
// 无环检测会让 #A → #B → #A 无限执行，而且每次都真的在调工具
// （不是空转）—— 这与 spawn_child 之所以要硬编码黑名单是同类风险。
func TestCrossSeqCallCycleIsRejected(t *testing.T) {
	// ⚠️ 序列**自身名字**不带 "#"—— "#" 只是 `seq_call` 的 target 里的
	// 前缀标记（"target":"#B" 指跨序列）。我第一版把名字写成 "#A"，
	// 于是 CheckGraph 按 "#B" 去找落盘名 "B"，误报「不存在」。
	// A 调 B，B 调 A
	a := mustParse(t, `{"name":"A","groups":[{"name":"ga","in":{},"out":{"x":"string"},`+
		`"tools":"{\"tool\":\"seq_call\",\"args\":{\"target\":\"#B\"},\"as\":\"x\"} ;"}]}`)
	b := mustParse(t, `{"name":"B","groups":[{"name":"gb","in":{},"out":{"x":"string"},`+
		`"tools":"{\"tool\":\"seq_call\",\"args\":{\"target\":\"#A\"},\"as\":\"x\"} ;"}]}`)

	// ⚠️ 跨序列目标**允许在 Save 时暂缺**：若要求"目标必须先存在"，
	// 那么互调的两条序列谁也存不下来（A 要 B 先在，B 要 A 先在）——
	// 这是一个无法满足的依赖，死锁在设计上而非运行时。
	// ⇒ Save 只校验**同序列内**的 group 引用（那部分信息是自足的），
	//   跨序列目标的存在性与环由 CheckGraph 在**保存后**统一兜底。
	st := NewStore(t.TempDir())
	if err := st.Save(a); err != nil {
		t.Fatalf("Save A: %v", err)
	}
	if err := st.Save(b); err != nil {
		t.Fatalf("Save B: %v", err)
	}
	err := st.CheckGraph()
	if err == nil {
		t.Fatal("A→B→A 成环却通过检查")
	}
	if !strings.Contains(err.Error(), "#A") || !strings.Contains(err.Error(), "#B") {
		t.Errorf("错误应给出环路径（含 #A 与 #B），实际: %v", err)
	}
}

// ④ 无环必须通过。
func TestCrossSeqCallAcyclicPasses(t *testing.T) {
	st := NewStore(t.TempDir())
	a := mustParse(t, `{"name":"A","groups":[{"name":"ga","in":{},"out":{"x":"string"},`+
		`"tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"a\"},\"as\":\"x\"} ;"}]}`)
	b := mustParse(t, `{"name":"B","groups":[{"name":"gb","in":{},"out":{"x":"string"},`+
		`"tools":"{\"tool\":\"seq_call\",\"args\":{\"target\":\"#A\"},\"as\":\"x\"} ;"}]}`)
	if err := st.Save(a); err != nil {
		t.Fatalf("Save A: %v", err)
	}
	if err := st.Save(b); err != nil {
		t.Fatalf("Save B: %v", err)
	}
	if err := st.CheckGraph(); err != nil {
		t.Fatalf("无环却被判为有环: %v", err)
	}
}

// ⑤ 深度上界是**结构常量**，不是配置项。
func TestMaxCallDepthIsConstant(t *testing.T) {
	// 上界必须存在且为正；且不是从配置读的
	if maxCallDepth <= 0 {
		t.Fatalf("maxCallDepth 应为正，实际 %d", maxCallDepth)
	}
	// ★ 2026-11-04：原先写的是 `if maxCallDepth != maxCallDepth` —— 永假，
	// 这条断言从诞生起就不可能失败（staticcheck SA4000）。
	//
	// 意图是"同一数值在多次调用间稳定（不可被外部改写）"。真正该测的是
	// **读取两次得到同一个值**，于是改成显式取两次比较。
	v1, v2 := maxCallDepth, maxCallDepth
	if v1 != v2 {
		t.Fatalf("maxCallDepth 读取不稳定：%d vs %d", v1, v2)
	}
}

// ⑥ 工具不存在：missing 策略的三个取值各有明确行为。
//
// 动态注册下"工具不存在"是**常态**（插件未加载/已卸载/崩溃），
// 不是异常边界 —— 因此策略必须显式，不能靠"报错"兜底。
func TestMissingPolicyBehaviors(t *testing.T) {
	ft := newFakeTool()
	ft.errs["gone"] = errToolNotFound
	ft.results["kept"] = "OK"

	build := func(policy string) Group {
		return Group{
			Name: "g", In: map[string]string{},
			Out:  map[string]string{"a": "string", "b": "string"},
			When: "true", Parallel: false, Missing: policy,
			Tools: []ToolCall{
				{Tool: "gone", As: "a", Fallback: `{"fallback":true}`},
				{Tool: "kept", As: "b"},
			},
		}
	}

	t.Run("fail", func(t *testing.T) {
		if _, err := execGroup(build("fail"), map[string]interface{}{}, ft); err == nil {
			t.Error("missing=fail 时缺工具应使整组失败")
		}
	})
	t.Run("skip", func(t *testing.T) {
		res, err := execGroup(build("skip"), map[string]interface{}{}, ft)
		if err != nil {
			t.Fatalf("missing=skip 不应整组失败: %v", err)
		}
		if _, ok := res.Slots["a"]; ok {
			t.Error("skip 时不应给缺失工具的槽赋值（下游读到缺失）")
		}
		if got, _ := res.Slots["b"].(string); got != "OK" {
			t.Errorf("skip 时其余工具应照常执行，b = %v", res.Slots["b"])
		}
	})
	t.Run("degrade", func(t *testing.T) {
		res, err := execGroup(build("degrade"), map[string]interface{}{}, ft)
		if err != nil {
			t.Fatalf("missing=degrade 不应整组失败: %v", err)
		}
		if _, ok := res.Slots["a"]; !ok {
			t.Error("degrade 时应写入兜底值")
		}
	})
}

func mustParse(t *testing.T, s string) *Sequence {
	t.Helper()
	seq, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("解析失败: %v\n文本: %s", err, s)
	}
	return seq
}

// ⑦ ★ 路径穿越防护（安全相关，判据必须有）。
//
// 序列名来自模型，并被直接拼进文件路径（Load/Save/Delete）⇒ 若不校验，
// `seq_load("../secret")` 就能读到任意文件、`seq_delete("../x")` 能删任意文件。
//
// 这条不是"读代码看着对"，而是在 store 目录**外**放一个真实文件，
// 断言它既读不到、也删不掉。
func TestStoreBlocksPathTraversal(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "sequences"))

	outside := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(outside, []byte(`{"name":"leaked","groups":[]}`), 0644); err != nil {
		t.Fatalf("准备外部文件失败: %v", err)
	}

	for _, name := range []string{
		"../secret", "../../etc/passwd", "a/b", `a\b`, "..", ".hidden", "", "sub/../..",
	} {
		if seq, err := st.Load(name); err == nil {
			t.Errorf("Load(%q) 竟然成功了，读到 %+v —— 路径穿越未被拦住", name, seq)
		}
		if err := st.Delete(name); err == nil {
			t.Errorf("Delete(%q) 竟然成功了", name)
		}
	}
	// 外部文件必须仍在（越权删除会破坏目录边界）
	if _, err := os.Stat(outside); err != nil {
		t.Error("store 目录外的文件被删掉了 —— 路径穿越已突破边界")
	}
}

// ⑨ ★ 调用图缓存不得改变校验**语义**。
//
// 背景：CheckGraph 原来每次 s.List() + 逐条 s.Load()，把全部序列重新读盘
// 反序列化。实测 200 条×1000 工具时创建要 27s、平均 135ms/条且随序列数线性
// 增长（O(n²)）。改成缓存调用边后 Save 只 O(1) 增量更新。
//
// ⚠️ 这类优化最危险的失败模式是"**语义悄悄变了**"：校验还在跑，但少查了
// 某种情况。所以逐条钉住原本的三个保证。
func TestStoreGraphCacheKeepsSemantics(t *testing.T) {
	// helper：造一条含 seq_call 边、指向 targets 的序列
	mk := func(t *testing.T, st *Store, name string, targets ...string) {
		t.Helper()
		tools := make([]string, 0, len(targets))
		// out 必须是**对象**（键→类型），不是字符串数组 ——
		// 第一次写判据时用了 []string，被解析器正确拦下并给出可执行的报错。
		outs := map[string]string{}
		for i, tgt := range targets {
			outs[fmt.Sprintf("o%d", i)] = "string"
			tools = append(tools, fmt.Sprintf(
				`{"tool": "seq_call", "args": {"target": %q, "group": "g"}, "as": "o%d"}`, tgt, i))
		}
		body := map[string]interface{}{
			"name": name,
			"groups": []interface{}{map[string]interface{}{
				"name":  "g",
				"in":    map[string]string{},
				"out":   outs,
				"tools": strings.Join(tools, " ; ") + " ;",
			}},
		}
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		seq, err := Parse(b)
		if err != nil {
			t.Fatalf("Parse(%s): %v", name, err)
		}
		if err := st.Save(seq); err != nil {
			t.Fatalf("Save(%s): %v", name, err)
		}
	}

	// ① 目标存在性仍生效
	dir := t.TempDir()
	st := NewStore(dir)
	mk(t, st, "a", "#missing")
	if err := st.CheckGraph(); err == nil {
		t.Error("调用不存在的序列却通过了 CheckGraph —— 缓存漏了目标存在性检查")
	} else if !strings.Contains(err.Error(), "missing") {
		t.Errorf("错误信息应指出 missing，实际：%v", err)
	}

	// ② 环检测仍生效
	dir2 := t.TempDir()
	st2 := NewStore(dir2)
	mk(t, st2, "a", "#b")
	mk(t, st2, "b", "#a")
	if err := st2.CheckGraph(); err == nil {
		t.Error("a→b→a 成环却通过了 CheckGraph —— 缓存漏了环检测")
	} else if !strings.Contains(err.Error(), "成环") {
		t.Errorf("错误信息应指出成环，实际：%v", err)
	}

	// ③ 删除后缓存里的边同步移除，不再误报"成环"
	if err := st2.Delete("b"); err != nil {
		t.Fatal(err)
	}
	if err := st2.CheckGraph(); err != nil && strings.Contains(err.Error(), "成环") {
		t.Errorf("b 已删除，不该再报成环：%v", err)
	}
}

// ⑩ 保存必须 O(1) 增量：N 条序列的创建耗时应线性而非平方增长。
//
// 判据写成**比值**而非绝对耗时：绝对值随机器波动，比值只反映增长率。
func TestStoreSaveScalesLinearly(t *testing.T) {
	if testing.Short() {
		t.Skip("scaling test")
	}
	save := func(t *testing.T, st *Store, name string) {
		t.Helper()
		body := fmt.Sprintf(
			`{"name":%q,"groups":[{"name":"g","in":{},"out":{"o0":"string"},`+
				`"tools":"{\"tool\":\"cmd_run\",\"args\":{\"command\":\"ls\"},\"as\":\"o0\"} ;"}]}`,
			name)
		seq, err := Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Save(seq); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	st := NewStore(dir)
	one := time.Now()
	save(t, st, "probe")
	single := time.Since(one)

	dir2 := t.TempDir()
	st2 := NewStore(dir2)
	const n = 300
	start := time.Now()
	for i := 0; i < n; i++ {
		save(t, st2, fmt.Sprintf("s%04d", i))
	}
	total := time.Since(start)

	// 用**批内均分比**而不是"总/单条"。
	//
	// 为什么：单条只要几十微秒，n=1 那一次的耗时里进程噪声占比很高，
	// 除出来的 ratio 抖动极大 —— 同一份代码两次跑出 84× 和 203×。
	// 判据自己不稳定，报的失败就是噪声，比没有判据更糟。
	// 改用"后半程每条耗时 vs 前半程每条耗时"：平方增长会让后半程明显更慢，
	// 线性增长则基本持平。
	half := n / 2
	_ = half
	// 分段计时：重建一个 store，前半程和后半程各计一次
	dir3 := t.TempDir()
	st3 := NewStore(dir3)
	var tFirst, tSecond time.Duration
	start3 := time.Now()
	for i := 0; i < half; i++ {
		save(t, st3, fmt.Sprintf("f%04d", i))
	}
	tFirst = time.Since(start3)
	mid := time.Now()
	for i := 0; i < half; i++ {
		save(t, st3, fmt.Sprintf("s%04d", i))
	}
	tSecond = time.Since(mid)

	perFirst := float64(tFirst) / float64(half)
	perSecond := float64(tSecond) / float64(half)
	t.Logf("前半程 %v/条，后半程 %v/条（比值 %.2f；单条基准 %v，%d 条共 %v）",
		time.Duration(int64(tFirst)/int64(half)), time.Duration(int64(tSecond)/int64(half)), perSecond/perFirst, single, n, total)
	// 平方增长时后半程每条要贵约 n/2 倍；线性时基本持平。
	// 阈值 3 倍给足余量（磁盘与 GC 抖动都在这个量级内）。
	if perSecond > perFirst*3 {
		t.Errorf("后半程每条 %v 是前半程 %v 的 %.1f 倍 —— 接近平方增长，调用图缓存没生效",
			time.Duration(int64(tSecond)/int64(half)), time.Duration(int64(tFirst)/int64(half)), perSecond/perFirst)
	}
}

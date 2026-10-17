package kernelboot

// 依赖图制品化的判据（2026-10-16）。
//
// 设计见 docs/zh/kernel-modularization-design.md §2.5。
//
// 这组判据的重心是**两个不同 hash 各防一类故障**（§2.5.2）：
//
//	① 图内自校验  ⇒ 防「图文件损坏」
//	② 内嵌 hash   ⇒ 防「图与二进制不匹配」
//
// ★ 只测 ① 是不够的：图文件自身完好、自校验也过，但它描述的是**旧代码**
// ⇒ 按旧图调度 ⇒ 顺序错 ⇒ 正是本设计要消灭的病 A。
// 所以 ② 必须有独立判据，且要证明它**真的会拦住不匹配**。

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleEdges() []GraphEdge {
	return []GraphEdge{
		{Name: "security", Provides: []string{"crypto", "keystore"}},
		{Name: "config", Provides: []string{"config"}, Requires: []string{"crypto"}},
		{Name: "llm", Provides: []string{"llm"}, Requires: []string{"config", "keystore"}},
	}
}

// ★★★ 判据：编解码**往返一致**（含嵌套依赖的形状）。
func TestGraphEncodeDecodeRoundTrip(t *testing.T) {
	in := sampleEdges()
	data := EncodeGraph(in)
	gf, err := DecodeGraph(data)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(gf.Modules) != len(in) {
		t.Fatalf("模块数不符: %d vs %d", len(gf.Modules), len(in))
	}
	// 按名字比对（编码时排序了，所以顺序可能不同 —— 这是刻意的）
	byName := map[string]GraphEdge{}
	for _, m := range gf.Modules {
		byName[m.Name] = m
	}
	for _, want := range in {
		got, ok := byName[want.Name]
		if !ok {
			t.Fatalf("模块 %s 丢失", want.Name)
		}
		if strings.Join(got.Provides, ",") != strings.Join(sortedCopy(want.Provides), ",") {
			t.Errorf("%s 的 Provides 不符: %v vs %v", want.Name, got.Provides, want.Provides)
		}
		if strings.Join(got.Requires, ",") != strings.Join(sortedCopy(want.Requires), ",") {
			t.Errorf("%s 的 Requires 不符: %v vs %v", want.Name, got.Requires, want.Requires)
		}
	}
}

// ★★★ 判据：**同一份声明产出逐字节相同的结果**（hash 稳定的前提）。
//
// 为何必须独立判据：如果顺序影响字节，hash 在无害改动（代码里调换两个模块）
// 后也会变 ⇒ "每次构建都说不一致" ⇒ 人开始无视告警 ⇒ hash 失去意义。
func TestGraphEncodingIsOrderIndependent(t *testing.T) {
	a := sampleEdges()
	// 打乱顺序（含 Provides/Requires 数组内部顺序）
	b := []GraphEdge{
		{Name: "llm", Provides: []string{"llm"}, Requires: []string{"keystore", "config"}},
		{Name: "config", Provides: []string{"config"}, Requires: []string{"crypto"}},
		{Name: "security", Provides: []string{"keystore", "crypto"}},
	}
	da, db := EncodeGraph(a), EncodeGraph(b)
	if !bytesEqual(da, db) {
		t.Fatalf("★ 声明顺序不同导致字节不同（hash 会漂移，告警失去意义）\n"+
			"a=%x\nb=%x", da[:32], db[:32])
	}
	if GraphDigest(a) != GraphDigest(b) {
		t.Fatalf("★ digest 随声明顺序变化: %s vs %s", GraphDigest(a), GraphDigest(b))
	}
}

// ★★★ **关键判据 ①**：图文件损坏必须被拦。
func TestGraphDetectsFileCorruption(t *testing.T) {
	data := EncodeGraph(sampleEdges())

	// ① 位翻转（模拟磁盘错误）
	flipped := append([]byte(nil), data...)
	flipped[len(flipped)/2] ^= 0x01
	if _, err := DecodeGraph(flipped); err == nil {
		t.Fatal("★ 位翻转未被检出（自校验失效）")
	}

	// ② 截断（模拟传输不完整）
	if _, err := DecodeGraph(data[:len(data)-4]); err == nil {
		t.Fatal("★ 截断未被检出")
	}
	// ③ 尾部多余字节
	if _, err := DecodeGraph(append(append([]byte(nil), data...), 0xAA)); err == nil {
		t.Fatal("★ 尾部残留未被检出")
	}
	// ④ 空文件 / 过短
	for _, n := range []int{0, 8, 16} {
		if _, err := DecodeGraph(data[:n]); err == nil {
			t.Errorf("★ 长度为 %d 的输入未被拒", n)
		}
	}
}

// ★★★ **关键判据 ②**：格式标识/版本不符必须被拦（防来自不兼容版本的图）。
func TestGraphRejectsWrongMagicAndVersion(t *testing.T) {
	data := EncodeGraph(sampleEdges())
	// 改 magic 后重算自校验（模拟"图是完好的，但来自别的格式版本"）
	bad := append([]byte(nil), data...)
	copy(bad[0:8], []byte("HGDEPS99"))
	// 重算末尾 hash 让它"自校验通过"，从而**只有版本检查能拦住它**
	fixSelfHash(t, bad)
	if _, err := DecodeGraph(bad); err == nil {
		t.Fatal("★ 格式标识不符未被检出")
	} else if !strings.Contains(err.Error(), "格式标识") && !strings.Contains(err.Error(), "版本") {
		t.Errorf("错误应点明格式/版本问题: %v", err)
	}
}

// ★★★ 判据：损坏的长度字段不得导致巨额分配（解析器对不可信输入要有界）。
func TestGraphRejectsAbsurdLengths(t *testing.T) {
	// 手工构造：合法头部 + 一个超大长度
	//
	// ★ 必须构造成"自校验能通过"的样子，才能证明**长度检查**本身在起作用
	// （否则是自校验先拦下，判据测不到长度检查）。
	var b []byte
	b = append(b, []byte(graphMagic)...)
	b = append(b, 1, 0, 0, 0)          // version = 1
	b = append(b, 1, 0, 0, 0)          // modCount = 1
	b = append(b, 0xFF, 0xFF)          // nameLen = 65535（超过 maxFieldLen=4096）
	b = append(b, make([]byte, 32)...) // 自校验 hash 占位（下面重算）
	fixSelfHash(t, b)
	if _, err := DecodeGraph(b); err == nil {
		t.Fatal("★ 超大字段长度未被拒（可能造成巨额分配）")
	}
}

// ★★★ **最重要的判据**：图与二进制**不配套**必须被拦（§2.5.2 的情况 2）。
//
// 这是本设计的核心保护：图文件自身完好、自校验也过（所以 ① 拦不住），
// 但它描述的是**旧代码** ⇒ 按旧图调度 ⇒ 顺序错 = 病 A。
func TestGraphMismatchWithBinaryIsRejected(t *testing.T) {
	// 实际产出的图（当前代码）
	actual := EncodeGraph(sampleEdges())
	gf, err := DecodeGraph(actual)
	if err != nil {
		t.Fatal(err)
	}

	// 二进制内嵌的是**另一份声明**的 hash（模拟"改了代码但没重出图"）
	oldCode := []GraphEdge{
		{Name: "security", Provides: []string{"crypto"}},
		{Name: "config", Provides: []string{"config"}, Requires: []string{"crypto"}},
		// 注意：llm 模块当时还不存在
	}
	staleHash := GraphDigest(oldCode)

	err = CheckAgainstBinary(gf, staleHash)
	if err == nil {
		t.Fatal("★ 图与二进制不配套却通过了校验 —— 会按旧图调度（病 A 复发）")
	}
	// 错误必须给出**可执行的修复方向**
	for _, want := range []string{"不配套", "重新"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应含 %q 以指导修复: %v", want, err)
		}
	}
	// 且必须同时打印两个 hash（便于人工核对是哪边旧了）
	if !strings.Contains(err.Error(), gf.Digest[:8]) || !strings.Contains(err.Error(), staleHash[:8]) {
		t.Errorf("错误应同时给出两个 hash 的前缀: %v", err)
	}

	// 配套时必须通过
	if err := CheckAgainstBinary(gf, gf.Digest); err != nil {
		t.Errorf("配套的图不该被拒: %v", err)
	}
}

// ★★★ 判据：**开发环境**（未注入 hash）不做配套校验，但**生产**（已注入）缺图即错。
//
// 为何要区分：单测/`go run` 不可能先跑生成器；
// 而生产缺图若静默回退到"运行期重建"，就把 §2.5.2 要防的风险变成静默容忍。
func TestGraphPresencePolicyByEnvironment(t *testing.T) {
	gf, _ := DecodeGraph(EncodeGraph(sampleEdges()))

	// ① 开发：未注入 hash ⇒ 跳过校验（无论图在不在）
	if err := CheckAgainstBinary(gf, ""); err != nil {
		t.Errorf("开发环境（未注入 hash）不该做配套校验: %v", err)
	}
	if err := CheckAgainstBinary(nil, ""); err != nil {
		t.Errorf("开发环境缺图不该报错: %v", err)
	}

	// ② 生产：已注入 hash ⇒ 缺图必须报错（不许回退到重建）
	err := CheckAgainstBinary(nil, "deadbeefdeadbeef")
	if err == nil {
		t.Fatal("★ 生产环境（已注入 hash）缺图却通过了 —— 会静默回退到运行期重建")
	}
	if !strings.Contains(err.Error(), "生产") && !strings.Contains(err.Error(), "回退") {
		t.Errorf("错误应点明不允许回退: %v", err)
	}
}

// ★★★ 判据：**落盘后读回**（真实文件 IO 路径，不只是内存往返）。
func TestGraphFileRoundTripOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, GraphFileName)

	want := EncodeGraph(sampleEdges())
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gf, err := DecodeGraph(got)
	if err != nil {
		t.Fatalf("从磁盘读回后解码失败: %v", err)
	}
	if len(gf.Modules) != 3 {
		t.Errorf("模块数不符: %d", len(gf.Modules))
	}
	// 文件命名必须与文档一致（生产靠这个名字找图）
	if GraphFileName != "depgraph.bin" {
		t.Errorf("制品文件名应为 depgraph.bin（文档 §2.5.4），实际 %s", GraphFileName)
	}
}

// ★★★ 判据：嵌套依赖的形状在制品里**保真**（用户的明确要求）。
//
// 场景：llm 依赖 config，config 依赖 crypto（crypto 由 security 提供）
// ⇒ 制品必须完整保留这条链，而不是被"展平"或丢失中间层。
func TestGraphPreservesNestedDependencyChain(t *testing.T) {
	edges := []GraphEdge{
		{Name: "security", Provides: []string{"crypto"}},
		{Name: "config", Provides: []string{"config"}, Requires: []string{"crypto"}},
		{Name: "llm", Provides: []string{"llm"}, Requires: []string{"config"}},
		{Name: "agent", Provides: []string{"agent"}, Requires: []string{"llm"}},
	}
	gf, err := DecodeGraph(EncodeGraph(edges))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]GraphEdge{}
	for _, m := range gf.Modules {
		byName[m.Name] = m
	}
	// 逐环核对链条完整性
	if got := byName["config"].Requires; len(got) != 1 || got[0] != "crypto" {
		t.Errorf("config 的依赖链断裂: %v", got)
	}
	if got := byName["llm"].Requires; len(got) != 1 || got[0] != "config" {
		t.Errorf("llm 的依赖链断裂: %v", got)
	}
	if got := byName["agent"].Requires; len(got) != 1 || got[0] != "llm" {
		t.Errorf("agent 的依赖链断裂: %v", got)
	}
}

// fixSelfHash 重算末尾的 32 字节自校验 hash，让"非法内容但自校验通过"成为可能
// （用于测试**格式检查**而不是 hash 检查）。
//
// 前提：data 长度 >= 32（即已有 32 字节 hash 尾巴）。用例需自行保证。
func fixSelfHash(t *testing.T, data []byte) {
	t.Helper()
	if len(data) < 32 {
		t.Fatalf("判据构造错误：payload 太短（%d 字节），无法容纳 32 字节自校验 hash", len(data))
	}
	payload := data[:len(data)-32]
	sum := sha256.Sum256(payload)
	copy(data[len(data)-32:], sum[:])
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

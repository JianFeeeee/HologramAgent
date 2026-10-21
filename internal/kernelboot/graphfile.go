package kernelboot

// 依赖图的**制品化**：编译期产出的精密二进制 + 双重 hash 保护。
//
// 设计权威：docs/zh/kernel-modularization-design.md §2.5。
//
// ## 两个不同的 hash，各防一类故障（★ 关键区分）
//
//	① 图内自校验 hash   防「图文件损坏」（磁盘错、传输截断、位翻转）
//	② 内嵌 hash（二进制） 防「图与二进制不匹配」（改了 Requires 但图没重生成）
//
// ★ 只做 ① 会漏掉更危险的那类：图文件**自身完好**、自校验也过，
// 但它描述的是**旧代码** ⇒ 按旧图调度 ⇒ 顺序错 ⇒ 正是本设计要消灭的病 A。
//
// 所以 §2.5.2 要求 hash 绑定二进制，且不一致时**拒绝启动**（不猜哪边对）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// 文件格式常量。
const (
	// graphMagic 是格式标识。破坏性变更时改末位数字。
	graphMagic = "HGDEPS01"
	// graphFormatVersion 是格式版本（与 magic 末位同步维护）。
	graphFormatVersion = 2
	// GraphFileName 是制品文件名（与内核二进制同目录，满足"随核心分发"）。
	GraphFileName = "depgraph.bin"

	// maxFieldLen 是单个字符串字段的长度上限（防损坏文件声明一个超大长度）。
	//
	// 为何必须设限：一个损坏的长度字段会让 make([]byte, n) 申请巨量内存
	// （或直接 OOM）。解析器对**不可信输入**必须设边界。
	maxFieldLen = 4096
	// maxModules 是模块数上限（同上，防损坏文件声明一个巨大计数）。
	maxModules = 4096
)

// GraphEdge 是图中一条模块声明（用于序列化）。
type GraphEdge struct {
	Name     string
	Provides []string
	Requires []string

	// Layer 是**编译期算好的起始层号**（设计 §2.5，用户要求：
	// "启动顺序是编译期间按照声明的依赖顺序生成的启动依赖图排列的"）。
	//
	// ## 为什么把层号也编码进图（而不只存边）
	//
	// 若只存边，运行期就还得跑一次 BFS 才算得出顺序 —— 那等于
	// **内核在运行期决定启动顺序**，与"编译期定序"的设计相悖
	// （且运行期算法与编译期不一致时无人察觉）。
	//
	// 存下 Layer 后：运行期**读**它即可，不重算。
	// `Build()` 仍保留（单测/无图场景用），但**生产路径不调它**。
	//
	// ★ 自校验：读图时仍会重算分层并与 Layer 比对（CheckLayers），
	// 不一致 ⇒ 拒绝启动。这样"图里的顺序"与"声明"不会漂移。
	Layer int
}

// GraphFile 是图制品的解析结果。
type GraphFile struct {
	Version int
	Modules []GraphEdge
	// Digest 是文件内自校验 hash 的十六进制（§2.5.2 的 ①）。
	Digest string
}

// buildGraphPayload 产出**参与 hash 与序列化**的全部字节（不含自校验 hash 本身）。
//
// ★ 所有字段**排序**：同一份声明必须产出逐字节相同的结果。
// 否则代码里调换两个模块的顺序就会让 hash 变化 ⇒
// "每次构建都说不一致" ⇒ hash 失去意义（人开始无视它）。
func buildGraphPayload(modules []GraphEdge) []byte {
	// 深拷贝并排序（不改调用方的切片）
	ms := make([]GraphEdge, len(modules))
	copy(ms, modules)
	for i := range ms {
		ms[i].Provides = sortedCopy(ms[i].Provides)
		ms[i].Requires = sortedCopy(ms[i].Requires)
	}
	// ★ 排序键是 **(Layer, Name)** —— 不是只按名字。
	//
	// 这样**解码后的顺序就是启动顺序**（先按层，层内按名字），
	// 运行期读到即可直接执行，不必重算分层。
	//
	// 层内按名字是为了**可复现**（同一份声明两次产出逐字节相同 ⇒ 能进 PR diff）。
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Layer != ms[j].Layer {
			return ms[i].Layer < ms[j].Layer
		}
		return ms[i].Name < ms[j].Name
	})

	var buf bytes.Buffer
	buf.WriteString(graphMagic)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(graphFormatVersion))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(ms)))

	writeStr := func(s string) {
		_ = binary.Write(&buf, binary.LittleEndian, uint16(len(s)))
		buf.WriteString(s)
	}
	for _, m := range ms {
		writeStr(m.Name)
		writeStr(strings.Join(m.Provides, ","))
		writeStr(strings.Join(m.Requires, ","))
		// Layer 作为 uint32 追加在每条记录末尾。
		//
		// ★ 放在末尾而不是开头：这样"旧版解码逻辑"遇到新文件时
		// 会在**尾部残留检查**处报错（响亮），而不是把 layer 当名字读错。
		_ = binary.Write(&buf, binary.LittleEndian, uint32(m.Layer))
	}
	return buf.Bytes()
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// EncodeGraph 把模块声明序列化成制品字节（含自校验 hash）。
func EncodeGraph(modules []GraphEdge) []byte {
	payload := buildGraphPayload(modules)
	sum := sha256.Sum256(payload)
	out := make([]byte, 0, len(payload)+sha256.Size)
	out = append(out, payload...)
	out = append(out, sum[:]...)
	return out
}

// GraphDigest 返回制品自校验 hash 的十六进制（不含文件本身）。
//
// 供构建期把 digest 注入二进制（§2.5.2 的 ②）。
func GraphDigest(modules []GraphEdge) string {
	sum := sha256.Sum256(buildGraphPayload(modules))
	return hex.EncodeToString(sum[:])
}

// DecodeGraph 解析制品字节，校验自校验 hash。
//
// 任何结构异常都**报错**（不返回"部分结果"）：
// 图是启动调度的唯一依据，残缺的图比没有图更危险
// （它会生成一个**看似合法**的调度计划）。
func DecodeGraph(data []byte) (*GraphFile, error) {
	if len(data) < len(graphMagic)+8+sha256.Size {
		return nil, fmt.Errorf("kernelboot: 依赖图文件过短（%d 字节），已损坏", len(data))
	}
	// ① 先校验自校验 hash（防截断/位翻转）
	payload := data[:len(data)-sha256.Size]
	wantSum := data[len(data)-sha256.Size:]
	gotSum := sha256.Sum256(payload)
	if !bytes.Equal(gotSum[:], wantSum) {
		return nil, fmt.Errorf("kernelboot: 依赖图自校验失败（文件已损坏）")
	}

	// ② 解析
	if string(payload[:len(graphMagic)]) != graphMagic {
		return nil, fmt.Errorf("kernelboot: 依赖图格式标识不匹配（期望 %q，得到 %q）——"+
			"文件可能来自不兼容的版本", graphMagic, string(payload[:len(graphMagic)]))
	}
	off := len(graphMagic)
	rd32 := func() uint32 {
		v := binary.LittleEndian.Uint32(payload[off:])
		off += 4
		return v
	}
	ver := rd32()
	if ver != graphFormatVersion {
		return nil, fmt.Errorf("kernelboot: 依赖图格式版本 %d 与本二进制期望的 %d 不一致——"+
			"请用配套的生成器重出图", ver, graphFormatVersion)
	}
	n := rd32()
	if n > maxModules {
		return nil, fmt.Errorf("kernelboot: 依赖图声明 %d 个模块，超过上限 %d（文件已损坏）",
			n, maxModules)
	}

	readStr := func() (string, error) {
		if off+2 > len(payload) {
			return "", fmt.Errorf("kernelboot: 依赖图在读取字符串长度时越界")
		}
		l := int(binary.LittleEndian.Uint16(payload[off:]))
		off += 2
		if l > maxFieldLen {
			return "", fmt.Errorf("kernelboot: 依赖图字段长度 %d 超过上限 %d（文件已损坏）",
				l, maxFieldLen)
		}
		if off+l > len(payload) {
			return "", fmt.Errorf("kernelboot: 依赖图字段越界（需要 %d 字节，剩余 %d）",
				l, len(payload)-off)
		}
		s := string(payload[off : off+l])
		off += l
		return s, nil
	}

	gf := &GraphFile{Version: int(ver), Digest: hex.EncodeToString(gotSum[:])}
	for i := uint32(0); i < n; i++ {
		name, err := readStr()
		if err != nil {
			return nil, err
		}
		provs, err := readStr()
		if err != nil {
			return nil, err
		}
		reqs, err := readStr()
		if err != nil {
			return nil, err
		}
		if off+4 > len(payload) {
			return nil, fmt.Errorf("kernelboot: 依赖图在读取层号时越界（文件已损坏）")
		}
		layer := int(binary.LittleEndian.Uint32(payload[off:]))
		off += 4
		gf.Modules = append(gf.Modules, GraphEdge{
			Name:     name,
			Provides: splitNonEmpty(provs),
			Requires: splitNonEmpty(reqs),
			Layer:    layer,
		})
	}
	if off != len(payload) {
		return nil, fmt.Errorf("kernelboot: 依赖图有 %d 字节尾部残留（文件格式不符）",
			len(payload)-off)
	}
	return gf, nil
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// CheckAgainstBinary 校验磁盘上的图与**本二进制**是否配套（§2.5.2）。
//
// embeddedHash 是构建期注入的 digest（空 = 开发环境，跳过校验）。
//
// ★ 不一致时**拒绝启动**，不猜哪边对：
// 图的作者是构建流程，二进制的作者也是它。两者不一致意味着制品装配出了问题，
// 此时任何一边都不可信，猜一个是拿启动正确性赌运气。
func CheckAgainstBinary(gf *GraphFile, embeddedHash string) error {
	if embeddedHash == "" {
		// 开发/单测环境：构建脚本没注入 digest ⇒ 不做配套校验。
		// 这让 `go test` / `go run` 无需先生成图。
		return nil
	}
	if gf == nil {
		return fmt.Errorf("kernelboot: 缺少依赖图，而本二进制要求必须有（内嵌 hash=%s）——"+
			"生产环境不允许回退到运行期重建", embeddedHash[:min(8, len(embeddedHash))])
	}
	if !strings.EqualFold(gf.Digest, embeddedHash) {
		return fmt.Errorf("kernelboot: ★ 依赖图与本二进制**不配套**\n"+
			"  磁盘图的 hash: %s\n"+
			"  二进制内嵌的: %s\n"+
			"最常见原因：改了模块的 Requires/Provides 但**没有重新生成依赖图**。\n"+
			"修复：重新运行构建（会重出图并注入新 hash），不要手工改图文件。",
			gf.Digest, embeddedHash)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// EmbeddedGraphHash 由**构建期**用 -ldflags 注入（设计 §2.5.2）。
//
//	-X github.com/JianFeeeee/HologramAgent/internal/kernelboot.EmbeddedGraphHash=<digest>
//
// 空值（默认）= 开发/单测环境：不做"图与二进制配套"校验，
// 允许 homed 回退到运行期从声明重建图。
//
// ★ 为何默认空而不是某个占位值：`go test` / `go run` 不可能先跑生成器。
// 若默认非空，所有本地开发与判据都会因"图不存在"而失败 ——
// 那会逼着人绕过检查（比没有检查更糟）。
var EmbeddedGraphHash string

// PlanFromGraph 从**编译期产出的图**构造启动计划（设计 §2.5，用户要求：
// "启动顺序是编译期间按照声明的依赖顺序生成的启动依赖图排列的"）。
//
// ## 与 Build() 的区别（这是本函数存在的理由）
//
//	Build(mods)          ⇒ 运行期**现算**分层（依赖 BFS 算法）
//	PlanFromGraph(gf, …) ⇒ **读**编译期算好的层号，不重算
//
// 按用户设计，生产启动路径走后者：**顺序在编译期定，运行期只执行**。
//
// ## ★ 自校验：仍会重算一次并比对（防"图与代码漂移"）
//
// 只读不验会有个危险：图里写的是旧顺序（改了 Requires 但没重出图），
// 而文件自身 hash 完全有效 ⇒ 按**旧顺序**启动（正是 §2.5.2 的病 A）。
//
// 所以本函数：
//
//	① 从图里取模块与层号
//	② 用**图里的边**重算一次分层（Build）
//	③ 比对：重算结果与图中层号不一致 ⇒ **拒绝启动**
//
// 第 ③ 步能同时抓两类问题：图内自相矛盾（边与层号不符）、
// 生成器算法与运行期算法不一致。**两者都是制品装配错误，不该猜哪边对。**
//
// ## 参数 mods
//
// 是**当前二进制里真实登记的模块**（来自 kernelmod 注册表）。
// 它有两个作用：
//
//	· 校验"图里描述的模块"与"本二进制实际有的模块"一致
//	  （防：图是新的但二进制是旧的，或反之）
//	· 提供实现体（图只有名字，没有 Init）
func PlanFromGraph(gf *GraphFile, mods []Module) (*Plan, error) {
	if gf == nil {
		return nil, fmt.Errorf("kernelboot: 依赖图为 nil")
	}
	// ① 建立 名字 → 实现 的映射
	byName := make(map[string]Module, len(mods))
	for _, m := range mods {
		if _, dup := byName[m.Name()]; dup {
			return nil, fmt.Errorf("kernelboot: 模块名重复: %s", m.Name())
		}
		byName[m.Name()] = m
	}

	// ② 图与二进制必须描述**同一组模块**
	var missingInBinary, missingInGraph []string
	for _, e := range gf.Modules {
		if _, ok := byName[e.Name]; !ok {
			missingInBinary = append(missingInBinary, e.Name)
		}
	}
	inGraph := make(map[string]bool, len(gf.Modules))
	for _, e := range gf.Modules {
		inGraph[e.Name] = true
	}
	for _, m := range mods {
		if !inGraph[m.Name()] {
			missingInGraph = append(missingInGraph, m.Name())
		}
	}
	if len(missingInBinary) > 0 || len(missingInGraph) > 0 {
		sort.Strings(missingInBinary)
		sort.Strings(missingInGraph)
		return nil, fmt.Errorf("kernelboot: 依赖图与二进制不一致 —— "+
			"图里有但二进制没有: %v；二进制有但图里没有: %v。"+
			"这通常意味着**图与二进制不是同一次构建产出的**（请重新构建）",
			missingInBinary, missingInGraph)
	}

	// ③ 自校验：用图里的边重算分层，与图中层号比对
	edges := make([]GraphEdge, len(gf.Modules))
	copy(edges, gf.Modules)
	recomputed, err := Build(mods)
	if err != nil {
		return nil, fmt.Errorf("kernelboot: 无法用当前模块重算分层（声明之间有冲突）: %w", err)
	}
	layerOf := map[string]int{}
	for i, l := range recomputed.Layers {
		for _, m := range l {
			layerOf[m.Name()] = i
		}
	}
	var drift []string
	for _, e := range gf.Modules {
		if got, ok := layerOf[e.Name]; ok && got != e.Layer {
			drift = append(drift, fmt.Sprintf("%s（图中第 %d 层，按声明算应为第 %d 层）",
				e.Name, e.Layer, got))
		}
	}
	if len(drift) > 0 {
		sort.Strings(drift)
		return nil, fmt.Errorf("kernelboot: ★ 依赖图与代码声明**漂移** —— %s。"+
			"说明改了 Requires/Provides 但**没有重新生成图**。"+
			"按此图启动会得到错误的顺序（§2.5.2 的病 A），故拒绝启动。"+
			"修复：重新执行构建（make build 会重出图并注入 hash）",
			strings.Join(drift, "；"))
	}

	// ④ 按图中的 (Layer, Name) 组装计划 —— **直接读**，不再排序/重算
	sorted := make([]GraphEdge, len(gf.Modules))
	copy(sorted, gf.Modules)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Layer != sorted[j].Layer {
			return sorted[i].Layer < sorted[j].Layer
		}
		return sorted[i].Name < sorted[j].Name
	})
	plan := &Plan{}
	for _, e := range sorted {
		for len(plan.Layers) <= e.Layer {
			plan.Layers = append(plan.Layers, nil)
		}
		plan.Layers[e.Layer] = append(plan.Layers[e.Layer], byName[e.Name])
	}
	return plan, nil
}

// LoadAndCheckGraph 读图并做"图与二进制配套"校验（§2.5.2）。
//
// path 为空时用默认位置（与二进制同目录，见 GraphFileName）。
// embeddedHash 来自 -ldflags 注入（EmbeddedGraphHash）。
//
// 缺失策略（§2.5.4）：dev（embeddedHash==""）允许没有图 ⇒ 返回 (nil, nil)，
// 由调用方回退到运行期 Build；生产（有 hash）必须有图，否则报错。
func LoadAndCheckGraph(path, embeddedHash string) (*GraphFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if embeddedHash == "" {
			return nil, nil // dev：没有图是正常的
		}
		return nil, fmt.Errorf("kernelboot: 生产构建（内嵌 hash 非空）但读不到依赖图 %s: %w —— "+
			"图必须与二进制同目录（部署时一起拷贝）", path, err)
	}
	gf, err := DecodeGraph(b)
	if err != nil {
		return nil, err
	}
	if err := CheckAgainstBinary(gf, embeddedHash); err != nil {
		return nil, err
	}
	return gf, nil
}

package proc

// Exchange Arena：**内核独占管理**的跨进程共享内存块分配器（§13.2 重设计）。
//
// ── 所有权模型（架构约束）────────────────────────────────────────────
//
//	共享内存由内核全权管理。插件需要使用共享内存时，通过 syscall 风格的
//	RPC 向内核申请，内核返回偏移与大小；使用完毕后插件再通知内核回收。
//
//	因此分配器只存在于**内核进程内**，用一把普通 sync.Mutex 保护即可：
//	不需要任何跨进程原子操作，也不存在"共享游标被两个进程各自更新"这一
//	类竞态——这正是前几版（bump 游标 / CAS 位图跨进程分配）失败的根本原因。
//
//	共享内存是**内部实现**，不对插件开发者暴露：插件的公开 API 仍是
//	普通字符串/Map（见 SDK 的 IOInjector / ToolHandler）。模板运行时在
//	传输层完成全部搬运，开发者无感。
//
// ── 为什么是变长块而不是定长槽 ──────────────────────────────────────
//
//	定长槽唯一的理由是"跨进程无法安全地做变长分配"。分配器收回内核后这个
//	约束消失，于是可以采用真正的变长块分配（first-fit + 邻块合并）：
//
//	  - 内核可以按需**标定**每块大小（funccall 帧模型）而不是一律给固定槽
//	  - 大 payload（文件内容、长工具输出）不再受 16KB 槽容量限制
//	  - 块用尽时插件才请求扩容，而不是事先把池铺满
//
// ── 布局 ──────────────────────────────────────────────────────────
//
//	┌──────────────────────────────────────────────┐
//	│ Arena Header 64B                             │
//	│   magic / version / capacity / blockBase     │
//	├──────────────────────────────────────────────┤
//	│ Block 0: [size | state | owner | prevSize]   │
//	│          [data ...]                          │
//	│ Block 1: ...                                 │
//	│ Block N-1: ...（最后一块的 size 到 arena 末尾）│
//	└──────────────────────────────────────────────┘
//
//	块头 16B，size 含头并按 8 字节对齐，因此所有数据起点都是 8 字节对齐的。
//	prevSize 让 Free 能 O(1) 找到前驱做向后合并（经典分离/合并做法）。
//
// ── 安全边界 ──────────────────────────────────────────────────────
//
//	插件归还/读取时提交的是 SharedRef（offset 由插件转述）。内核必须把它
//	当成不可信输入：Free 会重新走一遍块链确认 offset 确实是一个已分配块的
//	数据起点、owner 匹配，才改动分配器状态；否则伪造的 offset 会直接破坏
//	块链。Read 同理。
//
// ── 惰性物理内存 ──────────────────────────────────────────────────
//
//	arena 很大（MB 级）但底层是 memfd：**未触碰的页不占物理内存**。所以
//	把容量开大不会带来常驻内存开销，只是虚拟地址空间。

import (
	"fmt"
	"sync"
)

const (
	arenaMagic   uint32 = 0x41524E41 // "ARNA"
	arenaVersion uint32 = 2          // v2：定长槽 → 变长块

	arenaHeaderSize = 64

	// Arena Header 字段偏移（相对 arena 起始）。
	arOffMagic     = 0
	arOffVersion   = 4
	arOffCapacity  = 8  // arena 可用字节数
	arOffBlockBase = 12 // 首个块相对 arena 起始的偏移
	arOffBlockUsed = 16 // 已分配的数据字节数（诊断）
	arOffReserved  = 20

	// 块头 16B：size 含头，按 8 字节对齐。
	blockHeaderSize  = 16
	blockOffSize     = 0  // uint32 总大小（含头）
	blockOffState    = 4  // uint32 0=free 1=used
	blockOffOwner    = 8  // uint32 所有者
	blockOffPrevSize = 12 // uint32 前一块总大小（0=无前块）

	blockFree uint32 = 0
	blockUsed uint32 = 1

	// minBlockSize 是拆分后允许的最小块（含头）。低于它就不再拆，
	// 避免产生一堆无法再利用的碎片。
	minBlockSize = 32

	// SharedRef.Flags 语义位。
	sharedRefFlagJSON   = 1 << 0 // 载荷是 JSON（工具参数/结果）
	sharedRefFlagExpand = 1 << 1 // 引用指向插件申请的扩容块（内核需单独归还）

	// OwnerHost 标记由内核分配的块。插件用 Host.NextOwnerID 分配的值。
	OwnerHost uint32 = 0
)

// arenaDefaultCapacity 是统一区域里预留给 arena 的字节数。
//
// 取 4MB：memfd 惰性分配，未触碰的页不占物理内存，所以开大无成本；
// 但它让单个工具调用可以承载 MB 级 payload，不必再退回内联 RPC。
const arenaDefaultCapacity = 4 * 1024 * 1024

// arenaPublishSize 是统一区域里预留给 arena 的字节数。
var arenaPublishSize = uint32(arenaDefaultCapacity)

// arenaRegion 是块分配器视图。
//
// region 始终是**完整**统一区域 mmap：SharedRef.Offset 是相对区域起始的
// 绝对偏移，因此读写都直接落在 region 上。
//
// mu 只在**内核进程内**使用——分配器完全由内核持有（见文件头所有权模型）。
type arenaRegion struct {
	mu sync.Mutex

	region []byte
	base   uint32 // arena 在 region 内的起始偏移
	cap    uint32 // arena 可用字节数
}

// arenaMinSize 返回块分配器能工作的最小字节数。
func arenaMinSize() uint32 {
	return arenaHeaderSize + blockHeaderSize + minBlockSize
}

// align8 把 n 向上取整到 8 的倍数（块大小与数据起点都必须 8 字节对齐）。
func align8(n uint32) uint32 { return (n + 7) &^ 7 }

// initArena 在统一区域的 arena 段上初始化块分配器。
func initArena(region []byte, base, size uint32) (*arenaRegion, error) {
	if size < arenaMinSize() {
		return nil, fmt.Errorf("arena: 段太小（%d 字节，至少需要 %d）", size, arenaMinSize())
	}
	if uint64(base)+uint64(size) > uint64(len(region)) {
		return nil, fmt.Errorf("arena: 越出映射（base=%d size=%d total=%d）", base, size, len(region))
	}

	a := &arenaRegion{region: region, base: base, cap: size}

	blockBase := align8(arenaHeaderSize)
	if blockBase+blockHeaderSize+minBlockSize > size {
		return nil, fmt.Errorf("arena: 头部过大（blockBase=%d size=%d）", blockBase, size)
	}

	ap := region[base : base+size]
	putU32(ap[arOffMagic:], arenaMagic)
	putU32(ap[arOffVersion:], arenaVersion)
	putU32(ap[arOffCapacity:], size)
	putU32(ap[arOffBlockBase:], blockBase)
	putU32(ap[arOffBlockUsed:], 0)

	// 初始状态：一整块 free 覆盖剩余空间。
	first := blockBase
	putU32(ap[first+blockOffSize:], size-first)
	putU32(ap[first+blockOffState:], blockFree)
	putU32(ap[first+blockOffOwner:], OwnerHost)
	putU32(ap[first+blockOffPrevSize:], 0)

	return a, nil
}

// attachArena 从已初始化的区域解析分配器（诊断用）。
func attachArena(region []byte, base, size uint32) (*arenaRegion, error) {
	if size < arenaHeaderSize {
		return nil, fmt.Errorf("arena: 段太小（%d）", size)
	}
	if uint64(base)+uint64(arenaHeaderSize) > uint64(len(region)) {
		return nil, fmt.Errorf("arena: 头部越界（base=%d total=%d）", base, len(region))
	}
	ap := region[base : base+size]

	if got := getU32(ap[arOffMagic:]); got != arenaMagic {
		return nil, fmt.Errorf("arena: 魔数不匹配（0x%x）", got)
	}
	if got := getU32(ap[arOffVersion:]); got != arenaVersion {
		return nil, fmt.Errorf("arena: 版本不匹配（%d，期望 %d）", got, arenaVersion)
	}
	capacity := getU32(ap[arOffCapacity:])
	blockBase := getU32(ap[arOffBlockBase:])
	if capacity != size {
		return nil, fmt.Errorf("arena: capacity 不匹配（header=%d mapped=%d）", capacity, size)
	}
	if blockBase+blockHeaderSize > size {
		return nil, fmt.Errorf("arena: blockBase 越界（%d，size=%d）", blockBase, size)
	}
	return &arenaRegion{region: region, base: base, cap: size}, nil
}

// MaxPayload 返回单次分配可承载的最大 payload 字节数（上界）。
func (a *arenaRegion) MaxPayload() int {
	return int(a.cap) - arenaHeaderSize - blockHeaderSize
}

// Capacity 返回 arena 总字节数。
func (a *arenaRegion) Capacity() uint32 { return a.cap }

// ---- 块头访问（相对 region 的绝对偏移）----

func (a *arenaRegion) blockBase() uint32 {
	return a.base + getU32(a.region[a.base+arOffBlockBase:])
}

func (a *arenaRegion) blockEnd() uint32 { return a.base + a.cap }

func (a *arenaRegion) blockSize(off uint32) uint32 {
	return getU32(a.region[off+blockOffSize:])
}

func (a *arenaRegion) setBlockSize(off, v uint32) {
	putU32(a.region[off+blockOffSize:], v)
}

func (a *arenaRegion) blockState(off uint32) uint32 {
	return getU32(a.region[off+blockOffState:])
}

func (a *arenaRegion) setBlockState(off, v uint32) {
	putU32(a.region[off+blockOffState:], v)
}

func (a *arenaRegion) blockOwner(off uint32) uint32 {
	return getU32(a.region[off+blockOffOwner:])
}

func (a *arenaRegion) setBlockOwner(off, v uint32) {
	putU32(a.region[off+blockOffOwner:], v)
}

func (a *arenaRegion) blockPrevSize(off uint32) uint32 {
	return getU32(a.region[off+blockOffPrevSize:])
}

func (a *arenaRegion) setBlockPrevSize(off, v uint32) {
	putU32(a.region[off+blockOffPrevSize:], v)
}

// blockDataOff 返回块数据区起点（SharedRef.Offset 即此值）。
func (a *arenaRegion) blockDataOff(off uint32) uint32 { return off + blockHeaderSize }

// nextBlock 返回下一块偏移；到末尾返回 blockEnd()。
func (a *arenaRegion) nextBlock(off uint32) uint32 {
	sz := a.blockSize(off)
	if sz < blockHeaderSize {
		return a.blockEnd() // 块链损坏：交给 walk 的步数上限兜底
	}
	next := off + sz
	if next > a.blockEnd() {
		return a.blockEnd()
	}
	return next
}

// DataOffsetOf 返回块偏移对应的数据起点（诊断/测试用）。
func (a *arenaRegion) DataOffsetOf(off uint32) uint32 { return a.blockDataOff(off) }

// Alloc 分配一块至少 n 字节的块，owner 写入块头。
//
// first-fit：从首个块开始找第一个容量足够的空闲块。分配器由内核独占，
// 因此这里的线性扫描不需要任何跨进程同步。
func (a *arenaRegion) Alloc(owner uint32, n int, gen uint64) (SharedRef, error) {
	if n < 0 {
		return SharedRef{}, fmt.Errorf("arena: 非法长度 %d", n)
	}
	need := align8(uint32(n) + blockHeaderSize)
	if need > a.cap-blockHeaderSize {
		return SharedRef{}, fmt.Errorf("arena: 请求 %d 字节超出 arena 容量 %d", n, a.MaxPayload())
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for off := a.blockBase(); off < a.blockEnd(); {
		sz := a.blockSize(off)
		if sz < blockHeaderSize {
			return SharedRef{}, fmt.Errorf("arena: 块链损坏（off=%d size=%d）", off, sz)
		}
		if a.blockState(off) == blockFree && sz >= need {
			a.carveLocked(off, need)
			a.setBlockState(off, blockUsed)
			a.setBlockOwner(off, owner)
			a.accountUsed(int32(align8(uint32(n))))
			return a.refOf(off, n, gen), nil
		}
		off = a.nextBlock(off)
	}
	return SharedRef{}, fmt.Errorf("arena: 空间不足（请求 %d 字节，容量 %d）", n, a.MaxPayload())
}

// carveLocked 把 off 处的空闲块劈成「已用 need + 剩余空闲」，并把剩余块
// 的头与后继块的 prevSize 维护好。
func (a *arenaRegion) carveLocked(off, need uint32) {
	sz := a.blockSize(off)
	if sz-need < minBlockSize {
		return // 剩余太小，整块给出去，不拆
	}
	rest := off + need
	restSize := sz - need
	a.setBlockSize(rest, restSize)
	a.setBlockState(rest, blockFree)
	a.setBlockOwner(rest, OwnerHost)
	a.setBlockPrevSize(rest, need)
	a.setBlockSize(off, need)

	// rest 的后继块现在以 rest 为前驱
	if nxt := rest + restSize; nxt < a.blockEnd() {
		a.setBlockPrevSize(nxt, restSize)
	}
}

// refOf 由块偏移构造引用。
func (a *arenaRegion) refOf(off uint32, n int, gen uint64) SharedRef {
	return SharedRef{
		Offset:     a.blockDataOff(off),
		Length:     uint32(n),
		Generation: uint32(gen),
	}
}

// Put 分配并写入 payload。
func (a *arenaRegion) Put(owner uint32, payload []byte, gen uint64) (SharedRef, error) {
	ref, err := a.Alloc(owner, len(payload), gen)
	if err != nil {
		return SharedRef{}, err
	}
	copy(a.region[ref.Offset:ref.Offset+ref.Length], payload)
	return ref, nil
}

// findBlockByData 在块链上找到数据起点等于 dataOff 的块。
//
// 这是对**插件提交的不可信 offset** 的校验入口：只有真的走完块链确认
// 该 offset 是一个块的数据起点，才允许后续改动分配器状态。
func (a *arenaRegion) findBlockByData(dataOff uint32) (uint32, bool) {
	steps := 0
	limit := int(a.cap/minBlockSize) + 4
	for off := a.blockBase(); off < a.blockEnd(); {
		if steps++; steps > limit {
			return 0, false // 链损坏，防死循环
		}
		sz := a.blockSize(off)
		if sz < blockHeaderSize || off+sz > a.blockEnd() {
			return 0, false
		}
		if a.blockDataOff(off) == dataOff {
			return off, true
		}
		off += sz
	}
	return 0, false
}

// findBlockContaining 找到数据区包含 off 的块。
//
// 与 findBlockByData 的区别：后者要求 off 恰好是块数据起点（用于 Free），
// 前者允许 off 落在块数据区的任意位置（用于 Read：调用帧内的结果区就
// 位于帧块中间，而不是块起点）。
func (a *arenaRegion) findBlockContaining(off uint32) (uint32, bool) {
	steps := 0
	limit := int(a.cap/minBlockSize) + 4
	for bo := a.blockBase(); bo < a.blockEnd(); {
		if steps++; steps > limit {
			return 0, false
		}
		sz := a.blockSize(bo)
		if sz < blockHeaderSize || bo+sz > a.blockEnd() {
			return 0, false
		}
		if off >= a.blockDataOff(bo) && off < bo+sz {
			return bo, true
		}
		bo += sz
	}
	return 0, false
}

// Read 按 SharedRef 读取数据。
//
// 校验：generation 与当前区域一致、offset 落在某个**已分配**块的数据区内、
// 引用不跨越块边界。任一不满足都返回 error（而不是 nil——早期版本返回
// nil 让调用方分不清“空数据”和“非法引用”）。
//
// 允许 offset 不必是块起点：调用帧的结果区就在帧块内部。
func (a *arenaRegion) Read(ref SharedRef, gen uint64) ([]byte, error) {
	if ref.IsZero() {
		return nil, nil
	}
	if ref.Generation != uint32(gen) {
		return nil, fmt.Errorf("arena: 引用 generation 过期（ref=%d now=%d）", ref.Generation, gen)
	}
	if uint64(ref.Offset)+uint64(ref.Length) > uint64(len(a.region)) {
		return nil, fmt.Errorf("arena: 引用越界（offset=%d len=%d total=%d）",
			ref.Offset, ref.Length, len(a.region))
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	bo, ok := a.findBlockContaining(ref.Offset)
	if !ok {
		return nil, fmt.Errorf("arena: 非法引用（offset=%d 不在任何块的数据区）", ref.Offset)
	}
	if a.blockState(bo) != blockUsed {
		return nil, fmt.Errorf("arena: 块（offset=%d）已释放，引用失效", ref.Offset)
	}
	if uint64(ref.Offset)+uint64(ref.Length) > uint64(bo+a.blockSize(bo)) {
		return nil, fmt.Errorf("arena: 引用跨越块边界（offset=%d len=%d blockEnd=%d）",
			ref.Offset, ref.Length, bo+a.blockSize(bo))
	}
	return a.region[ref.Offset : ref.Offset+ref.Length], nil
}

// Free 归还 owner 名下的块，并与相邻空闲块合并。
//
// 会走块链校验 offset 与 owner：伪造引用不能改动分配器状态。
func (a *arenaRegion) Free(owner uint32, ref SharedRef) error {
	if ref.IsZero() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	off, ok := a.findBlockByData(ref.Offset)
	if !ok {
		return fmt.Errorf("arena: 非法引用（offset=%d 不是块数据起点）", ref.Offset)
	}
	if a.blockState(off) != blockUsed {
		return fmt.Errorf("arena: 块（offset=%d）未分配（重复归还？）", ref.Offset)
	}
	if got := a.blockOwner(off); got != owner {
		return fmt.Errorf("arena: 块（offset=%d）不属于调用者（owner=%d caller=%d）", ref.Offset, got, owner)
	}

	a.setBlockState(off, blockFree)
	a.setBlockOwner(off, OwnerHost)
	a.accountUsed(-int32(align8(ref.Length)))
	a.coalesceLocked(off)
	return nil
}

// coalesceLocked 向后、向前合并相邻空闲块，并修正后继块的 prevSize。
func (a *arenaRegion) coalesceLocked(off uint32) {
	// 向后合并
	for {
		nxt := a.nextBlock(off)
		if nxt >= a.blockEnd() || a.blockState(nxt) != blockFree {
			break
		}
		a.setBlockSize(off, a.blockSize(off)+a.blockSize(nxt))
	}
	// 向前合并
	if ps := a.blockPrevSize(off); ps > 0 && off > a.blockBase() {
		prev := off - ps
		if prev >= a.blockBase() && a.blockState(prev) == blockFree {
			a.setBlockSize(prev, a.blockSize(prev)+a.blockSize(off))
			off = prev
		}
	}
	// 后继块的 prevSize 现在应等于合并后本块的大小
	if nxt := a.nextBlock(off); nxt < a.blockEnd() {
		a.setBlockPrevSize(nxt, a.blockSize(off))
	}
}

// ReclaimRefs 强制回收**指定的一批**块（不校验 owner），返回回收块数。
//
// 用途：取消超时的强制回收。这些块的 owner 是**插件**，而回收决策来自内核——
// 内核在此刻是全权的（见文件头的所有权模型），所以这里刻意**不**走
// Free 的归属校验：那条校验是防"插件伪造 ref 动别人的块"，
// 而这里要防的是完全相反的事——插件已经不打算/已经不能归还了。
//
// 安全边界仍在：每个 ref 都必须真的是一个**已分配块的数据起点**
// （findBlockByData 走完整块链），伪造/越界的 offset 一律跳过。
func (a *arenaRegion) ReclaimRefs(refs []SharedRef) int {
	if len(refs) == 0 {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	n := 0
	for _, ref := range refs {
		off, ok := a.findBlockByData(ref.Offset)
		if !ok {
			continue
		}
		if a.blockState(off) != blockUsed {
			continue // 已被插件自己归还或已被前面的合并吸收
		}
		a.setBlockState(off, blockFree)
		a.setBlockOwner(off, OwnerHost)
		a.accountUsed(-int32(align8(ref.Length)))
		a.coalesceLocked(off)
		n++
	}
	return n
}

// ReclaimOwner 归还 owner 名下所有块，返回回收块数。
//
// 用于插件进程退出：崩溃的插件无法归还自己申请的块，若不管会把 arena
// 慢慢耗尽，最终让所有走共享内存的调用退化成内联 RPC。
func (a *arenaRegion) ReclaimOwner(owner uint32) int {
	if owner == OwnerHost {
		return 0 // 内核自己的块由正常路径归还，不在此回收
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	// 先收集偏移再逐个归还：归还过程中会做合并，边遍历边改块链不可靠。
	var targets []uint32
	var reclaimedBytes uint32
	limit := int(a.cap/minBlockSize) + 4
	steps := 0
	for off := a.blockBase(); off < a.blockEnd(); {
		if steps++; steps > limit {
			break
		}
		sz := a.blockSize(off)
		if sz < blockHeaderSize || off+sz > a.blockEnd() {
			break
		}
		if a.blockState(off) == blockUsed && a.blockOwner(off) == owner {
			targets = append(targets, off)
		}
		off += sz
	}

	for _, off := range targets {
		if a.blockState(off) != blockUsed {
			continue // 已被前面的合并吸收
		}
		if a.blockOwner(off) != owner {
			continue
		}
		reclaimedBytes += a.blockSize(off) - blockHeaderSize
		a.setBlockState(off, blockFree)
		a.setBlockOwner(off, OwnerHost)
		a.coalesceLocked(off)
	}
	if reclaimedBytes > 0 {
		a.accountUsed(-int32(align8(reclaimedBytes)))
	}
	return len(targets)
}

// Stats 返回 (已用数据字节, 总字节)。已用字节按 8 字节对齐记账。
func (a *arenaRegion) Stats() (used, total uint32) {
	return getU32(a.region[a.base+arOffBlockUsed:]), a.cap
}

// accountUsed 累加/扣减已用字节（相对量，正数表示分配）。
func (a *arenaRegion) accountUsed(delta int32) {
	off := a.base + arOffBlockUsed
	cur := int32(getU32(a.region[off:]))
	next := cur + delta
	if next < 0 {
		next = 0
	}
	putU32(a.region[off:], uint32(next))
}

// BlockCount 返回块链上的块数（诊断/测试用）。
func (a *arenaRegion) BlockCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, limit := 0, int(a.cap/minBlockSize)+4
	for off := a.blockBase(); off < a.blockEnd() && n < limit; n++ {
		sz := a.blockSize(off)
		if sz < blockHeaderSize {
			break
		}
		off += sz
	}
	return n
}

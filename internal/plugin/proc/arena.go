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
	"errors"
	"fmt"
	"sync"
)

const (
	arenaMagic   uint32 = 0x41524E41 // "ARNA"
	arenaVersion uint32 = 3          // v3：真实偏移 → 分配表句柄（破坏性，旧插件必重编）

	arenaHeaderSize = 64

	// Arena Header 字段偏移（相对 arena 起始）。
	arOffMagic     = 0
	arOffVersion   = 4
	arOffCapacity  = 8  // arena 可用字节数
	arOffBlockBase = 12 // 首个块相对 arena 起始的偏移
	arOffBlockUsed = 16 // 已分配的数据字节数（诊断）
	// arOffTableBase/arOffTableCap 指向**分配表**：定长表项数组，
	// 内核写、插件只读。SharedRef.Handle 即该数组下标。
	arOffTableBase = 20
	arOffTableCap  = 24
	arOffReserved  = 28

	// 分配表项布局（32 字节，8 字节对齐 ⇒ 单次读取不撕裂）。
	//
	// 表项是**真实地址的唯一来源**：搬块时只改 offset/checkId，
	// 下标（Handle）不变，因此持有者手上的 ref 永远有效。
	tblEntrySize   = 32
	tblOffOffset   = 0  // uint32 块数据区真实偏移
	tblOffLength   = 4  // uint32 数据字节数
	tblOffRefCount = 8  // uint32 引用计数（归零才可搬/可释放）
	tblOffCheckID  = 12 // uint32 校验头（与块头相同，防迁移后错位读）
	tblOffFlags    = 16 // uint32 JSON / 扩容块
	tblOffLive     = 20 // uint32 1=占用 0=空闲
	tblOffOwner    = 24 // uint32 分配者
	tblOffReserved = 28

	// 块头 24B：size 含头，按 8 字节对齐。
	blockHeaderSize  = 24
	blockOffSize     = 0  // uint32 总大小（含头）
	blockOffState    = 4  // uint32 0=free 1=used
	blockOffOwner    = 8  // uint32 所有者
	blockOffPrevSize = 12 // uint32 前一块总大小（0=无前块）
	// blockOffCheckID 是**迁移校验头**：与分配表项的 checkId 相同。
	// 查表拿到 offset 后读它；不一致说明查表与访问之间块被搬过，
	// 调用方必须重查表。这是唯一能挡住"读到旧位置垃圾"的防线。
	blockOffCheckID  = 16 // uint32
	blockOffTableIDx = 20 // uint32 该块归属的分配表下标（0=无）

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

// 容量由 NewHostWithArenaSize 决定（见 host.go），这里不再维护第二份。
// 之前有个 arenaPublishSize 变量，但自 NewHost 改为接受参数后已无人读取 ——
// 留着一个"看着能改、改了没效果"的开关比没有更危险。

// arenaRegion 是块分配器视图。
//
// region 始终是**完整**统一区域 mmap：SharedRef.Offset 是相对区域起始的
// 绝对偏移，因此读写都直接落在 region 上。
//
// mu 只在**内核进程内**使用——分配器完全由内核持有（见文件头所有权模型）。
type arenaRegion struct {
	// mu 保护分配器结构（块链、分配表）。**只在改结构时持有**，
	// 读路径（插件侧查表）不经过它 —— 表在共享内存，插件只读。
	mu sync.Mutex

	region []byte
	base   uint32 // arena 在 region 内的起始偏移
	cap    uint32 // arena 可用字节数

	// tblBase 是分配表在 region 内的起点，tblCap 是表项个数。
	tblBase uint32
	tblCap  uint32

	// nextCheck 是校验头生成器（单调递增，不复用）。
	nextCheck uint32
}

// arenaMinSize 返回块分配器能工作的最小字节数。
func arenaMinSize() uint32 {
	return arenaHeaderSize + blockHeaderSize + minBlockSize
}

// align8 把 n 向上取整到 8 的倍数（块大小与数据起点都必须 8 字节对齐）。
func align8(n uint32) uint32 { return (n + 7) &^ 7 }

// initArena 在统一区域的 arena 段上初始化块分配器。
//
// 布局（均在 arena 段内，偏移相对段起点）：
//
//	[0, 64)                     Arena Header
//	[tableBase, tableBase+表项)  分配表（定长数组，内核写、插件只读）
//	[blockBase, size)           块链
//
// 分配表是**真实地址的唯一来源**：块搬动时只改表项里的 offset/checkId，
// 表下标（SharedRef.Handle）不变，因此持有者的 ref 永远有效。
func initArena(region []byte, base, size uint32) (*arenaRegion, error) {
	if size < arenaMinSize() {
		return nil, fmt.Errorf("arena: 段太小（%d 字节，至少需要 %d）", size, arenaMinSize())
	}
	if uint64(base)+uint64(size) > uint64(len(region)) {
		return nil, fmt.Errorf("arena: 越出映射（base=%d size=%d total=%d）", base, size, len(region))
	}

	ap := region[base : base+size]

	tableBase := align8(arenaHeaderSize)
	avail := size - tableBase
	maxEntries := avail / (tblEntrySize + blockHeaderSize + minBlockSize)
	if maxEntries == 0 {
		return nil, fmt.Errorf("arena: 段太小，容纳不下任何分配表项（%d 字节）", size)
	}
	if maxEntries > 4096 {
		maxEntries = 4096
	}
	tableCap := maxEntries
	blockBase := align8(tableBase + tableCap*tblEntrySize)
	if blockBase+blockHeaderSize+minBlockSize > size {
		return nil, fmt.Errorf("arena: 头部过大（blockBase=%d size=%d）", blockBase, size)
	}

	putU32(ap[arOffMagic:], arenaMagic)
	putU32(ap[arOffVersion:], arenaVersion)
	putU32(ap[arOffCapacity:], size)
	putU32(ap[arOffBlockBase:], blockBase)
	putU32(ap[arOffBlockUsed:], 0)
	putU32(ap[arOffTableBase:], tableBase)
	putU32(ap[arOffTableCap:], tableCap)

	// 分配表清零（live=0 即空闲表项）。
	for i := uint32(0); i < tableCap; i++ {
		e := tableBase + i*tblEntrySize
		putU32(ap[e+tblOffLive:], 0)
		putU32(ap[e+tblOffRefCount:], 0)
		putU32(ap[e+tblOffOffset:], 0)
		putU32(ap[e+tblOffLength:], 0)
		putU32(ap[e+tblOffCheckID:], 0)
	}

	// 初始状态：一整块 free 覆盖剩余空间。
	first := blockBase
	putU32(ap[first+blockOffSize:], size-first)
	putU32(ap[first+blockOffState:], blockFree)
	putU32(ap[first+blockOffOwner:], OwnerHost)
	putU32(ap[first+blockOffPrevSize:], 0)
	putU32(ap[first+blockOffCheckID:], 0)
	putU32(ap[first+blockOffTableIDx:], 0)

	return &arenaRegion{
		region:    region,
		base:      base,
		cap:       size,
		tblBase:   tableBase,
		tblCap:    tableCap,
		nextCheck: 1,
	}, nil
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
	tableBase := getU32(ap[arOffTableBase:])
	tableCap := getU32(ap[arOffTableCap:])
	if tableBase+tblEntrySize > size {
		return nil, fmt.Errorf("arena: 分配表越界（base=%d size=%d）", tableBase, size)
	}
	return &arenaRegion{region: region, base: base, cap: size,
		tblBase: tableBase, tblCap: tableCap, nextCheck: 1}, nil
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

	for attempt := 0; attempt < 2; attempt++ {
		for off := a.blockBase(); off < a.blockEnd(); {
			sz := a.blockSize(off)
			if sz < blockHeaderSize {
				return SharedRef{}, fmt.Errorf("arena: 块链损坏（off=%d size=%d）", off, sz)
			}
			if a.blockState(off) == blockFree && sz >= need {
				a.carveLocked(off, need)
				ref, err := a.claimBlockLocked(off, n, owner)
				if err != nil {
					return SharedRef{}, err
				}
				return ref, nil
			}
			off = a.nextBlock(off)
		}
		// 首次适配失败 ⇒ 先整理碎片再试一次（只在真撞上碎片时付搬运成本）。
		if attempt == 0 && !a.compactLocked() {
			break
		}
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

// claimBlockLocked 把块标记为已用、占一个分配表项、写块头校验头，
// 返回只含表下标的引用。调用方须持 a.mu。
//
// 校验头（checkId）同时写进表项和块头：查表拿到 offset 后比对两者，
// 不一致即说明"查表与访问之间块被搬过"，调用方必须重查表。
func (a *arenaRegion) claimBlockLocked(off uint32, n int, owner uint32) (SharedRef, error) {
	idx, ok := a.claimTableLocked(off, uint32(n), owner)
	if !ok {
		a.setBlockState(off, blockFree)
		a.setBlockOwner(off, OwnerHost)
		a.coalesceLocked(off)
		return SharedRef{}, fmt.Errorf("arena: 分配表已满（%d 项）", a.tblCap)
	}

	check := a.nextCheckID()
	ct := a.tableEntry(idx)
	putU32(a.region[ct+tblOffCheckID:], check)
	putU32(a.region[ct+tblOffRefCount:], 1)

	a.setBlockState(off, blockUsed)
	a.setBlockOwner(off, owner)
	putU32(a.region[off+blockOffCheckID:], check)
	putU32(a.region[off+blockOffTableIDx:], idx)
	a.accountUsed(int32(align8(uint32(n))))

	return SharedRef{Handle: idx, Length: uint32(n)}, nil
}

// tableEntry 返回表项在 region 内的绝对偏移。调用方须持锁或确保表只读。
func (a *arenaRegion) tableEntry(idx uint32) uint32 {
	return a.base + a.tblBase + idx*tblEntrySize
}

// claimTableLocked 找一个空闲表项并登记为占用。调用方须持 a.mu。
func (a *arenaRegion) claimTableLocked(off, length, owner uint32) (uint32, bool) {
	for i := uint32(0); i < a.tblCap; i++ {
		e := a.tableEntry(i)
		if getU32(a.region[e+tblOffLive:]) == 0 {
			putU32(a.region[e+tblOffOffset:], a.blockDataOff(off))
			putU32(a.region[e+tblOffLength:], length)
			putU32(a.region[e+tblOffOwner:], owner)
			putU32(a.region[e+tblOffFlags:], 0)
			putU32(a.region[e+tblOffRefCount:], 0)
			putU32(a.region[e+tblOffLive:], 1)
			return i, true
		}
	}
	return 0, false
}

// nextCheckID 生成单调递增的校验头（0 保留为"无校验头"）。
func (a *arenaRegion) nextCheckID() uint32 {
	a.nextCheck++
	if a.nextCheck == 0 {
		a.nextCheck = 1
	}
	return a.nextCheck
}

// Put 分配并写入 payload。
func (a *arenaRegion) Put(owner uint32, payload []byte, gen uint64) (SharedRef, error) {
	ref, err := a.Alloc(owner, len(payload), gen)
	if err != nil {
		return SharedRef{}, err
	}
	if err := a.writeRef(ref, payload, gen); err != nil {
		_ = a.Free(owner, ref)
		return SharedRef{}, err
	}
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

// ErrStaleRef 表示引用指向的块在"查表 → 访问"之间被迁移过。
//
// 调用方（SDK / 内核内部）应**释放持有的锁后重新查表**再试，
// 而不是在锁里重试（那会把搬块的写者饿死）。
var ErrStaleRef = errors.New("arena: 块已迁移（校验头不一致），需重新查表")

// resolveRef 查分配表得到真实数据偏移，并在块锁内校验块头校验头。
//
// ## 为什么在锁内校验（2026-11-14）
//
// 校验头与块内容必须是**同一时刻**的一致快照。锁外比对等于没比对：
// 读完校验头、还没读数据，块就被搬走了。所以整个"校验 + 取数据"
// 都在 a.mu 内完成。
//
// ## 为什么返回 error 而不是重试
//
// 搬块需要写锁，而这里持的是同一把锁 —— 在锁内重试只会自锁。
// 调用方必须退锁后重新查表（见 ErrStaleRef 的说明）。
func (a *arenaRegion) resolveRef(ref SharedRef) (uint32, uint32, error) {
	if ref.Handle >= a.tblCap {
		return 0, 0, fmt.Errorf("arena: 表下标越界（handle=%d cap=%d）", ref.Handle, a.tblCap)
	}
	e := a.tableEntry(ref.Handle)
	if getU32(a.region[e+tblOffLive:]) == 0 {
		return 0, 0, fmt.Errorf("arena: 表项已释放（handle=%d）", ref.Handle)
	}
	base := getU32(a.region[e+tblOffOffset:])
	wantCheck := getU32(a.region[e+tblOffCheckID:])
	tblLen := getU32(a.region[e+tblOffLength:])

	if uint64(ref.InnerOffset)+uint64(ref.Length) > uint64(tblLen) {
		return 0, 0, fmt.Errorf("arena: 块内偏移越界（inner=%d len=%d cap=%d）",
			ref.InnerOffset, ref.Length, tblLen)
	}
	start := base + ref.InnerOffset
	if uint64(start)+uint64(ref.Length) > uint64(len(a.region)) {
		return 0, 0, fmt.Errorf("arena: 引用越界（offset=%d len=%d total=%d）",
			start, ref.Length, len(a.region))
	}
	bo, ok := a.findBlockContaining(start)
	if !ok {
		return 0, 0, fmt.Errorf("arena: 非法引用（offset=%d 不在任何块的数据区）", start)
	}
	// ★ 校验头在块锁内比对：不一致 ⇒ 查表之后块被搬过。
	if got := getU32(a.region[bo+blockOffCheckID:]); got != wantCheck {
		return 0, 0, ErrStaleRef
	}
	if a.blockState(bo) != blockUsed {
		return 0, 0, fmt.Errorf("arena: 块（offset=%d）已释放，引用失效", start)
	}
	return start, tblLen, nil
}

// Read 按 SharedRef 读取数据。
//
// 校验顺序：表下标 → 表项存活 → 块内偏移 → 块锁内校验头 → 块状态。
// 校验头不一致返回 ErrStaleRef（块被迁移），调用方须退锁后重查表。
func (a *arenaRegion) Read(ref SharedRef, gen uint64) ([]byte, error) {
	if ref.IsZero() {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	start, _, err := a.resolveRef(ref)
	if err != nil {
		return nil, err
	}
	return a.region[start : start+ref.Length], nil
}

// ReadDup 读取引用指向的数据并**拷贝一份独立切片**。
//
// ## 为什么需要它（Compact 引入后的必需项）
//
// Read 返回的是 region 内的切片视图：锁一释放，Compact 就可能把块搬走，
// 那个切片立刻指向被覆盖的内存（读到别的块的数据，或零）。调用方一旦
// 跨语句持有它就会静默拿到错值 —— 这正是"看起来正常但语义错"的一类缺陷。
//
// ReadDup 在锁内拷贝，调用方拿到的是自己的副本，搬块不再影响它。
func (a *arenaRegion) ReadDup(ref SharedRef, gen uint64) ([]byte, error) {
	if ref.IsZero() {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	start, _, err := a.resolveRef(ref)
	if err != nil {
		return nil, err
	}
	out := make([]byte, ref.Length)
	copy(out, a.region[start:start+ref.Length])
	return out, nil
}

// Write 把 payload 写进引用指向的位置（块锁内校验后写入）。
func (a *arenaRegion) Write(ref SharedRef, payload []byte) error {
	if ref.IsZero() {
		return nil
	}
	if len(payload) != int(ref.Length) {
		return fmt.Errorf("arena: 写入长度 %d 与引用长度 %d 不符", len(payload), ref.Length)
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	start, _, err := a.resolveRef(ref)
	if err != nil {
		return err
	}
	copy(a.region[start:start+ref.Length], payload)
	return nil
}

// writeRef 是 Write 的 gen 兼容包装（旧调用点传 gen）。
func (a *arenaRegion) writeRef(ref SharedRef, payload []byte, gen uint64) error {
	return a.Write(ref, payload)
}

// Free 归还 owner 名下的块（引用计数归零时才真正释放）。
//
// 校验走分配表：handle 必须存在、owner 必须匹配。伪造引用不能改动分配器状态。
func (a *arenaRegion) Free(owner uint32, ref SharedRef) error {
	if ref.IsZero() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if ref.Handle >= a.tblCap {
		return fmt.Errorf("arena: 表下标越界（handle=%d cap=%d）", ref.Handle, a.tblCap)
	}
	e := a.tableEntry(ref.Handle)
	if getU32(a.region[e+tblOffLive:]) == 0 {
		return fmt.Errorf("arena: 表项已释放（handle=%d，重复归还？）", ref.Handle)
	}
	if got := getU32(a.region[e+tblOffOwner:]); got != owner {
		return fmt.Errorf("arena: 表项（handle=%d）不属于调用者（owner=%d caller=%d）", ref.Handle, got, owner)
	}

	rc := getU32(a.region[e+tblOffRefCount:])
	if rc > 1 {
		putU32(a.region[e+tblOffRefCount:], rc-1)
		return nil
	}
	base := getU32(a.region[e+tblOffOffset:])
	length := getU32(a.region[e+tblOffLength:])
	bo, ok := a.findBlockContaining(base)
	if ok && getU32(a.region[e+tblOffLive:]) != 0 {
		// 只有块内第一个引用释放时才还块（子区间引用共享同一块）。
		if getU32(a.region[bo+blockOffTableIDx:]) == ref.Handle {
			a.setBlockState(bo, blockFree)
			a.setBlockOwner(bo, OwnerHost)
			putU32(a.region[bo+blockOffCheckID:], 0)
			putU32(a.region[bo+blockOffTableIDx:], 0)
			a.accountUsed(-int32(align8(length)))
			a.coalesceLocked(bo)
		}
	}
	putU32(a.region[e+tblOffLive:], 0)
	putU32(a.region[e+tblOffRefCount:], 0)
	putU32(a.region[e+tblOffCheckID:], 0)
	putU32(a.region[e+tblOffOffset:], 0)
	putU32(a.region[e+tblOffLength:], 0)
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
		if ref.Handle >= a.tblCap {
			continue
		}
		e := a.tableEntry(ref.Handle)
		if getU32(a.region[e+tblOffLive:]) == 0 {
			continue // 已归还或已被前面的回收吸收
		}
		off, ok := a.releaseTableEntryLocked(ref.Handle)
		if !ok {
			continue
		}
		if off != 0 {
			a.setBlockState(off, blockFree)
			a.setBlockOwner(off, OwnerHost)
			putU32(a.region[off+blockOffCheckID:], 0)
			putU32(a.region[off+blockOffTableIDx:], 0)
			a.accountUsed(-int32(align8(getU32(a.region[e+tblOffLength:]))))
			a.coalesceLocked(off)
		}
		n++
	}
	return n
}

// releaseTableEntryLocked 释放一个表项，返回需要一并归还的块偏移
// （0 表示该表项不是块的首引用，只需清表项）。调用方须持 a.mu。
func (a *arenaRegion) releaseTableEntryLocked(handle uint32) (uint32, bool) {
	e := a.tableEntry(handle)
	base := getU32(a.region[e+tblOffOffset:])
	var off uint32
	if bo, ok := a.findBlockContaining(base); ok {
		if getU32(a.region[bo+blockOffTableIDx:]) == handle {
			off = bo
		}
	}
	putU32(a.region[e+tblOffLive:], 0)
	putU32(a.region[e+tblOffRefCount:], 0)
	putU32(a.region[e+tblOffCheckID:], 0)
	putU32(a.region[e+tblOffOffset:], 0)
	putU32(a.region[e+tblOffLength:], 0)
	return off, true
}

// Compact 把存活块向段首紧凑排列，消除碎片，返回搬动的块数。
//
// ## 为什么搬活块是安全的（本设计的全部意义）
//
// 旧实现把**绝对偏移**放在 SharedRef 里，搬块 ⇒ 所有持有者的 ref 立即失效，
// 而内核无法批量改写插件进程里的局部变量 ⇒ 整理根本做不了（实测 4MB 段
// used=64 字节却分不出 2MB，只能靠邻块合并，而小块隔断让它无效）。
//
// 现在 SharedRef 只存表下标（Handle），真实地址经分配表查询，所以：
//
//	搬块 → 只改表项的 offset + 换一个新 checkId ⇒ 句柄不变，引用保持有效
//
// ## checkId 的作用
//
// SDK 侧查表是**无锁只读**的（表在内核共享内存头）。若插件刚查完表、
// 内核算好搬了块，插件拿到的就是过期的 offset。块头里的 checkId 与表项
// 比对不一致即说明"这次查表结果已过期"，插件重查一次即可 —— 不会读错数据。
//
// ## 并发边界
//
// 本函数持 a.mu（与内核侧所有读写互斥），所以内核侧不会有"读到一半被搬走"。
// SDK 侧的窗口由 checkId 兜住。回收只搬**表项仍存活**的块 —— 已释放的块
// 本来就是空闲空间，直接并进尾部空洞。
func (a *arenaRegion) Compact(gen uint64) (moved int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.compactLocked() {
		return 1
	}
	return 0
}

// compactLocked 是 Compact 的加锁内实现，返回是否发生了搬运。
func (a *arenaRegion) compactLocked() bool {
	moved := false

	// 收集存活块（按物理顺序），供紧凑重排。
	type live struct {
		off      uint32
		size     uint32
		tableIdx uint32
		dataLen  uint32
	}
	var lives []live

	for off := a.blockBase(); off < a.blockEnd(); {
		sz := a.blockSize(off)
		if sz < blockHeaderSize || off+sz > a.blockEnd() {
			return moved
		}
		if a.blockState(off) == blockUsed {
			idx := getU32(a.region[off+blockOffTableIDx:])
			var dataLen uint32
			if idx != 0 && idx < a.tblCap {
				dataLen = getU32(a.region[a.tableEntry(idx)+tblOffLength:])
			}
			if dataLen == 0 || dataLen+blockHeaderSize > sz {
				dataLen = sz - blockHeaderSize
			}
			lives = append(lives, live{off: off, size: sz, tableIdx: idx, dataLen: dataLen})
		}
		off = a.nextBlock(off)
	}

	// 从段首开始重新排布：每个存活块紧贴前一个。
	cursor := a.blockBase()
	for _, l := range lives {
		if l.off == cursor {
			cursor += l.size
			continue
		}
		// 搬动整块（块头 + 数据）到 cursor 处。
		size := l.size
		copy(a.region[cursor:cursor+size], a.region[l.off:l.off+size])
		// 新位置的块头记录新偏移对应关系；首块 prevSize 视前驱而定。
		var prevSize uint32
		if cursor > a.blockBase() {
			if po, ok := a.findBlockContaining(cursor - 1); ok {
				prevSize = a.blockSize(po)
			}
		}
		putU32(a.region[cursor+blockOffPrevSize:], prevSize)

		// 更新表项：真实地址变了，同时换新 checkId（让 SDK 侧重查）。
		if l.tableIdx != 0 && l.tableIdx < a.tblCap {
			e := a.tableEntry(l.tableIdx)
			check := a.nextCheckID()
			putU32(a.region[e+tblOffOffset:], a.blockDataOff(cursor))
			putU32(a.region[e+tblOffCheckID:], check)
			putU32(a.region[cursor+blockOffCheckID:], check)
		}
		cursor += size
		moved = true
	}

	// 剩余空间合成一个尾部空闲块。
	rest := a.blockEnd() - cursor
	if rest >= minBlockSize {
		putU32(a.region[cursor+blockOffSize:], rest)
		putU32(a.region[cursor+blockOffState:], blockFree)
		putU32(a.region[cursor+blockOffOwner:], OwnerHost)
		putU32(a.region[cursor+blockOffPrevSize:], cursor-a.blockBase())
		putU32(a.region[cursor+blockOffCheckID:], 0)
		putU32(a.region[cursor+blockOffTableIDx:], 0)
	}

	return moved
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

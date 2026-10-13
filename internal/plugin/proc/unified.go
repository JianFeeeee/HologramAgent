package proc

// 统一共享内存区域布局（§13.1）。
//
// 之前的两块独立 memfd（StageContext 256KB + EvtRing ~320KB，fd 3/4）
// 合并为单一 memfd（fd 3），eventfd 独占 fd 4。
//
// 区域内按固定偏移定位各 segment，所有偏移相对段起始，跨进程 mmap 到
// 不同虚拟地址仍能正确解引用（§3.3 实验 2 已验证）。
//
// ┌──────────────────────────────────────────────────────┐
// │ SuperBlock 64B                                       │
// │   magic / version / generation / capacity / reserved │
// │   ctxOff / ctxSize / evtOff / evtSize                │
// ├──────────────────────────────────────────────────────┤
// │ StageContext segment  （内部布局不变）                │
// ├──────────────────────────────────────────────────────┤
// │ EvtRing segment       （内部布局不变）                │
// ├──────────────────────────────────────────────────────┤
// │ ToolCall lane  [§13.3]                               │
// ├──────────────────────────────────────────────────────┤
// │ InputCh lane  [§13.5]                                │
// ├──────────────────────────────────────────────────────┤
// │ OutputCh lane [§13.6]                                │
// ├──────────────────────────────────────────────────────┤
// │ Dynamic Arena                                        │
// └──────────────────────────────────────────────────────┘

import (
	"fmt"
	"sync/atomic"
	"unsafe"
)

const (
	// unifiedMagic 标识统一共享内存区域。
	unifiedMagic uint32 = 0x554D5352 // "UMSR" — Unified Memory Shared Region

	superBlockSize = 64 // SuperBlock 占前 64 字节
)

// SuperBlock 布局：64 字节，所有偏移相对区域起始。
//
// [0,4)   magic
// [4,8)   reserved（原 version，已废：SuperBlock 靠 magic + capacity 自描述）
// [8,16)  generation（atomic uint64，resize 时 bump）
// [16,20) capacity（区域总字节数）
// [20,24) ctxOff（StageContext segment 偏移）
// [24,28) ctxSize（StageContext segment 字节数）
// [28,32) evtOff（EvtRing segment 偏移）
// [32,36) evtSize（EvtRing segment 字节数）
// [36,40) arenaOff（Exchange Arena 起点，8 字节对齐）
// [40,44) arenaCap（Exchange Arena 字节数）
// [44,64) reserved
const (
	sbOffMagic      = 0
	sbOffReserved0  = 4 // 保留（原 version 槽位，布局不变以免动其余偏移）
	sbOffGeneration = 8
	sbOffCapacity   = 16
	sbOffCtxOff     = 20
	sbOffCtxSize    = 24
	sbOffEvtOff     = 28
	sbOffEvtSize    = 32
	sbOffArenaOff   = 36 // Exchange Arena 起点
	sbOffArenaCap   = 40 // Exchange Arena 容量
	sbOffReserved1  = 44
	sbOffReserved5  = 48
	sbOffReserved6  = 52
	sbOffReserved7  = 56
	sbOffReserved8  = 60
)

// SharedRef 是跨进程共享内存描述符，替代内联 JSON 数据。
//
// 所有数据交换（工具调用参数/结果、Cleaner、输入/输出通道消息）
// 都通过 SharedRef 传递：RPC 只传 16 字节描述符，实际数据在共享内存中。
//
// Generation 防 ABA：扩容 remap 后旧描述符自动失效。
type SharedRef struct {
	Offset     uint32 // 相对区域起始的偏移
	Length     uint32 // 数据字节数
	Generation uint32 // 扩容后 bump
	Flags      uint32 // 保留，位 0 = 二进制，位 1 = JSON
}

const sharedRefSize = 16

func (r SharedRef) IsZero() bool {
	return r.Offset == 0 && r.Length == 0
}

// Slice 从共享内存中按 SharedRef 切片。data 必须是完整的 mmap 区域。
//
// 注意：这是**无校验**的原始切片，仅供已知安全的路径使用。
// 跨进程引用一律走 arenaRegion.Read（校验 generation / 槽号 / 占用状态）。
func (r SharedRef) Slice(data []byte) []byte {
	end := uint64(r.Offset) + uint64(r.Length)
	if r.IsZero() || end > uint64(len(data)) {
		return nil
	}
	return data[r.Offset:uint32(end)]
}

// unifiedRegion 统一共享内存区域的内核侧视图。
//
// arenaOff/arenaCap 指向 Exchange Arena 段；槽池句柄由 Host 持有
// （见 arena.go），unifiedRegion 只负责布局与 generation。
type unifiedRegion struct {
	data []byte

	ctxOff  uint32
	ctxSize uint32
	evtOff  uint32
	evtSize uint32

	arenaOff uint32 // Exchange Arena 起点（8 字节对齐）
	arenaCap uint32 // Exchange Arena 容量
}

// initUnifiedRegion 在 mmap 区域上初始化 SuperBlock + 两个 segment。
func initUnifiedRegion(data []byte, ctxTotal, evtTotal int) (*unifiedRegion, error) {
	cap := uint32(len(data))
	total := superBlockSize + ctxTotal + evtTotal
	if int(cap) < total {
		return nil, fmt.Errorf("unified: 区域过小（%d 字节，至少需要 %d）", cap, total)
	}

	ctxOff := uint32(superBlockSize)
	evtOff := ctxOff + uint32(ctxTotal)
	// arena 基址必须 8 字节对齐：位图用 4 字节原子操作，槽头含 uint32。
	arenaOff := (evtOff + uint32(evtTotal) + 7) &^ 7
	arenaCap := cap - arenaOff

	putU32(data[sbOffMagic:], unifiedMagic)
	putU64(data[sbOffGeneration:], 0)
	putU32(data[sbOffCapacity:], cap)
	putU32(data[sbOffCtxOff:], ctxOff)
	putU32(data[sbOffCtxSize:], uint32(ctxTotal))
	putU32(data[sbOffEvtOff:], evtOff)
	putU32(data[sbOffEvtSize:], uint32(evtTotal))
	putU32(data[sbOffArenaOff:], arenaOff)
	putU32(data[sbOffArenaCap:], arenaCap)

	return &unifiedRegion{
		data:     data,
		ctxOff:   ctxOff,
		ctxSize:  uint32(ctxTotal),
		evtOff:   evtOff,
		evtSize:  uint32(evtTotal),
		arenaOff: arenaOff,
		arenaCap: arenaCap,
	}, nil
}

// attachUnifiedRegion 从已有 mmap 区域解析 SuperBlock（插件侧调用）。
func attachUnifiedRegion(data []byte) (*unifiedRegion, error) {
	if len(data) < superBlockSize {
		return nil, fmt.Errorf("unified: 区域过小（%d 字节）", len(data))
	}
	magic := getU32(data[sbOffMagic:])
	if magic != unifiedMagic {
		return nil, fmt.Errorf("unified: 魔数不匹配（0x%x，期望 0x%x）", magic, unifiedMagic)
	}
	// 不自描述校验版本号：SuperBlock 靠 magic + capacity + 各段越界检查把关
	//（这些是结构的派生事实，不存在“忘同步一个人工号”的失效模式）。
	ctxOff := getU32(data[sbOffCtxOff:])
	ctxSize := getU32(data[sbOffCtxSize:])
	evtOff := getU32(data[sbOffEvtOff:])
	evtSize := getU32(data[sbOffEvtSize:])

	capacity := getU32(data[sbOffCapacity:])
	arenaOff := getU32(data[sbOffArenaOff:])
	arenaCap := getU32(data[sbOffArenaCap:])

	if capacity != uint32(len(data)) {
		return nil, fmt.Errorf("unified: capacity 不匹配（header=%d mapped=%d）", capacity, len(data))
	}
	if uint64(ctxOff)+uint64(ctxSize) > uint64(len(data)) {
		return nil, fmt.Errorf("unified: StageContext 越界（off=%d size=%d total=%d）", ctxOff, ctxSize, len(data))
	}
	if uint64(evtOff)+uint64(evtSize) > uint64(len(data)) {
		return nil, fmt.Errorf("unified: EvtRing 越界（off=%d size=%d total=%d）", evtOff, evtSize, len(data))
	}
	if uint64(arenaOff)+uint64(arenaCap) > uint64(len(data)) {
		return nil, fmt.Errorf("unified: arena 越界（off=%d cap=%d total=%d）", arenaOff, arenaCap, len(data))
	}

	return &unifiedRegion{
		data:     data,
		ctxOff:   ctxOff,
		ctxSize:  ctxSize,
		evtOff:   evtOff,
		evtSize:  evtSize,
		arenaOff: arenaOff,
		arenaCap: arenaCap,
	}, nil
}

func (r *unifiedRegion) generation() uint64 {
	return getU64(r.data[sbOffGeneration:])
}

func (r *unifiedRegion) bumpGeneration() uint64 {
	for {
		old := getU64(r.data[sbOffGeneration:])
		new := old + 1
		p := (*uint64)(unsafe.Pointer(&r.data[sbOffGeneration]))
		if atomic.CompareAndSwapUint64(p, old, new) {
			return new
		}
	}
}

func (r *unifiedRegion) ctxData() []byte {
	return r.data[r.ctxOff : r.ctxOff+r.ctxSize]
}

func (r *unifiedRegion) evtData() []byte {
	return r.data[r.evtOff : r.evtOff+r.evtSize]
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func getU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func putU64(b []byte, v uint64) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}

func getU64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// arenaReset 已移除。
//
// 旧实现把共享游标重置为 1，会覆盖其他进程/goroutine 仍在使用的分配。
// 现设计用槽位图：释放是精确的 per-slot CAS，不存在"整体重置"语义。

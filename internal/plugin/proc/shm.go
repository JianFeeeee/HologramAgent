// Package proc 实现外部插件的子进程加载通道（plugin.bin）。
//
// 数据面设计：定长头 + arena，变长数据用相对基址的 {off,len} 描述符
//（相对偏移是关键：各进程 mmap 到不同虚拟地址也能正确解引用）。
//
//	homed ──spawn──> plugin（纯 Go 二进制，无 cgo）
//	  ├── stdio JSON-RPC   控制面：51 个 method id 平移为 method 名（§3.2）
//	  ├── shm + 偏移        数据面：StageContext 并发改写、二进制零拷贝（§3.3）
//	  └── eventfd          通知面：事件环 post-and-forget（§3.6）
//
// 本文件负责数据面的共享段布局与 arena 分配器。
package proc

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
)

// 共享段魔数与版本，用于挂载时校验对端布局一致。
//
// v2（2026-10-12）：新增 fExtraBag —— Extra 具名 4 键之外的插件自建键
// （如 qq 的 qq_auth 绑帧身份）跨进程/跨 stage 透传。加字段必升版：
// AttachSegment 与插件握手对版本失败即拒，旧插件不会静默错读新布局。
const (
	shmMagic   uint32 = 0x48415348 // "HASH" — HomeAgent SHared
	shmVersion uint32 = 2
)

// 段布局（所有偏移均相对**段起始**，arena 内偏移相对 arenaBase）：
//
//	[0, headerSize)                 Header：魔数/版本/arena 游标
//	[headerSize, ctxEnd)            ShmStageCtx：每字段一个 Slice{off,len} 描述符
//	[arenaBase, arenaBase+arenaCap) arena：append-only 变长数据区
//
// **相对偏移是关键**（§3.3）——各进程 mmap 到不同虚拟地址仍能正确解引用。
const (
	headerSize = 64

	// Header 内字段偏移
	offMagic     = 0  // uint32
	offVersion   = 4  // uint32
	offArenaBase = 8  // uint32
	offArenaCap  = 12 // uint32
	offArenaUsed = 16 // uint32（原子 bump 游标）
	offCtxBase   = 20 // uint32
	offSeq       = 24 // uint64：每次成功写回自增，供乐观读校验
)

// Slice 是 arena 内变长数据的描述符，off 相对 arenaBase。
// 长度为 0 表示空值；off==0 && len==0 表示"字段未设置"。
type Slice struct {
	Off uint32
	Len uint32
}

const sliceSize = 8

// IsUnset 报告该描述符是否表示"字段从未被写入"。
// 注意与"写入了空字符串"区分：后者 Off 非 0、Len 为 0。
func (s Slice) IsUnset() bool { return s.Off == 0 && s.Len == 0 }

// stageField 枚举 StageContext 的 16 个字段在共享段中的槽位。
//
// **字段级粒度是消除 lost update 的机制**：每个字段独立一个 Slice 描述符，
// 只改 FinalText 的插件完全不触碰 ToolResults 的描述符，因此不存在
// "只读插件把自己收到的旧快照写回、覆盖他人改写"的问题（对比今日副本模型
// 实测 35.8~36.8% 丢失率，见 §8.4）。
//
// 字段内部的编码方式（原始字符串 vs JSON）不影响这一性质：
// ToolCall.Arguments 是 map[string]interface{}、ToolResult.Result 是 interface{}，
// 无法拆成定长结构，故以 JSON 存入 arena——工具结果中位数仅 93B（§2.5），
// 序列化开销占 LLM 往返的 0.0001%，不构成瓶颈。
type stageField int

const (
	fRawMessage stageField = iota
	fUserID
	fGroupID
	fLLMText
	fReasoningContent
	fFinalText
	fResponse // 配合 fResponseSet 表达 *string 的 nil 语义
	fPhase
	fContextMsgs      // JSON
	fToolCalls        // JSON
	fToolResults      // JSON
	fMemory           // JSON
	fTokenUsage       // JSON
	fErrors           // JSON
	fExtraMediaBlocks // JSON —— Extra 的 4 个键提升为具名字段
	fExtraMediaType
	fExtraInputSource
	fExtraOutputChannel
	// fExtraBag 承载 Extra 里**具名 4 键之外的全部插件自建键**（JSON）。
	//
	// 背景（2026-10-12 权限失效修复）：qq 插件靠 ctx.Extra["qq_auth"] 把
	// 身份绑到推理帧上（中断抢占/并发合并场景下全局身份不可靠，见该插件
	// README「身份必须绑帧」）。但此前的桥只透传 4 个具名键，插件自建键
	// 在 stage 之间静默丢失 → before_toolcall 永远回落到可变全局身份 →
	// 绑帧（以及建立在它之上的权限门）在 proc 插件下整体失效。
	//
	// bag 把 Extra 恢复成「任意键跨进程/跨 stage 存活」的原语义。值以 JSON
	// 往返：插件侧拿到 map[string]interface{}（数字统一 float64），不能
	// 类型断言回私有 struct —— 插件侧解码约定见 sdk example/qq。
	fExtraBag

	stageFieldCount
)

// 标志位区（紧跟描述符数组）：表达 bool 与指针的 nil 语义。
const (
	flagNoMemory    = 0
	flagResponseSet = 1
	flagCount       = 8 // 预留到 8 字节，便于对齐与后续扩展
)

// ctxSize 是 ShmStageCtx 区域的总字节数。
const ctxSize = int(stageFieldCount)*sliceSize + flagCount

// Segment 是一块已 mmap 的共享段，内核与插件进程各持一个实例
// （底层同一物理页，虚拟地址可不同）。
type Segment struct {
	data []byte // 完整 mmap 区域
}

// NewSegment 在给定的 mmap 区域上初始化段布局（内核侧调用一次）。
func NewSegment(data []byte) (*Segment, error) {
	if len(data) < headerSize+ctxSize+1 {
		return nil, fmt.Errorf("proc: 共享段过小（%d 字节，至少需要 %d）",
			len(data), headerSize+ctxSize+1)
	}
	s := &Segment{data: data}

	arenaBase := uint32(headerSize + ctxSize)
	arenaCap := uint32(len(data)) - arenaBase

	binary.LittleEndian.PutUint32(data[offMagic:], shmMagic)
	binary.LittleEndian.PutUint32(data[offVersion:], shmVersion)
	binary.LittleEndian.PutUint32(data[offArenaBase:], arenaBase)
	binary.LittleEndian.PutUint32(data[offArenaCap:], arenaCap)
	binary.LittleEndian.PutUint32(data[offArenaUsed:], 0)
	binary.LittleEndian.PutUint32(data[offCtxBase:], headerSize)
	binary.LittleEndian.PutUint64(data[offSeq:], 0)

	// 描述符与标志位清零（IsUnset 语义依赖此）
	for i := headerSize; i < headerSize+ctxSize; i++ {
		data[i] = 0
	}
	return s, nil
}

// AttachSegment 挂载一块已由 NewSegment 初始化的区域（插件进程侧调用）。
// 校验魔数与版本，避免版本不一致时静默错读。
func AttachSegment(data []byte) (*Segment, error) {
	if len(data) < headerSize+ctxSize {
		return nil, fmt.Errorf("proc: 共享段过小（%d 字节）", len(data))
	}
	if got := binary.LittleEndian.Uint32(data[offMagic:]); got != shmMagic {
		return nil, fmt.Errorf("proc: 共享段魔数不匹配（0x%x，期望 0x%x）", got, shmMagic)
	}
	if got := binary.LittleEndian.Uint32(data[offVersion:]); got != shmVersion {
		return nil, fmt.Errorf("proc: 共享段版本不匹配（%d，本内核 %d）——插件需用配套 hmapdev 重编",
			got, shmVersion)
	}
	return &Segment{data: data}, nil
}

func (s *Segment) arenaBase() uint32 { return binary.LittleEndian.Uint32(s.data[offArenaBase:]) }
func (s *Segment) arenaCap() uint32  { return binary.LittleEndian.Uint32(s.data[offArenaCap:]) }
func (s *Segment) ctxBase() uint32   { return binary.LittleEndian.Uint32(s.data[offCtxBase:]) }

// Seq 返回当前世代号。每次 WriteBack 成功后自增，供乐观读校验（§3.3）。
func (s *Segment) Seq() uint64 {
	return atomic.LoadUint64((*uint64)(ptrU64(s.data[offSeq:])))
}

func (s *Segment) bumpSeq() { atomic.AddUint64((*uint64)(ptrU64(s.data[offSeq:])), 1) }

// ArenaUsed 返回 arena 已用字节数（诊断/压实判断用）。
func (s *Segment) ArenaUsed() uint32 {
	return atomic.LoadUint32((*uint32)(ptrU32(s.data[offArenaUsed:])))
}

// ArenaCap 返回 arena 容量。
func (s *Segment) ArenaCap() uint32 { return s.arenaCap() }

// alloc 在 arena 上分配 n 字节并返回相对 arenaBase 的偏移。
//
// **append-only（§3.3）**：插件把 FinalText 从 10 字节改成 10KB 时分配新区域、
// 更新描述符，旧区域留作垃圾；arena 用尽由内核在 stage 结束后（此时无插件持锁）
// 整体压实。代价是单次 stage 内写入总量有上限——**上限必须显式报错而非静默截断**
// （§4.4 风险登记）。
//
// 调用方须持有 stage 写锁（锁仲裁见 lock.go），故这里用非原子的读-改-写即可；
// 仍用原子操作是为了让未持锁的诊断读取（ArenaUsed）不产生数据竞争。
func (s *Segment) alloc(n int) (uint32, error) {
	if n < 0 {
		return 0, fmt.Errorf("proc: 非法分配长度 %d", n)
	}
	// 偏移 0 保留给"字段未设置"语义，故 arena 从 1 开始分配。
	used := s.ArenaUsed()
	if used == 0 {
		used = 1
	}
	end := uint64(used) + uint64(n)
	if end > uint64(s.arenaCap()) {
		return 0, fmt.Errorf("proc: arena 空间不足——需要 %d 字节，剩余 %d 字节（容量 %d，已用 %d）；"+
			"单次 stage 写入总量超限，请减少写入或等待内核压实",
			n, int64(s.arenaCap())-int64(used), s.arenaCap(), used)
	}
	atomic.StoreUint32((*uint32)(ptrU32(s.data[offArenaUsed:])), uint32(end))
	return used, nil
}

// write 把 b 写入 arena 并返回描述符。空切片返回 {Off:1, Len:0}
// （非 IsUnset —— 表达"写入了空值"，与"未设置"区分）。
func (s *Segment) write(b []byte) (Slice, error) {
	if len(b) == 0 {
		return Slice{Off: 1, Len: 0}, nil
	}
	off, err := s.alloc(len(b))
	if err != nil {
		return Slice{}, err
	}
	base := s.arenaBase()
	copy(s.data[base+off:base+off+uint32(len(b))], b)
	return Slice{Off: off, Len: uint32(len(b))}, nil
}

// read 按描述符取出 arena 中的字节（返回的是段内切片视图，调用方须在持锁期间使用）。
func (s *Segment) read(sl Slice) ([]byte, error) {
	if sl.IsUnset() || sl.Len == 0 {
		return nil, nil
	}
	base := s.arenaBase()
	if uint64(sl.Off)+uint64(sl.Len) > uint64(s.arenaCap()) {
		return nil, fmt.Errorf("proc: 描述符越界（off=%d len=%d cap=%d）", sl.Off, sl.Len, s.arenaCap())
	}
	return s.data[base+sl.Off : base+sl.Off+sl.Len], nil
}

// descOffset 返回字段 f 的描述符在段内的绝对偏移。
func (s *Segment) descOffset(f stageField) uint32 {
	return s.ctxBase() + uint32(int(f)*sliceSize)
}

func (s *Segment) getDesc(f stageField) Slice {
	o := s.descOffset(f)
	return Slice{
		Off: binary.LittleEndian.Uint32(s.data[o:]),
		Len: binary.LittleEndian.Uint32(s.data[o+4:]),
	}
}

func (s *Segment) setDesc(f stageField, sl Slice) {
	o := s.descOffset(f)
	binary.LittleEndian.PutUint32(s.data[o:], sl.Off)
	binary.LittleEndian.PutUint32(s.data[o+4:], sl.Len)
}

func (s *Segment) flagsOffset() uint32 {
	return s.ctxBase() + uint32(int(stageFieldCount)*sliceSize)
}

func (s *Segment) getFlag(bit int) bool {
	return s.data[s.flagsOffset()+uint32(bit)] != 0
}

func (s *Segment) setFlag(bit int, v bool) {
	b := byte(0)
	if v {
		b = 1
	}
	s.data[s.flagsOffset()+uint32(bit)] = b
}

// Compact 回收 arena 垃圾：把仍被描述符引用的数据紧凑重排到段头部。
//
// 必须在**无插件持锁**时调用（§3.3：由内核在 stage 结束后执行）。
// 返回回收的字节数。
func (s *Segment) Compact() uint32 {
	before := s.ArenaUsed()

	// 收集现存描述符指向的数据，按字段顺序重新写入。
	type kept struct {
		f    stageField
		data []byte
	}
	var live []kept
	for f := stageField(0); f < stageFieldCount; f++ {
		sl := s.getDesc(f)
		if sl.IsUnset() {
			continue
		}
		b, err := s.read(sl)
		if err != nil {
			// 描述符损坏：丢弃该字段而非让压实失败（诊断由上层日志承担）
			s.setDesc(f, Slice{})
			continue
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		live = append(live, kept{f: f, data: cp})
	}

	// 重置游标后按序回填
	atomic.StoreUint32((*uint32)(ptrU32(s.data[offArenaUsed:])), 0)
	for _, k := range live {
		sl, err := s.write(k.data)
		if err != nil {
			// 压实后仍放不下：理论上不可能（总量未增），保守清空该字段
			s.setDesc(k.f, Slice{})
			continue
		}
		s.setDesc(k.f, sl)
	}

	after := s.ArenaUsed()
	if before > after {
		return before - after
	}
	return 0
}

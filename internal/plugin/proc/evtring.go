package proc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// ---- 事件类型编码（编译时确定，与 pubsdk.EventType 一一对应）----

var evtTypeNames = [evtTypeMax]string{
	"raw_input",
	"agent_output",
	"agent_llm_chain",
	"tool_call",
	"reasoning",
	"stage",
	"system",
	"reasoning_delta",
	"content_delta",
	"skill_detected",
}

var evtTypeIndex = map[string]uint32{
	"raw_input":       evtTypeRawInput,
	"agent_output":    evtTypeAgentOutput,
	"agent_llm_chain": evtTypeAgentLLMChain,
	"tool_call":       evtTypeToolCall,
	"reasoning":       evtTypeReasoning,
	"stage":           evtTypeStage,
	"system":          evtTypeSystem,
	"reasoning_delta": evtTypeReasoningDelta,
	"content_delta":   evtTypeContentDelta,
	"skill_detected":  evtTypeSkillDetected,
}

func encodeEvtType(t pubsdk.EventType) uint32 {
	if idx, ok := evtTypeIndex[string(t)]; ok {
		return idx
	}
	return 0xFFFFFFFF // 未知类型：子进程 typeMask 用 0 匹配全部，此值不影响
}

func decodeEvtType(idx uint32) pubsdk.EventType {
	if int(idx) < len(evtTypeNames) {
		return pubsdk.EventType(evtTypeNames[idx])
	}
	return ""
}

func evtTypeMask(types ...pubsdk.EventType) uint32 {
	var mask uint32
	for _, t := range types {
		if idx, ok := evtTypeIndex[string(t)]; ok {
			mask |= 1 << idx
		}
	}
	return mask
}

// ---- 事件环共享段布局（§3.6）----

const (
	evtRingMagic   uint32 = 0x48455654 // "HEVT"
	evtRingVersion uint32 = 1
	evtRingCap     uint32 = 8192 // 2^13，满足流式场景突发（实验 4）
	evtRingSlotLen uint32 = 32   // seq(8)+type(4)+off(4)+len(4)+pad(12)

	evtOffMagic    uint32 = 0
	evtOffVersion  uint32 = 4
	evtOffWriteSeq uint32 = 8
	evtOffCap      uint32 = 16
	evtOffSlots    uint32 = 20

	evtTypeRawInput       uint32 = 0
	evtTypeAgentOutput    uint32 = 1
	evtTypeAgentLLMChain  uint32 = 2
	evtTypeToolCall       uint32 = 3
	evtTypeReasoning      uint32 = 4
	evtTypeStage          uint32 = 5
	evtTypeSystem         uint32 = 6
	evtTypeReasoningDelta uint32 = 7
	evtTypeContentDelta   uint32 = 8
	evtTypeSkillDetected  uint32 = 9
	evtTypeMax            uint32 = 10

	evtHeaderSize = 20
	evtArenaCap   = 64 * 1024
	evtTotalSize  = int(evtHeaderSize + evtRingCap*evtRingSlotLen + evtArenaCap)
)

// ---- 内核侧：EvtRing ----

type EvtRing struct {
	data      []byte
	writeSeq  atomic.Uint64
	cap       uint32
	slotsBase uint32
	arenaBase uint32
	arenaCap  uint32
	arenaUsed atomic.Uint32
	mu        sync.Mutex
}

func NewEvtRing(data []byte) (*EvtRing, error) {
	if uint32(len(data)) < evtHeaderSize+evtRingCap*evtRingSlotLen+evtArenaCap {
		return nil, fmt.Errorf("事件环段太小：需要 %d，实际 %d", evtTotalSize, len(data))
	}
	if got := binary.LittleEndian.Uint32(data[evtOffMagic:]); got != evtRingMagic {
		return nil, fmt.Errorf("事件环魔数不匹配（0x%x）", got)
	}
	return &EvtRing{
		data:      data,
		cap:       evtRingCap,
		slotsBase: evtOffSlots,
		arenaBase: evtOffSlots + evtRingCap*evtRingSlotLen,
		arenaCap:  evtArenaCap,
	}, nil
}

func (r *EvtRing) Init() {
	binary.LittleEndian.PutUint32(r.data[evtOffMagic:], evtRingMagic)
	binary.LittleEndian.PutUint32(r.data[evtOffVersion:], evtRingVersion)
	binary.LittleEndian.PutUint32(r.data[evtOffCap:], r.cap)
	r.writeSeq.Store(0)
}

// Written 返回已写入的事件条数（含因环满而只标记未落盘的那些）。
//
// 供诊断与测试观测「某次 Publish 是否真的通过了 EventRing」——
// 这比读内部字段稳定，也是关停退订验证所需的可观察量。
func (r *EvtRing) Written() uint64 { return r.writeSeq.Load() }

// WritePush post-and-forget，**绝不阻塞**（§3.6 约束 B）。
func (r *EvtRing) WritePush(evtType pubsdk.EventType, payload []byte) {
	seq := r.writeSeq.Add(1) - 1
	var off uint32
	r.mu.Lock()
	used := r.arenaUsed.Load()
	if used+uint32(len(payload)) <= r.arenaCap {
		off = r.arenaBase + used
		r.arenaUsed.Store(used + uint32(len(payload)))
		copy(r.data[off:], payload)
	}
	r.mu.Unlock()
	idx := seq % uint64(r.cap)
	slotOff := r.slotsBase + uint32(idx)*evtRingSlotLen
	binary.LittleEndian.PutUint64(r.data[slotOff:], seq)
	binary.LittleEndian.PutUint32(r.data[slotOff+8:], encodeEvtType(evtType))
	binary.LittleEndian.PutUint32(r.data[slotOff+12:], off)
	binary.LittleEndian.PutUint32(r.data[slotOff+16:], uint32(len(payload)))
	binary.LittleEndian.PutUint64(r.data[evtOffWriteSeq:], seq+1)
}

// ---- 子进程侧：EvtConsumer ----

type EvtConsumer struct {
	ringData []byte
	evtfd    evtfdReader
	handler  func(*pubsdk.Event) error
	readSeq  uint64
	typeMask uint32
	mu       sync.Mutex
	running  bool
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	doneOnce sync.Once // close 纪律：见 internal/safeclose
}

type evtfdReader interface {
	Read(b []byte) (int, error)
}

// evtfdFder 是可取出原始 fd 的通知句柄（*os.File 满足）。
//
// 有它才能用 poll(2) 加超时等待可读。**为什么不能用 SetReadDeadline**：
// eventfd/pipe 经 os.NewFile 包装后不会注册进 Go netpoller（os.NewFile 对
// 非 open 得到的 fd 一律按非 pollable 处理），Read 退化成阻塞 syscall，
// SetReadDeadline 返回错误且不生效——Run 会永久卡在 syscall.Read，
// 此时调用方若已 munmap 区域（host.Close），恢复后的 drainEvents 就是
// 读已解除映射的内存：SIGSEGV，recover 捕不到。
type evtfdFder interface {
	Fd() uintptr
}

func NewEvtConsumer(ringData []byte, evtfd evtfdReader, mask uint32, handler func(*pubsdk.Event) error) *EvtConsumer {
	return &EvtConsumer{
		ringData: ringData,
		evtfd:    evtfd,
		handler:  handler,
		typeMask: mask,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// readWakeInterval 是 poll 超时间隔：保证 Run 至少这么频繁地检查 stop。
const readWakeInterval = 100 * time.Millisecond

func (c *EvtConsumer) Run() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
		safeclose.Safe(&c.doneOnce, c.done)
	}()

	buf := make([]byte, 8)
	for {
		select {
		case <-c.stop:
			return
		default:
		}

		// 显式 poll(2) 加超时：只有它能打破阻塞 Read，让 Stop 真正生效。
		if f, ok := c.evtfd.(evtfdFder); ok {
			ready, err := pollEvtfd(int(f.Fd()), int(readWakeInterval/time.Millisecond))
			if err != nil {
				return // 通知句柄已失效，退出以免空转
			}
			if !ready {
				continue // 超时：回到顶部检查 stop
			}
		}

		// 阻塞等待内核通知（走 netpoller，只 park goroutine）
		if _, err := c.evtfd.Read(buf); err != nil {
			select {
			case <-c.stop:
				return
			default:
			}
			continue
		}
		c.drainEvents()
	}
}

func (c *EvtConsumer) drainEvents() {
	writeSeq := binary.LittleEndian.Uint64(c.ringData[evtOffWriteSeq:])
	cap := uint64(evtRingCap)
	for c.readSeq < writeSeq {
		// 每处理一条就检查一次 stop：handler 可能很慢，
		// 不加这个检查的话 Stop 要等整轮 drain 完才生效。
		select {
		case <-c.stop:
			return
		default:
		}
		if writeSeq-c.readSeq > cap {
			c.readSeq = writeSeq - cap
		}
		idx := c.readSeq % cap
		slotOff := evtOffSlots + uint32(idx)*evtRingSlotLen
		seq := binary.LittleEndian.Uint64(c.ringData[slotOff:])
		etype := binary.LittleEndian.Uint32(c.ringData[slotOff+8:])
		off := binary.LittleEndian.Uint32(c.ringData[slotOff+12:])
		slen := binary.LittleEndian.Uint32(c.ringData[slotOff+16:])
		if seq != c.readSeq {
			// slot 已被新事件覆盖——逐个扫太慢（溢出场景 readSeq=0 要跳 100+ 步），
			// 直接跳到 writeSeq 附近找下一个可读 slot。
			// 简化：溢出后直接跳到 writeSeq - cap（最旧的可读事件）。
			if writeSeq > cap {
				c.readSeq = writeSeq - cap
			} else {
				c.readSeq = writeSeq
			}
			continue
		}
		// 位掩码过滤
		if c.typeMask != 0 && (1<<etype)&c.typeMask == 0 {
			c.readSeq++
			continue
		}
		if off > 0 && slen > 0 && uint64(off)+uint64(slen) <= uint64(len(c.ringData)) {
			payload := make([]byte, slen)
			copy(payload, c.ringData[off:off+slen])
			var evt pubsdk.Event
			if err := json.Unmarshal(payload, &evt); err == nil {
				c.handler(&evt)
			}
		}
		c.readSeq++
	}
}

// Stop 请求消费者退出。
//
// 非阻塞：Stop 返回**不代表** Run 已退出（最多 readWakeInterval 后退出）。
// 若要在 Stop 之后释放 ringData（host.Close 会 munmap 整个区域），
// 必须先 Stop() 再 Wait()。
func (c *EvtConsumer) Stop() {
	safeclose.Safe(&c.stopOnce, c.stop)
}

// Wait 阻塞至 Run 退出。返回后 drainEvents 保证不会再访问 ringData。
func (c *EvtConsumer) Wait() { <-c.done }

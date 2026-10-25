package proc

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// Host 持有**被全部子进程插件共享的统一内存区域**，是共享数据面的
// 所有权中心（§3.3/§3.4）。
//
// ❗ 为什么必须共享一块区域：
// 若每个插件各持一块段，则「内核 ctx → 段 → 插件改 → 回读 ctx」退化成
// 副本模型——两个插件各写各的段、各自回读，最后回读者覆盖前者，
// lost update 原样复现（§8.4 实测 35.8~36.8%）。
//
// 统一区域（§13.1）：单 memfd 包含 SuperBlock + StageContext + EvtRing
// + Exchange Arena。子进程经 fd 3 mmap 同一 memfd → 同一份物理页，
// 区域头 SuperBlock 告知各 segment 的偏移与大小。
//
// fd 分配：fd 3 = 统一区域，fd 4 = eventfd。
type Host struct {
	memfd   *os.File
	data    []byte         // 统一区域完整 mmap
	unified *unifiedRegion // SuperBlock 解析结果
	seg     *Segment       // StageContext segment（位于 unified ctxData）
	shmSize int            // 统一区域总大小

	// arena 是跨进程共享内存分配器（§13.2 重设计），由内核独占管理：
	// 插件通过 RPC 申请/归还，不做任何分配决策。
	//
	// 分配与回收都在内核进程内进行，一把 mu 即可保证安全，
	// 不存在跨进程分配器那种“共享游标被两个进程各自更新”的竞态。
	arena *arenaRegion

	// nextOwner 给每个插件进程分配一个不透明 owner ID，供 ReclaimOwner 使用。
	nextOwner atomic.Uint32

	// 事件通知（独立于共享段）
	evtfd       *os.File // Unix：eventfd/pipe 读端（fd 4）。Windows 为 nil。
	evtNotifyFd int      // 通知句柄的平台无关标识
	evtRing     *EvtRing // 内核侧事件环句柄（位于 unified evtData）

	evtSubscriber EvtRingSubscriber
	locks         *lockRegistry
	stageMu       sync.Mutex
	coordMu       sync.Mutex
	coord         *stageCoordinator
	sup           *Supervisor

	// closing 置真后 beginStage 直接拒绝新 stage（Close 与新 stage 进场的赛跑：
	// unmap 后若又进来一个 stage，它照样会去写已释放的段）。
	// 用 atomic 是为了让 beginStage 无需额外加锁就能读到。
	closing atomic.Bool
}

// NewHost 创建共享段（平台层 allocShm + 布局初始化）。
//
// 段的**传递机制**按平台分开（shmalloc_*.go），但**布局**完全一致：
//   - Linux：memfd，经 ExtraFiles 传继承 fd
//   - macOS：立即 unlink 的临时文件（无 memfd_create），同样走 fd 继承
//   - Windows：命名 FileMapping（无 fd 继承语义），插件按名字打开
//
// 三者共同点：全部插件看到同一份物理页，段内一律用相对偏移而非指针
// （实验 2 已验证各进程 mmap 到不同虚拟地址时偏移解引用仍正确）。
func NewHost() (*Host, error) {
	// 统一区域大小：SuperBlock + StageContext + EvtRing + Exchange Arena
	//
	// +8 是 arena 基址 8 字节对齐的 padding 余量：arenaOff 向上取整可能
	// 吃掉最多 4 字节，预留 8 字节保证 arenaCap 不会小于 arenaPublishSize。
	unifiedSize := superBlockSize + shmDefaultSize + evtTotalSize + int(arenaPublishSize) + 8
	memfd, data, err := allocShm(unifiedSize)
	if err != nil {
		return nil, err
	}

	// 初始化 SuperBlock + 两个 segment
	ur, err := initUnifiedRegion(data, shmDefaultSize, evtTotalSize)
	if err != nil {
		freeShm(memfd, data)
		return nil, err
	}

	// 初始化 Exchange Arena（内核独占的变长块分配器）
	arena, err := initArena(data, ur.arenaOff, ur.arenaCap)
	if err != nil {
		freeShm(memfd, data)
		return nil, fmt.Errorf("共享内存分配器初始化: %w", err)
	}
	if used, total := arena.Stats(); used != 0 || total != ur.arenaCap {
		freeShm(memfd, data)
		return nil, fmt.Errorf("分配器初始化异常：used=%d total=%d", used, total)
	}

	// 创建 StageContext segment（位于 SuperBlock 之后）
	seg, err := NewSegment(ur.ctxData())
	if err != nil {
		freeShm(memfd, data)
		return nil, err
	}

	// 初始化 EvtRing 子区域头部（magic/version/cap）
	evtData := ur.evtData()
	putU32(evtData[evtOffMagic:], evtRingMagic)
	putU32(evtData[evtOffVersion:], evtRingVersion)
	putU32(evtData[evtOffCap:], evtRingCap)

	// 创建 EvtRing segment（位于 StageContext 之后）
	evtRing, err := NewEvtRing(evtData)
	if err != nil {
		freeShm(memfd, data)
		return nil, fmt.Errorf("事件环初始化: %w", err)
	}
	evtRing.Init()

	// eventfd 独立于共享段，仍为单独 fd
	efd, err := evtfdCreate()
	if err != nil {
		freeShm(memfd, data)
		return nil, fmt.Errorf("创建 eventfd: %w", err)
	}

	return &Host{
		sup:         NewSupervisor(),
		memfd:       memfd,
		data:        data,
		unified:     ur,
		seg:         seg,
		shmSize:     unifiedSize,
		arena:       arena,
		evtfd:       evtfdReadFile(efd),
		evtNotifyFd: efd,
		evtRing:     evtRing,
		locks:       &lockRegistry{},
	}, nil
}

// shmDefaultSize 是共享 StageContext 段的大小。
//
// 取 256KB：StageContext 全字段 JSON 化后典型 < 4KB（工具结果中位 93B，§2.5），
// append-only 中间垃圾由 stage 结束时 Compact 回收，256KB 给足余量。
// 全部插件共享一块，总开销恒定，不随插件数增长。
const shmDefaultSize = 256 * 1024

// Close 释放共享段（StageContext + 事件环）。
// Supervisor 返回子进程台账（供 registry 查询/关停）。
func (h *Host) Supervisor() *Supervisor { return h.sup }

func (h *Host) Close() error {
	// 先停全部子进程再拆段：插件还持有映射时 unmap，
	// 它们下一次访问共享段就是 SIGBUS。
	if h.sup != nil {
		h.sup.StopAll(0)
	}

	// ★ 必须在 unmap **之前**退掉事件环订阅。
	//
	// 那些订阅的 handler 会 ring.WritePush（写共享内存）。若让它们留在
	// Bus 上，munmap 之后只要有一条事件经过 Publish，handler 就写已解除
	// 映射的内存 ⇒ SIGSEGV。注意 Bus.safeCall 的 recover **捕不到**它
	// （runtime 致命错误不是 panic），所以后果是整个 homed 被杀。
	//
	// 顺序要求：StopAll 之后（不再有新订阅进来）、freeShm 之前。
	if c, ok := h.evtSubscriber.(evtCloser); ok && c != nil {
		c.Close()
	}

	// ★ 等正在进行的 stage 收尾（2026-10-08 修 shutdown use-after-free）。
	//
	// 上面的注释只处理了**事件环**一路写者，但漏了 **stage 数据面**：
	//
	// 上面的注释只处理了**事件环**一路写者，但漏了 **stage 数据面**：
	// beginStage 的首进者会在 stageMu 保护下调 Segment.WriteAll（写共享段），
	// 而这条路径**不在**事件环订阅里，退订阅根本不影响它。
	//
	// 实测后果：每次关闭都 SIGSEGV（栈：stageCoordinator.enter →
	// Segment.WriteAll → writeLocal → descOffset），
	// 即首次进入的新进程日志里必然带一份“fatal error: fault”。
	// 这还使部署验证门形同虚设——分不清“新版本崩了”还是“旧版本关闭时崩”。
	//
	// 为何用 stageMu 而不是另加 done 通道：stageMu 的语义已经是
	// 「本轮 stage 独占共享段」（首进者取、最后离开者放，见 beginStage/
	// endStage）。取到它就意味着“此刻没有 stage 在写段”，正是我们要等的东西，
	// 不必再造一个会与它失去同步的并行状态。
	//
	// 锁序：stageMu 是这组锁的最后一级（beginStage 是 coordMu → stageMu，
	// endStage 不取锁），此处独占获取不会成环。
	// 注意 StageHost 的 handler 已由 StopAll/上层停过，不会无限持锁。
	h.closing.Store(true) // 先挡住新 stage，再等旧 stage 退出
	h.stageMu.Lock()

	var firstErr error
	if h.data != nil {
		// 先让段不可再写（置 nil 后 segdata 已失效），
		// 再真正 unmap——完成写入的 stage 已在上面等完。
		if err := freeShm(h.memfd, h.data); err != nil && firstErr == nil {
			firstErr = err
		}
		h.data, h.memfd, h.unified, h.seg = nil, nil, nil, nil
	}
	h.stageMu.Unlock()
	if h.evtfd != nil {
		h.evtfd.Close()
		h.evtfd = nil
	}
	evtfdClose(h.evtNotifyFd)
	return firstErr
}

// beginStage 由插件 handler 进入时调用。
//
// 首个进入者：获取 stageMu（独占共享段）→ 把内核 StageContext 写入段。
// 后续进入者：仅递增 inflight。
//
// enter() 在 coordMu 内完成，两个原因：
//  1. 首进者的 WriteAll 未结束前不能让后到者拿到 coord 就去读共享段
//     （旧码的后到者 enter 立即返回，可能读到写一半的段）。
//  2. 与 endStage 的摘除互斥，防止后到者挂进一个正在收尾的协调器
//     （具体见 endStage 的注释）。
//
// 锁序：stageMu → coordMu。endStage 只解锁 stageMu、不获取，所以无环。
func (h *Host) beginStage(sc *pubsdk.StageContext) (*stageCoordinator, error) {
	// Close 已开始：共享段即将/已被 unmap，此时再写段就是 use-after-free
	// （runtime fatal，recover 捕不到）。直接拒绝，让上层得到明确错误。
	if h.closing.Load() {
		return nil, errors.New("proc: 内核正在关闭，拒绝新 stage")
	}
	h.coordMu.Lock()
	if h.coord == nil {
		// 首个进入者：独占共享段直到本次 stage 全部插件离开。
		// 必须先放 coordMu 再取 stageMu，不能反序。
		h.coordMu.Unlock()
		h.stageMu.Lock()
		h.coordMu.Lock()
		if h.coord == nil {
			coord := newStageCoordinator(h.seg)
			h.coord = coord
			if err := coord.enter(sc, true); err != nil {
				// 注意：runStage 的 defer endStage(coord) 是在 beginStage
				// 返回 err 的检查之后才注册的，所以这条路径上
				// endStage 永远不会被调用——stageMu 必须在此自行释放，
				// 否则整个 stage 通道永久卡死。
				h.coord = nil
				h.coordMu.Unlock()
				h.stageMu.Unlock()
				return nil, err
			}
			h.coordMu.Unlock()
			h.locks.bind(coord.lock)
			return coord, nil
		}
		// 双检失败：等锁期间已有其他插件建好协调器，退回后到者路径。
		h.stageMu.Unlock()
	}

	coord := h.coord
	if err := coord.enter(sc, false); err != nil {
		h.coordMu.Unlock()
		return nil, err
	}
	h.coordMu.Unlock()
	h.locks.bind(coord.lock)
	return coord, nil
}

// endStage 由插件 handler 返回时调用。
// 最后离开者：把共享段结果读回内核 StageContext → 压实 arena → 释放 stageMu。
//
// coordMu 必须覆盖「递减 inflight → 判定最后离开者 → 摘除 h.coord」全过程。
// 旧码把 leave() 放在 coordMu 之外，留出了这个窗口（即 2026-09-04 06:56:18
// 线上 fatal error: sync: unlock of unlocked mutex 的真因）：
//
//	A.endStage: leave() → inflight 1→0, last=true，尚未摘除 h.coord
//	B.beginStage: 看到 h.coord != nil，以「后到者」身份 enter，inflight 0→1
//	              （后到者不取 stageMu）
//	A.endStage: h.coord = nil；stageMu.Unlock()            ← 第 1 次
//	B.endStage: leave() → inflight 1→0, last=true → stageMu.Unlock()  ← 第 2 次 💥
//
// B 从未持有 stageMu（它是后到者），却因为挂进了一个正在收尾的协调器
// 而成为“最后离开者”，于是对同一把锁解了两次。sync.Mutex 的双重解锁是
// runtime fatal，**recover 捕不到**——这就是为何 stage.go / stages.go 里
// 那两层 recover 全部失效、整个 homed 直接死掉的原因。
func (h *Host) endStage(coord *stageCoordinator) error {
	h.coordMu.Lock()
	last, sc, written := coord.depart()
	if last && h.coord == coord {
		h.coord = nil
	}
	h.coordMu.Unlock()
	if !last {
		return nil
	}
	// finish 必须在 stageMu.Unlock() 之前：先放锁会让下一轮 stage
	// 在回读未完时就改写共享段。
	err := coord.finish(sc, written)
	h.stageMu.Unlock()
	// ★ 先代为释放可能泄漏的锁，再摘除路由（2026-10-07）。
	//   顺序很重要：先释放才有意义（锁已无人持有时本就没东西可释放），
	//   而摘除必须最后——否则释放期间来的 stage.lock 会打到 nil 上。
	//
	//   这两件事缺一不可：
	//     · releaseIfLeaked —— 插件异常路径未 unlock 时，锁不会毒化下一轮
	//     · unbind       —— 下一轮的 stage.lock 不得再打到本轮的旧锁上
	if h.locks.releaseIfLeaked(coord.lock) {
		log.Printf("[proc] stage 协调器结束时清理了泄漏的 stage 锁")
	}
	h.locks.unbind(coord.lock)
	return err
}

// ForceReleaseLock 在插件进程崩溃时释放其可能持有的 stage 锁（实验 9 自愈机制）。
func (h *Host) ForceReleaseLock(plugin string) bool {
	return h.locks.forceRelease(plugin)
}

// Segment 暴露共享段（供诊断与测试）。
func (h *Host) Segment() *Segment { return h.seg }

// stageCoordinator 跟踪一次 stage 执行中参与插件的进出。
type stageCoordinator struct {
	seg  *Segment
	lock *stageLock

	mu       sync.Mutex
	inflight int
	written  bool
	ctxRef   *pubsdk.StageContext
}

func newStageCoordinator(seg *Segment) *stageCoordinator {
	return &stageCoordinator{seg: seg, lock: newStageLock()}
}

// enter 登记一个插件进入本次 stage；first 为真时把内核状态写入共享段。
func (c *stageCoordinator) enter(sc *pubsdk.StageContext, first bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight++
	if !first || c.written {
		return nil
	}
	c.ctxRef = sc
	if err := c.seg.WriteAll(sc); err != nil {
		c.inflight--
		return fmt.Errorf("写入共享段: %w", err)
	}
	c.written = true
	return nil
}

// leave 登记一个插件离开；返回是否为最后一个离开者。
//
// 拆成两段：depart() 只动计数（由 endStage 在 coordMu 内调用，使
// 「递减 → 判定最后者 → 摘除 h.coord」成为原子操作），finish() 做
// 共享段回读与压实。本方法保留给单测用。
func (c *stageCoordinator) leave() (last bool, err error) {
	last, sc, written := c.depart()
	if !last {
		return last, nil
	}
	return last, c.finish(sc, written)
}

// depart 递减 inflight 并报告是否为最后离开者。
func (c *stageCoordinator) depart() (last bool, sc *pubsdk.StageContext, written bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight--
	return c.inflight == 0, c.ctxRef, c.written
}

// finish 把共享段结果读回内核 StageContext 并压实 arena
// （此时无插件持锁，满足 §3.3 的压实前提）。
func (c *stageCoordinator) finish(sc *pubsdk.StageContext, written bool) error {
	if !written || sc == nil {
		return nil
	}
	if rErr := c.seg.ReadInto(sc); rErr != nil {
		return fmt.Errorf("回读共享段: %w", rErr)
	}
	if reclaimed := c.seg.Compact(); reclaimed > 0 {
		log.Printf("[proc] stage 结束，arena 压实回收 %d 字节", reclaimed)
	}
	return nil
}

// Arena 返回跨进程共享槽池。
func (h *Host) Arena() *arenaRegion { return h.arena }

// Generation 返回统一区域当前 generation（SharedRef 校验用）。
func (h *Host) Generation() uint64 { return h.unified.generation() }

// NextOwnerID 分配一个插件专用的槽 owner ID。
//
// 从 1 开始（OwnerHost=0 保留给内核），单调递增，不会重复。
func (h *Host) NextOwnerID() uint32 { return h.nextOwner.Add(1) }

// ReclaimRefs 强制回收指定的一批扩容块（取消超时时由 Process 调用）。
//
// 这些块的 owner 是**插件**，回收决策来自内核——内核此刻是全权的
// （见 arena.go 文件头的所有权模型）。不回收就是"取消了却瞒下内存"：
// 插件已静默退出，永远不会归还，且全程无日志。
func (h *Host) ReclaimRefs(refs []SharedRef) int { return h.arena.ReclaimRefs(refs) }

// ReclaimOwner 回收某个 owner 名下所有槽（插件退出时调用）。
func (h *Host) ReclaimOwner(owner uint32) int { return h.arena.ReclaimOwner(owner) }

// ShmSize 返回共享段大小（供诊断/日志）。
func (h *Host) ShmSize() int { return h.shmSize }

// EvtRing 返回内核侧事件环句柄。
func (h *Host) EvtRing() *EvtRing { return h.evtRing }

// Evtfd 返回通知读端的 *os.File（Unix；eventfd/pipe）。
// Windows 返回 nil——命名 Event 不是文件句柄，用 EvtNotifyFd 代替。
func (h *Host) Evtfd() *os.File { return h.evtfd }

// EvtNotifyFd 返回通知句柄的平台无关标识，供 EventRing 写通知。
//
// Unix 是真 fd；Windows 是映射到命名 Event 句柄的伪 fd。
// EvtfdNotify 接受这个值并按平台分派。
func (h *Host) EvtNotifyFd() int { return h.evtNotifyFd }

// SetEvtSubscriber 注入事件环订阅接口（由 Registry 在创建 Host 后设置）。
func (h *Host) SetEvtSubscriber(sub EvtRingSubscriber) { h.evtSubscriber = sub }

// EvtData 返回事件环段 mmap 数据（子进程消费者用）。
func (h *Host) EvtData() []byte { return h.unified.evtData() }

// EvtfdReadFile 返回 eventfd 的 *os.File（供子进程读取消费）。
func (h *Host) EvtfdReadFile() *os.File { return h.evtfd }

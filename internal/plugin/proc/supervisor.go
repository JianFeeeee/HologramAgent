package proc

import (
	"fmt"
	safeclose "github.com/JianFeeeee/HomeAgent/internal/safeclose"
	"log"
	"sort"
	"sync"
	"time"
)

// Supervisor 是内核侧**唯一**的子进程台账。
//
// 为什么必须有它，而不是让每个 Plugin 各自管好自己的 Process：
//
//  1. **没有台账就没有"全部子进程"这个概念**。内核关停时只能遍历 registry 的
//     插件表逐个 Stop，而 registry 表是按插件名索引的——握手失败、Start 中途
//     出错、或刚 spawn 还没进表就崩了的进程，registry 根本不知道它们存在，
//     那些进程会变成孤儿（ppid=1）继续跑，还持有共享段映射。
//  2. **诊断面缺失**。此前 `/api/manager/status` 之类的接口拿不到"实跑几个子进程、
//     各自 PID 多少、活了多久、崩过几次"，运维只能 ps | grep。
//  3. **收割保证**。每个 Process 自带一根 waitLoop 立即 wait4(2)，Supervisor
//     只负责登记/注销与聚合视图；两者配合才能做到"进程一死内核立刻知道"。
//
// 生命周期：Spawn 成功握手后 track，Process.markExited 里 untrack。
type Supervisor struct {
	mu    sync.RWMutex
	procs map[string]*Process
	// closed 后拒绝新的 track，防止关停竞态里又冒出新进程。
	closed bool
}

// NewSupervisor 创建空台账。
func NewSupervisor() *Supervisor {
	return &Supervisor{procs: make(map[string]*Process)}
}

// track 登记一个已握手成功的子进程。
//
// 同名覆盖是正常情况（重载：旧进程 untrack 早于或晚于新进程 track 都可能，
// 取决于 Kill 与 Spawn 的交错），故不报错，只在真覆盖时留日志。
func (s *Supervisor) track(p *Process) {
	if p == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// 关停途中还有进程完成握手：立即结束它，不让它活过内核。
		go p.Kill()
		return
	}
	if old, ok := s.procs[p.name]; ok && old != p {
		log.Printf("[proc] 台账中 %s 已有 pid=%d，被 pid=%d 覆盖", p.name, old.PID(), p.PID())
	}
	s.procs[p.name] = p
}

// untrack 注销（进程已退出）。只有当表里那一项确实是它时才删，
// 避免重载时新进程被旧进程的退出回调误删。
func (s *Supervisor) untrack(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.procs, name)
}

// Get 按插件名取子进程句柄。
func (s *Supervisor) Get(name string) (*Process, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.procs[name]
	return p, ok
}

// Count 返回在册子进程数。
func (s *Supervisor) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.procs)
}

// ProcInfo 是单个子进程的运行期快照。
type ProcInfo struct {
	Name  string `json:"name"`
	PID   int    `json:"pid"`
	Alive bool   `json:"alive"`
	Bin   string `json:"bin"`
}

// List 返回全部在册子进程的快照（按插件名排序，便于稳定展示）。
func (s *Supervisor) List() []ProcInfo {
	s.mu.RLock()
	out := make([]ProcInfo, 0, len(s.procs))
	for name, p := range s.procs {
		alive := true
		select {
		case <-p.Exited():
			alive = false
		default:
		}
		out = append(out, ProcInfo{Name: name, PID: p.PID(), Alive: alive, Bin: p.bin})
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// StopAll 停止全部在册子进程：先并发发 plugin.stop 走优雅路径，
// 到期仍在的一律 Kill。
//
// 这是内核关停时**必须**调的：不调则子进程被 init 收养成孤儿，
// 继续持有共享段映射（段已被内核 unmap，它们下次访问就是 SIGBUS），
// 并且下次 homed 启动时同名插件会与残留进程抢同一份外部资源
// （qq 的 WS 连接、browser 的 chromium profile 锁）。
func (s *Supervisor) StopAll(timeout time.Duration) {
	s.mu.Lock()
	s.closed = true
	procs := make([]*Process, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.mu.Unlock()

	if len(procs) == 0 {
		return
	}
	log.Printf("[proc] 关停 %d 个子进程插件", len(procs))

	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func(pr *Process) {
			defer wg.Done()
			if err := pr.Stop(); err != nil {
				log.Printf("[proc] 停止 %s: %v", pr.Name(), err)
			}
		}(p)
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	go func() { wg.Wait(); safeclose.Safe(&doneOnce, done) }()

	if timeout <= 0 {
		timeout = stopGracePeriod * 2
	}
	select {
	case <-done:
	case <-time.After(timeout):
		// 优雅停止没在预算内完成：剩下的直接 Kill。
		// 不能无限等——homed 关停被单个卡住的插件拖住比杀掉它更糟。
		var stuck []string
		var killers sync.WaitGroup
		for _, p := range procs {
			select {
			case <-p.Exited():
			default:
				stuck = append(stuck, fmt.Sprintf("%s(pid=%d)", p.Name(), p.PID()))
				// ★ 必须等 Kill 完成，不能发射后不管。
				// 本函数返回后调用方（Host.Close）立刻 freeShm 解除映射，
				// 而 Kill 内部要等 markExited 跑完（含 onExit → ReclaimOwner，
				// 那是要读共享内存的）。不等就 unmap ⇒ SIGSEGV。
				// Kill 自带 killReapTimeout 上限，不会无限拖住关停。
				killers.Add(1)
				go func(pr *Process) {
					defer killers.Done()
					_ = pr.Kill()
				}(p)
			}
		}
		if len(stuck) > 0 {
			log.Printf("[proc] %v 内未优雅退出，强制结束: %v", timeout, stuck)
		}
		killers.Wait()
	}
}

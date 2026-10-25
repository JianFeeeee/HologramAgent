package proc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/meta"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
)

// Process 管理一个外部插件子进程：spawn / 双向 JSON-RPC / 优雅停止 / 崩溃检测。
//
// 生命周期：沿用内核既有的注册面摘除 + 退避重启机制（见 stats/registry）。
//
// 与 C ABI 路径的关键差异：
//   - **崩溃隔离**：插件 panic 只让子进程退出，homed 存活（今日 panic 跨 C 栈可带崩内核）
//   - **真正的取消**：Kill() 后 OS 回收全部资源，零泄漏
//     （今日 cgo 调用不可抢占，超时后 OS 线程永久占用，现网已泄漏 26 次，§9.3）
//   - **可同步等真实结果**：RPC 天然可等应答
//     （今日 cgo 不可嵌套，output_send 只能异步、永远假成功，§9.4）
type Process struct {
	name string
	bin  string
	dir  string

	cmd    *exec.Cmd
	stdin  *bufio.Writer
	stdout io.ReadCloser

	// stdinFile / stdoutFile 是父进程侧的管道端（手工 os.Pipe，非 cmd.StdinPipe）。
	// 持有它们才能在退出时主动 Close，逼 readLoop 从 Scan 里出来。
	stdinFile  *os.File
	stdoutFile *os.File

	// writeMu 串行化 stdin 写入：NDJSON 帧不能交错，否则对端解析错乱。
	writeMu sync.Mutex

	// inflight 记录在途调用：请求 ID → *call。
	//
	// 每个 *call 自带状态机（见 callstate.go）。用状态而不是"map 里有没有
	// 这个键"来表达调用是否还活着——取消后条目**不删**，只是把状态推进到
	// callCanceled，于是迟到的应答仍能与它的调用对上，从而被**有依据地**处理，
	// 而不是靠"找不到 id 就丢"这种副作用式正确。
	mu       sync.Mutex
	nextID   uint64
	inflight map[uint64]*call
	closed   bool

	// handler 处理插件反向发起的调用（51 个 core.* method）。
	handler RequestHandler

	// onCancelIgnored 在「发出取消后超过 cancelGrace 仍未停手」时回调。
	//
	// 语义严格限定为**取消超时**，不包含业务超时：长任务超时是正常的
	// （时间给少了），而"喊停之后还不停"才是插件失去响应的证据。
	onCancelIgnored func(callID uint64, grace time.Duration)

	// onCancelHonored 在「已被取消的调用最终仍回了应答」时回调——
	// 说明插件收到了 cancel 且及时停手了。用于把连续计数清零。
	onCancelHonored func(callID uint64)

	// reclaimBlocks 强制回收一批扩容块（由 Host 注入）。
	//
	// 为什么用回调而不是让 Process 持有 *Host：Host 依赖 Process
	// （Spawn 时把自己传进来会成环），而且 Process 只在这一处需要 arena 能力。
	// 注入一个函数把依赖面收到最小。
	reclaimBlocks func(refs []SharedRef) int

	// exited 在进程被收割后关闭，用于唤醒所有等待者。
	exited     chan struct{}
	exitedOnce sync.Once
	exitOnce   sync.Once
	exitErr    atomic.Pointer[error]
	readerWG   sync.WaitGroup
	waiterWG   sync.WaitGroup
	readyOnce  sync.Once
	ready      chan struct{}

	// waitErr 由**唯一的** waitLoop 写入：cmd.Wait() 的返回值。
	// waitDone 关闭后 waitErr 才可读。
	waitErr      error
	waitDone     chan struct{}
	waitDoneOnce sync.Once

	// onExit 在进程退出时回调（内核用它喂 plugin_health.recordCrash，
	// 以及 ForceRelease 释放该插件持有的 stage 锁）。
	onExit func(name string, err error)

	// sup 是内核的集中进程表（可为 nil，单测直接 Spawn 时）。
	sup *Supervisor

	// shmSize 是握手时告知插件的共享段大小（0 表示本插件不用共享段）。
	shmSize int
	// evtRingSize 是事件环段大小（0 表示不支持事件环）。
	evtRingSize int
}

// RequestHandler 处理插件 → 内核的调用。
// 返回值会被序列化为 Response.Result；返回 error 则序列化为 Response.Error。
type RequestHandler func(method string, params json.RawMessage) (interface{}, error)

// Options 是 Spawn 的可选配置。
type Options struct {
	// Dir 是子进程工作目录（通常为插件目录）。
	Dir string
	// Env 追加到子进程环境变量。
	Env []string
	// ExtraFiles 传给子进程的额外文件描述符（fd 3 起）。
	// 共享内存段的 memfd 经此传递——子进程 mmap fd 3 即挂载同一段。
	ExtraFiles []*os.File
	// ShmSize 是共享段大小，握手时告知插件（与 ExtraFiles[0] 的 memfd 对应）。
	ShmSize int
	// EvtRingSize 是事件环段大小（0 表示不支持事件环）。
	EvtRingSize int
	// Handler 处理插件反向调用。
	Handler RequestHandler
	// OnExit 进程退出回调。
	OnExit func(name string, err error)
	// Supervisor 是内核的集中进程表；为 nil 时不纳管（单测路径）。
	Supervisor *Supervisor
	// HandshakeTimeout 建链超时，默认 10s。
	HandshakeTimeout time.Duration
}

// 默认超时。
const (
	// DefaultHandshakeTimeout 是内核与插件**建链（握手）**的默认超时。
	//
	// 导出它是因为它是**唯一真相源**：插件的 manifest 换算
	// （PluginManifest.HandshakeTimeout）必须引用这个常量，
	// 否则两处会各自漂移（"以为改了一处其实另一处没改"）。
	DefaultHandshakeTimeout = 10 * time.Second
	// stopGracePeriod 是发出 plugin.stop 后等待进程自行退出的时间。
	// 超时则 Kill——**这是"真正的取消"**，对比 cgo 路径超时后线程永久泄漏。
	stopGracePeriod = 5 * time.Second
	// killReapTimeout 是 SIGKILL 后等待 waitLoop 收割的上限。
	// 正常情况 wait4 微秒级返回；超过说明卡在不可中断的内核态。
	killReapTimeout = 2 * time.Second
)

// defaultHandshakeTimeout 是包内旧名（保持既有调用点不改）。
const defaultHandshakeTimeout = DefaultHandshakeTimeout

// ErrProcessExited 表示子进程已退出，调用无法完成。
var ErrProcessExited = errors.New("proc: 插件进程已退出")

// Spawn 启动插件子进程并完成握手。
func Spawn(name, bin string, opts Options) (*Process, error) {
	if opts.Handler == nil {
		return nil, fmt.Errorf("proc: %s 缺少 RequestHandler（插件无法回调内核）", name)
	}
	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = defaultHandshakeTimeout
	}

	cmd := exec.Command(bin)
	cmd.Dir = opts.Dir
	// stderr 直通内核日志：插件的 panic 栈、log 输出可直接看到。
	cmd.Stderr = os.Stderr
	// 插件自成进程组（Setpgid）。为何必须：
	//
	// 插件会用 exec.Command 拉孙进程（bili→yt-dlp→ffmpeg、editdoc→python、
	// browser→chromium —— 实测 8 个插件都这么干，且无一做进程组隔离）。
	// 不分组时孙进程与内核同组，Kill 只能打给插件本体，孙进程变孤儿：
	//  1. 它继续持有插件 stdout 管道的写端 ⇒ 内核 readLoop 永不 EOF；
	//  2. 它自己活成孤儿，继续占 CPU/网络/文件句柄。
	// 分组后 Kill 可以 kill(-pgid) 一次带走整棵树。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	cmd.ExtraFiles = opts.ExtraFiles
	applyProcAttr(cmd)

	// 管道手工创建而非用 cmd.StdinPipe/StdoutPipe。
	//
	// 原因：cmd.Wait() 会等待并**关闭** StdinPipe/StdoutPipe 创建的管道，
	// 且文档明确要求“读完再 Wait”。既然现在有一根专职的 waitLoop 立即
	// Wait（不等 readLoop），就必须自己控制管道生命期，否则会与
	// os/exec 的内部关闭竞争，在 readLoop 里读到 "file already closed"。
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("proc: %s stdin 管道: %w", name, err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, fmt.Errorf("proc: %s stdout 管道: %w", name, err)
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW

	p := &Process{
		name:        name,
		bin:         bin,
		dir:         opts.Dir,
		cmd:         cmd,
		stdin:       bufio.NewWriter(stdinW),
		stdout:      stdoutR,
		stdinFile:   stdinW,
		stdoutFile:  stdoutR,
		inflight:    make(map[uint64]*call),
		handler:     opts.Handler,
		exited:      make(chan struct{}),
		ready:       make(chan struct{}),
		waitDone:    make(chan struct{}),
		onExit:      opts.OnExit,
		sup:         opts.Supervisor,
		shmSize:     opts.ShmSize,
		evtRingSize: opts.EvtRingSize,
	}

	if err := cmd.Start(); err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("proc: 启动 %s (%s): %w", name, bin, err)
	}
	// 子进程已继承它们，父进程侧关掉对端。
	// stdoutW 必须关：否则子进程死后写端仍被父进程持有，readLoop 永不到 EOF。
	stdinR.Close()
	stdoutW.Close()

	// 专职收割协程：这是 cmd.Wait() 的**唯一**调用点。
	//
	// 为何不能靠 readLoop 的 EOF：EOF 只说明 stdout 写端全部关闭，而插件
	// fork 出去的孙子进程（browser 拉 chromium、editdoc 拉 python）继承着
	// 同一个 stdout：插件本体死了但孙子还持有写端，EOF 就不来，
	// 内核完全感知不到插件已死（进程表里是僵尸，注册表里一切正常）。
	// wait 直接盯进程本身，不受 fd 继承影响。
	p.waiterWG.Add(1)
	go p.waitLoop()

	p.readerWG.Add(1)
	go p.readLoop()

	// 等 readLoop 就绪后再握手，避免应答早于 reader 启动而丢失。
	<-p.ready

	if err := p.handshake(timeout); err != nil {
		p.Kill()
		return nil, err
	}
	if p.sup != nil {
		p.sup.track(p)
	}
	return p, nil
}

// Name 返回插件名。
func (p *Process) Name() string { return p.name }

// PID 返回子进程 PID（用于诊断/日志）。
func (p *Process) PID() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Exited 返回一个在进程退出时关闭的通道。
func (p *Process) Exited() <-chan struct{} { return p.exited }

// ExitError 返回进程退出原因（正常退出为 nil）。
func (p *Process) ExitError() error {
	if e := p.exitErr.Load(); e != nil {
		return *e
	}
	return nil
}

func (p *Process) handshake(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	raw, err := p.CallContext(ctx, MethodHandshake, HandshakeParams{
		CoreVersion: meta.Version,
		PluginName:  p.name,
		FieldCount:  uint32(stageFieldCount),
		ShmSize:     p.shmSize,
		EvtRingSize: p.evtRingSize,
	})
	if err != nil {
		return fmt.Errorf("proc: %s 握手失败: %w", p.name, err)
	}
	var res HandshakeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("proc: %s 握手应答解析失败: %w", p.name, err)
	}
	// 主版本号是唯一兼容判据：跨主版本必是被破坏性变更分开的两端。
	if want, got := MajorVersion(meta.Version), MajorVersion(res.SDKVersion); want != got {
		return fmt.Errorf("proc: %s 主版本不匹配（插件 SDK %s，内核 %s）——请用配套 hmapdev 重编",
			p.name, res.SDKVersion, meta.Version)
	}
	log.Printf("[proc] %s 已建链（pid=%d sdk=%s core=%s）",
		p.name, p.PID(), res.SDKVersion, meta.Version)
	return nil
}

// readLoop 读取子进程 stdout 的 NDJSON 帧，分派为「应答」或「插件发起的请求」。
//
// 参考 clawhubadapter/sidecarProcess 的成熟做法：大 buffer 防长行截断、
// pending 表定位应答、退出时唤醒全部等待者。
// summarizeFrame 把一段非 JSON 文本压成单行摘要（诊断用）。
//
// 用途：让"插件往 stdout 打印了什么"一眼可见 —— 否则只看到
// 「invalid character 'x'」而不知道是哪句话（实测排查代价见 readLoop 里那段注释）。
//
// 处理：
//   - 控制字符（\r\n\t 等）替换成空格，避免多行打散日志
//   - 超长截断并标注原始字节数
func summarizeFrame(b []byte, max int) string {
	var sb strings.Builder
	for _, c := range b {
		if c < 0x20 || c == 0x7f {
			sb.WriteByte(' ')
			continue
		}
		sb.WriteByte(c)
		if sb.Len() >= max {
			break
		}
	}
	out := strings.TrimSpace(sb.String())
	if len(b) > max {
		return out + fmt.Sprintf(" …（共 %d 字节，已截断）", len(b))
	}
	return out
}

func (p *Process) readLoop() {
	defer p.readerWG.Done()

	scanner := bufio.NewScanner(bufio.NewReader(p.stdout))
	// 单帧上限 1MB：控制面帧本应很小（工具结果中位 93B），
	// 超大 payload 应走共享内存 arena 而非 RPC 帧。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	safeclose.Safe(&p.readyOnce, p.ready)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// 帧可能是 Response（有 id 无 method）或 Request（有 method）。
		var probe struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// ★★★ 这条日志必须**可定位**（2026-10-16 实测修正）。
			//
			// ## 原问题的形态
			//
			// 子进程插件的 **stdout 是 JSON-RPC 协议通道**，stderr 才是日志通道
			// （SDK 侧 log.SetOutput(os.Stderr)）。插件若用 `fmt.Printf` 写日志，
			// 那句话会以**裸文本**进入协议流 ⇒ 就是这里。
			//
			// 原日志只报「收到非法 JSON 帧（N 字节）: invalid character 'x'」：
			//   · **不显示内容** ⇒ 看不到是"哪句话"混进来了；
			//   · **不提原因** ⇒ 看起来像协议/内核故障，不像"插件自己 printf"。
			//
			// 实测代价：生产环境累计 **1038 条**这种报错（weather 113 / rss 103 /
			// calendar 102 / 第三方 huawei_smarthome 720），而真因只是那 4 个插件
			// 用了 fmt.Printf。定位它花了一整轮排查。
			//
			// ⇒ 现在把**内容摘要**与**可能原因 + 修法**一起打出来。
			// 限长是因为坏帧可能很大（插件打了长文本/二进制），
			// 截断到 160 字节足以认出"这是哪句日志"。
			log.Printf("[proc] %s 收到非法 JSON 帧（%d 字节）: %v\n"+
				"        ↑ 内容摘要: %s\n"+
				"        可能原因：插件往 **stdout** 打印了日志，而 stdout 是"+
				"JSON-RPC **协议通道**（stderr 才是日志通道）。\n"+
				"        修法：把插件里的 fmt.Printf/fmt.Println 改成 log.Printf"+
				"（脚手架模板 1.4.0 起已修正）。",
				p.name, len(line), err, summarizeFrame(line, 160))
			continue
		}

		if probe.Method != "" {
			// 插件发起的调用：拷贝一份再交给 goroutine（scanner 会复用底层数组）
			buf := make([]byte, len(line))
			copy(buf, line)
			go p.serveRequest(buf)
			continue
		}

		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			log.Printf("[proc] %s 应答解析失败: %v", p.name, err)
			continue
		}
		p.deliverResponse(&resp)
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[proc] %s 读取 stdout 出错: %v", p.name, err)
	}

	// stdout 关闭（EOF）通常意味着进程结束——2.5ms 内即可感知（实验 6）。
	//
	// 但 EOF **不是**权威信号：插件 fork 的孙子进程继承同一 stdout 写端时，
	// 插件本体死了 EOF 也不会到。真正的死亡判定在 waitLoop。
	// 这里只等 waitLoop 的结果（若进程确实已退，它立即就给）。
	<-p.waitDone
	p.markExited()
}

// waitLoop 是内核侧**唯一**的 cmd.Wait() 调用点，每个子进程一根。
//
// 为何需要专职协程而不是靠 readLoop 的 EOF：
//  1. **EOF 不等于进程死**。插件用 exec.Command 拉起的孙子进程（browser 拉
//     chromium、editdoc 拉 python）默认继承插件的 stdout。插件被 kill 后
//     孙子还活着持有写端，readLoop 就永远阻在 Scan 上——内核根本不知道
//     插件已经死了，工具调用一直超时，自愈也永不触发。
//  2. **不收割就是僵尸进程**。不调 Wait 的已退出子进程以 Z 状态占着 PID 槽位。
//  3. **反应速度**。Wait 底层是 wait4(2)，内核侧退出即返回（微秒级），
//     比任何轮询健康检查都快，也不消耗 CPU。
func (p *Process) waitLoop() {
	defer p.waiterWG.Done()
	p.waitErr = p.cmd.Wait()
	safeclose.Safe(&p.waitDoneOnce, p.waitDone)

	// 主动拆管道：若孙子进程仍持有 stdout 写端，readLoop 不会自己退，
	// 关掉读端逼它从 Scan 里出来（报 file already closed，已预期）。
	if p.stdoutFile != nil {
		_ = p.stdoutFile.Close()
	}
	if p.stdinFile != nil {
		_ = p.stdinFile.Close()
	}

	p.markExited()
}

// markExited 唤醒所有等待者、触发 onExit 回调（幂等，两条路径可并发调用）。
//
// 这是「把 panic 捕获换成进程退出检测」的落点（§2.3）。
// 注意：不在此处调 cmd.Wait()——它属于 waitLoop，Wait 并非并发安全，
// 两处调会报 "wait: no child processes" 或丢失真实退出码。
func (p *Process) markExited() {
	p.exitOnce.Do(func() {
		<-p.waitDone // 保证 waitErr 可读
		if p.waitErr != nil {
			e := fmt.Errorf("插件进程 %s 异常退出: %w", p.name, p.waitErr)
			p.exitErr.Store(&e)
			log.Printf("[proc] %s 退出: %v", p.name, p.waitErr)
		} else {
			log.Printf("[proc] %s 正常退出", p.name)
		}

		// 进程没了：所有在途调用都无法完成。按状态机收敛——
		// 只有**仍在 pending** 的才需要被唤醒（已取消的调用方早已返回，
		// 它不等结果；若也去唤醒，会往无人读的通道里塞东西并掩盖真实状态）。
		p.mu.Lock()
		p.closed = true
		var toWake []*call
		for id, c := range p.inflight {
			delete(p.inflight, id)
			if c.transitionTo(callPending, callFailed) {
				toWake = append(toWake, c)
			}
		}
		p.mu.Unlock()

		// 唤醒所有在途调用，避免调用方挂死到自己的超时
		for _, c := range toWake {
			c.ch <- &Response{Error: ErrProcessExited.Error()}
		}

		// ★ 顺序至关重要：onExit 必须在 safeclose.Safe(&p.exitedOnce, p.exited) **之前**完成。
		//
		// onExit（内核侧即 Plugin.handleExit）会调 Host.ReclaimOwner 回收该插件
		// 残留的共享槽——**那是要读共享内存区域的**。而 exited 一关闭，
		// Stop()/Kill() 就返回，StopAll 随即返回，调用方（Host.Close）立刻
		// freeShm 解除映射；若此刻 onExit 还没跑完，ReclaimOwner 就成了读
		// 已 munmap 的内存 —— SIGSEGV（recover 捕不到，直接杀进程）。
		//
		// 实测崩溃栈（2026-09-25，全量 go test 偶发）：
		//   readLoop(process.go:334) → markExited → once.Do
		//     → onExit → handleExit → Host.ReclaimOwner
		//       → arenaRegion.ReclaimOwner → blockBase → getU32 → SIGSEGV
		//
		// 因此 exited 的语义是「**完全**收尾完毕」，而不是「进程已死」：
		// 任何等待者（Stop/Kill/CallContext/Alive）在它关闭后都可以安全地
		// 释放共享内存、卸载资源。
		if p.sup != nil {
			p.sup.untrack(p.name)
		}
		if p.onExit != nil {
			p.onExit(p.name, p.ExitError())
		}

		safeclose.Safe(&p.exitedOnce, p.exited)
	})
}

// serveRequest 处理插件反向发起的调用。
func (p *Process) serveRequest(line []byte) {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		log.Printf("[proc] %s 请求解析失败: %v", p.name, err)
		return
	}

	// panic 隔离：插件的回调参数可能触发内核 handler 的 panic，
	// 不能让它带崩整个 readLoop（更不能带崩 homed）。
	var (
		result interface{}
		err    error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("内核 handler 处理 %s 时 panic: %v", req.Method, r)
				log.Printf("[proc] %s: %v", p.name, err)
			}
		}()
		result, err = p.handler(req.Method, req.Params)
	}()

	// ID==0 是通知，不回应答（§2.4 约束 B：post-and-forget）
	if req.ID == 0 {
		if err != nil {
			log.Printf("[proc] %s 通知 %s 处理失败: %v", p.name, req.Method, err)
		}
		return
	}

	resp := Response{ID: req.ID}
	if err != nil {
		resp.Error = err.Error()
	} else if result != nil {
		if b, mErr := json.Marshal(result); mErr == nil {
			resp.Result = b
		} else {
			resp.Error = fmt.Sprintf("结果序列化失败: %v", mErr)
		}
	}
	if wErr := p.writeFrame(&resp); wErr != nil {
		log.Printf("[proc] %s 回写应答失败: %v", p.name, wErr)
	}
}

// writeFrame 序列化并写入一帧（串行化，NDJSON 不能交错）。
func (p *Process) writeFrame(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.stdin.Write(b); err != nil {
		return err
	}
	if err := p.stdin.WriteByte('\n'); err != nil {
		return err
	}
	return p.stdin.Flush()
}

// Call 发起 RPC 并等待应答（无超时上限，由调用方 context 控制）。
func (p *Process) Call(method string, params interface{}) (json.RawMessage, error) {
	return p.CallContext(context.Background(), method, params)
}

// CallContext 发起 RPC 并等待应答，受 ctx 取消/超时控制。
//
// ## ctx 取消时会发生两件事
//
//  1. **调用方立即返回**（旧行为，不变）；
//  2. **向插件进程发一条 call.cancel 通知**，告诉它放弃这个正在跑的 handler。
//
// 第 2 条是 2026-10-25 补的：在此之前取消只是把请求从 pending 表里删掉，
// 插件侧的 handler 毫不知情、继续跑到自己结束，期间占着连接/子进程/文件句柄。
// 结果被丢弃、成本照付 —— 高频超时的工具会持续后台堆积。
//
// ## 为什么不能只靠“把进程杀掉”
//
// 一个插件进程承载多个工具与会话；为一个超时的调用杀掉它，会把同进程里
// 仅存正在正常工作（甚至正在写数据库）的其它调用一起带走。所以必须精确到
// **单次调用**的取消，而这只能靠插件侧配合。
//
// 通知是 fire-and-forget（不等待确认）：
// 取消路径绝不能再变成同步点，否则内核超时后还要等插件回一句“我收到了”。
func (p *Process) CallContext(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("proc: %s 序列化 %s 参数: %w", p.name, method, err)
		}
		raw = b
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("proc: %s 调用 %s: %w", p.name, method, ErrProcessExited)
	}
	p.nextID++
	c := newCall(p.nextID, method)
	p.inflight[c.id] = c
	p.mu.Unlock()

	if err := p.writeFrame(&Request{ID: c.id, Method: method, Params: raw}); err != nil {
		p.retire(c, callFailed)
		return nil, fmt.Errorf("proc: %s 发送 %s: %w", p.name, method, err)
	}

	select {
	case resp := <-c.ch:
		if resp.Error != "" {
			return nil, fmt.Errorf("proc: %s.%s: %s", p.name, method, resp.Error)
		}
		return resp.Result, nil
	case <-ctx.Done():
		// ★ 状态机先落定，再发通知：顺序反过来的话，插件可能在极短时间内
		// 应答并撞上"仍在 pending"的旧状态，导致这次取消被判成普通完成。
		p.abandon(c, ctx.Err())
		return nil, fmt.Errorf("proc: %s 调用 %s: %w", p.name, method, ctx.Err())
	case <-p.exited:
		p.retire(c, callFailed)
		return nil, fmt.Errorf("proc: %s 调用 %s: %w", p.name, method, ErrProcessExited)
	}
}

// deliverResponse 按**状态机**处置一条来自插件的应答。
//
// 这是唯一的分发实现：readLoop 与单测都走这里，避免"测试替身与真实路径
// 行为不一致"——第一版我在测试里复制了一份分发逻辑，结果 onCancelHonored
// 只在真路径里被调用，测试却显示通过（假绿）。
//
// 四种情形，各有明确处置：
//
//	pending  → done     ：正常完成，把结果交给等待者。
//	canceled → done     ：调用方早已放弃。应答**有主**（我们知道它属于哪次
//	                      调用、那次调用为什么被放弃），所以这不是"未知 id"，
//	                      而是"预期之内的迟到应答"——同时它是一个**正面信号**：
//	                      插件收到了 cancel 并停手了。
//	done/failed         ：重复应答（协议错误），记 Warn 并丢弃。
//	表里没有该 id        ：真正的未知 id，记 Warn。
func (p *Process) deliverResponse(resp *Response) {
	p.mu.Lock()
	c, ok := p.inflight[resp.ID]
	if ok && c.State() != callPending && c.State() != callCanceled {
		p.mu.Unlock()
		log.Printf("[proc] %s 丢弃重复应答 id=%d（该调用已处于 %s）",
			p.name, resp.ID, c.State())
		return
	}
	if ok {
		delete(p.inflight, resp.ID)
	}
	p.mu.Unlock()

	if !ok {
		log.Printf("[proc] %s 丢弃未知 id=%d 的应答（不属于任何在途调用）", p.name, resp.ID)
		return
	}

	switch {
	case c.transitionTo(callPending, callDone):
		c.ch <- resp
	case c.transitionTo(callCanceled, callDone):
		// ★ 正面信号：插件收到了 cancel 并且停手了。要把"取消无响应"的
		// 连续计数清零——判据是"**连续** N 次"而不是"累计 N 次"。
		if p.onCancelHonored != nil {
			p.onCancelHonored(c.id)
		}
		// 通道容量 1 且此处无读者，不写入（写了会阻塞 readLoop）。
	default:
		log.Printf("[proc] %s 丢弃无法归类的应答 id=%d（状态 %s）",
			p.name, resp.ID, c.State())
	}
}

// CallByID 返回某个在途调用的状态句柄（不存在返回 nil）。
//
// 用途：插件申请扩容块时带 CallID，内核据此把块登记到**那次调用**名下
// （见 call.trackBlock）。归属跟着调用本身走，不需要另一张表。
func (p *Process) CallByID(id uint64) *call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight[id]
}

// retire 把调用从在途表里摘掉并把状态推进到 to。
//
// 返回"是否由本次调用完成了迁移"——只有赢得迁移的一方才该承担后续动作
// （唤醒等待者 / 发取消通知），避免重复。
func (p *Process) retire(c *call, to callState) bool {
	// ★ 顺序：**先迁移状态，再摘表**。
	//
	// 反过来（先摘表后迁移）会留一个窗口：此时状态可能仍是 pending，而
	// 表里已经没有它——一个同时到达的应答就会走"未知 id"分支，
	// 而我们本来能说清它属于哪次调用。先迁移则保证：任何时刻
	// "表里有它" ⇒ 它的状态是可解释的。
	//
	// retire 的语义是"调用收尾"：既可能收 pending（正常结束/进程退出），
	// 也可能收 canceled（宽限期到了，对端仍未回答）。
	//
	// ★ 但**不收 done/failed**：那两个是终态，对它们做任何迁移都是协议错误
	// （重复应答/重复收尾），放行会让状态机失去单向性。这由
	// legalTransition 兜底——done/failed 的合法目标列表为空，CAS 必然失败。
	moved := c.transitionTo(callPending, to) || c.transitionTo(callCanceled, to)
	p.mu.Lock()
	if p.inflight[c.id] == c {
		delete(p.inflight, c.id)
	}
	p.mu.Unlock()
	return moved
}

// abandon 在调用方放弃（超时/取消）时收敛状态，并通知对端停手。
//
// 这里体现的就是"取消是一等状态"：条目**不删**、状态改成 callCanceled，
// 稍后若插件仍然回了一个应答，readLoop 能认出它属于一个**已取消**的调用，
// 从而有依据地丢弃（而不是靠"找不到 id"这种副作用）。
func (p *Process) abandon(c *call, cause error) {
	// pending → canceled：只有赢的一方负责发通知。
	if c.transitionTo(callPending, callCanceled) {
		p.cancelCall(c.id, cause)
	}
	// ★ **不摘表**。这正是本次改造的关键点：
	//
	// 摘了表，插件随后回的应答就成了"未知 id"——我们明明知道它属于哪次
	// 调用、也知道那次调用是被取消的，却只能当成异常丢弃。
	// 保留条目后，readLoop 能按状态做明确处置（canceled→done，静默丢弃），
	// 而且"这条应答为什么被丢"是有依据的，不是靠"找不到 key"这种副作用。
	//
	// 条目何时消失？三条路径：
	//  1. 迟到的应答到达时由 readLoop 摘（正常路径）；
	//  2. 进程退出时统一清；
	//  3. ★ **宽限期**到了还没回 → onCancelIgnored（见下）。
	//     对端若永远不回（卡死/已死但进程还在），条目必须有一个上界，
	//     而"取消频繁 + 对端从不回答"正是最该防的组合。
	time.AfterFunc(cancelGrace, func() { p.expireCanceled(c) })
}

// cancelGrace 是**取消宽限期**：从发出 call.cancel 起，等它停手的时间。
//
// 为什么必须有这个超时，以及它和业务超时的区别（这是两个不同的问题）：
//
//	业务超时  = 这次任务跑太久   → 正常现象，只说明该换个更小的任务
//	取消超时  = 喊停之后还不停   → **异常信号**，说明插件卡死/失去响应
//
// 只有后者才该被当成"插件不稳定"。business 超时本身不能作为判据：
// 长任务本来就该超时，那是调用方给的时间不够，不是插件的错。
//
// 取 5 秒：取消是软信号，插件可能正在收尾（写盘、断开连接）。
// 正常模板收到 call.cancel 后 ms 级就退出（取消判据实测 < 100ms），
// 5 秒足够覆盖任何合理收尾，又远短于业务超时上限（10 分钟）。
const cancelGrace = 5 * time.Second

// reclaimRefs 调用注入的回收函数（未注入时返回 0，便于独立测试）。
func (p *Process) reclaimRefs(refs []SharedRef) int {
	if p.reclaimBlocks == nil || len(refs) == 0 {
		return 0
	}
	return p.reclaimBlocks(refs)
}

// SetReclaimBlocks 注入扩容块强制回收能力（Host 创建 Process 后调用）。
func (p *Process) SetReclaimBlocks(fn func(refs []SharedRef) int) { p.reclaimBlocks = fn }

// OnCancelHonored 设置「取消被响应」回调。
func (p *Process) OnCancelHonored(fn func(callID uint64)) { p.onCancelHonored = fn }

// OnCancelIgnored 设置取消超时回调（Plugin 创建 Process 后调用）。
func (p *Process) OnCancelIgnored(fn func(callID uint64, grace time.Duration)) {
	p.onCancelIgnored = fn
}

// expireCanceled 在取消宽限期到期时收尾一次已被取消的调用。
//
// 三件事，缺一不可：
//
//	① 强制回收该调用名下的扩容块 —— 插件已静默退出，永远不会归还它们。
//	   不回收就是"取消了却瞒下内存"：无日志、只涨不落。
//	② 计数并上报「取消无响应」—— 这是插件不稳定的直接证据。
//	③ 状态收敛到 failed（不是 done）：这次调用确实没有正常完成。
func (p *Process) expireCanceled(c *call) {
	if !p.retire(c, callFailed) {
		return // 宽限期内已经收到应答/已被回收，无需处理
	}
	// ① 回收块：refs 的 owner 是插件，但回收决策来自内核（见 ReclaimRefs）。
	if n := p.reclaimRefs(c.takeBlocks()); n > 0 {
		log.Printf("[proc] %s 调用 %d 取消后无响应：已强制回收 %d 个扩容块", p.name, c.id, n)
	}
	// ② 上报：由上层判定稳定性并决定是否重启。
	//    这里只负责"如实汇报事实"，不做重启决策——proc 不知道插件生命周期策略。
	log.Printf("[proc] %s 调用 %d 在取消后 %v 内未停手（插件失去响应）", p.name, c.id, cancelGrace)
	if p.onCancelIgnored != nil {
		p.onCancelIgnored(c.id, cancelGrace)
	}
}

// Notify 发送不需要应答的通知（ID=0，fire-and-forget）。
//
// 用于事件投递等路径：内核发通知**绝不等待消费者**（§2.4 约束 B——
// 流式输出逐 token 发布，任何等待都会造成卡顿）。
func (p *Process) Notify(method string, params interface{}) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return ErrProcessExited
	}
	return p.writeFrame(&Request{Method: method, Params: raw})
}

// cancelCall 告知插件进程放弃某个仍在执行的调用。
//
// 设计要点（值得写在代码里，否则后人很容易把它改成阻塞式）：
//
//   - **不等待应答**：用 Notify（ID=0）。取消是 best-effort 通知，
//     插件若不认识这个方法（旧版模板）会当成未知请求忽略，不影响任何流程；
//     若回一个“清不了”反而会把取消路径变成新的阻塞点。
//   - **失败只记日志**：进程已死、管道已满都属于“本来就不用再取消了”。
//     绝不能把通知失败上升为调用方的错误，否则超时会变成两种不同的错。
//   - **先清 pending 再发**：反过来的话，插件可能在极短时间内就跑完了并
//     回了应答，而那条应答会落在一个已废弃的 id 上（虽然会被丢弃，但日志
//     和 pending 清理会变得难以推理）。
func (p *Process) cancelCall(id uint64, cause error) {
	reason := "canceled"
	if cause != nil {
		reason = cause.Error()
	}
	if err := p.Notify(MethodCallCancel, CallCancelParams{CallID: id, Reason: reason}); err != nil {
		log.Printf("[proc] %s 发送调用取消失败（id=%d，可能已退出）: %v", p.name, id, err)
	}
}

// Stop 优雅停止：发 plugin.stop → 等宽限期 → 超时则 Kill。
//
// 插件侧收到 plugin.stop 后应先跑 RunStopHandlers 再 Stop()，
// 与 C ABI 路径的停止链路语义一致（§2.3 已验证被正确调用）。
func (p *Process) Stop() error {
	select {
	case <-p.exited:
		return nil // 已经退出
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), stopGracePeriod)
	defer cancel()
	if _, err := p.CallContext(ctx, MethodPluginStop, nil); err != nil {
		// 停止调用失败不影响后续 Kill——插件可能已经崩了
		if !errors.Is(err, ErrProcessExited) {
			log.Printf("[proc] %s plugin.stop 失败（将强制结束）: %v", p.name, err)
		}
	}

	select {
	case <-p.exited:
		return nil
	case <-time.After(stopGracePeriod):
		log.Printf("[proc] %s 宽限期内未退出，强制结束", p.name)
		return p.Kill()
	}
}

// Kill 强制结束子进程并回收资源。
//
// **这是 C ABI 路径拿不到的能力**：cgo 调用不可被 Go runtime 抢占或取消，
// 超时后该 OS 线程永久占用（实验 14 实测 20 次调用线性泄漏 +18 线程）。
// 子进程模型下 Kill 后 OS 回收全部资源，零泄漏。
func (p *Process) Kill() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	// 杀**整个进程组**（负 pid = 进程组），不只是插件本体。
	//
	// 插件拉起的孙进程（bili→yt-dlp→ffmpeg 等，实测 8 个插件都拉孙进程）
	// 继承插件的 stdout 管道写端。只杀本体的话孙进程变孤儿：
	// 它继续持有写端 ⇒ 内核 readLoop 永远等不到 EOF ⇒ 关停挂死。
	// 线上症状：StopAll 里只有 bili 报 "SIGKILL 后 2s 仍未被收割"，
	// 之后近 90 秒无日志，systemd SIGKILL。
	//
	// 兜底：Setpgid 未生效（老插件/平台不支持）时退回杀本体，
	// 否则 kill(-pgid) 会失败而插件还活着。
	pid := p.cmd.Process.Pid
	killErr := syscall.Kill(-pid, syscall.SIGKILL)
	if killErr != nil {
		// 进程组不存在或无权限：退回只杀本体。
		// 不能直接返回错误：Setpgid 未生效时（老插件、非 Unix 平台）
		// 负 pid 会报 ESRCH，此时必须仍然把插件本体杀掉。
		killErr = p.cmd.Process.Kill()
	}
	// 等 waitLoop 收割完成。不再在此兜底调 markExited：
	// cmd.Wait 只能由 waitLoop 调一次，两处调会报 "wait: no child processes"。
	select {
	case <-p.exited:
	case <-time.After(killReapTimeout):
		// SIGKILL 后仍未收割：进程卡在不可中断的内核态（D 状态，如 NFS I/O）。
		// 不能无限等，否则重载路径整体挂死；留日志供定位。
		log.Printf("[proc] %s SIGKILL 后 %v 仍未被收割（进程可能卡在内核态）", p.name, killReapTimeout)
	}
	// readerWG.Wait 必须有界：孙进程持有 stdout 写端时 readLoop 永不返回，
	// 无超时就是"任何插件泄漏一个孙进程都能拖死整个关停"。
	// 超时后主动关掉读端，强制 readLoop 从 Scan 里出来（file already closed，
	// 已在 waitLoop 里被列为预期错误）。
	readerDone := make(chan struct{})
	var readerDoneOnce sync.Once
	go func() {
		p.readerWG.Wait()
		safeclose.Safe(&readerDoneOnce, readerDone)
	}()
	select {
	case <-readerDone:
	case <-time.After(killReapTimeout):
		log.Printf("[proc] %s 的 stdout 读取未在 %v 内结束（孙进程可能仍持有写端），强制关闭读端",
			p.name, killReapTimeout)
		if p.stdoutFile != nil {
			_ = p.stdoutFile.Close()
		}
		select {
		case <-readerDone:
		case <-time.After(killReapTimeout):
			// 极端情况：关管道也没能让它退出。不再等 —— 宁可让这次
			// Stop 少等 2 秒，也不能把关停无限期挂住。
			log.Printf("[proc] %s 读端关闭后 readLoop 仍未退出，放弃等待", p.name)
		}
	}
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return fmt.Errorf("proc: 结束 %s: %w", p.name, killErr)
	}
	return nil
}

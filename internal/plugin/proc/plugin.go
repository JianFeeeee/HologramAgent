package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// Plugin 是 registry 可加载的子进程插件，与内置插件同构的启停接口。
//
// 生命周期：
//
//	New()        创建（尚未 spawn）
//	Start(core)  spawn 子进程 → 握手（传共享段 fd）→ plugin.init → plugin.start
//	             （plugin.start 期间插件反向注册工具/阶段/通道）
//	Stop()       plugin.stop → 宽限期 → 必要时 Kill
//	Close()      强制结束（registry 卸载/重载路径）
//
// **共享段不属于 Plugin**：它属于 Host，被全部子进程插件共享。
// 若每插件一段，「内核 ctx → 段 → 插件改 → 回读 ctx」在多插件下会退化成
// 副本模型，lost update 原样复现（§8.4）。
type Plugin struct {
	name   string
	bin    string
	dir    string
	config map[string]interface{}

	host    *Host
	proc    *Process
	handler *coreHandler

	// env 追加到子进程环境变量（测试用；生产由 registry 按需设置）。
	env []string

	// onCrash 由 registry 注入，把进程退出喂给 plugin_health.recordCrash（§2.3）。
	onCrash func(name string, err error)

	// caps 是 manifest 声明的能力集（§3.8 权限梯度）。
	caps *capabilitySet

	// handshakeTimeout 是插件**自行声明**的建链超时（manifest 的
	// handshake_timeout_sec）。<=0 ⇒ 用 DefaultHandshakeTimeout。
	// 见 docs/zh/kernel-modularization-design.md §2.4.2。
	handshakeTimeout time.Duration

	// ownerID 是本插件在共享槽池里的身份（由 Host 分配）。
	// 内核用它校验 arena.free 的归属，并在插件退出时回收残留槽。
	ownerID uint32

	stopOnce sync.Once

	// stopping 标记「本次退出是内核主动发起的」，用于压掉 onCrash。
	//
	// 必要性：Stop() 宽限期超时与 Close() 都走 Process.Kill()，
	// 而 Kill 产生的 `signal: killed` 是非 nil 的 waitErr——若不区分，
	// 重载/禁用/卸载这些**内核自己发起**的停止会被 handleExit 当成崩溃上报，
	// 触发一轮多余的自动重启（重载路径下等于把刚装好的插件又推倒一次）。
	stopping atomic.Bool
}

// New 创建子进程插件（不启动进程）。
//
// host 必须是全部子进程插件共用的实例（由 registry 创建一次）。
// capabilities 来自 manifest 的 capabilities 字段；为空时不限制（存量插件向后兼容）。
//
// 建链超时用内核默认值。需要按插件声明覆盖时用 NewWithTimeout。
func New(name, bin, dir string, config map[string]interface{}, host *Host, onCrash func(string, error), capabilities ...string) *Plugin {
	return NewWithTimeout(name, bin, dir, config, host, onCrash, 0, capabilities...)
}

// NewWithTimeout 同 New，但可指定**建链（握手）超时**。
//
// timeout <= 0 ⇒ 用 DefaultHandshakeTimeout（与 New 行为一致）。
//
// ## 为何单独一个构造函数而不是改 New 的签名
//
// `New` 末尾是变参 `capabilities ...string`。中间插一个 duration 参数
// 会让所有调用点错位（值会静默地用错），而编译器**不会**报错
// （变参位置可吞类型兼容的实参）——那是本仓最怕的"静默错误"。
// 加一个新构造函数既保持旧签名不变，又把新能力显式化。
func NewWithTimeout(name, bin, dir string, config map[string]interface{}, host *Host, onCrash func(string, error), timeout time.Duration, capabilities ...string) *Plugin {
	return &Plugin{
		name:             name,
		bin:              bin,
		dir:              dir,
		config:           config,
		host:             host,
		onCrash:          onCrash,
		caps:             newCapabilitySet(capabilities),
		handshakeTimeout: timeout,
	}
}

// Name 实现 sdk.Plugin。
func (p *Plugin) Name() string { return p.name }

// PID 返回子进程号；未启动或已退出返回 0。
// 供 pluginmgr 呈现「插件实际在跑哪个进程」。
func (p *Plugin) PID() int {
	if p.proc == nil {
		return 0
	}
	if !p.Alive() {
		return 0
	}
	return p.proc.PID()
}

// Alive 报告子进程是否仍存活。
func (p *Plugin) Alive() bool {
	if p.proc == nil {
		return false
	}
	select {
	case <-p.proc.Exited():
		return false
	default:
		return true
	}
}

// Start 启动子进程并完成注册。
//
// core 是内核为该插件构建的能力面（internal/sdk.PluginSDK 天然满足 CoreSDK）。
func (p *Plugin) Start(core CoreSDK) error {
	if p.host == nil {
		return fmt.Errorf("proc: %s 缺少共享段 Host", p.name)
	}

	// 领一个槽池身份；插件退出时用它回收残留槽。
	p.ownerID = p.host.NextOwnerID()

	p.handler = &coreHandler{
		sdk:     core,
		name:    p.name,
		host:    p.host,
		owner:   p.ownerID,
		locks:   p.host.locks,
		evtRing: p.host.evtSubscriber,
		caps:    p.caps,
	}
	// 反向调用闭包：注册回调时捕获，运行期经 RPC 打到插件进程。
	p.handler.invokeTool = p.invokeTool
	p.handler.invokeCleaner = p.invokeCleaner
	p.handler.invokeStageFn = p.invokeStage
	p.handler.invokeOutput = p.invokeOutput

	// ★ 显式把插件目录传给**插件进程**（随机化之前的隐式 cwd 不可靠：
	// 插件自己 chdir 后就失效，而 rawfile 的路径解析依赖它）。
	pluginEnv := append([]string{}, p.env...)
	pluginEnv = append(pluginEnv,
		"HA_PLUGIN_DIR="+p.dir,
		"HA_PLUGIN_RAWFILE_DIR="+filepath.Join(p.dir, "rawfile"),
		"HA_PLUGIN_WORK_DIR="+filepath.Join(p.dir, "work"),
	)

	proc, err := Spawn(p.name, p.bin, Options{
		Dir: p.dir,
		// 插件自行声明的建链超时（<=0 时 Spawn 内部回退到默认值）。
		HandshakeTimeout: p.handshakeTimeout,
		// 共享段的传递机制按平台不同（shmpass_*.go）：
		// Unix 经 ExtraFiles 传继承 fd（3=统一共享内存区域 SuperBlock+StageContext+EvtRing，
		// 4=事件通知 eventfd）；Windows 无 fd 继承语义，改用命名内核对象，名字经环境变量传入。
		// 权威定义在 shmpass_unix.go 的 procExtraFilesForShm，修改时三处必须同步。
		Env:         append(pluginEnv, p.host.procEnvForShm()...),
		ExtraFiles:  p.host.procExtraFilesForShm(),
		ShmSize:     p.host.shmSize,
		EvtRingSize: evtTotalSize,
		Handler:     p.handler.Handle,
		OnExit:      p.handleExit,
		Supervisor:  p.host.Supervisor(),
	})
	if err != nil {
		return err
	}
	p.proc = proc

	// plugin.init：构造插件实例
	if _, err := proc.Call(MethodPluginInit, PluginInitParams{
		Name:   p.name,
		Config: p.config,
	}); err != nil {
		proc.Kill()
		return fmt.Errorf("proc: %s plugin.init 失败: %w", p.name, err)
	}

	// plugin.start：插件在此期间反向注册工具/阶段/通道
	if _, err := proc.Call(MethodPluginStart, nil); err != nil {
		proc.Kill()
		return fmt.Errorf("proc: %s plugin.start 失败: %w", p.name, err)
	}
	return nil
}

// Stop 优雅停止（实现 sdk.Plugin）。
func (p *Plugin) Stop() error {
	p.stopping.Store(true)
	var err error
	p.stopOnce.Do(func() {
		if p.proc != nil {
			err = p.proc.Stop()
		}
	})
	return err
}

// Close 强制结束子进程。
//
// **这里是真 kill + wait**——对比 cabi 路径的 Close 只做 dlclose，
// 而 dlclose 对 Go c-shared 是 no-op（§1.1，热重载静默失效的根因）。
func (p *Plugin) Close() error {
	p.stopping.Store(true)
	var err error
	p.stopOnce.Do(func() {
		if p.proc != nil {
			err = p.proc.Kill()
		}
	})
	return err
}

// handleExit 在子进程退出时把信号喂给 plugin_health（§2.3 逻辑复用），
// 并释放该插件可能持有的 stage 锁。
//
// 后者是"锁仲裁回内核"的自愈价值：持锁者死亡不会导致全局死锁，
// 无需 robust pthread_mutex（实验 9）。
func (p *Plugin) handleExit(name string, err error) {
	if p.host != nil {
		if p.host.ForceReleaseLock(name) {
			log.Printf("[proc] %s 退出，内核已释放其持有的 stage 锁", name)
		}
		// 回收该插件未归还的共享槽：崩溃的插件无法自己归还，
		// 不回收会让槽池慢慢耗尽，最终所有共享内存调用退化成内联 RPC。
		if n := p.host.ReclaimOwner(p.ownerID); n > 0 {
			log.Printf("[proc] %s 退出，回收 %d 个残留共享槽", name, n)
		}
	}
	// 内核主动停止（Stop/Close，含宽限期超时后的 Kill）不算崩溃：
	// 否则重载/禁用/卸载都会误触发自动重启。
	if p.stopping.Load() {
		return
	}
	if err != nil && p.onCrash != nil {
		p.onCrash(name, err)
	}
}

// ---- 内核 → 插件的反向调用 ----

// invokeTool 在插件进程内执行工具。
//
// 按 **funccall 模型**：内核是 caller，为每次调用**标定一块内存帧**
// （参数段 + 结果预算段）交给插件（callee）。参数永远写在帧里，不再有
// “小 payload 走内联”的按大小分支。
//
// 结果超出预算时插件才向内核申请扩容块，并在引用上打
// sharedRefFlagExpand，内核据此单独归还。
//
// 控制面仍是 RPC（请求 ID 关联、ctx 取消、崩溃唤醒都由 Process 承载）。
func (p *Plugin) invokeTool(name string, args map[string]interface{}) (interface{}, error) {
	if p.proc == nil {
		return nil, ErrProcessExited
	}
	arena := p.host.Arena()
	gen := p.host.Generation()

	argJSON, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("proc: %s 工具 %s 参数序列化失败: %w", p.name, name, err)
	}

	// 内核标定调用帧：参数段 + 结果预算段。
	frame, err := arena.Alloc(OwnerHost, len(argJSON)+toolResultBudget, gen)
	if err != nil {
		return nil, fmt.Errorf("proc: %s 工具 %s 分配调用帧失败: %w", p.name, name, err)
	}
	defer func() { _ = arena.Free(OwnerHost, frame) }()

	area, err := arena.Read(frame, gen)
	if err != nil {
		return nil, fmt.Errorf("proc: %s 工具 %s 读取调用帧失败: %w", p.name, name, err)
	}
	copy(area[:len(argJSON)], argJSON)

	raw, err := p.proc.Call(MethodToolInvoke, ToolInvokeParams{
		Name:    name,
		Frame:   frame,
		ArgsLen: uint32(len(argJSON)),
	})
	if err != nil {
		return nil, err
	}
	var res ToolInvokeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("proc: %s 工具 %s 应答解析失败: %w", p.name, name, err)
	}
	if res.ResultRef.IsZero() {
		return nil, fmt.Errorf("proc: %s 工具 %s 未返回结果引用", p.name, name)
	}
	// 插件申请了扩容块：内核负责归还。
	if res.ResultRef.Flags&sharedRefFlagExpand != 0 {
		defer func() { _ = arena.Free(OwnerHost, res.ResultRef) }()
	}
	data, err := arena.Read(res.ResultRef, gen)
	if err != nil {
		return nil, fmt.Errorf("proc: %s 工具 %s 读取共享结果失败: %w", p.name, name, err)
	}
	var out interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("proc: %s 工具 %s 解析共享结果失败: %w", p.name, name, err)
	}
	return out, nil
}

// invokeCleaner 在插件进程内执行工具或通道注册时提供的 Cleaner 函数。
//
// 数据面参数（输入槽 / 预分配响应槽）由内核在 params 里给出，
// 插件只负责读输入、写结果，不做任何分配。
func (p *Plugin) invokeCleaner(params CleanerInvokeParams) (CleanerInvokeResult, error) {
	if p.proc == nil {
		return CleanerInvokeResult{}, ErrProcessExited
	}
	raw, err := p.proc.Call(MethodCleanerInvoke, params)
	if err != nil {
		return CleanerInvokeResult{}, err
	}
	var res CleanerInvokeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return CleanerInvokeResult{}, fmt.Errorf("proc: %s %s Cleaner %s 应答解析失败: %w", p.name, params.Scope, params.Name, err)
	}
	return res, nil
}

func (p *Plugin) invokeStage(ctx context.Context, stage string, seq uint64) error {
	if p.proc == nil {
		return ErrProcessExited
	}
	raw, err := p.proc.CallContext(ctx, MethodStageInvoke, StageInvokeParams{
		Stage: stage,
		Seq:   seq,
	})
	if err != nil {
		return err
	}
	var res StageInvokeResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("proc: %s stage %s 应答解析失败: %w", p.name, stage, err)
		}
	}
	if res.DirtyFields > 0 {
		log.Printf("[proc] %s stage %s 改写了 %d 个字段", p.name, stage, res.DirtyFields)
	}
	return nil
}

// invokeOutput 经插件输出通道发送，**同步等待真实结果**。
//
// 这是 §9.4 的根治：C ABI 下 cgo 不可嵌套，只能异步 fire-and-forget，
// 导致 output_send 永远返回 {status:queued} + err=nil，模型永远以为发送成功
// （现网 7 天内 2 次消息实际发不出）。进程模型下 RPC 天然可等应答。
func (p *Plugin) invokeOutput(channel string, args map[string]interface{}) (interface{}, error) {
	if p.proc == nil {
		return nil, ErrProcessExited
	}

	argJSON, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("proc: %s 输出通道 %s 参数序列化失败: %w", p.name, channel, err)
	}

	// 与工具调用同一个 funccall 帧模型（§13.6）：payload 全在共享内存，
	// RPC 只传偏移描述符。输出 payload 在真机上可能含图片/文件描述等大字段，
	// 内联会撑爆 stdin/stdout 管道。
	//
	// 帧尾不预留结果区：output 的应答很小（"ok" 或 status map），直接走
	// RPC 应答字段即可，不像 tool.invoke 那样需要几十 KB 的结果预算。
	var params OutputInvokeParams
	params.Channel = channel
	if len(argJSON) > 0 {
		arena := p.host.Arena()
		gen := p.host.Generation()
		frame, err := arena.Alloc(OwnerHost, len(argJSON), gen)
		if err != nil {
			return nil, fmt.Errorf("proc: %s 输出通道 %s 分配调用帧失败: %w", p.name, channel, err)
		}
		defer func() { _ = arena.Free(OwnerHost, frame) }()

		area, err := arena.Read(frame, gen)
		if err != nil {
			return nil, fmt.Errorf("proc: %s 输出通道 %s 读取调用帧失败: %w", p.name, channel, err)
		}
		copy(area[:len(argJSON)], argJSON)
		params.Frame = frame
		params.ArgsLen = uint32(len(argJSON))
	}

	raw, err := p.proc.Call(MethodOutputInvoke, params)
	if err != nil {
		return nil, err // 真实失败上报，模型可感知并重试
	}
	if len(raw) == 0 {
		return map[string]interface{}{"status": "ok"}, nil
	}

	// 结果可能在共享内存里（插件把大结果写回帧结果区）。
	// 先按结构化应答解析；ResultRef 为零则回退到内联字段。
	var outRes OutputInvokeResult
	if jerr := json.Unmarshal(raw, &outRes); jerr == nil && !outRes.ResultRef.IsZero() {
		arena := p.host.Arena()
		gen := p.host.Generation()
		// 插件申请了扩容块：内核负责归还（与 invokeTool 一致）。
		if outRes.ResultRef.Flags&sharedRefFlagExpand != 0 {
			defer func() { _ = arena.Free(OwnerHost, outRes.ResultRef) }()
		}
		data, err := arena.Read(outRes.ResultRef, gen)
		if err != nil {
			return nil, fmt.Errorf("proc: %s 输出通道 %s 读取共享结果失败: %w", p.name, channel, err)
		}
		var out interface{}
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("proc: %s 输出通道 %s 解析共享结果失败: %w", p.name, channel, err)
		}
		return out, nil
	}

	var res map[string]interface{}
	if err := json.Unmarshal(raw, &res); err == nil {
		if _, ok := res["status"]; !ok {
			res["status"] = "sent"
		}
		return res, nil
	}

	// 插件返回的是标量（如 "ok"）——**原样透传，不要伪造 status**。
	//
	// 为什么必须透传：核心 output.go 会把 map 结果格式化成富回执
	// （"已通过 [qq] 通道发送: map[status:sent]"），模型看到"发送成功 +
	// 详情"会把这一步当成"上一步完成、继续下一步"的信号，形成
	// output_send 回声循环。插件（如 qq）刻意返回极简的 "ok" 就是为了
	// 掐断这个信号；早期实现在这里把非 map 响应替换成 {status:sent}，
	// 等于把它又变回富回执——**只改插件永远修不掉这个循环**。
	var scalar interface{}
	if err := json.Unmarshal(raw, &scalar); err != nil {
		return map[string]interface{}{"status": "ok"}, nil
	}
	return scalar, nil
}

// 编译期确认 Plugin 具备 registry 需要的启停形状。
var _ interface {
	Name() string
	Stop() error
	Close() error
} = (*Plugin)(nil)

// 引用一下公开 SDK，确保本文件的类型假设与它同版本。
var _ = pubsdk.StageScopeGlobal

//go:build linux || windows

package agentcli

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/plugin"
	safeclose "github.com/JianFeeeee/HomeAgent/internal/safeclose"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

const (
	DefaultTimeout        = 5 * time.Minute
	ReadBufSize           = 4096
	MaxOutputBuffer       = 128 * 1024
	DefaultNotifyBytes    = 2048            // 积累 2KB 未读输出再通知
	DefaultNotifyInterval = 2 * time.Second // 同一终端两次通知的最小间隔（兜底）
)

// ptyTerm 抽象平台终端后端（Linux PTY / Windows ConPTY）。
type ptyTerm interface {
	Read(buf []byte) (int, error)
	WriteString(s string) (int, error)
	Resize(rows, cols uint16) error
	Running() bool
	Kill() error
	Close() error
}

// terminalRunning 判断终端后端进程是否仍在运行：优先走平台实现，
// 否则回落到 exec.Cmd 的 ProcessState（仅 Linux 传统路径）。
func terminalRunning(t *TerminalSession) bool {
	if p, ok := t.session.(interface {
		Running() bool
	}); ok && p.Running() {
		return true
	}
	return t.cmd != nil && (t.cmd.ProcessState == nil || !t.cmd.ProcessState.Exited())
}

// terminalWatch 终端提醒规则（由 terminal_watch 工具设置）。
type terminalWatch struct {
	interval    time.Duration // 固定时间反馈间隔，0 禁用
	onExit      bool          // 命令执行结束提醒（默认 true）
	bufferBytes int           // 该终端专用缓冲阈值（字节），0 使用全局 notify_bytes
	quiet       bool          // 静默模式：不随输出流通知，仅定时反馈/结束提醒/空闲汇总
}

type TerminalSession struct {
	stopOnce  sync.Once
	doneOnce  sync.Once
	id        string
	command   string
	cmd       *exec.Cmd
	session   ptyTerm
	mu        sync.Mutex
	buf       bytes.Buffer
	createdAt time.Time
	timeout   time.Duration
	closed    bool
	stopCh    chan struct{}
	done      chan struct{}

	// 通知节流字段
	unreadBytes  int           // 最近一次通知后积累的未读字节数
	lastNotify   time.Time     // 最近一次通知时间
	lastData     time.Time     // 最近一次读到的数据时间（用于判定输出停止）
	lastFeedback time.Time     // 最近一次定时反馈时间
	backoff      time.Duration // 输出风暴退避：持续高速输出时通知间隔翻倍
	watch        terminalWatch // 该终端的提醒规则

	// resourcesReleased 标记 releaseResources 是否已执行（幂等保护）。
	// 不与 closed 合用：closed 语义是「用户主动要求关闭」，releaseResources
	// 是「后端资源已释放」，readLoop 自然退出时只后者为真。
	resourcesReleased bool

	// 实时画面推流（terminal_output 事件）
	stream bytes.Buffer // 待推送的增量输出，由 readLoop 每 200ms flush 一次
}

func (t *TerminalSession) Write(input string) (int, error) {
	return t.session.WriteString(input)
}

func (t *TerminalSession) Read(buf []byte) (int, error) {
	return t.session.Read(buf)
}

func (t *TerminalSession) Resize(rows, cols uint16) error {
	return t.session.Resize(rows, cols)
}

func (t *TerminalSession) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()

	safeclose.Safe(&t.stopOnce, t.stopCh)
	// 终止进程并释放 PTY（幂等；readLoop 自然退出时已调过就直接返回）。
	t.releaseResources()
	// 等 readLoop 走完退出流程（它会发最后的 output/停止事件）。
	<-t.done
}

func (t *TerminalSession) ReadOutput() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

func (t *TerminalSession) ReadAndClearOutput() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.buf.String()
	t.buf.Reset()
	return s
}

func (t *TerminalSession) appendOutput(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buf.Len()+len(data) > MaxOutputBuffer {
		excess := t.buf.Len() + len(data) - MaxOutputBuffer
		if t.buf.Len() > excess {
			t.buf.Next(excess)
		} else {
			t.buf.Reset()
		}
	}
	t.buf.Write(data)
	// 同步追加到实时画面推流缓冲（最大 64KB，超出丢弃最旧部分）
	const maxStream = 64 * 1024
	if t.stream.Len()+len(data) > maxStream {
		excess := t.stream.Len() + len(data) - maxStream
		if t.stream.Len() > excess {
			t.stream.Next(excess)
		} else {
			t.stream.Reset()
		}
	}
	t.stream.Write(data)
}

func (t *TerminalSession) IsExpired() bool {
	return time.Since(t.createdAt) >= t.timeout
}

type Plugin struct {
	stopChOnce     sync.Once
	name           string
	mu             sync.Mutex
	wg             sync.WaitGroup
	stopCh         chan struct{}
	stopOnce       sync.Once
	sessions       map[string]*TerminalSession
	nextID         int
	defaultTimeout time.Duration
	notifyBytes    int
	notifyInterval time.Duration
}

func New(name string) *Plugin {
	return &Plugin{
		name:     name,
		stopCh:   make(chan struct{}),
		sessions: make(map[string]*TerminalSession),
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "default_timeout", Type: "string", DisplayName: "默认终端超时",
		Description: "终端自动关闭的默认时间，例如 5m, 10m, 30m, 1h（默认 5m）",
		Default:     "5m",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "notify_bytes", Type: "int", DisplayName: "通知阈值字节数",
		Description: "累积多少字节未读输出后发送通知（默认 2048）",
		Default:     "2048",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "notify_interval", Type: "string", DisplayName: "通知最小间隔",
		Description: "同一终端两次通知的最小时间间隔，如 2s, 5s（默认 2s）",
		Default:     "2s",
	})
	if v, _ := s.Settings().Get("default_timeout"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if d, err := time.ParseDuration(s); err == nil {
				p.defaultTimeout = d
			}
		}
	}
	if p.defaultTimeout <= 0 {
		p.defaultTimeout = DefaultTimeout
	}
	if v, _ := s.Settings().Get("notify_bytes"); v != nil {
		if i, ok := v.(float64); ok && i > 0 {
			p.notifyBytes = int(i)
		}
	}
	if p.notifyBytes <= 0 {
		p.notifyBytes = DefaultNotifyBytes
	}
	if v, _ := s.Settings().Get("notify_interval"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if d, err := time.ParseDuration(s); err == nil {
				p.notifyInterval = d
			}
		}
	}
	if p.notifyInterval <= 0 {
		p.notifyInterval = DefaultNotifyInterval
	}

	// agentcli 通道：终端生命周期/输出事件经它注入 agent（见本文件 InjectText* 调用）。
	_ = s.RegisterInputChannel("agentcli", sdk.ChannelDef{})
	s.RegisterTool("terminal_create", sdk.ToolDef{
		Name: "terminal_create",
		Description: "创建一个新的交互式终端会话。返回终端 ID，后续通过此 ID 进行读写操作。适用于运行交互式程序如 vim、ssh、top、nano 等。" +
			"通知模式通过 notify 参数选择（默认 exit）：exit=仅命令执行结束后提醒一次；interval=定时反馈（如 interval=30s 每 30 秒反馈一次状态摘要）；" +
			"buffer=未读输出积累到指定字节数后提醒（如 buffer=8192）；多个模式用逗号组合（如 interval=30s,buffer=8192）。终端默认 5 分钟后自动关闭，可通过 timeout 参数调整。",
		NoMemory: true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "要执行的命令（默认 bash）。如需运行特定程序直接传入即可，例如：vim /tmp/test.txt",
				},
				"notify": map[string]interface{}{
					"type":        "string",
					"description": "通知模式（可选）：exit（默认，命令结束后提醒）；interval=时长（定时反馈，如 30s/1m）；buffer=字节数（缓冲阈值提醒）；可逗号组合",
				},
				"timeout": map[string]interface{}{
					"type":        "string",
					"description": "终端自动关闭时间，例如 5m, 10m, 30m, 1h（默认 5m）",
				},
				"rows": map[string]interface{}{
					"type":        "integer",
					"description": "终端行数（默认 24）",
				},
				"cols": map[string]interface{}{
					"type":        "integer",
					"description": "终端列数（默认 80）",
				},
			},
		},
		// 建会话：写共享会话表
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleCreate(s, args)
	})

	s.RegisterTool("terminal_write", sdk.ToolDef{
		Name:        "terminal_write",
		Description: "向指定终端发送输入。支持普通文本和特殊键（通过 key 参数传入）。特殊键包括：enter, tab, escape, ctrl_a~ctrl_z, alt_a~alt_z, f1~f12, up, down, left, right, home, end, backspace, delete, page_up, page_down。普通文本传入 input 参数即可。",
		NoMemory:    true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "终端 ID，来自 terminal_create 的返回值",
				},
				"input": map[string]interface{}{
					"type":        "string",
					"description": "要发送的文本内容（普通文字直接输入）",
				},
				"key": map[string]interface{}{
					"type":        "string",
					"description": "特殊按键：enter, tab, escape, ctrl_a~ctrl_z, alt_a~alt_z, f1~f12, up, down, left, right, home, end, backspace, delete, page_up, page_down",
				},
			},
			"required": []string{"id"},
		},
		// 写终端：与会话缓冲区共享，须按序
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleWrite(s, args)
	})

	s.RegisterTool("terminal_read", sdk.ToolDef{
		Name:        "terminal_read",
		Description: "读取指定终端的输出。mode=new（默认）返回自上次读取以来的新输出并清空缓冲；mode=now 返回终端当前显示的全部屏幕内容（不清空缓冲）。如需持续监控请多次调用。",
		NoMemory:    true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "终端 ID",
				},
				"mode": map[string]interface{}{
					"type":        "string",
					"description": "读取模式：new（默认，新输出并清空缓冲）或 now（当前屏幕全部内容，不清理）",
				},
				"clear": map[string]interface{}{
					"type":        "boolean",
					"description": "读取后是否清除缓冲区（默认与 mode 一致：new 清除，now 不清除）",
				},
			},
			"required": []string{"id"},
		},
		// 读终端：与会话缓冲区共享，须按序
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleRead(args)
	})

	s.RegisterTool("terminal_resize", sdk.ToolDef{
		Name:        "terminal_resize",
		Description: "调整指定终端的尺寸（行数和列数）。",
		NoMemory:    true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "终端 ID",
				},
				"rows": map[string]interface{}{
					"type":        "integer",
					"description": "行数",
				},
				"cols": map[string]interface{}{
					"type":        "integer",
					"description": "列数",
				},
			},
			"required": []string{"id"},
		},
		// 改窗口尺寸：改会话状态
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleResize(args)
	})

	s.RegisterTool("terminal_close", sdk.ToolDef{
		Name:        "terminal_close",
		Description: "关闭指定终端会话。释放资源。",
		NoMemory:    true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "终端 ID",
				},
			},
			"required": []string{"id"},
		},
		// 关会话：改共享会话表
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleClose(s, args)
	})

	s.RegisterTool("terminal_list", sdk.ToolDef{
		Name:        "terminal_list",
		Description: "列出所有活跃的终端会话及其状态。",
		NoMemory:    true,
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 已核实只读：只列举会话
		ParallelSafe: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleList()
	})

	s.RegisterTool("terminal_watch", sdk.ToolDef{
		Name: "terminal_watch",
		Description: "为指定终端设置提醒规则，避免长时间运行任务（编译/下载/构建等）的输出造成通知风暴。" +
			"可选规则：interval=固定时间反馈（每隔该时长向 agent 反馈一次终端状态摘要）；" +
			"on_exit=命令执行结束提醒；buffer_bytes=未读输出积累到该字节数时提醒一次；" +
			"quiet=静默模式（抑制随输出流的通知，仅保留定时反馈与结束提醒，推荐长任务使用）。" +
			"未提供的字段保持原值，clear=true 清除全部规则。默认 on_exit=true。",
		NoMemory: true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "终端 ID，来自 terminal_create 的返回值",
				},
				"interval": map[string]interface{}{
					"type":        "string",
					"description": "固定时间反馈间隔，如 30s, 1m, 5m（可选，0 禁用）",
				},
				"on_exit": map[string]interface{}{
					"type":        "boolean",
					"description": "命令执行结束时是否提醒（默认 true）",
				},
				"buffer_bytes": map[string]interface{}{
					"type":        "integer",
					"description": "未读输出积累阈值（字节），达到后提醒一次（可选，默认全局 2048）",
				},
				"quiet": map[string]interface{}{
					"type":        "boolean",
					"description": "静默模式：不随输出流通知，仅保留定时反馈与结束提醒（推荐编译/下载等长任务）",
				},
				"clear": map[string]interface{}{
					"type":        "boolean",
					"description": "清除该终端全部提醒规则（恢复默认行为）",
				},
			},
			"required": []string{"id"},
		},
		// 订阅输出：注册监听者，改共享状态
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleWatch(args)
	})

	p.wg.Add(1)
	go p.cleanupLoop(s)

	return nil
}

func (p *Plugin) Stop() error {
	// 幂等：Stop 可能被多条路径调到（StopAndUnload 后再 StopAll、
	// 停机与热重载交错）。close 一个已关闭的 channel 会 panic，
	// 故用 Once 兜住。
	// ★ 不能写成 p.stopOnce.Do(func(){ safeclose.Safe(&p.stopOnce, ...) })：
	// sync.Once 不可重入 ⇒ 外层 Do 持锁、内层 Do 等同一把锁 ⇒ **自锁死锁**
	// （实测：agentcli 测试 600s 超时，栈显示 sync.(*Once).Do 嵌套）。
	// 正确做法：close 的守卫用**独立的** once。
	p.stopOnce.Do(func() {
		safeclose.Safe(&p.stopChOnce, p.stopCh)
		p.shutdown()
	})
	return nil
}

func (p *Plugin) shutdown() {
	// 必须**先**关掉活跃终端，再等 wg。
	//
	// 为什么顺序不能反：readLoop 自己也记在 p.wg 上，而它的退出条件
	// 是「t.stopCh 收到信号」或「进程自行结束／超时」——它不监 p.stopCh。
	// 旧实现在这里先 p.wg.Wait() 才 Close 终端：只要还有任何一个终端开着，
	// readLoop 永远等不到 t.stopCh，wg.Wait() 就永返回不了。
	// 后果是插件卸载 / 热重载（StopAndUnload / ReloadOne）与停机全挂在
	// 这一步，且持有 registry 锁时就是全内核一起挂。
	p.mu.Lock()
	sessions := make([]*TerminalSession, 0, len(p.sessions))
	for _, t := range p.sessions {
		sessions = append(sessions, t)
	}
	p.sessions = nil
	p.mu.Unlock()

	for _, t := range sessions {
		t.Close()
	}

	p.wg.Wait()
}

func (p *Plugin) nextIDLocked() string {
	p.nextID++
	return fmt.Sprintf("term_%d", p.nextID)
}

func (p *Plugin) handleCreate(s *sdk.PluginSDK, args map[string]interface{}) (interface{}, error) {
	command, _ := args["command"].(string)
	if command == "" {
		command = defaultShell()
	}

	timeoutStr, _ := args["timeout"].(string)
	timeout := p.defaultTimeout
	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = d
		}
	}
	// SSH 命令自动使用更长的超时（5 分钟）
	if timeoutStr == "" && (strings.HasPrefix(command, "ssh ") || strings.HasPrefix(command, "ssh -")) {
		timeout = 5 * time.Minute
	}

	rows := uint16(24)
	cols := uint16(80)
	if r, ok := args["rows"].(float64); ok && r > 0 {
		rows = uint16(r)
	}
	if c, ok := args["cols"].(float64); ok && c > 0 {
		cols = uint16(c)
	}

	// 通知模式：默认 exit（命令执行结束后提醒一次）。
	// 支持 interval=30s / buffer=8192 / quiet，可逗号组合。
	watch := terminalWatch{onExit: true, quiet: true}
	if notifyStr, ok := args["notify"].(string); ok && notifyStr != "" {
		watch = parseNotifyMode(notifyStr, watch)
	}

	term, cmd, err := newCommandPty(command, rows, cols)
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("创建终端失败: %v", err)}, nil
	}

	session := &TerminalSession{
		id:        "",
		command:   command,
		cmd:       cmd,
		session:   term,
		createdAt: time.Now(),
		timeout:   timeout,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
		watch:     watch,
	}

	p.mu.Lock()
	id := p.nextIDLocked()
	session.id = id
	p.sessions[id] = session
	p.mu.Unlock()

	p.wg.Add(1)
	go p.readLoop(session, s)

	// 生命周期事件：终端创建即时上报（带 command），让内核权威视图与所有
	// 订阅者（WebUI/CLI）即使在该终端无输出的情况下也能知道它的存在。
	emitTermState(s, session, true)

	log.Printf("[agentcli] created terminal %s: command=%q timeout=%v rows=%d cols=%d", id, command, timeout, rows, cols)

	return map[string]interface{}{
		"id":          id,
		"status":      "created",
		"command":     command,
		"timeout":     timeout.String(),
		"rows":        rows,
		"cols":        cols,
		"notify_mode": notifyModeString(watch),
	}, nil
}

// notifyModeString 输出可读的通知模式描述。
func notifyModeString(w terminalWatch) string {
	var parts []string
	if w.onExit {
		parts = append(parts, "exit")
	}
	if w.interval > 0 {
		parts = append(parts, "interval="+w.interval.String())
	}
	if w.bufferBytes > 0 {
		parts = append(parts, fmt.Sprintf("buffer=%d", w.bufferBytes))
	}
	if len(parts) == 0 {
		return "quiet"
	}
	return strings.Join(parts, ",")
}

func (p *Plugin) handleWrite(s *sdk.PluginSDK, args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return map[string]interface{}{"error": "id is required"}, nil
	}

	p.mu.Lock()
	session, ok := p.sessions[id]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("终端 %s 不存在或已关闭", id)}, nil
	}

	input, _ := args["input"].(string)
	key, _ := args["key"].(string)

	var data []byte

	if key != "" {
		b, err := mapKey(key)
		if err != nil {
			return map[string]interface{}{"error": err.Error()}, nil
		}
		data = b
	} else if input != "" {
		data = []byte(input)
	} else {
		return map[string]interface{}{"error": "需要提供 input 或 key 参数"}, nil
	}

	n, err := session.Write(string(data))
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("写入失败: %v", err)}, nil
	}

	return map[string]interface{}{
		"status":   "ok",
		"bytes":    n,
		"terminal": id,
	}, nil
}

// parseNotifyMode 解析 notify 参数并合并进 watch。
// 支持：exit / quiet / interval=时长 / buffer=字节数，逗号分隔组合。
func parseNotifyMode(s string, base terminalWatch) terminalWatch {
	w := base
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		key := strings.TrimSpace(kv[0])
		val := ""
		if len(kv) == 2 {
			val = strings.TrimSpace(kv[1])
		}
		switch key {
		case "exit":
			w.onExit = true
			w.quiet = false
		case "quiet", "silent":
			w.quiet = true
		case "interval":
			if d, err := time.ParseDuration(val); err == nil && d > 0 {
				w.interval = d
			}
		case "buffer":
			var n int
			if _, err := fmt.Sscanf(val, "%d", &n); err == nil && n > 0 {
				w.bufferBytes = n
			}
		}
	}
	return w
}

func (p *Plugin) handleRead(args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return map[string]interface{}{"error": "id is required"}, nil
	}

	mode, _ := args["mode"].(string)
	clear := mode != "now"
	if v, ok := args["clear"].(bool); ok {
		clear = v
	}

	p.mu.Lock()
	session, ok := p.sessions[id]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("终端 %s 不存在或已关闭", id)}, nil
	}

	var output string
	session.mu.Lock()
	if clear {
		output = session.buf.String()
		session.buf.Reset()
		// 实时画面推流缓冲同步清空，避免 terminal_output 事件与读取结果重复
		session.stream.Reset()
	} else {
		output = session.buf.String()
	}
	session.mu.Unlock()

	if output == "" {
		if mode == "now" {
			output = "[终端当前无屏幕内容]"
		} else {
			output = "[终端无新输出]"
		}
	}

	return map[string]interface{}{
		"status":   "ok",
		"terminal": id,
		"mode":     mode,
		"output":   output,
		"running":  terminalRunning(session),
		"uptime":   time.Since(session.createdAt).String(),
	}, nil
}

func (p *Plugin) handleResize(args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return map[string]interface{}{"error": "id is required"}, nil
	}

	rows, okRows := args["rows"].(float64)
	cols, okCols := args["cols"].(float64)
	if !okRows || !okCols {
		return map[string]interface{}{"error": "rows 和 cols 为必填"}, nil
	}

	p.mu.Lock()
	session, ok := p.sessions[id]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("终端 %s 不存在或已关闭", id)}, nil
	}

	if err := session.Resize(uint16(rows), uint16(cols)); err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("调整尺寸失败: %v", err)}, nil
	}

	return map[string]interface{}{
		"status":   "ok",
		"terminal": id,
		"rows":     rows,
		"cols":     cols,
	}, nil
}

func (p *Plugin) handleClose(s *sdk.PluginSDK, args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return map[string]interface{}{"error": "id is required"}, nil
	}

	p.mu.Lock()
	session, ok := p.sessions[id]
	if ok {
		delete(p.sessions, id)
	}
	p.mu.Unlock()

	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("终端 %s 不存在或已关闭", id)}, nil
	}

	session.Close()
	log.Printf("[agentcli] closed terminal %s", id)

	// 生命周期事件：显式上报关闭（readLoop 退出时也会发，幂等）。
	emitTermState(s, session, false)

	return map[string]interface{}{
		"status":   "closed",
		"terminal": id,
	}, nil
}

func (p *Plugin) handleWatch(args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return map[string]interface{}{"error": "id is required"}, nil
	}

	p.mu.Lock()
	session, ok := p.sessions[id]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("终端 %s 不存在或已关闭", id)}, nil
	}

	session.mu.Lock()
	if v, ok := args["clear"].(bool); ok && v {
		session.watch = terminalWatch{onExit: true}
	} else {
		if v, ok := args["interval"].(string); ok && v != "" {
			if d, err := time.ParseDuration(v); err == nil && d >= 0 {
				session.watch.interval = d
			}
		}
		if v, ok := args["on_exit"].(bool); ok {
			session.watch.onExit = v
		}
		if v, ok := args["buffer_bytes"].(float64); ok && v >= 0 {
			session.watch.bufferBytes = int(v)
		}
		if v, ok := args["quiet"].(bool); ok {
			session.watch.quiet = v
		}
		if session.watch.interval == 0 && session.watch.bufferBytes == 0 && !session.watch.quiet {
			session.watch.onExit = true
		}
	}
	w := session.watch
	session.mu.Unlock()

	log.Printf("[agentcli] watch updated for %s: %+v", id, w)
	return map[string]interface{}{
		"status":       "ok",
		"terminal":     id,
		"interval":     w.interval.String(),
		"on_exit":      w.onExit,
		"buffer_bytes": w.bufferBytes,
		"quiet":        w.quiet,
	}, nil
}

func (p *Plugin) handleList() (interface{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	type termInfo struct {
		ID        string `json:"id"`
		Command   string `json:"command"`
		Uptime    string `json:"uptime"`
		ExpiresIn string `json:"expires_in"`
		Running   bool   `json:"running"`
	}

	var terms []termInfo
	for _, t := range p.sessions {
		running := terminalRunning(t)
		remaining := t.timeout - time.Since(t.createdAt)
		if remaining < 0 {
			remaining = 0
		}
		terms = append(terms, termInfo{
			ID:        t.id,
			Command:   t.command,
			Uptime:    time.Since(t.createdAt).Round(time.Second).String(),
			ExpiresIn: remaining.Round(time.Second).String(),
			Running:   running,
		})
	}

	if terms == nil {
		terms = []termInfo{}
	}

	return map[string]interface{}{
		"status":    "ok",
		"count":     len(terms),
		"terminals": terms,
	}, nil
}

// flushTermStream 把终端待推送的增量输出作为 terminal_output 事件发布。
//
// 必须抽成公共函数：退出路径（进程结束/超时/读取错误/stopCh）与常规 200ms
// ticker 都要走同一条推送路径。否则短命令（echo/ls 这类在首个 ticker 之前
// 就结束的）残留在 stream 里的输出永远发不出事件，只留在 buf 里——
// agent 用 terminal_read 能看到，但内核权威视图的 output 恒为空。
// 返回是否真的发布了（无残留时为 false）。
func flushTermStream(s *sdk.PluginSDK, t *TerminalSession) bool {
	if s == nil || t == nil {
		return false
	}
	var streamData string
	t.mu.Lock()
	if t.stream.Len() > 0 {
		streamData = t.stream.String()
		t.stream.Reset()
	}
	t.mu.Unlock()
	if streamData == "" {
		return false
	}
	s.Publish(&sdk.Event{
		Type:      sdk.EventTerminalOutput,
		Source:    "agentcli",
		Payload:   map[string]interface{}{"terminal_id": t.id, "command": t.command, "output": streamData, "running": terminalRunning(t)},
		Timestamp: time.Now().UnixMilli(),
	})
	return true
}

// emitTermState 把终端存活状态作为 EventTerminalOutput 事件上报。
//
// 生命周期事件（创建/关闭/退出/超时）必须显式发：终端无输出时 ticker
// 不会发事件，而所有插件（WebUI/CLI）的终端视图都依赖这些事件
// 才能知道终端的存在与终止。payload 带 command 供无 ToolCall 事件的
// 直调路径（CLI /terminal create 经 ToolAPI.ExecuteTool）回填命令名。
// 幂等：重复发同一 running 值不会产生状态跳变。
func emitTermState(s *sdk.PluginSDK, t *TerminalSession, running bool) {
	if s == nil || t == nil {
		return
	}
	s.Publish(&sdk.Event{
		Type:      sdk.EventTerminalOutput,
		Source:    "agentcli",
		Payload:   map[string]interface{}{"terminal_id": t.id, "command": t.command, "running": running},
		Timestamp: time.Now().UnixMilli(),
	})
}

// releaseResources 幂等地释放终端后端：杀进程 + 关 PTY。
//
// 为何需要单独一个方法：readLoop 是终端自然的退出点（超时/进程结束/
// 读取错误/插件停机），但 close(t.done) 的时机意味着它**不能**调
// TerminalSession.Close()——后者会 <-t.done 等 readLoop 退出，而 readLoop
// 正在自己里面，直接死锁。所以这里只做「不再需要 readLoop 配合」的那半：
// 终止进程与释放 fd。
func (t *TerminalSession) releaseResources() {
	t.mu.Lock()
	if t.resourcesReleased {
		t.mu.Unlock()
		return
	}
	t.resourcesReleased = true
	t.mu.Unlock()

	if t.session != nil {
		_ = t.session.Kill()
		// 回收子进程（避免僵尸）。后端可选实现：Linux PTY 在 Kill 后
		// 必须 Wait 才能把 <defunct> 清掉；不实现的后端（如 Windows
		// ConPTY）跳过即可。
		if reaper, ok := t.session.(interface{ Wait() error }); ok {
			_ = reaper.Wait()
		}
		_ = t.session.Close()
	}
}

func (p *Plugin) readLoop(t *TerminalSession, s *sdk.PluginSDK) {
	defer p.wg.Done()
	defer safeclose.Safe(&t.doneOnce, t.done)

	// 无论从哪个分支退出，都释放进程与 PTY。
	//
	// 旧实现只在「超时」和「进程退出」两个分支 delete(sessions) 后直接
	// return：超时分支完全不碰 session，一个 sleep 999 超时后进程、PTY fd
	// 与 reader 協程全数泄漏（readLoop 已经从 sessions 里删掉了，cleanupLoop
	// 也再看不到它，没人能回收）。defer 保证「只要退出就释放」。
	defer t.releaseResources()

	// reader 协程独享这个读缓冲：结果随 readResult 携带，
	// readLoop 不再从其中做 copy（见 reader 注释，那是对共享缓冲
	// 的并发读写，-race 实测触发）。
	buf := make([]byte, ReadBufSize)
	pollInterval := 200 * time.Millisecond

	readCh := make(chan readResult, 4)
	go p.reader(t, buf, readCh)

	// 实时画面推流 ticker：每 200ms 批量发布一次 terminal_output 事件
	flushTicker := time.NewTicker(200 * time.Millisecond)
	defer flushTicker.Stop()

	// 退出时先把残留输出推出去，再报停止。
	//
	// defer 是 LIFO：下面这两行声明顺序决定执行顺序——先 flush 后 emit。
	// 若反了，停止事件会先于最后一段输出到达，内核会先把 running 置 false
	// 再追加输出（状态看着对但顺序错）；更重要的是短命令的输出
	// 只存在于 stream 里，不 flush 就彻底丢了。
	defer emitTermState(s, t, false)
	defer flushTermStream(s, t)

	// 立即发送首次"终端已启动"通知，让 agent 感知存在。
	// 用 NoMemory：这是状态提示，不是对话内容。不关掉的话每开一个终端都会
	// 在记忆里留下一条"[终端 X 已启动]"，把真实内容挤掉。
	s.InjectTextOpts("agentcli", "agentcli", fmt.Sprintf("[终端 %s 已启动]", t.id),
		sdk.InjectOptions{NoMemory: true})
	now := time.Now()
	t.mu.Lock()
	t.lastNotify = now
	t.lastData = now
	t.lastFeedback = now
	t.mu.Unlock()

	// 硬上限：未读输出积累达到该值也通知一次（防大输出静默丢失），频率极低
	hardNotifyBytes := 64 * 1024
	hardNotifyInterval := 10 * time.Second
	// 输出停止判定：超过该时长无新数据则视为输出停止
	quietLatency := 2 * time.Second

	for {
		// p.stopCh：插件停机。readLoop 记在 p.wg 上，若不在此退出，
		// Stop() 的 wg.Wait() 就只能等终端自己超时（最长 30 分钟）。
		select {
		case <-p.stopCh:
			return
		default:
		}

		if t.IsExpired() {
			log.Printf("[agentcli] terminal %s expired after %v", t.id, t.timeout)
			s.InjectTextOpts("agentcli", "agentcli", fmt.Sprintf("[终端 %s 已超时关闭（%s）]", t.id, t.timeout),
				sdk.InjectOptions{NoMemory: true})
			p.mu.Lock()
			delete(p.sessions, t.id)
			p.mu.Unlock()
			return
		}

		if !terminalRunning(t) {
			if t.watch.onExit {
				s.InjectTextOpts("agentcli", "agentcli", fmt.Sprintf("[终端 %s 中的命令已执行结束]", t.id),
					sdk.InjectOptions{NoMemory: true})
			} else {
				s.InjectTextOpts("agentcli", "agentcli", fmt.Sprintf("[终端 %s 中的进程已退出]", t.id),
					sdk.InjectOptions{NoMemory: true})
			}
			p.mu.Lock()
			delete(p.sessions, t.id)
			p.mu.Unlock()
			return
		}

		// 固定时间反馈：watch.interval > 0 时每隔该时长主动反馈一次状态摘要
		t.mu.Lock()
		if t.watch.interval > 0 && time.Since(t.lastFeedback) >= t.watch.interval {
			t.lastFeedback = time.Now()
			t.lastNotify = t.lastFeedback
			unread := t.unreadBytes
			t.unreadBytes = 0
			preview := previewTail(t.buf.String(), 120)
			t.mu.Unlock()
			s.InjectText("agentcli", "agentcli",
				// 刻意**不**用 NoMemory：这条带上终端真实输出（preview），
				// 属于该记的内容。只有纯状态通知才关记忆。
				fmt.Sprintf("[终端 %s 定时反馈: 运行中, 期间新输出约 %d 字节]\n%s", t.id, unread, preview))
			continue
		}
		t.mu.Unlock()

		select {
		case <-t.stopCh:
			return
		case <-flushTicker.C:
			// 批量推送终端实时画面增量（独立 ticker，避免被高密度数据饿死）
			flushTermStream(s, t)
		case r := <-readCh:
			if r.err != nil {
				// 读取错误/EOF → 立即通知（进程可能已结束）
				s.InjectTextOpts("agentcli", "agentcli", fmt.Sprintf("[终端 %s 读取结束: %v]", t.id, r.err),
					sdk.InjectOptions{NoMemory: true})
				return
			}
			if r.n > 0 {
				t.appendOutput(r.data)

				// 缓冲阈值通知（仅当 agent 显式选择 buffer 模式，或未读积累达到硬上限）。
				// 默认模式（仅 exit 提醒）下不随输出流通知，杜绝通知风暴。
				t.mu.Lock()
				t.lastData = time.Now()
				t.unreadBytes += r.n
				bufThr := t.watch.bufferBytes
				if bufThr <= 0 {
					bufThr = p.notifyBytes
				}
				minInterval := p.notifyInterval
				if t.watch.interval > 0 {
					minInterval = t.watch.interval
				}
				// 风暴退避：距上次通知不足 1s 说明输出极速，通知间隔翻倍（上限 30s）
				if time.Since(t.lastNotify) < time.Second && t.unreadBytes >= bufThr {
					if t.backoff == 0 {
						t.backoff = minInterval
					} else if t.backoff < 30*time.Second {
						t.backoff *= 2
						if t.backoff > 30*time.Second {
							t.backoff = 30 * time.Second
						}
					}
				}
				interval := t.backoff + minInterval
				isHard := t.watch.bufferBytes <= 0 && t.unreadBytes >= hardNotifyBytes
				if isHard && hardNotifyInterval > interval {
					interval = hardNotifyInterval
				}
				need := t.unreadBytes >= bufThr && time.Since(t.lastNotify) >= interval
				if need {
					t.lastNotify = time.Now()
					t.unreadBytes = 0
					preview := previewTail(t.buf.String(), 200)
					t.mu.Unlock()
					s.InjectText("agentcli", "agentcli",
						// 同样刻意保留记忆：preview 是终端新输出，是真实内容。
						fmt.Sprintf("[终端 %s 有新输出]\n%s", t.id, preview))
				} else {
					t.mu.Unlock()
				}
			}
		case <-time.After(pollInterval):
			// 空闲轮询：输出已停止时复位退避
			t.mu.Lock()
			if t.backoff > 0 && time.Since(t.lastData) >= quietLatency {
				t.backoff = 0
			}
			t.mu.Unlock()
		}
	}
}

// previewTail 返回 s 末尾最多 n 字符，并转义控制字符保证可读。
func previewTail(s string, n int) string {
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return sanitizePreview(s)
}

type readResult struct {
	n    int
	data []byte
	err  error
}

// reader 从终端读取输出并通过 channel 交给 readLoop。
//
// 读到的数据**随结果一起传**而不是复用外层共享的 buf：
// reader 是唯一写 buf 的 goroutine，readLoop 又常在 reader 尚未
// 写完下一段时就从 buf[:r.n] 做 copy——同一个 shared buf 被并发
// 读写就是 data race（-race 实测触发）。改为每个结果自带切片后，
// 读与拷贝天然隔离，不再共享可变状态。
func (p *Plugin) reader(t *TerminalSession, buf []byte, ch chan<- readResult) {
	for {
		n, err := t.session.Read(buf)
		var data []byte
		if n > 0 {
			data = make([]byte, n)
			copy(data, buf[:n])
		}
		select {
		case ch <- readResult{n, data, err}:
		case <-t.stopCh:
			return
		}
		if err != nil {
			return
		}
	}
}

func (p *Plugin) cleanupLoop(s *sdk.PluginSDK) {
	defer p.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			// 先持锁筛选出待关闭的终终并移出 map，再在锁外逐个关闭。
			// 旧实现在持锁期间 go func 调 term.Close()（内部会 Kill 进程并等
			// <-t.done），临界区被拉长且与 TerminalSession 的退出路径交错。
			var expired []*TerminalSession
			p.mu.Lock()
			for id, t := range p.sessions {
				switch {
				case t.IsExpired():
					log.Printf("[agentcli] cleanup: terminal %s expired", id)
				case !terminalRunning(t):
					log.Printf("[agentcli] cleanup: terminal %s process exited", id)
				default:
					continue
				}
				delete(p.sessions, id)
				expired = append(expired, t)
			}
			p.mu.Unlock()

			for _, t := range expired {
				go func(term *TerminalSession) { term.Close() }(t)
			}
		}
	}
}

func mapKey(key string) ([]byte, error) {
	key = strings.ToLower(key)
	switch key {
	case "enter":
		return []byte{0x0D}, nil
	case "tab":
		return []byte{0x09}, nil
	case "escape", "esc":
		return []byte{0x1B}, nil
	case "backspace":
		return []byte{0x7F}, nil
	case "delete":
		return []byte{0x1B, 0x5B, 0x33, 0x7E}, nil
	case "home":
		return []byte{0x1B, 0x5B, 0x48}, nil
	case "end":
		return []byte{0x1B, 0x5B, 0x46}, nil
	case "page_up":
		return []byte{0x1B, 0x5B, 0x35, 0x7E}, nil
	case "page_down":
		return []byte{0x1B, 0x5B, 0x36, 0x7E}, nil
	case "up":
		return []byte{0x1B, 0x5B, 0x41}, nil
	case "down":
		return []byte{0x1B, 0x5B, 0x42}, nil
	case "right":
		return []byte{0x1B, 0x5B, 0x43}, nil
	case "left":
		return []byte{0x1B, 0x5B, 0x44}, nil
	}

	if strings.HasPrefix(key, "ctrl_") && len(key) == 6 {
		c := key[5]
		if c >= 'a' && c <= 'z' {
			return []byte{byte(c - 'a' + 1)}, nil
		}
	}

	if strings.HasPrefix(key, "alt_") && len(key) == 5 {
		c := key[4]
		if c >= 'a' && c <= 'z' {
			return []byte{0x1B, c}, nil
		}
	}

	if strings.HasPrefix(key, "f") && len(key) <= 4 {
		var n int
		if _, err := fmt.Sscanf(key, "f%d", &n); err == nil && n >= 1 && n <= 12 {
			return functionKeyCode(n), nil
		}
	}

	return nil, fmt.Errorf("不支持的特殊按键: %s", key)
}

func functionKeyCode(n int) []byte {
	switch n {
	case 1:
		return []byte{0x1B, 0x5B, 0x50}
	case 2:
		return []byte{0x1B, 0x5B, 0x51}
	case 3:
		return []byte{0x1B, 0x5B, 0x52}
	case 4:
		return []byte{0x1B, 0x5B, 0x53}
	case 5:
		return []byte{0x1B, 0x5B, 0x31, 0x35, 0x7E}
	case 6:
		return []byte{0x1B, 0x5B, 0x31, 0x37, 0x7E}
	case 7:
		return []byte{0x1B, 0x5B, 0x31, 0x38, 0x7E}
	case 8:
		return []byte{0x1B, 0x5B, 0x31, 0x39, 0x7E}
	case 9:
		return []byte{0x1B, 0x5B, 0x32, 0x30, 0x7E}
	case 10:
		return []byte{0x1B, 0x5B, 0x32, 0x31, 0x7E}
	case 11:
		return []byte{0x1B, 0x5B, 0x32, 0x32, 0x7E}
	case 12:
		return []byte{0x1B, 0x5B, 0x32, 0x34, 0x7E}
	default:
		return []byte{}
	}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if opErr, ok := err.(*os.PathError); ok {
		err = opErr.Err
	}
	if syscallErr, ok := err.(syscall.Errno); ok {
		return syscallErr == syscall.EAGAIN || syscallErr == syscall.EWOULDBLOCK || syscallErr == syscall.ETIMEDOUT
	}
	return strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "would block")
}

func init() {
	plugin.RegisterFactory("agentcli", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
	plugin.RegisterPluginMeta("agentcli", "终端交互", "Agent CLI")
}

func sanitizePreview(s string) string {
	var buf bytes.Buffer
	for _, r := range s {
		if r == '\n' {
			buf.WriteString("\\n")
		} else if r == '\r' {
			buf.WriteString("\\r")
		} else if r == '\t' {
			buf.WriteString("\\t")
		} else if r >= 32 && r <= 126 {
			buf.WriteRune(r)
		} else if r == 0x1B {
			buf.WriteString("^[")
		} else if r < 32 {
			fmt.Fprintf(&buf, "^%c", r+'A'-1)
		} else {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

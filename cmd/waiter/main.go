package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/meta"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
)

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
)

const clearLine = "\033[2K\r"

var colors = true

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// isTTYFile 判断文件是否为字符终端（非终端时禁用 spinner 转圈）。
func isTTYFile(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// startSpinner 启动 npm 风格的加载动画，返回停止函数。
// stop() 幂等：终止动画并清除当前行。非终端环境直接空操作。
func startSpinner(label string) func() {
	if !colors || !isTTYFile(os.Stdout) {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	var doneOnce sync.Once
	go func() {
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Printf("%s%s %s%s\n", clearLine, colorDim, spinnerFrames[i%len(spinnerFrames)]+" "+label, colorReset)
				i++
			}
		}
	}()
	stop := func() {
		once.Do(func() {
			safeclose.Safe(&doneOnce, done)
			fmt.Print(clearLine)
		})
	}
	return stop
}

func init() {
	if os.Getenv("NO_COLOR") != "" {
		colors = false
	}
}

func printlnC(color, msg string) {
	if !colors {
		fmt.Println(msg)
		return
	}
	fmt.Printf("%s%s%s\n", color, msg, colorReset)
}

func historyPath() string {
	home, _ := os.UserHomeDir()
	xdgData := os.Getenv("XDG_DATA_HOME")
	if xdgData == "" {
		xdgData = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(xdgData, "homeagent")
	os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "cli_history")
}

func discoverSocket(configured string) string {
	if configured != "" {
		return configured
	}
	if s := os.Getenv("HOMEAGENT_SOCKET"); s != "" {
		return s
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".homeagent", "cli.sock"),
		"/var/lib/homeagent/cli.sock",
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append([]string{filepath.Join(xdg, "homeagent", "cli.sock")}, candidates...)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

func main() {
	socket := flag.String("socket", "", "unix socket path")
	remote := flag.String("remote", "", "remote webui URL")
	apiKey := flag.String("api-key", "", "API key for remote mode")
	configPath := flag.String("config", "", "config file path")
	chat := flag.String("chat", "", "send a message and print final text (one-shot)")
	say := flag.String("say", "", "deprecated alias of -chat")
	deviceGateway := flag.String("device", "", "remotedevice 网关地址（如 127.0.0.1:9890），启动设备桥")
	deviceToken := flag.String("device-token", "", "设备接入 token")
	deviceAuthorized := flag.Bool("device-authorized", false, "客户端本地授权（允许远程操控本机；也可在 waiter.yaml 配 device_authorized: true）")
	daemonMode := flag.Bool("daemon", false, "后台驻留模式：维持 homed 连接 + 设备桥，等待 TUI 实例接入")
	testCap := flag.String("test-cap", "", "测试本地能力（screensue/speakeruse/screensee/clipboardsee/clipboardsue/computeruse/camerasue），如 --test-cap screensue")
	testCapArgs := flag.String("test-cap-args", "", "测试能力的参数")
	showVersion := flag.Bool("version", false, "打印版本并退出")
	flag.Parse()

	// 版本号直接来自 internal/meta（与 homed 同一事实源，不可能各写一个）。
	if *showVersion {
		fmt.Printf("waiter %s (commit %s, built %s)\n", meta.Version, meta.Commit, meta.BuildTime)
		return
	}

	// 本地能力测试模式（无需连接服务器）
	if *testCap != "" {
		runCapTest(*testCap, *testCapArgs)
		return
	}

	cfg := discoverConfig(*configPath)
	cfg.MergeCLI(*socket, *remote, *apiKey)
	cfg.ApplyDefault()

	if cfg.Socket == "" && cfg.Remote == "" {
		cfg.Socket = discoverSocket("")
	}

	// Daemon 模式：后台驻留
	if *daemonMode {
		runDaemon(cfg)
		return
	}

	oneShotMsg := *chat
	if oneShotMsg == "" {
		oneShotMsg = *say
	}

	state := &State{}
	if err := state.Connect(cfg); err != nil {
		printlnC(colorRed, fmt.Sprintf("connect: %v", err))
		os.Exit(1)
	}
	defer state.Disconnect()

	// 设备桥：--device 或配置 device_gateway 时，waiter 作为被控设备接入 remotedevice
	dg := *deviceGateway
	if dg == "" {
		dg = cfg.DeviceGateway
	}
	dt := *deviceToken
	if dt == "" {
		dt = cfg.DeviceToken
	}
	// 网关地址优先向门户**发现**（服务端才知道子域标签与基域名），
	// 失败再回退到用户配置 —— 老版本 HomeAgent 没有发现端点。
	// 只在用户已配置门户地址时尝试：没配门户就没有可问的对象。
	if portal := cfg.Remote; portal != "" {
		if discovered, err := discoverGateway(portal, cfg.APIKey, 5*time.Second); err == nil {
			printlnC(colorGreen, "device gateway discovered: "+discovered)
			dg = discovered
		} else if dg == "" {
			printlnC(colorYellow, "device gateway discovery failed: "+err.Error())
		}
	}
	if dg != "" && dt != "" {
		if err := startDeviceBridge(dg, dt); err != nil {
			printlnC(colorYellow, fmt.Sprintf("device bridge: %v (continue without)", err))
		} else {
			// 命令白名单：waiter.yaml device_cmd_allowlist，留空用内置默认集。
			//
			// ★ 在 startDeviceBridge **之后**赋值：白名单只在收到命令时才用，
			//   放在这里能保证它一定在第一帧命令到达前就绪。
			if len(cfg.DeviceCmdAllowlist) > 0 {
				deviceCmdAllowed = buildCmdMatcher(cfg.DeviceCmdAllowlist)
				printlnC(colorGreen, fmt.Sprintf("device cmd allowlist: %d 条（来自 waiter.yaml）",
					len(cfg.DeviceCmdAllowlist)))
			} else {
				printlnC(colorGreen, fmt.Sprintf("device cmd allowlist: 默认 %d 条（waiter.yaml 未配置 device_cmd_allowlist）",
					len(defaultCmdAllowlist)))
			}

			// 客户端本地授权：命令行 --device-authorized 或 waiter.yaml device_authorized
			auth := *deviceAuthorized || cfg.DeviceAuthorized
			deviceBridge.SetAuthorized(auth)
			printlnC(colorGreen, "device bridge active: "+deviceBridgeID+" authorized="+fmt.Sprint(auth))
			defer stopDeviceBridge()
		}
	}

	if oneShotMsg != "" {
		// ★ -chat 是一次性问答，会立刻走到上面的 return 并触发
		// defer stopDeviceBridge()，桥的生命周期只有几百毫秒。
		//
		// 后果：设备来不及完成 hello→bind 登记就已断开，服务端列表里永远
		// 看不到它（实测：`device bridge active` 打印了、bind_ack 也收到了，
		// 但 /api/v1/device/online 始终为空）。
		// 这不是桥的错 —— 用裸客户端把 hello/bind 发完并保持连接，同一实例
		// 上设备立刻出现在列表里（已验证）。
		//
		// 等待 bind 确认（或短暂超时）再退出：既让登记完成，也不把一次性
		// 命令拖长。bind 失败要明说，而不是静默丢掉设备。
		waitDeviceBind(3 * time.Second)
		oneshot(state, oneShotMsg)
		return
	}

	runInteractive(state, cfg)
}

// runInteractive 交互入口：TTY 下走 Bubble Tea 全屏 TUI，非 TTY 回退行式 REPL。
func runInteractive(state *State, cfg *Config) {
	history := newHistory(historyPath(), 1000)
	history.load()

	if isTTYFile(os.Stdin) && isTTYFile(os.Stdout) && colors {
		if err := runTUI(state, cfg, history); err != nil {
			printlnC(colorRed, fmt.Sprintf("tui: %v", err))
			printlnC(colorYellow, "falling back to line mode")
			runLineMode(state, cfg, history)
		}
		return
	}
	runLineMode(state, cfg, history)
}

func oneshot(state *State, msg string) {
	var sr streamRender
	stop := startSpinner("thinking...")
	resp, err := state.SendChatStream(msg, func(rl respLine) {
		// 第一个过程帧到达即停转，后续帧直接渲染
		stop()
		if sr.handleDelta(rl) {
			return // delta 已增量渲染
		}
		sr.reset() // 聚合帧/工具帧：结束 delta 流，换行输出
		printServerEvent(rl)
	})
	stop()
	if err != nil {
		printlnC(colorRed, fmt.Sprintf("error: %v", err))
		os.Exit(1)
	}
	printlnC(colorGreen, resp)
}

// runCapTest 本地能力测试（无需连接服务器）
func runCapTest(capName, args string) {
	fmt.Printf("=== 测试能力: %s ===\n", capName)
	fmt.Printf("参数: %s\n", args)
	fmt.Println("===========================")

	// 覆盖 sendBridgeResult 为本地打印
	sendBridgeResult = func(reqID, status, output, errMsg string) {
		fmt.Printf("结果状态: %s\n", status)
		if output != "" {
			fmt.Printf("输出: %s\n", output)
		}
		if errMsg != "" {
			fmt.Printf("错误: %s\n", errMsg)
		}
	}

	handleHomeagentCmd("test-001", "homeagent-"+capName+" "+args)
	fmt.Println("===========================")
	fmt.Println("测试完成")
}

// runLineMode 传统行式 REPL（非 TTY 回退 / TUI 启动失败时使用）。
func runLineMode(state *State, cfg *Config, history *History) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	line := newLineEditor(history)

	restore, err := setRawMode(0)
	if err != nil {
		restore = func() {}
	}
	defer restore()

	modeLabel := "local"
	addrLabel := cfg.Socket
	if cfg.Remote != "" {
		modeLabel = "remote"
		addrLabel = cfg.Remote
	}
	if colors {
		fmt.Printf("%sHomeAgent CLI%s %s%s (%s://%s)%s\n", colorBold, colorReset, colorDim, "v"+meta.Version, modeLabel, addrLabel, colorReset)
	} else {
		fmt.Printf("HomeAgent CLI v%s (%s://%s)\n", meta.Version, modeLabel, addrLabel)
	}
	fmt.Println("Type /help for commands.")

	var readerCancel func()
	var spinnerStopMu sync.Mutex
	var spinnerStop = func() {}
	startReader := func() {
		ctx, cancel := context.WithCancel(context.Background())
		readerCancel = cancel
		go state.ReadLoop(ctx, func(line string) {
			spinnerStopMu.Lock()
			stop := spinnerStop
			spinnerStopMu.Unlock()
			stop()
			printServerOutput(line)
		})
	}
	startReader()

	reconnect := func() {
		if readerCancel != nil {
			readerCancel()
		}
		state.Disconnect()
		for i := 0; i < 30; i++ {
			if err := state.Connect(cfg); err != nil {
				printlnC(colorYellow, fmt.Sprintf("reconnecting (%d/30): %v", i+1, err))
				time.Sleep(2 * time.Second)
				continue
			}
			printlnC(colorGreen, "reconnected")
			startReader()
			return
		}
		printlnC(colorRed, "giving up after 30 attempts")
	}

loop:
	for {
		text, err := line.read()
		if err != nil {
			break
		}
		line.clear()

		cmd := strings.TrimSpace(text)
		if cmd == "" {
			continue
		}

		if cmd[0] == '/' {
			if handleBuiltin(cmd, cfg, state, reconnect, os.Stdout) {
				if cmd == "/exit" || cmd == "/quit" {
					break loop
				}
				continue
			}
		}

		history.add(cmd)
		history.save()

		if err := state.Send(cmd); err != nil {
			printlnC(colorYellow, "connection lost, reconnecting...")
			line.redrawPending(cmd)
			reconnect()
			state.Send(cmd)
		}

		// 发送成功后启动加载动画，收到第一帧服务器输出时自动停止
		spinnerStopMu.Lock()
		spinnerStop = startSpinner("thinking...")
		spinnerStopMu.Unlock()

		select {
		case <-sigCh:
			break loop
		default:
		}
	}

	if readerCancel != nil {
		readerCancel()
	}
}

// streamRender 累积 token 级 delta 帧并增量重绘当前行。
// 聚合帧（reasoning/tool_call/response）到达时清空累积状态（该轮已结束）。
// 旧服务器不发 delta，此结构始终为空，行为与原来完全一致。
type streamRender struct {
	reasoning strings.Builder
	content   strings.Builder
}

// handleDelta 处理 delta 帧；返回是否消费了该帧。
// reset=true 的空帧表示服务端轮次作废（用户中断）：清空累积并定格已显示内容。
func (sr *streamRender) handleDelta(rl respLine) bool {
	switch rl.Type {
	case "reasoning_delta":
		if rl.Reset {
			sr.reasoning.Reset()
			fmt.Print(clearLine)
			return true
		}
		sr.reasoning.WriteString(rl.Content)
		if colors {
			fmt.Printf("%s%s· %s%s", clearLine, colorDim, sr.reasoning.String(), colorReset)
		} else {
			fmt.Printf("%s[思考] %s", clearLine, sr.reasoning.String())
		}
		return true
	case "content_delta":
		if rl.Reset {
			sr.content.Reset()
			fmt.Print(clearLine + "\n") // 定格已显示的部分内容，换行
			return true
		}
		sr.content.WriteString(rl.Content)
		if colors {
			fmt.Printf("%s%s%s%s", clearLine, colorGreen, sr.content.String(), colorReset)
		} else {
			fmt.Printf("%s%s", clearLine, sr.content.String())
		}
		return true
	}
	return false
}

// reset 在收到聚合帧/工具帧时调用：delta 流被打断或结束，
// 下一行输出不再覆盖 delta 内容。
func (sr *streamRender) reset() {
	sr.reasoning.Reset()
	sr.content.Reset()
	fmt.Print(clearLine + "\n")
}

// printServerOutput 渲染一行服务器输出（JSON 帧）。
func printServerOutput(content string) {
	rl := parseRespLineStruct(content)
	if !colors {
		fmt.Printf("%s%s\n", clearLine, renderPlain(rl, content))
		return
	}
	printServerEventColored(rl, content)
}

// printServerEvent 渲染一个已解析的过程/终结事件。
func printServerEvent(rl respLine) {
	if !colors {
		fmt.Printf("%s%s\n", clearLine, renderPlain(rl, ""))
		return
	}
	printServerEventColored(rl, "")
}

// renderPlain 无色模式下的纯文本渲染。
func renderPlain(rl respLine, raw string) string {
	switch rl.Type {
	case "reasoning", "reasoning_delta":
		return "[思考] " + rl.Content
	case "content_delta":
		return rl.Content
	case "tool_call":
		return fmt.Sprintf("[工具] %s (%s) %s", rl.Tool, rl.Status, rl.Result)
	case "response":
		return rl.Content
	case "error":
		return "[错误] " + rl.Error
	case "stage":
		// 过程信息：让无头调用方（脚本、基准 runner）也能看到进展，
		// 而不是只能等最终 response 帧。
		if rl.Tool != "" {
			return "[阶段] " + rl.Phase + " (" + rl.Tool + ")"
		}
		return "[阶段] " + rl.Phase
	case "system":
		// 内核级状态（目前只有 plugin_crashed）。
		//
		// 为何要接（2026-10-13 协议审计）：该事件一直在发，而 CLI 是唯一
		// 完全接不到的客户端 —— 插件炸了终端里毫无提示，只能去翻 journal。
		if rl.Event == "plugin_crashed" {
			return "[系统] 插件 " + rl.Plugin + " 异常退出，内核正在重启它"
		}
		return "[系统] " + rl.Event
	default:
		if raw != "" {
			return raw
		}
		return rl.Content
	}
}

// printServerEventColored 彩色模式下的帧渲染。
func printServerEventColored(rl respLine, raw string) {
	switch rl.Type {
	case "reasoning":
		fmt.Printf("%s%s· %s%s\n", clearLine, colorDim, rl.Content, colorReset)
	case "tool_call":
		mark, markColor := "⚙", colorYellow
		switch rl.Status {
		case "ok":
			mark, markColor = "✔", colorGreen
		case "denied", "interrupted", "error":
			mark, markColor = "✘", colorRed
		}
		preview := rl.Result
		if preview != "" {
			preview = " " + preview
		}
		fmt.Printf("%s%s%s %s [%s]%s%s\n", clearLine, markColor, mark, rl.Tool, rl.Status, preview, colorReset)
	case "response":
		fmt.Printf("%s%s%s%s\n", clearLine, colorGreen, rl.Content, colorReset)
	case "error":
		fmt.Printf("%s%s%s%s\n", clearLine, colorRed, rl.Error, colorReset)
	case "stage":
		// 阶段进展：用暗色（与“思考”同级）——它是过程信息，不应比最终回复更显眼。
		if rl.Tool != "" {
			fmt.Printf("%s%s▸ %s (%s)%s\n", clearLine, colorDim, rl.Phase, rl.Tool, colorReset)
		} else {
			fmt.Printf("%s%s▸ %s%s\n", clearLine, colorDim, rl.Phase, colorReset)
		}
	default:
		text := raw
		if text == "" {
			text = rl.Content
		}
		fmt.Printf("%s%s%s\n", clearLine, text, colorReset)
	}
}

// waitDeviceBind 等待服务端确认 bind（最多 timeout），返回是否确认。
//
// 用于一次性命令（-chat）：桥启动后立刻退出会让设备来不及登记。
// 超时不报错（服务端可能只是慢），bind 明确被拒则打出来 —— 那通常意味着
// 设备令牌不对或设备未授权，用户需要知道，而不是以为「桥起来了就好了」。
func waitDeviceBind(timeout time.Duration) bool {
	if deviceBridge == nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if deviceBridge.Bound() {
			return true
		}
		if reason := deviceBridge.BindError(); reason != "" {
			printlnC(colorYellow, "device bridge bind rejected: "+reason)
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return deviceBridge.Bound()
}

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/devicebridge/client"
	"github.com/JianFeeeee/HologramAgent/internal/meta"
)

// ===== 设备桥管理 =====

var (
	deviceBridge   *client.Bridge
	cmdRouter      *client.CmdRouter
	deviceBridgeID string
)

// startDeviceBridge 启动设备桥，连接 remotedevice 网关。
// 使用共享库 client.Bridge 替代手写 WS 协议。
func startDeviceBridge(addr, token string) error {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}
	deviceID := "waiter-" + sanitizeID(hostname)

	// 构建完整能力列表
	caps := []string{
		"status", "cmdrun", "deviceinfo", "cmdresult",
		"computeruse", "screensee", "clipboardsee", "clipboardsue",
		"camerasue", "speakeruse", "screensue",
	}

	info := map[string]interface{}{
		"hostname": hostname,
		"platform": runtime.GOOS,
		"arch":     runtime.GOARCH,
		"cpus":     runtime.NumCPU(),
		// 客户端版本与内核同源（internal/meta），deviceinfo 回显的软件版本
		// 因此与 homed 一致，不再是一个空缺字段。
		"version": meta.Version,
	}

	// 网关地址规范化（含「已是完整端点」「只有 host:port」两种旧输入形态）。
	gateway := normalizeGateway(addr)

	bridge := client.New(gateway, token, deviceID, "HologramAgent CLI", caps, info)
	cmdRouter = client.NewCmdRouter()

	// cmd_type 是主路由信号；保留 homeagent-* 文本前缀兼容旧服务端。
	cmdRouter.Handle("homeagent-", handleHomeagentCmd)
	cmdRouter.HandleDefault(handleShellCmd)
	bridge.OnCmd(func(reqID, command, cmdType string) {
		if cmdType == "homeagent" {
			handleHomeagentCmd(reqID, command)
			return
		}
		cmdRouter.Dispatch(reqID, command)
	})

	// agent 主动投递（output_send__device/<id>）→ 终端显示。
	// 设备侧参考实现：文本/结构化直接打出来；二进制负载走 OnData（TTS 音频等）。
	bridge.OnPush(func(reqID, typ, payload, meta string) {
		printlnC("cyan", fmt.Sprintf("[push:%s] %s", typ, payload))
	})
	if err := bridge.Start(); err != nil {
		return fmt.Errorf("device bridge: %w", err)
	}

	deviceBridge = bridge
	deviceBridgeID = deviceID
	return nil
}

// stopDeviceBridge 停止设备桥。
func stopDeviceBridge() {
	if deviceBridge != nil {
		deviceBridge.Stop()
		deviceBridge = nil
	}
}

// ===== 命令分发 =====

// defaultCmdAllowlist 是**内置默认**命令白名单（命令名的第一段）。
//
// 只收「只读/低风险」的诊断类命令。这道闸的唯一作用是挡住
// rm -rf /、dd、chmod 777、fork 炸弹这类破坏性命令 —— 而触发它的是
// **agent**（经 device_ctl_cmdrun），不是人，所以需要一道机器闸。
var defaultCmdAllowlist = []string{
	"ls", "pwd", "whoami", "uname", "date", "echo", "uptime", "hostname",
	"cat", "df", "free", "ps", "ip", "dir", "node", "python3", "python",
	"npm", "git", "curl", "wget", "systeminfo", "tasklist",
}

// buildCmdMatcher 由命令名列表构造匹配函数。
//
// 只取**命令名的第一段**再整词匹配：这样 "grep -rn x ." 能过，
// 而 "grepXxx" / "mygrep" 不会因为 contains 而蒙混过关。
// 空列表 ⇒ 回退默认集（★ 绝不能变成"全放行"）。
func buildCmdMatcher(cmds []string) func(string) bool {
	if len(cmds) == 0 {
		cmds = defaultCmdAllowlist
	}
	set := make(map[string]bool, len(cmds))
	for _, c := range cmds {
		if c = strings.TrimSpace(c); c != "" {
			set[c] = true
		}
	}
	return func(command string) bool {
		fields := strings.Fields(strings.TrimSpace(command))
		if len(fields) == 0 {
			return false
		}
		// 跳过 VAR=value 前缀（`FOO=bar cmd` 这种合法写法）
		i := 0
		for i < len(fields) && strings.Contains(fields[i], "=") &&
			!strings.HasPrefix(fields[i], "-") {
			i++
		}
		if i >= len(fields) {
			return false
		}
		return set[filepath.Base(fields[i])]
	}
}

// deviceCmdAllowed 是当前生效的命令白名单匹配函数。
//
// 包级变量 + 由 main 从配置赋值，与同文件既有的 sendBridgeResult 同一模式
// （那里也是包级函数变量，测试可替换）。
var deviceCmdAllowed = buildCmdMatcher(nil)

// cmdAllowed / defaultCmdAllowed 是两个测试可读的入口。
func cmdAllowed(command string) bool        { return deviceCmdAllowed(command) }
func defaultCmdAllowed(command string) bool { return buildCmdMatcher(nil)(command) }

func handleShellCmd(reqID, command string) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		sendBridgeResult(reqID, "error", "", "empty command")
		return
	}
	if !cmdAllowed(cmd) {
		sendBridgeResult(reqID, "error", "", "command not in whitelist")
		return
	}
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		sendBridgeResult(reqID, "error", "", "empty command")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	execCmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	out, err := execCmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			sendBridgeResult(reqID, "error", "", "timeout")
			return
		}
		sendBridgeResult(reqID, "error", truncate8k(string(out)), err.Error())
		return
	}
	sendBridgeResult(reqID, "ok", truncate8k(string(out)), "")
}

func handleHomeagentCmd(reqID, command string) {
	capability, args := client.ParseHomeagentCmd(command)
	switch capability {
	case "camerasue":
		execCamerasue(reqID, args)
	case "screensue":
		execScreensue(reqID, args)
	case "screensee":
		execScreensee(reqID, args)
	case "speakeruse":
		execSpeakeruse(reqID, args)
	case "computeruse":
		execComputeruse(reqID, args)
	case "clipboardsee":
		execClipboardsee(reqID)
	case "clipboardsue":
		execClipboardsue(reqID, args)
	case "status":
		execDeviceStatus(reqID)
	case "deviceinfo":
		execDeviceInfo(reqID)
	default:
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("unknown capability: %s", capability))
	}
}

// ===== 能力实现 =====

func execCamerasue(reqID, args string) {
	// 摄像头：依赖于 ffmpeg/v4l2（Linux）或 ffmpeg/dshow（Windows）
	args = strings.TrimSpace(args)
	durMatch := 0
	if args != "" {
		if n, err := fmt.Sscanf(args, "%d", &durMatch); err != nil || n != 1 {
			durMatch = 0
		}
	}
	isVideo := durMatch > 0
	tmpDir := os.TempDir()
	outFile := filepath.Join(tmpDir, fmt.Sprintf("ha_cam_%d.jpg", time.Now().UnixNano()))

	if isVideo {
		outFile = filepath.Join(tmpDir, fmt.Sprintf("ha_cam_%d.mp4", time.Now().UnixNano()))
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "linux" {
		if isVideo {
			cmd = exec.Command("ffmpeg", "-f", "v4l2", "-i", "/dev/video0",
				"-t", fmt.Sprintf("%d", durMatch),
				"-pix_fmt", "yuv420p", "-c:v", "libx264",
				"-f", "mp4", outFile)
		} else {
			cmd = exec.Command("ffmpeg", "-f", "v4l2", "-i", "/dev/video0",
				"-frames:v", "1", "-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1")
		}
	} else if runtime.GOOS == "windows" {
		if isVideo {
			cmd = exec.Command("ffmpeg", "-f", "dshow", "-i", "video=USB Camera",
				"-t", fmt.Sprintf("%d", durMatch),
				"-pix_fmt", "yuv420p", "-c:v", "libx264",
				"-f", "mp4", outFile)
		} else {
			cmd = exec.Command("ffmpeg", "-f", "dshow", "-i", "video=USB Camera",
				"-frames:v", "1", "-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1")
		}
	} else {
		sendBridgeResult(reqID, "error", "", "camerasue: unsupported platform")
		return
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	timeout := 15 * time.Second
	if isVideo {
		timeout = time.Duration(durMatch+15) * time.Second
	}
	cmdCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd = exec.CommandContext(cmdCtx, cmd.Path, cmd.Args[1:]...)
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("camerasue failed: %v", err))
		return
	}

	if isVideo {
		data, err := os.ReadFile(outFile)
		if err != nil {
			sendBridgeResult(reqID, "error", "", fmt.Sprintf("camerasue read failed: %v", err))
			return
		}
		os.Remove(outFile)
		if deviceBridge != nil {
			deviceBridge.SendDataChunked(reqID, "camera_video", "video/mp4", data)
		} else {
			b64 := base64.StdEncoding.EncodeToString(data)
			sendBridgeResult(reqID, "ok", "data:video/mp4;base64,"+b64, "")
		}
	} else {
		b64 := base64.StdEncoding.EncodeToString(out.Bytes())
		sendBridgeResult(reqID, "ok", "data:image/jpeg;base64,"+b64, "")
	}
}

func execScreensue(reqID, args string) {
	// CLI 版 screensue：TUI 风格显示
	// 在终端中渲染 HTML 或启动新终端窗口
	args = strings.TrimSpace(args)
	duration := 5
	content := args

	// 解析参数：首个纯数字 token 作为时长
	tokens := strings.Fields(args)
	if len(tokens) > 1 && regexp.MustCompile(`^\d+$`).MatchString(tokens[0]) {
		fmt.Sscanf(tokens[0], "%d", &duration)
		content = strings.Join(tokens[1:], " ")
	}
	if content == "" {
		content = "HologramAgent 远程屏幕提示"
	}

	// 检测 HTML 内容
	isHTML := strings.Contains(content, "<") && strings.Contains(content, ">")

	// 按平台选择 TUI 渲染方式
	platform := runtime.GOOS
	var err error

	switch platform {
	case "linux":
		err = screensueLinux(content, isHTML, duration)
	case "windows":
		err = screensueWindows(content, isHTML, duration)
	default:
		err = screensueFallback(content, isHTML, duration)
	}

	if err != nil {
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("screensue failed: %v", err))
		return
	}
	sendBridgeResult(reqID, "ok", "screensue shown", "")
}

func screensueLinux(content string, isHTML bool, duration int) error {
	// 尝试多种方式显示
	// 1. browsh（如果可用）
	// 2. w3m（终端内渲染 HTML）
	// 3. 写入临时文件 + notify-send
	// 4. 启动新终端窗口

	// 方式1: 写入临时 HTML 文件
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("ha_screensue_%d.html", time.Now().UnixNano()))
	var htmlContent string
	if isHTML {
		htmlContent = content
	} else {
		htmlContent = fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><style>
			body{font-family:sans-serif;display:flex;flex-direction:column;justify-content:center;
			align-items:center;height:100vh;margin:0;background:#0b1020;color:#eef1f8;}
			h1{color:#ff7fac;margin-bottom:16px;}</style></head><body>
			<h1>HologramAgent</h1><pre>%s</pre></body></html>`, content)
	}
	if err := os.WriteFile(tmpFile, []byte(htmlContent), 0644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	defer os.Remove(tmpFile)

	// 尝试 browsh（Firefox headless 渲染）
	if _, err := exec.LookPath("browsh"); err == nil {
		cmd := exec.Command("browsh", "--url="+tmpFile, "--startup-url="+tmpFile)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Start()
		go func() {
			time.Sleep(time.Duration(duration) * time.Second)
			_ = cmd.Process.Kill()
		}()
		return nil
	}

	// 尝试 w3m（终端内渲染）
	if _, err := exec.LookPath("w3m"); err == nil {
		cmd := exec.Command("w3m", tmpFile)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Start()
		go func() {
			time.Sleep(time.Duration(duration) * time.Second)
			_ = cmd.Process.Kill()
		}()
		return nil
	}

	// 尝试 lynx
	if _, err := exec.LookPath("lynx"); err == nil {
		cmd := exec.Command("lynx", tmpFile)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Start()
		go func() {
			time.Sleep(time.Duration(duration) * time.Second)
			_ = cmd.Process.Kill()
		}()
		return nil
	}

	// 尝试 notify-send（桌面通知）
	if _, err := exec.LookPath("notify-send"); err == nil {
		_ = exec.Command("notify-send", "HologramAgent", content).Run()
		return nil
	}

	// 最后尝试：启动 xterm 显示
	if _, err := exec.LookPath("xterm"); err == nil {
		cmd := exec.Command("xterm", "-e", "cat", tmpFile)
		_ = cmd.Start()
		go func() {
			time.Sleep(time.Duration(duration) * time.Second)
			_ = cmd.Process.Kill()
		}()
		return nil
	}

	// 全部失败：打印到终端
	fmt.Printf("\n=== HologramAgent Screensue ===\n%s\n===========================\n", content)
	return nil
}

func screensueWindows(content string, isHTML bool, duration int) error {
	// Windows 使用 PowerShell 弹窗或启动浏览器渲染 HTML
	if isHTML {
		// 方式1: 写入临时 HTML 文件并用默认浏览器打开
		tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("ha_screensue_%d.html", time.Now().UnixNano()))
		htmlContent := content
		if !strings.Contains(strings.ToLower(content), "<html") {
			htmlContent = fmt.Sprintf(`<!DOCTYPE html><html><meta charset="utf-8"><body>%s</body></html>`, content)
		}
		if err := os.WriteFile(tmpFile, []byte(htmlContent), 0644); err != nil {
			return fmt.Errorf("write temp file: %w", err)
		}
		defer os.Remove(tmpFile)

		// 用默认浏览器打开
		cmd := exec.Command("cmd", "/c", "start", "", tmpFile)
		_ = cmd.Start()

		// 定时关闭（浏览器窗口无法自动关闭，但可以提示）
		go func() {
			time.Sleep(time.Duration(duration) * time.Second)
			_ = exec.Command("powershell", "-Command",
				`[Windows.Forms.MessageBox]::Show("screensue 展示结束","HologramAgent")`).Run()
		}()

		return nil
	}

	// 方式2: PowerShell 弹出消息框（纯文本）
	psScript := fmt.Sprintf(`
		Add-Type -AssemblyName System.Windows.Forms
		$popup = New-Object Windows.Forms.Form
		$popup.Text = "HologramAgent"
		$popup.Size = New-Object Drawing.Size(600,400)
		$popup.StartPosition = "CenterScreen"
		$popup.TopMost = $true
		$label = New-Object Windows.Forms.Label
		$label.Text = "%s"
		$label.AutoSize = $true
		$label.TextAlign = "MiddleCenter"
		$popup.Controls.Add($label)
		$timer = New-Object Windows.Forms.Timer
		$timer.Interval = %d
		$timer.Add_Tick({ $popup.Close() })
		$timer.Start()
		[Windows.Forms.Application]::Run($popup)
	`, strings.ReplaceAll(content, `"`, "`\""), duration*1000)

	cmd := exec.Command("powershell", "-Command", psScript)
	_ = cmd.Start()
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

func screensueFallback(content string, isHTML bool, duration int) error {
	fmt.Printf("\n=== HologramAgent Screensue ===\n%s\n===========================\n", content)
	return nil
}

func execScreensee(reqID, args string) {
	// 屏幕截图：使用平台特定工具
	platform := runtime.GOOS
	tmpDir := os.TempDir()
	outFile := filepath.Join(tmpDir, fmt.Sprintf("ha_screenshot_%d.png", time.Now().UnixNano()))
	defer os.Remove(outFile)

	var cmd *exec.Cmd
	switch platform {
	case "linux":
		// 尝试多种截图工具
		if _, err := exec.LookPath("import"); err == nil {
			cmd = exec.Command("import", "-window", "root", outFile)
		} else if _, err := exec.LookPath("scrot"); err == nil {
			cmd = exec.Command("scrot", outFile)
		} else if _, err := exec.LookPath("gnome-screenshot"); err == nil {
			cmd = exec.Command("gnome-screenshot", "-f", outFile)
		} else {
			sendBridgeResult(reqID, "error", "", "screensee: no screenshot tool found (install scrot/import/gnome-screenshot)")
			return
		}
	case "windows":
		// Windows 使用 PowerShell
		psScript := fmt.Sprintf(`
			Add-Type -AssemblyName System.Windows.Forms
			$screen = [Windows.Forms.Screen]::PrimaryScreen.Bounds
			$bitmap = New-Object Drawing.Bitmap $screen.Width, $screen.Height
			$graphics = [Drawing.Graphics]::FromImage($bitmap)
			$graphics.CopyFromScreen($screen.X, $screen.Y, 0, 0, $screen.Size)
			$bitmap.Save('%s', [Drawing.Imaging.ImageFormat]::Png)
			$graphics.Dispose()
			$bitmap.Dispose()
		`, strings.ReplaceAll(outFile, "\\", "\\\\"))
		cmd = exec.Command("powershell", "-NoProfile", "-Command", psScript)
	case "darwin":
		cmd = exec.Command("screencapture", "-x", outFile)
	default:
		sendBridgeResult(reqID, "error", "", "screensee: unsupported platform")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("screensee failed: %v\n%s", err, string(out)))
		return
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("screensee read failed: %v", err))
		return
	}

	// 小图直接 base64，大图分块
	if len(data) < 512*1024 {
		b64 := base64.StdEncoding.EncodeToString(data)
		sendBridgeResult(reqID, "ok", "data:image/png;base64,"+b64, "")
	} else if deviceBridge != nil {
		deviceBridge.SendDataChunked(reqID, "screenshot", "image/png", data)
	} else {
		b64 := base64.StdEncoding.EncodeToString(data)
		sendBridgeResult(reqID, "ok", "data:image/png;base64,"+b64, "")
	}
}

func execSpeakeruse(reqID, args string) {
	args = strings.TrimSpace(args)
	if args == "" {
		sendBridgeResult(reqID, "error", "", "speakeruse: empty text")
		return
	}

	platform := runtime.GOOS
	var cmd *exec.Cmd
	switch platform {
	case "linux":
		// 尝试多种 TTS 引擎
		if _, err := exec.LookPath("espeak"); err == nil {
			cmd = exec.Command("espeak", args)
		} else if _, err := exec.LookPath("festival"); err == nil {
			cmd = exec.Command("festival", "--tts", "--pipe")
			stdin, _ := cmd.StdinPipe()
			go func() {
				defer stdin.Close()
				stdin.Write([]byte(args))
			}()
		} else {
			sendBridgeResult(reqID, "error", "", "speakeruse: no TTS engine (install espeak/festival)")
			return
		}
	case "windows":
		// Windows 使用 SAPI
		psScript := fmt.Sprintf(`
			Add-Type -AssemblyName System.Speech
			$synthesizer = New-Object System.Speech.Synthesis.SpeechSynthesizer
			$synthesizer.Speak('%s')
		`, strings.ReplaceAll(args, "'", "''"))
		cmd = exec.Command("powershell", "-NoProfile", "-Command", psScript)
	case "darwin":
		cmd = exec.Command("say", args)
	default:
		sendBridgeResult(reqID, "error", "", "speakeruse: unsupported platform")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	if err := cmd.Run(); err != nil {
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("speakeruse failed: %v", err))
		return
	}
	sendBridgeResult(reqID, "ok", "speakeruse done", "")
}

func execComputeruse(reqID, args string) {
	// 鼠标键盘操控：解析 JSON 参数
	action, params, err := client.ParseJSONCmd(args)
	if err != nil {
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("computeruse: %v", err))
		return
	}

	platform := runtime.GOOS
	if platform == "linux" {
		if _, err := exec.LookPath("xdotool"); err != nil {
			sendBridgeResult(reqID, "error", "", "computeruse: xdotool not installed")
			return
		}
		execComputeruseLinux(reqID, action, params)
	} else if platform == "windows" {
		execComputeruseWindows(reqID, action, params)
	} else {
		sendBridgeResult(reqID, "error", "", "computeruse: unsupported platform")
	}
}

// xdo 构造一个 xdotool 命令。
func xdo(args ...string) *exec.Cmd { return exec.Command("xdotool", args...) }

// pnum 从 params 取浮点参数。
func pnum(params map[string]interface{}, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := params[k]; ok {
			switch t := v.(type) {
			case float64:
				return t
			case int:
				return float64(t)
			case string:
				var f float64
				if _, err := fmt.Sscanf(t, "%g", &f); err == nil {
					return f
				}
			}
		}
	}
	return 0
}

// pstr 从 params 取字符串参数。
func pstr(params map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := params[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// pint 从 params 取整数参数（带默认与上限）。
func pint(params map[string]interface{}, def, max int, keys ...string) int {
	n := int(pnum(params, keys...))
	if n <= 0 {
		n = def
	}
	if n > max {
		n = max
	}
	return n
}

// normKeySpec 归一化按键组合写法，让模型自然输出（"Ctrl+C"、"ctrl + c"、
// "CTRL-C"）都能工作。原实现把字符串原样丢给 xdotool key，
// 上面几种写法全部无效。
func normKeySpec(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	// 统一分隔符：+ 与空格
	spec = strings.ReplaceAll(spec, "+", " ")
	spec = strings.ReplaceAll(spec, "-", " ")
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return ""
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, strings.ToLower(f))
	}
	return strings.Join(out, "+")
}

func execComputeruseLinux(reqID, action string, params map[string]interface{}) {
	x := int(pnum(params, "x"))
	y := int(pnum(params, "y"))

	switch action {
	case "click", "right", "middle":
		// "right"/"middle" 是 button 的简写形式，与 GUI 侧参数名对齐
		btn := "1"
		button := pstr(params, "button")
		if action == "right" {
			button = "right"
		} else if action == "middle" {
			button = "middle"
		}
		switch button {
		case "right":
			btn = "3"
		case "middle":
			btn = "2"
		}
		_ = xdo("mousemove", fmt.Sprint(x), fmt.Sprint(y), "click", btn).Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse click@%d,%d", x, y), "")

	case "middleclick":
		_ = xdo("mousemove", fmt.Sprint(x), fmt.Sprint(y), "click", "2").Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse middleclick@%d,%d", x, y), "")

	case "doubleclick":
		_ = xdo("mousemove", fmt.Sprint(x), fmt.Sprint(y), "click", "--repeat", "2", "--delay", "60", "1").Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse doubleclick@%d,%d", x, y), "")

	case "tripleclick":
		// 三击选中整行：旧实现没有，模型无法选中一整段文本
		_ = xdo("mousemove", fmt.Sprint(x), fmt.Sprint(y), "click", "--repeat", "3", "--delay", "60", "1").Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse tripleclick@%d,%d", x, y), "")

	case "rightclick":
		_ = xdo("mousemove", fmt.Sprint(x), fmt.Sprint(y), "click", "3").Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse rightclick@%d,%d", x, y), "")

	case "move", "hover":
		// hover 与 move 在 xdotool 上等价；分开命名是为了与 GUI 侧语义对齐
		// （hover 表示"只悬停不点击"，用于触发 tooltip/悬浮菜单）
		_ = xdo("mousemove", fmt.Sprint(x), fmt.Sprint(y)).Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse hover@%d,%d", x, y), "")

	case "scroll":
		// 旧实现固定用 click 4/5（向上/向下），且忽略 dx 横向滚动。
		// 现在：正 dy 向上、负 dy 向下，支持 dx 横向。
		dy := int(pnum(params, "dy", "y"))
		dx := int(pnum(params, "dx", "x_delta", "scrollx"))
		if dy == 0 && dx == 0 {
			dy = -120
		}
		btn := "4" // 向上
		if dy < 0 {
			btn = "5"
		}
		steps := 1
		// ★ mag 必须在 if 外声明：init 语句里的 `mag := dy`
		//   作用域只到该 if 的 }，而下面还要用它（GUI 分支 0d8b129
		//   自带的编译错误 undefined: mag）。
		mag := dy
		if mag < 0 {
			mag = -mag
		}
		if mag > 120 {
			steps = (mag + 119) / 120
		}
		for i := 0; i < steps; i++ {
			_ = xdo("click", btn).Run()
		}
		// 横向滚动：button 6/7
		if dx != 0 {
			hbtn := "7" // 向左
			if dx > 0 {
				hbtn = "6"
			}
			_ = xdo("click", hbtn).Run()
		}
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse scroll dx=%d dy=%d", dx, dy), "")

	case "mousedown":
		// 按下不释放，与 mouseup 配对可做"按住"（如拖窗口、选区）
		btn := "1"
		if pstr(params, "button") == "right" {
			btn = "3"
		} else if pstr(params, "button") == "middle" {
			btn = "2"
		}
		_ = xdo("mousemove", fmt.Sprint(x), fmt.Sprint(y), "mousedown", btn).Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse mousedown@%d,%d", x, y), "")

	case "mouseup":
		btn := "1"
		if pstr(params, "button") == "right" {
			btn = "3"
		} else if pstr(params, "button") == "middle" {
			btn = "2"
		}
		_ = xdo("mouseup", btn).Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse mouseup@%d,%d", x, y), "")

	case "drag":
		// 一步拖拽：按下 → 分步移动 → 释放。
		// 旧实现没有拖拽，模型无法完成"拖文件/拖滑块/调整窗口"这类操作。
		tx := int(pnum(params, "tox", "tx", "to_x"))
		ty := int(pnum(params, "toy", "ty", "to_y"))
		btn := "1"
		if pstr(params, "button") == "right" {
			btn = "3"
		}
		steps := pint(params, 12, 60, "steps")
		_ = xdo("mousemove", fmt.Sprint(x), fmt.Sprint(y), "mousedown", btn).Run()
		// 分步移动：部分应用（canvas 拖拽、排序）依赖中间 move 事件
		for i := 1; i <= steps; i++ {
			tt := float64(i) / float64(steps)
			mx := int(float64(x) + (float64(tx)-float64(x))*tt)
			my := int(float64(y) + (float64(ty)-float64(y))*tt)
			_ = xdo("mousemove", fmt.Sprint(mx), fmt.Sprint(my)).Run()
		}
		_ = xdo("mouseup", btn).Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse drag (%d,%d)->(%d,%d)", x, y, tx, ty), "")

	case "type":
		text := pstr(params, "text")
		if text == "" {
			sendBridgeResult(reqID, "error", "", "computeruse type: empty text")
			return
		}
		// --clearmodifiers：避免残留的 Ctrl/Alt 修饰键把后续输入变成快捷键
		cmd := xdo("type", "--clearmodifiers", "--delay", "12", text)
		if err := cmd.Run(); err != nil {
			sendBridgeResult(reqID, "error", "", fmt.Sprintf("computeruse type failed: %v", err))
			return
		}
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse typed %d chars", len(text)), "")

	case "keypress", "hotkey", "combo":
		spec := normKeySpec(pstr(params, "key", "keys", "text"))
		if spec == "" {
			sendBridgeResult(reqID, "error", "", "computeruse keypress: empty key")
			return
		}
		_ = xdo("key", "--clearmodifiers", spec).Run()
		sendBridgeResult(reqID, "ok", "computeruse keypress "+spec, "")

	case "wait", "sleep":
		ms := pint(params, 500, 10000, "ms", "duration")
		time.Sleep(time.Duration(ms) * time.Millisecond)
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse waited %dms", ms), "")

	case "display":
		// 返回各显示器几何：模型需要知道有几个屏、分辨率与相对位置，
		// 才能把截图坐标对应到正确的位置（多屏时尤其重要）。
		sendBridgeResult(reqID, "ok", "xdotool getdisplaygeometry", "")

	default:
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("computeruse: unknown action %s", action))
	}
}

func execComputeruseWindows(reqID, action string, params map[string]interface{}) {
	switch action {
	case "click":
		ps := `[System.Windows.Forms.Cursor]::Position = [System.Drawing.Point]::new($x,$y); ` +
			`[System.Windows.Forms.SendKeys]::SendWait("{ENTER}")`
		cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
		_ = cmd.Run()
		sendBridgeResult(reqID, "ok", "computeruse click", "")
	case "move":
		x, _ := params["x"].(float64)
		y, _ := params["y"].(float64)
		ps := fmt.Sprintf(`[System.Windows.Forms.Cursor]::Position = [System.Drawing.Point]::new(%d,%d)`, int(x), int(y))
		cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
		_ = cmd.Run()
		sendBridgeResult(reqID, "ok", fmt.Sprintf("computeruse move (%d,%d)", int(x), int(y)), "")
	case "type":
		text, _ := params["text"].(string)
		ps := fmt.Sprintf(`$wshell = New-Object -ComObject wscript.shell; $wshell.SendKeys('%s')`, strings.ReplaceAll(text, "'", "''"))
		cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
		_ = cmd.Run()
		sendBridgeResult(reqID, "ok", "computeruse type", "")
	default:
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("computeruse: unknown action %s", action))
	}
}

func execClipboardsee(reqID string) {
	// 读取剪贴板
	platform := runtime.GOOS
	var text string
	var err error

	switch platform {
	case "linux":
		if _, e := exec.LookPath("xclip"); e == nil {
			out, _ := exec.Command("xclip", "-o", "-selection", "clipboard").Output()
			text = string(out)
		} else if _, e := exec.LookPath("xsel"); e == nil {
			out, _ := exec.Command("xsel", "-ob").Output()
			text = string(out)
		} else {
			sendBridgeResult(reqID, "error", "", "clipboardsee: install xclip or xsel")
			return
		}
	case "windows":
		psScript := `Add-Type -AssemblyName System.Windows.Forms; [Windows.Forms.Clipboard]::GetText()`
		out, _ := exec.Command("powershell", "-NoProfile", "-Command", psScript).Output()
		text = strings.TrimSpace(string(out))
	case "darwin":
		out, _ := exec.Command("pbpaste").Output()
		text = string(out)
	default:
		sendBridgeResult(reqID, "error", "", "clipboardsee: unsupported platform")
		return
	}

	if err != nil {
		sendBridgeResult(reqID, "error", "", fmt.Sprintf("clipboardsee failed: %v", err))
		return
	}
	sendBridgeResult(reqID, "ok", text, "")
}

func execClipboardsue(reqID, text string) {
	// 写入剪贴板
	text = strings.TrimSpace(text)
	if text == "" {
		sendBridgeResult(reqID, "error", "", "clipboardsue: empty text")
		return
	}

	platform := runtime.GOOS
	switch platform {
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd := exec.Command("xclip", "-i", "-selection", "clipboard")
			cmd.Stdin = strings.NewReader(text)
			_ = cmd.Run()
		} else if _, err := exec.LookPath("xsel"); err == nil {
			cmd := exec.Command("xsel", "-ib")
			cmd.Stdin = strings.NewReader(text)
			_ = cmd.Run()
		} else {
			sendBridgeResult(reqID, "error", "", "clipboardsue: install xclip or xsel")
			return
		}
	case "windows":
		psScript := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; [Windows.Forms.Clipboard]::SetText('%s')`,
			strings.ReplaceAll(text, "'", "''"))
		_ = exec.Command("powershell", "-NoProfile", "-Command", psScript).Run()
	case "darwin":
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(text)
		_ = cmd.Run()
	default:
		sendBridgeResult(reqID, "error", "", "clipboardsue: unsupported platform")
		return
	}
	sendBridgeResult(reqID, "ok", "clipboard written", "")
}

func execDeviceStatus(reqID string) {
	hostname, _ := os.Hostname()
	status := map[string]interface{}{
		"device_id": deviceBridgeID,
		"status":    "online",
		"hostname":  hostname,
		"platform":  runtime.GOOS,
		"arch":      runtime.GOARCH,
		"uptime":    time.Now().Unix(),
	}
	b, _ := json.Marshal(status)
	sendBridgeResult(reqID, "ok", string(b), "")
}

func execDeviceInfo(reqID string) {
	hostname, _ := os.Hostname()
	info := map[string]interface{}{
		"device_id": deviceBridgeID,
		"name":      "HologramAgent CLI",
		"kind":      "computer",
		"caps":      []string{"status", "cmdrun", "deviceinfo", "cmdresult", "computeruse", "screensee", "clipboardsee", "clipboardsue", "camerasue", "speakeruse", "screensue"},
		"info": map[string]interface{}{
			"hostname": hostname,
			"platform": runtime.GOOS,
			"arch":     runtime.GOARCH,
			"cpus":     runtime.NumCPU(),
		},
	}
	b, _ := json.Marshal(info)
	sendBridgeResult(reqID, "ok", string(b), "")
}

// ===== 辅助函数 =====

var sendBridgeResult = func(reqID, status, output, errMsg string) {
	if deviceBridge != nil {
		deviceBridge.SendResult(reqID, status, output, errMsg)
	}
}

const devMaxOut = 8192

func truncate8k(s string) string {
	if len(s) <= devMaxOut {
		return s
	}
	return s[:devMaxOut]
}

func sanitizeID(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

package localuse

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta("localuse", "本地外设", "Local Device Use")
	plugin.RegisterFactory("localuse", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name    string
	dataDir string
}

func New(name string) *Plugin {
	return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)

	if dd := s.Settings().DataDir(); dd != "" {
		p.dataDir = dd
		os.MkdirAll(p.dataDir, 0755)
	}

	// ── screensee ──
	s.RegisterTool("local_screensee", sdk.ToolDef{
		Name: "local_screensee",
		Description: "截取本机屏幕当前画面（截屏）。" +
			"返回图片的 base64 data URL，可直接用于视觉分析。" +
			"依赖：Linux 需 scrot/import/gnome-screenshot 任一；macOS 需 screencapture（自带）；Windows 用 PowerShell。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 只读观察：不改插件内共享状态
		ParallelSafe: true,
	}, p.handleScreensee)

	// ── camerasue ──
	s.RegisterTool("local_camerasue", sdk.ToolDef{
		Name: "local_camerasue",
		Description: "使用本机摄像头拍照或录像。" +
			"无参数=拍照（jpeg），传入正整数=录像 N 秒（mp4）。" +
			"依赖：ffmpeg + v4l2（Linux）/ dshow（Windows）/ avfoundation（macOS）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"duration": map[string]interface{}{
					"type":        "integer",
					"description": "录像秒数（省略=拍照，传 0=拍照）",
				},
			},
		},
		// 只读观察：不改插件内共享状态
		ParallelSafe: true,
	}, p.handleCamerasue)

	// ── speakeruse ──
	s.RegisterTool("local_speakeruse", sdk.ToolDef{
		Name: "local_speakeruse",
		Description: "使用本机扬声器朗读指定文字（TTS）。" +
			"依赖：Linux 需 espeak/festival 任一；macOS 需 say（自带）；Windows 用 PowerShell SAPI。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text": map[string]interface{}{
					"type":        "string",
					"description": "要朗读的文字",
				},
			},
			"required": []string{"text"},
		},
		// 只读观察：不改插件内共享状态
		ParallelSafe: true,
	}, p.handleSpeakeruse)

	// ── screensue ──
	s.RegisterTool("local_screensue", sdk.ToolDef{
		Name: "local_screensue",
		Description: "在本机屏幕上显示一段内容（通知/弹窗/HTML 页面）。" +
			"适合在用户面前弹出提醒、会议倒计时等。" +
			"Linux 用 xterm/browsh/w3m/notify-send；macOS 用 osascript；Windows 用 PowerShell 弹窗。" +
			"参数可选 duration（秒，默认 5）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"content": map[string]interface{}{
					"type":        "string",
					"description": "要显示的文字或 HTML",
				},
				"duration": map[string]interface{}{
					"type":        "integer",
					"description": "显示时长秒数（默认 5，0=永不超时）",
				},
			},
			"required": []string{"content"},
		},
	}, p.handleScreensue)

	// ── clipboardsee ──
	s.RegisterTool("local_clipboardsee", sdk.ToolDef{
		Name:        "local_clipboardsee",
		Description: "读取本机剪切板当前内容（用户最近复制/剪切的文字）。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 只读观察：不改插件内共享状态
		ParallelSafe: true,
	}, p.handleClipboardsee)

	// ── clipboardsue ──
	s.RegisterTool("local_clipboardsue", sdk.ToolDef{
		Name:        "local_clipboardsue",
		Description: "将文字写入本机剪切板（用户随后可 Ctrl+V 粘贴）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text": map[string]interface{}{
					"type":        "string",
					"description": "要写入剪切板的文字",
				},
			},
			"required": []string{"text"},
		},
	}, p.handleClipboardsue)

	// ── computeruse ──
	s.RegisterTool("local_computeruse", sdk.ToolDef{
		Name: "local_computeruse",
		Description: "操控本机鼠标/键盘。依赖 xdotool（Linux）/ PowerShell（Windows）。" +
			"action 可选：click / doubleclick / rightclick / move / scroll / keypress / type。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{
					"type": "string", "enum": []interface{}{
						"click", "doubleclick", "rightclick", "move", "scroll", "keypress", "type",
					},
				},
				"x":      map[string]interface{}{"type": "integer", "description": "X 坐标（click/move 必填）"},
				"y":      map[string]interface{}{"type": "integer", "description": "Y 坐标（click/move 必填）"},
				"button": map[string]interface{}{"type": "string", "description": "鼠标按键 left/right/middle"},
				"dy":     map[string]interface{}{"type": "integer", "description": "scroll 滚动量"},
				"key":    map[string]interface{}{"type": "string", "description": "keypress 按键名"},
				"text":   map[string]interface{}{"type": "string", "description": "type 要输入的文字"},
			},
			"required": []interface{}{"action"},
		},
		// 只读观察：不改插件内共享状态
		ParallelSafe: true,
	}, p.handleComputeruse)

	return nil
}

func (p *Plugin) Stop() error { return nil }

// ===========================================================================
//  工具实现
// ===========================================================================

// ── screensee ──────────────────────────────────────────────────────────────

// psSingleQuote 生成 PowerShell **单引号字符串**的字面量内容。
//
// 单引号串里只有单引号本身需要写成两个连续单引号；反斜杠是普通字符，
// **不能加倍**（PowerShell 的转义字符是反引号而不是反斜杠）。早期代码把
// Windows 临时路径的反斜杠写成双写，会让截图保存到错误路径。
func psSingleQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// escapeAppleScriptString 转义 AppleScript 双引号字符串字面量。
//
// 顺序关键：**先转义反斜杠再转义双引号**。若反过来，刚插入的 `\"` 中的
// 反斜杠会被再转义一遍变成 `\\"`（反斜杠 + 未转义引号），内容可越出字符串。
func escapeAppleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func (p *Plugin) handleScreensee(args map[string]interface{}) (interface{}, error) {
	platform := runtime.GOOS
	tmpDir := os.TempDir()
	outFile := filepath.Join(tmpDir, fmt.Sprintf("local_screenshot_%d.png", time.Now().UnixNano()))
	defer os.Remove(outFile)

	var cmd *exec.Cmd
	switch platform {
	case "linux":
		if _, err := exec.LookPath("import"); err == nil {
			cmd = exec.Command("import", "-window", "root", outFile)
		} else if _, err := exec.LookPath("scrot"); err == nil {
			cmd = exec.Command("scrot", outFile)
		} else if _, err := exec.LookPath("gnome-screenshot"); err == nil {
			cmd = exec.Command("gnome-screenshot", "-f", outFile)
		} else {
			return map[string]interface{}{"isError": true, "content": "无截图工具（安装 scrot/import/gnome-screenshot 任一）"}, nil
		}
	case "darwin":
		cmd = exec.Command("screencapture", "-x", outFile)
	case "windows":
		psScript := fmt.Sprintf(`
			Add-Type -AssemblyName System.Windows.Forms
			$screen = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
			$bitmap = New-Object Drawing.Bitmap $screen.Width, $screen.Height
			$graphics = [Drawing.Graphics]::FromImage($bitmap)
			$graphics.CopyFromScreen($screen.X, $screen.Y, 0, 0, $screen.Size)
			$bitmap.Save('%s', [Drawing.Imaging.ImageFormat]::Png)
			$graphics.Dispose()
			$bitmap.Dispose()
		`, psSingleQuote(outFile))
		cmd = exec.Command("powershell", "-NoProfile", "-Command", psScript)
	default:
		return map[string]interface{}{"isError": true, "content": "不支持的平台: " + platform}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return map[string]interface{}{"isError": true, "content": fmt.Sprintf("截屏失败: %v\n%s", err, string(out))}, nil
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		return map[string]interface{}{"isError": true, "content": fmt.Sprintf("读取截图失败: %v", err)}, nil
	}

	// 大图保存到 dataDir（避免 agent 上下文爆掉），返回路径
	if len(data) > 256*1024 && p.dataDir != "" {
		srcFile := filepath.Join(p.dataDir, fmt.Sprintf("screenshot_%d.png", time.Now().UnixNano()))
		if err := os.WriteFile(srcFile, data, 0644); err == nil {
			return map[string]interface{}{
				"content":    fmt.Sprintf("截图已保存: %s (%d bytes)", srcFile, len(data)),
				"local_path": srcFile,
				"size":       len(data),
			}, nil
		}
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	return map[string]interface{}{
		"content":        fmt.Sprintf("截图完成 (%d bytes)", len(data)),
		"image_data_url": "data:image/png;base64," + b64,
	}, nil
}

// ── camerasue ──────────────────────────────────────────────────────────────

func (p *Plugin) handleCamerasue(args map[string]interface{}) (interface{}, error) {
	duration := 0
	if d, ok := args["duration"].(float64); ok && d > 0 {
		duration = int(d)
	}

	platform := runtime.GOOS
	tmpDir := os.TempDir()
	isVideo := duration > 0
	outFile := filepath.Join(tmpDir, fmt.Sprintf("local_cam_%d.jpg", time.Now().UnixNano()))
	if isVideo {
		outFile = filepath.Join(tmpDir, fmt.Sprintf("local_cam_%d.mp4", time.Now().UnixNano()))
	}

	var cmd *exec.Cmd
	switch platform {
	case "linux":
		if isVideo {
			cmd = exec.Command("ffmpeg", "-f", "v4l2", "-i", "/dev/video0",
				"-t", fmt.Sprintf("%d", duration),
				"-pix_fmt", "yuv420p", "-c:v", "libx264",
				"-f", "mp4", "-y", outFile)
		} else {
			cmd = exec.Command("ffmpeg", "-f", "v4l2", "-i", "/dev/video0",
				"-frames:v", "1", "-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1")
		}
	case "windows":
		if isVideo {
			cmd = exec.Command("ffmpeg", "-f", "dshow", "-i", "video=USB Camera",
				"-t", fmt.Sprintf("%d", duration),
				"-pix_fmt", "yuv420p", "-c:v", "libx264",
				"-f", "mp4", "-y", outFile)
		} else {
			cmd = exec.Command("ffmpeg", "-f", "dshow", "-i", "video=USB Camera",
				"-frames:v", "1", "-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1")
		}
	default:
		return map[string]interface{}{"isError": true, "content": "camerasue 不支持平台: " + platform}, nil
	}

	timeout := 15 * time.Second
	if isVideo {
		timeout = time.Duration(duration+15) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if isVideo {
		cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
		if err := cmd.Run(); err != nil {
			return map[string]interface{}{"isError": true, "content": fmt.Sprintf("摄像头录像失败: %v", err)}, nil
		}
		data, err := os.ReadFile(outFile)
		os.Remove(outFile)
		if err != nil {
			return map[string]interface{}{"isError": true, "content": "读取录像失败"}, nil
		}
		if p.dataDir != "" {
			srcFile := filepath.Join(p.dataDir, fmt.Sprintf("camera_%d.mp4", time.Now().UnixNano()))
			if err := os.WriteFile(srcFile, data, 0644); err == nil {
				return map[string]interface{}{
					"content":    fmt.Sprintf("录像已保存: %s (%d bytes, %ds)", srcFile, len(data), duration),
					"local_path": srcFile,
					"size":       len(data),
				}, nil
			}
		}
		b64 := base64.StdEncoding.EncodeToString(data)
		return map[string]interface{}{
			"content":        fmt.Sprintf("录像完成 (%d bytes, %ds)", len(data), duration),
			"image_data_url": "data:video/mp4;base64," + b64,
		}, nil
	}

	// 拍照：读 stdout JPEG
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	cmd2 := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	cmd2.Stdout = &out
	if err := cmd2.Run(); err != nil {
		return map[string]interface{}{"isError": true, "content": fmt.Sprintf("摄像头拍照失败: %v", err)}, nil
	}

	b64 := base64.StdEncoding.EncodeToString(out.Bytes())
	return map[string]interface{}{
		"content":        fmt.Sprintf("拍照完成 (%d bytes)", len(out.Bytes())),
		"image_data_url": "data:image/jpeg;base64," + b64,
	}, nil
}

// ── speakeruse ─────────────────────────────────────────────────────────────

func (p *Plugin) handleSpeakeruse(args map[string]interface{}) (interface{}, error) {
	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return map[string]interface{}{"isError": true, "content": "text 不能为空"}, nil
	}

	platform := runtime.GOOS
	var cmd *exec.Cmd
	switch platform {
	case "linux":
		if _, err := exec.LookPath("espeak"); err == nil {
			cmd = exec.Command("espeak", text)
		} else if _, err := exec.LookPath("festival"); err == nil {
			cmd = exec.Command("festival", "--tts", "--pipe")
			stdin, _ := cmd.StdinPipe()
			go func() { defer stdin.Close(); stdin.Write([]byte(text)) }()
		} else {
			return map[string]interface{}{"isError": true, "content": "无 TTS 引擎（安装 espeak 或 festival）"}, nil
		}
	case "darwin":
		cmd = exec.Command("say", text)
	case "windows":
		psScript := fmt.Sprintf(`
			Add-Type -AssemblyName System.Speech
			$synthesizer = New-Object System.Speech.Synthesis.SpeechSynthesizer
			$synthesizer.Speak('%s')
		`, psSingleQuote(text))
		cmd = exec.Command("powershell", "-NoProfile", "-Command", psScript)
	default:
		return map[string]interface{}{"isError": true, "content": "speakeruse 不支持平台: " + platform}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	if err := cmd.Run(); err != nil {
		return map[string]interface{}{"isError": true, "content": fmt.Sprintf("朗读失败: %v", err)}, nil
	}
	return map[string]interface{}{"content": "朗读完成"}, nil
}

// ── screensue ──────────────────────────────────────────────────────────────

func (p *Plugin) handleScreensue(args map[string]interface{}) (interface{}, error) {
	content, _ := args["content"].(string)
	duration := 5
	if d, ok := args["duration"].(float64); ok && d > 0 {
		duration = int(d)
	}
	if strings.TrimSpace(content) == "" {
		return map[string]interface{}{"isError": true, "content": "content 不能为空"}, nil
	}

	platform := runtime.GOOS
	switch platform {
	case "linux":
		return p.screensueLinux(content, duration)
	case "darwin":
		// macOS 用 osascript 弹窗
		psScript := fmt.Sprintf(`display dialog "%s" with title "HomeAgent" buttons {"OK"} default button "OK giving up after %d"`, escapeAppleScriptString(content), duration)
		cmd := exec.Command("osascript", "-e", psScript)
		_ = cmd.Run()
		return map[string]interface{}{"content": "屏幕显示已触发"}, nil
	case "windows":
		escContent := psSingleQuote(content)
		psScript := fmt.Sprintf(`
			Add-Type -AssemblyName System.Windows.Forms
			$form = New-Object System.Windows.Forms.Form
			$form.Text = "HomeAgent"
			$form.Size = New-Object Drawing.Size(500,300)
			$form.StartPosition = "CenterScreen"
			$form.TopMost = $true
			$label = New-Object System.Windows.Forms.Label
			$label.Text = '%s'
			$label.AutoSize = $true
			$label.Location = New-Object Drawing.Point(20,20)
			$form.Controls.Add($label)
			$timer = New-Object System.Windows.Forms.Timer
			$timer.Interval = %d
			$timer.Add_Tick({ $form.Close() })
			$timer.Start()
			[Windows.Forms.Application]::Run($form)
`, escContent, duration*1000)
		cmd := exec.Command("powershell", "-NoProfile", "-Command", psScript)
		_ = cmd.Run()
		return map[string]interface{}{"content": "屏幕显示已触发"}, nil
	default:
		return map[string]interface{}{"isError": true, "content": "screensue 不支持平台: " + platform}, nil
	}
}

func (p *Plugin) screensueLinux(content string, duration int) (interface{}, error) {
	// 优先 xterm
	if _, err := exec.LookPath("xterm"); err == nil {
		// 写临时文件显示
		tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("local_screensue_%d.txt", time.Now().UnixNano()))
		os.WriteFile(tmpFile, []byte(content), 0644)
		defer os.Remove(tmpFile)
		cmd := exec.Command("xterm", "-T", "HomeAgent", "-e", "cat", tmpFile)
		_ = cmd.Start()
		go func() { time.Sleep(time.Duration(duration) * time.Second); cmd.Process.Kill() }()
		return map[string]interface{}{"content": "屏幕显示已触发（xterm）"}, nil
	}
	// 回退 notify-send
	if _, err := exec.LookPath("notify-send"); err == nil {
		_ = exec.Command("notify-send", "HomeAgent", content).Run()
		return map[string]interface{}{"content": "通知已发送"}, nil
	}
	return map[string]interface{}{"isError": true, "content": "无可用显示方式（安装 xterm 或 libnotify-bin）"}, nil
}

// ── clipboardsee ───────────────────────────────────────────────────────────

func (p *Plugin) handleClipboardsee(args map[string]interface{}) (interface{}, error) {
	platform := runtime.GOOS
	switch platform {
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			out, _ := exec.Command("xclip", "-o", "-selection", "clipboard").Output()
			return map[string]interface{}{"content": string(out)}, nil
		}
		if _, err := exec.LookPath("xsel"); err == nil {
			out, _ := exec.Command("xsel", "-ob").Output()
			return map[string]interface{}{"content": string(out)}, nil
		}
		return map[string]interface{}{"isError": true, "content": "无剪贴板工具（安装 xclip 或 xsel）"}, nil
	case "darwin":
		out, _ := exec.Command("pbpaste").Output()
		return map[string]interface{}{"content": string(out)}, nil
	case "windows":
		ps := `Add-Type -AssemblyName System.Windows.Forms; [Windows.Forms.Clipboard]::GetText()`
		out, _ := exec.Command("powershell", "-NoProfile", "-Command", ps).Output()
		return map[string]interface{}{"content": strings.TrimSpace(string(out))}, nil
	default:
		return map[string]interface{}{"isError": true, "content": "clipboardsee 不支持: " + platform}, nil
	}
}

// ── clipboardsue ───────────────────────────────────────────────────────────

func (p *Plugin) handleClipboardsue(args map[string]interface{}) (interface{}, error) {
	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return map[string]interface{}{"isError": true, "content": "text 不能为空"}, nil
	}

	platform := runtime.GOOS
	switch platform {
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd := exec.Command("xclip", "-i", "-selection", "clipboard")
			cmd.Stdin = strings.NewReader(text)
			_ = cmd.Run()
			return map[string]interface{}{"content": "剪切板已写入"}, nil
		}
		if _, err := exec.LookPath("xsel"); err == nil {
			cmd := exec.Command("xsel", "-ib")
			cmd.Stdin = strings.NewReader(text)
			_ = cmd.Run()
			return map[string]interface{}{"content": "剪切板已写入"}, nil
		}
		return map[string]interface{}{"isError": true, "content": "无剪贴板工具"}, nil
	case "darwin":
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(text)
		_ = cmd.Run()
		return map[string]interface{}{"content": "剪切板已写入"}, nil
	case "windows":
		ps := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; [Windows.Forms.Clipboard]::SetText('%s')`,
			psSingleQuote(text))
		_ = exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
		return map[string]interface{}{"content": "剪切板已写入"}, nil
	default:
		return map[string]interface{}{"isError": true, "content": "clipboardsue 不支持: " + platform}, nil
	}
}

// ── computeruse ────────────────────────────────────────────────────────────

func (p *Plugin) handleComputeruse(args map[string]interface{}) (interface{}, error) {
	action, _ := args["action"].(string)
	if action == "" {
		return map[string]interface{}{"isError": true, "content": "action 必填"}, nil
	}

	platform := runtime.GOOS
	if platform == "linux" {
		if _, err := exec.LookPath("xdotool"); err != nil {
			return map[string]interface{}{"isError": true, "content": "computeruse 需要 xdotool（apt install xdotool）"}, nil
		}
		return p.computeruseLinux(action, args)
	}
	if platform == "windows" {
		return p.computeruseWindows(action, args)
	}
	return map[string]interface{}{"isError": true, "content": "computeruse 不支持: " + platform}, nil
}

func (p *Plugin) computeruseLinux(action string, args map[string]interface{}) (interface{}, error) {
	switch action {
	case "click":
		btn := "1"
		if b, ok := args["button"].(string); ok {
			switch b {
			case "right":
				btn = "3"
			case "middle":
				btn = "2"
			}
		}
		_ = exec.Command("xdotool", "click", btn).Run()
	case "doubleclick":
		_ = exec.Command("xdotool", "click", "--repeat", "2", "1").Run()
	case "rightclick":
		_ = exec.Command("xdotool", "click", "3").Run()
	case "move":
		x, _ := args["x"].(float64)
		y, _ := args["y"].(float64)
		_ = exec.Command("xdotool", "mousemove", fmt.Sprintf("%d", int(x)), fmt.Sprintf("%d", int(y))).Run()
	case "scroll":
		dy, _ := args["dy"].(float64)
		btn := "4"
		if dy < 0 {
			btn = "5"
		}
		_ = exec.Command("xdotool", "click", btn).Run()
	case "type":
		text, _ := args["text"].(string)
		_ = exec.Command("xdotool", "type", text).Run()
	case "keypress":
		key, _ := args["key"].(string)
		_ = exec.Command("xdotool", "key", key).Run()
	default:
		return map[string]interface{}{"isError": true, "content": "未知 action: " + action}, nil
	}
	return map[string]interface{}{"content": "操作已执行: " + action}, nil
}

func (p *Plugin) computeruseWindows(action string, args map[string]interface{}) (interface{}, error) {
	switch action {
	case "click", "move":
		x, _ := args["x"].(float64)
		y, _ := args["y"].(float64)
		ps := fmt.Sprintf(`[System.Windows.Forms.Cursor]::Position = [System.Drawing.Point]::new(%d,%d)`, int(x), int(y))
		_ = exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
	case "type":
		text, _ := args["text"].(string)
		ps := fmt.Sprintf(`$w = New-Object -ComObject wscript.shell; $w.SendKeys('%s')`, psSingleQuote(text))
		_ = exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
	default:
		return map[string]interface{}{"isError": true, "content": "Windows computeruse 暂不支持 " + action}, nil
	}
	return map[string]interface{}{"content": "操作已执行: " + action}, nil
}

// 确保正则在编译时生效（避免 unused）
var _ = regexp.MustCompile(`.*`)

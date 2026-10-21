package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/devicebridge/client"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
)

// ---------------------------------------------------------------------------
// Daemon 模式：后台驻留，维持 homed 连接 + 设备桥 + 消息缓冲
//
// 工作原理：
//   - daemon 保持一个到 homed 的持久连接
//   - TUI 实例通过 Unix socket 连接到 daemon
//   - daemon 为每个 TUI 客户端分配独立的 homed 响应（通过 homeMu 序列化）
//   - 新客户端连入时回放缓冲的历史消息（方便重连后看到上下文）
// ---------------------------------------------------------------------------

const (
	daemonSocketName = "waiter.sock"
	msgBufCap        = 256 // 环形缓冲最近 N 行 homed 输出
)

type msgEntry struct {
	line string
	seq  uint64
}

type daemonHandler struct {
	stopOnce sync.Once
	// homed 连接
	homeMu   sync.Mutex
	homeConn net.Conn
	homeR    *bufio.Reader
	homeCfg  *Config

	// 消息缓冲（新客户端连入时回放）
	bufMu  sync.Mutex
	buf    []msgEntry
	bufSeq uint64
	bufCap int

	// 生命周期
	stopCh chan struct{}
}

func newDaemonHandler() *daemonHandler {
	return &daemonHandler{
		bufCap: msgBufCap,
		stopCh: make(chan struct{}),
	}
}

// ===== 消息缓冲 =====

func (h *daemonHandler) appendBuf(line string) {
	h.bufMu.Lock()
	defer h.bufMu.Unlock()
	h.bufSeq++
	h.buf = append(h.buf, msgEntry{line: line, seq: h.bufSeq})
	if len(h.buf) > h.bufCap {
		h.buf = h.buf[len(h.buf)-h.bufCap:]
	}
}

func (h *daemonHandler) replayBuffer() []string {
	h.bufMu.Lock()
	defer h.bufMu.Unlock()
	lines := make([]string, 0, len(h.buf))
	for _, e := range h.buf {
		lines = append(lines, e.line)
	}
	return lines
}

// ===== homed 连接 =====

func (h *daemonHandler) connectHome(cfg *Config) error {
	h.homeCfg = cfg
	if cfg.Remote != "" {
		return fmt.Errorf("daemon: remote mode not supported")
	}
	if cfg.Socket == "" {
		cfg.Socket = discoverSocket("")
	}
	c, err := net.DialTimeout("unix", cfg.Socket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("daemon: connect home: %w", err)
	}
	h.homeConn = c
	h.homeR = bufio.NewReader(c)
	log.Printf("[daemon] connected to home %s", cfg.Socket)
	return nil
}

func (h *daemonHandler) closeHome() {
	if h.homeConn != nil {
		h.homeConn.Close()
		h.homeConn = nil
	}
}

func (h *daemonHandler) reconnectHome() {
	cfg := h.homeCfg
	if cfg == nil {
		cfg = discoverConfig("")
	}
	if cfg.Socket == "" && cfg.Remote == "" {
		cfg.Socket = discoverSocket("")
	}
	for i := 0; i < 30; i++ {
		select {
		case <-h.stopCh:
			return
		default:
		}
		h.closeHome()
		time.Sleep(2 * time.Second)
		if err := h.connectHome(cfg); err != nil {
			log.Printf("[daemon] reconnect home (%d/30): %v", i+1, err)
			continue
		}
		log.Printf("[daemon] reconnected to home")
		return
	}
	log.Printf("[daemon] gave up reconnecting to home")
}

// handleClient 处理单个 TUI 客户端：
// 1. 回放缓冲历史
// 2. 读客户端输入 → 转发到 homed
// 3. 读 homed 响应 → 回写给该客户端（独占响应，不广播）
func (h *daemonHandler) handleClient(c net.Conn) {
	defer c.Close()

	cid := fmt.Sprintf("%s", c.RemoteAddr())
	log.Printf("[daemon] client %s connected", cid)
	defer log.Printf("[daemon] client %s disconnected", cid)

	// 1) 回放缓冲（新客户端看到最近对话上下文）
	for _, line := range h.replayBuffer() {
		fmt.Fprintf(c, "%s\n", line)
	}

	// 2) 循环：读客户端 → 转发 homed → 读 homed 响应 → 回写客户端
	reader := bufio.NewReader(c)
	for {
		c.SetReadDeadline(time.Now().Add(5 * time.Minute))
		line, err := reader.ReadString('\n')
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}

		// 转发到 homed（加锁保证请求-响应配对）
		h.homeMu.Lock()
		if h.homeConn == nil {
			h.homeMu.Unlock()
			fmt.Fprintf(c, `{"type":"error","error":"not connected to home"}`+"\n")
			continue
		}
		_, sendErr := fmt.Fprintf(h.homeConn, "%s\n", line)
		if sendErr != nil {
			h.homeMu.Unlock()
			fmt.Fprintf(c, `{"type":"error","error":"send failed"}`+"\n")
			continue
		}

		// 读 homed 响应（所有帧：reasoning_delta / content_delta / tool_call / response / error）
		for {
			h.homeConn.SetReadDeadline(time.Now().Add(60 * time.Second))
			respLine, readErr := h.homeR.ReadString('\n')
			if readErr != nil {
				h.homeMu.Unlock()
				log.Printf("[daemon] home read error during client %s: %v", cid, readErr)
				h.reconnectHome()
				// 回写错误给客户端
				fmt.Fprintf(c, `{"type":"error","error":"home disconnected"}`+"\n")
				goto nextMessage
			}
			respLine = strings.TrimSuffix(respLine, "\n")
			if respLine == "" {
				continue
			}
			// 写入缓冲 + 回写给发起请求的客户端
			h.appendBuf(respLine)
			fmt.Fprintf(c, "%s\n", respLine)

			// 检查是否是终结帧
			if strings.Contains(respLine, `"type":"response"`) || strings.Contains(respLine, `"type":"error"`) {
				break
			}
		}
		h.homeMu.Unlock()

	nextMessage:
	}
}

// ===== 启动入口 =====

func runDaemon(cfg *Config) {
	dh := newDaemonHandler()

	// 连接 homed（设备桥场景下可失败——被控主机无需 homed）
	if cfg.Socket != "" || cfg.Remote != "" {
		if err := dh.connectHome(cfg); err != nil {
			log.Printf("[daemon] home connect failed: %v (continue with device bridge only)", err)
			dh.homeConn = nil
		}
	} else {
		log.Printf("[daemon] no home socket configured, running device bridge only")
	}
	defer dh.closeHome()

	// 启动设备桥（设备网关场景下为核心职责）
	startDaemonDeviceBridge(cfg)

	// 监听 Unix socket
	sockPath := daemonSocketPath()
	os.Remove(sockPath)
	os.MkdirAll(filepath.Dir(sockPath), 0755)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: listen %s: %v\n", sockPath, err)
		os.Exit(1)
	}
	defer func() {
		ln.Close()
		os.Remove(sockPath)
	}()

	log.Printf("[daemon] listening on %s", sockPath)
	fmt.Printf("waiter daemon started\n  socket: %s\n  press Ctrl+C to stop\n", sockPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("[daemon] shutting down")
		safeclose.Safe(&dh.stopOnce, dh.stopCh)
		dh.closeHome()
		ln.Close()
	}()

	// 单客户端模式：串行处理（同一时刻只有一个 TUI 连接）
	// 这与 homed CLI 插件的行为一致——一个连接对应一个活跃会话。
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-dh.stopCh:
				log.Printf("[daemon] stopped")
				return
			default:
				log.Printf("[daemon] accept error: %v", err)
				continue
			}
		}
		dh.handleClient(conn)
	}
}

// ===== Socket 工具 =====

func daemonSocketPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/tmp"
	}
	return filepath.Join(home, ".homeagent", daemonSocketName)
}

func daemonIsRunning() bool {
	sock := daemonSocketPath()
	c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func startDaemonDeviceBridge(cfg *Config) {
	dg := cfg.DeviceGateway
	dt := cfg.DeviceToken
	if dg == "" || dt == "" {
		return
	}
	// ★ 命令白名单必须在**这里**也赋值一次。
	//
	// 原因：daemon 模式（waiter --daemon，生产两台都这么跑）走的是本函数，
	// 不经过 main.go 里那处赋值。只改 main.go 的话，配置在 daemon 下**完全不生效**
	// —— 而症状是"配置写了、启动日志也打了招呼、命令照样被拒"，极难定位。
	//
	// 赋值放在 goroutine 之前：白名单在收到第一帧命令时就要就绪。
	if len(cfg.DeviceCmdAllowlist) > 0 {
		deviceCmdAllowed = buildCmdMatcher(cfg.DeviceCmdAllowlist)
		printlnC(colorGreen, fmt.Sprintf("device cmd allowlist: %d 条（来自 waiter.yaml）",
			len(cfg.DeviceCmdAllowlist)))
	} else {
		printlnC(colorGreen, fmt.Sprintf("device cmd allowlist: 默认 %d 条（waiter.yaml 未配置 device_cmd_allowlist）",
			len(defaultCmdAllowlist)))
	}

	// 设备桥重连循环：WS 断开时自动重连，并保留配置中的本地授权状态。
	go runDeviceBridgeLoop(dg, dt, cfg.DeviceAuthorized)
}

// runDeviceBridgeLoop 无限重连循环：建立设备桥 → 等待断开 → 重连。
func runDeviceBridgeLoop(gateway, token string, authorized bool) {
	for {
		bridge, err := connectDeviceBridge(gateway, token, authorized)
		if err != nil {
			log.Printf("[daemon] device bridge connect failed: %v, retrying in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("[daemon] device bridge connected, waiting...")
		bridge.Wait() // 阻塞直到断开
		log.Printf("[daemon] device bridge disconnected, reconnecting in 3s")
		time.Sleep(3 * time.Second)
	}
}

// connectDeviceBridge 创建并启动一次设备桥，返回 bridge 实例供 Wait()。
func connectDeviceBridge(gateway, token string, authorized bool) (*client.Bridge, error) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}
	deviceID := "waiter-" + sanitizeID(hostname)

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
	}

	// 确保 gateway URL 格式正确
	gw := gateway
	if !strings.HasPrefix(gw, "ws://") && !strings.HasPrefix(gw, "wss://") {
		gw = "ws://" + gw
	}
	if !strings.Contains(gw, "/api/v1/device/ws") {
		gw = gw + "/api/v1/device/ws"
	}

	bridge := client.New(gw, token, deviceID, hostname, caps, info)
	bridge.SetAuthorized(authorized)

	// 注册命令处理器：cmd_type 是主信号，同时兼容旧版 homeagent-* 文本前缀。
	cr := client.NewCmdRouter()
	cr.Handle("homeagent-", handleHomeagentCmd)
	cr.HandleDefault(handleShellCmd)
	bridge.OnCmd(func(reqID, command, cmdType string) {
		if cmdType == "homeagent" {
			handleHomeagentCmd(reqID, command)
			return
		}
		cr.Dispatch(reqID, command)
	})

	if err := bridge.Start(); err != nil {
		return nil, err
	}

	// 设置全局变量供 sendBridgeResult 使用
	deviceBridge = bridge
	deviceBridgeID = deviceID

	return bridge, nil
}

package client

import (
	"encoding/json"
	"fmt"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	"log"
	"os"
	"runtime"
	"sync"
	"time"
)

// CmdHandler 是本地命令路由回调类型。
type CmdHandler func(reqID, command string)

// BridgeCmdHandler 接收服务端明确下发的路由信号（shell 或 homeagent）。
type BridgeCmdHandler func(reqID, command, cmdType string)

// CmdResult 是命令执行结果回调（用于异步通知 GUI 层）。
type CmdResultHandler func(reqID, status, output, errMsg string)

// DataHandler 是二进制数据接收回调（如 TTS 音频）。
type DataHandler func(reqID, kind, mime string, data []byte)

// PushHandler 接收 agent **主动投递**给本设备的内容。
//
// 与 cmd 的区别：cmd 是"让设备做一件事"（请求-响应，结果要回传），
// push 是"把这段内容交给设备"（agent 经 output_send__device/<id> 发起，
// 一种单向投递）。宿主按自己的形态落地：终端打出来、音箱念出来、屏幕显示。
//
// typ: text / structured / image / file / audio（二进制走 DataHandler，不走这里）
type PushHandler func(reqID, typ, payload, meta string)

// Bridge 是设备桥客户端核心结构体。
// 管理 WebSocket 连接、消息路由、心跳保活和命令分发。
// 授权状态由设备端本地存储（客户端鉴权），服务端不存储；
// 未授权时收到 cmd 直接拒绝执行并回执 error。
type Bridge struct {
	stopOnce   sync.Once
	doneOnce   sync.Once
	mu         sync.RWMutex
	gateway    string
	token      string
	deviceID   string
	name       string
	kind       string
	caps       []string
	info       map[string]interface{}
	authorized bool // 客户端本地授权状态（用户在设备上手动开启）

	// bound 表示服务端已确认 bind（设备真正登记进网关）。
	// 与「连接已建立」是两件事：连接成功但 bind 被拒时设备是**失联**的，
	// 必须能区分（原实现完全不看 bind_ack 的 ok，失败静默）。
	mu2          sync.RWMutex
	bound        bool
	lastBind     string // 最近一次 bind 失败原因（空 = 未失败过）
	boundHandler func(bound bool, reason string)

	ws      *wsConn
	stopCh  chan struct{}
	doneCh  chan struct{}
	started bool

	// 回调
	cmdHandler    BridgeCmdHandler
	resultHandler CmdResultHandler
	dataHandler   DataHandler
	pushHandler   PushHandler

	// 二进制数据聚合（服务端→设备，如 TTS 音频）
	speechAccum *speechBuffer

	// 心跳间隔
	pingInterval time.Duration
}

// speechBuffer 聚合服务端分块推送的二进制数据。
type speechBuffer struct {
	reqID string
	kind  string
	mime  string
	total int
	data  []byte
}

// New 创建设备桥客户端。
// gateway: ws://host:port（可选 /api/v1/device/ws 路径）
// token: 接入令牌
// deviceID: 设备唯一标识
// name: 设备显示名称
// caps: 能力列表（如 ["status","cmdrun","screensee","computeruse"]）
// info: 额外设备信息（hostname, platform, arch 等），可为 nil
func New(gateway, token, deviceID, name string, caps []string, info map[string]interface{}) *Bridge {
	if info == nil {
		info = make(map[string]interface{})
	}
	// 填充默认信息
	if _, ok := info["hostname"]; !ok {
		hostname, _ := os.Hostname()
		info["hostname"] = hostname
	}
	if _, ok := info["platform"]; !ok {
		info["platform"] = runtime.GOOS
	}
	if _, ok := info["arch"]; !ok {
		info["arch"] = runtime.GOARCH
	}
	if _, ok := info["cpus"]; !ok {
		info["cpus"] = runtime.NumCPU()
	}

	return &Bridge{
		gateway:      gateway,
		token:        token,
		deviceID:     deviceID,
		name:         name,
		kind:         "computer",
		caps:         caps,
		info:         info,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		pingInterval: 30 * time.Second,
	}
}

// SetAuthorized 设置客户端本地授权状态（用户在设备上手动开启）。
// 授权后立即重新发送 hello 同步到服务端展示。
func (b *Bridge) SetAuthorized(auth bool) {
	b.mu.Lock()
	b.authorized = auth
	b.mu.Unlock()
	// 重新 hello 同步状态
	b.mu.RLock()
	ws := b.ws
	connected := ws != nil && !ws.closed
	b.mu.RUnlock()
	if connected {
		b.sendJSON(map[string]interface{}{
			"op": "hello",
			"device": map[string]interface{}{
				"device_id":  b.deviceID,
				"name":       b.name,
				"kind":       b.kind,
				"caps":       b.caps,
				"info":       b.info,
				"authorized": auth,
			},
		})
	}
}

// markBound 记录服务端已确认 bind。
func (b *Bridge) markBound() {
	b.mu2.Lock()
	b.bound = true
	b.lastBind = ""
	h := b.boundHandler
	b.mu2.Unlock()
	if h != nil {
		h(true, "")
	}
}

// markUnbound 记录 bind 失败（连接可能随即被服务端关闭）。
func (b *Bridge) markUnbound(reason string) {
	b.mu2.Lock()
	b.bound = false
	b.lastBind = reason
	h := b.boundHandler
	b.mu2.Unlock()
	if h != nil {
		h(false, reason)
	}
}

// Bound 返回服务端是否已确认 bind。
//
// 为什么要单独一个状态：连接成功 ≠ 设备可用。bind 被拒时 TCP/WS 是通的，
// 但设备没有登记进网关，命令永远下发不到 —— 只看「连接是否建立」的
// 健康检查会给出假阳性。
func (b *Bridge) Bound() bool {
	b.mu2.RLock()
	defer b.mu2.RUnlock()
	return b.bound
}

// BindError 返回最近一次 bind 失败原因（空 = 未失败）。
func (b *Bridge) BindError() string {
	b.mu2.RLock()
	defer b.mu2.RUnlock()
	return b.lastBind
}

// OnBoundState 注册绑定状态变更回调（GUI/waiter 据此提示用户）。
func (b *Bridge) OnBoundState(handler func(bound bool, reason string)) {
	b.mu2.Lock()
	b.boundHandler = handler
	b.mu2.Unlock()
}

// Authorized 返回当前客户端本地授权状态。
func (b *Bridge) Authorized() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.authorized
}

// OnCmd 注册命令处理器。当收到 remotedevice 下发的 cmd 时调用。
func (b *Bridge) OnCmd(handler BridgeCmdHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cmdHandler = handler
}

// OnResult 注册命令结果回调（用于异步通知）。
func (b *Bridge) OnResult(handler CmdResultHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resultHandler = handler
}

// OnData 注册二进制数据接收回调（如 TTS 音频）。
func (b *Bridge) OnData(handler DataHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dataHandler = handler
}

// OnPush 注册 agent 主动投递内容的回调（服务端 op=push）。
func (b *Bridge) OnPush(handler PushHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pushHandler = handler
}

// SetPingInterval 设置心跳间隔（默认 30 秒）。
func (b *Bridge) SetPingInterval(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pingInterval = d
}

// Start 启动设备桥连接。
// 会阻塞直到连接建立或超时失败。
func (b *Bridge) Start() error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return fmt.Errorf("devicebridge: already started")
	}
	b.started = true
	b.mu.Unlock()

	ws, err := dialWS(b.gateway, b.token, 10*time.Second)
	if err != nil {
		b.mu.Lock()
		b.started = false
		b.mu.Unlock()
		return fmt.Errorf("devicebridge: dial: %w", err)
	}

	b.mu.Lock()
	b.ws = ws
	b.mu.Unlock()

	// 发送 hello（含设备自报的授权状态，服务端仅展示不决策）
	b.mu.RLock()
	auth := b.authorized
	b.mu.RUnlock()
	b.sendJSON(map[string]interface{}{
		"op": "hello",
		"device": map[string]interface{}{
			"device_id":  b.deviceID,
			"name":       b.name,
			"kind":       b.kind,
			"caps":       b.caps,
			"info":       b.info,
			"authorized": auth,
		},
	})

	// 发送 bind
	b.sendJSON(map[string]interface{}{
		"op":        "bind",
		"device_id": b.deviceID,
		"token":     b.token,
	})

	go b.readLoop()
	go b.pingLoop()

	return nil
}

// Stop 停止设备桥连接。
func (b *Bridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		return
	}
	// 幂等（本仓 close 纪律）。原写法是
	//   select { case <-b.stopCh: return; default: close(b.stopCh) }
	// 单线程安全，但**并发**下两个 goroutine 可同时落到 default ⇒ 双 close panic。
	if !safeclose.Safe(&b.stopOnce, b.stopCh) {
		return
	}
	if b.ws != nil {
		_ = b.ws.close()
		b.ws = nil
	}
}

// Wait 等待设备桥连接关闭。
func (b *Bridge) Wait() {
	<-b.doneCh
}

// DeviceID 返回设备 ID。
func (b *Bridge) DeviceID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.deviceID
}

// Connected 返回是否已连接。
func (b *Bridge) Connected() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ws != nil && !b.ws.closed
}

// ===== 发送消息 =====

// SendResult 发送命令执行结果。
func (b *Bridge) SendResult(reqID, status, output, errMsg string) {
	msg := map[string]interface{}{
		"op":        "cmd_result",
		"req_id":    reqID,
		"status":    status,
		"device_id": b.deviceID,
	}
	if output != "" {
		msg["output"] = output
	}
	if errMsg != "" {
		msg["error"] = errMsg
	}
	b.sendJSON(msg)
}

// SendDataStart 开始二进制数据传输（设备→网关，如录像回传）。
func (b *Bridge) SendDataStart(reqID, kind, mime string, total int) {
	b.sendJSON(map[string]interface{}{
		"op":         "cmd_data_start",
		"req_id":     reqID,
		"kind":       kind,
		"mime":       mime,
		"total":      total,
		"chunk_size": 8192,
	})
}

// SendDataChunk 发送一块二进制数据。
func (b *Bridge) SendDataChunk(data []byte) error {
	b.mu.RLock()
	ws := b.ws
	b.mu.RUnlock()
	if ws == nil || ws.closed {
		return fmt.Errorf("devicebridge: not connected")
	}
	return ws.writeBinary(data)
}

// SendDataEnd 结束二进制数据传输。
func (b *Bridge) SendDataEnd(reqID, status, errMsg string) {
	msg := map[string]interface{}{
		"op":     "cmd_data_end",
		"req_id": reqID,
		"status": status,
	}
	if errMsg != "" {
		msg["error"] = errMsg
	}
	b.sendJSON(msg)
}

// SendDataChunked 便捷方法：自动分块发送完整二进制数据。
func (b *Bridge) SendDataChunked(reqID, kind, mime string, data []byte) {
	total := len(data)
	b.SendDataStart(reqID, kind, mime, total)
	const chunkSize = 8192
	for off := 0; off < total; off += chunkSize {
		end := off + chunkSize
		if end > total {
			end = total
		}
		if err := b.SendDataChunk(data[off:end]); err != nil {
			b.SendDataEnd(reqID, "error", err.Error())
			return
		}
	}
	b.SendDataEnd(reqID, "ok", "")
}

// SendEvent 发送设备主动上报事件。
func (b *Bridge) SendEvent(eventType string, payload interface{}) {
	b.sendJSON(map[string]interface{}{
		"op":        "event",
		"device_id": b.deviceID,
		"type":      eventType,
		"payload":   payload,
	})
}

// SendStatus 发送设备状态更新。
func (b *Bridge) SendStatus(status string) {
	b.sendJSON(map[string]interface{}{
		"op":        "status",
		"device_id": b.deviceID,
		"status":    status,
	})
}

// ===== 内部方法 =====

func (b *Bridge) sendJSON(v interface{}) {
	b.mu.RLock()
	ws := b.ws
	b.mu.RUnlock()
	if ws == nil || ws.closed {
		return
	}
	payload := mustJSON(v)
	_ = ws.writeText(payload)
}

func (b *Bridge) readLoop() {
	defer func() {
		b.mu.Lock()
		b.started = false
		if b.ws != nil {
			_ = b.ws.close()
			b.ws = nil
		}
		b.mu.Unlock()
		safeclose.Safe(&b.doneOnce, b.doneCh)
	}()

	for {
		select {
		case <-b.stopCh:
			return
		default:
		}

		// 设置读超时（2 倍 ping 间隔）
		b.mu.RLock()
		ws := b.ws
		interval := b.pingInterval
		b.mu.RUnlock()
		if ws == nil {
			return
		}

		ws.setDeadline(time.Now().Add(interval * 2))
		payload, isClose, opcode, err := ws.readFrame()
		if err != nil {
			if err == errPing {
				_ = ws.writePong()
				continue
			}
			// 超时或其他错误，退出。
			//
			// **必须记日志**：此前这里静默 return，设备断线的真因（读超时 / 对端
			// 关闭 / 帧错）在设备侧完全不可见，只能靠对端日志倒推。
			// 2 倍 ping 间隔内的读超时通常是“心跳没人回”——查服务端 writePong 是否真发出。
			log.Printf("[devicebridge] read loop exit (opcode=%#x, close=%v): %v", opcode, isClose, err)
			return
		}
		if isClose {
			return
		}
		if opcode == 0x2 {
			// 二进制帧：处于聚合状态时追加
			b.handleBinaryFrame(payload)
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		b.handleMessage(msg)
	}
}

func (b *Bridge) handleMessage(msg map[string]interface{}) {
	op, _ := msg["op"].(string)
	switch op {
	case "cmd":
		reqID, _ := msg["req_id"].(string)
		command, _ := msg["command"].(string)
		cmdType, _ := msg["cmd_type"].(string)
		if reqID == "" || command == "" {
			return
		}
		// 客户端鉴权：未授权时拒绝执行（服务端不存储授权状态，无法被 agent 篡改）
		b.mu.RLock()
		auth := b.authorized
		handler := b.cmdHandler
		b.mu.RUnlock()
		if !auth {
			log.Printf("[devicebridge] cmd rejected (unauthorized) req=%s cmd=%s", reqID, truncateString(command, 60))
			b.SendResult(reqID, "error", "", "设备未授权：请在设备本机开启远程控制授权")
			return
		}
		// 记录日志
		log.Printf("[devicebridge] cmd req=%s type=%s cmd=%s", reqID, cmdType, truncateString(command, 60))

		if handler != nil {
			handler(reqID, command, cmdType)
		}

	case "hello_ack", "bind_ack":
		// ★ 必须分别处理，且要判失败。
		//
		// 原实现只打一行 `device=%v`，而服务端**成功**时回的是
		// {"op":"bind_ack","ok":true} —— 根本没有 device 字段，于是日志
		// 永远显示 `bind_ack device=<nil>`。这会让人误判成「绑定失败」，
		// 而实际上连接是好的（实测：同一现象经直连与经反代完全一致）。
		//
		// 更严重的是失败无人处理：服务端 bind 被拒时回
		// {"op":"bind_ack","ok":false,"error":"bind rejected"} 并**关闭连接**，
		// 客户端却既不报错也不重连，设备静默失联。
		if op == "bind_ack" {
			if ok, _ := msg["ok"].(bool); !ok {
				reason, _ := msg["error"].(string)
				if reason == "" {
					reason = "bind rejected"
				}
				log.Printf("[devicebridge] bind 被拒：%s（令牌不匹配或设备未授权）", reason)
				b.markUnbound(reason)
				return
			}
			log.Printf("[devicebridge] bind 成功，设备已登记")
			b.markBound()
			return
		}
		dev, _ := msg["device"].(string)
		online, _ := msg["online"].(bool)
		log.Printf("[devicebridge] hello_ack device=%s online=%v", dev, online)

	case "cmd_speech_start":
		reqID, _ := msg["req_id"].(string)
		kind, _ := msg["kind"].(string)
		mime, _ := msg["mime"].(string)
		total := 0
		if v, ok := msg["total"].(float64); ok {
			total = int(v)
		}
		b.mu.Lock()
		b.speechAccum = &speechBuffer{
			reqID: reqID,
			kind:  kind,
			mime:  mime,
			total: total,
		}
		b.mu.Unlock()

	case "cmd_speech_end":
		reqID, _ := msg["req_id"].(string)
		b.mu.Lock()
		acc := b.speechAccum
		b.speechAccum = nil
		b.mu.Unlock()
		if acc == nil || acc.reqID != reqID {
			return
		}
		data := acc.data
		b.mu.RLock()
		dh := b.dataHandler
		b.mu.RUnlock()
		if dh != nil {
			dh(reqID, acc.kind, acc.mime, data)
		}

	case "push":
		// agent 主动投递（output_send__device/<id>）。二进制负载走
		// cmd_speech_* → DataHandler，这里只处理文本/结构化。
		reqID, _ := msg["req_id"].(string)
		typ, _ := msg["type"].(string)
		payload, _ := msg["payload"].(string)
		meta, _ := msg["meta"].(string)
		if typ == "" {
			typ = "text"
		}
		b.mu.RLock()
		ph := b.pushHandler
		b.mu.RUnlock()
		if ph != nil {
			ph(reqID, typ, payload, meta)
		} else {
			log.Printf("[devicebridge] push req=%s type=%s payload=%s", reqID, typ, truncateString(payload, 120))
		}

	default:
		log.Printf("[devicebridge] unhandled op=%s", op)
	}
}

func (b *Bridge) handleBinaryFrame(payload []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.speechAccum == nil {
		return
	}
	b.speechAccum.data = append(b.speechAccum.data, payload...)
	// 防滥用：超出声明 total 的 2 倍或硬上限 64MB 时放弃
	limit := b.speechAccum.total*2 + 1024
	if limit < 64<<20 {
		limit = 64 << 20
	}
	if len(b.speechAccum.data) > limit {
		log.Printf("[devicebridge] speech data exceeded limit, dropped")
		b.speechAccum = nil
	}
}

func (b *Bridge) pingLoop() {
	b.mu.RLock()
	interval := b.pingInterval
	b.mu.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopCh:
			return
		case <-ticker.C:
			b.mu.RLock()
			ws := b.ws
			b.mu.RUnlock()
			if ws != nil && !ws.closed {
				_ = ws.writeFrame(0x9, nil) // ping
			}
		}
	}
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

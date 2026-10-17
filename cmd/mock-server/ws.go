package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/devicebridge/client"
)

// ===== WebSocket 帧编码/解码（RFC 6455） =====

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type WSConn struct {
	conn net.Conn
	rw   *bufio.ReadWriter
	mu   sync.Mutex
}

func upgradeWS(w http.ResponseWriter, r *http.Request) (*WSConn, error) {
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "not websocket", 400)
		return nil, fmt.Errorf("not websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing key", 400)
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}

	h := sha256.Sum256([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h[:])

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", 500)
		return nil, fmt.Errorf("hijack not supported")
	}

	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "hijack failed", 500)
		return nil, err
	}

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := bufrw.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := bufrw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	return &WSConn{conn: conn, rw: bufrw}, nil
}

func (ws *WSConn) ReadFrame() (opcode byte, payload []byte, err error) {
	for {
		b0, err := ws.rw.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		opcode = b0 & 0x0F

		b1, err := ws.rw.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		masked := b1&0x80 != 0
		length := int64(b1 & 0x7F)

		switch {
		case length == 126:
			var b [2]byte
			if _, err := io.ReadFull(ws.rw, b[:]); err != nil {
				return 0, nil, err
			}
			length = int64(binary.BigEndian.Uint16(b[:]))
		case length == 127:
			var b [8]byte
			if _, err := io.ReadFull(ws.rw, b[:]); err != nil {
				return 0, nil, err
			}
			length = int64(binary.BigEndian.Uint64(b[:]))
		}

		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(ws.rw, maskKey[:]); err != nil {
				return 0, nil, err
			}
		}

		payload = make([]byte, length)
		if _, err := io.ReadFull(ws.rw, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}

		if opcode == 0x8 { // Close
			ws.sendFrame(0x8, nil, false)
			return opcode, payload, fmt.Errorf("ws closed")
		}
		if opcode == 0x9 { // Ping
			ws.sendFrame(0xA, payload, false) // Pong
			continue
		}
		if opcode == 0xA { // Pong
			continue
		}

		return opcode, payload, nil
	}
}

func (ws *WSConn) sendFrame(opcode byte, payload []byte, masked bool) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	buf := []byte{0x80 | opcode} // FIN + opcode
	length := len(payload)

	switch {
	case length <= 125:
		if masked {
			buf = append(buf, byte(length)|0x80)
		} else {
			buf = append(buf, byte(length))
		}
	case length <= 65535:
		if masked {
			buf = append(buf, 126|0x80)
		} else {
			buf = append(buf, 126)
		}
		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, uint16(length))
		buf = append(buf, b...)
	default:
		if masked {
			buf = append(buf, 127|0x80)
		} else {
			buf = append(buf, 127)
		}
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(length))
		buf = append(buf, b...)
	}

	var maskKey [4]byte
	if masked {
		rand.Read(maskKey[:])
		buf = append(buf, maskKey[:]...)
		maskedPayload := make([]byte, length)
		copy(maskedPayload, payload)
		for i := range maskedPayload {
			maskedPayload[i] ^= maskKey[i%4]
		}
		buf = append(buf, maskedPayload...)
	} else {
		buf = append(buf, payload...)
	}

	_, err := ws.conn.Write(buf)
	return err
}

func (ws *WSConn) WriteJSON(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.sendFrame(0x1, b, false) // Text frame
}

func (ws *WSConn) ReadJSON(v interface{}) error {
	_, payload, err := ws.ReadFrame()
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}

func (ws *WSConn) Close() {
	ws.sendFrame(0x8, nil, false)
	ws.conn.Close()
}

// ===== Mock Remotedevice 设备桥 =====

type MockDevice struct {
	DeviceID string
	Name     string
	Caps     []string
	Conn     *WSConn
	Online   bool
}

type MockRemoteDevice struct {
	mu      sync.Mutex
	devices map[string]*MockDevice
}

func NewMockRemoteDevice() *MockRemoteDevice {
	return &MockRemoteDevice{
		devices: make(map[string]*MockDevice),
	}
}

func (s *MockServer) handleDeviceWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgradeWS(w, r)
	if err != nil {
		log.Printf("[ws] upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	log.Printf("[ws] 新设备连接")

	// 处理 hello/bind/cmd 协议
	var device *MockDevice
	for {
		var msg struct {
			Op       string          `json:"op"`
			DeviceID string          `json:"device_id"`
			Token    string          `json:"token"`
			ReqID    string          `json:"req_id"`
			Command  string          `json:"command"`
			CmdType  string          `json:"cmd_type"`
			Status   string          `json:"status"`
			Output   string          `json:"output"`
			Error    string          `json:"error"`
			Device   json.RawMessage `json:"device"`
			Payload  json.RawMessage `json:"payload"`
		}

		if err := ws.ReadJSON(&msg); err != nil {
			log.Printf("[ws] read error: %v", err)
			break
		}

		switch msg.Op {
		case "hello":
			var devMeta struct {
				DeviceID string   `json:"device_id"`
				Name     string   `json:"name"`
				Kind     string   `json:"kind"`
				Caps     []string `json:"caps"`
			}
			json.Unmarshal(msg.Device, &devMeta)

			device = &MockDevice{
				DeviceID: devMeta.DeviceID,
				Name:     devMeta.Name,
				Caps:     devMeta.Caps,
				Conn:     ws,
				Online:   true,
			}

			s.mu.Lock()
			// 更新设备列表
			found := false
			for i := range s.devices {
				if s.devices[i].DeviceID == devMeta.DeviceID {
					s.devices[i].Online = true
					s.devices[i].Caps = devMeta.Caps
					found = true
					break
				}
			}
			if !found {
				s.devices = append(s.devices, Device{
					DeviceID:   devMeta.DeviceID,
					Name:       devMeta.Name,
					Authorized: false,
					Online:     true,
					Caps:       devMeta.Caps,
				})
			}
			s.mu.Unlock()

			ws.WriteJSON(map[string]interface{}{
				"op":   "hello_ack",
				"code": 0,
			})
			log.Printf("[ws] 设备登记: %s (%s) caps=%v", devMeta.DeviceID, devMeta.Name, devMeta.Caps)

		case "bind":
			if msg.DeviceID == "" || msg.Token == "" {
				ws.WriteJSON(map[string]interface{}{
					"op":    "bind_ack",
					"code":  1,
					"error": "missing device_id or token",
				})
				break
			}

			s.mu.Lock()
			for i := range s.devices {
				if s.devices[i].DeviceID == msg.DeviceID {
					s.devices[i].Authorized = true
				}
			}
			s.mu.Unlock()

			ws.WriteJSON(map[string]interface{}{
				"op":   "bind_ack",
				"code": 0,
			})
			log.Printf("[ws] 设备授权: %s", msg.DeviceID)

			// 绑定成功后，发送全量能力测试命令序列
			go func() {
				time.Sleep(500 * time.Millisecond)

				// 测试 1: screensee 截图
				log.Printf("[ws] 发命令 1/7: homeagent-screensee")
				ws.WriteJSON(client.CmdMsg{
					Op:      "cmd",
					ReqID:   "test_1_screensee_" + fmt.Sprintf("%d", time.Now().UnixMilli()),
					Command: "homeagent-screensee",
					CmdType: "homeagent",
				})

				time.Sleep(800 * time.Millisecond)

				// 测试 2: clipboardsue 写入剪贴板
				log.Printf("[ws] 发命令 2/7: homeagent-clipboardsue")
				ws.WriteJSON(client.CmdMsg{
					Op:      "cmd",
					ReqID:   "test_2_clipboardsue_" + fmt.Sprintf("%d", time.Now().UnixMilli()),
					Command: "homeagent-clipboardsue HomeAgent远程测试_" + fmt.Sprintf("%d", time.Now().UnixMilli()),
					CmdType: "homeagent",
				})

				time.Sleep(800 * time.Millisecond)

				// 测试 3: clipboardsee 读取剪贴板
				log.Printf("[ws] 发命令 3/7: homeagent-clipboardsee")
				ws.WriteJSON(client.CmdMsg{
					Op:      "cmd",
					ReqID:   "test_3_clipboardsee_" + fmt.Sprintf("%d", time.Now().UnixMilli()),
					Command: "homeagent-clipboardsee",
					CmdType: "homeagent",
				})

				time.Sleep(800 * time.Millisecond)

				// 测试 4: computeruse 鼠标移动（使用非标准 JSON 格式测试兼容性）
				log.Printf("[ws] 发命令 4/7: homeagent-computeruse move")
				ws.WriteJSON(client.CmdMsg{
					Op:      "cmd",
					ReqID:   "test_4_computeruse_" + fmt.Sprintf("%d", time.Now().UnixMilli()),
					Command: `homeagent-computeruse move {x:500,y:300}`,
					CmdType: "homeagent",
				})

				time.Sleep(800 * time.Millisecond)

				// 测试 4b: computeruse 鼠标点击（标准 JSON 格式）
				log.Printf("[ws] 发命令 4b/7: homeagent-computeruse click")
				ws.WriteJSON(client.CmdMsg{
					Op:      "cmd",
					ReqID:   "test_4b_click_" + fmt.Sprintf("%d", time.Now().UnixMilli()),
					Command: `homeagent-computeruse {"x":800,"y":500,"action":"click","button":"left"}`,
					CmdType: "homeagent",
				})

				time.Sleep(800 * time.Millisecond)

				// 测试 5: speakeruse TTS 播报
				log.Printf("[ws] 发命令 5/7: homeagent-speakeruse")
				ws.WriteJSON(client.CmdMsg{
					Op:      "cmd",
					ReqID:   "test_5_speakeruse_" + fmt.Sprintf("%d", time.Now().UnixMilli()),
					Command: "homeagent-speakeruse 你好，这是来自远程Mock服务器的测试播报",
					CmdType: "homeagent",
				})

				time.Sleep(800 * time.Millisecond)

				// 测试 6: screensue 弹窗显示
				log.Printf("[ws] 发命令 6/7: homeagent-screensue（HTML）")
				ws.WriteJSON(client.CmdMsg{
					Op:      "cmd",
					ReqID:   "test_6_screensue_" + fmt.Sprintf("%d", time.Now().UnixMilli()),
					Command: `homeagent-screensue 10 <!DOCTYPE html><html><head><meta charset="utf-8"><style>body{background:linear-gradient(135deg,#667eea,#764ba2);color:white;font-family:sans-serif;padding:30px;margin:0}h1{font-size:32px;text-shadow:0 2px 8px rgba(0,0,0,0.3)}.card{background:rgba(255,255,255,0.15);border-radius:12px;padding:20px;margin:12px 0;backdrop-filter:blur(8px)}.badge{display:inline-block;background:#4ade80;color:#000;padding:3px 10px;border-radius:16px;font-weight:bold}</style></head><body><h1>HomeAgent GUI 全量测试</h1><div class="card"><h2>能力测试结果</h2><table border="1" cellpadding="6" style="border-collapse:collapse;width:100%"><tr><th>能力</th><th>结果</th></tr><tr><td>screensee 截图</td><td><span class="badge">通过</span></td></tr><tr><td>clipboard 读写</td><td><span class="badge">通过</span></td></tr><tr><td>computeruse 操控</td><td><span class="badge">通过</span></td></tr><tr><td>speakeruse TTS</td><td><span class="badge">通过</span></td></tr><tr><td>screensue 渲染</td><td><span class="badge">通过</span></td></tr></table></div><p style="text-align:center;color:rgba(255,255,255,0.7)">2026-08-23 19:50</p></body></html>`,
					CmdType: "homeagent",
				})

				time.Sleep(800 * time.Millisecond)

				// 测试 7: omniparse 解析当前窗口 UI 元素
				log.Printf("[ws] 发命令 7/7: homeagent-omniparse")
				ws.WriteJSON(client.CmdMsg{
					Op:      "cmd",
					ReqID:   "test_7_omniparse_" + fmt.Sprintf("%d", time.Now().UnixMilli()),
					Command: "homeagent-omniparse",
					CmdType: "homeagent",
				})
			}()

		case "cmd_result":
			log.Printf("[ws] 命令结果: req=%s status=%s", msg.ReqID, msg.Status)
			if msg.Output != "" {
				output := msg.Output
				if len(output) > 100 {
					output = output[:100] + "..."
				}
				log.Printf("[ws] 输出: %s", output)
			}
			if msg.Error != "" {
				log.Printf("[ws] 错误: %s", msg.Error)
			}

		case "data_start":
			var ds client.DataStart
			json.Unmarshal(msg.Payload, &ds)
			log.Printf("[ws] 二进制数据开始: req=%s kind=%s mime=%s total=%d", ds.ReqID, ds.Kind, ds.MIME, ds.Total)

		case "data_end":
			log.Printf("[ws] 二进制数据结束: req=%s status=%s", msg.ReqID, msg.Status)

		case "speech_start":
			log.Printf("[ws] TTS 音频开始: req=%s", msg.ReqID)

		case "speech_end":
			log.Printf("[ws] TTS 音频结束: req=%s", msg.ReqID)

		case "status":
			log.Printf("[ws] 状态上报: %s -> %s", msg.DeviceID, msg.Status)

		case "event":
			log.Printf("[ws] 事件上报: %s type=%s", msg.DeviceID, string(msg.Payload))

		default:
			log.Printf("[ws] 未知消息类型: %s", msg.Op)
		}
	}

	if device != nil {
		device.Online = false
		s.mu.Lock()
		for i := range s.devices {
			if s.devices[i].DeviceID == device.DeviceID {
				s.devices[i].Online = false
			}
		}
		s.mu.Unlock()
	}
	log.Printf("[ws] 设备断开")
}

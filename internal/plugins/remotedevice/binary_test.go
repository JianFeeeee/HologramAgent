package remotedevice

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ===== 测试用最小 WS 客户端（模拟 GUI 设备桥）=====

type testWSClient struct {
	conn net.Conn
	rw   *bufio.ReadWriter
}

func dialTestWS(t *testing.T, url, token string) *testWSClient {
	t.Helper()
	req := "GET /api/v1/device/ws?token=" + token + " HTTP/1.1\r\n" +
		"Host: test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := readLine(br)
	if err != nil {
		t.Fatalf("read upgrade resp: %v", err)
	}
	if !strings.Contains(resp, "101") {
		t.Fatalf("expected 101, got %s", resp)
	}
	for {
		line, err := readLine(br)
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" || line == "" {
			break
		}
	}
	return &testWSClient{conn: conn, rw: &bufio.ReadWriter{Reader: br, Writer: bufio.NewWriter(conn)}}
}

func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		b, err := br.ReadByte()
		if err != nil {
			return sb.String(), err
		}
		sb.WriteByte(b)
		if b == '\n' {
			return sb.String(), nil
		}
	}
}

// sendText 发送客户端文本帧（带掩码，RFC6455 要求客户端帧必须掩码）
func (c *testWSClient) sendText(payload []byte) {
	c.sendFrame(0x1, payload)
}

func (c *testWSClient) sendBinary(payload []byte) {
	c.sendFrame(0x2, payload)
}

func (c *testWSClient) sendFrame(opcode byte, payload []byte) {
	maskKey := make([]byte, 4)
	rand.Read(maskKey)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ maskKey[i%4]
	}
	var hdr []byte
	hdr = append(hdr, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 0x80|126)
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(n))
		hdr = append(hdr, ext...)
	default:
		hdr = append(hdr, 0x80|127)
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		hdr = append(hdr, ext...)
	}
	c.rw.Write(hdr)
	c.rw.Write(maskKey)
	c.rw.Write(masked)
	c.rw.Flush()
}

// readMsg 读一帧（跳过 pong），返回 opcode 与 payload
func (c *testWSClient) readMsg() (byte, []byte, error) {
	for {
		payload, isClose, opcode, err := readFrame(c.rw.Reader)
		if err != nil || isClose {
			return 0, nil, err
		}
		if opcode == 0xa {
			continue
		}
		return opcode, payload, nil
	}
}

func (c *testWSClient) close() { c.conn.Close() }

func (c *testWSClient) bindDevice(t *testing.T, deviceID, token string) {
	t.Helper()
	c.sendText(mustJSON(map[string]interface{}{
		"op": "bind", "device_id": deviceID, "token": token,
	}))
	op, payload, err := c.readMsg()
	if err != nil {
		t.Fatalf("read bind_ack: %v", err)
	}
	if op != 0x1 {
		t.Fatalf("expected bind_ack text frame, got %x", op)
	}
	var ack map[string]interface{}
	if err := json.Unmarshal(payload, &ack); err != nil {
		t.Fatalf("decode bind_ack: %v", err)
	}
	if ack["op"] != "bind_ack" || ack["ok"] != true {
		t.Fatalf("bind rejected: %v", ack)
	}
}

func (c *testWSClient) readHelloAck(t *testing.T) string {
	t.Helper()
	op, payload, err := c.readMsg()
	if err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	if op != 0x1 {
		t.Fatalf("expected hello_ack text frame, got %x", op)
	}
	var ack map[string]interface{}
	if err := json.Unmarshal(payload, &ack); err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	deviceID, _ := ack["device"].(string)
	if ack["op"] != "hello_ack" || deviceID == "" {
		t.Fatalf("expected hello_ack with device id, got %v", ack)
	}
	return deviceID
}

func (c *testWSClient) readHelloAckAndBind(t *testing.T, token string) {
	t.Helper()
	deviceID := c.readHelloAck(t)
	c.bindDevice(t, deviceID, token)
}

// ===== 端到端：hello/bind/cmd + 二进制分块回传（录像协议）=====

func TestWSBinaryChunkUpload(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-123"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()
	url := srv.URL

	cli := dialTestWS(t, url, token)
	defer cli.close()

	// hello 后必须完成 bind，设备才会注册并开始处理数据。
	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"gui-test","name":"测试机","kind":"computer","caps":["cmd"]}}`))
	cli.readHelloAckAndBind(t, token)

	// 模拟设备收到 cmd 后以二进制分块回传（cmd_data_start → 0x2×N → cmd_data_end）
	videoData := make([]byte, 20000) // 跨多个 8KB 块
	for i := range videoData {
		videoData[i] = byte(i % 251)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		cli.sendText(mustJSON(map[string]interface{}{
			"op": "cmd_data_start", "req_id": "req-video-1",
			"kind": "camera_video", "mime": "video/mp4",
			"total": len(videoData), "chunk_size": 8192,
		}))
		const chunk = 8192
		for off := 0; off < len(videoData); off += chunk {
			end := off + chunk
			if end > len(videoData) {
				end = len(videoData)
			}
			cli.sendBinary(videoData[off:end])
		}
		cli.sendText(mustJSON(map[string]interface{}{
			"op": "cmd_data_end", "req_id": "req-video-1", "status": "ok", "total": len(videoData),
		}))
	}()

	// 服务端等待聚合结果（AwaitResult 由 deliverResult 唤醒）
	res, err := reg.AwaitResult("req-video-1", 5*time.Second)
	if err != nil {
		t.Fatalf("await aggregated result: %v", err)
	}
	if res["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", res["status"])
	}
	if got, _ := res["size"].(int); got != len(videoData) {
		t.Fatalf("size mismatch: got %v want %d", res["size"], len(videoData))
	}

	// 校验 base64 数据完整性
	stored, ok := reg.GetResult("req-video-1")
	if !ok {
		t.Fatal("result not persisted")
	}
	b64, _ := stored["data_base64"].(string)
	if len(b64) == 0 {
		t.Fatal("data_base64 empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if string(decoded) != string(videoData) {
		t.Fatal("decoded data mismatch")
	}
}

// ===== 媒体落盘：SetMediaDir 后 cmd_result 返回 file 路径而非 base64 内联 =====
func TestWSBinaryMediaToFile(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-123"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })
	mediaDir := t.TempDir()
	reg.SetMediaDir(mediaDir)

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"gui-media","name":"媒体机","kind":"computer","caps":["cmd"]}}`))
	cli.readHelloAckAndBind(t, token)

	videoData := make([]byte, 30000)
	for i := range videoData {
		videoData[i] = byte(i % 253)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cli.sendText(mustJSON(map[string]interface{}{
			"op": "cmd_data_start", "req_id": "req-file-1",
			"kind": "camera_video", "mime": "video/mp4",
			"total": len(videoData),
		}))
		const chunk = 8192
		for off := 0; off < len(videoData); off += chunk {
			end := off + chunk
			if end > len(videoData) {
				end = len(videoData)
			}
			cli.sendBinary(videoData[off:end])
		}
		cli.sendText(mustJSON(map[string]interface{}{
			"op": "cmd_data_end", "req_id": "req-file-1", "status": "ok",
		}))
	}()

	res, err := reg.AwaitResult("req-file-1", 5*time.Second)
	if err != nil {
		t.Fatalf("await result: %v", err)
	}
	// 落盘模式：file 字段存在且内容一致；不应再有 data_base64
	fp, ok := res["file"].(string)
	if !ok || fp == "" {
		t.Fatalf("expected file path in result, got %v", res)
	}
	if _, hasB64 := res["data_base64"]; hasB64 {
		t.Fatal("data_base64 should be absent in file mode")
	}
	// ★ 2026-10-06：文件名含内容摘要（SaveInlineMedia）。
	//   截屏/录像这类会被反复抓取的媒体，用 reqID 命名会把同内容存成多份、
	//   不同内容却可能覆盖 —— 摘要后缀两边都避免。
	if filepath.Dir(fp) != mediaDir {
		t.Fatalf("file 应落在媒体目录 %s，实际 %s", mediaDir, fp)
	}
	base := filepath.Base(fp)
	if !strings.HasPrefix(base, "req-file-1_") || !strings.HasSuffix(base, ".mp4") {
		t.Fatalf("文件名应为 req-file-1_<digest>.mp4，实际 %s", base)
	}
	got, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read media file: %v", err)
	}
	if string(got) != string(videoData) {
		t.Fatal("media file content mismatch")
	}
}

// ===== 端到端：PushData 下发音频（网关→设备 cmd_speech 协议）=====

func TestWSPushDataAudio(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-456"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"audio-dev","name":"音频机","kind":"speaker"}}`))
	cli.readHelloAckAndBind(t, token)

	audioData := []byte("RIFF....fake-wav-audio-data-for-testing....")

	// 异步下发音频
	errCh := make(chan error, 1)
	go func() {
		errCh <- reg.PushData("audio-dev", "req-speech-1", "speech", "audio/wav", audioData)
	}()

	// 设备侧按协议读取：start 文本帧 → N 个二进制帧 → end 文本帧
	var start map[string]interface{}
	var chunks [][]byte
	var end map[string]interface{}
	deadline := time.After(5 * time.Second)
	for end == nil {
		select {
		case <-deadline:
			t.Fatal("timeout reading speech protocol frames")
		default:
		}
		op, payload, err := cli.readMsg()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch op {
		case 0x1:
			var msg map[string]interface{}
			json.Unmarshal(payload, &msg)
			switch msg["op"] {
			case "cmd_speech_start":
				start = msg
			case "cmd_speech_end":
				end = msg
			}
		case 0x2:
			chunks = append(chunks, payload)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("PushData error: %v", err)
	}

	if start == nil || start["op"] != "cmd_speech_start" {
		t.Fatal("missing cmd_speech_start")
	}
	if start["mime"] != "audio/wav" || start["kind"] != "speech" {
		t.Fatalf("unexpected start fields: %v", start)
	}
	if int(start["total"].(float64)) != len(audioData) {
		t.Fatalf("total mismatch: %v", start["total"])
	}
	if end["req_id"] != "req-speech-1" {
		t.Fatalf("unexpected end: %v", end)
	}
	var got []byte
	for _, c := range chunks {
		got = append(got, c...)
	}
	if string(got) != string(audioData) {
		t.Fatalf("audio data mismatch: got %d bytes want %d", len(got), len(audioData))
	}
}

// ===== PushData 对离线设备报错 =====

func TestPushDataOfflineDevice(t *testing.T) {
	reg := NewRegistry()
	err := reg.PushData("no-such-device", "req-x", "speech", "audio/wav", []byte{1})
	if err == nil || !strings.Contains(err.Error(), "not online") {
		t.Fatalf("expected not online error, got %v", err)
	}
}

// ===== screensee：截屏回传 + 视觉描述回调 =====

func TestScreenseeEndToEnd(t *testing.T) {
	reg := NewRegistry()
	reg.SetMediaDir(t.TempDir())
	token := "test-token-see"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	dev := &devicectlDevice{reg: reg}
	var gotDataURL string
	dev.SetSeeHandler(func(dataURL string, provider string) (string, error) {
		gotDataURL = dataURL
		return "屏幕上显示的是测试画面", nil
	})

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	// 设备 hello + bind（bind 需 token 才能被授权流程识别，这里直接手动授权）
	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"see-dev","name":"屏幕机","kind":"computer","caps":["cmd"]}}`))
	cli.readHelloAckAndBind(t, token)

	// 设备侧循环收命令并回执（模拟 GUI screensee 实现）
	go func() {
		for {
			op, payload, err := cli.readMsg()
			if err != nil {
				return
			}
			if op != 0x1 {
				continue
			}
			var msg map[string]interface{}
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			if msg["op"] == "cmd" && msg["command"] == "screensee" {
				reqID, _ := msg["req_id"].(string)
				fakeJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10} // JPEG magic
				b64 := base64.StdEncoding.EncodeToString(fakeJPEG)
				cli.sendText(mustJSON(map[string]interface{}{
					"op": "cmd_result", "req_id": reqID, "device_id": "see-dev",
					"status": "ok", "output": "data:image/jpeg;base64," + b64,
				}))
			}
		}
	}()

	// agent 调用 screensee
	// ★ 2026-10-06：默认走落盘 + 回路径；旧的自动描述路径用 auto_describe=true。
	res, err := dev.Execute("screensee", map[string]interface{}{
		"device_id": "see-dev", "auto_describe": true,
	})
	if err != nil {
		t.Fatalf("screensee: %v", err)
	}
	m := res.(map[string]interface{})
	if m["description"] != "屏幕上显示的是测试画面" {
		t.Fatalf("unexpected description: %v", m["description"])
	}
	if !strings.HasPrefix(gotDataURL, "data:image/jpeg;base64,") {
		t.Fatalf("handler received bad dataURL: %s", gotDataURL)
	}

	// 客户端鉴权模式：服务端不拦截，总是转发（设备端自行决定是否执行）。
	// see-dev 未声明 screensee 之外的问题，此处仅验证服务端不再因授权状态报错。
	if _, err := dev.Execute("screensee", map[string]interface{}{"device_id": "see-dev"}); err != nil {
		t.Fatalf("server should forward regardless of authorization, got: %v", err)
	}
}

// ===== computeruse：鼠标/键盘控制命令下发 =====

func TestComputeruseEndToEnd(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-cu"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	dev := &devicectlDevice{reg: reg}

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"cu-dev","name":"操控机","kind":"computer","caps":["cmd","computeruse"]}}`))
	cli.readHelloAckAndBind(t, token)

	// 设备侧收 computeruse 命令并回执
	var receivedCmd string
	done := make(chan struct{})
	go func() {
		for {
			op, payload, err := cli.readMsg()
			if err != nil {
				return
			}
			if op != 0x1 {
				continue
			}
			var msg map[string]interface{}
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			if msg["op"] == "cmd" && msg["cmd_type"] == "homeagent" {
				receivedCmd, _ = msg["command"].(string)
				reqID, _ := msg["req_id"].(string)
				if strings.HasPrefix(receivedCmd, "computeruse ") {
					cli.sendText(mustJSON(map[string]interface{}{
						"op": "cmd_result", "req_id": reqID, "device_id": "cu-dev",
						"status": "ok", "output": "clicked at (3009,450)",
					}))
					close(done)
					return
				}
			}
		}
	}()

	// agent 调用 computeruse（结构化参数）
	res, err := dev.Execute("computeruse", map[string]interface{}{
		"device_id": "cu-dev", "action": "click", "x": float64(3009), "y": float64(450),
	})
	if err != nil {
		t.Fatalf("computeruse: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("device did not receive command")
	}
	// 验证下发的命令是合法 JSON 参数格式
	payloadJSON := strings.TrimPrefix(receivedCmd, "computeruse ")
	var params map[string]interface{}
	if err := json.Unmarshal([]byte(payloadJSON), &params); err != nil {
		t.Fatalf("command payload not valid JSON: %v (%s)", err, payloadJSON)
	}
	if params["action"] != "click" || params["x"] != float64(3009) || params["y"] != float64(450) {
		t.Fatalf("unexpected params: %v", params)
	}
	if m := res.(map[string]interface{}); m["status"] != "ok" {
		t.Fatalf("expected ok result: %v", m)
	}

	// 缺坐标应报错
	if _, err := dev.Execute("computeruse", map[string]interface{}{"device_id": "cu-dev", "action": "click"}); err == nil {
		t.Fatal("click without x/y should error")
	}
	// 未知 action 应报错
	if _, err := dev.Execute("computeruse", map[string]interface{}{"device_id": "cu-dev", "action": "fly"}); err == nil {
		t.Fatal("unknown action should error")
	}
	// type 需要 text
	if _, err := dev.Execute("computeruse", map[string]interface{}{"device_id": "cu-dev", "action": "type"}); err == nil {
		t.Fatal("type without text should error")
	}
}

// ===== clipboardsee / clipboardsue：剪切板读写 =====

func TestClipboardEndToEnd(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-clip"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	dev := &devicectlDevice{reg: reg}

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"clip-dev","name":"剪贴板机","kind":"computer","caps":["cmd"]}}`))
	cli.readHelloAckAndBind(t, token)

	// 设备侧响应剪贴板命令
	go func() {
		for {
			op, payload, err := cli.readMsg()
			if err != nil {
				return
			}
			if op != 0x1 {
				continue
			}
			var msg map[string]interface{}
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			if msg["op"] != "cmd" || msg["cmd_type"] != "homeagent" {
				continue
			}
			cmd, _ := msg["command"].(string)
			reqID, _ := msg["req_id"].(string)
			switch {
			case cmd == "clipboardsee":
				cli.sendText(mustJSON(map[string]interface{}{
					"op": "cmd_result", "req_id": reqID, "device_id": "clip-dev",
					"status": "ok", "output": "https://example.com/copied-link",
				}))
			case strings.HasPrefix(cmd, "clipboardsue "):
				written := strings.TrimPrefix(cmd, "clipboardsue ")
				cli.sendText(mustJSON(map[string]interface{}{
					"op": "cmd_result", "req_id": reqID, "device_id": "clip-dev",
					"status": "ok", "output": "clipboard set: " + written,
				}))
			}
		}
	}()

	t.Run("clipboardsee_returns_content", func(t *testing.T) {
		res, err := dev.Execute("clipboardsee", map[string]interface{}{"device_id": "clip-dev"})
		if err != nil {
			t.Fatalf("clipboardsee: %v", err)
		}
		m := res.(map[string]interface{})
		if m["content"] != "https://example.com/copied-link" {
			t.Fatalf("unexpected content: %v", m["content"])
		}
		if m["empty"] == true {
			t.Fatal("content should not be empty")
		}
	})

	t.Run("clipboardsue_writes_text", func(t *testing.T) {
		long := strings.Repeat("你好", 100) // 200 runes，验证 preview 截断
		res, err := dev.Execute("clipboardsue", map[string]interface{}{"device_id": "clip-dev", "text": long})
		if err != nil {
			t.Fatalf("clipboardsue: %v", err)
		}
		m := res.(map[string]interface{})
		if m["written"] != len(long) {
			t.Fatalf("written mismatch: %v", m["written"])
		}
		preview, _ := m["preview"].(string)
		if !strings.HasSuffix(preview, "...") || len([]rune(preview)) > 64 {
			t.Fatalf("preview should be truncated: %q", preview)
		}
	})

	t.Run("clipboardsue_requires_text", func(t *testing.T) {
		if _, err := dev.Execute("clipboardsue", map[string]interface{}{"device_id": "clip-dev"}); err == nil {
			t.Fatal("missing text should error")
		}
	})

	t.Run("server_forwards_regardless_of_authorization", func(t *testing.T) {
		// 客户端鉴权模式：服务端不再拦截未授权设备，由设备端自行拒绝。
		// 这里验证服务端能正常查询设备（不因授权状态报错）。
		if _, ok := reg.Get("clip-dev"); !ok {
			t.Fatal("device should be registered")
		}
	})
}

// ===== 能力矩阵：caps 声明 → 工具可用性 =====

func TestCapabilityMatrix(t *testing.T) {
	cases := []struct {
		name     string
		caps     []string
		tool     string
		expected bool
	}{
		{"摄像头只声明camera不能screensee", []string{"camera"}, "screensee", false},
		{"摄像头只声明camera可以camerasue", []string{"camera"}, "camerasue", true},
		{"屏幕设备支持screensue+screensee", []string{"screen"}, "screensee", true},
		{"clipboard能力含读写", []string{"clipboard"}, "clipboardsue", true},
		{"精确声明computeruse", []string{"computeruse"}, "computeruse", true},
		{"历史cmd视为全能力", []string{"status", "cmdrun", "deviceinfo"}, "screensee", true},
		{"无任何已知能力视为全兼容", []string{}, "computeruse", true},
		{"混合：有已知能力则严格匹配", []string{"camera", "screen"}, "computeruse", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deviceSupportsTool(tc.caps, tc.tool); got != tc.expected {
				t.Fatalf("deviceSupportsTool(%v, %s) = %v, want %v", tc.caps, tc.tool, got, tc.expected)
			}
		})
	}

	// 端到端：声明 camera 的设备调 screensee 应被拒绝
	reg := NewRegistry()
	token := "test-cap-token"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })
	dev := &devicectlDevice{reg: reg}

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()
	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"cam-only","name":"纯摄像头","kind":"camera","caps":["camera"]}}`))
	cli.readHelloAckAndBind(t, token)

	if _, err := dev.Execute("screensee", map[string]interface{}{"device_id": "cam-only"}); err == nil {
		t.Fatal("camera-only device should not support screensee")
	} else if !strings.Contains(err.Error(), "未声明 screensee 能力") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ===== 设备主动上报事件 → 事件回调 =====

func TestDeviceEventReport(t *testing.T) {
	reg := NewRegistry()
	token := "test-evt-token"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	var events []map[string]interface{}
	var evtMu sync.Mutex
	reg.SetEventHandler(func(deviceID string, msg map[string]interface{}) {
		evtMu.Lock()
		events = append(events, msg)
		evtMu.Unlock()
	})

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()
	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"cam-watch","name":"监控摄像头","kind":"camera","caps":["camera"]}}`))
	cli.readHelloAckAndBind(t, token)

	// 设备主动上报：识别到未知人员驻留
	cli.sendText(mustJSON(map[string]interface{}{
		"op": "event", "device_id": "cam-watch",
		"type":   "unknown_person_detected",
		"detail": "后门区域检测到陌生面孔，驻留超过30秒",
	}))
	// 不带 device_id 时应回退到当前连接的设备
	cli.sendText(mustJSON(map[string]interface{}{
		"op":   "event",
		"type": "motion",
	}))

	deadline := time.After(3 * time.Second)
	for {
		evtMu.Lock()
		n := len(events)
		evtMu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected 2 events, got %d", n)
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	evtMu.Lock()
	defer evtMu.Unlock()
	if events[0]["type"] != "unknown_person_detected" {
		t.Fatalf("unexpected first event: %v", events[0])
	}
	if events[1]["type"] != "motion" {
		t.Fatalf("unexpected second event: %v", events[1])
	}
}

func TestWSDoesNotExposeDeviceBeforeBind(t *testing.T) {
	reg := NewRegistry()
	token := "prebind-token"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()
	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"prebind-dev","name":"待绑定设备","kind":"computer","caps":["cmd"]}}`))
	deviceID := cli.readHelloAck(t)
	if deviceID != "prebind-dev" {
		t.Fatalf("unexpected device id: %s", deviceID)
	}
	if _, ok := reg.Get(deviceID); ok {
		t.Fatal("device must not be registered before bind")
	}
	if reg.Online(deviceID) {
		t.Fatal("device must not be online before bind")
	}
	if err := reg.PushJSON(deviceID, map[string]interface{}{"op": "cmd"}); err == nil {
		t.Fatal("command push must fail before bind")
	}

	cli.sendText(mustJSON(map[string]interface{}{
		"op": "cmd_result", "req_id": "prebind-result", "status": "ok",
	}))
	time.Sleep(20 * time.Millisecond)
	if _, ok := reg.GetResult("prebind-result"); ok {
		t.Fatal("result must be ignored before bind")
	}
}

func TestWSRejectedBindDoesNotRegister(t *testing.T) {
	reg := NewRegistry()
	reg.SetAcceptToken(func(provided string) bool { return provided == "expected-token" })

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()
	cli := dialTestWS(t, srv.URL, "")
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"rejected-dev","name":"拒绝设备","kind":"computer"}}`))
	deviceID := cli.readHelloAck(t)
	cli.sendText(mustJSON(map[string]interface{}{
		"op": "bind", "device_id": deviceID, "token": "wrong-token",
	}))
	op, payload, err := cli.readMsg()
	if err != nil {
		t.Fatalf("read rejected bind_ack: %v", err)
	}
	if op != 0x1 {
		t.Fatalf("expected rejected bind_ack text frame, got %x", op)
	}
	var ack map[string]interface{}
	if err := json.Unmarshal(payload, &ack); err != nil {
		t.Fatalf("decode rejected bind_ack: %v", err)
	}
	if ack["op"] != "bind_ack" || ack["ok"] != false {
		t.Fatalf("expected rejected bind_ack, got %v", ack)
	}
	if _, ok := reg.Get(deviceID); ok {
		t.Fatal("rejected device must not be registered")
	}
	if reg.Online(deviceID) {
		t.Fatal("rejected device must not be online")
	}
}

func TestWSHandshakeAuthorizationAllowsUnrelatedBindToken(t *testing.T) {
	reg := NewRegistry()
	transportToken := "transport-token"
	reg.SetAcceptToken(func(provided string) bool { return provided == transportToken })

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()
	cli := dialTestWS(t, srv.URL, transportToken)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"transport-dev","name":"代理设备","kind":"computer"}}`))
	deviceID := cli.readHelloAck(t)
	cli.bindDevice(t, deviceID, "unrelated-body-token")
	if !reg.Online(deviceID) {
		t.Fatal("handshake-authorized device should be online after bind")
	}
}

func TestAwaitResultReturnsResultDeliveredBeforeWaiter(t *testing.T) {
	reg := NewRegistry()
	want := map[string]interface{}{"status": "ok", "value": "early"}
	reg.deliverResult("early-result", want)

	got, err := reg.AwaitResult("early-result", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("await early result: %v", err)
	}
	if got["status"] != want["status"] || got["value"] != want["value"] {
		t.Fatalf("unexpected early result: %v", got)
	}
}

// ===== ping 帧处理：不得因未 bind 而断连 =====

// ★ 未 bind（或已 bind）时收到 ping，服务端必须回 pong，**不得关闭连接**。
//
// 真实 bug（生产日志实证）：原实现用 wsWriteLocked(curID, writePong)，而
// conns[curID] 只在 bind 成功后才写入 ⇒ 握手后、bind 前到来的 ping 找不到
// 写锁入口，函数返回错误，读循环直接 return 关连接。
//
// 后果：客户端每 30s ping 一次，只要有一次落在未 bind 窗口就断连；生产日志里
// 同一设备 20 秒内多次 "ws connected" 且 online/offline 反复交替，正是这个。
//
// 判据直打 bug 点：只握手、**不发 hello/bind**，发 ping，要求收到 pong。
// readMsg 会跳过 pong，所以这里直接读原始帧。
func TestPingBeforeBindDoesNotDropConnection(t *testing.T) {
	reg := NewRegistry()
	token := "tok-ping-bind"
	reg.SetAcceptToken(func(s string) bool { return s == token })

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	c := dialTestWS(t, srv.URL, token)
	defer c.close()

	// 只握手，不发 hello/bind —— 模拟「尚未绑定完成」的窗口
	c.sendFrame(0x9, nil) // ping

	// 必须收到 pong；EOF/错误说明服务端在 ping 路径上断了连接
	c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, opcode, err := readFrame(c.rw.Reader)
	if err != nil {
		t.Fatalf("未 bind 时 ping 导致连接不可用（服务端 bug）: %v", err)
	}
	if opcode != 0xA {
		t.Fatalf("期望 pong(0xA)，收到 opcode=%#x", opcode)
	}
}

// 已 bind 的设备发 ping 同样必须得到 pong，且连接与在线状态都保持。
func TestPingAfterBindGetsPong(t *testing.T) {
	reg := NewRegistry()
	token := "tok-ping-bound"
	reg.SetAcceptToken(func(s string) bool { return s == token })
	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	c := dialTestWS(t, srv.URL, token)
	defer c.close()
	c.sendText(mustJSON(map[string]interface{}{
		"op": "hello",
		"device": map[string]interface{}{
			"device_id": "d-ping", "name": "d", "kind": "computer", "caps": []string{"status"},
		},
	}))
	c.readHelloAck(t)
	c.bindDevice(t, "d-ping", token)
	if !reg.Online("d-ping") {
		t.Fatal("bind 后设备应在线")
	}

	c.sendFrame(0x9, nil) // ping
	c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, opcode, err := readFrame(c.rw.Reader)
	if err != nil {
		t.Fatalf("已 bind 设备 ping 失败: %v", err)
	}
	if opcode != 0xA {
		t.Fatalf("期望 pong(0xA)，收到 %#x", opcode)
	}
	if !reg.Online("d-ping") {
		t.Error("ping 之后设备不应掉线")
	}
}

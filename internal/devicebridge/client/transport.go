package client

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const (
	wsGUID    = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsVersion = "13"
	crlf      = "\r\n"
)

// wsConn 封装一条 WebSocket 连接（客户端视角，帧带 mask）。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer

	// closed 是**跨 goroutine** 的：写者是 Stop()（或 readLoop 退出时），
	// 读者是 readFrame（读循环）、writeFrame（发送路径）、以及 Bridge 的
	// 若干状态查询。
	//
	// ★ 原实现是裸 bool（`go test -race` 实测 DATA RACE：readFrame 的读
	// 与 close() 的写并发）。Bridge 的方法持 b.mu 读它，但写者 close()
	// 不持同一把锁 —— 锁保护不了它。改成 atomic.Bool 才有真正的可见性保证
	// （inline 写过之后，跨 goroutine 也能看见连接已关闭）。
	closed atomic.Bool
}

// isClosed 报告连接是否已被关闭（可并发调用）。
func (w *wsConn) isClosed() bool { return w.closed.Load() }

// markClosed 标记连接已关闭（可并发调用）。
func (w *wsConn) markClosed() { w.closed.Store(true) }

// dialWS 发起 WS 客户端握手升级。
// 支持 ws:// 和 wss://（wss 暂未实现，若需要需加 TLS dial）。
func dialWS(rawURL, token string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("devicebridge: invalid ws url %q: %w", rawURL, err)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}
	path := u.Path
	if u.RawQuery != "" {
		path = path + "?" + u.RawQuery
	}
	if path == "" {
		path = "/"
	}
	// 默认路径
	if token != "" && strings.Index(path, "token=") < 0 {
		if strings.ContainsRune(path, '?') {
			path = path + "&token=" + urlEscape(token)
		} else {
			path = path + "?token=" + urlEscape(token)
		}
	}

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.Dial("tcp", host)
	if err != nil {
		return nil, fmt.Errorf("devicebridge: dial %s: %w", host, err)
	}

	key := wsKey()
	var sb strings.Builder
	sb.WriteString("GET " + path + " HTTP/1.1" + crlf)
	sb.WriteString("Host: " + host + crlf)
	sb.WriteString("Upgrade: websocket" + crlf)
	sb.WriteString("Connection: Upgrade" + crlf)
	sb.WriteString("Sec-WebSocket-Key: " + key + crlf)
	sb.WriteString("Sec-WebSocket-Version: " + wsVersion + crlf + crlf)
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		conn.Close()
		return nil, fmt.Errorf("devicebridge: write upgrade: %w", err)
	}

	br := bufio.NewReader(conn)
	var headerBuf strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("devicebridge: read upgrade resp: %w", err)
		}
		headerBuf.WriteString(line)
		if strings.Contains(headerBuf.String(), crlf+crlf) {
			break
		}
	}
	if !strings.Contains(headerBuf.String(), " 101 ") {
		conn.Close()
		return nil, fmt.Errorf("devicebridge: upgrade failed: %s", firstLine(headerBuf.String()))
	}

	return &wsConn{conn: conn, br: br, bw: bufio.NewWriter(conn)}, nil
}

// writeText 发送 WS 文本帧（0x1，带 mask）。
func (w *wsConn) writeText(payload []byte) error {
	return w.writeFrame(0x1, payload)
}

// writeBinary 发送 WS 二进制帧（0x2，带 mask）。
func (w *wsConn) writeBinary(payload []byte) error {
	return w.writeFrame(0x2, payload)
}

// writeFrame 发送一个 WS 帧（客户端 mask 模式）。
func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	if w.isClosed() {
		return fmt.Errorf("devicebridge: connection closed")
	}
	length := len(payload)

	// 帧头
	hdrLen := 2
	switch {
	case length < 126:
		// 1 byte length
	case length <= 0xffff:
		hdrLen += 2
	default:
		hdrLen += 8
	}
	hdrLen += 4 // mask key

	hdr := make([]byte, hdrLen)
	hdr[0] = 0x80 | opcode
	switch {
	case length < 126:
		hdr[1] = 0x80 | byte(length)
	case length <= 0xffff:
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(length))
	default:
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(length))
	}

	// mask key
	var maskKey [4]byte
	rand.Read(maskKey[:])
	copy(hdr[hdrLen-4:], maskKey[:])

	// mask payload
	masked := make([]byte, length)
	for i := 0; i < length; i++ {
		masked[i] = payload[i] ^ maskKey[i&3]
	}

	if _, err := w.bw.Write(hdr); err != nil {
		return err
	}
	if _, err := w.bw.Write(masked); err != nil {
		return err
	}
	return w.bw.Flush()
}

// readFrame 读取一个 WS 帧。返回 (payload, isClose, opcode, error)。
// 客户端收到的帧不带 mask。
func (w *wsConn) readFrame() ([]byte, bool, byte, error) {
	if w.isClosed() {
		return nil, true, 0, fmt.Errorf("devicebridge: connection closed")
	}
	b0, err := w.br.ReadByte()
	if err != nil {
		return nil, true, 0, err
	}
	opcode := b0 & 0x0f
	b1, err := w.br.ReadByte()
	if err != nil {
		return nil, true, 0, err
	}
	length := uint64(b1 & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return nil, true, opcode, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return nil, true, opcode, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	// 二进制帧允许更大（8MB），文本帧 1MB
	maxFrame := uint64(1 << 20)
	if opcode == 0x2 {
		maxFrame = 8 << 20
	}
	if length > maxFrame {
		return nil, true, opcode, fmt.Errorf("devicebridge: frame too large (%d bytes)", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(w.br, payload); err != nil {
		return nil, true, opcode, err
	}
	switch opcode {
	case 0x1, 0x2:
		return payload, false, opcode, nil
	case 0x8:
		return nil, true, opcode, nil
	case 0x9: // ping
		return nil, false, opcode, errPing
	case 0xa: // pong
		return nil, false, opcode, nil
	default:
		return nil, false, opcode, fmt.Errorf("devicebridge: unsupported opcode %x", opcode)
	}
}

// writePong 发送 pong 帧。
func (w *wsConn) writePong() error {
	return w.writeFrame(0xa, nil)
}

// close 发送关闭帧并关闭连接。
func (w *wsConn) close() error {
	w.markClosed()
	_ = w.writeFrame(0x8, nil)
	return w.conn.Close()
}

// ===== 辅助函数 =====

var errPing = fmt.Errorf("ping")

func wsKey() string {
	var b [16]byte
	rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

func urlEscape(s string) string {
	var sb strings.Builder
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('%')
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&0xf])
		}
	}
	return sb.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func wsAccept(key string) string {
	h := sha256.Sum256([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// readWSFrame 读取一个 WS 帧（从已有的 bufio.Reader，兼容非 wsConn 场景）。
func readWSFrame(br *bufio.Reader) ([]byte, bool, byte, error) {
	b0, err := br.ReadByte()
	if err != nil {
		return nil, true, 0, err
	}
	opcode := b0 & 0x0f
	b1, err := br.ReadByte()
	if err != nil {
		return nil, true, 0, err
	}
	length := uint64(b1 & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, true, opcode, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, true, opcode, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	maxFrame := uint64(1 << 20)
	if opcode == 0x2 {
		maxFrame = 8 << 20
	}
	if length > maxFrame {
		return nil, true, opcode, fmt.Errorf("frame too large")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, true, opcode, err
	}
	switch opcode {
	case 0x1, 0x2:
		return payload, false, opcode, nil
	case 0x8:
		return nil, true, opcode, nil
	default:
		return nil, false, opcode, nil
	}
}

// writeWSFrame 发送一个 WS 帧（非 mask 模式，服务端用）。
func writeWSFrame(w io.Writer, opcode byte, payload []byte) error {
	length := len(payload)
	hdr := []byte{0x80 | opcode}
	switch {
	case length < 126:
		hdr = append(hdr, byte(length))
	case length <= 0xffff:
		hdr = append(hdr, 126, 0, 0)
		binary.BigEndian.PutUint16(hdr[len(hdr)-2:], uint16(length))
	default:
		hdr = append(hdr, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[len(hdr)-8:], uint64(length))
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return nil
}

// ensureTimeout 设置连接读写超时。
func (w *wsConn) setDeadline(t time.Time) {
	if w.conn != nil {
		w.conn.SetDeadline(t)
	}
}

// LocalAddr 返回本地地址。
func (w *wsConn) LocalAddr() net.Addr {
	if w.conn != nil {
		return w.conn.LocalAddr()
	}
	return nil
}

// RemoteAddr 返回远程地址。
func (w *wsConn) RemoteAddr() net.Addr {
	if w.conn != nil {
		return w.conn.RemoteAddr()
	}
	return nil
}

// Package ipc 实现 guard↔worker 之间的 PING/ACK 心跳协议：worker 监听 unix socket，
// guard 发 PING，worker 回 ACK（含 kernel 状态快照），实现带自诊断上报的存活判定，
// 取代单纯的文件心跳 + 退出码。
package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// sinkPath 心跳 socket 文件路径。
func sinkPath(dataDir string) string {
	return filepath.Join(dataDir, "worker.ipc")
}

// Status worker 上报给 guard 的自诊断快照。
type Status struct {
	PID       int    `json:"pid"`
	Boot      string `json:"boot"`                // normal | failback
	UptimeSec int64  `json:"uptime_sec"`          // 进程存活秒数
	LLMOK     *bool  `json:"llm_ok,omitempty"`    // rescue 源可达性（nil=未探）
	Tools     int    `json:"tools"`               // 已注册工具数
	LastDiag  string `json:"last_diag,omitempty"` // 最近一次自诊断结论（如 diag_loc.cause）
}

// StatusFunc 组装 worker 当前状态。
type StatusFunc func() *Status

// Server worker 侧：监听 unix socket，处理 PING→ACK。
type Server struct {
	doneOnce sync.Once
	path     string
	ln       net.Listener
	status   StatusFunc
	stop     chan struct{}
	done     chan struct{}
}

// NewServer 创建心跳服务端（尚未 Listen，见 Start）。
func NewServer(dataDir string, status StatusFunc) *Server {
	return &Server{path: sinkPath(dataDir), status: status, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start 启动监听。若 socket 已存在则先清理（无心跳残留）。
func (s *Server) Start() error {
	if s.status == nil {
		s.status = func() *Status { return nil }
	}
	if err := os.RemoveAll(s.path); err != nil {
		return err
	}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("ipc listen %s: %w", s.path, err)
	}
	s.ln = ln
	go s.acceptLoop()
	return nil
}

func (s *Server) acceptLoop() {
	defer safeclose.Safe(&s.doneOnce, s.done)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		return
	}
	line := sc.Text()
	if line != "PING" {
		return
	}
	resp := map[string]interface{}{"type": "ACK"}
	if st := s.status(); st != nil {
		resp["status"] = st
	}
	data, _ := json.Marshal(resp)
	conn.Write(append(data, '\n'))
}

// Stop 关闭监听。
func (s *Server) Stop() {
	if s.ln != nil {
		s.ln.Close()
	}
	<-s.done
}

// Client guard 侧：向 worker 心跳 socket 发 PING 并等 ACK。
type Client struct {
	path string
}

// NewClient 创建客户端。
func NewClient(dataDir string) *Client {
	return &Client{path: sinkPath(dataDir)}
}

// Ping 发一次 PING、收 ACK。timeout 内未收到返回错误。返回 (status, error)；
// status 可能为 nil（ACK 无状态体）。
func (c *Client) Ping(timeout time.Duration) (*Status, error) {
	conn, err := net.DialTimeout("unix", c.path, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte("PING\n")); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		return nil, fmt.Errorf("ipc: empty ACK")
	}
	var resp struct {
		Type   string  `json:"type"`
		Status *Status `json:"status"`
	}
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return nil, err
	}
	if resp.Type != "ACK" {
		return nil, fmt.Errorf("ipc: unexpected reply %q", resp.Type)
	}
	return resp.Status, nil
}

// Close 无状态（客户端用完即释放连接）。
func (c *Client) Close() {}

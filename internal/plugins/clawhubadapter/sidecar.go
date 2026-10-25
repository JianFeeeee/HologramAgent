package clawhubadapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type sidecarRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type sidecarResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      int              `json:"id"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type OCPTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type OCCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"content"`
}

// OCNotification 是模拟器主动推送的通知
type OCNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// pendingResp 是待应答通道 + 它自己的 close 守卫。
//
// 为什么需要 once：Close() 可能被多条路径调用（Stop / 重载 / 停机），
// 而 `close(ch)` 二次即 panic（本仓 close 纪律，见 internal/safeclose）。
// 这些 channel 是**按请求创建**的，不能共用一个全局 Once ⇒ 每个自带一个。
type pendingResp struct {
	ch   chan []byte
	once sync.Once
}

func (p *pendingResp) closeOnce() { safeclose.Safe(&p.once, p.ch) }

type sidecarProcess struct {
	readerReadyOnce sync.Once
	name            string
	dir             string
	cmd             *exec.Cmd
	stdin           *bufio.Writer
	mu              sync.Mutex
	nextID          int
	closed          bool
	stopped         bool

	// 异步 reader
	pending     map[int]*pendingResp
	notifyCh    chan OCNotification
	readerStop  chan struct{}
	readerWg    sync.WaitGroup
	readerReady chan struct{}
}

func newSidecarProcess(name, dir string, cmd *exec.Cmd, stdin *bufio.Writer, stdout io.Reader) *sidecarProcess {
	sp := &sidecarProcess{
		name:        name,
		dir:         dir,
		cmd:         cmd,
		stdin:       stdin,
		pending:     make(map[int]*pendingResp),
		notifyCh:    make(chan OCNotification, 1024),
		readerStop:  make(chan struct{}),
		readerReady: make(chan struct{}),
	}
	sp.readerWg.Add(1)
	go sp.readLoop(stdout)
	<-sp.readerReady
	return sp
}

func (s *sidecarProcess) readLoop(r io.Reader) {
	defer s.readerWg.Done()
	scanner := bufio.NewScanner(bufio.NewReader(r))
	// 加大 scanner buffer 防止长行截断
	scanner.Buffer(make([]byte, 0, 1024*64), 1024*64)
	safeclose.Safe(&s.readerReadyOnce, s.readerReady)

	for {
		select {
		case <-s.readerStop:
			return
		default:
		}

		if !scanner.Scan() {
			if scanner.Err() != nil {
				log.Printf("[clawhubadapter] sidecar %s read error: %v", s.name, scanner.Err())
			}
			return
		}
		line := scanner.Text()

		var base struct {
			ID     *int             `json:"id"`
			Method string           `json:"method,omitempty"`
			Error  *json.RawMessage `json:"error,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &base); err != nil {
			continue
		}

		if base.ID != nil {
			s.mu.Lock()
			pr, ok := s.pending[*base.ID]
			delete(s.pending, *base.ID)
			s.mu.Unlock()
			if ok {
				pr.ch <- []byte(line)
			}
		} else if base.Method != "" {
			var notif OCNotification
			if err := json.Unmarshal([]byte(line), &notif); err == nil {
				select {
				case s.notifyCh <- notif:
				default:
					log.Printf("[clawhubadapter] sidecar %s notify channel full, dropping: %s", s.name, notif.Method)
				}
			}
		}
	}
}

func (s *sidecarProcess) NotifyChan() <-chan OCNotification {
	return s.notifyCh
}

func launchSidecar(dir, name, simDir string) (*sidecarProcess, error) {
	mainJS := filepath.Join(dir, "main.js")
	if _, err := os.Stat(mainJS); os.IsNotExist(err) {
		return nil, nil
	}
	return launchProcess("node", mainJS, dir, name, simDir)
}

// launchProcess 以 dir 作为工作目录拉起子进程（绝大多数调用的形态）。
func launchProcess(bin, arg, dir, name, simDir string) (*sidecarProcess, error) {
	return launchProcessIn(bin, arg, dir, name, simDir, dir)
}

// launchProcessIn 与 launchProcess 相同，但工作目录可单独指定。
//
// 为什么要单独一个函数而不是给 launchProcess 加参数：那会改动 4 个既有
// 调用点（含测试），而它们的语义都是"工作目录 = skill 目录"。只有
// skill_workdir 配置这一处需要另给目录，用变体表达的意图更清楚。
func launchProcessIn(bin, arg, dir, name, simDir, workDir string) (*sidecarProcess, error) {
	nodePath := bin
	if bin == "node" {
		if p := os.Getenv("NODE_PATH"); p != "" {
			nodePath = filepath.Join(p, "node")
		}
	}

	cmd := exec.Command(nodePath, arg, dir)
	if workDir == "" {
		workDir = dir
	}
	cmd.Dir = workDir
	cmd.Stderr = os.Stderr

	// Add openclaw CLI bin dir to PATH so subprocesses can exec 'openclaw' command
	if simDir != "" {
		binDir := filepath.Join(simDir, "bin")
		if info, err := os.Stat(binDir); err == nil && info.IsDir() {
			env := os.Environ()
			binDirPath := binDir + string(os.PathListSeparator)
			for i, e := range env {
				if strings.HasPrefix(e, "PATH=") {
					env[i] = "PATH=" + binDirPath + e[5:]
					break
				}
			}
			cmd.Env = env
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}

	sp := newSidecarProcess(name, dir, cmd, bufio.NewWriter(stdin), stdout)

	if err := sp.waitReady(); err != nil {
		sp.Close()
		return nil, err
	}

	return sp, nil
}

func (s *sidecarProcess) waitReady() error {
	done := make(chan error, 1)
	go func() {
		_, err := s.call("ping", nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("sidecar %s ping timeout", s.name)
	}
}

func (s *sidecarProcess) call(method string, params interface{}) ([]byte, error) {
	ch := make(chan []byte, 1)

	s.mu.Lock()
	if s.closed || s.stopped {
		s.mu.Unlock()
		return nil, fmt.Errorf("sidecar %s closed", s.name)
	}
	s.nextID++
	id := s.nextID
	s.pending[id] = &pendingResp{ch: ch}

	req := sidecarRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	if _, err := s.stdin.Write(data); err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	if _, err := s.stdin.Write([]byte("\n")); err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	if err := s.stdin.Flush(); err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	select {
	case raw := <-ch:
		if raw == nil {
			return nil, fmt.Errorf("sidecar %s error: nil response", s.name)
		}
		var resp sidecarResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("sidecar %s unmarshal: %w", s.name, err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("sidecar %s error: %s", s.name, resp.Error.Message)
		}
		if resp.Result == nil {
			return nil, nil
		}
		return []byte(*resp.Result), nil
	case <-time.After(30 * time.Second):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("sidecar %s call %s timeout", s.name, method)
	}
}

func (s *sidecarProcess) ListTools() ([]OCPTool, error) {
	data, err := s.call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var result struct {
		Tools []OCPTool `json:"tools"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func (s *sidecarProcess) CallTool(name string, args map[string]interface{}) (string, error) {
	data, err := s.call("tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", nil
	}
	var result OCCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	var sb string
	for _, c := range result.Content {
		if c.Type == "text" {
			sb += c.Text
		}
	}
	return sb, nil
}

func (s *sidecarProcess) CallProvider(providerType string, args map[string]interface{}) (string, error) {
	data, err := s.call("provider/call", map[string]interface{}{
		"type":   providerType,
		"action": "execute",
		"args":   args,
	})
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", nil
	}
	var result OCCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	var sb string
	for _, c := range result.Content {
		if c.Type == "text" {
			sb += c.Text
		}
	}
	return sb, nil
}

func (s *sidecarProcess) Close() {
	s.mu.Lock()
	if s.closed || s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()

	// 先杀进程（关闭 stdout pipe），然后 reader 的 Scan() 会退出
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.cmd.Wait()
	}

	// 等 reader 循环结束
	s.readerWg.Wait()

	s.mu.Lock()
	for id, pr := range s.pending {
		pr.closeOnce()
		delete(s.pending, id)
	}
	s.mu.Unlock()

	log.Printf("[clawhubadapter] sidecar %s stopped", s.name)
}

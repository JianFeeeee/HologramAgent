package text

import (
	"bufio"
	"encoding/json"
	"fmt"
	safeclose "github.com/JianFeeeee/HomeAgent/internal/safeclose"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event — 原始 I/O 事件记录，写入 JSONL
type Event struct {
	Timestamp int64    `json:"ts"`
	Source    string   `json:"source"`
	Input     string   `json:"input"`
	Response  string   `json:"response,omitempty"`
	ToolsUsed []string `json:"tools_used,omitempty"`
	AgentID   string   `json:"agent_id,omitempty"`
}

// Memory — 文本记忆：追加写 JSONL，按时间/大小旋转
type Memory struct {
	pauseMu  sync.Mutex
	paused   bool
	pauseCh  chan struct{}
	stopOnce sync.Once
	dir      string
	interval time.Duration
	maxSize  int64

	mu      sync.Mutex
	current *os.File
	encoder *json.Encoder
	created time.Time
	size    int64

	stopCh chan struct{}
}

type Option func(*Memory)

func WithRotationInterval(d time.Duration) Option {
	return func(m *Memory) { m.interval = d }
}

func WithMaxSizeBytes(n int64) Option {
	return func(m *Memory) { m.maxSize = n }
}

func New(dir string, opts ...Option) *Memory {
	m := &Memory{
		dir:      dir,
		interval: 24 * time.Hour,
		maxSize:  10 * 1024 * 1024,
		stopCh:   make(chan struct{}),
		pauseCh:  make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func (m *Memory) Start() error {
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		return fmt.Errorf("text memory dir: %w", err)
	}
	if err := m.openCurrent(); err != nil {
		return err
	}
	go m.rotationLoop()
	return nil
}

// isPaused 报告是否处于伪关机。
func (m *Memory) isPaused() bool {
	m.pauseMu.Lock()
	defer m.pauseMu.Unlock()
	return m.paused
}

// Pause 伪关机：停止滚动写盘（可逆）。返回前持有写锁，
// 保证**当前没有任何在途写入**。
//
// 语义（迁移依赖）：返回后到 Resume 之前，m.mu 保护的写路径
// 仍可能被 Append 调用 —— 迁移时调用方应先停掉上游（插件/agent），
// 本 Pause 只负责**后台定时写**这一路。
func (m *Memory) Pause() error {
	m.pauseMu.Lock()
	m.paused = true
	m.pauseMu.Unlock()
	// 拿一次写锁再放开：确保在途的 rotateLocked 已结束
	m.mu.Lock()
	m.mu.Unlock()
	return nil
}

// Resume 唤醒。
func (m *Memory) Resume() error {
	m.pauseMu.Lock()
	was := m.paused
	m.paused = false
	m.pauseMu.Unlock()
	if was {
		select {
		case m.pauseCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// Stop 幂等（本仓 close 纪律：见 internal/safeclose）。
func (m *Memory) Stop() {
	safeclose.Safe(&m.stopOnce, m.stopCh)
	m.mu.Lock()
	if m.current != nil {
		m.current.Close()
	}
	m.mu.Unlock()
}

func (m *Memory) Append(evt Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.needRotate() {
		m.rotateLocked()
	}

	if err := m.encoder.Encode(evt); err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	m.current.Sync()
	return nil
}

func (m *Memory) needRotate() bool {
	return time.Since(m.created) > m.interval || m.size > m.maxSize
}

func (m *Memory) rotateLocked() {
	if m.current != nil {
		m.current.Close()
	}
	m.openCurrent()
}

func (m *Memory) openCurrent() error {
	now := time.Now()
	name := fmt.Sprintf("text_%s.jsonl", now.Format("2006-01-02_15-04-05"))
	path := filepath.Join(m.dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open text log %s: %w", path, err)
	}

	stat, _ := f.Stat()
	m.current = f
	m.encoder = json.NewEncoder(f)
	m.created = now
	m.size = stat.Size()
	return nil
}

func (m *Memory) rotationLoop() {
	ticker := time.NewTicker(m.interval / 2)
	defer ticker.Stop()
	for {
		// ★ 伪关机门：迁移期间不能滚动写盘（见 kernelboot/lifecycle.go）
		if m.isPaused() {
			select {
			case <-m.pauseCh:
				continue
			case <-m.stopCh:
				return
			}
		}
		select {
		case <-ticker.C:
			if m.isPaused() {
				continue
			}
			m.mu.Lock()
			if m.needRotate() {
				m.rotateLocked()
				log.Printf("[text memory] rotated log file")
			}
			m.mu.Unlock()
		case <-m.stopCh:
			return
		}
	}
}

// Replay — 从 JSONL 文件流式回放事件
func (m *Memory) Replay(fn func(Event) error) error {
	m.mu.Lock()
	files, err := m.listFiles()
	m.mu.Unlock()
	if err != nil {
		return err
	}

	for _, fpath := range files {
		if err := m.replayFile(fpath, fn); err != nil {
			return err
		}
	}
	return nil
}

func (m *Memory) replayFile(path string, fn func(Event) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if err := fn(evt); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (m *Memory) listFiles() ([]string, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "text_") && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(m.dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// RecentEvents — 返回最近 n 条事件（跨所有文件的最新事件）
func (m *Memory) RecentEvents(n int) ([]Event, error) {
	var all []Event
	err := m.Replay(func(evt Event) error {
		all = append(all, evt)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

func (m *Memory) FileCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	files, err := m.listFiles()
	if err != nil {
		return 0
	}
	return len(files)
}

// PurgeByFilter 删除所有满足 filter 函数的事件（重写所有 JSONL 文件）
func (m *Memory) PurgeByFilter(filter func(Event) bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 关闭当前文件，准备重建
	if m.current != nil {
		m.current.Close()
		m.current = nil
	}

	files, err := m.listFiles()
	if err != nil {
		return 0, err
	}

	totalRemoved := 0
	for _, fpath := range files {
		kept, removed, err := m.purgeFile(fpath, filter)
		if err != nil {
			log.Printf("[text memory] purge file %s: %v", fpath, err)
			continue
		}
		totalRemoved += removed

		if len(kept) == 0 {
			os.Remove(fpath)
		} else if removed > 0 {
			m.rewriteFile(fpath, kept)
		}
	}

	// 重新打开当前文件
	m.openCurrent()
	return totalRemoved, nil
}

// ReplaceByFilter 替换所有满足 filter 的事件（通过 replace 函数修改），重写文件
func (m *Memory) ReplaceByFilter(filter func(Event) bool, replace func(Event) Event) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current != nil {
		m.current.Close()
		m.current = nil
	}

	files, err := m.listFiles()
	if err != nil {
		return 0, err
	}

	totalReplaced := 0
	for _, fpath := range files {
		events, replaced, err := m.replaceFile(fpath, filter, replace)
		if err != nil {
			log.Printf("[text memory] replace file %s: %v", fpath, err)
			continue
		}
		totalReplaced += replaced

		if len(events) == 0 {
			os.Remove(fpath)
		} else if replaced > 0 {
			m.rewriteFile(fpath, events)
		}
	}

	m.openCurrent()
	return totalReplaced, nil
}

// ——— internal helpers ———

func (m *Memory) purgeFile(path string, filter func(Event) bool) (kept []Event, removed int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if filter(evt) {
			removed++
		} else {
			kept = append(kept, evt)
		}
	}
	return kept, removed, scanner.Err()
}

func (m *Memory) replaceFile(path string, filter func(Event) bool, replace func(Event) Event) (events []Event, replaced int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if filter(evt) {
			evt = replace(evt)
			replaced++
		}
		events = append(events, evt)
	}
	return events, replaced, scanner.Err()
}

func (m *Memory) rewriteFile(path string, events []Event) {
	f, err := os.Create(path)
	if err != nil {
		log.Printf("[text memory] rewrite %s: %v", path, err)
		return
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, evt := range events {
		enc.Encode(evt)
	}
}

func (m *Memory) Stats() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	files, _ := m.listFiles()
	return map[string]interface{}{
		"file_count":        len(files),
		"current_size":      m.size,
		"rotation_bytes":    m.maxSize,
		"rotation_interval": m.interval.String(),
		"dir":               m.dir,
	}
}

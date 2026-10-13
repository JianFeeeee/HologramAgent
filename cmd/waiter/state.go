package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

type State struct {
	conn Conn
	mu   sync.Mutex
}

func (s *State) Connect(cfg *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
	}
	c, err := dial(cfg)
	if err != nil {
		return err
	}
	s.conn = c
	return nil
}

func (s *State) Disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

func (s *State) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

func (s *State) RemoteConn() *remoteConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rc, ok := s.conn.(*remoteConn); ok {
		return rc
	}
	return nil
}

func (s *State) Send(line string) error {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return fmt.Errorf("not connected")
	}
	return c.Send(line)
}

func (s *State) SendChat(msg string) (string, error) {
	return s.SendChatStream(msg, nil)
}

// SendChatStream 发送一条对话消息并循环读取响应行直至终结帧。
// onEvent 回调在每收到一个过程帧（reasoning/tool_call）时被调用，
// 可为 nil；返回值为最终响应内容或错误。
func (s *State) SendChatStream(msg string, onEvent func(respLine)) (string, error) {
	if err := s.Send(msg); err != nil {
		return "", err
	}
	for {
		line, err := s.readLine()
		if err != nil {
			return "", err
		}
		rl := parseRespLineStruct(line)
		switch rl.Type {
		case "response":
			return rl.Content, nil
		case "error":
			if rl.Error == "" {
				rl.Error = line
			}
			return "", fmt.Errorf("%s", rl.Error)
		default:
			// 过程帧：reasoning / tool_call / 旧版服务器的普通文本
			if rl.Type == "" && onEvent == nil && rl.Content == "" && rl.Error == "" {
				// 非JSON旧行且无回调：直接当最终输出（向后兼容旧服务器）
				return line, nil
			}
			if onEvent != nil {
				onEvent(rl)
			}
		}
	}
}

func (s *State) SendBuiltin(cmd string) (string, error) {
	if err := s.Send(cmd); err != nil {
		return "", err
	}
	line, err := s.readLine()
	if err != nil {
		return "", err
	}
	return parseRespLine(line)
}

func (s *State) readLine() (string, error) {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return "", fmt.Errorf("not connected")
	}
	return c.ReadLine()
}

type respLine struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Error   string `json:"error"`
	Tool    string `json:"tool"`
	Status  string `json:"status"`
	Result  string `json:"result"`
	// Phase 是 stage 帧的阶段名（on_input / pre_action / post_action /
	// toolcall / before_output / after_output …）。
	//
	// 为何要加（2026-10-13 协议审计）：CLI 此前是唯一看不到阶段进展的客户端，
	// 服务端补发 stage 帧后，客户端必须有字段承载它。
	Phase string `json:"phase,omitempty"`
	// Event / Plugin 是 system 帧的字段（目前只有 plugin_crashed）。
	//
	// 为何要加（2026-10-13 协议审计）：system 是内核级状态事件，
	// 而 CLI 此前既不订阅它、也没有字段承载 —— 插件崩溃对终端用户不可见。
	Event  string `json:"event,omitempty"`
	Plugin string `json:"plugin,omitempty"`
	Reset  bool   `json:"reset,omitempty"` // delta 帧：服务端轮次作废，清空累积
}

// parseRespLineStruct 解析一行 JSON 响应帧，解析失败时将原文放入 Content。
func parseRespLineStruct(line string) respLine {
	var rl respLine
	if err := json.Unmarshal([]byte(line), &rl); err != nil {
		return respLine{Content: line}
	}
	return rl
}

func parseRespLine(line string) (string, error) {
	rl := parseRespLineStruct(line)
	switch rl.Type {
	case "response":
		return rl.Content, nil
	case "error":
		return "", fmt.Errorf("%s", rl.Error)
	default:
		return line, nil
	}
}

func (s *State) ReadLoop(ctx context.Context, cb func(string)) {
	for {
		s.mu.Lock()
		c := s.conn
		s.mu.Unlock()
		if c == nil {
			return
		}
		done := make(chan struct{})
		var line string
		var readErr error
		go func() {
			line, readErr = c.ReadLine()
			close(done)
		}()
		select {
		case <-done:
			if readErr != nil {
				return
			}
			cb(line)
		case <-ctx.Done():
			return
		}
	}
}

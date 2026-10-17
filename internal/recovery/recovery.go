// Package recovery 实现 L1 failback 的确定性恢复梯子与 guard↔worker 之间的
// 恢复任务/结果文件协议。整个梯子无 token 消耗（除 rescue 源 QuickChat 复检外）：
//
//	probe(main 可达) → 还原 DNS/proxy → probe → 还原 config 快照+ReloadFromConfig → probe
package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Brightness 恢复梯子各阶段的判定强度（越低越优先主张）。
type Brightness int

const (
	BrightDeterministic Brightness = iota // 无推理
	BrightMinimal                         // 最小推理（未知/混合分支）
)

// RescueSource guard.yaml 中配置的救援 LLM 源（锚定 IP，绕开被破坏的 DNS/代理）。
type RescueSource struct {
	Name     string `yaml:"name" json:"name"`
	BaseURL  string `yaml:"base_url" json:"base_url"`
	APIKey   string `yaml:"api_key" json:"api_key"`
	Model    string `yaml:"model" json:"model"`
	Adapter  string `yaml:"adapter" json:"adapter"`
	Thinking bool   `yaml:"thinking_enabled" json:"thinking_enabled"`
}

// Task guard 写、failback worker 读的恢复任务（跨进程文件协议）。
type Task struct {
	Attempt        int          `json:"attempt"`
	MaxAttempts    int          `json:"max_attempts"`
	AttemptTimeout string       `json:"attempt_timeout"` // 单轮超时，如 "120s"
	TriggerPrompt  string       `json:"trigger_prompt,omitempty"`
	KnowledgeBase  string       `json:"knowledge_base,omitempty"`
	RescueSource   RescueSource `json:"rescue_source"`
	PluginList     []string     `json:"plugins,omitempty"` // failback 插件集（缺省回退 config.db）
}

// Plugins 返回 failback 插件集；为空则返回 nil。
func (t *Task) Plugins() []string { return t.PluginList }

// TaskPath 返回 <dataDir>/recovery/recovery_task.json。
func TaskPath(dataDir string) string {
	return filepath.Join(dataDir, "recovery", "recovery_task.json")
}

// SaveTask 持久化恢复任务。
func SaveTask(path string, t *Task) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadTask 读回恢复任务。
func LoadTask(path string) (*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Result recovery worker 单次 failback 尝试的结论，写给 guard 读取。
type Result struct {
	Attempt        int       `json:"attempt"`
	Success        bool      `json:"success"`
	Cause          string    `json:"cause"`           // 判定出的根因类（取 diag / 梯子命中）
	NetworkOK      bool      `json:"network_ok"`      // 网络（resolv/hosts）是否与基线一致
	RestoredNet    []string  `json:"restored_net"`    // 已还原的网络文件
	ConfigRestored bool      `json:"config_restored"` // 是否还原 LLM 配置快照
	QuickChatOK    bool      `json:"quickchat_ok"`    // 最后用 rescue 源复检是否可达
	Steps          []string  `json:"steps"`           // 走过的恢复步骤
	Message        string    `json:"message"`
	Timestamp      time.Time `json:"timestamp"`
}

// Quote 生成一行可读摘要（写入 worker 日志 / guard 决策用）。
func (r *Result) Quote() string {
	return fmt.Sprintf("success=%v cause=%q net_ok=%v config_restored=%v quickchat=%v steps=%d",
		r.Success, r.Cause, r.NetworkOK, r.ConfigRestored, r.QuickChatOK, len(r.Steps))
}

// Prober 探测 LLM 源是否可达（QuickChat）。用于梯子各阶段复检。
type Prober func(ctx context.Context, label string) (bool, error)

// NetworkRestorer 还原 DNS/proxy。返回改动列表。
type NetworkRestorer func() ([]string, error)

// ConfigRestorer 还原 LLM 配置快照并 ReloadFromConfig。返回精度（改动键数）。
type ConfigRestorer func() (int, error)

// Ladder 确定性恢复梯子。
type Ladder struct {
	ResultFile     string
	Attempt        int
	Probe          Prober
	RestoreNetwork NetworkRestorer
	RestoreConfig  ConfigRestorer
	Log            func(format string, args ...interface{})
}

// Run 执行一次恢复梯子并返回结论（同时持久化到 ResultFile）。错误仅表示梯子
// 自身失败；结论成败由 Result.Success 表达。
func (l *Ladder) Run(ctx context.Context) *Result {
	res := &Result{
		Attempt:   l.Attempt,
		Cause:     "unknown",
		Timestamp: time.Now(),
	}
	l.addStep(res, "begin attempt")

	// 1. 初始探测：quickchat 已通 → 无需恢复（网络与配置至少一个坏，但 rescue 可达）
	ok, err := l.Probe(ctx, "initial")
	if err == nil && ok {
		res.Success = true
		res.Cause = "healthy"
		l.addStep(res, "initial probe: LLM reachable")
		l.finish(res)
		return res
	}
	res.Cause = "unreachable"
	l.addStep(res, "initial probe: unreachable: %v", err)

	// 2. 还原 DNS/proxy（L1 第一步小修命中即停）
	if nested, rerr := l.RestoreNetwork(); rerr != nil {
		l.addStep(res, "network restore error: %v", rerr)
	} else {
		res.RestoredNet = nested
		res.NetworkOK = len(nested) == 0
		l.addStep(res, "network restore: %d changed", len(nested))
	}
	if ok, _ = l.Probe(ctx, "after-network"); err == nil && ok {
		res.Success = true
		res.Cause = "network_config"
		l.addStep(res, "post-network probe: reachable")
		l.finish(res)
		return res
	}
	l.addStep(res, "post-network probe: unreachable")

	// 3. 还原 config 快照 + ReloadFromConfig（L1 第二步）
	if n, cerr := l.RestoreConfig(); cerr != nil {
		l.addStep(res, "config restore error: %v", cerr)
	} else {
		res.ConfigRestored = n > 0
		l.addStep(res, "config restore: %d keys", n)
	}
	if ok, _ = l.Probe(ctx, "after-config"); err == nil && ok {
		res.Success = true
		res.Cause = "config"
		l.addStep(res, "post-config probe: reachable")
		l.finish(res)
		return res
	}
	l.addStep(res, "post-config probe: unreachable")
	res.QuickChatOK = false
	l.addStep(res, "ladder exhausted")

	l.finish(res)
	return res
}

func (l *Ladder) addStep(res *Result, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	res.Steps = append(res.Steps, msg)
	if l.Log != nil {
		l.Log("[recovery] %s", msg)
	}
}

func (l *Ladder) finish(res *Result) {
	res.QuickChatOK = res.Success
	if l.ResultFile != "" {
		if err := SaveResult(l.ResultFile, res); err != nil {
			if l.Log != nil {
				l.Log("[recovery] save result: %v", err)
			}
		}
	}
}

// SaveResult 把恢复结论持久化到文件（guard 读取）。
func SaveResult(path string, r *Result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadResult 读回恢复结论。
func LoadResult(path string) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ResultPath 返回 <dataDir>/recovery/recovery_result.json。
func ResultPath(dataDir string) string {
	return filepath.Join(dataDir, "recovery", "recovery_result.json")
}

// SortSteps 供测试/展示封装（保序打印）。
func SortSteps(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}

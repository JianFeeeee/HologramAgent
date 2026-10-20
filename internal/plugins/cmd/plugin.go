package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

// isWindows 缓存运行时检测结果
var isWindows = runtime.GOOS == "windows"

// shellUnquote 拆解命令字符串，处理单引号/双引号包裹的参数
func shellUnquote(s string) []string {
	var args []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

func init() {
	plugin.RegisterPluginMeta("cmd", "命令执行", "Command")
	plugin.RegisterFactory("cmd", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type cmdRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Command   string    `json:"command"`
	Stdout    string    `json:"stdout"`
	Stderr    string    `json:"stderr"`
	ExitCode  int       `json:"exit_code"`
	Workdir   string    `json:"workdir"`
	Timeout   string    `json:"timeout"`
	Duration  string    `json:"duration"`
	Status    string    `json:"status"`
}

const maxHistory = 100

type Plugin struct {
	name           string
	defaultTimeout string
	maxOutput      int
	mu             sync.Mutex
	history        []cmdRecord
}

func New(name string) *Plugin {
	return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "default_timeout", Type: "string", DisplayName: "默认命令超时",
		Description: "命令执行的默认超时时间，例如 30s, 1m, 5m（默认 30s）",
		Default:     "30s",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "max_output_bytes", Type: "int", DisplayName: "最大输出字节数",
		Description: "命令输出的最大字节数，超出部分将被截断（默认 32000）",
		Default:     "32000",
	})
	p.maxOutput = 32000
	if v, _ := s.Settings().Get("max_output_bytes"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if n, err := fmt.Sscanf(s, "%d", &p.maxOutput); err == nil && n > 0 {
			}
		}
	}
	p.defaultTimeout = "30s"
	if v, _ := s.Settings().Get("default_timeout"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			p.defaultTimeout = s
		}
	}

	s.RegisterTool("cmd_run", sdk.ToolDef{
		Name:        "cmd_run",
		Description: "执行一条系统命令并返回输出。适用于查询系统信息、运行脚本、操作文件等单次命令场景。经完整 bash 执行，支持管道、分号、&&、命令替换、heredoc、重定向；不支持交互（vim/ssh/top 这类需要持续输入的程序请用 terminal_create，不要硬等它退出）。",
		NoMemory:    true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "要执行的命令",
				},
				"timeout": map[string]interface{}{
					"type":        "string",
					"description": "超时时间，例如 10s, 1m, 30s（默认 30s）",
				},
				"workdir": map[string]interface{}{
					"type":        "string",
					"description": "工作目录（可选，默认由 core.agent.workdir 配置决定）",
				},
			},
			"required": []string{"command"},
		},
		// 执行体只依赖入参；唯一共享 p.history 由 recordCmd 加 p.mu 保护
		ParallelSafe: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return map[string]interface{}{"error": "command is required"}, nil
		}

		timeoutStr, _ := args["timeout"].(string)
		if timeoutStr == "" {
			timeoutStr = p.defaultTimeout
		}
		timeout, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("invalid timeout %q: %v", timeoutStr, err)}, nil
		}

		workdir, _ := args["workdir"].(string)
		if workdir == "" {
			if sett := s.Settings(); sett != nil {
				if v, _ := sett.GetCore("core.agent.workdir"); v != nil {
					if str, ok := v.(string); ok && str != "" {
						workdir = str
					}
				}
			}
		}

		tStart := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Linux/Unix 经 /bin/sh -c 执行完整 shell 语法：管道、分号、&&、
		// 命令替换、heredoc、重定向全部可用。旧实现 shellUnquote 拆词后
		// 直接 exec，导致 pwd; ls / 变成执行名为 "pwd;" 的程序（exit -1）、
		// heredoc 被截断——agent 多次反馈"命令解析奇怪"即此。
		var cmd *exec.Cmd
		if isWindows {
			execCmd := "chcp 65001>nul & " + command
			cmd = exec.CommandContext(ctx, "cmd.exe", "/d", "/c", execCmd)
		} else {
			shell := "/bin/sh"
			if _, err := os.Stat("/bin/bash"); err == nil {
				shell = "/bin/bash" // bash 支持更完整的语法（数组、[[ ]] 等）
			}
			cmd = exec.CommandContext(ctx, shell, "-c", command)
		}
		if workdir != "" {
			cmd.Dir = workdir
		}

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		rec := cmdRecord{Timestamp: tStart, Command: command, Workdir: workdir, Timeout: timeoutStr}
		exitCode := -1

		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				rec.Status = "timeout"
				rec.Stdout = p.truncateOutput(stdout.String())
				rec.Stderr = p.truncateOutput(stderr.String())
				rec.Duration = time.Since(tStart).Round(time.Millisecond).String()
				p.recordCmd(rec)
				return map[string]interface{}{
					"status": "timeout",
					"stdout": rec.Stdout,
					"stderr": rec.Stderr,
					"error":  fmt.Sprintf("命令执行超时（%s）", timeoutStr),
				}, nil
			}
		}
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}

		rec.Status = "ok"
		rec.Stdout = p.truncateOutput(stdout.String())
		rec.Stderr = p.truncateOutput(stderr.String())
		rec.ExitCode = exitCode
		rec.Duration = time.Since(tStart).Round(time.Millisecond).String()
		p.recordCmd(rec)

		return map[string]interface{}{
			"status":    "ok",
			"stdout":    rec.Stdout,
			"stderr":    rec.Stderr,
			"exit_code": exitCode,
			"command":   command,
		}, nil
	})

	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		if len(ctx.ToolCalls) == 0 {
			return nil
		}
		tc := ctx.ToolCalls[0]
		if tc.Name != "cmd_run" {
			return nil
		}
		command, _ := tc.Arguments["command"].(string)
		if command == "" {
			return nil
		}
		if blocked, reason := isDangerousCommand(command); blocked {
			ctx.Lock()
			ctx.Response = &reason
			ctx.Unlock()
			return nil
		}
		return nil
	}, sdk.StageScopeOwnTools)

	return nil
}

func (p *Plugin) Stop() error {
	return nil
}

func (p *Plugin) recordCmd(r cmdRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.history = append(p.history, r)
	if len(p.history) > maxHistory {
		p.history = p.history[len(p.history)-maxHistory:]
	}
}

func (p *Plugin) truncateOutput(s string) string {
	maxLen := p.maxOutput
	if maxLen <= 0 {
		maxLen = 32000
	}
	if len(s) > maxLen {
		return s[:maxLen] + fmt.Sprintf("\n... [输出被截断，共 %d 字节]", len(s))
	}
	return strings.TrimRight(s, "\n")
}

func isDangerousCommand(command string) (bool, string) {
	pid := os.Getpid()
	pidStr := fmt.Sprintf("%d", pid)
	lower := strings.ToLower(command)

	selfTargets := []string{"homed", pidStr}
	killCmds := []string{"kill ", "killall ", "pkill ", "kill -", "kill -9"}
	serviceCmds := []string{
		"systemctl stop homeagent",
		"systemctl restart homeagent",
		"systemctl kill homeagent",
		"service homeagent stop",
		"service homeagent restart",
	}

	for _, kp := range killCmds {
		if !strings.Contains(lower, kp) {
			continue
		}
		for _, t := range selfTargets {
			if strings.Contains(lower, strings.ToLower(t)) {
				return true, fmt.Sprintf("命令被拦截：禁止向 homed 进程(pid=%s)发送信号", pidStr)
			}
		}
	}

	for _, sc := range serviceCmds {
		if strings.Contains(lower, sc) {
			return true, fmt.Sprintf("命令被拦截：禁止操作 homed 服务进程(pid=%s)", pidStr)
		}
	}

	return false, ""
}

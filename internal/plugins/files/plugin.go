package files

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/system"
)

func init() {
	plugin.RegisterPluginMeta("files", "文件系统", "Files")
	plugin.RegisterFactory("files", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name     string
	sdk      *sdk.PluginSDK
	mu       sync.RWMutex
	filesDir string
	baseDir  string // L0 写前留档根目录（<data>/file_baseline 的父目录），空则禁用
}

var isWindowsBuild = runtime.GOOS == "windows"

func New(name string) *Plugin {
	return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "dir",
		Default:     "/",
		Type:        "string",
		DisplayName: "文件系统根目录",
		Description: "文件操作允许访问的根目录（设为 / 表示完整主机文件系统）",
		Category:    "files",
	})

	dir := getSetting[string](s.Settings(), "dir", "/")
	if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve files.dir: %w", err)
	}
	p.filesDir = abs

	// L0 写前留档：主 agent 写 /etc 等受保护路径前自动存档原文（homed 注入 <data>）
	if cfg := s.Config().Get(); cfg != nil {
		if dd := cfg.Daemon.DataDir; dd != "" {
			p.baseDir = dd
		}
	}
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "baseline_dir",
		Default:     p.baseDir,
		Type:        "string",
		DisplayName: "写前留档目录",
		Description: "写受保护系统路径前自动存档原文的目录（空禁用）",
		Category:    "files",
	})
	if v, err := s.Settings().Get("baseline_dir"); err == nil && v != nil {
		if s, ok := v.(string); ok && s != "" {
			p.baseDir = s
		}
	}
	if p.baseDir != "" {
		log.Printf("[%s] write-ahead baseline dir: %s", p.name, system.FileBaselineDir(p.baseDir))
	}

	tp := p.name + "_"

	s.RegisterTool(tp+"read", sdk.ToolDef{
		Name:        tp + "read",
		Description: fmt.Sprintf("读取文件内容。支持 offset/limit 分段读取大文件。沙箱路径: %s", p.filesDir),
		NoMemory:    false,
		Cleaner: func(output string) string {
			var r struct{ Content string }
			if err := json.Unmarshal([]byte(output), &r); err != nil {
				return output
			}
			return r.Content
		},
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":   map[string]interface{}{"type": "string", "description": "文件路径（绝对路径或相对于沙箱的路径）"},
				"offset": map[string]interface{}{"type": "integer", "description": "起始行号（从1开始，可选，默认1）"},
				"limit":  map[string]interface{}{"type": "integer", "description": "最多返回的行数（可选，默认全部）"},
			},
			"required": []string{"path"},
		},
	}, p.handleRead)

	s.RegisterTool(tp+"write", sdk.ToolDef{
		Name:        tp + "write",
		Description: fmt.Sprintf("写入文件（原子替换：先写临时副本再 rename，不会留下半截文件；覆盖受保护系统路径前自动留档原文）。自动创建父目录。生成多行内容直接传全文，不要用 echo 拼接。沙箱路径: %s", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]interface{}{"type": "string", "description": "文件路径"},
				"content": map[string]interface{}{"type": "string", "description": "要写入的内容"},
				"mode":    map[string]interface{}{"type": "string", "description": "写入模式: overwrite（覆盖，默认）| append（追加到末尾）| insert（插入到指定行）| create（创建新文件，已存在则报错）"},
				"line":    map[string]interface{}{"type": "integer", "description": "插入模式时的目标行号（从1开始），内容将插入到该行之前"},
			},
			"required": []string{"path", "content"},
		},
	}, p.handleWrite)

	s.RegisterTool(tp+"edit", sdk.ToolDef{
		Name:        tp + "edit",
		Description: fmt.Sprintf("对文件执行精确字符串替换。每个 old 必须在原文中唯一匹配。沙箱路径: %s", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "文件路径"},
				"edits": map[string]interface{}{
					"type":        "array",
					"description": "一个或多个替换操作。每个 old 必须在原文中恰好出现一次。不要包含重叠的 edit。",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"old": map[string]interface{}{"type": "string", "description": "要查找的原文（必须在文件中唯一）"},
							"new": map[string]interface{}{"type": "string", "description": "替换后的文本"},
						},
						"required": []string{"old", "new"},
					},
				},
			},
			"required": []string{"path", "edits"},
		},
	}, p.handleEdit)

	s.RegisterTool(tp+"ls", sdk.ToolDef{
		Name:        tp + "ls",
		Description: fmt.Sprintf("列出目录内容。目录以 / 后缀标记。沙箱路径: %s", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":  map[string]interface{}{"type": "string", "description": "目录路径（可选，默认为沙箱根目录）"},
				"limit": map[string]interface{}{"type": "integer", "description": "最多返回条目数（可选，默认500）"},
			},
		},
	}, p.handleLs)

	log.Printf("[%s] started, sandbox: %s", p.name, p.filesDir)
	return nil
}

func (p *Plugin) Stop() error {
	log.Printf("[%s] stopped", p.name)
	return nil
}

func (p *Plugin) resolvePath(userPath string) (string, error) {
	if userPath == "" {
		userPath = "."
	}
	if !filepath.IsAbs(userPath) {
		userPath = filepath.Join(p.filesDir, userPath)
	}
	abs, err := filepath.Abs(userPath)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	base := filepath.Clean(p.filesDir)
	if !pathWithinSandbox(abs, base) {
		return "", fmt.Errorf("path outside sandbox: %s", userPath)
	}
	return abs, nil
}

// pathWithinSandbox 判断 abs 是否位于沙箱 base 之内。
// Windows 文件系统大小写不敏感，且卷根目录（如 C:\）应放行全盘路径。
func pathWithinSandbox(abs, base string) bool {
	lower := func(s string) string {
		if isWindowsBuild {
			return strings.ToLower(s)
		}
		return s
	}
	abs = filepath.Clean(abs)
	base = filepath.Clean(base)
	if equalFoldPath(abs, base) {
		return true
	}
	// 根沙箱（Linux "/"）表示整机可访问：Clean("") 会返回 "."，
	// 而 prefix 变成 "//" 导致所有绝对路径误判逃逸（生产实锤：files.dir=/ 时
	// 全部 files_read/write 报 path outside sandbox）。根目录直接放行。
	if base == string(filepath.Separator) {
		return true
	}
	// 卷根沙箱（C:\、D:\ 等）表示整机可访问
	if isWindowsBuild && len(base) == 3 && base[1] == ':' && base[2] == '\\' {
		return true
	}
	prefix := lower(base) + string(filepath.Separator)
	return strings.HasPrefix(lower(abs), prefix)
}

func equalFoldPath(a, b string) bool {
	if isWindowsBuild {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func (p *Plugin) handleRead(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: " + path), nil
		}
		return errorResult("stat error: " + err.Error()), nil
	}
	if info.IsDir() {
		return errorResult("is a directory, use ls instead: " + path), nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return errorResult("read error: " + err.Error()), nil
	}

	text := string(data)
	lines := strings.Split(text, "\n")
	totalLines := len(lines)

	offset := 0
	if v, ok := args["offset"].(float64); ok && v > 0 {
		offset = int(v) - 1
	}
	if offset >= totalLines {
		return errorResult(fmt.Sprintf("offset %d exceeds file length (%d lines)", offset+1, totalLines)), nil
	}

	limit := totalLines - offset
	if v, ok := args["limit"].(float64); ok && v > 0 {
		if int(v) < limit {
			limit = int(v)
		}
	}

	end := offset + limit
	if end > totalLines {
		end = totalLines
	}

	selected := lines[offset:end]
	output := strings.Join(selected, "\n")

	var sb strings.Builder
	sb.WriteString(output)

	if end < totalLines {
		nextOffset := end + 1
		sb.WriteString(fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", offset+1, end, totalLines, nextOffset))
	} else if offset > 0 {
		sb.WriteString(fmt.Sprintf("\n\n[%d lines total]", totalLines))
	}

	return map[string]interface{}{
		"content": sb.String(),
		"size":    len(data),
		"lines":   totalLines,
	}, nil
}

func (p *Plugin) handleWrite(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}
	content, _ := args["content"].(string)
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "overwrite"
	}

	line := 0
	if v, ok := args["line"].(float64); ok && v > 0 {
		line = int(v)
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	// L0 写前留档：覆盖已有受保护文件前，原文存档供 guard 还原。
	if mode != "create" {
		if archived, aerr := p.archiveBeforeWrite(absPath); aerr != nil {
			log.Printf("[%s] write-ahead archive %s: %v", p.name, absPath, aerr)
		} else if archived {
			log.Printf("[%s] write-ahead archived %s", p.name, absPath)
		}
	}

	switch mode {
	case "create":
		if _, err := os.Stat(absPath); err == nil {
			return errorResult("file already exists: " + path), nil
		}
		dir := filepath.Dir(absPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errorResult("mkdir error: " + err.Error()), nil
		}
		if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
			return errorResult("write error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Created %s (%d bytes)", path, len(content)),
		}, nil

	case "append":
		dir := filepath.Dir(absPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errorResult("mkdir error: " + err.Error()), nil
		}
		f, err := os.OpenFile(absPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return errorResult("open error: " + err.Error()), nil
		}
		defer f.Close()
		if _, err := f.WriteString(content); err != nil {
			return errorResult("append error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Appended %d bytes to %s", len(content), path),
		}, nil

	case "insert":
		if line < 1 {
			return errorResult("line must be >= 1 for insert mode"), nil
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			if os.IsNotExist(err) {
				return errorResult("file not found: " + path), nil
			}
			return errorResult("read error: " + err.Error()), nil
		}
		lines := strings.Split(string(data), "\n")
		if line > len(lines)+1 {
			return errorResult(fmt.Sprintf("line %d exceeds file length (%d lines)", line, len(lines))), nil
		}
		idx := line - 1
		newLines := make([]string, 0, len(lines)+1)
		newLines = append(newLines, lines[:idx]...)
		newLines = append(newLines, content)
		newLines = append(newLines, lines[idx:]...)
		result := strings.Join(newLines, "\n")
		if err := os.WriteFile(absPath, []byte(result), 0644); err != nil {
			return errorResult("write error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Inserted %d bytes at line %d in %s", len(content), line, path),
		}, nil

	default: // overwrite
		dir := filepath.Dir(absPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errorResult("mkdir error: " + err.Error()), nil
		}
		if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
			return errorResult("write error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Wrote %d bytes to %s", len(content), path),
		}, nil
	}
}

func (p *Plugin) handleEdit(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	// L0 写前留档（edit 可能覆盖已存在文件）
	if archived, aerr := p.archiveBeforeWrite(absPath); aerr != nil {
		log.Printf("[%s] write-ahead archive %s: %v", p.name, absPath, aerr)
	} else if archived {
		log.Printf("[%s] write-ahead archived %s", p.name, absPath)
	}

	rawEdits, ok := args["edits"].([]interface{})
	if !ok || len(rawEdits) == 0 {
		return errorResult("edits must be a non-empty array"), nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: " + path), nil
		}
		return errorResult("read error: " + err.Error()), nil
	}

	original := string(data)
	content := original
	applied := 0
	var errors []string

	for i, raw := range rawEdits {
		edit, ok := raw.(map[string]interface{})
		if !ok {
			errors = append(errors, fmt.Sprintf("edit[%d]: invalid format", i))
			continue
		}
		oldText, _ := edit["old"].(string)
		newText, _ := edit["new"].(string)
		if oldText == "" {
			errors = append(errors, fmt.Sprintf("edit[%d]: old is required", i))
			continue
		}

		count := strings.Count(content, oldText)
		if count == 0 {
			errors = append(errors, fmt.Sprintf("edit[%d]: could not find %q in %s", i, oldText, path))
			continue
		}
		if count > 1 {
			errors = append(errors, fmt.Sprintf("edit[%d]: found %d occurrences of %q, must be unique", i, count, oldText))
			continue
		}

		content = strings.Replace(content, oldText, newText, 1)
		applied++
	}

	if applied == 0 {
		msg := "no edits applied"
		if len(errors) > 0 {
			msg += ": " + strings.Join(errors, "; ")
		}
		return errorResult(msg), nil
	}

	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return errorResult("write error: " + err.Error()), nil
	}

	msg := fmt.Sprintf("Successfully applied %d/%d edits to %s", applied, len(rawEdits), path)
	if len(errors) > 0 {
		msg += "\nWarnings:\n" + strings.Join(errors, "\n")
	}

	return map[string]interface{}{
		"content": msg,
	}, nil
}

func (p *Plugin) handleLs(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("path not found: " + path), nil
		}
		return errorResult("stat error: " + err.Error()), nil
	}
	if !info.IsDir() {
		return errorResult("not a directory: " + path), nil
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return errorResult("readdir error: " + err.Error()), nil
	}

	limit := 500
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	var lines []string
	entryLimitReached := false
	for i, entry := range entries {
		if i >= limit {
			entryLimitReached = true
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		info, err := entry.Info()
		if err == nil {
			name = fmt.Sprintf("%-40s %8d", name, info.Size())
		}
		lines = append(lines, name)
	}

	if len(lines) == 0 {
		return map[string]interface{}{
			"content": "(empty directory)",
		}, nil
	}

	output := strings.Join(lines, "\n")
	if entryLimitReached {
		output += fmt.Sprintf("\n\n[%d entries limit reached. Use limit=N for more.]", limit)
	}

	return map[string]interface{}{
		"content": output,
	}, nil
}

func errorResult(msg string) map[string]interface{} {
	return map[string]interface{}{
		"isError": true,
		"content": msg,
	}
}

// archiveBeforeWrite L0 写前留档：若目标为受保护系统路径且已存在，则存档原文。
func (p *Plugin) archiveBeforeWrite(absPath string) (bool, error) {
	if p.baseDir == "" {
		return false, nil
	}
	return system.ArchiveBeforeWrite(p.baseDir, absPath)
}

func getSetting[T any](s sdk.SettingsAPI, key string, def T) T {
	v, err := s.Get(key)
	if err != nil || v == nil {
		return def
	}
	val, ok := v.(T)
	if !ok {
		return def
	}
	return val
}

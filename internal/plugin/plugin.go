package plugin

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

type PluginType string

const (
	PluginTypeSKILL PluginType = "skill"
)

type IOConfig struct {
	Type        string   `json:"type"`
	InputRoute  string   `json:"input_route"`
	OutputRoute string   `json:"output_route"`
	OutputCaps  []string `json:"output_caps"`
}

type Plugin interface {
	Name() string
	PluginType() PluginType
	Description() string
	Version() string
	Tools() []ToolDef
	Enabled() bool
	SetEnabled(bool)
	IOConfig() *IOConfig
	RawContent() string
}

type ToolDef = agentIO.ToolDef

type SKILLPlugin struct {
	mu          sync.RWMutex
	name        string
	description string
	version     string
	author      string
	enabled     bool
	rawContent  string
	sourceDir   string
	toolDefs    []ToolDef
	ioConfig    *IOConfig
}

func LoadSKILL(path string) (*SKILLPlugin, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	name := filepath.Base(path)
	p := &SKILLPlugin{
		name:      name,
		sourceDir: path,
		enabled:   true,
	}

	if info.IsDir() {
		skillFile := filepath.Join(path, "SKILL.md")
		if data, err := os.ReadFile(skillFile); err == nil {
			p.rawContent = string(data)
			p.description = extractDescription(p.rawContent)
			p.version = extractField(p.rawContent, "version")
			p.author = extractField(p.rawContent, "author")
			p.ioConfig = extractIOConfig(p.rawContent)
		}

		metaFile := filepath.Join(path, "skill.json")
		if data, err := os.ReadFile(metaFile); err == nil {
			var meta struct {
				Name        string    `json:"name"`
				Description string    `json:"description"`
				Version     string    `json:"version"`
				Author      string    `json:"author"`
				IO          *IOConfig `json:"io,omitempty"`
			}
			if err := json.Unmarshal(data, &meta); err == nil {
				if meta.Name != "" {
					p.name = meta.Name
				}
				if meta.Description != "" {
					p.description = meta.Description
				}
				if meta.Version != "" {
					p.version = meta.Version
				}
				if meta.Author != "" {
					p.author = meta.Author
				}
				if meta.IO != nil {
					p.ioConfig = meta.IO
				}
			}
		}
	} else if filepath.Ext(path) == ".md" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		p.rawContent = string(data)
		p.name = name[:len(name)-3]
		p.description = extractDescription(p.rawContent)
		p.ioConfig = extractIOConfig(p.rawContent)
	}

	if p.rawContent != "" {
		p.toolDefs = extractToolDefs(p.rawContent)
	}

	log.Printf("[plugin] loaded SKILL: %s v%s", p.name, p.version)
	return p, nil
}

func (p *SKILLPlugin) Name() string           { return p.name }
func (p *SKILLPlugin) PluginType() PluginType { return PluginTypeSKILL }
func (p *SKILLPlugin) Description() string    { return p.description }
func (p *SKILLPlugin) Version() string        { return p.version }
func (p *SKILLPlugin) Enabled() bool          { p.mu.RLock(); defer p.mu.RUnlock(); return p.enabled }
func (p *SKILLPlugin) SetEnabled(v bool)      { p.mu.Lock(); defer p.mu.Unlock(); p.enabled = v }
func (p *SKILLPlugin) Tools() []ToolDef       { return p.toolDefs }
func (p *SKILLPlugin) IOConfig() *IOConfig    { return p.ioConfig }
func (p *SKILLPlugin) RawContent() string     { return p.rawContent }
func (p *SKILLPlugin) SourceDir() string      { return p.sourceDir }

// ValidateSKILLContent 校验 SKILL.md 内容是否可被 LoadSKILL 正确解析：
// 必须含非空正文（description 来源），且提取出的工具定义名称合法。
// 供 skillmgr 的 skill_create 在落盘前校验生成结果。
func ValidateSKILLContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("SKILL content is empty")
	}
	if extractDescription(content) == "" {
		return fmt.Errorf("SKILL content has no description (first non-empty non-heading line required)")
	}
	for _, td := range extractToolDefs(content) {
		if strings.TrimSpace(td.Name) == "" || strings.ContainsAny(td.Name, " \t\n/") {
			return fmt.Errorf("invalid tool name in SKILL content: %q", td.Name)
		}
	}
	return nil
}

func extractDescription(content string) string {
	inFrontmatter := false
	for i, line := range splitLines(content) {
		trimmed := trimSpace(line)
		// 跳过 YAML frontmatter 块（首行 --- 至闭合 ---），
		// 否则分隔符会被误认为描述（所有带 frontmatter 的 SKILL.md 描述都变成 "---"）
		if i == 0 && trimmed == "---" {
			inFrontmatter = true
			continue
		}
		if inFrontmatter {
			if trimmed == "---" {
				inFrontmatter = false
			}
			continue
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return trimmed
		}
	}
	return ""
}

func extractField(content string, field string) string {
	prefix := field + ":"
	lowerPrefix := strings.ToLower(prefix)
	for _, line := range splitLines(content) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), lowerPrefix) {
			for i := 0; i < len(trimmed); i++ {
				if trimmed[i] == ':' {
					v := strings.TrimSpace(trimmed[i+1:])
					// YAML 风格引号值：剥掉成对的首尾引号
					if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
						v = v[1 : len(v)-1]
					}
					return v
				}
			}
		}
	}
	return ""
}

func extractToolDefs(content string) []ToolDef {
	lines := splitLines(content)
	var defs []ToolDef
	var currentTool *ToolDef
	inCodeBlock := false

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		if strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### ") {
			if currentTool != nil && currentTool.Name != "" {
				defs = append(defs, *currentTool)
			}
			currentTool = &ToolDef{}
			namePart := strings.TrimPrefix(trimmed, "## ")
			if isNonToolSection(namePart) {
				currentTool = nil
				continue
			}
			currentTool.Name = namePart
			currentTool.Parameters = map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			}
			continue
		}

		if strings.HasPrefix(trimmed, "### Tool: ") {
			if currentTool != nil && currentTool.Name != "" {
				defs = append(defs, *currentTool)
			}
			currentTool = &ToolDef{}
			namePart := strings.TrimPrefix(trimmed, "### Tool: ")
			currentTool.Name = namePart
			currentTool.Parameters = map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			}
			continue
		}

		if currentTool == nil || currentTool.Name == "" {
			continue
		}

		if currentTool.Description == "" && trimmed != "" &&
			!strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "#") {
			currentTool.Description = trimmed
			continue
		}

		if strings.HasPrefix(trimmed, "- ") {
			paramLine := strings.TrimPrefix(trimmed, "- ")
			colonIdx := strings.Index(paramLine, ":")
			if colonIdx > 0 {
				paramName := strings.TrimSpace(paramLine[:colonIdx])
				paramDesc := strings.TrimSpace(paramLine[colonIdx+1:])
				if paramName != "" {
					props := currentTool.Parameters["properties"].(map[string]interface{})
					props[paramName] = map[string]interface{}{
						"type":        "string",
						"description": paramDesc,
					}
				}
			}
		}
	}

	if currentTool != nil && currentTool.Name != "" {
		defs = append(defs, *currentTool)
	}
	return defs
}

func isNonToolSection(name string) bool {
	lower := strings.ToLower(name)
	skip := []string{
		"tools", "usage", "examples", "installation", "setup",
		"configuration", "overview", "description", "notes",
		"parameters", "return", "returns", "options", "syntax",
	}
	for _, s := range skip {
		if lower == s || strings.HasPrefix(lower, s+" ") || strings.HasPrefix(lower, s+":") {
			return true
		}
	}
	return false
}

func extractIOConfig(content string) *IOConfig {
	ioType := extractField(content, "io_type")
	if ioType == "" {
		return nil
	}
	cfg := &IOConfig{
		Type:       ioType,
		InputRoute: extractField(content, "io_input_route"),
	}
	if cfg.InputRoute == "" {
		cfg.InputRoute = extractField(content, "io_route")
	}
	cfg.OutputRoute = extractField(content, "io_output_route")
	if cfg.OutputRoute == "" {
		cfg.OutputRoute = cfg.InputRoute
	}
	capsStr := extractField(content, "io_output_caps")
	if capsStr != "" {
		for _, c := range strings.Split(capsStr, ",") {
			cfg.OutputCaps = append(cfg.OutputCaps, strings.TrimSpace(c))
		}
	}
	return cfg
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if i > start {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	return lines
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// KeepTypes 通过引用确保编译器不丢弃类型
var _ = []interface{}{
	PluginTypeSKILL,
}

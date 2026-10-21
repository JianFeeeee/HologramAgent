package skillmgr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// registerTools 注册 LLM 可调用的 skill 管理工具。
func (p *Plugin) registerTools() {
	p.registerList()
	p.registerInfo()
	p.registerLoad()
	p.registerUnload()
	p.registerEnable()
	p.registerDisable()
	p.registerCreate()
	p.registerExport()
	p.registerInstall()
}

const tp = "skill_"

func (p *Plugin) registerList() {
	p.sdk.RegisterTool(tp+"list", sdk.ToolDef{
		Name:        tp + "list",
		Description: "列出所有已加载的原生技能（native skill）：名称、版本、启用状态、描述。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		entries := p.snapshot()
		if len(entries) == 0 {
			return "当前没有已加载的原生技能。", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "原生技能 (%d):\n", len(entries))
		for _, e := range entries {
			state := "启用"
			if !e.enabled {
				state = "禁用"
			}
			fmt.Fprintf(&b, "  %s v%s [%s] - %s\n",
				e.sk.Name(), e.sk.Version(), state, e.sk.Description())
		}
		return b.String(), nil
	})
}

func (p *Plugin) registerInfo() {
	p.sdk.RegisterTool(tp+"info", sdk.ToolDef{
		Name:        tp + "info",
		Description: "查看指定技能的详细信息与 SKILL.md 全文。执行技能前必读：按文档指示操作。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "技能名称"},
			},
			"required": []string{"name"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		e, ok := p.get(name)
		if !ok {
			return nil, fmt.Errorf("skill %q not loaded; use skill_list to view available skills", name)
		}
		state := "启用"
		if !e.enabled {
			state = "禁用"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "名称: %s\n版本: %s\n状态: %s\n路径: %s\n描述: %s\n",
			e.sk.Name(), e.sk.Version(), state, e.path, e.sk.Description())
		if tools := e.sk.Tools(); len(tools) > 0 {
			b.WriteString("文档中声明的操作步骤:\n")
			for _, td := range tools {
				fmt.Fprintf(&b, "  - %s: %s\n", td.Name, td.Description)
			}
		}
		fmt.Fprintf(&b, "\n--- SKILL.md ---\n%s", e.sk.RawContent())
		return b.String(), nil
	})
}

func (p *Plugin) registerLoad() {
	p.sdk.RegisterTool(tp+"load", sdk.ToolDef{
		Name:        tp + "load",
		Description: "从 skills 目录热加载指定技能（目录名或单 .md 文件名），加载后立即可用。skill_create 填充完内容后调用此工具生效。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "skills 目录下的条目名（不含路径）"},
			},
			"required": []string{"name"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
			return nil, fmt.Errorf("invalid skill entry name: %q", name)
		}
		path := filepath.Join(p.skillsDir, name)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("not found in skills dir: %s", path)
		}
		e, err := p.loadOne(path)
		if err != nil {
			return nil, err
		}
		return fmt.Sprintf("已加载技能 %s v%s", e.sk.Name(), e.sk.Version()), nil
	})
}

func (p *Plugin) registerUnload() {
	name := tp + "unload"
	p.sdk.RegisterTool(name, sdk.ToolDef{
		Name:        name,
		Description: "卸载指定技能（仅从内存移除；磁盘文件保留。需删除文件用参数 delete_files）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":         map[string]interface{}{"type": "string", "description": "技能名称"},
				"delete_files": map[string]interface{}{"type": "boolean", "description": "是否同时删除磁盘文件（默认 false）"},
			},
			"required": []string{"name"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		delFiles, _ := args["delete_files"].(bool)
		e, ok := p.get(name)
		if !ok {
			return nil, fmt.Errorf("skill %q not loaded", name)
		}
		path := e.path
		if !p.removeOne(name) {
			return nil, fmt.Errorf("skill %q not loaded", name)
		}
		msg := fmt.Sprintf("已卸载技能 %s（内存）", name)
		if delFiles {
			if err := os.RemoveAll(path); err != nil {
				return msg + fmt.Sprintf("，但删除文件失败: %v", err), nil
			}
			msg += "，磁盘文件已删除"
		}
		return msg, nil
	})
}

func (p *Plugin) registerEnable() { p.registerSetEnabled(true) }
func (p *Plugin) registerDisable() {
	p.registerSetEnabled(false)
}

func (p *Plugin) registerSetEnabled(v bool) {
	action := "disable"
	word := "禁用"
	if v {
		action = "enable"
		word = "启用"
	}
	name := tp + action
	desc := word + "指定技能。禁用后 agent 不再使用该技能（保留在列表中标记为禁用）。"
	p.sdk.RegisterTool(name, sdk.ToolDef{
		Name:        name,
		Description: desc,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "技能名称"},
			},
			"required": []string{"name"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		skillName, _ := args["name"].(string)
		p.mu.Lock()
		defer p.mu.Unlock()
		e, ok := p.skills[skillName]
		if !ok {
			return nil, fmt.Errorf("skill %q not loaded", skillName)
		}
		e.enabled = v
		e.sk.SetEnabled(v)
		return fmt.Sprintf("已%s技能 %s", word, skillName), nil
	})
}

func (p *Plugin) registerCreate() {
	name := tp + "create"
	p.sdk.RegisterTool(name, sdk.ToolDef{
		Name:        name,
		Description: "创建新技能骨架：生成标准 SKILL.md 模板写入 skills/<name>/。创建后在 content 参数填入完整文档内容并再次调用以落盘生效（两步式），或直接传 content 一步完成。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":        map[string]interface{}{"type": "string", "description": "技能名（小写字母/数字/连字符，如 weather-notify）"},
				"description": map[string]interface{}{"type": "string", "description": "一句话描述技能用途（模板模式的 frontmatter description）"},
				"content":     map[string]interface{}{"type": "string", "description": "可选。完整 SKILL.md 内容；提供时直接校验并写入，否则生成骨架模板"},
				"scripts":     map[string]interface{}{"type": "boolean", "description": "模板模式是否创建 scripts/ 目录占位（默认 true）"},
			},
			"required": []string{"name"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		description, _ := args["description"].(string)
		content, _ := args["content"].(string)
		withScripts := true
		if ws, ok := args["scripts"].(bool); ok {
			withScripts = ws
		}
		if err := validateSkillName(name); err != nil {
			return nil, err
		}

		dir := filepath.Join(p.skillsDir, name)

		// 两步式第二步 / 一步式：提供 content 则校验后直接写入
		if strings.TrimSpace(content) != "" {
			if err := plugin.ValidateSKILLContent(content); err != nil {
				return nil, fmt.Errorf("content 校验失败: %w", err)
			}
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, SkillFileName), []byte(content), 0644); err != nil {
				return nil, err
			}
			e, err := p.loadOne(dir)
			if err != nil {
				return nil, err
			}
			return fmt.Sprintf("技能 %s 已写入并加载生效（v%s）。用 skill_info 可查看全文。", e.sk.Name(), e.sk.Version()), nil
		}

		// 模板模式：生成骨架
		if fileExists(filepath.Join(dir, SkillFileName)) {
			return nil, fmt.Errorf("skill %s already exists at %s; use a different name or edit the file directly", name, dir)
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
		if withScripts {
			os.MkdirAll(filepath.Join(dir, "scripts"), 0755)
		}
		tpl := renderTemplate(name, description)
		if err := os.WriteFile(filepath.Join(dir, SkillFileName), []byte(tpl), 0644); err != nil {
			return nil, err
		}
		return fmt.Sprintf(
			"骨架已生成: %s\n下一步：读取该文件，按实际用途补全正文与操作步骤（## 步骤小节会被解析为技能动作说明），\n然后调用 skill_create 并传入完整 content 覆盖写入，最后自动加载生效。",
			filepath.Join(dir, SkillFileName)), nil
	})
}

func (p *Plugin) registerExport() {
	p.sdk.RegisterTool(tp+"export", sdk.ToolDef{
		Name:        tp + "export",
		Description: "导出指定技能为 .skm 分发包（tar.gz 格式），输出到 output 路径或默认 data/exports/ 下。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":   map[string]interface{}{"type": "string", "description": "技能名称"},
				"output": map[string]interface{}{"type": "string", "description": "输出 .skm 文件完整路径（可选，默认 data/exports/<name>.skm）"},
			},
			"required": []string{"name"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		output, _ := args["output"].(string)
		e, ok := p.get(name)
		if !ok {
			return nil, fmt.Errorf("skill %q not loaded; use skill_list first", name)
		}
		outPath := output
		if outPath == "" {
			exportsDir := filepath.Join(p.skillsDir, "..", "exports")
			os.MkdirAll(exportsDir, 0755)
			outPath = filepath.Join(exportsDir, name+PackExt)
		}
		n, err := packSkill(e.path, outPath)
		if err != nil {
			return nil, err
		}
		return fmt.Sprintf("已导出 %d 个文件到 %s", n, outPath), nil
	})
}

func (p *Plugin) registerInstall() {
	p.sdk.RegisterTool(tp+"install", sdk.ToolDef{
		Name:        tp + "install",
		Description: "安装技能包：支持 .skm 包路径或 local:<skills目录路径> 本地目录。同名技能已存在时传 overwrite=true 原地覆盖（保留无持久配置，直接替换文件）。安装后立即加载生效。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"source":    map[string]interface{}{"type": "string", "description": "安装来源：<path>.skm 或 local:<dir>"},
				"overwrite": map[string]interface{}{"type": "boolean", "description": "同名技能存在时覆盖更新（默认 false）"},
			},
			"required": []string{"source"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		source, _ := args["source"].(string)
		overwrite, _ := args["overwrite"].(bool)
		source = strings.TrimSpace(source)
		switch {
		case strings.HasPrefix(source, "local:"):
			dir := strings.TrimPrefix(source, "local:")
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				return nil, fmt.Errorf("local dir not found: %s", dir)
			}
			dstName := filepath.Base(dir)
			if err := validateSkillName(dstName); err != nil {
				return nil, err
			}
			dst := filepath.Join(p.skillsDir, dstName)
			if err := p.replaceSkillDir(dst, overwrite); err != nil {
				return nil, err
			}
			if err := copyDir(dir, dst); err != nil {
				return nil, fmt.Errorf("copy failed: %w", err)
			}
			e, err := p.loadOne(dst)
			if err != nil {
				os.RemoveAll(dst)
				return nil, fmt.Errorf("installed but load failed: %w", err)
			}
			return fmt.Sprintf("已从本地目录安装技能 %s v%s", e.sk.Name(), e.sk.Version()), nil

		case strings.HasSuffix(strings.ToLower(source), PackExt):
			if _, err := os.Stat(source); err != nil {
				return nil, fmt.Errorf("pack not found: %s", source)
			}
			dstName := strings.TrimSuffix(filepath.Base(source), PackExt)
			if err := validateSkillName(dstName); err != nil {
				return nil, err
			}
			dst := filepath.Join(p.skillsDir, dstName)
			if err := p.replaceSkillDir(dst, overwrite); err != nil {
				return nil, err
			}
			n, err := unpackSkill(source, dst)
			if err != nil {
				os.RemoveAll(dst)
				return nil, fmt.Errorf("unpack failed: %w", err)
			}
			e, err := p.loadOne(dst)
			if err != nil {
				os.RemoveAll(dst)
				return nil, fmt.Errorf("unpacked %d files but load failed: %w", n, err)
			}
			return fmt.Sprintf("已安装 %d 个文件，技能 %s v%s 生效", n, e.sk.Name(), e.sk.Version()), nil

		default:
			return nil, fmt.Errorf("unsupported source: %q (use <path>.skm or local:<dir>)", source)
		}
	})
}

// replaceSkillDir 安装前的同名目录处理：不存在则放行；存在且 overwrite=true
// 则先卸载旧实例并删除旧目录（skill 无持久配置，直接替换）；否则报错。
func (p *Plugin) replaceSkillDir(dst string, overwrite bool) error {
	if _, err := os.Stat(dst); err != nil {
		return nil // 不存在，直接装
	}
	if !overwrite {
		return fmt.Errorf("skill dir already exists: %s (传 overwrite=true 覆盖更新)", dst)
	}
	name := filepath.Base(dst)
	p.removeOne(name) // 从注册表移除旧实例
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("remove old skill dir: %w", err)
	}
	return nil
}

// validateSkillName 校验技能名：小写字母/数字/连字符，1-64 字符。
func validateSkillName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("invalid skill name length")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("invalid character %q in skill name %q (allow lowercase/digits/-/_/.)", r, name)
		}
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("skill name cannot start with dot")
	}
	return nil
}

// renderTemplate 生成 SKILL.md 骨架。
func renderTemplate(name, description string) string {
	if description == "" {
		description = "(TODO: 一句话描述本技能的用途)"
	}
	return fmt.Sprintf(`---
name: %s
version: 0.1.0
author: homeagent
---

# %s

%s

## 使用时机

(TODO: 描述什么场景下 agent 应当使用本技能)

## 操作步骤

### step-1

(TODO: 第一步做什么。首行非标题文本会作为该步骤的描述)

- param1: 参数1说明
- param2: 参数2说明

执行示例：

`+"```"+`bash
echo "hello from %s"
`+"```"+`

## 注意事项

- (TODO: 安全提示、失败重试策略等)
`, name, name, description, name)
}

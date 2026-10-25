package clawhubadapter

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

//go:embed manager/main.js
var managerSrc string

//go:embed pysimulator/main.py
var pySimulatorSrc string

//go:embed simulator/openclaw_cli.js
var openclawCliSrc string

func init() {
	plugin.RegisterPluginMeta("clawhubadapter", "ClawHub 适配器", "ClawHub Adapter")
	plugin.RegisterFactory("clawhubadapter", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		dir := ""
		if dataDir, ok := config["data_dir"].(string); ok && dataDir != "" {
			dir = filepath.Join(dataDir, "skills")
		}
		return New(name, dir), nil
	})
}

type Plugin struct {
	stopChOnce   sync.Once
	name         string
	skillsDir    string
	simulatorDir string
	// pythonBin 是运行 skill 子进程的解释器（配置 python_bin）。
	// 留空表示未配置：此时优先用 <skill>/.venv/bin/python，再退回系统 python3。
	pythonBin string
	// skillWorkdir 是 skill 子进程的工作目录（配置 skill_workdir）。
	// 留空表示用 skill 自身目录。
	skillWorkdir string
	sidecars     []*sidecarProcess
	manager      *sidecarProcess
	mu           sync.Mutex
	sdk          *sdk.PluginSDK
	dispatcher   *RegistryDispatcher
	httpClient   *http.Client

	stopCh   chan struct{}
	stopOnce sync.Once
}

// pluginSingleton 内核单例引用（Start 时设置），供 SendToChannel/ChannelSender 使用
var pluginSingleton *Plugin

func New(name, skillsDir string) *Plugin {
	sd := ""
	if skillsDir != "" {
		sd = filepath.Join(skillsDir, ".simulator")
	}
	return &Plugin{
		name:         name,
		stopCh:       make(chan struct{}),
		skillsDir:    skillsDir,
		simulatorDir: sd,
		dispatcher:   NewDispatcher(),
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	pluginSingleton = p

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "skills_dir", Type: "string", DisplayName: "Skill 加载目录",
		Description: "OpenClaw 技能加载目录路径（留空则使用默认路径）",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "simulator_dir", Type: "string", DisplayName: "模拟器工作目录",
		Description: "OpenClaw 模拟器工作目录路径（留空则使用默认路径）",
	})
	// ★ Python 解释器与 skill 工作目录（2026-10-25 新增）。
	//
	// 此前是硬编码猜测：先找 /usr/bin/python3，再找 /usr/local/bin/python3。
	// 后果是 skill 声明的依赖（httpx 等）装在哪个解释器里就决定了它能不能跑，
	// 而"猜"这件事根本没有正确解 —— 不同部署的 Python 布局完全不同。
	//
	// 现在：由配置**显式指定**，与 skills_dir/simulator_dir 同一套风格。
	// 留空时仍走按 skill 目录探测 .venv 的既有逻辑（向后兼容）。
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "python_bin", Type: "string", DisplayName: "Python 解释器",
		Description: "运行 skill 用的 Python 解释器绝对路径，例如 /opt/ha-py/bin/python。" +
			"留空则优先用 <skill 目录>/.venv/bin/python，再退回系统 python3。",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "skill_workdir", Type: "string", DisplayName: "Skill 工作目录",
		Description: "skill 子进程的工作目录（留空则用 skill 自身目录）。" +
			"需要固定相对路径读写时用它；不影响解释器选择。",
	})
	p.httpClient = &http.Client{Timeout: 30 * time.Second}
	if v, _ := s.Settings().Get("skills_dir"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			p.skillsDir = s
		}
	}
	if v, _ := s.Settings().Get("simulator_dir"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			p.simulatorDir = s
		}
	}
	if v, _ := s.Settings().Get("python_bin"); v != nil {
		if str, ok := v.(string); ok && str != "" {
			p.pythonBin = str
		}
	}
	if v, _ := s.Settings().Get("skill_workdir"); v != nil {
		if str, ok := v.(string); ok && str != "" {
			p.skillWorkdir = str
		}
	}
	// 默认目录：内核 data_dir 下 skills 目录（与配置 core.daemon.data_dir 对齐）
	if p.skillsDir == "" {
		if v, _ := s.Settings().GetCore("daemon.data_dir"); v != nil {
			if dir, ok := v.(string); ok && dir != "" {
				p.skillsDir = filepath.Join(dir, "skills")
			}
		}
	}
	if p.skillsDir == "" {
		p.skillsDir = filepath.Join("data", "skills")
	}
	if p.simulatorDir == "" {
		p.simulatorDir = filepath.Join(p.skillsDir, ".simulator")
	}

	// Launch OC plugin manager first (handles OC-format plugin installation and lifecycle)
	os.MkdirAll(p.skillsDir, 0755)
	if err := p.launchManager(s); err != nil {
		log.Printf("[clawhubadapter] launch manager: %v", err)
	}

	// Load existing plugins from skills dir
	if entries, err := os.ReadDir(p.skillsDir); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			skillPath := filepath.Join(p.skillsDir, entry.Name())
			subs, err := os.ReadDir(skillPath)
			if err != nil {
				continue
			}

			hasMainJS := false
			hasMainPy := false
			hasOCManifest := false
			hasOCPackage := false
			for _, f := range subs {
				switch f.Name() {
				case "main.js":
					hasMainJS = true
				case "main.py":
					hasMainPy = true
				case "openclaw.plugin.json":
					hasOCManifest = true
				case "package.json":
					hasOCPackage = hasOCExtensions(filepath.Join(skillPath, "package.json"))
				}
			}

			switch {
			case hasMainJS:
				if err := p.loadSidecar(s, skillPath, entry.Name()); err != nil {
					log.Printf("[clawhubadapter] sidecar %s: %v", entry.Name(), err)
				}
			case hasMainPy:
				if err := p.loadPySidecar(s, skillPath, entry.Name()); err != nil {
					// ★ main.py 不等于 JSON-RPC sidecar。ClawHub 上大量技能的
					//   main.py 是 **CLI 入口**（`python3 main.py <tool> '<json>'`），
					//   对它发 ping 必然超时（实测 bilibili / gaode 各等 5s）。
					//
					//   以前这里只打一行错误就完事 —— 启动日志一片红，而技能既没
					//   变成工具、也没交给 skillmgr，等于半死不活。
					//   现在：失败即降级为「纯指令技能」移交 skillmgr，让模型仍可
					//   按 SKILL 文档用命令跑它；日志改成说明而不是错误。
					log.Printf("[clawhubadapter] pysidecar %s not ready (%v); "+
						"treating as CLI-type skill, handing off to skillmgr", entry.Name(), err)
					p.publishSkillDetected(s, skillPath)
				}
			case hasOCManifest || hasOCPackage:
				log.Printf("[clawhubadapter] ocplugin %s handled by manager", entry.Name())
			default:
				// 纯 SKILL 类型：不属于 OpenClaw 兼容层，移交原生 skillmgr 管理。
				// skillmgr 启动时也会自行全扫（本插件启动更早的场景由事件补齐）。
				p.publishSkillDetected(s, skillPath)
			}
		}
	}

	// Register plugin management tools that talk to the manager (always, even if skills dir is empty)
	tp := p.name + "_"
	s.RegisterTool(tp+"plugin_install", sdk.ToolDef{
		Name:        tp + "plugin_install",
		Description: "安装插件。支持 npm: 前缀（npm 包）、clawhub: 前缀（ClawHub 市场，如 clawhub:openclaw-codex-app-server）。安装后立即可用。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"package": map[string]interface{}{"type": "string", "description": "插件包标识。npm:<pkg> 从 npm 安装，clawhub:<pkg> 从 ClawHub 市场安装"},
			},
			"required": []string{"package"},
		},
	}, p.handlePluginInstall)

	s.RegisterTool(tp+"plugin_uninstall", sdk.ToolDef{
		Name:        tp + "plugin_uninstall",
		Description: "卸载已安装的插件，支持所有类型（sidecar、skill、manager 插件、ClawHub 安装的插件）。会停止进程、删除目录并清理注册。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "要卸载的插件名称（目录名，如 my-plugin）"},
			},
			"required": []string{"name"},
		},
	}, p.handlePluginUninstall)

	s.RegisterTool(tp+"search", sdk.ToolDef{
		Name:        tp + "search",
		Description: "搜索 ClawHub 插件市场，查找可安装的插件。返回插件名称、描述和安装命令提示。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "description": "搜索关键词"},
			},
			"required": []string{"query"},
		},
	}, p.handleClawHubSearch)

	s.RegisterTool(tp+"list", sdk.ToolDef{
		Name:        tp + "list",
		Description: "列出所有已安装的 ClawHub 插件及其工具。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handlePluginList)

	s.RegisterTool(tp+"plugin_info", sdk.ToolDef{
		Name:        tp + "plugin_info",
		Description: "查看单个已安装插件的详细信息：类型（OC/sidecar/SKILL）、工具列表、关联通道及运行状态。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "插件名称（目录名）"},
			},
			"required": []string{"name"},
		},
	}, p.handlePluginInfo)

	s.RegisterTool(tp+"plugin_reload", sdk.ToolDef{
		Name:        tp + "plugin_reload",
		Description: "重新加载已安装的插件（代码或配置变更后生效，如 ClawHub 更新）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "插件名称（目录名）"},
			},
			"required": []string{"name"},
		},
	}, p.handlePluginReload)

	s.RegisterTool(tp+"channel_list", sdk.ToolDef{
		Name:        tp + "channel_list",
		Description: "列出所有已注册的消息通道及其运行状态（running/connected/账号列表）。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleChannelList)

	s.RegisterTool(tp+"channel_send", sdk.ToolDef{
		Name:        tp + "channel_send",
		Description: "向指定通道注入一条消息（经通道插件的 chatPolls/getPolls 轮询取走，如微信/钉钉通用通道）。用于主动向通道投递内容。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"channel":   map[string]interface{}{"type": "string", "description": "通道名称（channel_list 查询）"},
				"content":   map[string]interface{}{"type": "string", "description": "消息内容"},
				"from":      map[string]interface{}{"type": "string", "description": "发送方标识（可选）"},
				"accountId": map[string]interface{}{"type": "string", "description": "账号 ID（可选，默认 default）"},
			},
			"required": []string{"channel", "content"},
		},
	}, p.handleChannelSend)

	s.RegisterTool(tp+"channel_start", sdk.ToolDef{
		Name:        tp + "channel_start",
		Description: "启动指定通道的账号（重新执行 startAccount，恢复心跳/收消息轮询）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"channel": map[string]interface{}{"type": "string", "description": "通道名称（channel_list 查询）"},
			},
			"required": []string{"channel"},
		},
	}, p.handleChannelStart)

	s.RegisterTool(tp+"channel_stop", sdk.ToolDef{
		Name:        tp + "channel_stop",
		Description: "停止指定通道（执行 stopAccount，停止心跳/收消息轮询）。channel 省略则停止全部通道。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"channel":   map[string]interface{}{"type": "string", "description": "通道名称（可选，省略则停止全部）"},
				"accountId": map[string]interface{}{"type": "string", "description": "账号 ID（可选，默认停止该通道全部账号）"},
			},
			"required": []string{},
		},
	}, p.handleChannelStop)

	// Start file-based IPC for CLI integration (settings sync + reload requests)
	go p.ipcGoroutine(s)

	return nil
}

func (p *Plugin) ipcGoroutine(s *sdk.PluginSDK) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.mu.Lock()
			simDir := p.simulatorDir
			p.mu.Unlock()
			if simDir == "" {
				continue
			}

			// 1. Write settings snapshot for CLI config:get/config:list
			settings := s.Settings().Dump()
			if data, err := json.MarshalIndent(settings, "", "  "); err == nil {
				os.WriteFile(filepath.Join(simDir, ".settings.json"), data, 0644)
			}

			// 2. Process pending settings changes from CLI config:set
			pendingPath := filepath.Join(simDir, ".settings-pending.json")
			if data, err := os.ReadFile(pendingPath); err == nil {
				var pending map[string]interface{}
				if json.Unmarshal(data, &pending) == nil {
					for k, v := range pending {
						s.Settings().Set(k, v)
					}
				}
				os.Remove(pendingPath)
			}

			// 3. Process reload requests from CLI plugin:install
			reloadPath := filepath.Join(simDir, ".reload-request")
			if data, err := os.ReadFile(reloadPath); err == nil {
				name := strings.TrimSpace(string(data))
				if name != "" {
					if err := p.reloadPlugin(name); err != nil {
						log.Printf("[clawhubadapter] reload from CLI: %v", err)
					} else {
						log.Printf("[clawhubadapter] reloaded plugin from CLI request: %s", name)
					}
				}
				os.Remove(reloadPath)
			}
		}
	}
}

// ─── Manager ────────────────────────────────────────────────

// launchManager 启动 OC 插件管理器（Node.js 进程），用于安装/卸载/加载 OC 格式插件
func (p *Plugin) launchManager(s *sdk.PluginSDK) error {
	managerPath := filepath.Join(p.simulatorDir, "manager.js")
	if err := os.MkdirAll(p.simulatorDir, 0755); err != nil {
		return fmt.Errorf("create simulator dir: %w", err)
	}
	if err := os.WriteFile(managerPath, []byte(managerSrc), 0644); err != nil {
		return fmt.Errorf("write manager: %w", err)
	}

	// Write openclaw CLI wrapper so plugins can exec 'openclaw plugin:install' etc.
	cliBinDir := filepath.Join(p.simulatorDir, "bin")
	if err := os.MkdirAll(cliBinDir, 0755); err != nil {
		return fmt.Errorf("create cli bin dir: %w", err)
	}
	openclawPath := filepath.Join(cliBinDir, "openclaw")
	if err := os.WriteFile(openclawPath, []byte(openclawCliSrc), 0755); err != nil {
		return fmt.Errorf("write openclaw CLI: %w", err)
	}

	// Ensure skills dir exists for the manager to scan
	os.MkdirAll(p.skillsDir, 0755)

	sp, err := launchProcess("node", managerPath, p.skillsDir, "manager", p.simulatorDir)
	if err != nil {
		return fmt.Errorf("launch manager: %w", err)
	}
	if sp == nil {
		return nil
	}

	// Drain initial registration notifications from already-loaded plugins
	for done := false; !done; {
		select {
		case n := <-sp.NotifyChan():
			p.translateAndRegister(n, sp, s, "manager")
		default:
			done = true
		}
	}
	go p.notifyLoop(sp, s, "manager")

	p.mu.Lock()
	p.manager = sp
	p.sidecars = append(p.sidecars, sp)
	p.mu.Unlock()

	log.Printf("[clawhubadapter] OC plugin manager started")
	return nil
}

// ─── 管理工具处理 ────────────────────────────────────────────

func (p *Plugin) handlePluginInstall(args map[string]interface{}) (interface{}, error) {
	pkg, _ := args["package"].(string)
	if pkg == "" {
		return errorResult("package is required"), nil
	}

	if strings.HasPrefix(pkg, "clawhub:") {
		return p.installFromClawHub(pkg)
	}

	if strings.HasPrefix(pkg, "npm:") {
		pkg = strings.TrimPrefix(pkg, "npm:")
	}

	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()

	if mgr == nil {
		return errorResult("plugin manager not available"), nil
	}

	data, err := mgr.call("plugins/install", map[string]interface{}{
		"package": pkg,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("install failed: %v", err)), nil
	}

	var result struct {
		Name  string   `json:"name"`
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("Plugin installed. Raw response: %s", string(data)),
		}, nil
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("已安装插件: %s\n  工具: %s", result.Name, strings.Join(result.Tools, ", ")),
	}, nil
}

const clawhubAPI = "https://clawhub.ai/api/v1"

func (p *Plugin) installFromClawHub(spec string) (interface{}, error) {
	name := strings.TrimPrefix(spec, "clawhub:")
	if name == "" {
		return errorResult("clawhub package name is required"), nil
	}

	pkgURL := fmt.Sprintf("%s/packages/%s/download", clawhubAPI, url.PathEscape(name))
	resp, err := p.httpClient.Get(pkgURL)
	if err != nil {
		return errorResult(fmt.Sprintf("download failed: %v", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return errorResult(fmt.Sprintf("ClawHub API error (status %d): %s", resp.StatusCode, string(body))), nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult(fmt.Sprintf("read download: %v", err)), nil
	}

	extractDir := filepath.Join(p.skillsDir, name)
	os.MkdirAll(extractDir, 0755)

	if err := extractArchive(data, extractDir); err != nil {
		return errorResult(fmt.Sprintf("extract failed: %v", err)), nil
	}

	if err := p.reloadPlugin(name); err != nil {
		// Fallback: try manager's enhanced detection
		p.mu.Lock()
		mgr := p.manager
		p.mu.Unlock()

		if mgr != nil {
			data, mgrErr := mgr.call("plugins/detect", map[string]interface{}{
				"dir":  extractDir,
				"name": name,
			})
			if mgrErr == nil {
				var result struct {
					Name  string   `json:"name"`
					Tools []string `json:"tools"`
					Type  string   `json:"type"`
				}
				if json.Unmarshal(data, &result) == nil {
					return map[string]interface{}{
						"content": fmt.Sprintf("已从 ClawHub 安装插件: %s\n  工具: %s", result.Name, strings.Join(result.Tools, ", ")),
					}, nil
				}
			}
		}

		return errorResult(fmt.Sprintf("loaded but with warning: %v", err)), nil
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("已从 ClawHub 安装插件: %s", name),
	}, nil
}

func (p *Plugin) handleClawHubSearch(args map[string]interface{}) (interface{}, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return errorResult("query is required"), nil
	}

	searchURL := fmt.Sprintf("%s/search?q=%s", clawhubAPI, url.QueryEscape(query))
	resp, err := p.httpClient.Get(searchURL)
	if err != nil {
		return errorResult(fmt.Sprintf("search failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Results []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"displayName"`
			Summary     string `json:"summary"`
			Version     string `json:"version"`
			Downloads   int    `json:"downloads"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("Search results (raw):\n%s", string(body)),
		}, nil
	}

	if len(result.Results) == 0 {
		return map[string]interface{}{
			"content": fmt.Sprintf("未找到匹配 \"%s\" 的 ClawHub 插件", query),
		}, nil
	}

	var lines []string
	for _, pkg := range result.Results {
		ver := pkg.Version
		if ver == "" {
			ver = "latest"
		}
		name := pkg.DisplayName
		if name == "" {
			name = pkg.Slug
		}
		lines = append(lines, fmt.Sprintf("- %s (%s) v%s | ⬇ %d\n  %s\n  安装: clawhubadapter_plugin_install package=clawhub:%s",
			name, pkg.Slug, ver, pkg.Downloads, pkg.Summary, pkg.Slug))
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("在 ClawHub 找到 %d 个插件:\n%s", len(result.Results), strings.Join(lines, "\n")),
	}, nil
}

func (p *Plugin) handlePluginUninstall(args map[string]interface{}) (interface{}, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return errorResult("name is required"), nil
	}

	var logs []string
	pluginDir := filepath.Join(p.skillsDir, name)

	// 1. Stop & remove sidecar process if running
	p.mu.Lock()
	var aliveSidecars []*sidecarProcess
	for _, sp := range p.sidecars {
		if sp.name == name {
			sp.Close()
			logs = append(logs, fmt.Sprintf("已停止 sidecar 进程: %s", name))
		} else {
			aliveSidecars = append(aliveSidecars, sp)
		}
	}
	p.sidecars = aliveSidecars

	p.mu.Unlock()

	// 3. Try manager for OC plugins
	mgr := p.manager
	if mgr != nil {
		data, err := mgr.call("plugins/uninstall", map[string]interface{}{
			"name": name,
		})
		if err == nil {
			logs = append(logs, fmt.Sprintf("管理器已卸载: %s", string(data)))
		}
	}

	// 4. Delete directory from disk
	if _, statErr := os.Stat(pluginDir); statErr == nil {
		if err := os.RemoveAll(pluginDir); err != nil {
			logs = append(logs, fmt.Sprintf("删除目录失败: %v", err))
		} else {
			logs = append(logs, fmt.Sprintf("已删除目录: %s", pluginDir))
		}
	}

	if len(logs) == 0 {
		return errorResult(fmt.Sprintf("未找到插件 '%s'", name)), nil
	}

	result := fmt.Sprintf("已卸载插件: %s\n%s", name, strings.Join(logs, "\n"))
	result += "\n提示: 部分工具注册信息将在下次重启后完全清理。如需立即生效，请使用 reload 命令。"
	return map[string]interface{}{
		"content": result,
	}, nil
}

func (p *Plugin) handlePluginList(args map[string]interface{}) (interface{}, error) {
	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()

	if mgr != nil {
		data, err := mgr.call("plugins/list", nil)
		if err == nil && data != nil {
			var result struct {
				Plugins []struct {
					Name  string `json:"name"`
					Tools []struct {
						Name        string `json:"name"`
						Description string `json:"description"`
					} `json:"tools"`
				} `json:"plugins"`
			}
			if err := json.Unmarshal(data, &result); err == nil {
				var parts []string
				parts = append(parts, fmt.Sprintf("Skills dir: %s\n", p.skillsDir))

				if len(result.Plugins) > 0 {
					parts = append(parts, fmt.Sprintf("\nOC 插件 (%d):", len(result.Plugins)))
					for _, pl := range result.Plugins {
						var names []string
						for _, t := range pl.Tools {
							names = append(names, t.Name)
						}
						parts = append(parts, fmt.Sprintf("  %s: %s", pl.Name, strings.Join(names, ", ")))
					}
				}

				p.mu.Lock()
				if len(p.sidecars) > 0 {
					sidecarCount := 0
					for _, sp := range p.sidecars {
						if sp != p.manager {
							sidecarCount++
						}
					}
					if sidecarCount > 0 {
						parts = append(parts, fmt.Sprintf("\nSidecar 插件 (%d):", sidecarCount))
						for _, sp := range p.sidecars {
							if sp == p.manager {
								continue
							}
							tools, _ := sp.ListTools()
							var names []string
							for _, t := range tools {
								names = append(names, t.Name)
							}
							parts = append(parts, fmt.Sprintf("  %s: %s", sp.name, strings.Join(names, ", ")))
						}
					}
				}
				caps := p.dispatcher.Capabilities()
				if len(caps) > 0 {
					parts = append(parts, fmt.Sprintf("\nCapabilities (%d):", len(caps)))
					for _, c := range caps {
						parts = append(parts, fmt.Sprintf("  %s", c))
					}
				}
				if len(result.Plugins) == 0 && len(p.sidecars) <= 1 {
					parts = append(parts, "没有已安装的插件。")
				}
				p.mu.Unlock()

				return map[string]interface{}{
					"content": strings.Join(parts, "\n"),
				}, nil
			}
		}
	}

	// Fallback: list known plugins from Go side
	p.mu.Lock()
	defer p.mu.Unlock()

	var parts []string
	parts = append(parts, fmt.Sprintf("Skills dir: %s\n", p.skillsDir))

	if len(p.sidecars) > 0 {
		parts = append(parts, fmt.Sprintf("\nSidecar/OC 插件 (%d):", len(p.sidecars)))
		for _, sp := range p.sidecars {
			tools, err := sp.ListTools()
			toolList := ""
			if err == nil {
				var names []string
				for _, t := range tools {
					names = append(names, t.Name)
				}
				toolList = strings.Join(names, ", ")
			}
			parts = append(parts, fmt.Sprintf("  %s: %s", sp.name, toolList))
		}
	}

	caps := p.dispatcher.Capabilities()
	if len(caps) > 0 {
		parts = append(parts, fmt.Sprintf("\nCapabilities (%d):", len(caps)))
		for _, c := range caps {
			parts = append(parts, fmt.Sprintf("  %s", c))
		}
	}

	if len(p.sidecars) == 0 {
		parts = append(parts, "没有已安装的插件（原生技能由 skillmgr 管理，用 skill_list 查看）。")
	}

	return map[string]interface{}{
		"content": strings.Join(parts, "\n"),
	}, nil
}

func (p *Plugin) handlePluginInfo(args map[string]interface{}) (interface{}, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return errorResult("name is required"), nil
	}
	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()

	typ := ""
	var tools []string

	if mgr != nil {
		if data, err := mgr.call("plugins/list", nil); err == nil && data != nil {
			var result struct {
				Plugins []struct {
					Name  string `json:"name"`
					Tools []struct {
						Name        string `json:"name"`
						Description string `json:"description"`
					} `json:"tools"`
				} `json:"plugins"`
			}
			if json.Unmarshal(data, &result) == nil {
				for _, pl := range result.Plugins {
					if pl.Name == name {
						typ = "OC (manager)"
						for _, t := range pl.Tools {
							tools = append(tools, t.Name)
						}
					}
				}
			}
		}
	}

	if typ == "" {
		p.mu.Lock()
		for _, sp := range p.sidecars {
			if sp == mgr || sp.name != name {
				continue
			}
			typ = "sidecar"
			if tl, err := sp.ListTools(); err == nil {
				for _, t := range tl {
					tools = append(tools, t.Name)
				}
			}
		}
		p.mu.Unlock()
	}

	if typ == "" {
		return errorResult(fmt.Sprintf("未找到插件 '%s'", name)), nil
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("插件: %s\n类型: %s", name, typ))
	if len(tools) > 0 {
		parts = append(parts, fmt.Sprintf("工具 (%d):\n  - %s", len(tools), strings.Join(tools, "\n  - ")))
	}

	var own []string
	for _, c := range p.channelSummary(mgr) {
		if c["plugin"] == name {
			own = append(own, fmt.Sprintf("  %s | type=%v running=%v connected=%v accounts=%v",
				c["name"], c["type"], c["running"], c["connected"], c["accounts"]))
		}
	}
	if len(own) > 0 {
		parts = append(parts, "通道:\n"+strings.Join(own, "\n"))
	} else {
		parts = append(parts, "通道: 无")
	}

	return map[string]interface{}{
		"content": strings.Join(parts, "\n"),
	}, nil
}

func (p *Plugin) handlePluginReload(args map[string]interface{}) (interface{}, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return errorResult("name is required"), nil
	}
	if err := p.reloadPlugin(name); err != nil {
		return errorResult(fmt.Sprintf("reload failed: %v", err)), nil
	}
	return map[string]interface{}{
		"content": fmt.Sprintf("插件 %s 已重新加载", name),
	}, nil
}

// channelSummary 汇总 manager 通道注册信息与 Go 侧 channel_status 实时缓存
func (p *Plugin) channelSummary(mgr *sidecarProcess) []map[string]interface{} {
	var out []map[string]interface{}
	if mgr == nil {
		return out
	}
	data, err := mgr.call("plugins/channels", nil)
	if err != nil || data == nil {
		return out
	}
	var result struct {
		Channels []struct {
			Name     string                 `json:"name"`
			Plugin   string                 `json:"plugin"`
			Type     string                 `json:"type"`
			Status   map[string]interface{} `json:"status"`
			Accounts []string               `json:"accounts"`
		} `json:"channels"`
	}
	if json.Unmarshal(data, &result) != nil {
		return out
	}
	for _, c := range result.Channels {
		entry := map[string]interface{}{
			"name": c.Name, "plugin": c.Plugin, "type": c.Type, "accounts": c.Accounts,
		}
		channelStatusMu.Lock()
		if st, ok := channelStatus[c.Name]; ok {
			entry["running"] = st["running"]
			entry["connected"] = st["connected"]
		} else {
			entry["running"] = c.Status["running"]
			entry["connected"] = c.Status["connected"]
		}
		channelStatusMu.Unlock()
		out = append(out, entry)
	}
	return out
}

func (p *Plugin) handleChannelList(args map[string]interface{}) (interface{}, error) {
	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()
	if mgr == nil {
		return errorResult("plugin manager not available"), nil
	}
	chans := p.channelSummary(mgr)
	if len(chans) == 0 {
		return map[string]interface{}{
			"content": "没有已注册的通道。",
		}, nil
	}
	var lines []string
	for _, c := range chans {
		lines = append(lines, fmt.Sprintf("- %s | plugin=%v type=%v running=%v connected=%v accounts=%v",
			c["name"], c["plugin"], c["type"], c["running"], c["connected"], c["accounts"]))
	}
	return map[string]interface{}{
		"content": fmt.Sprintf("已注册通道 (%d):\n%s", len(chans), strings.Join(lines, "\n")),
	}, nil
}

func (p *Plugin) handleChannelSend(args map[string]interface{}) (interface{}, error) {
	channel, _ := args["channel"].(string)
	content, _ := args["content"].(string)
	if channel == "" || content == "" {
		return errorResult("channel and content are required"), nil
	}
	payload := map[string]interface{}{"content": content}
	if from, _ := args["from"].(string); from != "" {
		payload["from"] = from
	}
	if acc, _ := args["accountId"].(string); acc != "" {
		payload["accountId"] = acc
	}
	if err := p.SendToChannel(channel, payload); err != nil {
		return errorResult(fmt.Sprintf("send failed: %v", err)), nil
	}
	return map[string]interface{}{
		"content": fmt.Sprintf("消息已投递到通道 %s（等待插件轮询取走）", channel),
	}, nil
}

func (p *Plugin) handleChannelStart(args map[string]interface{}) (interface{}, error) {
	channel, _ := args["channel"].(string)
	if channel == "" {
		return errorResult("channel is required"), nil
	}
	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()
	if mgr == nil {
		return errorResult("plugin manager not available"), nil
	}
	data, err := mgr.call("channel/start", map[string]interface{}{"channel": channel})
	if err != nil {
		return errorResult(fmt.Sprintf("start failed: %v", err)), nil
	}
	var result struct {
		Status string `json:"status"`
	}
	json.Unmarshal(data, &result)
	return map[string]interface{}{
		"content": fmt.Sprintf("通道 %s 启动请求已发出（%v）", channel, result.Status),
	}, nil
}

func (p *Plugin) handleChannelStop(args map[string]interface{}) (interface{}, error) {
	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()
	if mgr == nil {
		return errorResult("plugin manager not available"), nil
	}
	params := map[string]interface{}{}
	channel, _ := args["channel"].(string)
	accountId, _ := args["accountId"].(string)
	if channel != "" {
		params["channel"] = channel
	}
	if accountId != "" {
		params["accountId"] = accountId
	}
	data, err := mgr.call("channel/stop", params)
	if err != nil {
		return errorResult(fmt.Sprintf("stop failed: %v", err)), nil
	}
	var result struct {
		Status string `json:"status"`
	}
	json.Unmarshal(data, &result)
	target := channel
	if target == "" {
		target = "全部"
	}
	return map[string]interface{}{
		"content": fmt.Sprintf("通道 %s 停止请求已发出（%v）", target, result.Status),
	}, nil
}

func (p *Plugin) loadOCPlugin(s *sdk.PluginSDK, dir, name string) error {
	// Delegate to manager's plugins/load instead of launching a separate simulator.
	// The manager is the single Node.js process that handles all OC plugins.
	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()
	if mgr == nil {
		return fmt.Errorf("plugin manager not available")
	}

	// Check if manager already has this plugin loaded
	data, listErr := mgr.call("plugins/list", nil)
	if listErr == nil && data != nil {
		var result struct {
			Plugins []struct {
				Name string `json:"name"`
			} `json:"plugins"`
		}
		if json.Unmarshal(data, &result) == nil {
			for _, pl := range result.Plugins {
				if pl.Name == name {
					log.Printf("[clawhubadapter] plugin %s already loaded by manager, skipping", name)
					return nil
				}
			}
		}
	}

	// Ask manager to load this plugin (notifications flow through the manager's stdout → Go notifyLoop)
	data, err := mgr.call("plugins/load", map[string]interface{}{
		"dir":  dir,
		"name": name,
	})
	if err != nil {
		return fmt.Errorf("manager plugins/load: %w", err)
	}

	var result struct {
		Name  string   `json:"name"`
		Tools []string `json:"tools"`
	}
	if json.Unmarshal(data, &result) == nil {
		log.Printf("[clawhubadapter] ocplugin %s loaded via manager with %d tools", name, len(result.Tools))
	} else {
		log.Printf("[clawhubadapter] ocplugin %s loaded via manager, raw: %s", name, string(data))
	}
	return nil
}

// notifyLoop 持续监听模拟器的注册通知，即时转译注册到核心。
// 生命周期绑定 sidecarProcess.NotifyChan，sp.Close() 时会关闭通道使循环退出。
func (p *Plugin) notifyLoop(sp *sidecarProcess, s *sdk.PluginSDK, pluginName string) {
	for n := range sp.NotifyChan() {
		p.translateAndRegister(n, sp, s, pluginName)
	}
}

func (p *Plugin) translateAndRegister(n OCNotification, sp *sidecarProcess, s *sdk.PluginSDK, pluginName string) {
	if n.Method == "channel_input" {
		var params struct {
			Channel string                 `json:"channel"`
			Payload map[string]interface{} `json:"payload"`
		}
		if err := json.Unmarshal(n.Params, &params); err != nil || params.Channel == "" {
			return
		}
		channelInputBufMu.Lock()
		channelInputBuf[params.Channel] = append(channelInputBuf[params.Channel], params.Payload)
		channelInputBufMu.Unlock()
		content, _ := params.Payload["content"].(string)
		if content == "" {
			data, _ := json.Marshal(params.Payload)
			content = string(data)
		}
		// 同步注入并取回复，再把回复送回通道（agent → output_send__<通道> → deliver → 微信）
		out := s.InjectInputSync(pluginName, params.Channel, "text", map[string]interface{}{
			"content": content,
		})
		if out != nil {
			reply, _ := out.Payload["content"].(string)
			if reply != "" {
				meta := map[string]interface{}{}
				if from, _ := params.Payload["from"].(string); from != "" {
					meta["user_id"] = from
				}
				if acc, _ := params.Payload["accountId"].(string); acc != "" {
					meta["accountId"] = acc
				}
				go func() {
					if _, err := sp.CallTool(params.Channel, map[string]interface{}{
						"payload": reply,
						"meta":    meta,
					}); err != nil {
						log.Printf("[clawhubadapter] channel %s reply dispatch failed: %v", params.Channel, err)
					}
				}()
			}
		}
		return
	}
	if n.Method == "channel_status" {
		var params struct {
			Channel string                 `json:"channel"`
			Status  map[string]interface{} `json:"status"`
		}
		if err := json.Unmarshal(n.Params, &params); err != nil || params.Channel == "" {
			return
		}
		channelStatusMu.Lock()
		if channelStatus == nil {
			channelStatus = make(map[string]map[string]interface{})
		}
		channelStatus[params.Channel] = params.Status
		channelStatusMu.Unlock()
		log.Printf("[clawhubadapter] channel_status %s: running=%v connected=%v", params.Channel,
			params.Status["running"], params.Status["connected"])
		return
	}
	if n.Method == "channel_output" {
		// 降级输出事件（通道已启动但 deliver 尚未建立）：仅记录，消息不丢失于协议层
		var params struct {
			Channel string `json:"channel"`
			Type    string `json:"type"`
			Text    string `json:"text"`
		}
		if json.Unmarshal(n.Params, &params) == nil && params.Channel != "" {
			log.Printf("[clawhubadapter] channel_output %s (type=%s): %.120s", params.Channel, params.Type, params.Text)
		}
		return
	}
	if n.Method != "register" {
		return
	}
	var params struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(n.Params, &params); err != nil {
		return
	}
	p.dispatcher.Dispatch(params.Type, params.Data, pluginName, sp, s)
}

// 向通道注入外部输入（经 manager channel/send → pollQueue → 插件 chatPolls 轮询取走）。
// 供内核其他组件（webui 会话、其他插件）向依赖 runtime 轮询的通用通道插件投递消息。
func (p *Plugin) SendToChannel(channel string, payload map[string]interface{}) error {
	p.mu.Lock()
	m := p.manager
	p.mu.Unlock()
	if m == nil {
		return fmt.Errorf("clawhubadapter manager not running")
	}
	_, err := m.call("channel/send", map[string]interface{}{
		"channel": channel,
		"payload": payload,
	})
	return err
}

// ChannelSender 返回 clawhubadapter 单例，供内核其他组件注入通道输入（nil 表示未启动）
func ChannelSender() *Plugin {
	return pluginSingleton
}

func (p *Plugin) loadPySidecar(s *sdk.PluginSDK, dir, name string) error {
	simPath := filepath.Join(p.simulatorDir, "pysim.py")
	if err := os.MkdirAll(p.simulatorDir, 0755); err != nil {
		return fmt.Errorf("create simulator dir: %w", err)
	}
	if err := os.WriteFile(simPath, []byte(pySimulatorSrc), 0644); err != nil {
		return fmt.Errorf("write pysimulator: %w", err)
	}

	// 选择 Python 解释器。优先级（2026-10-25 改为可配置）：
	//
	//  ① 插件配置 python_bin —— **显式指定，最高优先**
	//  ② <skill 目录>/.venv/bin/python —— skill 自带的虚拟环境
	//  ③ 系统 python3（/usr/bin → /usr/local/bin）
	//
	// 为什么 ① 必须在最前：不同部署的 Python 布局完全不同，"猜"没有正确解。
	// 实测事故：`bilibili-all-in-one` 声明 requirements 含 httpx，而系统
	// Python 没装 ⇒ 启动即 ModuleNotFoundError。往系统 Python 里装又会污染
	// 发行版环境（PEP 668 明确禁止，--break-system-packages 会破坏 apt 的
	// 包一致性）。正确解是让部署方显式指定用哪个解释器/venv。
	pythonBin, err := resolvePython(name, dir, p.pythonBin)
	if err != nil {
		return err
	}

	// 工作目录：配置指定则用它（需创建），否则用 skill 自身目录。
	workDir := dir
	if p.skillWorkdir != "" {
		if err := os.MkdirAll(p.skillWorkdir, 0755); err != nil {
			return fmt.Errorf("创建 skill_workdir（%s）: %w", p.skillWorkdir, err)
		}
		workDir = p.skillWorkdir
	}
	sp, err := launchProcessIn(pythonBin, simPath, dir, name, p.simulatorDir, workDir)
	if err != nil {
		return fmt.Errorf("launch pysimulator: %w", err)
	}
	if sp == nil {
		return nil
	}

	// 处理注册通知 (同 OC 插件流程)
	for done := false; !done; {
		select {
		case n := <-sp.NotifyChan():
			p.translateAndRegister(n, sp, s, name)
		default:
			done = true
		}
	}
	go p.notifyLoop(sp, s, name)

	tools, err := sp.ListTools()
	if err != nil {
		sp.Close()
		return fmt.Errorf("list tools: %w", err)
	}
	log.Printf("[clawhubadapter] pysidecar %s registered %d tools", name, len(tools))

	p.mu.Lock()
	p.sidecars = append(p.sidecars, sp)
	p.mu.Unlock()
	return nil
}

func (p *Plugin) loadSidecar(s *sdk.PluginSDK, dir, name string) error {
	sp, err := launchSidecar(dir, name, p.simulatorDir)
	if err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	if sp == nil {
		return nil
	}

	tools, err := sp.ListTools()
	if err != nil {
		sp.Close()
		return fmt.Errorf("list tools: %w", err)
	}

	for _, tool := range tools {
		toolName := fmt.Sprintf("%s_%s", name, tool.Name)
		tDef := sdk.ToolDef{
			Name:        toolName,
			Description: fmt.Sprintf("[%s] %s", name, tool.Description),
			Parameters:  tool.InputSchema,
		}
		handler := func(sp *sidecarProcess, toolName string) sdk.ToolHandler {
			return func(args map[string]interface{}) (interface{}, error) {
				return sp.CallTool(toolName, args)
			}
		}(sp, tool.Name)
		if err := s.RegisterTool(toolName, tDef, handler); err != nil {
			log.Printf("[clawhubadapter] register sidecar tool %s: %v", toolName, err)
			continue
		}
		log.Printf("[clawhubadapter] registered sidecar tool: %s (from %s)", toolName, name)
	}

	p.mu.Lock()
	p.sidecars = append(p.sidecars, sp)
	p.mu.Unlock()
	log.Printf("[clawhubadapter] sidecar %s started with %d tools", name, len(tools))
	return nil
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 关停 ipcGoroutine（stopCh），避免重载后旧 goroutine 残留导致线程累积
	// 见 agentcli 同类注释：Once 不可重入，close 的守卫必须用独立 once。
	p.stopOnce.Do(func() {
		safeclose.Safe(&p.stopChOnce, p.stopCh)
	})
	for _, sp := range p.sidecars {
		sp.Close()
	}
	p.sidecars = nil
	p.manager = nil
	p.sdk = nil
	return nil
}

// publishSkillDetected 发布 skill_detected 事件，将纯 SKILL 条目移交原生
// skillmgr 插件注册。OpenClaw 兼容层不持有 native skill 状态。
func (p *Plugin) publishSkillDetected(s *sdk.PluginSDK, path string) {
	s.Publish(&events.Event{
		Type:      events.EventSkillDetected,
		Source:    p.name,
		Payload:   map[string]interface{}{"path": path},
		Timestamp: time.Now().Unix(),
	})
	log.Printf("[clawhubadapter] skill %s detected, handed off to skillmgr", filepath.Base(path))
}

func errorResult(msg string) interface{} {
	return map[string]interface{}{
		"isError": true,
		"content": msg,
	}
}

// hasOCExtensions 检测 package.json 中是否有 openclaw.extensions 或 openclaw.runtimeExtensions
func hasOCExtensions(pkgPath string) bool {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return false
	}
	var pkg struct {
		OpenClaw *struct {
			Extensions        interface{} `json:"extensions"`
			RuntimeExtensions interface{} `json:"runtimeExtensions"`
		} `json:"openclaw"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	return pkg.OpenClaw != nil && (pkg.OpenClaw.Extensions != nil || pkg.OpenClaw.RuntimeExtensions != nil)
}

func (p *Plugin) reloadPlugin(name string) error {
	pluginDir := filepath.Join(p.skillsDir, name)
	info, err := os.Stat(pluginDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("plugin dir not found: %s", pluginDir)
	}

	if p.sdk == nil {
		return fmt.Errorf("sdk not initialized")
	}

	subs, err := os.ReadDir(pluginDir)
	if err != nil {
		return err
	}

	hasMainJS := false
	hasMainPy := false
	hasOCManifest := false
	hasOCPackage := false
	for _, f := range subs {
		switch f.Name() {
		case "main.js":
			hasMainJS = true
		case "main.py":
			hasMainPy = true
		case "openclaw.plugin.json":
			hasOCManifest = true
		case "package.json":
			hasOCPackage = hasOCExtensions(filepath.Join(pluginDir, "package.json"))
		}
	}

	os.MkdirAll(p.simulatorDir, 0755)

	switch {
	case hasMainJS:
		return p.loadSidecar(p.sdk, pluginDir, name)
	case hasMainPy:
		return p.loadPySidecar(p.sdk, pluginDir, name)
	case hasOCManifest || hasOCPackage:
		return p.loadOCPlugin(p.sdk, pluginDir, name)
	default:
		// 纯 SKILL 类型：移交原生 skillmgr（与 Start 扫描行为一致）
		p.publishSkillDetected(p.sdk, pluginDir)
		return nil
	}
}

func extractArchive(data []byte, dest string) error {
	if len(data) < 4 {
		return fmt.Errorf("archive too small (%d bytes)", len(data))
	}

	if data[0] == 0x50 && data[1] == 0x4B && data[2] == 0x03 && data[3] == 0x04 {
		return extractZIP(data, dest)
	}

	return extractTGZ(data, dest)
}

func extractZIP(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("zip: %w", err)
	}
	for _, f := range zr.File {
		target := filepath.Join(dest, f.Name)
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0755)
		r, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			r.Close()
			return err
		}
		_, err = io.Copy(out, r)
		r.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTGZ(data []byte, dest string) error {
	gzr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(dest, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}

// fileExists 报告路径存在（错误信息不需要，调用方只关心能否用）。
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// resolvePython 决定用哪个 Python 解释器跑 skill，优先级：
//
//	① configured（插件配置 python_bin）—— 显式指定，最高优先
//	② <skill 目录>/.venv/bin/python —— skill 自带虚拟环境
//	③ 系统 python3（/usr/bin → /usr/local/bin）→ 最后退回 "python3"
//
// ## 为什么必须可配置（2026-10-25 真机事故）
//
// 此前是硬编码猜：先 /usr/bin/python3，再 /usr/local/bin/python3。
// `bilibili-all-in-one` 声明 requirements 含 httpx，而系统 Python 没装
// ⇒ 启动即 ModuleNotFoundError。而"猜"没有正确解：不同部署的 Python
// 布局完全不同（发行版 Python、pyenv、uv、conda、venv…）。
//
// 往系统 Python 里 pip install 是**错的**解法：PEP 668 明确禁止，
// --break-system-packages 会破坏 apt 管理的包一致性。
//
// ## 配错必须响亮
//
// 配置了但文件不存在时返回 error，**不静默退回**系统解释器 ——
// 否则用户以为配好了，实际跑在缺依赖的 Python 上，又是一次静默降级。
func resolvePython(name, skillDir, configured string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("配置的 python_bin 不可用（%s）: %w", configured, err)
		}
		log.Printf("[clawhub] skill %s: using configured interpreter %s", name, configured)
		return configured, nil
	}
	if venvPy := filepath.Join(skillDir, ".venv", "bin", "python"); fileExists(venvPy) {
		log.Printf("[clawhub] skill %s: using its venv interpreter %s", name, venvPy)
		return venvPy, nil
	}
	for _, candidate := range []string{"/usr/bin/python3", "/usr/local/bin/python3"} {
		if fileExists(candidate) {
			return candidate, nil
		}
	}
	return "python3", nil
}

// pickPythonFor 是 resolvePython 的便捷包装（测试与旧调用点用）。
//
// 忽略错误并返回空串会让"配错"看起来像"没配"，所以调用方应优先用
// resolvePython；这里只为保持测试可读性。
func pickPythonFor(name, skillDir, configured string) string {
	v, _ := resolvePython(name, skillDir, configured)
	return v
}

package pluginmgr

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// platformBinary 按当前 OS/ARCH 选择正确的插件二进制文件名。
// 返回 (zip内文件名, 安装后重命名).
//
// 子进程模式下各平台产物统一叫 plugin.bin（进程边界即 ABI 边界，
// 不存在 .so/.dylib/.dll 的区分），故 zip 内按平台加后缀区分，
// 解包时挑当前平台那一份重命名为 plugin.bin。
func platformBinary() (zipName, canonicalName string) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows", "freebsd":
		return fmt.Sprintf("plugin.bin.%s.%s", runtime.GOOS, runtime.GOARCH), "plugin.bin"
	default:
		return "", ""
	}
}

// validBinaries 是 .hmap 中所有可识别的文件入口（平台二进制或脚本）。
var validBinaries = map[string]bool{
	"plugin.bin": true,
	"main.lua":   true,
	"SKILL.md":   true,
}

// isPlatformBinary 判断 zip 条目是否为平台特定二进制（bundle 模式下仅当前平台的被解压）。
//
// 形式：plugin.bin.<goos>.<goarch>。不用固定表是因为平台组合会增长
// （linux/arm64、darwin/arm64 等），按前缀判断无需维护清单。
func isPlatformBinary(name string) bool {
	return strings.HasPrefix(name, "plugin.bin.")
}

var downloadClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to disallowed scheme: %s", req.URL.Scheme)
		}
		return nil
	},
}

// defaultHTTPAddr 是 HTTP API 的**内置默认**监听地址。
//
// ★ 曾经这里是一个**包级可变全局** `var HTTPAddr`，且 Start() 会把 settings 读到的值
// **反写**回该全局。两个真实后果：
//  1. 多实例互相污染——测试并行起两个 Registry，后启动的实例会把地址写进全局，
//     先启动那个的 startHTTPServer 读到的是别人的地址（实测与生产 homed 抢 9876）；
//  2. 全局读写在并发下没有同步，属数据竞态。
//
// 现在改为实例字段 p.httpAddr（默认值走本常量），不再有可被任意代码改写的包级状态。
const defaultHTTPAddr = "127.0.0.1:9876"

func init() {
	plugin.RegisterPluginMeta("pluginmgr", "插件管理", "Plugin Manager")
	plugin.RegisterFactory("pluginmgr", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name      string
	mu        sync.Mutex
	server    *http.Server
	mux       *http.ServeMux
	listen    net.Listener
	httpURL   string
	httpAddr  string // 本实例的监听地址（默认 defaultHTTPAddr；来自 settings）
	sdk       *sdk.PluginSDK
	pluginDir string
	// maxPackageBytes 是单个插件包 **压缩后** 的上限（0 = 不限）。
	//
	// 为什么是配置项而不是常量：内嵌运行时（node+pi 之类）就要 ~100MB，
	// 硬编码一个数必然在某天成为纯粹的障碍。默认值给得很宽松（见
	// defaultMaxPackageMB），有需要的部署可以调大或设为 0 关闭。
	maxPackageBytes int64
}

func New(name string) *Plugin {
	return &Plugin{name: name, mux: http.NewServeMux(), httpAddr: defaultHTTPAddr}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "http_addr",
		Default:     defaultHTTPAddr,
		Type:        "string",
		DisplayName: "HTTP 监听地址",
		Description: "插件管理 API 的监听地址，设为空可禁用 HTTP 服务；" +
			"填 127.0.0.1:0 让系统分配空闲端口（测试/多实例推荐）",
		Category: "pluginmgr",
	})

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "max_package_mb",
		Default:     defaultMaxPackageMB,
		Type:        "int",
		DisplayName: "插件包大小上限 (MB)",
		Description: "单个插件包**压缩后**的字节上限；0 = 不限制。" +
			"内嵌运行时（node/pi 等）的包实测约 99MB，旧上限 100MB 下余量仅 1MB —— " +
			"需要更大就调高，或设 0 关闭限制。" +
			"该值在**读入内存之前**生效，不是事后检查。",
		Category: "pluginmgr",
	})

	// 只写本实例字段，**不写任何包级状态**（见 defaultHTTPAddr 注释）。
	p.httpAddr = defaultHTTPAddr
	if v, _ := s.Settings().Get("http_addr"); v != nil {
		if addr, ok := v.(string); ok {
			p.httpAddr = addr // 允许空串 = 显式禁用 HTTP 服务
		}
	}

	// ★ pluginDir 的正确来源是**内核**（PluginMgrAPI.PluginDir），
	// 不是 core.plugin.dir 配置项。
	//
	// 实测教训（2026-10-15）：用 GetCore("plugin.dir") 读时，生产 config.db
	// 里**根本没有这个键**（内核只是"播种"默认值，不一定落库），于是
	// p.pluginDir 为空 —— 而所有以它为起点的功能（技能重扫、删除插件目录）
	// 会**静默什么都不做**。安装却照常成功（那条路径用的是自己算的 dir），
	// 所以症状是"局部静默失效"，极难发现。
	//
	// 先问内核，配置项只作兼容回退。
	if mgr := p.pluginMgr(); mgr != nil {
		if dir := strings.TrimSpace(mgr.PluginDir()); dir != "" {
			p.pluginDir = dir
		}
	}
	if p.pluginDir == "" {
		if v, _ := s.Settings().GetCore("plugin.dir"); v != nil {
			if dir, ok := v.(string); ok && dir != "" {
				p.pluginDir = dir
			}
		}
	}

	// 上限也走"先给默认值再覆盖"，与本文件其他配置项一致
	// （避免 settings 尚未就绪时拿到 0 而变成"无限制"）。
	p.maxPackageBytes = int64(defaultMaxPackageMB) << 20
	if v, _ := s.Settings().Get("max_package_mb"); v != nil {
		p.maxPackageBytes = parseLimitMB(v)
	}

	p.registerTools(s)

	// ★ 启动重扫随包 skill：移交只在**安装那一刻**发生，而 skill 落在
	// 插件目录里（不在 skillmgr 的 skillsDir）⇒ worker 重启后它会**静默消失**。
	// 延迟几秒是为了等 skillmgr 的订阅就绪（两者 Start 顺序不确定）；
	// skillmgr 的 loadOne 幂等，重复到达无副作用。见 skill_handout.go。
	p.scheduleStartupSkillRescan()

	if p.httpAddr != "" {
		p.startHTTPServer()
	}

	return nil
}

func (p *Plugin) Stop() error {
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}

// ======== Tools ========

func (p *Plugin) registerTools(s *sdk.PluginSDK) {
	s.RegisterTool("plugin_install", sdk.ToolDef{
		Name:        "plugin_install",
		Description: "安装 HologramAgent 插件包（.hmap）。两种来源：url（http/https 下载）或 path（本机路径，配合 plugindev_build 的产物用这个）。插件已存在时传 overwrite=true 原地更新（升级/降级/重装，保留配置表，无需卸载重装）。更新后需调用 plgreload 或重启生效。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type":        "string",
					"description": "插件包的下载 URL（http/https）",
				},
				"path": map[string]interface{}{
					"type":        "string",
					"description": "插件包在**本机**的路径（.hmap）。与 url 二选一；同时给出时以 path 为准",
				},
				"overwrite": map[string]interface{}{
					"type":        "boolean",
					"description": "已存在时原地更新（保留配置）。默认 false",
				},
			},
		},
		// 装插件：改磁盘与运行态，可能半安装
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		overwrite, _ := args["overwrite"].(bool)
		// path 优先：它对应"agent 自己构建出产物再装"的场景（plugindev_build → plugin_install）。
		if path, _ := args["path"].(string); strings.TrimSpace(path) != "" {
			pth := strings.TrimSpace(path)
			if st, err := os.Stat(pth); err != nil || st.IsDir() {
				return map[string]interface{}{"error": fmt.Sprintf("path 无效（必须是存在的 .hmap 文件）: %s", pth)}, nil
			}
			return p.installFromPath(pth, overwrite)
		}
		url, _ := args["url"].(string)
		if strings.TrimSpace(url) == "" {
			return map[string]interface{}{"error": "需要 url 或 path（二选一）"}, nil
		}
		return p.installFromURL(strings.TrimSpace(url), overwrite)
	})

	s.RegisterTool("plugin_list", sdk.ToolDef{
		Name:        "plugin_list",
		Description: "列出已安装的所有外部插件及其版本。同时返回运行状态（loaded/alive/pid/崩溃次数），子进程插件死了在此体现为 alive=false。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 已核实只读：只列举已装插件
		ParallelSafe: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.listPlugins()
	})

	s.RegisterTool("plugin_status", sdk.ToolDef{
		Name: "plugin_status",
		Description: "查看插件运行状态：进程是否存活、PID、加载通道、最近崩溃次数、当前注册的工具。" +
			"不传 name 则返回全部插件概览。工具调不通时先用它确认插件是否还活着。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称；缺省返回全部",
				},
			},
		},
		// 已核实只读：查运行状态
		ParallelSafe: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		return p.pluginStatus(name)
	})

	s.RegisterTool("plugin_restart", sdk.ToolDef{
		Name: "plugin_restart",
		Description: "重启单个插件（停止后重新加载，保留配置）。适用于：插件进程已死但自动重启被用尽" +
			"（plugin_status 的 crash_count 达上限），或换了 plugin.bin 需立即生效。不需重启 homed。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		// 重启插件：改运行态
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		return p.restartPlugin(name)
	})

	// ---- 持久启用/禁用（设计 §2.6.7.4）----
	//
	// 与 plugin_start/stop 的分工：
	//   enable/disable ⇒ **持久**（写 disabled_plugins，跨重启保留）= 系统里"禁止运行"
	//   start/stop     ⇒ **运行态**（不写配置）                = 现在关掉这个窗口
	s.RegisterTool("plugin_disable", sdk.ToolDef{
		Name: "plugin_disable",
		Description: "**持久**禁用插件：停止它并写入禁用配置，**跨重启保留**（开机也不再自启）。" +
			"如果只是想临时关掉它、不希望影响开机自启，请用 plugin_stop。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		mgr := p.pluginMgr()
		if mgr == nil {
			return nil, fmt.Errorf("内核插件管理面不可用")
		}
		if err := mgr.DisablePlugin(name, "agent"); err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"status":     "disabled",
			"name":       name,
			"persistent": "已写入禁用配置（跨重启保留）",
		}, nil
	})

	s.RegisterTool("plugin_enable", sdk.ToolDef{
		Name: "plugin_enable",
		Description: "**持久**启用插件：解除禁用配置并立即加载它（跨重启保留）。" +
			"注意：它同时**清除禁用标记**，所以之后开机会自启；" +
			"若只是想把 plugin_stop 关掉的插件开回来，用 plugin_start 更精确。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		mgr := p.pluginMgr()
		if mgr == nil {
			return nil, fmt.Errorf("内核插件管理面不可用")
		}
		if err := mgr.EnablePlugin(name); err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"status":     "enabled",
			"name":       name,
			"persistent": "禁用标记已清除（之后开机会自启）",
		}, nil
	})

	// ---- 运行态启停（设计 §2.6.7.4）----
	//
	// ★ 与 plugin_enable/plugin_disable 的分工（用户明确指出"只有 reload"的缺口）：
	//
	//	plugin_enable/disable ⇒ **持久**：写 disabled_plugins，跨重启保留
	//	plugin_start/stop     ⇒ **运行态**：现在启停，不写任何配置
	//
	// 缺了这两个的直接后果：agent 想"临时关掉某插件看现象"只能调
	// plugin_disable ⇒ 顺带把开机自启也关了 ⇒ 用户下次开机发现"怎么不启动了"。
	s.RegisterTool("plugin_start", sdk.ToolDef{
		Name: "plugin_start",
		Description: "**运行态**启动一个插件：现在拉起它，**不改持久状态**（重启后仍按" +
			"开机自启/禁用配置决定）。用于把 plugin_stop 关掉的插件开回来。" +
			"注意：被**持久禁用**的插件会被拒绝，那种情况请用 plugin_enable。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		mgr := p.pluginMgr()
		if mgr == nil {
			return nil, fmt.Errorf("内核插件管理面不可用")
		}
		if err := mgr.StartPlugin(name); err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"status": "started",
			"name":   name,
			// ★ 明确告知这是运行态操作，避免 agent 误以为改了持久设置
			"persistent": "未改动（重启后按开机自启/禁用配置决定）",
		}, nil
	})

	s.RegisterTool("plugin_stop", sdk.ToolDef{
		Name: "plugin_stop",
		Description: "**运行态**关闭一个插件：现在停掉它，**不改持久状态**" +
			"（不会影响开机自启，重启后该插件正常加载）。用于临时腾出资源或隔离问题。" +
			"与 plugin_disable 不同 —— 后者会**持久**禁用（跨重启保留）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		mgr := p.pluginMgr()
		if mgr == nil {
			return nil, fmt.Errorf("内核插件管理面不可用")
		}
		if err := mgr.StopPlugin(name); err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"status":     "stopped",
			"name":       name,
			"persistent": "未改动（开机自启设置不变，重启后会重新加载）",
		}, nil
	})

	s.RegisterTool("plugin_remove", sdk.ToolDef{
		Name:        "plugin_remove",
		Description: "卸载一个已安装的外部插件",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		// 卸插件：改磁盘与运行态
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		return p.removePlugin(name)
	})

	s.RegisterTool("plugin_info", sdk.ToolDef{
		Name:        "plugin_info",
		Description: "查看指定已安装插件的详细信息（版本、作者、描述等）",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		// 已核实只读：查单个插件详情
		ParallelSafe: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		return p.pluginInfo(name)
	})
}

// ======== HTTP API ========

func (p *Plugin) startHTTPServer() {
	p.mux.HandleFunc("/plugins", p.handlePlugins)
	p.mux.HandleFunc("/plugins/", p.handlePluginByID)

	listen, err := net.Listen("tcp", p.httpAddr)
	if err != nil {
		log.Printf("[pluginmgr] HTTP listen: %v", err)
		return
	}
	// 用**实际绑定**的地址而非配置值：配 :0 时只有 net.Listener 知道真实端口。
	// 这也让 httpURL 在多实例/测试下始终指向本实例真正监听的端点。
	url := "http://" + listen.Addr().String()
	p.mu.Lock()
	p.listen = listen
	p.httpURL = url
	p.mu.Unlock()

	p.server = &http.Server{Handler: p.mux}
	go func() {
		log.Printf("[pluginmgr] HTTP API on %s", url)
		if err := p.server.Serve(listen); err != nil && err != http.ErrServerClosed {
			log.Printf("[pluginmgr] HTTP serve: %v", err)
		}
	}()
}

// HTTPURL 返回本实例实际监听的基地址（形如 http://127.0.0.1:9876）；
// 未启动或禁用时返回空串。供诊断与需要知道“到底在哪个端口”的调用方使用。
func (p *Plugin) HTTPURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.httpURL
}

func (p *Plugin) handlePlugins(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := p.listPlugins()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, list)

	case http.MethodPost:
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") {
			var body struct {
				URL       string `json:"url"`
				Path      string `json:"path"`
				Overwrite bool   `json:"overwrite"` // 已存在时原地更新（保留配置）
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			switch {
			case body.URL != "":
				result, err := p.installFromURL(body.URL, body.Overwrite)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, result)
			case body.Path != "":
				result, err := p.installFromPath(body.Path, body.Overwrite)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, result)
			default:
				http.Error(w, "url or path is required", http.StatusBadRequest)
				return
			}
		} else {
			data, err := readLimited(r.Body, p.maxPackageBytes)
			if err != nil {
				http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
				return
			}
			result, err := p.installFromData(data, false)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, result)
		}

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *Plugin) handlePluginByID(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/plugins/")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		http.Error(w, "plugin name required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		info, err := p.pluginInfo(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, info)

	case http.MethodDelete:
		result, err := p.removePlugin(name)
		if err != nil {
			// 内置插件禁卸 → 409 Conflict；不存在 → 404；其余删除失败 → 500
			msg := err.Error()
			switch {
			case strings.Contains(msg, "built-in plugin"):
				writeJSON(w, http.StatusConflict, map[string]interface{}{"error": msg, "name": name, "plugin_type": "builtin"})
			case strings.Contains(msg, "not found"):
				writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": msg, "name": name})
			default:
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": msg, "name": name})
			}
			return
		}
		writeJSON(w, http.StatusOK, result)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ======== Core Logic ========

func (p *Plugin) installFromPath(path string, overwrite bool) (interface{}, error) {
	// 本地文件同样走限流读：先 Stat 拿大小快速拒绝（避免白读一遍），
	// 再用 readLimited 兜住"Stat 与读取之间文件变大"的竞态。
	if p.maxPackageBytes > 0 {
		if st, err := os.Stat(path); err == nil && st.Size() > p.maxPackageBytes {
			return nil, errPackageTooLarge(p.maxPackageBytes)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	defer f.Close()
	data, err := readLimited(f, p.maxPackageBytes)
	if err != nil {
		return nil, err
	}
	return p.installFromData(data, overwrite)
}

func (p *Plugin) installFromURL(rawURL string, overwrite bool) (interface{}, error) {
	log.Printf("[pluginmgr] downloading: %s", rawURL)

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme: %s (only http/https allowed)", parsed.Scheme)
	}

	resp, err := downloadClient.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %s", resp.Status)
	}

	// 先用 Content-Length 快速拒绝（覆盖绝大多数情况），再用 readLimited
	// 兜住分块传输（Content-Length 缺失或撒谎）。
	if p.maxPackageBytes > 0 && resp.ContentLength > p.maxPackageBytes {
		return nil, errPackageTooLarge(p.maxPackageBytes)
	}
	data, err := readLimited(resp.Body, p.maxPackageBytes)
	if err != nil {
		return nil, err
	}

	result, err := p.installFromData(data, overwrite)
	if err != nil {
		return nil, err
	}

	if m, ok := result.(map[string]interface{}); ok {
		m["source"] = "url"
		result = m
	}
	return result, nil
}

// installFromData 安装（或 overwrite=true 时原地更新）插件包。
// 更新语义：StopAndUnload 停止旧实例但保留配置表，备份旧目录→解压新包→失败回滚；
// 更新后配置原样生效，无需用户手动卸载重装。
func (p *Plugin) installFromData(data []byte, overwrite bool) (interface{}, error) {
	pkg, err := validatePackage(data)
	if err != nil {
		return map[string]interface{}{
			"error":   "invalid package",
			"details": err.Error(),
		}, nil
	}

	dir := p.pluginDir
	if dir == "" {
		return map[string]interface{}{"error": "plugin dir not configured"}, nil
	}

	target := filepath.Join(dir, pkg.Name)
	var oldVersion string
	existing := false
	if m, err := plugin.ReadManifest(target); err == nil && m != nil {
		existing = true
		oldVersion = m.Version
	} else if _, statErr := os.Stat(target); statErr == nil {
		existing = true // 目录存在但 manifest 不可读：视为已安装、版本未知
	}

	if existing && !overwrite {
		return map[string]interface{}{
			"error":   "plugin already exists",
			"name":    pkg.Name,
			"version": pkg.Version,
			"current": oldVersion,
			"action":  "remove_first",
			"hint":    `传 "overwrite": true 可原地更新（保留配置）`,
		}, nil
	}

	if existing && overwrite {
		// ★ 先记住升级前是否在运行：StopAndUnload 会把它停掉，而升级不该
		// 顺手改变运行状态。生产实测（2026-10-20）：重装 qq 后它静默停着，
		// agent 发现工具全没了，只能 get_plugin_tools×N → plugin_start 摸回来。
		wasLoaded := false
		if p.sdk != nil && p.sdk.PluginMgr() != nil {
			for _, rt := range p.sdk.PluginMgr().ListPluginRuntimes() {
				if rt.Name == pkg.Name && rt.Loaded {
					wasLoaded = true
					break
				}
			}
		}

		// 原地更新：停旧实例（保留配置表），备份旧目录，解压新包，失败回滚。
		if p.sdk != nil && p.sdk.PluginMgr() != nil {
			if err := p.sdk.PluginMgr().StopAndUnload(pkg.Name); err != nil {
				log.Printf("[pluginmgr] StopAndUnload %s: %v", pkg.Name, err)
			}
		}
		backup := target + ".bak"
		os.RemoveAll(backup)
		if err := os.Rename(target, backup); err != nil {
			return map[string]interface{}{
				"error":   "backup old plugin dir failed",
				"details": err.Error(),
			}, nil
		}
		if err := extractPackage(data, dir); err != nil {
			// 回滚：恢复旧目录并重新加载旧版
			os.RemoveAll(target)
			if rbErr := os.Rename(backup, target); rbErr != nil {
				return map[string]interface{}{
					"error":    "extract failed AND rollback failed",
					"details":  err.Error(),
					"rollback": rbErr.Error(),
				}, nil
			}
			if p.sdk != nil && p.sdk.PluginMgr() != nil {
				_ = p.sdk.PluginMgr().ReloadOne(pkg.Name)
			}
			return map[string]interface{}{
				"error":   "extract failed (rolled back to " + oldVersion + ")",
				"details": err.Error(),
			}, nil
		}
		os.RemoveAll(backup)

		// ★ 随包 skill 移交（升级路径）：解压完成后才扫，否则目录还不完整。
		// 失败不阻断 —— skill 登记是附带收益，插件本身已装好。
		skillN, skillFailed := p.handOutPluginSkills(target, pkg.Name)

		// 升级前在跑的插件，升级后自动拉回来（运行状态不该被"换个包"改变）。
		// 失败不阻断安装：返回里如实写 running_restored=false，调用方可见。
		runningRestored := false
		if wasLoaded && p.sdk != nil && p.sdk.PluginMgr() != nil {
			if err := p.sdk.PluginMgr().StartPlugin(pkg.Name); err != nil {
				log.Printf("[pluginmgr] 升级后恢复运行 %s 失败: %v（插件已安装，可手动 plugin_start）", pkg.Name, err)
			} else {
				runningRestored = true
			}
		}

		checksum := fmt.Sprintf("%x", sha256.Sum256(data))
		action := "upgraded"
		if cmpVersion(pkg.Version, oldVersion) < 0 {
			action = "downgraded"
		} else if cmpVersion(pkg.Version, oldVersion) == 0 {
			action = "reinstalled"
		}
		return map[string]interface{}{
			"status":            "installed",
			"name":              pkg.Name,
			"version":           pkg.Version,
			"previous_version":  oldVersion,
			"entry":             pkg.Entry,
			"checksum":          checksum,
			"action":            action,
			"reload_required":   true,
			"config_kept":       true,
			"was_running":       wasLoaded,
			"running_restored":  runningRestored,
			"skills_registered": skillN,
			"skills_failed":     skillFailed,
		}, nil
	}

	if err := extractPackage(data, dir); err != nil {
		return map[string]interface{}{
			"error":   "extract failed",
			"details": err.Error(),
		}, nil
	}

	checksum := fmt.Sprintf("%x", sha256.Sum256(data))

	// ★ 随包 skill 移交（全新安装路径）。
	skillN, skillFailed := p.handOutPluginSkills(target, pkg.Name)

	return map[string]interface{}{
		"status":            "installed",
		"name":              pkg.Name,
		"version":           pkg.Version,
		"entry":             pkg.Entry,
		"checksum":          checksum,
		"action":            "reload_required",
		"skills_registered": skillN,
		"skills_failed":     skillFailed,
	}, nil
}

// cmpVersion 比较点分版本号：a<b 返回 -1，a>b 返回 1，相等返回 0。
// 非数字段按字符串比较；长度不齐缺段视作 0。
func cmpVersion(a, b string) int {
	parse := func(s string) []int {
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 4)
		out := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				n = 0
			}
			out = append(out, n)
		}
		for len(out) < 3 {
			out = append(out, 0)
		}
		return out
	}
	a1, b1 := parse(a), parse(b)
	for i := range a1 {
		if a1[i] < b1[i] {
			return -1
		}
		if a1[i] > b1[i] {
			return 1
		}
	}
	return 0
}

func (p *Plugin) listPlugins() (interface{}, error) {
	dir := p.pluginDir
	if dir == "" {
		return []map[string]interface{}{}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]interface{}{}, nil
		}
		return nil, err
	}

	// 运行期状态一次取齐，避免逐个插件回内核查。
	//
	// 为何要带运行期：只读 plugin.json 的旧实现无法区分「已安装」与「正在跑」。
	// 生产上 editdoc 子进程被 kill 后，plugin_list 依旧把它列为正常插件，
	// 模型与 WebUI 都看不出异常，只能在调工具时吃一个“进程已退出”。
	runtimes := map[string]sdk.PluginRuntimeInfo{}
	if mgr := p.pluginMgr(); mgr != nil {
		for _, rt := range mgr.ListPluginRuntimes() {
			runtimes[rt.Name] = rt
		}
	}

	var plugins []map[string]interface{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		m, err := plugin.ReadManifest(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		item := map[string]interface{}{
			"name":        m.Name,
			"version":     m.Version,
			"description": m.Description,
			"author":      m.Author,
			"entry":       m.Entry,
			"deprecated":  m.Deprecated,
		}
		// auto_start 来自清单本身（即使插件从未被内核记录过也要能读到）：
		// 前端要区分「按需待命」与「该起没起」，不能只看 loaded。
		item["auto_start"] = m.ShouldAutoStart()
		if rt, ok := runtimes[m.Name]; ok {
			item["loaded"] = rt.Loaded
			item["alive"] = rt.Alive
			item["disabled"] = rt.Disabled
			item["channel"] = rt.Channel
			item["runtime_stopped"] = rt.RuntimeStopped
			item["builtin"] = rt.Builtin
			if rt.PID > 0 {
				item["pid"] = rt.PID
			}
			if rt.CrashCount > 0 {
				item["crash_count"] = rt.CrashCount
			}
			if len(rt.Tools) > 0 {
				item["tool_count"] = len(rt.Tools)
			}
		}
		plugins = append(plugins, item)
	}
	if plugins == nil {
		plugins = []map[string]interface{}{}
	}
	return plugins, nil
}

// pluginMgr 取内核插件管理面（可能为 nil：单测/未注入）。
func (p *Plugin) pluginMgr() sdk.PluginManager {
	if p.sdk == nil {
		return nil
	}
	return p.sdk.PluginMgr()
}

// pluginStatus 返回插件运行期状态（进程存活/PID/崩溃计数/工具清单）。
//
// 这是子进程化后插件管理器必须补上的一块：以前插件与内核同进程，
// “加载了”就等于“能用”；现在插件是独立进程，两者不再等价。
func (p *Plugin) pluginStatus(name string) (interface{}, error) {
	mgr := p.pluginMgr()
	if mgr == nil {
		return nil, fmt.Errorf("内核插件管理面不可用")
	}
	if name != "" {
		info, ok := mgr.PluginRuntime(name)
		if !ok {
			return nil, fmt.Errorf("plugin %q not found", name)
		}
		return info, nil
	}

	all := mgr.ListPluginRuntimes()
	// 汇总一行：让模型不用自己数就能看出“有东西挂了”。
	var loaded, dead int
	var unhealthy []string
	for _, rt := range all {
		if rt.Loaded {
			loaded++
		}
		if rt.Loaded && !rt.Alive {
			dead++
			unhealthy = append(unhealthy, rt.Name)
			continue
		}
		if rt.CrashCount > 0 {
			unhealthy = append(unhealthy, fmt.Sprintf("%s(崩溃%d次)", rt.Name, rt.CrashCount))
		}
	}
	sort.Strings(unhealthy)
	return map[string]interface{}{
		"total":     len(all),
		"loaded":    loaded,
		"dead":      dead,
		"unhealthy": unhealthy,
		"plugins":   all,
	}, nil
}

// restartPlugin 重启单个插件（保留配置）。
//
// 与 plgreload 的区别：后者按入口文件 hash 增量重载，二进制没改就不动；
// 而进程被 kill 时二进制正是没改的，所以一定要有一个无条件重启的入口。
func (p *Plugin) restartPlugin(name string) (interface{}, error) {
	mgr := p.pluginMgr()
	if mgr == nil {
		return nil, fmt.Errorf("内核插件管理面不可用")
	}
	if _, ok := mgr.PluginRuntime(name); !ok {
		return nil, fmt.Errorf("plugin %q not found", name)
	}
	if mgr.IsPluginDisabled(name) {
		return nil, fmt.Errorf("plugin %s 已被禁用，请先启用再重启", name)
	}
	if err := mgr.ReloadOne(name); err != nil {
		return nil, fmt.Errorf("restart %s: %w", name, err)
	}
	info, _ := mgr.PluginRuntime(name)
	return map[string]interface{}{
		"status":  "restarted",
		"name":    name,
		"runtime": info,
	}, nil
}

func (p *Plugin) removePlugin(name string) (interface{}, error) {
	// 内置插件只能禁用不能卸载：目录下无产物，且从注册表删除会破坏内核依赖。
	if p.sdk != nil && p.sdk.PluginMgr() != nil && p.sdk.PluginMgr().IsBuiltinPlugin(name) {
		return nil, fmt.Errorf("plugin %s is a built-in plugin and cannot be unloaded", name)
	}

	dir := filepath.Join(p.pluginDir, name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, fmt.Errorf("plugin %s not found", name)
	}

	// 先经内核卸载：停止插件（stop handlers + Stop）并执行插件注册的 onRemove 回调
	if p.sdk != nil && p.sdk.PluginMgr() != nil {
		if err := p.sdk.PluginMgr().RemovePlugin(name); err != nil {
			log.Printf("[pluginmgr] RemovePlugin %s: %v", name, err)
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("remove plugin dir: %w", err)
	}

	// 同步清理禁用表
	if p.sdk != nil && p.sdk.PluginMgr() != nil {
		if err := p.sdk.PluginMgr().EnablePlugin(name); err != nil {
			log.Printf("[pluginmgr] enable %s after remove: %v", name, err)
		}
	}

	// 卸载已即时生效（停止+注册表移除+目录删除），无需 reload
	return map[string]interface{}{
		"status":          "removed",
		"name":            name,
		"reload_required": false,
	}, nil
}

func (p *Plugin) pluginInfo(name string) (interface{}, error) {
	dir := filepath.Join(p.pluginDir, name)
	m, err := plugin.ReadManifest(dir)
	if err != nil {
		return nil, fmt.Errorf("plugin %q not found", name)
	}
	info := map[string]interface{}{
		"name":        m.Name,
		"version":     m.Version,
		"description": m.Description,
		"author":      m.Author,
		"license":     m.License,
		"homepage":    m.Homepage,
		"repository":  m.Repository,
		"entry":       m.Entry,
		"min_version": m.MinVersion,
		"tags":        m.Tags,
		"deprecated":  m.Deprecated,
	}

	entries, _ := os.ReadDir(dir)
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	info["files"] = files

	return info, nil
}

// ======== Package Validation ========

type pluginPackage struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Entry     string   `json:"entry"`
	Platforms []string `json:"platforms,omitempty"`
}

const (
	// defaultMaxPackageMB 是插件包默认上限（MB）。
	//
	// 为什么给这么大：内嵌运行时的插件包本身就接近 100MB
	// （node 二进制约 50MB + pi 依赖闭包约 50MB，压缩后）。
	// 这个值只作为"防止手滑传错文件"的兜底，真正的 DoS 防护
	// 由 readLimited 在**读取前**按 Content-Length / LimitReader 完成。
	//
	// 需要更大（或不需要限制）时改插件设置 max_package_mb（0 = 不限）。
	defaultMaxPackageMB = 512
)

// errPackageTooLarge 是统一的超限错误（三种入口共用）。
func errPackageTooLarge(limit int64) error {
	return fmt.Errorf("package too large (>%dMB)；如确需更大，请调整插件设置 max_package_mb（0 = 不限）", limit>>20)
}

// parseLimitMB 把配置值解析成字节上限（0 = 不限）。
//
// ★ 必须处理 string：插件配置从 DB 读出来是**字符串**
// （config.PluginSettings.Get → openFromStorage 返回 string），
// 不是 int/float64。初版照抄了 agentcli 的 `v.(float64)` 假设，
// 于是 switch 一个分支都不匹配 ⇒ **配置被静默忽略**，
// 用户改了 max_package_mb 却不生效，且没有任何提示。
//
// 兼容其它形态是为了稳健（不同装配路径可能给出不同静态类型），
// 但 string 是真实路径，必须有判据钉住。
func parseLimitMB(v interface{}) int64 {
	var mb int64
	switch n := v.(type) {
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return int64(defaultMaxPackageMB) << 20
		}
		parsed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			log.Printf("[pluginmgr] max_package_mb=%q 不是整数，回退默认 %dMB", s, defaultMaxPackageMB)
			return int64(defaultMaxPackageMB) << 20
		}
		mb = parsed
	case int:
		mb = int64(n)
	case int64:
		mb = n
	case float64:
		mb = int64(n)
	default:
		log.Printf("[pluginmgr] max_package_mb 类型意外（%T），回退默认 %dMB", v, defaultMaxPackageMB)
		return int64(defaultMaxPackageMB) << 20
	}
	if mb < 0 {
		// 负数视为"不限"（与 0 同义），而不是"拒一切" ——
		// 配置误填负号不该让插件系统完全不可用。
		mb = 0
	}
	return mb << 20
}

// readLimited 读入 reader 并在 **读取过程中** 施加上限。
//
// ★ 为什么必须在这里做，而不是读完再检查：
//
// 原实现在 io.ReadAll 之后才 `len(data) > 100<<20`。那时内存**已经占用**，
// 一个声明 `Content-Length: 10GB` 的请求可以先把进程撑爆再收到"包太大"的报错 ——
// 限制形同虚设。读前限流才是有意义的防护（也是本次把上限变成配置项的前提：
// 放宽一个无效检查不会带来新风险，放寛一个有效检查才会）。
//
// limit <= 0 表示不限制。
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	// 多读 1 字节：能读到 limit+1 才说明确实超了，
	// 否则恰好等于 limit 的包会被误判（边界差一）。
	lr := io.LimitReader(r, limit+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errPackageTooLarge(limit)
	}
	return data, nil
}

// validatePackage 只做**结构合法性**校验（元数据、平台二进制、路径安全）。
//
// 大小不在这里判：它必须在读入内存之前决定，见 readLimited。
// （曾经这里有一个 `len(data) > 100<<20` 的检查，属于"事后检查"，
// 挡不住任何东西却会在有一天需要大包时成为障碍 —— 已移除。）
func validatePackage(data []byte) (*pluginPackage, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip: %w", err)
	}

	// ★ 路径检查必须遍历**全部**条目，与找 manifest 分开。
	//
	// 原实现把路径检查放在寻找 plugin.json 的循环里，而该循环找到
	// manifest 后会 `break` —— 于是排在 plugin.json **之后**的条目
	// 完全没被检查。hmapdev 打包出的 rawfile 条目恒在 plugin.json 之后，
	// 正好落在检查盲区（实测发现）。
	//
	// 落盘侧（extractPackage）另有独立的 path traversal 检查，
	// 所以这不是可利用漏洞；但"校验函数只校验了一部分还叫校验"本身就是缺陷，
	// 且一旦将来有人复用 validatePackage 做准入判断就会出事。
	for _, f := range reader.File {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			return nil, fmt.Errorf("invalid path: %s", f.Name)
		}
	}

	var pkg pluginPackage
	hasManifest := false

	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Name != "plugin.json" {
			continue
		}
		hasManifest = true
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("read manifest: %w", err)
		}
		mData, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read manifest: %w", err)
		}
		if err := json.Unmarshal(mData, &pkg); err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		break
	}

	if !hasManifest {
		return nil, fmt.Errorf("missing plugin.json")
	}
	if pkg.Name == "" {
		return nil, fmt.Errorf("manifest: name required")
	}
	if pkg.Version == "" {
		return nil, fmt.Errorf("manifest: version required")
	}
	if pkg.Entry == "" {
		return nil, fmt.Errorf("manifest: entry required")
	}

	// verify at least one valid binary exists for any platform
	hasBinary := false
	zipEntries := map[string]bool{}
	for _, f := range reader.File {
		if !f.FileInfo().IsDir() {
			zipEntries[f.Name] = true
		}
	}

	if len(pkg.Platforms) > 0 {
		// bundle mode：每个声明的平台都要有对应二进制。
		// 子进程模式下条目形式为 plugin.bin.<goos>.<goarch>，
		// 故按前缀匹配而不枚举架构（同一 OS 可能有 amd64/arm64 两份）。
		for _, plat := range pkg.Platforms {
			prefix := "plugin.bin." + plat + "."
			found := false
			for name := range zipEntries {
				if strings.HasPrefix(name, prefix) {
					found = true
					break
				}
			}
			if found {
				hasBinary = true
			}
		}
	} else {
		// legacy mode: check entry exists and is a known binary
		if zipEntries[pkg.Entry] && validBinaries[pkg.Entry] {
			hasBinary = true
		}
	}

	if !hasBinary {
		return nil, fmt.Errorf("no valid binary found in package (entry=%q, platforms=%v)", pkg.Entry, pkg.Platforms)
	}

	return &pkg, nil
}

// copyZipEntry 解压 zip 中的单个文件到目标路径。
func copyZipEntry(f *zip.File, dest string) error {
	os.MkdirAll(filepath.Dir(dest), 0755)
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

func extractPackage(data []byte, pluginDir string) error {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}

	// 先读 manifest 确定插件名和平台信息
	var pkgName string
	var declaredPlatforms []string
	for _, f := range reader.File {
		if f.Name == "plugin.json" && !f.FileInfo().IsDir() {
			rc, _ := f.Open()
			mData, _ := io.ReadAll(rc)
			rc.Close()
			var m struct {
				Name      string   `json:"name"`
				Platforms []string `json:"platforms"`
			}
			json.Unmarshal(mData, &m)
			pkgName = m.Name
			declaredPlatforms = m.Platforms
			break
		}
	}
	if pkgName == "" {
		return fmt.Errorf("cannot determine plugin name")
	}

	target := filepath.Join(pluginDir, pkgName)
	os.MkdirAll(target, 0755)

	zipBin, canonicalName := platformBinary()
	isBundle := declaredPlatforms != nil

	for _, f := range reader.File {
		fpath := filepath.Join(target, f.Name)
		if !strings.HasPrefix(filepath.Clean(fpath), filepath.Clean(target)+string(os.PathSeparator)) {
			return fmt.Errorf("path traversal: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0755)
			continue
		}

		// bundle mode: skip other platforms' platform-specific binaries
		if isBundle && isPlatformBinary(f.Name) && f.Name != zipBin {
			continue
		}

		// 平台二进制重命名为规范名（plugin.bin.linux.amd64 → plugin.bin）
		dest := fpath
		if isBundle && f.Name == zipBin && canonicalName != zipBin {
			dest = filepath.Join(target, canonicalName)
		}

		if err := copyZipEntry(f, dest); err != nil {
			return err
		}

		// 子进程插件必须可执行。
		// zip 保留了原文件权限位，但经某些工具链/传输后可能丢失；
		// 内核加载时会因缺执行位报错（带 chmod +x 提示），在此提前补上。
		if filepath.Base(dest) == "plugin.bin" {
			if err := os.Chmod(dest, 0o755); err != nil {
				return fmt.Errorf("chmod %s: %w", dest, err)
			}
		}
	}

	return nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

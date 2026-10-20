package webui

import (
	"log"
	"math"
	"sort"
	"strings"

	"encoding/json"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/pkg/types"
	"net/http"
)

// 设置与插件面：配置读写、插件列表/详情（含 pluginmgr 反代）。

func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	if h.config == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "config not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.config.Get())
	case http.MethodPut:
		var cfg types.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid config"})
			return
		}
		h.config.Put(&cfg)
		writeJSON(w, http.StatusOK, map[string]string{"status": "config_updated"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// isInternalSetting 判断某个插件的配置键是不是**内部数据**（不是用户设置项）。
//
// chathistory 是 webui 自己持久化的整段聊天记录（生产实例实测 5.2MB）。它躺在
// 插件配置表里，于是会被设置接口当成普通配置项整块吐出：GET /api/v1/settings
// 因此返回 8MB+，并且前端把它渲染成一个巨大的文本框。它只应由 chat 相关
// 接口的 handleChatHistory 读，不进设置面。
func isInternalSetting(plugin, key string) bool {
	return plugin == "webui" && key == "chathistory"
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "config registry not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		prefix := r.URL.Query().Get("prefix")
		values := make(map[string]interface{})
		meta := make(map[string]*sdk.ConfigDef)

		if strings.HasPrefix(prefix, "plugin.") {
			// 插件配置：从插件自身 config_<name> 表读取
			pluginName := prefix[7:]
			keys, _ := h.settings.ListPlugin(pluginName, "")
			for _, k := range keys {
				if isInternalSetting(pluginName, k) {
					continue
				}
				v, _ := h.settings.GetPlugin(pluginName, k)
				fullKey := prefix + "." + k
				values[fullKey] = v
			}
			for _, def := range h.settings.DefsPlugin(pluginName, "") {
				fullKey := prefix + "." + def.Key
				meta[fullKey] = def
			}
		} else {
			// 核心配置：从 core config 表读取（键可为任意前缀，如 core.llm.*、webui.*）
			all := h.settings.Dump()
			var keys []string
			for k := range all {
				if strings.HasPrefix(k, prefix) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				values[k] = all[k]
			}
			for _, d := range h.settings.DefsCore(prefix) {
				meta[d.Key] = d
				// 有 def 但 DB 中尚无值的 key，用 default 填充以便在 WebUI 中显示和编辑
				if _, exists := values[d.Key]; !exists {
					values[d.Key] = d.Default
				}
			}
			// 无前缀时同时加载所有插件配置
			if prefix == "" {
				for _, p := range h.settings.Plugins() {
					if p == "core" {
						continue
					}
					pkeys, _ := h.settings.ListPlugin(p, "")
					for _, k := range pkeys {
						if isInternalSetting(p, k) {
							continue
						}
						v, _ := h.settings.GetPlugin(p, k)
						fullKey := "plugin." + p + "." + k
						values[fullKey] = v
					}
					for _, def := range h.settings.DefsPlugin(p, "") {
						fullKey := "plugin." + p + "." + def.Key
						meta[fullKey] = def
					}
				}
			}
		}
		// ★ E-3：列表永远默认屏蔽敏感值。明文揭示必须走单键 reveal 接口。
		h.maskSecretValues(values, false)

		plugins := []string{"core"}
		for _, p := range h.settings.Plugins() {
			if p != "core" {
				plugins = append(plugins, "plugin."+p)
			}
		}
		var pm map[string]sdk.PluginMeta
		if h.pluginMgr != nil {
			pm = h.pluginMgr.PluginMetas()
		}
		var disabledPlugins []sdk.DisabledPluginInfo
		if h.pluginMgr != nil {
			disabledPlugins = h.pluginMgr.ListDisabledPlugins()
		}
		sort.Strings(plugins)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"settings":         values,
			"meta":             meta,
			"plugins":          plugins,
			"plugin_meta":      pm,
			"disabled_plugins": disabledPlugins,
		})
	case http.MethodPut:
		var body struct {
			Key   string      `json:"key"`
			Value interface{} `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		// 整数型 float64（如 QQ 号 2.198972886e+09）规范化为 int64，
		// 避免被 fmt.Sprint 以科学计数法存库导致后续读取/解析失败。
		if f, ok := body.Value.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1e15 {
			body.Value = int64(f)
		}
		// ★ 掩码占位符**不是值**，绝不能存回内核。
		//
		// 真实故障（2026-10-16）：设置页里点一次「保存」，前端会把同一组字段
		// （含刚被掩码的 api_key）一并回传 ⇒ `••••••••` 被当真实值存进
		// `core.llm.sources.<n>.api_key` ⇒ 下游（commondev 的 pi 源导入器）
		// 把掩码写进 pi 的 auth.json ⇒ 模型调用 401。
		//
		// 这里拒绝并**明确告诉调用方怎么做**：它没说"保存失败"就完了，
		// 否则用户不知道该如何保存其他字段。
		if IsSecretPlaceholder(body.Value) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "该值是**掩码占位符**（列表接口为防泄漏而显示的“已设置”标记），" +
					"不是真实内容；存回它会用标记覆盖真值。" +
					"若未修改此字段请**不要保存它**（只保存你改动的键）；" +
					"若确实要改，请填入新的真值（或先调 reveal 接口取回当前值后再改）。",
				"key": body.Key,
			})
			return
		}
		deleting := body.Value == nil
		if strings.HasPrefix(body.Key, "plugin.") {
			parts := strings.SplitN(body.Key, ".", 3)
			if len(parts) >= 3 {
				if isInternalSetting(parts[1], parts[2]) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "该键属于插件内部数据，不经设置接口读写"})
					return
				}
				var err error
				if deleting {
					err = h.settings.RemovePlugin(parts[1], parts[2])
				} else {
					err = h.settings.SetPlugin(parts[1], parts[2], body.Value)
				}
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
			}
		} else {
			var err error
			if deleting {
				err = h.settings.RemoveCore(body.Key)
			} else {
				err = h.settings.SetCore(body.Key, body.Value)
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		if strings.HasPrefix(body.Key, "core.llm.") && h.llm != nil {
			if err := h.llm.ReloadFromConfig(); err != nil {
				log.Printf("[webui] failed to reload LLM providers: %v", err)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ======== Plugin Management (proxied to pluginmgr HTTP API) ========

func (h *Handler) pluginmgrAddr() string {
	addr := "127.0.0.1:9876"
	if h.settings == nil {
		return addr
	}
	if v, err := h.settings.GetPlugin("pluginmgr", "http_addr"); err == nil {
		if s, ok := v.(string); ok && s != "" {
			addr = s
		}
	}
	return addr
}

func (h *Handler) proxyToPluginmgr(w http.ResponseWriter, r *http.Request, path string) {
	addr := h.pluginmgrAddr()
	url := "http://" + addr + path
	// 与设备网关反代共用同一实现：原来两处各抄一份，于是同一个 bug
	//（跟随 3xx / 不逐帧 flush / 不透传 X-Forwarded）修了两遍还漏了两处。
	h.reverseToUpstream(w, r, url, "")
}

func (h *Handler) handlePlugins(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.proxyToPluginmgr(w, r, "/plugins")
	case http.MethodPost:
		h.proxyToPluginmgr(w, r, "/plugins")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handlePluginByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/plugins/")
	path = strings.TrimSuffix(path, "/")

	// 插件名白名单：仅允许单段安全名称，阻断路径穿越/空名/嵌套路径
	validPluginName := func(s string) bool {
		if s == "" || len(s) > 128 {
			return false
		}
		// 禁止路径分隔符、连续点(父目录穿越)、冒号、空格等危险字符
		if strings.Contains(s, "..") || strings.ContainsAny(s, "/\\: \t\r\n\x00") {
			return false
		}
		for _, c := range s {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
				return false
			}
		}
		return true
	}

	if path == "disabled" && r.Method == http.MethodGet {
		if h.pluginMgr == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not available"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"disabled": h.pluginMgr.ListDisabledPlugins()})
		return
	}

	if path == "reload" {
		if r.Method == http.MethodPost {
			if h.pluginMgr == nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin registry not available"})
				return
			}
			if _, err := h.pluginMgr.ReloadPlugins(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
			return
		}
		// reload/disabled 是保留字，不允许 DELETE/GET 等其它操作误把其当作插件名
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if idx := strings.LastIndex(path, "/"); idx > 0 {
		name := path[:idx]
		action := path[idx+1:]
		if r.Method == http.MethodPost {
			switch action {
			case "disable":
				if h.pluginMgr == nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not available"})
					return
				}
				if !validPluginName(name) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid plugin name"})
					return
				}
				if err := h.pluginMgr.DisablePlugin(name, "webui"); err != nil {
					// 未安装 → 404；已禁用 → 409；其余为真实内部错误
					status := http.StatusInternalServerError
					msg := err.Error()
					switch {
					case strings.HasSuffix(msg, "not installed"):
						status = http.StatusNotFound
					case strings.HasSuffix(msg, "already disabled"):
						status = http.StatusConflict
					}
					writeJSON(w, status, map[string]string{"error": msg})
					return
				}
				writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
				return

			case "start":
				// 运行态启动：只拉起，不写持久禁用清单 —— 与「启用」区分。
				// 前端「按需待命」的插件点「启动」走这里，不该顺手改持久配置。
				if h.pluginMgr == nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not available"})
					return
				}
				if !validPluginName(name) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid plugin name"})
					return
				}
				if err := h.pluginMgr.StartPlugin(name); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
				return

			case "stop":
				if h.pluginMgr == nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not available"})
					return
				}
				if !validPluginName(name) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid plugin name"})
					return
				}
				if err := h.pluginMgr.StopPlugin(name); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
				return

			case "enable":
				if h.pluginMgr == nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not available"})
					return
				}
				if !validPluginName(name) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid plugin name"})
					return
				}
				if err := h.pluginMgr.EnablePlugin(name); err != nil {
					// 启用失败多数是“插件不存在/加载失败” → 404 更贴切
					status := http.StatusInternalServerError
					if strings.Contains(err.Error(), "failed") {
						status = http.StatusNotFound
					}
					writeJSON(w, status, map[string]string{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, map[string]string{"status": "enabled"})
				return
			}
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 单段插件名路径（GET 详情 / DELETE 卸载）
	if !validPluginName(path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid plugin name"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.proxyToPluginmgr(w, r, "/plugins/"+path)
	case http.MethodDelete:
		h.proxyToPluginmgr(w, r, "/plugins/"+path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

package remotedevice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta("remotedevice", "远程设备网关", "Remote Device")
	plugin.RegisterFactory("remotedevice", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

const defaultAddr = "127.0.0.1:9890"

// Plugin 是 remotedevice 设备接入网关插件：
// 持有 HTTP 服务（WS 设备通道 + REST 管理面）与 devicectl Device（agent 工具）。
type Plugin struct {
	name     string
	registry *Registry
	mux      *http.ServeMux
	server   *http.Server
	addr     string
	token    string
	sdk      *sdk.PluginSDK
	dev      *devicectlDevice

	// devChansMu/devChans 维护"设备自报 id → 派生的通道名"。
	// 设备 id 是外部输入，不能直接进通道名（见 outputch.go 的 deviceChannelName）。
	devChansMu sync.Mutex
	devChans   map[string]string
}

func New(name string) *Plugin {
	return &Plugin{
		name:     name,
		registry: NewRegistry(),
		mux:      http.NewServeMux(),
	}
}

func (p *Plugin) Name() string { return p.name }

func genToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("tok-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func newReqID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s

	// ---- 设置 ----------------
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "listen_addr", Default: defaultAddr, Type: "string", DisplayName: "监听地址", Description: "设备网关 HTTP/WS 监听地址（默认 127.0.0.1:9890，仅本机）；填 127.0.0.1:0 让系统分配空闲端口", Category: "remotedevice"})
	// ---- 反代声明（HologramAgent 自带能力）----
	//
	// 设备网关需要被外部访问（设备/客户端要连 WS），但它默认只监听本机
	// 127.0.0.1:9890。声明后由 webui 的对外端口按 Host 子域反代出去，
	// **用户不需要额外开端口或配 frp 映射**。
	//
	// auth=none 是**刻意的**：调用方是设备与嵌入式客户端，不可能持有浏览器
	// 门户会话；本服务**自身已有接入令牌**（ws_token / X-API-Key），
	// 由 requireToken 强制校验。若这里声明 homeagent（默认值），会把设备链路
	// 全部挡在门户鉴权之外——那正是"认证必须可声明"的原因。
	//
	// websocket=true：设备注册/命令下发走 WS 长连接。
	s.RegisterProxy("gateway", sdk.ProxyDef{
		Name: "gateway",
		Host: "devices",
		// Path 让**非浏览器客户端**也能用：*.localhost 只有浏览器内置解析
		// 特例（RFC 6761），设备/固件/CLI 走系统解析器会以 no such host 失败。
		// 挂到门户自身 host 的路径下则无任何 DNS 依赖 —— 设备客户端沿用它
		// 已硬编码的 /api/v1/device/ws 路径即可，不需要知道反代的存在。
		Path:      "/api/v1/device",
		Target:    "127.0.0.1:9890",
		WebSocket: true,
		Auth:      sdk.ProxyAuthNone,
	})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "ws_token", Default: "", Type: "password", DisplayName: "接入 Token", Description: "设备绑定/接入时使用的令牌；留空启动时自动生成", Category: "remotedevice"})
	// 注意：不注册 authorized_devices 设置项 —— 鉴权在设备端执行（客户端存储），
	// 服务端不保存授权状态，避免 agent 经 config_set 工具自行授权。

	p.addr = defaultAddr
	if v, _ := s.Settings().Get("listen_addr"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			p.addr = s2
		}
	}
	p.token = ""
	if v, _ := s.Settings().Get("ws_token"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			p.token = s2
		}
	}
	if p.token == "" {
		p.token = genToken()
		if err := s.Settings().Set("ws_token", p.token); err != nil {
			log.Printf("[remotedevice] persist ws_token: %v", err)
		}
	}
	p.registry.SetAcceptToken(func(provided string) bool {
		return provided != "" && provided == p.token
	})

	// ---- devicectl Device（agent 工具） ----------------
	p.dev = &devicectlDevice{reg: p.registry}
	// screensee 视觉描述回调：截屏回传后用视觉模型描述屏幕内容
	p.dev.SetSeeHandler(p.describeScreen)
	if err := s.RegisterChannel("devicectl", p.dev); err != nil {
		log.Printf("[remotedevice] register devicectl channel: %v", err)
	}

	// ---- 媒体落盘目录：<data>/device_media ----------------
	// 设备回传的录像/照片等二进制聚合后写入此目录，cmd_result 返回 file 路径，
	// 避免 base64 内联撑爆 LLM 上下文。目录由 logManager/运维定期清理。
	if dataDir, err := s.Settings().GetCore("daemon.data_dir"); err == nil {
		if dd, ok := dataDir.(string); ok && dd != "" {
			p.registry.SetMediaDir(filepath.Join(dd, "device_media"))
		}
	}

	// ---- 设备通道随在线状态生灭（见 outputch.go 的 wireDeviceChannels）----
	p.wireDeviceChannels()

	// ---- 设备主动上报事件 → agent 注入 ----------------
	// 摄像头发现异常/传感器报警等场景：设备经 WS op=event 上报，
	// 插件将其格式化为文本经 SDK InjectText 异步注入 agent（source=device/{id}），
	// 同时发 EventBus 供 WebUI 展示。
	//
	// **注意**：agent 的输出**不会**被自动转回设备 —— 主动转发只有 webui 与 cli 两个
	// 交互界面（它们把最终回复渲染成对话气泡是本职）。设备要走
	// `output_send__device-<id>`（agent 主动调用），这才与"输出是 agent 的主动调用"一致。
	// 节流：同设备同类型事件 10s 内去重，防传感器风暴。
	lastEventAt := map[string]time.Time{}
	var eventMu sync.Mutex
	p.registry.SetEventHandler(func(deviceID string, msg map[string]interface{}) {
		evtType, _ := msg["type"].(string)
		if evtType == "" {
			evtType = "unknown"
		}
		key := deviceID + "|" + evtType
		eventMu.Lock()
		if last, ok := lastEventAt[key]; ok && time.Since(last) < 10*time.Second {
			eventMu.Unlock()
			log.Printf("[remotedevice] event throttled: %s from %s", evtType, deviceID)
			return
		}
		lastEventAt[key] = time.Now()
		eventMu.Unlock()

		// 组装人类可读的事件文本（agent 可直接理解）
		detail, _ := msg["detail"].(string)
		if detail == "" {
			if d, ok := msg["payload"].(map[string]interface{}); ok {
				b, _ := json.Marshal(d)
				detail = string(b)
			}
		}
		text := fmt.Sprintf("【设备事件上报】设备 %s 触发事件 %s", deviceID, evtType)
		if detail != "" {
			text += "：" + detail
		}
		text += "。请关注此事件并按需处理（如通知用户、调用相关工具核实）。"

		log.Printf("[remotedevice] event from %s: %s", deviceID, evtType)
		if p.sdk != nil {
			// 设备通道 device-<id> 是动态的（分隔符用 - 而非 /，见 deviceChannelName 的说明：
			// 通道名会进 LLM 函数名，必须满足 ^[a-zA-Z0-9_-]{1,64}$）。
			// 首次上报时**懒登记** inputch（Register 幂等），父 agent 才能把它划给驻留子。
			devCh := p.deviceChannelName(deviceID)
			_ = p.sdk.RegisterInputChannel(devCh, sdk.ChannelDef{})
			// 异步注入：不阻塞 WS 读循环；回复路由回 device/{id} 输出通道
			p.sdk.InjectInput(devCh, devCh, "text", map[string]interface{}{"content": text})
		}
	})

	// ---- REST 管理面 + WS 设备通道 ----------------
	p.registerRoutes()

	// 显式 net.Listen + Serve，而非 ListenAndServe：
	//
	// 1. 配 `127.0.0.1:0` 时只有 net.Listener 知道真实端口，ListenAndServe 拿不到。
	//    这不只是测试便利——它是 `:0` 语义能工作的前提（多实例/沙箱需要）。
	// 2. 监听失败必须**可见**：此前 ListenAndServe 在后台 goroutine 里报错，
	//    端口被占时只打一行日志、Start 仍返回 nil（插件表面「已加载」而网关根本没跑）。
	//    现在在 Start 里同步 Listen，把错误交给调用方。
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		return fmt.Errorf("remotedevice: 监听 %s 失败: %w", p.addr, err)
	}
	// 用**实际绑定**地址回写，使日志与诊断面显示真实端口（配 :0 时尤其重要）。
	p.addr = ln.Addr().String()

	p.server = &http.Server{Handler: p.mux}
	go func() {
		log.Printf("[remotedevice] device gateway listening on %s", p.addr)
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[remotedevice] server error: %v", err)
		}
	}()
	return nil
}

func (p *Plugin) registerRoutes() {
	// 设备通道（WS）
	p.mux.HandleFunc("/api/v1/device/ws", p.registry.ServeWS)
	// REST 管理面（全部需 token）
	// 注意：/api/v1/device/auth 已移除 —— 授权由设备端控制，服务端不提供授权接口。
	p.mux.HandleFunc("/api/v1/device", p.requireToken(p.handleDeviceList))
	p.mux.HandleFunc("/api/v1/device/online", p.requireToken(p.handleDeviceOnline))
	p.mux.HandleFunc("/api/v1/device/", p.requireToken(p.handleDeviceByID))
	p.mux.HandleFunc("/api/v1/device/push", p.requireToken(p.handleDevicePush))
}

// requireToken 校验 REST 请求的接入令牌（X-API-Key header 或 ?token=）。
func (p *Plugin) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("X-API-Key")
		if k == "" {
			k = r.URL.Query().Get("token")
		}
		if k == "" || k != p.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Plugin) handleDeviceList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"devices": p.registry.List()})
}

func (p *Plugin) handleDeviceOnline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"devices": p.registry.OnlineList()})
}

func (p *Plugin) handleDeviceByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/device/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		m, ok := p.registry.Get(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": "device not found"})
			return
		}
		writeJSON(w, http.StatusOK, m)
		return
	}
	http.NotFound(w, r)
}

func (p *Plugin) handleDevicePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req struct {
		DeviceID string                 `json:"device_id"`
		Payload  map[string]interface{} `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	if req.DeviceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "device_id required"})
		return
	}
	// ★ payload 必须校验，不能让它静默通过（2026-10-05 实测踩坑）。
	//
	// 本端点收的是**已成型的协议帧**（{"op":"cmd",...} 等），不是
	// “device_id + command” 这种便捷参数。以前直接PushJSON(req.Payload)，
	// 而 PushJSON 只是把 map 序列化后写进 socket —— 于是：
	//
	//   客户端发 {"device_id":"gui-JianF","command":"homeagent-clipboardsee"}
	//     → command 字段被**静默丢弃**，payload 为 nil
	//     → 设备收到 {"payload":null}，op 为空，直接忽略
	//     → 而服务端这边 PushJSON 写入 socket 成功，返回 {"status":"ok"}
	//
	// 一次「什么也没发生」的操作，报文上是彻底的成功。排查时只能靠设备侧
	// 日志才发现「命令没到」，极易误判成链路/鉴权/心跳问题（本次就绕了很久）。
	if req.Payload == nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "payload required: 期望 {\"device_id\":\"<id>\",\"payload\":{\"op\":\"cmd\",\"req_id\":\"<rid>\",\"cmd_type\":\"shell|homeagent\",\"command\":\"...\"}}；payload 是**协议帧本身**，不是 command 简写",
		})
		return
	}
	if err := p.registry.PushJSON(req.DeviceID, req.Payload); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

// describeScreen 用视觉模型描述设备屏幕截图（screensee 回调）。
// provider 为空时使用默认 LLM 源；模型不支持视觉时返回友好错误。
// describeScreen 把设备回传的屏幕截图交给视觉模型生成描述。
//
// ★★ 2026-10-05 签名改为 (string, error)（生产日志实证的教训）
//
//	原实现把 403 等**失败**渲染成一段普通文本返回：
//
//	    return fmt.Sprintf("屏幕截图视觉描述失败: %v（当前模型可能不支持图像输入）", err)
//
//	而调用方把它当作 description 字段、status=ok ⇒ **失败被包装成成功**。
//	实测（2026-10-05 13:51）：
//
//	    {"description":"屏幕截图视觉描述失败: api error 403:
//	               {\"error\":{\"message\":\"model \"gpt-5.6-sol\" is not allowed…"}
//
//	agent 读到的是一段自然语言，看不出这是工具失败，
//	于是又发了一次 screensee（换 provider）再失败一次。
//	与同一天修掉的 MCP `isError` 丢失是**同一个病**：失败与成功同形。
//
//	⇒ 失败必须以 error 上抛，文本只留作诊断线索。
func (p *Plugin) describeScreen(dataURL string, provider string) (string, error) {
	if p.sdk == nil || p.sdk.LLM() == nil {
		return "", fmt.Errorf("LLM 不可用，无法描述屏幕内容")
	}
	llm := p.sdk.LLM()
	req := &sdk.LLMCompletionRequest{
		MaxTokens: 2048,
		Messages: []sdk.LLMMessage{{
			Role: "user",
			Blocks: []sdk.LLMContentBlock{
				{Type: "text", Text: "这是用户设备的屏幕截图。请详细描述屏幕上显示的内容：正在运行的窗口/应用、可见的文字内容、界面状态等。如果是代码编辑器或终端，尽量转述关键文字信息。"},
				{Type: "image_url", ImageURL: dataURL},
			},
		}},
	}
	// 指定源：临时切换（低频操作，用完恢复原源）
	if provider != "" {
		prev := llm.CurrentSource()
		if err := llm.SetSource(provider); err != nil {
			// ★ 切源失败**不降级**：原来只打一行日志然后继续用当前源，
			//   而当前源可能与调用方指定的完全不同 ⇒ 模型看到的是
			//   「另一个源的结果」，却挂在指定源名下。直接报错更诚实。
			return "", fmt.Errorf("screensee: 切换到指定视觉源 %s 失败: %w", provider, err)
		} else if prev != "" {
			defer func() { _ = llm.SetSource(prev) }()
		}
	}
	// ★ 记录本次实际用的是哪个源：排查「为什么描述不对」时，
	//   第一件要知道的就是它到底是哪个源在回答。
	usedSource := llm.CurrentSource()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := llm.Chat(ctx, req)
	if err != nil {
		log.Printf("[remotedevice] screensee 视觉描述失败 source=%s err=%v", usedSource, err)
		// ★ 2026-10-05：模型名不被接受时，**就地降级到 AUTO 重试一次**。
		//
		// 生产实测（13:51）：agent 传 provider=visionllm，而那个源的
		// `model: gpt-5.6-sol` 被 llmsproxy 拒了：
		//
		//	model "gpt-5.6-sol" is not allowed for this key
		//
		// 根因不是「模型不支持图」，而是**那个 key 的 model scope 只有 AUTO**
		// （/etc/llmsproxy/config.yaml 的 keys[homeagent].models = [AUTO]）。
		// 而同一个 key + base_url 下 `model: AUTO` 实测完全能看图。
		//
		// ⇒ 显式模型名不可用时回 AUTO，比把错误抛给 agent 让它反复重试好。
		//   注意**不覆盖原错误**：两者都返回，由上层看到完整信息。
		if isModelNotAllowed(err) {
			log.Printf("[remotedevice] screensee: 源 %s 的模型名不被接受，回退 AUTO 重试", usedSource)
			if e2 := llm.SetSource("AUTO"); e2 == nil {
				rctx, rcancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer rcancel()
				if rresp, e3 := llm.Chat(rctx, req); e3 == nil && rresp != nil &&
					strings.TrimSpace(rresp.Content) != "" {
					log.Printf("[remotedevice] screensee AUTO 回退成功 len=%d", len(rresp.Content))
					return rresp.Content, nil
				} else {
					log.Printf("[remotedevice] screensee AUTO 回退也失败: %v", e3)
				}
			} else {
				log.Printf("[remotedevice] screensee AUTO 回退无法切源: %v", e2)
			}
		}
		// 保留原始错误文本（那是唯一诊断线索），但以 error 形态上抛。
		return "", fmt.Errorf("屏幕截图视觉描述失败（视觉源 %s，model=%s）: %w", usedSource, currentModelOf(p.sdk.Settings(), usedSource), err)
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		// 空内容与「失败」对模型同样难以区分，不能静默当成结果。
		return "", fmt.Errorf("屏幕截图视觉描述失败（视觉源 %s）: 模型返回空内容", usedSource)
	}
	log.Printf("[remotedevice] screensee 视觉描述完成 source=%s len=%d", usedSource, len(resp.Content))
	return resp.Content, nil
}

func (p *Plugin) Stop() error {
	// 注销全部设备通道：插件卸载/重载后这些通道不再有实现，
	// 留着会让 output_list_channels 骗模型。
	if p.sdk != nil {
		for _, m := range p.registry.List() {
			p.dropDeviceOutputChannel(m.DeviceID)
		}
	}
	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return p.server.Shutdown(ctx)
	}
	return nil
}

// isModelNotAllowed 判定错误是不是「这个 key 不许用该模型名」。
//
// ★ 2026-10-05：llmsproxy 的原文是
//
//	model "gpt-5.6-sol" is not allowed for this key
//
// （internal/gateway/chat.go 的 model_not_allowed）
//
// 只认这一类：它与「模型不存在」「key 无效」的处理完全不同 ——
// 前者换个模型名/回 AUTO 就能成，后者要动配置或凭证。
func isModelNotAllowed(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "is not allowed for this key") ||
		strings.Contains(s, "model_not_allowed") ||
		strings.Contains(s, "is not in this key's model scope")
}

// currentModelOf 取某个源当前配置的模型名（只用于错误信息，便于定位）。
//
// ★ 拿不到就返回 ""：**诊断信息缺失不该阻断主流程**，
//
//	所以这里不返回 error（内层的 fmt 已经带着原始错误文本了）。
func currentModelOf(settings interface {
	Get(string) (interface{}, error)
}, source string) string {
	if settings == nil {
		return ""
	}
	v, _ := settings.Get("core.llm.sources." + source + ".model")
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

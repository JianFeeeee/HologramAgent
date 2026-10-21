package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	"github.com/JianFeeeee/HologramAgent/internal/pluginview"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// DefaultSocket 由 main.go 在 Load() 前设置，覆盖默认 socket 路径。
var DefaultSocket string

const (
	cliSource  = "cli"
	cliChannel = "cli"
)

func init() {
	plugin.RegisterPluginMeta("cli", "CLI", "CLI")
	plugin.RegisterFactory("cli", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		sock := DefaultSocket
		if sock == "" {
			dataDir, ok := config["data_dir"].(string)
			if !ok {
				return nil, fmt.Errorf("cli plugin: config missing 'data_dir' or not a string")
			}
			sock = filepath.Join(dataDir, "cli.sock")
		}
		return New(name, sock), nil
	})
}

type Plugin struct {
	name       string
	socket     string
	ln         net.Listener
	mu         sync.Mutex
	wg         sync.WaitGroup
	termView   *pluginview.TerminalView
	termUnsubs []func()
}

func New(name, socketPath string) *Plugin {
	return &Plugin{
		name:   name,
		socket: socketPath,
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	// 终端状态归 agentcli，内核不再维护终端语义（2026-10-20 调整）。
	// CLI 与 WebUI 用同一份 internal/pluginview 实现订阅事件总线，
	// /terminals 与 /cmd/history 读自己攒的视图。
	p.termView = pluginview.NewTerminalView()
	p.termUnsubs = append(p.termUnsubs,
		s.Subscribe(sdk.EventTerminalOutput, func(evt *sdk.Event) { p.termView.OnTerminalOutput(evt.Payload) }),
		s.Subscribe(sdk.EventToolCall, func(evt *sdk.Event) { p.termView.OnToolCall(evt.Payload) }),
	)

	// inputch 先登记：本插件既用 "cli" 作输出目标，也用它注入输入（终端行）。
	// 输入侧必须显式登记，否则"把 inputch 划给驻留子"会找不到它。
	_ = s.RegisterInputChannel("cli", sdk.ChannelDef{})
	s.RegisterOutputChannel("cli", 1, "CLI 终端", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		if payload != "" {
			fmt.Println(payload)
			s.Publish(&sdk.Event{
				Type: sdk.EventAgentOutput,
				Payload: map[string]interface{}{
					"content": payload,
					"channel": "cli",
					"kind":    "channel_output",
				},
			})
		}
		return map[string]interface{}{"status": "ok"}, nil
	})

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "api_key", Type: "password", DisplayName: "CLI API 密钥",
		Description: "CLI 客户端连接时需提供的认证密钥（留空则使用 WebUI 密钥）",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "socket_path", Type: "string", DisplayName: "Socket 管道路径",
		Description: "CLI Unix 域套接字监听路径（留空则使用默认路径）",
	})
	if v, _ := s.Settings().Get("socket_path"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			p.socket = s
		}
	}

	dir := filepath.Dir(p.socket)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	os.Remove(p.socket)

	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		return fmt.Errorf("listen unix socket %s: %w", p.socket, err)
	}
	p.ln = ln

	os.Chmod(p.socket, 0666)

	p.wg.Add(1)
	go p.acceptLoop(s)

	log.Printf("[cli] unix socket listening on %s", p.socket)
	return nil
}

func (p *Plugin) acceptLoop(s *sdk.PluginSDK) {
	defer p.wg.Done()
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			break
		}
		p.wg.Add(1)
		go p.handleConn(conn, s)
	}
}

func (p *Plugin) handleConn(conn net.Conn, s *sdk.PluginSDK) {
	defer conn.Close()
	defer p.wg.Done()

	scanner := bufio.NewScanner(conn)

	apiKey := p.cliAPIKey(s)
	if apiKey != "" {
		if !scanner.Scan() {
			return
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "/auth ") || strings.TrimSpace(line[6:]) != apiKey {
			writeLine(conn, map[string]interface{}{
				"type":  "error",
				"error": "unauthorized",
			})
			return
		}
		writeLine(conn, map[string]interface{}{
			"type":    "response",
			"content": "authenticated",
		})
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if line[0] == '/' {
			if p.handleBuiltin(conn, line, s) {
				continue
			}
		}

		p.handleChat(&connWriter{conn: conn}, line, s)
	}
}

// connWriter 为单条连接提供互斥保护的 JSON 行写入。
// 对话过程中事件订阅回调运行在事件总线的发布 goroutine 上，
// 与主循环写最终响应并发，因此写入必须串行化。
type connWriter struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *connWriter) writeLine(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	w.mu.Lock()
	w.conn.Write(data)
	w.mu.Unlock()
}

// handleChat 处理一条对话消息：订阅内核的推理/工具调用事件并实时
// 转发给客户端（流式过程输出），InjectTextSync 返回后写出最终响应。
// 仅插件层改动：通过 SDK 订阅事件，不触碰内核。
func (p *Plugin) handleChat(w *connWriter, line string, s *sdk.PluginSDK) {
	unsubReasoning := s.Subscribe(sdk.EventReasoning, func(evt *sdk.Event) {
		if ch, _ := evt.Payload["channel"].(string); ch != cliChannel {
			return
		}
		content, _ := evt.Payload["content"].(string)
		if content == "" {
			return
		}
		w.writeLine(map[string]interface{}{"type": "reasoning", "content": content})
	})
	unsubToolCall := s.Subscribe(sdk.EventToolCall, func(evt *sdk.Event) {
		if ch, _ := evt.Payload["channel"].(string); ch != cliChannel {
			return
		}
		tool, _ := evt.Payload["tool"].(string)
		status, _ := evt.Payload["status"].(string)
		result, _ := evt.Payload["result"].(string)
		w.writeLine(map[string]interface{}{
			"type":   "tool_call",
			"tool":   tool,
			"status": status,
			"result": truncateOneLine(result, 160),
		})
	})
	// token 级流式增量帧：客户端可选订做逐 token 渲染。
	// 旧客户端收到未知 type 会忽略；聚合 reasoning/response 帧仍照常发送，
	// 保证旧/新客户端最终都能看到完整文本。
	unsubReasoningDelta := s.Subscribe(sdk.EventReasoningDelta, func(evt *sdk.Event) {
		if ch, _ := evt.Payload["channel"].(string); ch != cliChannel {
			return
		}
		content, _ := evt.Payload["content"].(string)
		if content == "" {
			return
		}
		w.writeLine(map[string]interface{}{"type": "reasoning_delta", "content": content})
	})
	unsubContentDelta := s.Subscribe(sdk.EventContentDelta, func(evt *sdk.Event) {
		if ch, _ := evt.Payload["channel"].(string); ch != cliChannel {
			return
		}
		content, _ := evt.Payload["content"].(string)
		if content == "" {
			return
		}
		w.writeLine(map[string]interface{}{"type": "content_delta", "content": content})
	})
	// stage：阶段管道（on_input / pre_action / post_action / toolcall / output …）。
	//
	// ★ 为何要补（2026-10-13 协议审计）：CLI 是**唯一**看不到阶段进展的客户端
	//   —— WebUI / GUI / 鸿蒙 都消费了 stage。后果具象：agent 跑长任务时
	//   终端长时间静默，用户分不清“在干活”还是“卡住了”。
	//
	// 为何单独发一帧而不是当普通文本：stage 是**过程信息**，与最终回复不同
	// 层级；客户端已有 [思考]/[工具] 前缀的渲染惯例，这里沿用同一约定。
	//
	// 过滤 _consolidation_：那是内核内部任务（记忆合并/归档），不属于用户
	// 这一轮对话；WebUI 也做了同样的过滤（见 dashboard.js 的 stage 处理）。
	unsubStage := s.Subscribe(sdk.EventStage, func(evt *sdk.Event) {
		ch, _ := evt.Payload["channel"].(string)
		if ch != cliChannel || ch == "_consolidation_" {
			return
		}
		phase, _ := evt.Payload["phase"].(string)
		if phase == "" {
			return
		}
		tool, _ := evt.Payload["tool"].(string)
		w.writeLine(map[string]interface{}{
			"type":  "stage",
			"phase": phase,
			"tool":  tool,
		})
	})
	// system：内核级状态事件（目前唯一发布者是 plugin_crashed）。
	//
	// ★ 为何要补（2026-10-13 协议审计）：该事件一直在发（dynamic_proc.go:175），
	//   注释还写着「webui/诊断插件可订阅」，但 CLI 完全接不到 —— 插件崩溃
	//   对终端用户不可见，只能去翻 journal。
	//
	// ★ 不过滤 channel：system 是**内核级**事件，不由某条输入通道产生，
	//   所以没有 channel 字段可比（这是它与其他事件的区别）。
	unsubSystem := s.Subscribe(sdk.EventSystem, func(evt *sdk.Event) {
		evtName, _ := evt.Payload["event"].(string)
		if evtName == "" {
			return
		}
		plugin, _ := evt.Payload["plugin"].(string)
		w.writeLine(map[string]interface{}{
			"type":   "system",
			"event":  evtName,
			"plugin": plugin,
		})
	})
	defer unsubReasoning()
	defer unsubToolCall()
	defer unsubReasoningDelta()
	defer unsubContentDelta()
	defer unsubStage()
	defer unsubSystem()

	resp := s.InjectTextSync(cliSource, cliChannel, line)
	if resp != nil {
		w.writeLine(chatResponseFrame(resp.Payload))
	} else {
		w.writeLine(map[string]interface{}{
			"type":  "error",
			"error": "agent is not available",
		})
	}
}

// chatResponseFrame 把内核的同步回执转成 chat 的 response 帧。
//
// 为何单独抽成函数：这是**无头调用方的唯一信息出口**
// （waiter -chat、基准 runner、脚本都靠它），而它此前只取了 content。
// 抽出来才能用判据钉住「哪些字段必须透传」——
// 症状形态是「写了、发了、不报错，只是收不到」，只有判据拦得住。
//
// 透传字段：
//   - content：回复正文；
//   - reasoning_content：思考内容（非空才带，不给普通回复挂空字段）；
//   - usage：用量（存在且非 nil 才带 —— 缺失与 0 是两回事，
//     消费方据此显示「—」而不是把「不知道」画成 0）。
func chatResponseFrame(payload map[string]interface{}) map[string]interface{} {
	frame := map[string]interface{}{
		"type":    "response",
		"content": "",
	}
	if payload == nil {
		return frame
	}
	if content, _ := payload["content"].(string); content != "" {
		frame["content"] = content
	}
	if reasoning, _ := payload["reasoning_content"].(string); reasoning != "" {
		frame["reasoning_content"] = reasoning
	}
	// 注意用「comma ok 且非 nil」而不是 != nil：
	// map 里存着一个 nil 值也是常见形态，那种情况同样算「没有」。
	if usage, ok := payload["usage"]; ok && usage != nil {
		frame["usage"] = usage
	}
	return frame
}

func (p *Plugin) cliAPIKey(s *sdk.PluginSDK) string {
	if s != nil {
		if v, _ := s.Settings().Get("api_key"); v != nil {
			if k, ok := v.(string); ok && k != "" {
				return k
			}
		}
	}
	return p.webuiAPIKey(s)
}

func (p *Plugin) webuiAPIKey(s *sdk.PluginSDK) string {
	if s == nil {
		return ""
	}
	v, _ := s.Settings().GetPlugin("webui", "api_key")
	if k, ok := v.(string); ok {
		return k
	}
	return ""
}

func (p *Plugin) handleBuiltin(conn net.Conn, line string, s *sdk.PluginSDK) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false
	}

	switch parts[0] {
	case "/help":
		p.cmdHelp(conn)
	case "/stop", "/interrupt":
		p.cmdInterrupt(conn, parts, s)
	case "/status":
		p.cmdStatus(conn, s)
	case "/kernel":
		p.cmdKernel(conn, s)
	case "/settings":
		p.cmdSettings(conn, parts, s)
	case "/plugin":
		p.cmdPlugin(conn, parts, s)
	case "/memory":
		p.cmdMemory(conn, parts, s)
	case "/knowledge":
		p.cmdKnowledge(conn, parts, s)
	case "/agents":
		p.cmdAgents(conn, s)
	case "/config":
		p.cmdConfig(conn, s)
	case "/tracker":
		p.cmdTracker(conn, parts, s)
	case "/adapters":
		p.cmdAdapters(conn, parts, s)
	case "/network":
		p.cmdNetwork(conn, s)
	case "/runtime":
		p.cmdRuntime(conn, s)
	case "/persona":
		p.cmdPersona(conn, parts, s)
	case "/terminals":
		p.cmdTerminals(conn, s)
	case "/cmd/history":
		p.cmdCmdHistory(conn, s)
	case "/terminal":
		p.cmdTerminal(conn, parts, s)
	default:
		return false
	}
	return true
}

func (p *Plugin) cmdHelp(conn net.Conn) {
	writeLine(conn, map[string]interface{}{
		"type": "response",
		"content": `内置命令（直接对话内核，不依赖网络）:
  /help                        显示此帮助
  /stop [消息]                 停止当前生成/发送中断消息（别名 /interrupt）
  /status                      系统运行状态
  /kernel                      内核状态（插件、工具、LLM、记忆）
  /settings                    列出所有配置
  /settings set <key> <val>    修改配置项
  /settings core.llm           按前缀筛选
  /plugin list                 列出所有插件
  /plugin install <url>        安装插件（需回环网络）
  /plugin remove <name>        卸载插件
  /plugin disable <name>       禁用插件
  /plugin enable <name>        启用插件
  /plugin info <name>          查看插件详情
  /plugin reload               重载插件
  /memory query <关键词>        查询图记忆
  /memory graph                导出整张图记忆快照
  /memory text [n]             最近 n 条文本记忆事件 + 统计
  /memory context [q]          组装后的记忆上下文（会注入什么）
  /memory tools                记忆工具定义 + 工具提示词
  /knowledge                   列出知识库
  /knowledge delete <name>     删除一条知识
  /knowledge stats             知识库统计
  /config [prefix]             导出内核配置（可按前缀筛选）
  /tracker                     变更追踪统计
  /tracker rollback            回滚本次会话的文件变更
  /adapters                    列出已加载的 Lua 适配器
  /adapters remove <name>      卸载适配器
  /network                     网络状态与 LLM 端点
  /runtime                     调度器/驻留子/通道拓扑快照
  /persona                     当前人格设定（/persona set <mode> [内容] 修改）
  /terminals                   终端会话列表（与 WebUI /terminals 同源）
  /cmd/history                 命令执行历史（cmd_run）
  /terminal create|write|read|close …  创建/写入/读取/关闭终端（调 agentcli 工具）
  /agents                      列出 Agent

其他文本直接发送给 Agent 处理。`,
	})
}

// ======== /stop ========

// cmdInterrupt 注入用户中断。核心拦截语义（interceptLoop）：
//   - 有 LLM 在跑：cancelLLM 取消当前流式请求，中断入队，process() 以
//     [中断消息] 重启轮次（模型看到被打断的上下文 + 用户新输入）；
//   - 无 LLM 在跑：作为普通输入处理（等同发了一条消息）。
//
// 可选附带消息：/stop 换个话题（空参数 = 纯取消）。
func (p *Plugin) cmdInterrupt(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	msg := strings.TrimSpace(strings.TrimPrefix(line2(parts), "/stop"))
	if alias := strings.TrimSpace(strings.TrimPrefix(line2(parts), "/interrupt")); alias != "" {
		msg = alias
	}
	// PriorityL3（交互）：/stop 是人在终端上当场下的指令，属于"需要及时处理"，
	// 不该用默认的 L1（后台）——那样它会被排在其它后台注入后面，停得不及时。
	payload := map[string]interface{}{
		"content":  msg,
		"priority": sdk.PriorityL3,
	}
	if msg == "" {
		// 无附带消息 = 停止：必须带 stop 标记，否则空内容会被 interceptLoop
		// 当空操作丢掉（旧行为：回了"已发送中断信号"但内核什么都没发生）。
		payload["stop"] = true
	}
	s.InjectInterrupt(cliSource, cliChannel, "text", payload)
	writeLine(conn, map[string]interface{}{
		"type":    "response",
		"content": "已发送中断信号",
	})
}

// line2 将命令行参数重组为原始字符串（保留词间空格，去掉首 token）。
func line2(parts []string) string {
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[1:], " ")
}

// ======== /status ========

func (p *Plugin) cmdStatus(conn net.Conn, s *sdk.PluginSDK) {
	st := s.Status()
	if st == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	ks := st.GetKernelStatus()

	llmStatus := "不可用"
	if ks.LLM.Available {
		llmStatus = fmt.Sprintf("%s (%d sources)", ks.LLM.Provider, ks.LLM.Sources)
	}
	memInfo := "未初始化"
	if ks.Memory.Available {
		memInfo = fmt.Sprintf("%d entities, %d relations", ks.Memory.EntityCount, ks.Memory.RelationCount)
	}
	runtime := fmt.Sprintf("goroutines=%d mem=%dMB", ks.Runtime.Goroutines, ks.Runtime.MemoryMB)
	uptime := ks.Uptime

	writeLine(conn, map[string]interface{}{
		"type": "response",
		"content": fmt.Sprintf(`HomeAgent 内核状态
  状态:   running
  运行:   %s
  LLM:    %s
  记忆:   %s
  运行时: %s
  插件:   %d loaded
  工具:   %d registered`, uptime, llmStatus, memInfo, runtime,
			len(ks.Plugins), len(ks.Tools)),
	})
}

// ======== /kernel ========

func (p *Plugin) cmdKernel(conn net.Conn, s *sdk.PluginSDK) {
	st := s.Status()
	if st == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	data, _ := json.MarshalIndent(st.GetKernelStatus(), "", "  ")
	writeLine(conn, map[string]interface{}{"type": "response", "content": string(data)})
}

// ======== /settings ========

func (p *Plugin) cmdSettings(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	sett := s.Settings()
	if sett == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "config registry not available"})
		return
	}

	if len(parts) >= 2 && parts[1] == "set" {
		if len(parts) < 4 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /settings set <key> <value>"})
			return
		}
		key := parts[2]
		val := strings.Join(parts[3:], " ")
		if err := sett.SetCore(key, val); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("已设置: %s = %s", key, val)})
		return
	}

	prefix := ""
	if len(parts) >= 2 {
		prefix = parts[1]
	}
	keys, _ := sett.ListCore(prefix)
	sort.Strings(keys)
	if len(keys) == 0 {
		writeLine(conn, map[string]interface{}{"type": "response", "content": "无匹配配置项"})
		return
	}
	var lines []string
	for _, k := range keys {
		v, _ := sett.GetCore(k)
		lines = append(lines, fmt.Sprintf("  %s = %v", k, v))
	}
	writeLine(conn, map[string]interface{}{
		"type":    "response",
		"content": fmt.Sprintf("配置 (%d 项):\n%s", len(keys), strings.Join(lines, "\n")),
	})
}

// ======== /plugin ========

func (p *Plugin) cmdPlugin(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	if len(parts) < 2 {
		writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin list|install <url>|remove <name>|info <name>|disable <name>|enable <name>"})
		return
	}

	pmgr := s.PluginMgr()

	switch parts[1] {
	case "list":
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		names := pmgr.ListLoadedPlugins()
		disabled := pmgr.ListDisabledPlugins()
		disabledNames := make(map[string]bool)
		for _, d := range disabled {
			disabledNames[d.Name] = true
		}
		all := make(map[string]string)
		for _, n := range names {
			all[n] = "\033[32m已加载\033[0m"
		}
		for _, d := range disabled {
			if _, ok := all[d.Name]; !ok {
				all[d.Name] = "\033[31m已禁用\033[0m"
			} else {
				all[d.Name] = "\033[32m已加载\033[0m (禁用将在重启后生效)"
			}
		}
		if len(all) == 0 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "无插件"})
			return
		}
		var lines []string
		for name, status := range all {
			lines = append(lines, fmt.Sprintf("  %s %s", name, status))
		}
		sort.Strings(lines)
		writeLine(conn, map[string]interface{}{
			"type":    "response",
			"content": fmt.Sprintf("插件 (%d):\n%s", len(all), strings.Join(lines, "\n")),
		})

	case "install":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin install <url>"})
			return
		}
		// 与 WebUI 同一实现：插件安装/升级逻辑在 pluginmgr 插件里，它监听一个
		// 回环 HTTP 地址（默认 127.0.0.1:9876，无鉴权）。WebUI 也是转发到它，
		// 这里直连同一端点，不再只打印一句“请去 WebUI”。
		addr := "127.0.0.1:9876"
		if v, err := s.Settings().GetPlugin("pluginmgr", "http_addr"); err == nil {
			if str, ok := v.(string); ok && str != "" {
				addr = str
			}
		}
		reqBody, _ := json.Marshal(map[string]interface{}{"url": parts[2]})
		resp, err := http.Post("http://"+addr+"/plugins", "application/json", strings.NewReader(string(reqBody)))
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "无法连接 pluginmgr(" + addr + "): " + err.Error()})
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		writeLine(conn, map[string]interface{}{"type": "response", "content": string(data)})

	case "remove":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin remove <name>"})
			return
		}
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		name := parts[2]
		dir := pmgr.PluginDir()
		if dir == "" {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin dir not configured"})
			return
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		// 同步清理禁用表
		_ = pmgr.EnablePlugin(name)
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %s 已删除，执行 /plugin reload 生效", name)})

	case "info":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin info <name>"})
			return
		}
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		name := parts[2]
		metas := pmgr.PluginMetas()
		meta, hasMeta := metas[name]
		loaded := false
		for _, n := range pmgr.ListLoadedPlugins() {
			if n == name {
				loaded = true
				break
			}
		}
		if loaded {
			display := name
			if hasMeta && meta.NameZh != "" {
				display = meta.NameZh
			}
			writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("名称: %s (%s)\n状态: 已加载", display, name)})
			return
		}
		if pmgr.IsPluginDisabled(name) {
			writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %q 已禁用", name)})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %q 未安装", name)})

	case "disable":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin disable <name>"})
			return
		}
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		if err := pmgr.DisablePlugin(parts[2], "cli"); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %s 已禁用", parts[2])})

	case "enable":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin enable <name>"})
			return
		}
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		if err := pmgr.EnablePlugin(parts[2]); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %s 已启用", parts[2])})

	case "reload":
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		if _, err := pmgr.ReloadPlugins(); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": "插件已重载"})

	default:
		writeLine(conn, map[string]interface{}{"type": "response", "content": "未知: /plugin " + parts[1] + "。支持: list, install, remove, info, disable, enable, reload"})
	}
}

// ======== /memory ========

// writeJSONContent 把一个结构以缩进 JSON 写入 response 帧。
// CLI 与 WebUI 对齐的口径：结构化数据一律 JSON（带缩进便于人读），
// 不再每个子命令各拼一种文本格式。
func writeJSONContent(conn net.Conn, v interface{}) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
		return
	}
	writeLine(conn, map[string]interface{}{"type": "response", "content": string(data)})
}

func (p *Plugin) cmdMemory(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	sub := ""
	if len(parts) >= 2 {
		sub = parts[1]
	}
	switch sub {
	case "query":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /memory query <关键词>"})
			return
		}
		q := strings.Join(parts[2:], " ")
		mem := s.Memory()
		if mem == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "memory not available"})
			return
		}
		entities, relations, err := mem.Recall([]string{q}, 2)
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeJSONContent(conn, map[string]interface{}{
			"entities":  entities,
			"relations": relations,
		})
	case "graph":
		mem := s.Memory()
		if mem == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "memory not available"})
			return
		}
		data, err := mem.GraphData()
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeJSONContent(conn, data)
	case "text":
		tm := s.TextMemory()
		if tm == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "text memory not available"})
			return
		}
		n := 20
		if len(parts) >= 3 {
			if v, err := strconv.Atoi(parts[2]); err == nil && v > 0 {
				n = v
			}
		}
		events, err := tm.RecentEvents(n)
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeJSONContent(conn, map[string]interface{}{
			"events": events,
			"stats":  tm.Stats(),
		})
	case "context":
		// 与 WebUI GET /api/v1/memory/context 同源：都走 IndexerAPI。
		// 打印“输进去会拼成什么上下文”，是排查召回质量最直接的一眼。
		idx := s.Indexer()
		if idx == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "indexer not available"})
			return
		}
		q := ""
		if len(parts) >= 3 {
			q = strings.Join(parts[2:], " ")
		}
		injected, err := idx.BuildContext(q)
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeJSONContent(conn, map[string]interface{}{
			"context":        idx.FormatContext(injected),
			"summary":        injected.Summary,
			"entities":       injected.Entities,
			"token_estimate": injected.TokenEstimate,
		})
	case "tools":
		// 与 WebUI GET /api/v1/memory/tools 同源。
		idx := s.Indexer()
		if idx == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "indexer not available"})
			return
		}
		writeJSONContent(conn, map[string]interface{}{
			"tools":       idx.GetToolDefinitions(),
			"tool_prompt": idx.BuildToolPrompt(),
		})
	default:
		writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /memory query <关键词> | /memory graph | /memory text [n] | /memory context [q] | /memory tools"})
	}
}

// ======== /knowledge ========

func (p *Plugin) cmdKnowledge(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	ks := s.Knowledge()
	if ks == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "knowledge not available"})
		return
	}
	sub := "list"
	if len(parts) >= 2 {
		sub = parts[1]
	}
	switch sub {
	case "list":
		items, err := ks.List()
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeJSONContent(conn, items)
	case "delete", "remove":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /knowledge delete <name>"})
			return
		}
		if err := ks.Remove(parts[2]); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": "已删除知识: " + parts[2]})
	case "stats":
		writeJSONContent(conn, ks.Stats())
	default:
		writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /knowledge | /knowledge delete <name> | /knowledge stats"})
	}
}

// ======== /config ========

func (p *Plugin) cmdConfig(conn net.Conn, s *sdk.PluginSDK) {
	cfg := s.Config()
	if cfg == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "config not available"})
		return
	}
	writeJSONContent(conn, cfg.Get())
}

// ======== /tracker ========

func (p *Plugin) cmdTracker(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	tr := s.Tracker()
	if tr == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "tracker not available"})
		return
	}
	if len(parts) >= 2 && parts[1] == "rollback" {
		if err := tr.Rollback(); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": "已回滚文件变更"})
		return
	}
	writeJSONContent(conn, map[string]interface{}{
		"stats":       tr.Stats(),
		"has_changes": tr.HasChanges(),
		"changesets":  tr.ChangeSets(),
	})
}

// ======== /adapters ========

func (p *Plugin) cmdAdapters(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	ad := s.Adapter()
	if ad == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "lua adapter not available"})
		return
	}
	if len(parts) >= 3 && (parts[1] == "remove" || parts[1] == "delete") {
		if err := ad.Remove(parts[2]); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": "已卸载适配器: " + parts[2]})
		return
	}
	writeJSONContent(conn, map[string]interface{}{"adapters": ad.List()})
}

// ======== /persona ========

// cmdPersona 与 WebUI 的 GET/POST /api/v1/persona 同口径。
//
// 人格的读写落在 core.agent.personal_prompt / core.internal.persona_initialized
// 两个配置键上，而插件 SDK 的 Settings() 恰好满足 internal/config.PersonaKV
// （GetCore/SetCore）—— WebUI 也是直接把 settings 传进去的，所以内部插件同样能做。
func (p *Plugin) cmdPersona(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	sett := s.Settings()
	if sett == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "settings not available"})
		return
	}
	if len(parts) >= 2 && parts[1] == "set" {
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /persona set default|custom|later [内容]"})
			return
		}
		mode := parts[2]
		content := strings.Join(parts[3:], " ")
		restart, err := config.SetPersonaKV(sett, mode, content)
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeJSONContent(conn, map[string]interface{}{
			"status": "ok", "mode": mode, "restart_required": restart,
		})
		return
	}
	// 存在人格文件时它优先（与 WebUI 一致，路径取 core.daemon.data_dir）
	fileOverride := false
	if v, err := sett.GetCore("core.daemon.data_dir"); err == nil {
		if dir, ok := v.(string); ok && dir != "" {
			if _, statErr := os.Stat(filepath.Join(dir, "personal", "personal.md")); statErr == nil {
				fileOverride = true
			}
		}
	}
	writeJSONContent(conn, map[string]interface{}{
		"initialized":    config.PersonaInitializedKV(sett),
		"current_prompt": config.CurrentPersonaKV(sett),
		"file_override":  fileOverride,
	})
}

// ======== /runtime ========

// cmdRuntime 与 WebUI 的 GET /api/v1/runtime 同口径。
//
// 数据来自 SDK 已经对内部插件开放的 KernelStatus（s.Status().GetKernelStatus()），
// 不是 WebUI 专属：Scheduler / Residents / Channels / InputChannels 都在里面。
// 之前 CLI 没接这一条，是漏接，不是没开放。
func (p *Plugin) cmdRuntime(conn net.Conn, s *sdk.PluginSDK) {
	st := s.Status()
	if st == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	ks := st.GetKernelStatus()
	if ks == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "kernel status not available"})
		return
	}
	writeJSONContent(conn, map[string]interface{}{
		"uptime":         ks.Uptime,
		"agent_id":       ks.AgentID,
		"scheduler":      ks.Scheduler,
		"residents":      ks.Residents,
		"channels":       ks.Channels,
		"input_channels": ks.InputChannels,
	})
}

// ======== /network ========

// cmdNetwork 与 WebUI 的 GET /api/v1/network 同口径：网络状态 + LLM 端点。
func (p *Plugin) cmdNetwork(conn net.Conn, s *sdk.PluginSDK) {
	var endpoints interface{}
	if cfg := s.Config(); cfg != nil {
		if c := cfg.Get(); c != nil {
			endpoints = c.Defaults.LLMEndpoints
		}
	}
	writeJSONContent(conn, map[string]interface{}{
		"network_status": "monitoring",
		"endpoints":      endpoints,
	})
}

// ======== /agents ========

// cmdAgents 与 WebUI 的 GET /api/v1/agents 同口径：返回受监管的 agent 列表。
// 之前只回了内核自己的 agent_id，驻留子信息全丢——而这正是 supervisor 对插件
// 已经开放的 ListAgents()。
func (p *Plugin) cmdAgents(conn net.Conn, s *sdk.PluginSDK) {
	if sup := s.Supervisor(); sup != nil {
		if agents := sup.ListAgents(); len(agents) > 0 {
			writeJSONContent(conn, map[string]interface{}{"agents": agents})
			return
		}
	}
	// 退化：supervisor 不可用时至少给出内核 agent_id
	st := s.Status()
	if st == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	ks := st.GetKernelStatus()
	writeJSONContent(conn, map[string]interface{}{"agents": []interface{}{map[string]string{"id": ks.AgentID, "state": "running"}}})
}

// ======== /terminals /cmd/history /terminal ========

// 终端视图：订阅事件总线攒的（与 WebUI 共用 internal/pluginview 实现）。
// 状态真值在 agentcli；内核不参与终端语义。

// cmdTerminals 返回终端会话快照，与 WebUI 的 GET /api/v1/terminals 同口径。
func (p *Plugin) cmdTerminals(conn net.Conn, s *sdk.PluginSDK) {
	if p.termView == nil {
		writeJSONContent(conn, map[string]interface{}{"terminals": []interface{}{}})
		return
	}
	terms, _ := p.termView.Snapshot()
	if terms == nil {
		terms = []sdk.TerminalStatus{}
	}
	writeJSONContent(conn, map[string]interface{}{"terminals": terms})
}

// cmdCmdHistory 返回命令历史快照，与 WebUI 的 GET /api/v1/cmd/history 同口径。
func (p *Plugin) cmdCmdHistory(conn net.Conn, s *sdk.PluginSDK) {
	if p.termView == nil {
		writeJSONContent(conn, map[string]interface{}{"history": []interface{}{}})
		return
	}
	_, cmds := p.termView.Snapshot()
	if cmds == nil {
		cmds = []sdk.CmdExecStatus{}
	}
	writeJSONContent(conn, map[string]interface{}{"history": cmds})
}

// cmdTerminal 通过 ToolAPI.ExecuteTool 调 agentcli 的终端工具。
//
// 终端本体属于 agentcli 插件（terminal_create/write/read/close），SDK 的
// ToolAPI.ExecuteTool 已允许跨插件调用工具，所以 CLI 不必新增接口就能开/写/读/关。
func (p *Plugin) cmdTerminal(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	if len(parts) < 2 {
		writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /terminal create <命令> | write <id> <输入> | read <id> | close <id>"})
		return
	}
	tools := s.Tool()
	if tools == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "tool api not available"})
		return
	}
	switch parts[1] {
	case "create":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /terminal create <命令>"})
			return
		}
		res, err := tools.ExecuteTool("terminal_create", map[string]interface{}{"command": strings.Join(parts[2:], " ")})
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeJSONContent(conn, res)
	case "write":
		if len(parts) < 4 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /terminal write <id> <输入>"})
			return
		}
		res, err := tools.ExecuteTool("terminal_write", map[string]interface{}{
			"id": parts[2], "input": strings.Join(parts[3:], " "),
		})
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeJSONContent(conn, res)
	case "read", "close":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /terminal " + parts[1] + " <id>"})
			return
		}
		res, err := tools.ExecuteTool("terminal_"+parts[1], map[string]interface{}{"id": parts[2]})
		if err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeJSONContent(conn, res)
	default:
		writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /terminal create|write|read|close …"})
	}
}

// ======== helpers ========

// truncateOneLine 将多行文本压成单行并按 rune 截断，用于事件结果预览。
func truncateOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func writeLine(conn net.Conn, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	conn.Write(data)
}

func (p *Plugin) Stop() error {
	for _, unsub := range p.termUnsubs {
		if unsub != nil {
			unsub()
		}
	}
	p.termUnsubs = nil

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
	}
	p.wg.Wait()
	os.Remove(p.socket)
	return nil
}

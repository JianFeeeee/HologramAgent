package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// 配色（对齐 deveco-code 深色主题：12 阶灰阶 + 语义色，无 emoji，纯文本标记）
// ---------------------------------------------------------------------------

var (
	cStep3   = lipgloss.Color("#1e1e1e")
	cStep7   = lipgloss.Color("#484848")
	cStep11  = lipgloss.Color("#808080")
	cStep12  = lipgloss.Color("#eeeeee")
	cPrimary = lipgloss.Color("#fab283") // 主色（暖橙）
	cAccent  = lipgloss.Color("#9d7cd8") // 紫
	cGreen   = lipgloss.Color("#7fd88f")
	cRed     = lipgloss.Color("#e06c75")
	cYellow  = lipgloss.Color("#e5c07b")
	cCyan    = lipgloss.Color("#56b6c2")
)

var (
	styleHeaderBox   = lipgloss.NewStyle().Foreground(cStep12).Background(cStep3).Padding(0, 1)
	styleTitle       = lipgloss.NewStyle().Bold(true).Foreground(cPrimary)
	styleDotOn       = lipgloss.NewStyle().Foreground(cGreen)
	styleDotOff      = lipgloss.NewStyle().Foreground(cRed)
	styleDim         = lipgloss.NewStyle().Foreground(cStep11)
	styleUserTag     = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	styleAgentTag    = lipgloss.NewStyle().Bold(true).Foreground(cPrimary)
	styleSysTag      = lipgloss.NewStyle().Bold(true).Foreground(cCyan)
	styleErrTag      = lipgloss.NewStyle().Bold(true).Foreground(cRed)
	styleReason      = lipgloss.NewStyle().Foreground(cStep11)
	styleInputBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cStep7)
	styleInputFocus  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cPrimary)
	styleStatusBar   = lipgloss.NewStyle().Foreground(cStep11).Background(cStep3).Padding(0, 1)
	styleSep         = lipgloss.NewStyle().Foreground(cStep7)
	styleToolPending = lipgloss.NewStyle().Foreground(cYellow)
	styleToolOK      = lipgloss.NewStyle().Foreground(cGreen)
	styleToolFail    = lipgloss.NewStyle().Foreground(cRed)
)

// ---------------------------------------------------------------------------
// 消息模型
// ---------------------------------------------------------------------------

type msgKind int

const (
	msgUser msgKind = iota
	msgAgent
	msgReasoning
	msgTool
	msgSystem
	msgError
)

type chatMsg struct {
	kind   msgKind
	text   string
	tool   string // msgTool: 工具名
	status string // msgTool: ok/denied/interrupted/error/running
	result string // msgTool: 结果预览
	final  bool   // msgAgent: 流式消息已完成（后续 delta 不再追加）
}

// ---- tea.Msg ----

type spinnerTickMsg struct{}

type serverLineMsg struct{ line string }

type readerErrMsg struct {
	err error
	gen int
}

type reconnectDoneMsg struct{ ok bool }

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type tuiModel struct {
	state *State
	cfg   *Config

	vp    viewport.Model
	input textarea.Model

	messages []chatMsg
	width    int
	height   int
	ready    bool

	busy       bool
	spinnerIdx int

	history      *History
	historyIdx   int // -1 = 无导航；0..n-1 = history.all() 下标（越大越新）
	historyDraft string

	lines       chan string
	errs        chan readerErrMsg
	readerGen   int // 当前 reader 世代；重启时递增
	readerAlive bool

	reconnecting bool
}

func newTuiModel(state *State, cfg *Config, history *History, lines chan string, errs chan readerErrMsg) tuiModel {
	ti := textarea.New()
	ti.Placeholder = "输入消息，/help 查看命令"
	ti.Prompt = ""
	ti.CharLimit = -1
	ti.SetHeight(1)
	ti.ShowLineNumbers = false
	ti.Focus()

	return tuiModel{
		state:       state,
		cfg:         cfg,
		input:       ti,
		vp:          viewport.New(80, 20),
		history:     history,
		historyIdx:  -1,
		lines:       lines,
		errs:        errs,
		readerAlive: true,
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, waitServer(m.lines, m.errs))
}

// waitServer 阻塞等待下一行服务器输出或读错误。
func waitServer(lines chan string, errs chan readerErrMsg) tea.Cmd {
	return func() tea.Msg {
		select {
		case l := <-lines:
			return serverLineMsg{l}
		case e := <-errs:
			return e
		}
	}
}

func spinTick() tea.Cmd {
	return tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

// spinCmd busy 时启动 spinner tick 循环。
func (m tuiModel) spinCmd() tea.Cmd {
	if !m.busy {
		return nil
	}
	return spinTick()
}

// reconnectCmd 后台重连（State 自带锁，goroutine 安全）。
func (m tuiModel) reconnectCmd() tea.Cmd {
	return func() tea.Msg {
		m.state.Disconnect()
		for i := 0; i < 15; i++ {
			if err := m.state.Connect(m.cfg); err == nil {
				return reconnectDoneMsg{ok: true}
			}
			time.Sleep(time.Second)
		}
		return reconnectDoneMsg{ok: false}
	}
}

// restartReader 重启读循环 goroutine。
func (m *tuiModel) restartReader() {
	m.readerGen++ // 使旧 reader 的迟到错误失效
	m.readerAlive = true
	go m.readPump(m.readerGen)
}

// readPump 持续读服务器输出并投递到 channel；出错时投递带世代号的 err 后退出。
func (m *tuiModel) readPump(gen int) {
	for {
		line, err := m.state.readLine()
		if err != nil {
			select {
			case m.errs <- readerErrMsg{err: err, gen: gen}:
			default:
			}
			return
		}
		select {
		case m.lines <- line:
		case <-time.After(30 * time.Second):
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.ready = true
			m.append(chatMsg{kind: msgSystem,
				text: fmt.Sprintf("connected %s://%s", m.modeLabel(), m.addrLabel())})
		}
		m.layout()

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyCtrlD:
			return m, tea.Quit
		case tea.KeyEnter:
			return m.handleSubmit()
		case tea.KeyUp:
			lines := m.history.all()
			if len(lines) == 0 {
				return m, nil
			}
			if m.historyIdx == -1 {
				m.historyDraft = m.input.Value()
				m.historyIdx = len(lines) - 1
			} else if m.historyIdx > 0 {
				m.historyIdx--
			}
			m.input.SetValue(lines[m.historyIdx])
			return m, nil
		case tea.KeyDown:
			if m.historyIdx >= 0 {
				m.historyIdx++
				if m.historyIdx >= len(m.history.all()) {
					m.historyIdx = -1
					m.input.SetValue(m.historyDraft)
				} else {
					m.input.SetValue(m.history.all()[m.historyIdx])
				}
			}
			return m, nil
		case tea.KeyPgUp:
			m.vp.HalfPageUp()
			return m, nil
		case tea.KeyPgDown:
			m.vp.HalfPageDown()
			return m, nil
		}

	case spinnerTickMsg:
		if m.busy {
			m.spinnerIdx++
			return m, spinTick()
		}

	case serverLineMsg:
		m.handleServerLine(msg.line)
		var cmds []tea.Cmd
		if m.readerAlive {
			cmds = append(cmds, waitServer(m.lines, m.errs))
		}
		return m, tea.Batch(cmds...)

	case readerErrMsg:
		if msg.gen != m.readerGen {
			// 旧 reader 的迟到错误：新 reader 已在运行，忽略
			return m, nil
		}
		m.readerAlive = false
		if !m.reconnecting {
			m.append(chatMsg{kind: msgError, text: "connection lost: " + msg.err.Error()})
		}
		return m, nil

	case reconnectDoneMsg:
		m.reconnecting = false
		if msg.ok {
			m.restartReader()
			m.append(chatMsg{kind: msgSystem, text: "reconnected"})
			return m, waitServer(m.lines, m.errs)
		}
		m.append(chatMsg{kind: msgError, text: "reconnect failed after 15 attempts"})
		return m, nil
	}

	// 其余按键交给输入框
	var icmd tea.Cmd
	m.input, icmd = m.input.Update(msg)
	var vcmd tea.Cmd
	m.vp, vcmd = m.vp.Update(msg)
	return m, tea.Batch(icmd, vcmd)
}

func (m tuiModel) handleSubmit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	m.input.Reset()
	m.historyIdx = -1
	if text == "" {
		return m, nil
	}

	// TUI 自己处理的内置命令
	switch text {
	case "/exit", "/quit":
		return m, tea.Quit
	case "/clear":
		m.messages = nil
		m.refreshViewport()
		return m, nil
	case "/reconnect":
		m.append(chatMsg{kind: msgSystem, text: "reconnecting..."})
		m.reconnecting = true
		return m, m.reconnectCmd()
	}

	// 其余内置命令：捕获输出进消息区
	if strings.HasPrefix(text, "/") {
		needReconnect := false
		var buf strings.Builder
		handled := handleBuiltin(text, m.cfg, m.state, func() { needReconnect = true }, &buf)
		if handled {
			if out := strings.TrimRight(buf.String(), "\n"); out != "" {
				m.append(chatMsg{kind: msgSystem, text: out})
			}
			if needReconnect {
				m.reconnecting = true
				return m, m.reconnectCmd()
			}
			// 即使内置命令也可能触发服务器回复（如 /status），所以继续保持监听
			if m.readerAlive {
				return m, waitServer(m.lines, m.errs)
			}
			return m, nil
		}
	}

	// 普通消息：发给 agent
	m.append(chatMsg{kind: msgUser, text: text})
	m.history.add(text)
	m.history.save()
	m.busy = true

	if err := m.state.Send(text); err != nil {
		m.busy = false
		m.append(chatMsg{kind: msgError, text: "send failed: " + err.Error()})
		m.reconnecting = true
		return m, m.reconnectCmd()
	}
	if m.readerAlive {
		return m, tea.Batch(waitServer(m.lines, m.errs), m.spinCmd())
	}
	return m, m.spinCmd()
}

func (m *tuiModel) handleServerLine(line string) {
	rl := parseRespLineStruct(line)
	switch rl.Type {
	case "reasoning":
		cm := chatMsg{kind: msgReasoning, text: rl.Content}
		// 连续 reasoning 增量合并到最后一条，形成流式效果
		if n := len(m.messages); n > 0 && m.messages[n-1].kind == msgReasoning {
			m.messages[n-1].text += rl.Content
		} else {
			m.append(cm)
		}
	case "reasoning_delta":
		// token 级增量：与 reasoning 同样合并到最后一条 reasoning 消息
		if rl.Reset {
			m.sealLastAgent()
			break
		}
		if n := len(m.messages); n > 0 && m.messages[n-1].kind == msgReasoning {
			m.messages[n-1].text += rl.Content
		} else if rl.Content != "" {
			m.append(chatMsg{kind: msgReasoning, text: rl.Content})
		}
	case "content_delta":
		// token 级增量：追加到最后一条 agent 消息（流式生成中的回复）
		if rl.Reset {
			m.sealLastAgent()
			break
		}
		if n := len(m.messages); n > 0 && m.messages[n-1].kind == msgAgent && !m.messages[n-1].final {
			m.messages[n-1].text += rl.Content
		} else if rl.Content != "" {
			m.append(chatMsg{kind: msgAgent, text: rl.Content})
		}
	case "tool_call":
		// 工具调用打断内容流：置 final 防止后续 delta 误追加到旧消息
		m.sealLastAgent()
		m.append(chatMsg{kind: msgTool, tool: rl.Tool, status: rl.Status, result: rl.Result})
	case "response":
		// 聚合最终响应：覆盖/替换 delta 累积的最后一条 agent 消息（内容相同），
		// 或在无 delta 时新建。置 final 标记本轮完成。
		m.busy = false
		if n := len(m.messages); n > 0 && m.messages[n-1].kind == msgAgent && !m.messages[n-1].final {
			if rl.Content != "" {
				m.messages[n-1].text = rl.Content // 以聚合为准（含 stage 插件改写后的最终文本）
			}
			m.messages[n-1].final = true
		} else {
			cm := chatMsg{kind: msgAgent, text: rl.Content, final: true}
			m.append(cm)
		}
	case "error":
		m.busy = false
		m.sealLastAgent()
		m.append(chatMsg{kind: msgError, text: rl.Error})
	default:
		// 非 JSON 旧行（旧服务器）：当最终输出
		m.busy = false
		m.append(chatMsg{kind: msgAgent, text: line, final: true})
	}
	m.refreshViewport()
}

// sealLastAgent 将最后一条未完成的 agent 流式消息标记为完成。
func (m *tuiModel) sealLastAgent() {
	if n := len(m.messages); n > 0 && m.messages[n-1].kind == msgAgent {
		m.messages[n-1].final = true
	}
}

func (m *tuiModel) append(cm chatMsg) {
	m.messages = append(m.messages, cm)
	m.refreshViewport()
}

func (m *tuiModel) modeLabel() string {
	if m.cfg.Remote != "" {
		return "remote"
	}
	return "local"
}

func (m *tuiModel) addrLabel() string {
	if m.cfg.Remote != "" {
		return m.cfg.Remote
	}
	return m.cfg.Socket
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

const tagWidth = 5

func (m tuiModel) renderMessage(cm chatMsg, width int) string {
	switch cm.kind {
	case msgUser:
		return hangingIndent(styleUserTag.Render("You"), cm.text, width)
	case msgAgent:
		return hangingIndent(styleAgentTag.Render("小宅"), cm.text, width)
	case msgReasoning:
		tail := lastNonEmptyLine(cm.text)
		if w := width - tagWidth - 4; w > 8 && lipgloss.Width(tail) > w {
			tail = truncateTail(tail, w)
		}
		return styleDim.Render("  · " + tail)
	case msgTool:
		var mark string
		var st lipgloss.Style
		switch cm.status {
		case "ok":
			mark, st = "[ok]", styleToolOK
		case "denied", "interrupted", "error":
			mark, st = "[fail]", styleToolFail
		default:
			mark, st = "[..]", styleToolPending
		}
		preview := ""
		if cm.result != "" {
			first := firstLine(cm.result)
			if w := width - tagWidth - 24; w > 8 && lipgloss.Width(first) > w {
				first = truncateTail(first, w)
			}
			preview = styleDim.Render(" " + first)
		}
		return "  " + st.Render(fmt.Sprintf("%-6s", mark)) +
			styleDim.Render(fmt.Sprintf("%-*s", 18, cm.tool)) + preview
	case msgSystem:
		return styleSysTag.Render("[sys]") + " " + styleDim.Render(cm.text)
	case msgError:
		return styleErrTag.Render("[err]") + " " +
			lipgloss.NewStyle().Foreground(cRed).Render(cm.text)
	}
	return ""
}

// hangingIndent 两列布局：首行 "tag body"，续行缩进对齐 body。
func hangingIndent(tag, body string, width int) string {
	indent := tagWidth + 1
	avail := width - indent
	if avail < 10 {
		avail = 10
	}
	pad := indent - lipgloss.Width(tag)
	if pad < 1 {
		pad = 1
	}
	prefix := tag + strings.Repeat(" ", pad)
	var b strings.Builder
	first := true
	for _, ln := range strings.Split(body, "\n") {
		for j, seg := range wordWrap(ln, avail) {
			if first && j == 0 {
				b.WriteString(prefix)
			} else {
				b.WriteString(strings.Repeat(" ", indent))
			}
			b.WriteString(seg)
			b.WriteString("\n")
		}
		first = false
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// wordWrap 按 display 宽度断行（宽字符按 2 列计）。
func wordWrap(s string, limit int) []string {
	if s == "" {
		return []string{""}
	}
	if lipgloss.Width(s) <= limit {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	curW := 0
	for _, r := range s {
		w := runeWidth(r)
		if curW+w > limit && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
			curW = 0
		}
		cur.WriteRune(r)
		curW += w
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func runeWidth(r rune) int {
	if r >= 0x1100 && (r <= 0x115F ||
		r == 0x2329 || r == 0x232A ||
		(r >= 0x2E80 && r <= 0xA4CF) ||
		(r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE30 && r <= 0xFE4F) ||
		(r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x20000 && r <= 0x3FFFD)) {
		return 2
	}
	return 1
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// truncateTail 尾部省略（保留开头）。
func truncateTail(s string, max int) string {
	out := ""
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > max-1 {
			break
		}
		out += string(r)
		w += rw
	}
	return out + "…"
}

// ---------------------------------------------------------------------------
// 布局与视图
// ---------------------------------------------------------------------------

const (
	headerRows  = 1
	inputRows   = 3 // 圆角边框上下 + 1 行输入
	statusRows  = 1
	gapRows     = 2 // 头部与消息区、消息区与输入框之间的空行
	minVpHeight = 4
)

func (m *tuiModel) layout() {
	h := m.height - headerRows - statusRows - inputRows - gapRows
	if h < minVpHeight {
		h = minVpHeight
	}
	m.vp.Width = m.width
	m.vp.Height = h
}

func (m *tuiModel) refreshViewport() {
	var b strings.Builder
	w := m.width - 2
	if w < 40 {
		w = 40
	}
	for i, cm := range m.messages {
		b.WriteString(m.renderMessage(cm, w))
		if i < len(m.messages)-1 {
			b.WriteString("\n\n") // 消息间空行分隔
		}
	}
	m.vp.SetContent(b.String())
	m.vp.GotoBottom()
}

func (m tuiModel) View() string {
	if !m.ready {
		return ""
	}

	// 顶栏
	dot := styleDotOn.Render("●")
	connText := "connected"
	if !m.state.Connected() {
		dot = styleDotOff.Render("●")
		connText = "disconnected"
	}
	left := styleTitle.Render("HologramAgent") + "  " + dot + " " + connText
	right := styleDim.Render(truncMid(m.addrLabel(), maxInt(10, m.width-lipgloss.Width(left)-8)))
	header := styleHeaderBox.Width(m.width).MaxWidth(m.width).Render(left + "  " + right)

	// 输入区
	boxStyle := styleInputBox
	if m.input.Focused() {
		boxStyle = styleInputFocus
	}
	inputBox := boxStyle.Width(m.width - 2).Render(m.input.View())

	statusBar := styleStatusBar.Width(m.width).MaxWidth(m.width).Render(m.statusLine())

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		"",
		m.vp.View(),
		"",
		inputBox,
		statusBar,
	)
}

func (m tuiModel) statusLine() string {
	var leftSeg string
	if m.busy {
		leftSeg = styleTitle.Render(spinnerFrames[m.spinnerIdx%len(spinnerFrames)] + " thinking...")
	} else {
		leftSeg = styleDim.Render("enter 发送 · PgUp/PgDn 翻页 · ctrl+c 退出")
	}
	right := styleDim.Render(connSummary(m.state, m.modeLabel()))
	gap := m.width - lipgloss.Width(leftSeg) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return leftSeg + strings.Repeat(" ", gap) + right
}

func connSummary(state *State, mode string) string {
	s := "● " + mode
	if state.Connected() {
		return styleDotOn.Render(s)
	}
	return styleDotOff.Render(s + " disconnected")
}

func truncMid(s string, limit int) string {
	if lipgloss.Width(s) <= limit {
		return s
	}
	half := (limit - 1) / 2
	r := []rune(s)
	if half < 1 {
		return "…"
	}
	return string(r[:half]) + "…" + string(r[len(r)-half:])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// 入口
// ---------------------------------------------------------------------------

// runTUI 启动 Bubble Tea 全屏界面。
func runTUI(state *State, cfg *Config, history *History) error {
	m := newTuiModel(state, cfg, history, make(chan string, 128), make(chan readerErrMsg, 8))
	m.readerAlive = true
	go m.readPump(m.readerGen) // 初始读循环

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

package remotedevice

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// devicectlDevice 把设备网关暴露为 IOManager 的一个 Device：
// Tools() 提供 devicedetect / device_ctl_status / device_ctl_cmdrun / device_ctl_cmdresult / screensee，
// Execute() 推送命令到设备，设备端自行鉴权（客户端存储授权）。
type devicectlDevice struct {
	reg *Registry

	// screensee 回调：设备截屏回传后由 agent 核心消费（视觉描述）。
	// 由插件 Start 注入；nil 时退化为仅返回 base64 数据。
	// ★ 2026-10-05 改为 (string, error)：视觉描述失败必须能上抛，
	//   不能渲染成普通文本（那会让 status=ok 与失败同形）。
	seeHandler func(dataURL string, provider string) (string, error)
}

func (d *devicectlDevice) Name() string                                 { return "devicectl" }
func (d *devicectlDevice) Type() agentIO.DeviceType                     { return agentIO.DeviceIO }
func (d *devicectlDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapStructured }
func (d *devicectlDevice) Description() string {
	return "远程设备控制网关：查看已接入/已授权的设备并发送控制指令（经用户授权的设备）"
}
func (d *devicectlDevice) ChannelDef() agentIO.ChannelDef { return agentIO.ChannelDef{} }
func (d *devicectlDevice) Start() error                   { return nil }
func (d *devicectlDevice) Stop() error                    { return nil }

func (d *devicectlDevice) Tools() []agentIO.ToolDef {
	return []agentIO.ToolDef{
		{
			Name: "devicedetect",
			Description: "扫描并列出已接入设备网关的设备（含在线/离线状态与授权状态）。" +
				"用途：查看当前有哪些设备连接了 HologramAgent、是否在线、是否已授权。" +
				"参数 kind 可选，只返回该种类的设备。返回值 device_id 用于后续 device_ctl_* 工具。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"kind": map[string]interface{}{"type": "string", "description": "按设备种类过滤（light/camera/computer/phone/...），可省略"},
				},
			},
		},
		{
			Name: "device_ctl_status",
			Description: "查询一台已在线设备的实时状态。" +
				"仅返回设备上报的状态信息（如电量/温度/运行状态）。" +
				"需要 device_id（来自 devicedetect）。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "device_ctl_cmdrun",
			Description: "在设备上执行 **shell 命令**（异步，超时后用 device_ctl_cmdresult 按 req_id 复查）。\n" +
				"★ 用途仅限 shell：查文件、跑诊断命令、看进程。command 直接写 shell 命令即可\n" +
				"（如 `df -h`、`tasklist`），无需前缀。\n" +
				"\n" +
				"★ 屏幕显示 / 摄像头 / 语音朗读**不要用本工具**，它们各有专用工具：\n" +
				"  · screensue(device_id, content, duration_seconds) — 在设备屏幕显示文字或 HTML\n" +
				"  · camerasue(device_id, duration_seconds) — 拍照或录像\n" +
				"  · speakeruse(device_id, text) — TTS 朗读\n" +
				"专用工具用结构化参数；本工具的 command 是**单个字符串**，" +
				"把 HTML 或长文本塞进来会因 JSON 转义/空格切分而变成非法参数（实测多次失败）。\n" +
				"\n" +
				"⚡ 高危：设备必须已授权，且该操作会改变设备行为。" +
				"返回 accepted=true 表示已下发并等待设备执行，之后可用 device_ctl_cmdresult 查询结果。" +
				"若设备未授权或离线，返回错误信息。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"command": map[string]interface{}{
						"type": "string",
						"description": "要执行的 shell 命令，如 `pwd`、`df -h`、`tasklist`。" +
							"（homeagent-* 旧前缀仍兼容，但屏幕/摄像头/语音请改用各自的专用工具。）",
					},
				},
				"required": []interface{}{"device_id", "command"},
			},
		},
		{
			Name: "device_ctl_cmdresult",
			Description: "查询之前 device_ctl_cmdrun 下发命令的执行结果（按 req_id 或 device_id 最近一次）。" +
				"若非阻塞或已超时，用此工具取回设备执行输出。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"req_id":    map[string]interface{}{"type": "string", "description": "命令请求 ID（device_ctl_cmdrun 返回）"},
					"device_id": map[string]interface{}{"type": "string", "description": "设备 ID（查询最近一次结果）"},
				},
			},
		},
		{
			Name: "screensue",
			Description: "在一台已授权设备的屏幕上显示内容（文字或 HTML 看板）。\n" +
				"★ 结构化参数：**不要**手拼 homeagent-screensue 命令字符串，用本工具。\n" +
				"内容走独立的 content 参数，时长走独立的 duration_seconds —— 二者不再挤在\n" +
				"同一个字符串里用空格分隔（那样模型必须同时处理 JSON 转义和空格切分，\n" +
				"长 HTML 会把工具参数撑爆）。\n" +
				"典型用途：给用户看会议提醒、公告、看板。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "要显示的内容：纯文字或 HTML 片段（含标签时按 HTML 渲染）。",
					},
					"content_path": map[string]interface{}{
						"type": "string",
						"description": "内容所在**服务端文件路径**（替代 content，适合几十 KB 的看板：" +
							"避免把整页 HTML 内联进工具参数）。与 content 二选一。",
					},
					"duration_seconds": map[string]interface{}{
						"type":        "integer",
						"description": "显示时长（秒）。默认 5；0 = 常驻直到用户手动关闭。",
					},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "camerasue",
			Description: "用一台已授权设备的摄像头拍照或录像。\n" +
				"★ 结构化参数：**不要**手拼 homeagent-camerasue 命令字符串，用本工具。\n" +
				"不传 duration_seconds = 抓拍单张 jpeg；传 N>0 = 录像 N 秒 mp4。\n" +
				"产物会落盘并返回 file 路径（与 screensee 一致，可直接交给 describe_image）。\n" +
				"依赖：设备端 ffmpeg + v4l2/dshow/avfoundation。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"duration_seconds": map[string]interface{}{
						"type":        "integer",
						"description": "录像秒数；省略或 0 = 抓拍单张照片（jpeg）。上限 300 秒。",
					},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "speakeruse",
			Description: "让一台已授权设备朗读一段文字（设备端 TTS 语音）。\n" +
				"★ 结构化参数：**不要**手拼 homeagent-speakeruse 命令字符串，用本工具。\n" +
				"长文本走 text_path（服务端文件），避免内联撑爆工具参数。\n" +
				"设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"text":      map[string]interface{}{"type": "string", "description": "要朗读的文字（与 text_path 二选一）"},
					"text_path": map[string]interface{}{
						"type":        "string",
						"description": "文字所在**服务端文件路径**（替代 text）。与 text 二选一。",
					},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "screensee",
			Description: "截取一台已授权设备的当前屏幕画面。\n" +
				"与 screensue（向用户屏幕显示内容）配对：screensue 是给用户看，screensee 是你看。\n" +
				"★★ 返回的是截图的**落盘路径**与元信息（file/mime/size/sha256/width/height），" +
				"**不含画面内容**。要看画面或读屏上文字，必须接着调用：" +
				"describe_image(path=<file>) 看画面，或 ocr_image(path=<file>) 读文字。" +
				"两步缺一不可——本工具不会自动描述任何东西。\n" +
				"需要 device_id（来自 devicedetect）。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"auto_describe": map[string]interface{}{
						"type": "boolean",
						"description": "可选（默认 false）：true 时跳过落盘、直接用视觉模型描述" +
							"（仅调试用：大截图约 13 万 token，且视觉源不可控）",
					},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "computeruse",
			Description: "控制一台已授权设备的鼠标/键盘（远程操控电脑屏幕）。" +
				"典型流程：先 screensee 看屏幕 → computeruse 操作 → 再 screensee 确认结果。" +
				"坐标为设备屏幕像素（原点左上角，与 screensee 截图一致）。" +
				"⚡ 高危：直接操作用户设备，务必确认操作意图明确。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"action": map[string]interface{}{
						"type":        "string",
						"description": "操作类型：click(单击) / doubleclick(双击) / rightclick(右键) / move(移动) / scroll(滚动) / keypress(按键) / type(输入文字)",
						"enum":        []interface{}{"click", "doubleclick", "rightclick", "move", "scroll", "keypress", "type"},
					},
					"x":      map[string]interface{}{"type": "integer", "description": "鼠标 X 坐标（像素）。click/doubleclick/rightclick/move 必填"},
					"y":      map[string]interface{}{"type": "integer", "description": "鼠标 Y 坐标（像素）。click/doubleclick/rightclick/move 必填"},
					"button": map[string]interface{}{"type": "string", "description": "鼠标按钮：left(默认)/right/middle（可选）"},
					"dy":     map[string]interface{}{"type": "integer", "description": "scroll 滚动量：正=向下，负=向上"},
					"key":    map[string]interface{}{"type": "string", "description": "keypress 按键名，如 Return / space / ctrl+c / alt+F4"},
					"text":   map[string]interface{}{"type": "string", "description": "type 要输入的文字"},
				},
				"required": []interface{}{"device_id", "action"},
			},
		},
		{
			Name: "clipboardsee",
			Description: "读取一台已授权设备的剪切板当前内容（用户最近复制/剪切的文字）。" +
				"与 clipboardsue 配对：clipboardsee 是读，clipboardsue 是写。" +
				"适用场景：用户说「看看我刚复制的东西」「把我复制的链接打开」。" +
				"⚡ 隐私敏感：剪切板可能含密码/隐私，仅在用户明确要求时使用。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "clipboardsue",
			Description: "把指定文字写入一台已授权设备的剪切板（用户之后可直接 Ctrl+V 粘贴）。" +
				"与 clipboardsee 配对：clipboardsee 是读，clipboardsue 是写。" +
				"适用场景：帮用户准备好要粘贴的长文本/链接/代码，避免 computeruse type 逐字输入慢且易错。" +
				"典型组合：clipboardsue 写入 → 提示用户 Ctrl+V，或 clipboardsue + computeruse keypress ctrl+v 自动粘贴。" +
				"设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"text":      map[string]interface{}{"type": "string", "description": "要写入剪切板的内容（与 content 二选一）"},
					"content": map[string]interface{}{
						"type": "string",
						"description": "要写入剪切板的内容所在**文件路径**（服务端读取，上限 2MB）。" +
							"长文本/代码块用这个：内联进 text 会撑爆工具参数。" +
							"★ 路径是服务端可见路径，不是设备上的路径。",
					},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "deviceinfo",
			Description: "探查一台设备接入网关时声明的详细信息与支持能力。" +
				"返回设备的 OS/架构/CPU/内存/能力 caps 等（设备接入时上报，非实时）。" +
				"需要 device_id（来自 devicedetect）。设备必须已授权。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
				},
				"required": []interface{}{"device_id"},
			},
		},
	}
}

func (d *devicectlDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	switch tool {
	case "output":
		// 出站：见 outputch.go。声明了 CapStructured 就必须真的能收输出。
		return d.output(args)
	case "devicedetect":
		return d.detect(args)
	case "device_ctl_status":
		return d.status(args)
	case "device_ctl_cmdrun":
		return d.cmdrun(args)
	case "device_ctl_cmdresult":
		return d.cmdresult(args)
	case "screensue":
		return d.screensue(args)
	case "camerasue":
		return d.camerasue(args)
	case "speakeruse":
		return d.speakeruse(args)
	case "screensee":
		return d.screensee(args)
	case "computeruse":
		return d.computeruse(args)
	case "clipboardsee":
		return d.clipboardsee(args)
	case "clipboardsue":
		return d.clipboardsue(args)
	case "deviceinfo":
		return d.info(args)
	default:
		return nil, fmt.Errorf("unknown device tool %s", tool)
	}
}

// publicDevices 把设备列表转为简洁 JSON（无内部字段）。
func publicDevices(in []DeviceMeta) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(in))
	for _, m := range in {
		entry := map[string]interface{}{
			"device_id":  m.DeviceID,
			"name":       m.Name,
			"kind":       m.Kind,
			"caps":       m.Caps,
			"authorized": m.Authorized,
			"online":     m.Online,
			"last_seen":  m.LastSeen,
		}
		if len(m.Info) > 0 {
			entry["info"] = m.Info
		}
		out = append(out, entry)
	}
	return out
}

// detect 实现 devicedetect。
func (d *devicectlDevice) detect(args map[string]interface{}) (interface{}, error) {
	kind, _ := args["kind"].(string)
	devs := d.reg.List()
	if kind != "" {
		var filtered []DeviceMeta
		for _, m := range devs {
			if strings.EqualFold(m.Kind, kind) {
				filtered = append(filtered, m)
			}
		}
		devs = filtered
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].DeviceID < devs[j].DeviceID })
	if len(devs) == 0 {
		return map[string]interface{}{"devices": []interface{}{}, "message": "暂无设备接入"}, nil
	}
	return map[string]interface{}{"devices": publicDevices(devs)}, nil
}

// status 实现 device_ctl_status（只读，查询设备最后一次上报状态）。
func (d *devicectlDevice) status(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("device_id required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	// 服务端不检查授权；设备端收到请求后自行决定是否执行。
	if !m.Online {
		return publicDevices([]DeviceMeta{m}), nil // 带 offline=true
	}
	return publicDevices([]DeviceMeta{m}), nil
}

// cmdrun 实现 device_ctl_cmdrun：检查授权+在线，push 命令，异步等待结果。
func (d *devicectlDevice) cmdrun(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	cmd, _ := args["command"].(string)
	if id == "" || cmd == "" {
		return nil, fmt.Errorf("device_id and command required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	// 服务端不检查授权；设备端收到请求后自行决定是否执行。
	if !m.Online {
		return nil, fmt.Errorf("device %s 不在线，无法执行命令", id)
	}
	// 命令类型：shell-cmd / homeagent-* 前缀区分；无前缀按 shell 处理（兼容旧格式）
	cmdType := "shell"
	switch {
	case strings.HasPrefix(cmd, "shell-cmd"):
		cmdType = "shell"
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "shell-cmd"))
	case strings.HasPrefix(cmd, "homeagent-cmd"):
		cmdType = "homeagent"
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "homeagent-cmd"))
	case strings.HasPrefix(cmd, "homeagent-"):
		cmdType = "homeagent"
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "homeagent-"))
	default:
		cmdType = "shell"
	}
	reqID := newReqID()
	if err := d.reg.PushCmd(id, reqID, cmd, cmdType); err != nil {
		return nil, fmt.Errorf("下发命令失败: %w", err)
	}
	// 阻塞等待设备结果（带超时）；结果同时由 registry 留档。
	res, err := d.reg.AwaitResult(reqID, 30*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return map[string]interface{}{"accepted": true, "req_id": reqID, "pending": true, "note": "命令已下发但设备未在超时内回执，可用 device_ctl_cmdresult 再查"}, nil
	}
	out := res
	out["req_id"] = reqID
	d.reg.SaveResult(reqID, out)
	return out, nil
}

// cmdresult 实现 device_ctl_cmdresult。
func (d *devicectlDevice) cmdresult(args map[string]interface{}) (interface{}, error) {
	reqID, _ := args["req_id"].(string)
	deviceID, _ := args["device_id"].(string)
	if reqID != "" {
		res, ok := d.reg.GetResult(reqID)
		if !ok {
			return nil, fmt.Errorf("no result for req %s（设备可能尚未回执）", reqID)
		}
		return res, nil
	}
	if deviceID != "" {
		// 返回该设备最近一次结果（简化：遍历 results 找 device_id 匹配的最近一条）
		// 说明：当前只按 req_id 查询；device_id 查询留给后续迭代。
		_, ok := d.reg.Get(deviceID)
		if !ok {
			return nil, fmt.Errorf("device %s 不存在", deviceID)
		}
		return map[string]interface{}{"device_id": deviceID, "note": "请用 device_ctl_cmdrun 返回的 req_id 查询命令结果"}, nil
	}
	return nil, fmt.Errorf("req_id 或 device_id 至少提供一个")
}

// info 实现 deviceinfo：返回设备详情 + 支持能力。
func (d *devicectlDevice) info(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("device_id required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	out := map[string]interface{}{
		"device_id":  m.DeviceID,
		"name":       m.Name,
		"kind":       m.Kind,
		"caps":       m.Caps,
		"authorized": m.Authorized,
		"online":     m.Online,
		"last_seen":  m.LastSeen,
	}
	if len(m.Info) > 0 {
		out["info"] = m.Info
	}
	return out, nil
}

// SetSeeHandler 注入 screensee 的视觉描述回调（agent 核心提供）。
func (d *devicectlDevice) SetSeeHandler(fn func(dataURL string, provider string) (string, error)) {
	d.seeHandler = fn
}

// screensee 实现 screensee：向设备下发 homeagent-screensee 截屏命令，
// screensee 截取设备屏幕。
//
// ★★ 2026-10-06：不再自动送视觉模型，改为**落盘 + 回路径**，
// 由 agent 显式调 describe_image(path=…)/ocr_image(path=…)。见函数体注释。
// args: device_id（必填）、provider（仅 auto_describe 时用）、auto_describe（调试用，默认 false）。
func (d *devicectlDevice) screensee(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	provider, _ := args["provider"].(string)
	if id == "" {
		return nil, fmt.Errorf("device_id required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	// 服务端不检查授权；设备端收到请求后自行决定是否执行。
	if !m.Online {
		return nil, fmt.Errorf("device %s 不在线", id)
	}
	if !d.reg.SupportsTool(id, "screensee") {
		return nil, fmt.Errorf("device %s 未声明 screensee 能力（caps=%v），无法截屏", id, m.Caps)
	}
	reqID := newReqID()
	if err := d.reg.PushCmd(id, reqID, "screensee", "homeagent"); err != nil {
		return nil, fmt.Errorf("下发截屏命令失败: %w", err)
	}
	res, err := d.reg.AwaitResult(reqID, 30*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return nil, fmt.Errorf("设备未在超时内回传屏幕画面: %w", err)
	}
	if res["status"] != "ok" {
		errMsg, _ := res["error"].(string)
		if errMsg == "" {
			errMsg = fmt.Sprintf("status=%v", res["status"])
		}
		return nil, fmt.Errorf("设备截屏失败: %s", errMsg)
	}
	output, _ := res["output"].(string)
	// ★ 2026-10-05：把回传内容**量化后记进服务端日志**。
	//
	// 起因：2026-10-05 生产日志里有两次 screensee 回了
	//	「我目前没有收到或无法查看这张屏幕截图」
	// 而服务端**没有任何记录**能回答「设备到底回传了什么」——
	// GUI 侧那行 console.log 只打在它自己的控制台，服务端留不下痕迹。
	// 于是排查只能靠猜（截屏为空？data URL 被截断？缩略图未就绪？）。
	//
	// 只记**元信息 + 极短内容指纹**，不记图片本身（日志会膨胀且含用户屏幕内容）：
	//   · 字节数、解出来的像素尺寸（jpeg/png 头部可解）、是否非空
	//   · 前若干字节的 base64 片段 + SHA256 前 16 hex
	//     ⇒ 「回传的是不是同一张图」「是不是被截断了」一眼可判。
	logScreenseePayload(id, output)
	// ★★ 2026-10-06：截图先**落盘**，再把路径交给 agent。
	//
	// 原行为：把 base64 直接送去视觉模型自动描述。三个问题：
	//  ① 96KB 截图 ≈ 13 万 token 的内联 base64；
	//  ② 自动挑源不可控 —— 生产实证：AUTO 把大图路由到不支持视觉的源，
	//     模型回「没有收到截图」，而我们无法区分「图没到」与「模型看不了」；
	//  ③ agent 不知道图存不存在，也就无法决定用 describe 还是 ocr。
	//
	// 现在：落盘 + 回元信息，agent 显式调 describe_image(path=…)/ocr_image(path=…)
	//      —— 需要哪个能力由它自己判断，且 provider 参数可控。
	//
	// 落盘失败当**工具失败**上抛（不静默退回内联）：图已经在手上却拿不到路径，
	// agent 会既没有描述也没有可查证产物，只剩一句「失败」。
	//
	// auto_describe=true 保留旧路径（调试用）。
	if auto, _ := args["auto_describe"].(bool); auto && d.seeHandler != nil {
		if !strings.HasPrefix(output, "data:") {
			output = "data:image/jpeg;base64," + output
		}
		desc, err := d.seeHandler(output, provider)
		if err != nil {
			// ★ 视觉描述失败 ⇒ 工具失败（不是「返回一段说明」）。
			//   生产实证：403 被塞进 description 且 status=ok，
			//   agent 以为拿到了结果，于是换 provider 又试一次（再失败一次）。
			d.reg.SaveResult(reqID, map[string]interface{}{
				"error":     err.Error(),
				"device":    id,
				"has_image": true, // 截图确实拿到了：区分「设备没回传」与「模型看不了」
			})
			return nil, fmt.Errorf("设备已回传截图，但视觉描述失败: %w", err)
		}
		return map[string]interface{}{"description": desc}, nil
	}

	meta, err := d.reg.SaveInlineMedia("screensee_"+reqID, output, "image/jpeg")
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{
			"error":     err.Error(),
			"device":    id,
			"has_image": true, // 图确实拿到了，只是没落盘
		})
		return nil, fmt.Errorf("截图已回传但落盘失败（无法交付给多模态工具）: %w", err)
	}
	d.reg.SaveResult(reqID, meta)

	out := map[string]interface{}{
		"file":      meta["file"],
		"mime":      meta["mime"],
		"size":      meta["size"],
		"sha256":    meta["sha256"],
		"device":    id,
		"next_step": "如需查看画面，请调用 describe_image 并传入 path 参数；需读图中的文字请用 ocr_image。",
	}
	if w, ok := meta["width"]; ok {
		out["width"] = w
	}
	if h, ok := meta["height"]; ok {
		out["height"] = h
	}
	return out, nil
}

// computeruse 实现 computeruse：向设备下发鼠标/键盘控制命令。
// 协议（GUI b4e5b39）：homeagent-computeruse {"x":px,"y":px,"action":"act","button":"btn","text":"txt"}
// 服务端负责把结构化参数序列化为 JSON，避免 LLM 手拼字符串出错。
func (d *devicectlDevice) computeruse(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	action, _ := args["action"].(string)
	if id == "" || action == "" {
		return nil, fmt.Errorf("device_id and action required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	// 服务端不检查授权；设备端收到请求后自行决定是否执行。
	if !m.Online {
		return nil, fmt.Errorf("device %s 不在线", id)
	}
	if !d.reg.SupportsTool(id, "computeruse") {
		return nil, fmt.Errorf("device %s 未声明 computeruse 能力（caps=%v），无法操控鼠标键盘", id, m.Caps)
	}

	// 构造 GUI 端约定的 JSON 参数（坐标相对 screensueDisplay 所选屏）
	params := map[string]interface{}{"action": action}
	switch action {
	case "click", "doubleclick", "rightclick", "move":
		x, xok := args["x"].(float64)
		y, yok := args["y"].(float64)
		if !xok || !yok {
			return nil, fmt.Errorf("action=%s 需要 x/y 坐标", action)
		}
		params["x"] = int(x)
		params["y"] = int(y)
		if btn, ok := args["button"].(string); ok && btn != "" {
			params["button"] = btn
		}
	case "scroll":
		dy, ok := args["dy"].(float64)
		if !ok {
			return nil, fmt.Errorf("action=scroll 需要 dy 滚动量（正=向下，负=向上）")
		}
		params["dy"] = int(dy)
	case "keypress":
		key, _ := args["key"].(string)
		if key == "" {
			return nil, fmt.Errorf("action=keypress 需要 key 按键名（如 Return / ctrl+c）")
		}
		params["key"] = key
	case "type":
		text, _ := args["text"].(string)
		if text == "" {
			return nil, fmt.Errorf("action=type 需要 text 要输入的文字")
		}
		params["text"] = text
	default:
		return nil, fmt.Errorf("不支持的操作类型 %s（可选 click/doubleclick/rightclick/move/scroll/keypress/type）", action)
	}

	cmdBytes, _ := json.Marshal(params)
	reqID := newReqID()
	if err := d.reg.PushCmd(id, reqID, "computeruse "+string(cmdBytes), "homeagent"); err != nil {
		return nil, fmt.Errorf("下发操控命令失败: %w", err)
	}
	res, err := d.reg.AwaitResult(reqID, 30*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return nil, fmt.Errorf("设备未在超时内回执: %w", err)
	}
	out := res
	out["req_id"] = reqID
	d.reg.SaveResult(reqID, out)
	return out, nil
}

// clipboardCheck 检查设备可操作性（存在/已授权/在线/能力声明），返回错误或 nil。
// tool 为空时跳过能力校验（如 device_ctl_cmdrun 由自身逻辑处理）。
func (d *devicectlDevice) clipboardCheck(id, verb, tool string) error {
	if id == "" {
		return fmt.Errorf("device_id required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return fmt.Errorf("device %s 不存在", id)
	}
	if !m.Online {
		return fmt.Errorf("device %s 不在线", id)
	}
	if tool != "" && !d.reg.SupportsTool(id, tool) {
		return fmt.Errorf("device %s 未声明 %s 能力（caps=%v），无法执行此操作", id, tool, m.Caps)
	}
	return nil
}

// clipboardsee 实现 clipboardsee：读取设备剪切板当前内容。
// 协议（GUI 配套）：homeagent-clipboardsee → 回执 output 字段为剪切板文字。
func (d *devicectlDevice) clipboardsee(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	if err := d.clipboardCheck(id, "读取", "clipboardsee"); err != nil {
		return nil, err
	}
	reqID := newReqID()
	if err := d.reg.PushCmd(id, reqID, "clipboardsee", "homeagent"); err != nil {
		return nil, fmt.Errorf("下发读取命令失败: %w", err)
	}
	res, err := d.reg.AwaitResult(reqID, 15*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return nil, fmt.Errorf("设备未在超时内回执: %w", err)
	}
	if res["status"] != "ok" {
		errMsg, _ := res["error"].(string)
		if errMsg == "" {
			errMsg = fmt.Sprintf("status=%v", res["status"])
		}
		return nil, fmt.Errorf("读取剪切板失败: %s", errMsg)
	}
	content, _ := res["output"].(string)
	out := map[string]interface{}{
		"req_id":  reqID,
		"content": content,
		"empty":   content == "",
	}
	d.reg.SaveResult(reqID, out)
	return out, nil
}

// clipboardsue 实现 clipboardsue：把文字写入设备剪切板。
// 协议（GUI 配套）：homeagent-clipboardsue <文字>，回执 ok 表示已写入。
func (d *devicectlDevice) clipboardsue(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	text, _ := args["text"].(string)
	// ★ content 参数（判据 clipboardsue_content_test.go 钉着它）——
	//   从**服务端可见的路径**读入剪切板，长文本/代码块不塞得进命令行。
	//   与 GUI 侧 screensue/speakeruse 的 @<路径> 是同一类问题的同一类解法。
	//
	// ★ 这段实现曾因「只提交判据没提交代码」被仓库清理冲掉，
	//   导致判据编译不过（undefined: clipboardsueMaxBytes）。
	//   判据与实现在同一个提交里，才不会再次走丢。
	content, _ := args["content"].(string)
	if strings.TrimSpace(content) == "" && strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("text 或 content 至少给一个（要写入剪切板的内容）")
	}
	if err := d.clipboardCheck(id, "写入", "clipboardsue"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(content) != "" {
		b, rerr := os.ReadFile(strings.TrimSpace(content))
		if rerr != nil {
			return nil, fmt.Errorf("读取 content 文件失败 %s: %w", strings.TrimSpace(content), rerr)
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("content 文件为空: %s", content)
		}
		if len(b) > clipboardsueMaxBytes {
			return nil, fmt.Errorf("content 文件过大 %dKB > 上限 %dKB",
				len(b)/1024, clipboardsueMaxBytes/1024)
		}
		text = string(b)
	}
	reqID := newReqID()
	// 命令格式：clipboardsue <文字>（GUI 端取首个空格后的全部内容作为写入文本）
	if err := d.reg.PushCmd(id, reqID, "clipboardsue "+text, "homeagent"); err != nil {
		return nil, fmt.Errorf("下发写入命令失败: %w", err)
	}
	res, err := d.reg.AwaitResult(reqID, 15*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return nil, fmt.Errorf("设备未在超时内回执: %w", err)
	}
	if res["status"] != "ok" {
		errMsg, _ := res["error"].(string)
		if errMsg == "" {
			errMsg = fmt.Sprintf("status=%v", res["status"])
		}
		return nil, fmt.Errorf("写入剪切板失败: %s", errMsg)
	}
	out := map[string]interface{}{
		"req_id":  reqID,
		"written": len(text),
		"preview": truncateForPreview(text, 60),
	}
	d.reg.SaveResult(reqID, out)
	return out, nil
}

// truncateForPreview 截断长文本用于回执预览。
func truncateForPreview(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// clipboardsueMaxBytes 是从文件读入剪切板的大小上限（与 GUI 侧 2MB 一致）。
const clipboardsueMaxBytes = 2 * 1024 * 1024

// logScreenseePayload 把设备回传的截图**元信息**记进服务端日志。
//
// ★ 为什么必须记在服务端（2026-10-05 教训）：
//
//	GUI 侧那行 `[device-bridge] screensee screen=0 bytes=N` 只打在它自己的
//	控制台。生产排障时手上只有 homed 的日志 ⇒ 无法回答「设备到底回传了什么」。
//	实测就有两次 screensee 返回「我收不到截图」，而服务端无任何痕迹可查，
//	最后只能靠推测（截屏为空 / data URL 截断 / 缩略图未就绪）。
//
// ★ 只记元信息与**极短指纹**，不记图片本体：
//
//	· 日志会按字节膨胀；
//	· 更重要的是截图是**用户屏幕内容**，属敏感信息，不该进日志。
//
// 记什么：字节数、magic、jpeg SOF 解出的像素尺寸、base64 头 24 字符、SHA256 前 16 hex。
// 能回答：「有没有回传」「是不是合法图片」「多长」「两次是不是同一张」「会不会被截断」。
func logScreenseePayload(deviceID, dataURL string) {
	if dataURL == "" {
		log.Printf("[remotedevice] screensee returned device=%s len=0 * empty content: device captured no image "+
			"(lock screen / virtual desktop / thumbnail not ready?)", deviceID)
		return
	}
	raw := dataURL
	if i := strings.Index(raw, ","); strings.HasPrefix(raw, "data:") && i >= 0 {
		raw = raw[i+1:]
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// ★ 解不出来本身就是关键信号：说明回传的不是合法 base64（可能被截断）。
		log.Printf("[remotedevice] screensee returned device=%s len=%d * base64 decode failed: %v",
			deviceID, len(raw), err)
		return
	}
	sum := sha256.Sum256(b)
	head := raw
	if len(head) > 24 {
		head = head[:24]
	}
	w, h := jpegSize(b)
	log.Printf("[remotedevice] screensee returned device=%s bytes=%d magic=%s %dx%d b64head=%q sha=%x",
		deviceID, len(b), magicName(b), w, h, head, sum[:8])
}

// magicName 认出图片类型，用于一眼判断「回传的到底是什么」。
func magicName(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpeg"
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	case len(b) == 0:
		return "empty"
	default:
		return "unknown"
	}
}

// jpegSize 从 jpeg 的 SOF 标记解出像素尺寸；不是 jpeg 或解不出返回 0,0。
//
// 不引入 image/jpeg 全解码：这里只要尺寸，而截图可能很大，
// 全解码一次是纯浪费（GUI 已经编码过一次了）。
func jpegSize(b []byte) (int, int) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 0, 0
	}
	i := 2
	for i+3 < len(b) {
		if b[i] != 0xFF {
			i++
			continue
		}
		marker := b[i+1]
		// SOF0..SOF3 / SOF5..SOF7 / SOF9..SOF11 / SOF13..SOF15 携带尺寸
		if marker >= 0xC0 && marker <= 0xCF &&
			marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			if i+9 < len(b) {
				h := int(b[i+5])<<8 | int(b[i+6])
				w := int(b[i+7])<<8 | int(b[i+8])
				return w, h
			}
			return 0, 0
		}
		// 其余段按 2 字节长度跳过
		if i+3 >= len(b) {
			break
		}
		segLen := int(b[i+2])<<8 | int(b[i+3])
		if segLen < 2 {
			break
		}
		i += 2 + segLen
	}
	return 0, 0
}

// ---- 三个结构化能力工具（2026-10-06）----
//
// # 为什么要把它们从 device_ctl_cmdrun 里拆出来
//
// 生产日志实证（10-06 12:46~13:04，agent 连续 9 次试参数）：
//
//	13:00:38  stream tool_call device_ctl_cmdrun argument fragments invalid JSON:
//	          "{\"device_id\": \"gui-JianF\", \"command\": \"homeagent-screensue 0
//	           <!DOCTYPE html>…<meta charset=\\\"utf-8\\\">…
//	13:00:38  args unparseable (982 bytes) — surfacing to model instead of calling
//
// 一屏 HTML 内联进 command 字符串后：① 引号要穿过两层 JSON 转义；
// ② 时长与内容共用一个字符串靠空格切分（内容里的空格有歧义）；
// ③ 长度无上限。三者叠加，工具调用直接变成非法 JSON —— **连执行都没执行**。
//
// 而 agent 的应对是反复试参数（"shown on display 0 for 15s/25s/30s/40s/60s/180s"）
// —— 它在猜格式，不是在做事。
//
// ⇒ 正解是给这三个能力**结构化参数**（本仓已有先例：clipboardsue 的 content）。
//   command 字符串通道仍保留作逃生口，但不再是唯一通路。

// deviceContentArg 解析「内联内容 / 服务端文件路径」二选一。
//
// 抽出来的理由：screensue / speakeruse 用法完全同形，两处各写一遍
// 必然漂移（本项目已多次记录这类同形错误）。
//
// 上限 2MB 与 clipboardsue 对齐：这是「服务端读一个文件塞进命令」的量级，
// 再大就不是「显示/朗读」而是「传输文件」了，该走别的通道。
const deviceContentMaxBytes = 2 * 1024 * 1024

func deviceContentArg(inline, pathArg, what string) (string, error) {
	hasInline := strings.TrimSpace(inline) != ""
	hasPath := strings.TrimSpace(pathArg) != ""
	switch {
	case hasInline && hasPath:
		return "", fmt.Errorf("%s 与 %s 只能给一个", what, what+"_path")
	case hasPath:
		b, err := os.ReadFile(strings.TrimSpace(pathArg))
		if err != nil {
			return "", fmt.Errorf("读取 %s 失败 %s: %w", what+"_path", pathArg, err)
		}
		if len(b) == 0 {
			return "", fmt.Errorf("%s 文件为空: %s", what+"_path", pathArg)
		}
		if len(b) > deviceContentMaxBytes {
			return "", fmt.Errorf("%s 文件过大 %dKB > 上限 %dKB",
				what+"_path", len(b)/1024, deviceContentMaxBytes/1024)
		}
		return string(b), nil
	case hasInline:
		if len(inline) > deviceContentMaxBytes {
			return "", fmt.Errorf("%s 过大 %dKB > 上限 %dKB；请改用 %s_path 传文件路径",
				what, len(inline)/1024, deviceContentMaxBytes/1024, what)
		}
		return inline, nil
	}
	return "", fmt.Errorf("必须给 %s 或 %s_path", what, what)
}

// pushHomeagentCapability 下发一条 homeagent-* 能力命令并等回执。
//
// ★ 为什么要抽：screensue/camerasue/speakeruse 三个工具都是
// 「拼 <capability> <args> → PushCmd(homeagent) → AwaitResult → 判 status」。
// 三处各写一遍的后果不只是重复 —— 超时值、错误文案、pending 语义
// 会在三次修改中各自漂移，而这类「同一逻辑多份副本」正是本项目
// 反复吃亏的形态。
//
// 返回 (回执, reqID, error)。回执 status=ok 时 error 为 nil。
func (d *devicectlDevice) pushHomeagentCapability(
	id, capability string, timeout time.Duration,
) (map[string]interface{}, string, error) {
	if err := d.deviceOnlineCheck(id, capability); err != nil {
		return nil, "", err
	}
	reqID := newReqID()
	if err := d.reg.PushCmd(id, reqID, capability, "homeagent"); err != nil {
		return nil, "", fmt.Errorf("下发 %s 命令失败: %w", capability, err)
	}
	res, err := d.reg.AwaitResult(reqID, timeout)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{
			"accepted": true, "error": err.Error(), "pending": true,
		})
		// ★ 超时不是「设备失败」而是「还没回」——说清区别，否则 agent
		//   会以为设备拒绝执行而换参数重试（实测它就是这么做的）。
		return nil, reqID, fmt.Errorf(
			"设备未在 %v 内回执（命令已下发，设备可能正忙或刚在重连；"+
				"可用 device_ctl_cmdresult 按 req_id=%s 复查）: %w", timeout, reqID, err)
	}
	if res["status"] != "ok" {
		msg, _ := res["error"].(string)
		if msg == "" {
			msg = fmt.Sprintf("status=%v", res["status"])
		}
		return res, reqID, fmt.Errorf("设备执行 %s 失败: %s", capability, msg)
	}
	return res, reqID, nil
}

// deviceOnlineCheck 三个结构化工具的公共前置校验。
//
// 与 cmdrun 的口径一致（存在 → 在线 → 声明能力），但把错误文案写得更可操作：
// 说清「是哪台设备、缺哪个能力、caps 里有什么」，而不是一句 "not supported"。
func (d *devicectlDevice) deviceOnlineCheck(id, tool string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("device_id required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return fmt.Errorf("device %s 不存在（用 devicedetect 看在线设备）", id)
	}
	if !m.Online {
		// ★ 与 screensee 的实测教训一致：设备频繁上下线（今天实测平均在线
		//   ~23 分钟），命令踩中重连窗口时若只报「不在线」，agent 会以为
		//   设备永久不可用。说清「可稍后重试」。
		return fmt.Errorf("device %s 当前不在线（设备可能正在重连，稍后重试；用 devicedetect 确认）", id)
	}
	if !d.reg.SupportsTool(id, tool) {
		return fmt.Errorf("device %s 未声明 %s 能力（caps=%v）", id, tool, m.Caps)
	}
	return nil
}

// screensue 在设备屏幕上显示内容。
func (d *devicectlDevice) screensue(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	content, _ := args["content"].(string)
	contentPath, _ := args["content_path"].(string)

	text, err := deviceContentArg(content, contentPath, "content")
	if err != nil {
		return nil, err
	}
	// 时长：省略 → 不传（设备端默认 5 秒）；显式 0 → 常驻。
	prefix := ""
	if v, ok := args["duration_seconds"]; ok && v != nil {
		secs := toIntArg(v)
		if secs < 0 {
			return nil, fmt.Errorf("duration_seconds 不能为负（0 = 常驻）")
		}
		prefix = strconv.Itoa(secs) + " "
	}
	// 协议：<capability> <可选的秒数> <内容>（GUI 侧取首个纯数字 token 作时长）
	cap := "screensue " + prefix + text
	res, reqID, err := d.pushHomeagentCapability(id, cap, 30*time.Second)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"device_id": id,
		"req_id":    reqID,
		"shown":     len(text),
		"result":    res["output"],
	}, nil
}

// camerasue 用设备摄像头拍照或录像。
func (d *devicectlDevice) camerasue(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	secs := 0
	if v, ok := args["duration_seconds"]; ok && v != nil {
		secs = toIntArg(v)
	}
	if secs < 0 {
		return nil, fmt.Errorf("duration_seconds 不能为负（0 = 抓拍单张）")
	}
	if secs > 300 {
		return nil, fmt.Errorf("duration_seconds 上限 300 秒（要更长的录像请分段）")
	}
	cap := "camerasue"
	if secs > 0 {
		cap = "camerasue " + strconv.Itoa(secs)
	}
	// 抓拍走小体积（一张 jpeg），录像可能几十 MB —— 超时按量级给。
	timeout := 30 * time.Second
	if secs > 0 {
		timeout = time.Duration(secs)*time.Second + 60*time.Second
	}
	res, reqID, err := d.pushHomeagentCapability(id, cap, timeout)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{
		"device_id": id,
		"req_id":    reqID,
		"mode":      map[bool]string{true: "video", false: "photo"}[secs > 0],
	}
	if secs > 0 {
		out["duration_seconds"] = secs
	}
	// ★ 大体积数据（data_base64 / file）原样透传：录像不能只回一句「成功」。
	for _, k := range []string{"file", "data_base64", "mime", "size", "kind"} {
		if v, ok := res[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

// speakeruse 让设备朗读文字。
func (d *devicectlDevice) speakeruse(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	text, _ := args["text"].(string)
	textPath, _ := args["text_path"].(string)

	body, err := deviceContentArg(text, textPath, "text")
	if err != nil {
		return nil, err
	}
	res, reqID, err := d.pushHomeagentCapability(id, "speakeruse "+body, 60*time.Second)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"device_id": id,
		"req_id":    reqID,
		"spoken":    len(body),
		"result":    res["output"],
	}, nil
}

// toIntArg 把 JSON 解出来的数字/字符串统一成 int。
//
// 模型的 duration_seconds 有时是 30，有时是 "30"（字符串）——
// 后者会让类型断言静默失败退回默认值，正是「参数丢了但不报错」这类毛病。
func toIntArg(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

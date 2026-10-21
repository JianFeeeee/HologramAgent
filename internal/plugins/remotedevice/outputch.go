package remotedevice

// 设备输出通道：把"agent 主动发给设备"做成**每设备一个输出通道** `device-<id>`。
//
// 为什么是输出通道而不是再加一批工具：
//   - **寻址**：`output_send__device-<id>` 直接指名道姓；模型看 `output_list_channels`
//     就知道当前有哪些设备在线，不必先 `devicedetect` 再往参数里塞 device_id。
//   - **能力**：caps 由设备声明的 caps 映射，**内核**在发送前就按 caps 拦
//     （把图片发给只支持文本的音箱会被拒，而不是等设备侧报错）。
//   - **授权**：`AllowedOutputs` 是内核级的授权闸（`executeOutputSendTool` 里先查
//     `IsOutputAllowed`）。父 agent 因此可以"只授权某一台设备"给驻留子 ——
//     这在工具模型下做不到（拿到 `device_ctl_cmdrun` 就能对任意设备下指令）。
//
// 而 `screensee`/`computeruse`/`device_ctl_*` 这类**请求-响应**仍留作工具：
// 它们的返回值（图像/命令输出/状态）必须进模型上下文，做成通道会丢掉这个语义。

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// deviceOutputCaps 把设备声明的 caps 映射成输出通道能力位。
//
// 映射依据（与 registry.go 的 capabilityTools/compatFullCaps 同一套词表）：
//   - 文本：任何设备都收（消息/指令都是文本）→ 总是 CapText
//   - 结构化：能跑命令（cmd/cmdrun/cmdresult 视为历史全能力）→ 能渲染结构化结果
//   - 音频：speaker/speakeruse，或 kind=speaker
//   - 图片/文件：有屏（screen/screensue/screensee/gui）、剪切板、摄像头，或 kind 是带屏设备
//   - **未声明任何已知能力** → 视为全能力（与 deviceSupportsTool 的旧设备兼容规则一致）
func deviceOutputCaps(caps []string, kind string) agentIO.OutputCapability {
	full := agentIO.CapText | agentIO.CapFile | agentIO.CapImage | agentIO.CapAudio | agentIO.CapStructured

	out := agentIO.CapText
	known := false
	for _, c := range caps {
		switch c {
		case "cmd", "cmdrun", "cmdresult":
			// 历史"全能力"标记：这类设备能跑命令、能收结构化结果。
			return full
		case "screen", "screensue", "screensee", "gui", "display":
			known = true
			out |= agentIO.CapImage | agentIO.CapFile
		case "clipboard", "clipboardsee", "clipboardsue":
			known = true
			out |= agentIO.CapFile
		case "camera", "camerasue":
			known = true
			out |= agentIO.CapImage | agentIO.CapFile
		case "speaker", "speakeruse", "audio":
			known = true
			out |= agentIO.CapAudio
		case "computeruse":
			known = true
			out |= agentIO.CapStructured
		}
	}
	// kind 兜底：带屏设备即便没声明 caps，也能收图和文件。
	switch kind {
	case "computer", "phone", "tablet", "tv":
		known = true
		out |= agentIO.CapImage | agentIO.CapFile | agentIO.CapStructured
	case "speaker":
		known = true
		out |= agentIO.CapAudio
	}
	if !known {
		return full // 旧设备兼容：未声明已知能力 ⇒ 全能力
	}
	return out
}

// deviceChannelName 由**设备自报的 id** 派生一个合规且唯一的通道名：`device-<派生值>`。
//
// 入站与出站**同名**：两者指的是同一台设备，分成两个名字只会让模型与授权表更难对。
//
// 为什么不能直接用 id：通道名会被内核拼进 LLM 的**函数名**（`output_send__<通道名>`），
// 上游规范是 `^[a-zA-Z0-9_-]{1,64}$`；而设备 id 是**外部输入**（设备自己声明），
// 可能含空格/非 ASCII/超长。违规的后果不是"这个工具不能用"，而是**整条请求被 400 拒绝** ——
// 实测把生产打挂：`Invalid 'tools[299].function.name'`，网关 auto tier 全链条失败，
// 内核只能报"所有 provider 都失败"，表现成"整个 agent 不说话了"。
//
// 派生规则（确定性，同一 id 永远同名）：
//  1. 保留 [A-Za-z0-9_-]，其它字符折成 '-'；折叠后为空则用 "dev"
//  2. 截断到 maxDeviceChannelSuffix 字符（给 "device-" 与短哈希留余量）
//  3. 若发生截断，或该名字已被**另一个** id 占用，则追加 id 的 6 位短哈希
//
// 设备 id 本身仍用于路由与日志（真名不丢），通道名只是它派生的标识符。
func (p *Plugin) deviceChannelName(id string) string {
	p.devChansMu.Lock()
	defer p.devChansMu.Unlock()
	if p.devChans == nil {
		p.devChans = make(map[string]string)
	}
	if name, ok := p.devChans[id]; ok {
		return name
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	base := b.String()
	if base == "" {
		base = "dev"
	}
	truncated := false
	if len(base) > maxDeviceChannelSuffix {
		base = base[:maxDeviceChannelSuffix]
		truncated = true
	}
	name := "device-" + base
	// 撞名检查：不同 id 折出同一个名字时必须可区分
	for otherID, otherName := range p.devChans {
		if otherName == name && otherID != id {
			truncated = true
			break
		}
	}
	if truncated {
		sum := sha1.Sum([]byte(id))
		name += "-" + hex.EncodeToString(sum[:3])
	}
	p.devChans[id] = name
	return name
}

const (
	// maxDeviceChannelSuffix 是通道名主体的长度上限。
	// 预算：上游函数名上限 64 = "output_send__"(13) + "device-"(7) + 主体 + "-"+短哈希(7)
	// ⇒ 主体最多 37；取 32 留余量（改名/前缀变动不会立刻越界）。
	maxDeviceChannelSuffix = 32
)

// wireDeviceChannels 把"设备上下线"接到通道的登记/注销上。
//
// 一台设备 = 一对**同名**通道 `device-<id>`：入站（设备上报 → agent）与出站
// （agent → 设备）。用**同步回调**而不是 ChangeChan（后者是 select+default，
// 缓冲满会丢事件；丢一次就留下死通道或漏注册）。
//
// 抽成方法而不是内联在 Start 里：测试要能走**同一条**接线，
// 否则测试自己塞 handler，Start 忘了接线也照样绿。
func (p *Plugin) wireDeviceChannels() {
	p.registry.SetPresenceHandler(
		func(meta DeviceMeta) {
			_ = p.sdk.RegisterInputChannel(p.deviceChannelName(meta.DeviceID), sdk.ChannelDef{})
			p.ensureDeviceOutputChannel(meta.DeviceID)
		},
		func(id string) { p.dropDeviceOutputChannel(id) },
	)
}

// ensureDeviceOutputChannel 给在线设备注册输出通道 device-<id>（幂等）。
func (p *Plugin) ensureDeviceOutputChannel(id string) {
	if p.sdk == nil || id == "" {
		return
	}
	meta, ok := p.registry.Get(id)
	if !ok || !meta.Online {
		return
	}
	ch := p.deviceChannelName(id)
	caps := deviceOutputCaps(meta.Caps, meta.Kind)
	desc := fmt.Sprintf("远程设备 %s（%s）：agent 主动向该设备发送内容；能力位 %s",
		id, fallback(meta.Name, meta.Kind), agentIO.OutputCapability(caps).String())
	// 重复注册是安全的：芯片侧 Register 会合并（owner/capacity 取旧值）。
	if err := p.sdk.RegisterOutputChannel(ch, int(caps), desc, sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		return pushToDevice(p.registry, id, args)
	}); err != nil {
		p.logf("register output channel %s: %v", ch, err)
		return
	}
	p.logf("device %s online → 输出通道 %s（caps=%s）", id, ch, agentIO.OutputCapability(caps).String())
}

// dropDeviceOutputChannel 设备下线时注销它的输出通道。
//
// 不注销的后果：`output_list_channels` 一直列着它，模型会往死通道发消息，
// 拿到的却只是"发送已提交"之类的假回执。
func (p *Plugin) dropDeviceOutputChannel(id string) {
	if p.sdk == nil || id == "" {
		return
	}
	ch := p.deviceChannelName(id)
	if err := p.sdk.UnregisterOutputChannel(ch); err != nil {
		p.logf("unregister output channel %s: %v", ch, err)
		return
	}
	p.logf("device %s offline → 注销输出通道 %s", id, ch)
}

// pushToDevice 把一次 output_send 的 {payload,type,meta} 转成下行帧发给设备。
//
// 线上格式（新增 op=push，与既有 op=cmd/cmd_speech_* 并列）：
//
//	{"op":"push","req_id":"...","type":"text|image|file|audio|structured","payload":"...","meta":"..."}
//
// 大负载（data URL 形式的图片/音频/文件）走既有分块通道 PushData，
// 避免把 base64 塞进一个超大文本帧。
func pushToDevice(reg *Registry, id string, args map[string]interface{}) (interface{}, error) {
	payload, _ := args["payload"].(string)
	typ, _ := args["type"].(string)
	metaStr, _ := args["meta"].(string)
	if payload == "" {
		return nil, fmt.Errorf("payload 不能为空")
	}
	if typ == "" {
		typ = "text"
	}
	reqID := fmt.Sprintf("push_%d", time.Now().UnixNano())

	if data, mime, ok := decodeDataURL(payload); ok && typ != "text" && typ != "structured" {
		if err := reg.PushData(id, reqID, typ, mime, data); err != nil {
			return nil, err
		}
		return map[string]interface{}{"status": "sent", "req_id": reqID}, nil
	}

	frame := map[string]interface{}{
		"op":      "push",
		"req_id":  reqID,
		"type":    typ,
		"payload": payload,
	}
	if metaStr != "" {
		frame["meta"] = metaStr
	}
	if err := reg.PushJSON(id, frame); err != nil {
		return nil, err
	}
	return map[string]interface{}{"status": "sent", "req_id": reqID}, nil
}

// decodeDataURL 解析 data:<mime>;base64,<data> 形式的内联负载。
func decodeDataURL(s string) (data []byte, mime string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return nil, "", false
	}
	rest := strings.TrimPrefix(s, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return nil, "", false
	}
	head, body := rest[:comma], rest[comma+1:]
	if !strings.HasSuffix(head, ";base64") {
		return nil, "", false
	}
	mime = strings.TrimSuffix(head, ";base64")
	b, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, "", false
	}
	return b, mime, true
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func (p *Plugin) logf(format string, a ...interface{}) {
	log.Printf("[remotedevice] "+format, a...)
}

// output 是**聚合通道** `devicectl` 的出站实现（每设备通道之外的另一条路）。
//
// 历史状态：devicectlDevice 一直声明 OutputCapabilities=CapStructured，
// 但 Execute 里根本没有 "output" 分支 ⇒ `output_send__devicectl` 必然报
// "unknown device tool output"。这里把它补实：按 meta/device_id 指到具体设备。
//
// 寻址方式（两者都收，模型的写法越少歧义越好）：
//   - args.meta 是 JSON 且含 device_id：{"device_id":"phone-1"}
//   - args.meta 直接就是设备 id：phone-1
//   - args.device_id
//
// 留空则返回**可执行**的提示（列出在线设备），而不是含糊报错 —— 模型据此重试。
func (d *devicectlDevice) output(args map[string]interface{}) (interface{}, error) {
	deviceID, _ := args["device_id"].(string)
	if deviceID == "" {
		if metaStr, _ := args["meta"].(string); metaStr != "" {
			var m map[string]interface{}
			if json.Unmarshal([]byte(metaStr), &m) == nil {
				deviceID, _ = m["device_id"].(string)
				if deviceID == "" {
					deviceID, _ = m["device"].(string)
				}
			}
			if deviceID == "" {
				deviceID = strings.TrimSpace(metaStr)
			}
		}
	}
	if deviceID == "" {
		ids := []string{}
		for _, m := range d.reg.OnlineList() {
			ids = append(ids, m.DeviceID)
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("devicectl 需要 meta.device_id 才能投递；当前没有在线设备（device_list_channels 可看每台设备的 device-<id> 通道）")
		}
		return nil, fmt.Errorf("devicectl 需要 meta.device_id（或直接用通道 device-<id>）；当前在线设备: %s", strings.Join(ids, ", "))
	}
	return pushToDevice(d.reg, deviceID, args)
}

// Package client 实现设备桥（remotedevice 插件）的**完整客户端协议**：
// WebSocket 建链 / hello+bind / 命令路由 / 二进制分块回传 / 回执帧构造。
//
// ## 谁在用
//
//   - `cmd/waiter`（产品 CLI）：`Bridge` + `CmdRouter` + `ParseHomeagentCmd`
//   - `cmd/mock-server`：同一套协议做假设备
//
// ## 关于「编译为 C 共享库给 GUI FFI 调用」
//
// 包文档历史上这么写过，但**现在没有 c-shared 构建**（全仓 `grep c-shared`
// 只命中 SDK 工具链里的"不得再用 c-shared"注释）。而且本包在 `internal/`
// 下，外部模块按 Go 规则无法 import —— 它从来就不是对外 API 面。
//
// GUI 那侧是**自己在 JS 里重写了一份协议**（cmd/gui/main.js 的
// device-bridge 相关代码），不复用本包。
//
// ⇒ 这里的导出符号是给**同仓其他包**（waiter / mock-server）用的，
//
//	不是"给外部设备客户端实现者的公开协议面"。改导出签名前按内部
//	API 对待即可，不必担心破坏外部消费者。
package client

import "encoding/json"

// ===== 消息类型（与 remotedevice plugin 协议对齐） =====

// HelloMsg 设备登记消息
type HelloMsg struct {
	Op     string     `json:"op"`
	Device DeviceMeta `json:"device"`
}

// DeviceMeta 设备元信息
type DeviceMeta struct {
	DeviceID string                 `json:"device_id"`
	Name     string                 `json:"name"`
	Kind     string                 `json:"kind"`
	Caps     []string               `json:"caps"`
	Info     map[string]interface{} `json:"info,omitempty"`
}

// BindMsg 设备绑定消息
type BindMsg struct {
	Op       string `json:"op"`
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
}

// CmdMsg 服务端下发的命令消息
type CmdMsg struct {
	Op      string `json:"op"`
	ReqID   string `json:"req_id"`
	Command string `json:"command"`
	CmdType string `json:"cmd_type"`
}

// CmdResult 命令执行结果
type CmdResult struct {
	Op       string `json:"op"`
	ReqID    string `json:"req_id"`
	Status   string `json:"status"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
}

// DataStart 二进制数据传输开始（设备→网关）
type DataStart struct {
	Op        string `json:"op"`
	ReqID     string `json:"req_id"`
	Kind      string `json:"kind"`
	MIME      string `json:"mime"`
	Total     int    `json:"total"`
	ChunkSize int    `json:"chunk_size,omitempty"`
}

// DataEnd 二进制数据传输结束
type DataEnd struct {
	Op     string `json:"op"`
	ReqID  string `json:"req_id"`
	Status string `json:"status"`
	Total  int    `json:"total,omitempty"`
	Error  string `json:"error,omitempty"`
}

// SpeechStart TTS 音频数据开始（网关→设备）
type SpeechStart struct {
	Op    string `json:"op"`
	ReqID string `json:"req_id"`
	Kind  string `json:"kind"`
	MIME  string `json:"mime"`
	Total int    `json:"total"`
}

// SpeechEnd TTS 音频数据结束
type SpeechEnd struct {
	Op    string `json:"op"`
	ReqID string `json:"req_id"`
}

// StatusMsg 设备状态上报
type StatusMsg struct {
	Op       string `json:"op"`
	DeviceID string `json:"device_id"`
	Status   string `json:"status"`
}

// EventMsg 设备主动上报事件
type EventMsg struct {
	Op       string      `json:"op"`
	DeviceID string      `json:"device_id,omitempty"`
	Type     string      `json:"type"`
	Payload  interface{} `json:"payload,omitempty"`
}

// ===== 序列化辅助 =====

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

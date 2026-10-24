package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// discoveryPath 是 HologramAgent 的「设备网关在哪」端点（相对门户根）。
const discoveryPath = "/api/v1/device/gateway"

// discoveryResponse 是发现端点的响应。
type discoveryResponse struct {
	Available bool `json:"available"`
	// URL 是**子域形态**（devices.<基域名>）。浏览器能解析（RFC 6761 内置
	// 特例），但系统解析器（getent/Go/Node）通常解析不到 *.localhost —— 实测如此。
	URL string `json:"url"`
	// URLPortal 是**门户同源形态**（同一 host、同一端口，走路径挂载），
	// 无任何 DNS 依赖。非浏览器客户端应当用这个。
	URLPortal string `json:"url_portal"`
	Preferred string `json:"preferred"`
	Host      string `json:"host"`   // devices.<基域名>
	Auth      string `json:"auth"`   // homeagent | none
	Reason    string `json:"reason"` // available=false 时的原因
	Hint      string `json:"hint"`
}

// normalizeGateway 把用户给的地址整理成可直接连接的 WebSocket URL。
//
// 保留两种输入形态的旧行为：
//   - 已含路径（含 /api/v1/device/ws）→ 原样使用；
//   - 只有 host[:port] → 补 ws:// 与默认 WS 路径。
//
// 新增：**已带子域标签的地址不再被改写**（例如 devices.example.com）——
// 旧实现只判断"是否含路径"，对子域地址是对的；这里把这条显式化，
// 避免以后有人加"自动补门户路径"的逻辑时把它改坏。
func normalizeGateway(addr string) string {
	g := strings.TrimSpace(addr)
	if g == "" {
		return ""
	}
	if strings.HasPrefix(g, "ws://") || strings.HasPrefix(g, "wss://") {
		if strings.Contains(g, "/api/v1/device/ws") {
			return g
		}
		return strings.TrimRight(g, "/") + "/api/v1/device/ws"
	}
	// http(s):// 形态：转成 ws(s)://，其余同下
	if strings.HasPrefix(g, "https://") {
		g = "wss://" + strings.TrimPrefix(g, "https://")
	} else if strings.HasPrefix(g, "http://") {
		g = "ws://" + strings.TrimPrefix(g, "http://")
	} else {
		g = "ws://" + g
	}
	if strings.Contains(g, "/api/v1/device/ws") {
		return g
	}
	return strings.TrimRight(g, "/") + "/api/v1/device/ws"
}

// discoverGateway 向门户询问设备网关的**权威地址**。
//
// 为什么需要：设备网关现在位于 devices.<基域名> 的子域反代上，而基域名与
// 子域标签都是**服务端配置**（webui.base_domain / 插件声明），客户端无从得知。
// 让服务端回答「网关在哪」是唯一不会漂移的做法。
//
// portalURL 是用户配置的门户地址（可能带路径/尾斜杠）；token 是门户 api_key。
// 任何失败都返回错误，由调用方决定是否回退到自配地址 —— 发现是**增强**而非
// 必需，老版本 HologramAgent 没有这个端点。
func discoverGateway(portalURL, token string, timeout time.Duration) (string, error) {
	base := strings.TrimSpace(portalURL)
	if base == "" {
		return "", fmt.Errorf("门户地址为空")
	}
	// http(s) → 对应的门户根；ws(s) 输入也要能问（GUI 里同一字段混用两种形态）
	switch {
	case strings.HasPrefix(base, "wss://"):
		base = "https://" + strings.TrimPrefix(base, "wss://")
	case strings.HasPrefix(base, "ws://"):
		base = "http://" + strings.TrimPrefix(base, "ws://")
	case !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://"):
		base = "http://" + base
	}
	// 用户可能填的是完整网关地址（含 /api/v1/device/ws）：截到根再拼发现路径
	if i := strings.Index(base, "/api/v1/"); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimRight(base, "/")

	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, base+discoveryPath, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("X-API-Key", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("发现端点返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var d discoveryResponse
	if err := json.Unmarshal(body, &d); err != nil {
		return "", fmt.Errorf("发现响应无法解析: %w", err)
	}
	if !d.Available {
		msg := d.Reason
		if msg == "" {
			msg = "服务端报告设备网关不可用"
		}
		return "", fmt.Errorf("%s", msg)
	}
	// 优先门户同源形态：waiter 是普通进程，走系统解析器，
	// 而 *.localhost 在系统解析器下通常解析不到（只有浏览器内置该特例）。
	if p := strings.TrimSpace(d.URLPortal); p != "" {
		return p, nil
	}
	if p := strings.TrimSpace(d.URL); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("服务端未给出可用的网关地址")
}

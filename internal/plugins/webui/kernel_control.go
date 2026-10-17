package webui

// 内核重启 / 关机的 HTTP 端点。
//
// ## 为什么这两个端点必须在 WebUI 里
//
// 有些状态变更只能靠重启内核生效（换 LLM 源、换主密钥、装配期配置）。
// 没有这两个按钮时，用户只能自己 ssh 上去敲 systemctl —— 而 WebUI
// 是大多数人唯一的界面。
//
// ## 断线是**预期行为**，不是故障
//
// 两个动作都会让内核进程消失：
//   · 重启：guard 用当前二进制重拉，断线几秒
//   · 关机：systemd 不再拉起，断线到人工启动
//
// ⇒ 必须**先把响应写回浏览器**，再让内核退出。否则用户看到的是
//   "点了一下，连接被重置"，无法区分"正在重启"和"真出故障了"。

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// handleKernelRestart 处理 POST /api/v1/kernel/restart
func (h *Handler) handleKernelRestart(w http.ResponseWriter, r *http.Request) {
	h.kernelControl(w, r, "restart")
}

// handleKernelShutdown 处理 POST /api/v1/kernel/shutdown
func (h *Handler) handleKernelShutdown(w http.ResponseWriter, r *http.Request) {
	h.kernelControl(w, r, "shutdown")
}

func (h *Handler) kernelControl(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	kc := h.kernelCtl
	if kc == nil {
		// 明确说"不可用"而不是静默失败：用户点按钮没反应会以为界面坏了
		writeJSONError(w, http.StatusServiceUnavailable,
			"当前部署没有接入内核控制通道（该构建未启用，或插件未声明 kernel-control 能力）")
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	// body 可为空（curl 直接 POST 不带内容）
	_ = json.NewDecoder(r.Body).Decode(&body)
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "WebUI 操作"
	}

	var err error
	if action == "restart" {
		err = kc.RequestKernelRestart(reason)
	} else {
		err = kc.RequestKernelShutdown(reason)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// ★ 先把响应写回去（Flush），再让内核消失。
	//
	// 不 Flush 的话响应可能还在 net/http 的缓冲里，进程就退出了 ⇒
	// 浏览器收到的是 connection reset，用户以为出错了。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	msg := map[string]interface{}{
		"status": "ok",
		"action": action,
		"reason": reason,
	}
	if action == "restart" {
		msg["message"] = "内核正在重启（约数秒）。页面稍后会自动重连。"
		msg["reconnect_hint_ms"] = 8000
	} else {
		msg["message"] = "内核正在关闭。需要人工执行 systemctl start 才能再启动。"
	}
	_ = json.NewEncoder(w).Encode(msg)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 给浏览器一点时间收包，再让内核真退出
	time.Sleep(300 * time.Millisecond)
}

// writeJSONError 是 handler 里统一的错误输出（保持与其它端点一致）。
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

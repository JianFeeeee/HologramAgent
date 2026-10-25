package webui

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"encoding/base64"
	"encoding/json"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"net/http"
	"path/filepath"
)

// 上传面：用户上传文件（handleChatFile）与下载（handleUploads）、中断对话。

// parseIntDefault 解析十进制整数，失败/空串返回 def。
func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// maxInlineMediaBytes 是上传媒体内联进 LLM 请求的字节上限。
//
// base64 会胀大 4/3，8MB 原图变成 ~11MB 文本；再加上网关的请求体上限与
// 模型的图像 token 预算，超过这个量级多半会被上游 413 拒掉。
// 超限时退回按路径处理（模型可用 describe_image 主动看）而不是报错。
const maxInlineMediaBytes = 8 << 20

// handleChatFile 处理用户经 webui 上传文件并附带消息注入 agent。
// 设计对齐 qq 插件收文件模式：文件落盘到固定目录（<data>/uploads），
// 注入文本带「文件名 + 保存路径」，agent 用 files_read 等工具按路径消费。
// 表单字段：file（必填，multipart 文件）、message（可选附言）、device_id/device_name。
func (h *Handler) handleChatFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if uploadsDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "uploads dir not initialized"})
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil { // 单文件上限 64MB
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart: " + err.Error()})
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file field required"})
		return
	}
	defer file.Close()
	message := r.FormValue("message")
	deviceID := r.FormValue("device_id")
	deviceName := r.FormValue("device_name")
	clientMsgID := r.FormValue("client_msg_id")

	if h.sdk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent unavailable"})
		return
	}

	// 落盘：保留原文件名；重名加毫秒后缀防覆盖。文件名消毒防路径穿越。
	base := filepath.Base(hdr.Filename)
	if base == "" || base == "." || strings.Contains(base, "..") {
		base = "upload.bin"
	}
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create uploads dir"})
		return
	}
	savePath := filepath.Join(uploadsDir, base)
	if _, err := os.Stat(savePath); err == nil {
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		savePath = filepath.Join(uploadsDir, fmt.Sprintf("%s_%d%s", stem, time.Now().UnixMilli(), ext))
	}
	out, err := os.Create(savePath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save file"})
		return
	}
	sz, err := io.Copy(out, file)
	out.Close()
	if err != nil {
		os.Remove(savePath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write file"})
		return
	}

	// 下载 URL（前端附件卡片用）：/uploads/ 与 /files/ 同一鉴权模型，路由在 RegisterRoutes 挂载
	dlURL := "/uploads/" + filepath.Base(savePath)
	attType := "file"
	ct := hdr.Header.Get("Content-Type")
	if ct == "" {
		// 部分客户端（curl -F、某些移动端）不带 Content-Type，退回按扩展名判定。
		// 判错的后果不只是卡片样式：图片被当普通文件就走不进视觉链路，模型看不到图。
		ct = contentTypeByExt(strings.ToLower(filepath.Ext(savePath)))
	}
	switch {
	case strings.HasPrefix(ct, "image/"):
		attType = "image"
	case strings.HasPrefix(ct, "audio/"):
		attType = "audio"
	}

	// 图片/音频直接进多模态链路：读回字节拼 data URL，随本轮 message 发给模型。
	//
	// 此前只注入一句「文件已保存到 <路径>」，指望模型自己调 files_read——
	// 但 files_read 返回的是文本，图片的字节对模型永远不可见，除非它想到再调
	// describe_image。走 InjectInputMedia 后与用户在 qq 发图走同一条统一输入主干：
	// 自动落进 CAS、挂上媒体记忆引用，且模型「本轮」就看得到图。
	var mediaBlocks []sdk.ContentBlock
	if attType == "image" || attType == "audio" {
		if sz > maxInlineMediaBytes {
			log.Printf("[webui] %s %s has %s exceeding inline limit %s, falling back to path handling",
				attType, base, formatBytesGo(sz), formatBytesGo(maxInlineMediaBytes))
		} else if raw, err := os.ReadFile(savePath); err != nil {
			log.Printf("[webui] reading back uploaded %s failed, falling back to path handling: %v", attType, err)
		} else {
			dataURL := "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(raw)
			if attType == "image" {
				mediaBlocks = []sdk.ContentBlock{{
					Type:     "image_url",
					ImageURL: &sdk.ImageURL{URL: dataURL, Detail: "auto"},
				}}
			} else {
				mediaBlocks = []sdk.ContentBlock{{
					Type:     "audio_url",
					AudioURL: &sdk.AudioURL{URL: dataURL},
				}}
			}
		}
	}

	// 注入 agent：文件元信息走 interrupt 通道（内核以 system 角色注入 LLM，
	// 不写入用户对话履历、不产生独立用户气泡——对齐 terminal_watch/timer 的
	// 工具提醒模式）。用户的附言若有则作为正常消息先行注入。
	// qq 插件同款文本格式：[xx发送了文件] + 路径，agent 用 files_read 消费。
	humanSize := formatBytesGo(sz)
	source := "webui"
	if deviceID != "" {
		source = "webui/" + deviceID
	}
	typeLabel := map[string]string{"image": "图片", "audio": "音频", "file": "文件"}[attType]
	if typeLabel == "" {
		typeLabel = "文件"
	}
	fileNote := fmt.Sprintf("[用户通过 webui 发送了%s: %s (%s)]\n文件已保存到: %s\n可用 files_read 等工具读取此路径处理。",
		typeLabel, base, humanSize, savePath)
	// 媒体已随本轮发给模型时不再叫它去读文件：那只会读到一堆二进制字节。
	if len(mediaBlocks) > 0 {
		fileNote = fmt.Sprintf("[用户通过 webui 发送了%s: %s (%s)]\n原文件保存在: %s",
			typeLabel, base, humanSize, savePath)
	}
	if message != "" {
		text := message
		go func() {
			// 附言作为用户消息（带附件卡片）注入；文件说明紧随其后以 interrupt 补充
			payload2 := map[string]interface{}{"content": text}
			if deviceID != "" {
				payload2["device_id"] = deviceID
				payload2["device_name"] = deviceName
			}
			if clientMsgID != "" {
				payload2["client_msg_id"] = clientMsgID + "-note"
			}
			payload2["upload_url"] = dlURL
			payload2["upload_type"] = attType
			payload2["upload_size"] = sz
			payload2["upload_name"] = base
			// 媒体跟附言同一条注入：拆开会让模型先看到「帮我看看这张图」而图在下一轮才到。
			if len(mediaBlocks) > 0 {
				payload2["media_blocks"] = mediaBlocks
			}
			h.sdk.InjectInput(source, "webui", "text", payload2)
		}()
		time.Sleep(100 * time.Millisecond) // 保证附言先入队
		h.sdk.InjectInterrupt(source, "webui", "text", map[string]interface{}{"content": fileNote, "no_memory": true})
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "accepted",
			"file":   map[string]interface{}{"url": dlURL, "name": base, "size": sz, "path": savePath, "type": attType},
		})
		return
	}
	// 无附言：仅文件说明，直接同步注入并等待回复（与普通聊天体验一致）
	payload := map[string]interface{}{
		"content":     fileNote,
		"upload_url":  dlURL,
		"upload_type": attType,
		"upload_size": sz,
		"upload_name": base,
	}
	if len(mediaBlocks) > 0 {
		payload["media_blocks"] = mediaBlocks
	}
	if deviceID != "" {
		payload["device_id"] = deviceID
		payload["device_name"] = deviceName
	}
	if clientMsgID != "" {
		payload["client_msg_id"] = clientMsgID
	}

	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	respCh := make(chan *agentIO.OutputEvent, 1)
	go func() {
		respCh <- h.sdk.InjectInputSync(source, "webui", "text", payload)
	}()
	var resp *agentIO.OutputEvent
	select {
	case resp = <-respCh:
	case <-ctx.Done():
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "agent timeout"})
		return
	}
	if resp == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent unavailable"})
		return
	}
	content, _ := resp.Payload["content"].(string)
	reasoning, _ := resp.Payload["reasoning_content"].(string)
	result := map[string]interface{}{
		"response": content,
		"file":     map[string]interface{}{"url": dlURL, "name": base, "size": sz, "path": savePath, "type": attType},
	}
	if reasoning != "" {
		result["reasoning_content"] = reasoning
	}
	writeJSON(w, http.StatusOK, result)
}

// formatBytesGo 服务端字节人性化显示。
func formatBytesGo(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB"}
	i := 0
	f := float64(n)
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// handleUploads 服务 /uploads/<name>：用户上传文件的下载（与 /files/ 同一安全模型）。
func (h *Handler) handleUploads(w http.ResponseWriter, r *http.Request) {
	if uploadsDir == "" {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/uploads/")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	fp := filepath.Join(uploadsDir, name)
	f, err := os.Open(fp)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	ct := contentTypeByExt(strings.ToLower(filepath.Ext(name)))
	w.Header().Set("Content-Type", ct)
	if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") {
		w.Header().Set("Content-Disposition", "inline; filename="+name)
	} else {
		w.Header().Set("Content-Disposition", "attachment; filename="+name)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// handleChatInterrupt 注入用户中断或「停止」。
//
// 两种语义分开（用户明确的设计）：
//   - **停止**（message 为空，即停止按钮）：①立即结束当前 LLM 推理（不重试）；
//     ②对停止那一刻已排队的 x 条消息，后续依次在 pre-action 阶段短路。
//     只传 stop=true，不入中断队列——旧实现把空停止当普通中断入队，取消后
//     还会以空内容重跑一轮，表现为“停了又活”。
//   - **中断/补充指令**（message 非空）：取消当前流式请求 + 中断入队，
//     模型看到被打断的上下文与用户新输入。
func (h *Handler) handleChatInterrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.sdk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent unavailable"})
		return
	}
	var body struct {
		Message  string `json:"message"`
		DeviceID string `json:"device_id"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // body 可选
	}

	source := "webui"
	if body.DeviceID != "" {
		source = "webui/" + body.DeviceID
	}
	// PriorityL4：终止/停止必须能立即打断当前任务（内核级插件才有的能力）。
	// agent 正卡在工具执行里时按不下手——那是临界区，由内核在安全点生效；
	// 但 LLM 流式段会被立刻取消。
	payload := map[string]interface{}{
		"content":  body.Message,
		"priority": sdk.PriorityL4,
	}
	if strings.TrimSpace(body.Message) == "" {
		// 空消息 = 停止。必须带 stop 标记，否则空内容会被 interceptLoop
		// 当成空操作丢掉（旧行为：接口回 200，实际什么都没发生）。
		payload["stop"] = true
	}
	h.sdk.InjectInterrupt(source, "webui", "text", payload)
	writeJSON(w, http.StatusOK, map[string]string{"status": "interrupted"})
}

package core

import (
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// 媒体记忆接线：把对话里出现的图片/音频变成一等记忆块。
//
// ★ 2026-10-11：块自带负载（CAS 已淘汰）。
//
// 此前这里把字节存进独立的内容寻址存储（CAS），块只留一个 digest 指向它。
// 那是把「媒体的内容」从块里拆出去另建一层——现在媒体字节直接进块的 Data，
// 与文本块的 Text 对称：一个块要么带文本、要么带字节。
//
// 媒体进入对话有两条路：用户直接发图（ContentBlock data URL）、插件注入
// （SetToolBlocks）。两条都在这里收口成块，挂到当轮 ContextEvent；
// 事件被 Prune 时块随之迁移到 L2 文档（块本身不变，只换持有层）。
//
// 不再生成任何描述文本，也不再往正文写 media marker：图片只按自己的
// 统一空间向量被检索，描述式索引是将就方案。

// blockModalityFromMIME 按 MIME 大类推断块的模态。
func blockModalityFromMIME(mime string) memory.BlockModality {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return memory.BlockImage
	case strings.HasPrefix(mime, "video/"):
		return memory.BlockVideo
	case strings.HasPrefix(mime, "audio/"):
		return memory.BlockAudio
	default:
		return memory.BlockText
	}
}

// captureBlockMedia 把 blocks 里的 data URL 媒体变成一等记忆块，返回块列表。
//
// 只处理 data URL：http(s) URL 拿不到字节，而"下载它再存"会把一次对话变成
// 一次网络请求（超时、鉴权、SSRF 全来了），不在本层解决。
//
// 块 ID 由内容派生（memory.MediaBlockID）：同内容 ⇒ 同 ID ⇒ 落库时
// ON CONFLICT 天然去重（这正是内容寻址当年的好处，但现在由块 ID 直接提供，
// 不需要一层独立的寻址存储）。
func (a *Agent) captureBlockMedia(blocks []agentAPI.ContentBlock, tool string) []memory.MemoryBlock {
	if len(blocks) == 0 {
		return nil
	}

	var out []memory.MemoryBlock
	for _, b := range blocks {
		var url string
		switch {
		case b.ImageURL != nil && b.ImageURL.URL != "":
			url = b.ImageURL.URL
		case b.AudioURL != nil && b.AudioURL.URL != "":
			url = b.AudioURL.URL
		default:
			continue
		}

		mime, data, ok := parseDataURL(url)
		if !ok {
			continue // http(s) URL 或格式不认，跳过
		}

		block := memory.MemoryBlock{
			ID:        memory.MediaBlockID(mime, data),
			Modality:  blockModalityFromMIME(mime),
			Data:      data,
			MIME:      mime,
			Size:      int64(len(data)),
			Tool:      tool,
			CreatedAt: time.Now(),
		}
		// 入库即算一次多模态坐标（多模态空间可用且为图像时）。
		// 之后块召回直接用它；换模型由启动时的 reembedStaleMedia 补算。
		a.embedBlockOnIngest(&block)
		out = append(out, block)
	}
	return out
}

// embedBlockOnIngest 给刚捕获的块立即计算多模态坐标。
// 只在多模态空间可用且为图像时执行；音频/未配置时静默跳过（保持既有行为）。
func (a *Agent) embedBlockOnIngest(b *memory.MemoryBlock) {
	if b == nil || a.multimodalSpace == nil || !a.multimodalSpace.Loaded() {
		return
	}
	if !strings.HasPrefix(b.MIME, "image/") {
		return
	}
	vec, err := a.multimodalSpace.EmbedImageDense(b.Data, b.MIME)
	if err != nil {
		log.Printf("[media] 入库嵌入失败 %s: %v", shortID(b.ID), err)
		return
	}
	b.Vector = vec
	b.Fingerprint = a.multimodalSpace.Fingerprint()
}

// stageMediaBlocks 累积本轮捕获的块，等 ContextEvent 建好后一起挂上。
//
// 为何要缓存而不是当场挂：块在 process() 执行期间被捕获，而承载它的
// ContextEvent 要等 process() 返回后才 Append——此刻还没有 owner。
// 与既有的 a.pendingMedia 同一手法（均由 schedulerLoop goroutine 独占读写）。
func (a *Agent) stageMediaBlocks(blocks ...memory.MemoryBlock) {
	if len(blocks) == 0 {
		return
	}
	a.pendingMediaBlocks = append(a.pendingMediaBlocks, blocks...)
}

// drainMediaBlocks 取出并清空本轮累积的块。
func (a *Agent) drainMediaBlocks() []memory.MemoryBlock {
	if len(a.pendingMediaBlocks) == 0 {
		return nil
	}
	out := a.pendingMediaBlocks
	a.pendingMediaBlocks = nil
	return out
}

// bindEventMedia 把本轮捕获的块挂到 ContextEvent 上。
//
// 块存储在事件自身（随 context.json 持久化），不维护引用账本：
// 存活与否由持有它的层决定。
func (a *Agent) bindEventMedia(evt *ContextEvent, blocks []memory.MemoryBlock) {
	if evt == nil || len(blocks) == 0 {
		return
	}
	if evt.ID == "" {
		evt.ID = newEventID()
	}
	evt.Blocks = append(evt.Blocks, blocks...)
}

// blockLabel 渲染一行媒体块标签，供提示词告知"这条记忆带着哪份媒体"。
//
// 不含任何生成的描述文本：图片按自己的向量被检索，标签仅提供 MIME 与
// 短块 ID，让模型知道有这份媒体、并可据此取回（块 ID 是它的唯一身份）。
func blockLabel(b *memory.MemoryBlock) string {
	if b == nil {
		return ""
	}
	label := string(b.Modality)
	if b.MIME != "" {
		label = b.MIME
	}
	return fmt.Sprintf("[%s %s]", label, shortID(b.ID))
}

// parseDataURL 从 data:<mime>;base64,<data> 解出 MIME 与原始字节。
//
// 自 CAS 淘汰后，这段解析归内核媒体接线自己持有（原在 media 包）。
func parseDataURL(url string) (mime string, data []byte, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", nil, false
	}
	rest := url[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", nil, false
	}
	head := rest[:comma]
	payload := rest[comma+1:]
	if !strings.HasSuffix(strings.ToLower(head), ";base64") {
		return "", nil, false
	}
	mime = head[:len(head)-len(";base64")]
	if mime == "" || payload == "" {
		return "", nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}
	return mime, decoded, true
}

// newEventID 生成 ContextEvent 的稳定标识。
//
// 沿用 document.Store 的 doc_<unixnano> 手法（同一份代码库里保持一致，
// 也避免为此引入 uuid 依赖）。纳秒精度足够：同一 Agent 的事件由
// schedulerLoop 单 goroutine 串行 Append，不存在同纳秒两条。
func newEventID() string {
	return fmt.Sprintf("evt_%d", time.Now().UnixNano())
}

// shortID 把块 ID 截短供日志/提示词使用。
func shortID(id string) string {
	if len(id) > 20 {
		return id[:20]
	}
	return id
}

package core

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 多模态回退链：主模型看不到图/听不到音频时，改用一个声明了 vision/audio
// 能力的源把媒体转写成文字，再以 text block 注入。
//
// 为何必须有这条链：core.llm.model=AUTO 时实际落到哪个上游由网关按优先级决定，
// 而网关可能把 image_url 块静默剥离后转发给纯文本上游（llmsproxy 的
// opencode adapter 就明写着 "multimodal part not supported by zen" 并丢弃
// 非 text part）。请求依然返回 200，带图与不带图的 prompt_tokens 完全相同，
// 模型于是回答「我没有看到图片」，而内核以为注入成功。
//
// 没有这条链的话，multimodal 插件在任何非视觉主模型下都只能假成功。

const (
	// modalFallbackTimeout 单次转写调用的上限。
	//
	// 为何是 180s：生产实测经网关转 claude-opus-5 看一张 400x400 图要 ~81s，
	// 90s 阅则定时贴着上限，多帧批量请求更慢。宁可等也不要徒劳一趟。
	modalFallbackTimeout = 180 * time.Second

	// modalFallbackMaxTokens 转写输出上限。描述一组图/一段音频不需要长文，
	// 且这段文字要塞回主模型上下文，过长会挤掉真正的对话内容。
	modalFallbackMaxTokens = 1500

	// modalFallbackMaxBlocks 单次最多转写多少个媒体块。
	// see_video 一次能注入 10 帧；即使批量合包，图越多上游越慢也越容易超
	// 单请求体积限制。超出部分如实报告未转写。
	modalFallbackMaxBlocks = 6
)

// modalFallbackResult 描述一次回退转写的结果，供调用方决定注入什么。
type modalFallbackResult struct {
	// Text 是转写出的文字（已含来源标注），为空表示没有可注入内容。
	Text string
	// Converted 实际成功转写的块数。
	Converted int
	// Skipped 因超出 modalFallbackMaxBlocks 而未处理的块数。
	Skipped int
	// Notice 给模型看的说明（能力缺失、转写失败等），始终如实。
	Notice string
}

// resolveModalFallback 按配置挑一个能处理该模态的 provider。
//
// 顺序：配置指定的 fallback_provider → 任意声明了该能力的已注册源。
// 后者是刻意的兜底：用户可能只在源上声明了 vision 而忘了填 fallback_provider，
// 此时静默失败比多找一个能用的源更糟。
func (a *Agent) resolveModalFallback(kind string) (agentAPI.Provider, string) {
	if a.providerManager == nil {
		return nil, ""
	}

	var configured string
	switch kind {
	case "image":
		configured = strings.TrimSpace(a.inputCfg.Image.FallbackProvider)
	case "audio":
		configured = strings.TrimSpace(a.inputCfg.Audio.FallbackProvider)
	}

	supports := func(p agentAPI.Provider) bool {
		if p == nil {
			return false
		}
		if kind == "audio" {
			return agentAPI.ProviderSupportsAudio(p)
		}
		return agentAPI.ProviderSupportsVision(p)
	}

	if configured != "" {
		p := a.providerManager.Get(configured)
		if p == nil {
			log.Printf("[agent] modal fallback %s: configured provider %q not registered", kind, configured)
		} else if !supports(p) {
			// 配置指向了一个没声明该能力的源：照用只会重演静默剥离，
			// 因此拒绝并继续找，日志点明配置与声明不一致。
			log.Printf("[agent] modal fallback %s: provider %q does not declare the capability, ignoring", kind, configured)
		} else if !a.providerManager.IsAvailable(configured) {
			log.Printf("[agent] modal fallback %s: provider %q in cooldown, trying others", kind, configured)
		} else {
			return p, configured
		}
	}

	// 兜底：扫已注册源，取第一个声明了该能力且当前可用的。
	for _, name := range a.providerManager.List() {
		if name == configured {
			continue // 上面已试过
		}
		p := a.providerManager.Get(name)
		if supports(p) && a.providerManager.IsAvailable(name) {
			return p, name
		}
	}
	return nil, ""
}

// modalFallbackModel 返回该模态回退调用应使用的模型名（空则用源自身默认）。
func (a *Agent) modalFallbackModel(kind string) string {
	switch kind {
	case "image":
		return strings.TrimSpace(a.inputCfg.Image.FallbackModel)
	case "audio":
		return strings.TrimSpace(a.inputCfg.Audio.FallbackModel)
	}
	return ""
}

// modalFallbackPrompt 返回转写用的提示词，配置为空时给一个可用默认。
func (a *Agent) modalFallbackPrompt(kind string) string {
	switch kind {
	case "image":
		if s := strings.TrimSpace(a.inputCfg.Image.DescribePrompt); s != "" {
			return s
		}
		return "Describe this image in detail: text, objects, people and scene."
	case "audio":
		if s := strings.TrimSpace(a.inputCfg.Audio.DescribePrompt); s != "" {
			return s
		}
		return "请转写这段音频的内容。"
	}
	return ""
}

// transcribeBlocksForFallback 把主模型看不懂的媒体块转写成文字。
//
// blocks 里的 text 块原样保留（它们本来就能被理解）；image_url/audio_url
// **按模态批量合包，每类只发一次请求**。
//
// 为何必须批量而不是逐块：生产实测 see_video 注入 6 帧时，逐帧调用让
// 4 帧里 3 帧超时，整轮拖到 363 秒。而视觉模型本来就能在一条消息里看
// 多张图——一次调用不仅快上一个数量级，模型还能看到帧与帧的时间推进
// 关系，分析质量更好。
//
// 返回的 Text 已带来源标注，让模型知道这是转写而非自己直接看到的。这点
// 很重要：模型据此能判断细节可靠性，也不会在用户追问像素级细节时编造。
func (a *Agent) transcribeBlocksForFallback(blocks []agentAPI.ContentBlock) modalFallbackResult {
	var res modalFallbackResult
	var kept []string      // 原样保留的 text 块
	var converted []string // 转写结果
	var notices []string

	// 先按模态分组，同时应用块数上限。
	var imgURLs, imgDetails []string
	var audURLs []string
	mediaSeen := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				kept = append(kept, b.Text)
			}
			continue
		case "image_url":
			mediaSeen++
			if mediaSeen > modalFallbackMaxBlocks {
				res.Skipped++
				continue
			}
			if b.ImageURL != nil && b.ImageURL.URL != "" {
				imgURLs = append(imgURLs, b.ImageURL.URL)
				imgDetails = append(imgDetails, b.ImageURL.Detail)
			}
		case "audio_url":
			mediaSeen++
			if mediaSeen > modalFallbackMaxBlocks {
				res.Skipped++
				continue
			}
			if b.AudioURL != nil && b.AudioURL.URL != "" {
				audURLs = append(audURLs, b.AudioURL.URL)
			}
		default:
			continue // 未知块类型：主模型也看不懂，丢弃
		}
	}

	// 图片：一次请求带全部帧
	if len(imgURLs) > 0 {
		p, srcName := a.resolveModalFallback("image")
		if p == nil {
			notices = append(notices, "The current model does not support images and no vision fallback source is available "+
				"(set core.input_processing.image.fallback_provider and core.llm.sources.<name>.vision=true on that source)")
		} else if text, err := a.chatModalFallbackBatch(p, "image", imgURLs, imgDetails); err != nil {
			// 转写失败必须说出来。静默跳过会让模型以为「图里没内容」，
			// 而事实是没人看过这些图。
			notices = append(notices, fmt.Sprintf("image transcription failed (source %s): %v", srcName, err))
			log.Printf("[agent] modal fallback image(%d) via %s failed: %v", len(imgURLs), srcName, err)
		} else {
			label := "image content"
			if len(imgURLs) > 1 {
				label = fmt.Sprintf("%d images/video frames", len(imgURLs))
			}
			converted = append(converted, fmt.Sprintf("[%s · transcribed by %s, not directly perceived by the current model]\n%s", label, srcName, text))
			res.Converted += len(imgURLs)
		}
	}

	// 音频：同样一次请求
	if len(audURLs) > 0 {
		p, srcName := a.resolveModalFallback("audio")
		if p == nil {
			notices = append(notices, "The current model does not support audio and no audio fallback source is available "+
				"(set core.input_processing.audio.fallback_provider and core.llm.sources.<name>.audio=true on that source)")
		} else if text, err := a.chatModalFallbackBatch(p, "audio", audURLs, nil); err != nil {
			notices = append(notices, fmt.Sprintf("audio transcription failed (source %s): %v", srcName, err))
			log.Printf("[agent] modal fallback audio(%d) via %s failed: %v", len(audURLs), srcName, err)
		} else {
			converted = append(converted, fmt.Sprintf("[音频内容 · 由 %s 转写，非当前模型直接感知]\n%s", srcName, text))
			res.Converted += len(audURLs)
		}
	}

	if res.Skipped > 0 {
		notices = append(notices, fmt.Sprintf(
			"另有 %d 个媒体块未转写（单次上限 %d）",
			res.Skipped, modalFallbackMaxBlocks))
	}

	var parts []string
	parts = append(parts, kept...)
	parts = append(parts, converted...)
	if len(notices) > 0 {
		parts = append(parts, "[注意] "+strings.Join(notices, "；"))
	}
	res.Text = strings.Join(parts, "\n\n")
	res.Notice = strings.Join(notices, "；")
	return res
}

// prepareToolBlocks 判定当前主模型能否直接消费这批媒体块。
//
// 返回 (native, "")：能直接看/听，原样作为 Blocks 注入。
// 返回 (nil, text) ：不能，已经回退链转写成文字，调用方并进纯文本 content。
// 返回 (nil, "")  ：既不能直接看也没回退源且无话可说（理论上不发生，
// transcribeBlocksForFallback 至少会给一条 notice）。
//
// 为何逐模态判定而不是一刀切：一批块里可能图能看、音频不能听（很多视觉
// 模型就是这样）。全部走回退会白白把本可直视的图降级成二手文字描述。
func (a *Agent) prepareToolBlocks(blocks []agentAPI.ContentBlock) ([]agentAPI.ContentBlock, string) {
	canVision := agentAPI.ProviderSupportsVision(a.provider)
	canAudio := agentAPI.ProviderSupportsAudio(a.provider)

	var native []agentAPI.ContentBlock
	var needFallback []agentAPI.ContentBlock
	for _, b := range blocks {
		switch b.Type {
		case "image_url":
			if canVision {
				native = append(native, b)
			} else {
				needFallback = append(needFallback, b)
			}
		case "audio_url":
			if canAudio {
				native = append(native, b)
			} else {
				needFallback = append(needFallback, b)
			}
		default:
			native = append(native, b) // text 等一律直通
		}
	}

	if len(needFallback) == 0 {
		return native, ""
	}

	res := a.transcribeBlocksForFallback(needFallback)
	log.Printf("[agent] modal fallback: %d block(s) transcribed, %d skipped (provider=%s vision=%v audio=%v)",
		res.Converted, res.Skipped, a.provider.Name(), canVision, canAudio)

	// 部分能直视、部分需转写：把转写文字作为 text 块并入 native，
	// 这样两部分内容同时到达模型。
	if len(native) > 0 {
		if res.Text != "" {
			native = append(native, agentAPI.ContentBlock{Type: "text", Text: res.Text})
		}
		return native, ""
	}
	return nil, res.Text
}

// chatModalFallbackBatch 向回退 provider 发**一次**请求，带上该模态的全部媒体。
//
// 多张图合包而非逐张调用：既为避开 N 倍往返延迟（生产实测逐帧调用使
// see_video 6 帧拖到 363 秒且 3/4 帧超时），也因为视觉模型看到成组帧时能
// 描述帧间变化，而逐帧转写只能得到 N 段互不相关的静态描述。
func (a *Agent) chatModalFallbackBatch(p agentAPI.Provider, kind string, urls, details []string) (string, error) {
	if len(urls) == 0 {
		return "", fmt.Errorf("no media to transcribe")
	}

	prompt := a.modalFallbackPrompt(kind)
	if len(urls) > 1 && kind == "image" {
		// 多张时补一句，否则模型容易只描述第一张。
		prompt = fmt.Sprintf("%s\n\n共 %d 张（若为视频关键帧则按时间顺序），"+
			"请逐张编号描述，并在最后概括帧间变化。", prompt, len(urls))
	}

	msg := agentAPI.Message{
		Role:   "user",
		Blocks: []agentAPI.ContentBlock{{Type: "text", Text: prompt}},
	}
	for i, u := range urls {
		if kind == "audio" {
			msg.Blocks = append(msg.Blocks, agentAPI.ContentBlock{
				Type:     "audio_url",
				AudioURL: &agentAPI.AudioURL{URL: u},
			})
			continue
		}
		detail := ""
		if i < len(details) {
			detail = details[i]
		}
		if detail == "" {
			// 单张时看清细节；多张（视频帧）用 low 控住体积与耗时。
			if len(urls) > 1 {
				detail = "low"
			} else {
				detail = "high"
			}
		}
		msg.Blocks = append(msg.Blocks, agentAPI.ContentBlock{
			Type:     "image_url",
			ImageURL: &agentAPI.ImageURL{URL: u, Detail: detail},
		})
	}

	ctx, cancel := context.WithTimeout(a.ctx, modalFallbackTimeout)
	defer cancel()
	resp, err := p.Chat(ctx, &agentAPI.CompletionRequest{
		Model:     a.modalFallbackModel(kind),
		Messages:  []agentAPI.Message{msg},
		MaxTokens: modalFallbackMaxTokens,
	})
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" {
		// 空回复不能当成功。上游剥掉媒体块后模型往往回一句「我没看到图片」
		// 或干脆空串——两种都说明这条回退链也没真看到。
		return "", fmt.Errorf("fallback source returned empty content (it may not support this modality either)")
	}
	return out, nil
}

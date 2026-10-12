// Package multimodal 是多模态工具的内置插件实现。
//
// # 为什么从内核搬出来（2026-10-05/06）
//
// `ocr_image` / `describe_image` / `transcribe_audio` 原先是内核
// `buildToolDefs()` 里硬编码的内置条件工具。两个问题：
//
//  1. **违反本仓第一原则**。README 写着「内核职责限定在 LLM 编排、记忆管理与
//     知识检索；所有 IO 能力由插件实现」，而「文件读取」正在 IO 列表里。
//     三个工具读本地文件（path 参数）却住在**内核进程内**——无沙箱、无能力面、
//     无审计。这是整个 `internal/plugin/proc/capability.go` 那套权限梯度
//     存在的意义所要防止的事。
//  2. **名不副实**。`ocr_image` 里没有任何 OCR 引擎，它只是「换个 prompt
//     再问 VLM 一次」（真 OCR 在 internal/nlp 的 ONNX，没接进来）。
//     工具名承诺的能力大于实现，模型据此误判精度。
//
// # 搬走之后内核还剩什么
//
// **input 通路留在内核**：`processInput` 处理用户直接发来的图/音时，
// 仍由内核把 media block 拼进本轮 LLM 请求（那是「输入即上下文」的一部分，
// 不是工具调用）。留在内核的是 `pendingMedia` 那个 map。
//
// # 本插件如何拿到媒体（不依赖内核的 pendingMedia）
//
// 两条来源，都不经内核：
//
//	① **path**：设备/工具回传的媒体已被 remotedevice 落盘（如 screensee 的
//	   file 字段），插件直接读文件。
//	② **块引用**：用户直接上传的媒体，内核在 `processInput` 里已落成记忆块
//	   （内容派生 ID）。插件用 `sdk.Memory().MediaBytes(ref)` 取回字节 ——
//	   比在内核与插件之间传 base64 干净得多（内核那份 pendingMedia 可以退场）。
package multimodal

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta("multimodal", "多模态工具", "Multimodal Tools")
	plugin.RegisterFactory("multimodal", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

// Plugin 提供 describe_image / transcribe_audio / ocr_image。
type Plugin struct {
	name string
	sdk  *sdk.PluginSDK

	ocrEnabled bool
	imgPrompt  string
	ocrPrompt  string
	audPrompt  string
	timeout    time.Duration
}

// New 构造插件。
func New(name string) *Plugin { return &Plugin{name: name} }

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s

	// ---- 设置 ----
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "ocr_enabled", Default: "true", Type: "bool",
		DisplayName: "启用 OCR 工具",
		Description: "★注意：本工具**不是专用 OCR 引擎**，而是「用视觉模型读图中的文字」。" +
			"高密度小字/表格可能不准；关闭后 ocr_image 不出现在工具表里。",
		Category: "multimodal",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "describe_prompt", Type: "text",
		DisplayName: "图片描述提示词",
		Description: "describe_image 使用的提示词。",
		Category:    "multimodal",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "ocr_prompt", Type: "text",
		DisplayName: "读字提示词",
		Description: "ocr_image 使用的提示词。",
		Category:    "multimodal",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "audio_prompt", Type: "text",
		DisplayName: "音频转写提示词",
		Description: "transcribe_audio 使用的提示词。",
		Category:    "multimodal",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "timeout_seconds", Default: "120", Type: "int",
		DisplayName: "多模态调用超时（秒）",
		Description: "单次多模态请求的上限。",
		Category:    "multimodal",
	})

	p.ocrEnabled = true
	p.timeout = 120 * time.Second
	p.loadSettings()

	if err := s.RegisterTool("describe_image", sdk.ToolDef{
		Name: "describe_image",
		Description: "描述一张图片的内容。可处理本轮用户上传的图片，" +
			"也可用 path 读设备/工具回传的图片文件（如 screensee 的截图）。",
		Parameters:   p.commonParams(),
		ParallelSafe: false, // 会切 LLM 源（provider 参数），非并发安全
	}, p.handleDescribeImage); err != nil {
		return err
	}

	if err := s.RegisterTool("transcribe_audio", sdk.ToolDef{
		Name:         "transcribe_audio",
		Description:  "把一段音频转写为文字。可处理本轮上传的音频，也可用 path 读文件。",
		Parameters:   p.commonParams(),
		ParallelSafe: false,
	}, p.handleTranscribeAudio); err != nil {
		return err
	}

	// ocr_image 的注册**受开关控制**（沿用内核原语义）。
	// 不启用时它不出现在工具表里 —— 这是刻意的：工具在表里却不可用，
	// 比不出现更糟（模型会反复调一个注定失败的工具）。
	if p.ocrEnabled {
		if err := s.RegisterTool("ocr_image", p.ocrToolDef(), p.handleOCRImage); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) Stop() error { return nil }

// commonParams 是三个工具共用的参数形态。
func (p *Plugin) commonParams() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type": "string",
				"description": "可选：媒体文件路径。★处理设备/工具回传的媒体时必须传" +
					"（screensee / camerasue 会回传 file 路径）；不传则处理本轮用户上传的媒体。",
			},
			"digest": map[string]interface{}{
				"type":        "string",
				"description": "可选：媒体在记忆里的**块引用**（块 ID 或其唯一前缀）。与 path 二选一。",
			},
			"provider": map[string]interface{}{
				"type":        "string",
				"description": "可选：用于该次调用的 LLM 源名称，不填则用当前源",
			},
		},
	}
}

func (p *Plugin) ocrToolDef() sdk.ToolDef {
	params := p.commonParams()
	params["properties"].(map[string]interface{})["language"] = map[string]interface{}{
		"type":        "string",
		"description": "可选：OCR 语言（如 chi_sim+eng），默认自动",
	}
	return sdk.ToolDef{
		Name: "ocr_image",
		Description: "读出图片中的文字。★这是**用视觉模型读字**，不是专用 OCR 引擎 —— " +
			"高密度小字/表格可能不准，重要内容建议配合 describe_image 交叉验证。",
		Parameters:   params,
		ParallelSafe: false,
	}
}

func (p *Plugin) loadSettings() {
	s := p.sdk.Settings()
	if s == nil {
		return
	}
	if v, _ := s.Get("ocr_enabled"); v != nil {
		p.ocrEnabled = truthy(v)
	}
	if v, _ := s.Get("timeout_seconds"); v != nil {
		if n := toInt(v); n > 0 {
			p.timeout = time.Duration(n) * time.Second
		}
	}
	if v, _ := s.Get("describe_prompt"); v != nil && strings.TrimSpace(fmt.Sprint(v)) != "" {
		p.imgPrompt = fmt.Sprint(v)
	}
	if v, _ := s.Get("ocr_prompt"); v != nil && strings.TrimSpace(fmt.Sprint(v)) != "" {
		p.ocrPrompt = fmt.Sprint(v)
	}
	if v, _ := s.Get("audio_prompt"); v != nil && strings.TrimSpace(fmt.Sprint(v)) != "" {
		p.audPrompt = fmt.Sprint(v)
	}
}

func truthy(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	case float64:
		return x != 0
	}
	return false
}

func toInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n := 0
		for _, c := range x {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return 0
}

// ---- 三个 handler ----

func (p *Plugin) handleDescribeImage(args map[string]interface{}) (interface{}, error) {
	return p.describe(args, p.imgPromptOrDefault(), "图片描述", 2048, "image_url")
}

func (p *Plugin) handleOCRImage(args map[string]interface{}) (interface{}, error) {
	return p.describe(args, p.ocrPromptOrDefault(), "读字结果", 4096, "image_url")
}

func (p *Plugin) handleTranscribeAudio(args map[string]interface{}) (interface{}, error) {
	return p.describe(args, p.audPromptOrDefault(), "音频转写", 2048, "audio_url")
}

func (p *Plugin) imgPromptOrDefault() string {
	if p.imgPrompt != "" {
		return p.imgPrompt
	}
	return "请详细描述这张图片的内容，包括其中的文字、物体、人物、场景等信息。"
}

func (p *Plugin) ocrPromptOrDefault() string {
	if p.ocrPrompt != "" {
		return p.ocrPrompt
	}
	return "请识别这张图片中的所有文字内容，按原文输出。仅输出文字本身，不要添加额外描述。"
}

func (p *Plugin) audPromptOrDefault() string {
	if p.audPrompt != "" {
		return p.audPrompt
	}
	return "请把这段音频转写为文字。"
}

// describe 解析媒体来源 → 切源（若指定）→ 发一次多模态请求。
//
// ★ 失败一律以 **error** 上抛，不渲染成普通文本。
//
//	这是从内核搬来时保留的纪律（见 internal/plugins/remotedevice 的
//	describeScreen 同类修复）：失败与成功同形会让 agent 反复重试。
func (p *Plugin) describe(args map[string]interface{}, prompt, prefix string,
	maxTokens int, blockType string) (interface{}, error) {

	url, err := p.resolveMedia(args, blockType)
	if err != nil {
		return nil, err
	}

	llm := p.sdk.LLM()
	if llm == nil {
		return nil, fmt.Errorf("LLM 不可用")
	}
	// 指定源：临时切换，用完恢复。切源失败**不降级**（否则「另一个源的结果」
	// 会挂在指定源名下）。
	provider, _ := args["provider"].(string)
	if provider != "" {
		prev := llm.CurrentSource()
		if e := llm.SetSource(provider); e != nil {
			return nil, fmt.Errorf("切换到指定源 %s 失败: %w", provider, e)
		} else if prev != "" {
			defer func() { _ = llm.SetSource(prev) }()
		}
	}
	used := llm.CurrentSource()

	msg := sdk.LLMMessage{
		Role: "user",
		Blocks: []sdk.LLMContentBlock{
			{Type: "text", Text: prompt},
		},
	}
	if blockType == "image_url" {
		msg.Blocks = append(msg.Blocks, sdk.LLMContentBlock{Type: "image_url", ImageURL: url})
	} else {
		msg.Blocks = append(msg.Blocks, sdk.LLMContentBlock{Type: "audio_url", ImageURL: url})
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	resp, err := llm.Chat(ctx, &sdk.LLMCompletionRequest{
		Messages:  []sdk.LLMMessage{msg},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("%s失败（视觉/音频源 %s）: %w", prefix, used, err)
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return nil, fmt.Errorf("%s失败（视觉/音频源 %s）: 模型返回空内容", prefix, used)
	}
	return map[string]interface{}{
		"description": resp.Content,
		"source":      used,
	}, nil
}

// resolveMedia 按 path → 块引用 的顺序解析出 data URL。
func (p *Plugin) resolveMedia(args map[string]interface{}, kind string) (string, error) {
	path, _ := args["path"].(string)
	ref, _ := args["digest"].(string)

	switch {
	case strings.TrimSpace(path) != "":
		b, err := os.ReadFile(strings.TrimSpace(path))
		if err != nil {
			return "", fmt.Errorf("读取文件失败 %s: %w", path, err)
		}
		if len(b) == 0 {
			return "", fmt.Errorf("文件为空: %s", path)
		}
		mime := mimeByExt(path)
		if mime == "" {
			mime = "image/png"
			if kind == "audio_url" {
				mime = "audio/wav"
			}
		}
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b), nil

	case strings.TrimSpace(ref) != "":
		// ★ CAS 淘汰：digest 参数现收**媒体块引用**，由记忆层按块取字节。
		if p.sdk.Memory() == nil {
			return "", fmt.Errorf("记忆系统不可用，无法按块引用取媒体")
		}
		b, mime, ok, err := p.sdk.Memory().MediaBytes(strings.TrimSpace(ref))
		if err != nil {
			return "", fmt.Errorf("按块引用取媒体失败 %s: %w", ref, err)
		}
		if !ok {
			return "", fmt.Errorf("媒体块引用 %s 无对应块或无负载", ref)
		}
		if mime == "" {
			mime = "image/png"
			if kind == "audio_url" {
				mime = "audio/wav"
			}
		}
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b), nil
	}
	// ★ 两条都没有 ⇒ 明确报错，并说清怎么办。
	return "", fmt.Errorf("没有可处理的媒体。请传 path（设备/工具回传的媒体，" +
		"如 screensee 返回的 file 路径）或 digest（记忆里的媒体块引用）；" +
		"本轮用户直接上传的图片/音频请直接用它们，无需本工具")
}

func mimeByExt(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a", ".aac":
		return "audio/aac"
	}
	return ""
}

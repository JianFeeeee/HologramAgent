package aiimage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta("ai_image", "AI 生图", "AI Image")
	plugin.RegisterFactory("ai_image", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name    string
	client  *http.Client
	apiKey  string
	baseURL string
	model   string
	size    string
	dataDir string
}

func New(name string) *Plugin {
	return &Plugin{
		name:   name,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *Plugin) Name() string { return p.name }

func getSettingString(s sdk.SettingsAPI, key, def string) string {
	if s == nil {
		return def
	}
	v, err := s.Get(key)
	if err != nil || v == nil {
		return def
	}
	if sv, ok := v.(string); ok && sv != "" {
		return sv
	}
	return def
}

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)

	// 插件专属数据目录（SDK DataDir API，内核保证存在）
	if dd := s.Settings().DataDir(); dd != "" {
		p.dataDir = dd
	}
	if p.dataDir != "" {
		os.MkdirAll(p.dataDir, 0755)
	}

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "base_url", Default: "http://127.0.0.1:8081/v1", Type: "string",
		DisplayName: "Base URL", Description: "OpenAI 兼容生图网关地址（默认本机 llmsproxy）",
		Category: "ai_image",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "api_key", Default: "", Type: "password",
		DisplayName: "API Key", Description: "生图网关 API Key（默认使用内核 LLM key，留空则取 core.llm.api_key）",
		Category: "ai_image",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "model", Default: "", Type: "string",
		DisplayName: "Model", Description: "生图模型 ID，留空由网关 AUTO 决定（如 flux-1）",
		Category: "ai_image",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "size", Default: "1024x1024", Type: "string",
		DisplayName: "Size", Description: "默认图片尺寸，如 1024x1024",
		Category: "ai_image",
	})

	p.apiKey = getSettingString(s.Settings(), "api_key", "")
	p.baseURL = strings.TrimRight(getSettingString(s.Settings(), "base_url", "http://127.0.0.1:8081/v1"), "/")
	p.model = getSettingString(s.Settings(), "model", "")
	p.size = getSettingString(s.Settings(), "size", "1024x1024")

	if p.apiKey == "" {
		if v, _ := s.Settings().GetCore("llm.api_key"); v != nil {
			if sv, ok := v.(string); ok && sv != "" {
				p.apiKey = sv
			}
		}
	}

	s.RegisterTool("ai_image_generate", sdk.ToolDef{
		Name: "ai_image_generate", Description: "Generate image from text prompt using AI. Returns image URL.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prompt": map[string]interface{}{"type": "string", "description": "Text description of the image to generate"},
				"size":   map[string]interface{}{"type": "string", "description": "Image size (1024x1024, etc.), default from config"},
				"model":  map[string]interface{}{"type": "string", "description": "Model override (e.g. flux-1)"},
				"n":      map[string]interface{}{"type": "integer", "description": "Number of images to generate (1-10), default 1"},
			},
			"required": []string{"prompt"},
		},
		// 外部调用，插件内无共享可变状态
		ParallelSafe: true,
	}, p.handleGenerate)

	return nil
}

func (p *Plugin) Stop() error { return nil }

type genRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
}

type genResp struct {
	Created int64 `json:"created"`
	Data    []struct {
		URL           string `json:"url"`
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (p *Plugin) handleGenerate(args map[string]interface{}) (interface{}, error) {
	prompt, _ := args["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return map[string]interface{}{"isError": true, "content": "prompt is required"}, nil
	}

	key := p.apiKey
	if key == "" {
		return map[string]interface{}{"isError": true, "content": "生图 API key 未配置（plugin.ai_image.api_key 或 core.llm.api_key）"}, nil
	}

	model := p.model
	if m, ok := args["model"].(string); ok && m != "" {
		model = m
	}
	size := p.size
	if sz, ok := args["size"].(string); ok && sz != "" {
		size = sz
	}
	n := 1
	if nv, ok := args["n"].(float64); ok {
		n = int(nv)
		if n < 1 {
			n = 1
		}
		if n > 10 {
			n = 10
		}
	}

	body := genRequest{Model: model, Prompt: prompt, N: n, Size: size, ResponseFormat: "url"}
	raw, _ := json.Marshal(body)

	url := p.baseURL + "/images/generations"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return map[string]interface{}{"isError": true, "content": "构造请求失败: " + err.Error()}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := p.client.Do(req)
	if err != nil {
		return map[string]interface{}{"isError": true, "content": "生图请求失败: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return map[string]interface{}{"isError": true, "content": fmt.Sprintf("生图 API error (status %d): %s", resp.StatusCode, string(respBody))}, nil
	}

	var result genResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return map[string]interface{}{"isError": true, "content": "解析生图响应失败: " + err.Error()}, nil
	}
	if result.Error != nil {
		return map[string]interface{}{"isError": true, "content": "生图 API error: " + result.Error.Message}, nil
	}
	if len(result.Data) == 0 {
		return map[string]interface{}{"isError": true, "content": "no images returned"}, nil
	}

	var urls []string
	for _, d := range result.Data {
		imgURL := d.URL
		if imgURL == "" && d.B64JSON != "" {
			imgURL = "data:image/png;base64," + d.B64JSON
		}
		if imgURL != "" {
			urls = append(urls, imgURL)
		}
	}
	if len(urls) == 0 {
		return map[string]interface{}{"isError": true, "content": "生图响应中没有可用图片"}, nil
	}

	// 下载到插件数据目录，返回本地文件路径（而非临时 S3 URL）：
	// - S3 临时 URL 约 1 小时过期，且对无浏览器 UA 客户端拒绝访问（agent 裸 curl 验证必败）
	// - 本地路径经 webui /files/ 永久下发，支持 output_send type=image
	var localPaths []string
	var dlErrs []string
	for i, u := range urls {
		if strings.HasPrefix(u, "data:") {
			continue // base64 内联图不落盘
		}
		path, err := p.downloadImage(u, fmt.Sprintf("ai_%d_%d", time.Now().UnixNano(), i))
		if err != nil {
			dlErrs = append(dlErrs, fmt.Sprintf("第%d张保存失败: %v", i+1, err))
			continue
		}
		localPaths = append(localPaths, path)
	}

	content := fmt.Sprintf("Generated %d image(s) with model %s", len(urls), model)
	if len(localPaths) > 0 {
		content += "\n本地文件：\n" + strings.Join(localPaths, "\n")
		content += "\n\n图片已保存到本地（不会过期）。如需展示请用 output_send__webui(payload=本地路径, type=image)。"
	}
	if len(dlErrs) > 0 {
		content += "\n\n" + strings.Join(dlErrs, "\n")
	}
	if len(localPaths) < len(urls) {
		content += "\n原始 URL（1小时内有效）：\n" + strings.Join(urls, "\n")
	}

	return map[string]interface{}{
		"content":     content,
		"images":      urls,
		"local_paths": localPaths,
		"prompt":      prompt,
		"model":       model,
	}, nil
}

// downloadImage 把生图返回的临时 URL 下载为本地文件，返回路径。
// 带浏览器 UA 规避图床对无 UA 客户端的拦截。
func (p *Plugin) downloadImage(imgURL, baseName string) (string, error) {
	if p.dataDir == "" {
		return "", fmt.Errorf("data dir unavailable")
	}
	dl := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodGet, imgURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; HomeAgent/1.0)")
	resp, err := dl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		if len(b) > 200 {
			b = b[:200]
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	ext := ".png"
	switch ct := resp.Header.Get("Content-Type"); {
	case strings.Contains(ct, "jpeg"), strings.Contains(ct, "jpg"):
		ext = ".jpg"
	case strings.Contains(ct, "webp"):
		ext = ".webp"
	}
	path := filepath.Join(p.dataDir, baseName+ext)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}

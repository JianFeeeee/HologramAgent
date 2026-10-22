package clawhubadapter

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

var (
	channelInputBuf   = map[string][]map[string]interface{}{}
	channelInputBufMu sync.Mutex
	channelStatus     map[string]map[string]interface{} // 通道运行状态（channel_status 通知）
	channelStatusMu   sync.Mutex
)

type ToolRegistry struct{}

func (r *ToolRegistry) Dispatch(data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	var d struct {
		Name        string                 `json:"name"`
		Label       string                 `json:"label"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
		Plugin      string                 `json:"plugin"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Name == "" {
		return
	}
	pn := pluginName
	if d.Plugin != "" {
		pn = d.Plugin
	}
	toolName := fmt.Sprintf("%s_%s", pn, d.Name)
	tDef := sdk.ToolDef{
		Name:        toolName,
		Description: d.Description,
		Parameters:  d.Parameters,
	}
	handler := func(sp *sidecarProcess, ocToolName string) sdk.ToolHandler {
		return func(args map[string]interface{}) (interface{}, error) {
			return sp.CallTool(ocToolName, args)
		}
	}(sp, d.Name)
	if err := s.RegisterTool(toolName, tDef, handler); err != nil {
		log.Printf("[clawhubadapter] register tool %s: %v", toolName, err)
	}
}

type providerDef struct {
	ocType     string
	toolSuffix string
	desc       string
}

var providerMap = []providerDef{
	{"image_generation", "generate_image", "根据文本描述生成图片，返回图片 URL"},
	{"music_generation", "generate_music", "根据描述生成音乐"},
	{"video_generation", "generate_video", "根据描述生成视频"},
	{"speech", "synthesize_speech", "将文本合成为语音"},
	{"web_search", "web_search", "搜索互联网信息"},
	{"web_fetch", "web_fetch", "获取指定网页的内容"},
	{"media_understanding", "analyze_media", "分析图片、音频或视频内容"},
	{"realtime_transcription", "transcribe_audio", "将音频转写为文字"},
	{"realtime_voice", "voice_io", "实时语音输入输出"},
}

type ProviderRegistry struct{}

func (r *ProviderRegistry) Dispatch(typeStr string, data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	var d struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Plugin      string `json:"plugin"`
	}
	json.Unmarshal(data, &d)

	pn := pluginName
	if d.Plugin != "" {
		pn = d.Plugin
	}

	// Strip _provider suffix: "image_generation_provider" -> "image_generation"
	lookupType := typeStr
	if lookupType != "provider" {
		lookupType = strings.TrimSuffix(lookupType, "_provider")
	}
	if lookupType == "" {
		return
	}

	for _, p := range providerMap {
		if p.ocType == lookupType {
			toolName := fmt.Sprintf("%s_%s", pn, p.toolSuffix)
			desc := p.desc
			if d.Name != "" {
				desc = fmt.Sprintf("[%s] %s", d.Name, desc)
			}
			props := map[string]interface{}{
				"prompt": map[string]interface{}{"type": "string", "description": "Prompt for generation or search query"},
			}
			switch p.ocType {
			case "image_generation":
				props["prompt"] = map[string]interface{}{"type": "string", "description": "Image description prompt"}
				props["size"] = map[string]interface{}{"type": "string", "description": "Image size (e.g. 1024x1024)", "enum": []interface{}{"256x256", "512x512", "1024x1024", "1792x1024", "1024x1792"}}
			case "web_search":
				props["query"] = map[string]interface{}{"type": "string", "description": "Search query"}
				delete(props, "prompt")
			case "web_fetch":
				props["url"] = map[string]interface{}{"type": "string", "description": "URL to fetch"}
				delete(props, "prompt")
			case "media_understanding":
				props["url"] = map[string]interface{}{"type": "string", "description": "Media URL to analyze"}
				props["media_type"] = map[string]interface{}{"type": "string", "description": "Media type", "enum": []interface{}{"image", "audio", "video"}}
			case "speech":
				props["text"] = map[string]interface{}{"type": "string", "description": "Text to synthesize"}
				props["voice"] = map[string]interface{}{"type": "string", "description": "Voice identifier"}
			case "music_generation":
				props["prompt"] = map[string]interface{}{"type": "string", "description": "Music description prompt"}
				props["duration"] = map[string]interface{}{"type": "number", "description": "Duration in seconds"}
			case "video_generation":
				props["prompt"] = map[string]interface{}{"type": "string", "description": "Video description prompt"}
				props["duration"] = map[string]interface{}{"type": "number", "description": "Duration in seconds"}
			case "realtime_transcription":
				props["audio_url"] = map[string]interface{}{"type": "string", "description": "Audio URL to transcribe"}
			case "realtime_voice":
				props["text"] = map[string]interface{}{"type": "string", "description": "Text to speak"}
				props["voice"] = map[string]interface{}{"type": "string", "description": "Voice identifier"}
			}
			tDef := sdk.ToolDef{
				Name:        toolName,
				Description: desc,
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": props,
				},
			}
			handler := func(sp *sidecarProcess, ocType string) sdk.ToolHandler {
				return func(args map[string]interface{}) (interface{}, error) {
					return sp.CallProvider(ocType, args)
				}
			}(sp, lookupType)
			if err := s.RegisterTool(toolName, tDef, handler); err != nil {
				log.Printf("[clawhubadapter] register provider tool %s: %v", toolName, err)
			}
			return
		}
	}

	if lookupType != typeStr {
		log.Printf("[clawhubadapter] provider %s -> lookup %s (plugin: %s)", typeStr, lookupType, pluginName)
	}
	switch lookupType {
	case "provider":
		log.Printf("[clawhubadapter] generic LLM provider from %s handled natively by HologramAgent", pluginName)
	case "embedding", "memory_embedding":
		log.Printf("[clawhubadapter] embedding provider %s from %s ignored (HologramAgent uses native embeddings)", lookupType, pluginName)
	default:
		log.Printf("[clawhubadapter] unknown provider type: %s (plugin: %s)", typeStr, pluginName)
	}
}

type ChannelRegistry struct{}

func (r *ChannelRegistry) Dispatch(data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	var d struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Plugin string `json:"plugin"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Name == "" {
		return
	}
	chName := d.Name
	pn := pluginName
	if d.Plugin != "" {
		pn = d.Plugin
	}

	var caps int
	switch d.Type {
	case "file":
		caps = 2
	case "image":
		caps = 4
	case "audio":
		caps = 8
	default:
		caps = 1
	}
	desc := fmt.Sprintf("OC channel %s (from %s)", chName, pn)
	// 通道名来自外部 OC 配置（动态）：声明处**同时**登记 inputch ——
	// 该通道既收（InjectInputSync 见 plugin.go）又发（output_send__<通道>）。
	_ = s.RegisterInputChannel(chName, sdk.ChannelDef{})
	s.RegisterOutputChannel(chName, caps, desc, sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		return sp.CallTool(chName, args)
	})

	readToolName := fmt.Sprintf("%s_read_%s_input", pn, strings.ReplaceAll(chName, "-", "_"))
	s.RegisterTool(readToolName, sdk.ToolDef{
		Name:        readToolName,
		Description: fmt.Sprintf("读取 %s 通道的待处理输入消息", chName),
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		channelInputBufMu.Lock()
		buf := channelInputBuf[chName]
		if len(buf) == 0 {
			channelInputBufMu.Unlock()
			return map[string]interface{}{"messages": []interface{}{}}, nil
		}
		msgs := make([]interface{}, len(buf))
		for i, m := range buf {
			msgs[i] = m
		}
		channelInputBuf[chName] = nil
		channelInputBufMu.Unlock()
		return map[string]interface{}{"messages": msgs}, nil
	})
}

type StageRegistry struct{}

func (r *StageRegistry) Dispatch(data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	var d struct {
		Name  string `json:"name"`
		Event string `json:"event"`
	}
	json.Unmarshal(data, &d)
	log.Printf("[clawhubadapter] hook %s/%s (plugin: %s) — stub: OC hooks need bidirectional bridge",
		d.Name, d.Event, pluginName)
}

type CapRecorder struct {
	mu   sync.Mutex
	caps []string
}

func (r *CapRecorder) Record(typeStr string, data json.RawMessage, pluginName string) {
	var d struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	json.Unmarshal(data, &d)
	capStr := fmt.Sprintf("[%s] capability: %s", pluginName, typeStr)
	if d.Name != "" {
		capStr += " (" + d.Name + ")"
	}
	if d.Description != "" {
		capStr += ": " + d.Description
	}
	r.mu.Lock()
	r.caps = append(r.caps, capStr)
	r.mu.Unlock()
}

func (r *CapRecorder) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.caps))
	copy(out, r.caps)
	return out
}

type RegistryDispatcher struct {
	toolReg     *ToolRegistry
	providerReg *ProviderRegistry
	channelReg  *ChannelRegistry
	stageReg    *StageRegistry
	capRecorder *CapRecorder
}

func NewDispatcher() *RegistryDispatcher {
	return &RegistryDispatcher{
		toolReg:     &ToolRegistry{},
		providerReg: &ProviderRegistry{},
		channelReg:  &ChannelRegistry{},
		stageReg:    &StageRegistry{},
		capRecorder: &CapRecorder{},
	}
}

func (d *RegistryDispatcher) Dispatch(typeStr string, data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	switch typeStr {
	case "tool":
		d.toolReg.Dispatch(data, pluginName, sp, s)
	case "channel":
		d.channelReg.Dispatch(data, pluginName, sp, s)
	case "provider", "image_generation_provider", "music_generation_provider",
		"video_generation_provider", "speech_provider",
		"realtime_transcription_provider", "realtime_voice_provider",
		"media_understanding_provider",
		"web_fetch_provider", "web_search_provider",
		"embedding_provider", "memory_embedding_provider":
		d.providerReg.Dispatch(typeStr, data, pluginName, sp, s)
		d.capRecorder.Record(typeStr, data, pluginName)
	case "hook", "runtime_lifecycle", "lifecycle",
		"agent_event_subscription", "agent_harness",
		"session_event", "conversation_binding_resolved",
		"interactive_handler",
		"cli", "cli_backend", "node_cli_feature",
		"command", "http_route", "service",
		"gateway_method", "gateway_discovery_service",
		"trusted_tool_policy", "tool_metadata",
		"context_engine",
		"memory_capability", "memory_prompt_section",
		"memory_flush_plan", "memory_runtime",
		"memory_prompt_supplement", "memory_corpus_supplement",
		"session_extension", "session_scheduler_job",
		"session_action", "control_ui_descriptor",
		"agent_tool_result_middleware":
		d.stageReg.Dispatch(data, pluginName, sp, s)
		d.capRecorder.Record(typeStr, data, pluginName)
	default:
		d.capRecorder.Record(typeStr, data, pluginName)
	}
}

func (d *RegistryDispatcher) Capabilities() []string {
	return d.capRecorder.Snapshot()
}

package sdk

import (
	"context"
	"fmt"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	luaVM "github.com/JianFeeeee/HologramAgent/internal/lua"
)

type llmImpl struct {
	mgr        *agentAPI.ProviderManager
	cfgReg     *internalConfig.ConfigRegistry
	lua        *luaVM.VM
	baseAPIKey string
}

func NewLLM(mgr *agentAPI.ProviderManager, cfgReg *internalConfig.ConfigRegistry, lua *luaVM.VM, baseAPIKey string) LLMAPI {
	return &llmImpl{mgr: mgr, cfgReg: cfgReg, lua: lua, baseAPIKey: baseAPIKey}
}

func (l *llmImpl) ListSources() []string {
	if l.mgr == nil {
		return nil
	}
	return l.mgr.List()
}

func (l *llmImpl) SetSource(name string) error {
	if l.mgr == nil {
		return nil
	}
	return l.mgr.SetDefault(name)
}

func (l *llmImpl) CurrentSource() string {
	if l.mgr == nil {
		return ""
	}
	p := l.mgr.Default()
	if p == nil {
		return ""
	}
	return p.Name()
}

func (l *llmImpl) Chat(ctx context.Context, req *LLMCompletionRequest) (*LLMCompletionResponse, error) {
	if l.mgr == nil {
		return nil, fmt.Errorf("llm: provider manager not available")
	}
	p := l.mgr.Default()
	if p == nil {
		return nil, fmt.Errorf("llm: no default provider")
	}
	apiReq := &agentAPI.CompletionRequest{
		Model:           req.Model,
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		Stream:          req.Stream,
		Tools:           req.Tools,
		ToolChoice:      req.ToolChoice,
		DisableThinking: req.DisableThinking,
	}
	if len(req.Messages) > 0 {
		apiReq.Messages = make([]agentAPI.Message, len(req.Messages))
		for i, m := range req.Messages {
			msg := agentAPI.Message{
				Role:             m.Role,
				Content:          m.Content,
				ReasoningContent: m.ReasoningContent,
				ToolCallID:       m.ToolCallID,
			}
			// 多模态 Blocks：text/image_url → agentAPI.ContentBlock
			for _, b := range m.Blocks {
				switch b.Type {
				case "text":
					msg.Blocks = append(msg.Blocks, agentAPI.ContentBlock{Type: "text", Text: b.Text})
				case "image_url":
					msg.Blocks = append(msg.Blocks, agentAPI.ContentBlock{
						Type:     "image_url",
						ImageURL: &agentAPI.ImageURL{URL: b.ImageURL, Detail: "high"},
					})
				}
			}
			if len(m.ToolCalls) > 0 {
				msg.ToolCalls = make([]agentAPI.ToolCall, len(m.ToolCalls))
				for j, tc := range m.ToolCalls {
					msg.ToolCalls[j] = agentAPI.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
				}
			}
			apiReq.Messages[i] = msg
		}
	}
	resp, err := p.Chat(ctx, apiReq)
	if err != nil {
		return nil, err
	}
	out := &LLMCompletionResponse{
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		FinishReason:     resp.FinishReason,
		TokenUsage: LLMTokenUsage{
			Prompt:     resp.TokenUsage.Prompt,
			Completion: resp.TokenUsage.Completion,
			Total:      resp.TokenUsage.Total,
		},
	}
	for _, tc := range resp.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, LLMToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	return out, nil
}

func (l *llmImpl) ReloadFromConfig() error {
	if l.mgr == nil || l.cfgReg == nil || l.lua == nil {
		return nil
	}
	cfg := l.cfgReg.ToConfig()
	if cfg == nil {
		return nil
	}
	l.mgr.Reset()
	adapterConcurrency := map[string]int{}
	for _, src := range cfg.LLM.Sources {
		if !agentAPI.IsValidSourceConfig(src.Name, src.BaseURL, src.Model, src.Adapter) {
			continue
		}
		key := src.APIKey
		if key == "" {
			key = l.baseAPIKey
		}
		provider := agentAPI.NewLuaAdaptedProvider(agentAPI.BaseConfig{
			Model:         src.Model,
			BaseURL:       src.BaseURL,
			APIKey:        key,
			Temperature:   cfg.LLM.Temperature,
			MaxTokens:     cfg.LLM.MaxTokens,
			ContextWindow: src.ContextWindow,
			MaxConcurrent: src.MaxConcurrent,
			Priority:      src.Priority,
			Vision:        src.Vision,
			Audio:         src.Audio,
		}, l.lua, src.Name, src.Adapter)
		l.mgr.Register(src.Name, provider)
		if src.Adapter != "" {
			adapterConcurrency[src.Adapter] += src.MaxConcurrent
		}
	}
	if l.lua != nil {
		l.lua.ConfigureConcurrency(adapterConcurrency)
	}
	if cfg.LLM.Provider != "" {
		if l.mgr.Get(cfg.LLM.Provider) != nil {
			_ = l.mgr.SetDefault(cfg.LLM.Provider)
		}
	}
	return nil
}

var _ LLMAPI = (*llmImpl)(nil)

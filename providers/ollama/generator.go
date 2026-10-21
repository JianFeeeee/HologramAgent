// Package ollamagen implements the generation.Provider SPI over a local Ollama
// HTTP endpoint.
//
// It lives outside the HomeAgent core: the core depends only on pkg/generation
// and selects this provider by name through configuration. Prompt rendering,
// JSON-schema constraint, sampling, and the Ollama wire format are all owned
// here.
//
// Why Ollama rather than an in-process ONNX runtime for generation: the
// distillation path runs on a 30-minute timer, not the request hot path, so a
// local HTTP round-trip is acceptable; and the structured-output constraint
// (format=<schema>) that makes a 1.7B model usable for triple extraction is a
// first-class Ollama feature. Measured on this host: qwen3:1.7b with a JSON
// schema produced 30/30 field pairs with 0 hallucinations across 6 real
// records of varied structure, versus free-form prompting which either emitted
// chat filler (0 parsed pairs) or replayed few-shot example answers (100%
// hallucination on structurally-dissimilar records).
package ollamagen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/pkg/generation"
)

const (
	defaultEndpoint  = "http://localhost:11434/api/generate"
	defaultModel     = "qwen3:1.7b"
	defaultKeepAlive = "30m"
	defaultThreads   = 10
	defaultTimeout   = 240 * time.Second
)

func init() {
	generation.Register("ollama", func(cfg generation.Config) (generation.Provider, error) {
		return New(cfg)
	})
}

// Provider talks to a local Ollama server over HTTP.
type Provider struct {
	endpoint  string
	model     string
	keepAlive string
	threads   int
	numCtx    int
	timeout   time.Duration
	client    *http.Client
}

// New builds a provider from options. Recognized options:
//
//	endpoint    Ollama generate URL (default http://localhost:11434/api/generate)
//	model       model tag (default qwen3:1.7b)
//	keep_alive  model residency hint (default 30m) — avoids per-call reload
//	num_thread  CPU threads (default 10) — critical: the Ollama default badly
//	            under-threads on this 12-core host (0.1 tok/s vs 2-3 tok/s)
//	num_ctx     context window in tokens (default 1024)
//	timeout_sec per-request timeout seconds (default 240)
func New(cfg generation.Config) (*Provider, error) {
	p := &Provider{
		endpoint:  optString(cfg, "endpoint", defaultEndpoint),
		model:     optString(cfg, "model", defaultModel),
		keepAlive: optString(cfg, "keep_alive", defaultKeepAlive),
		threads:   optInt(cfg, "num_thread", defaultThreads),
		numCtx:    optInt(cfg, "num_ctx", 1024),
		timeout:   time.Duration(optInt(cfg, "timeout_sec", int(defaultTimeout/time.Second))) * time.Second,
	}
	if p.model == "" {
		return nil, fmt.Errorf("ollamagen: model is empty")
	}
	p.client = &http.Client{Timeout: p.timeout}
	return p, nil
}

type genRequest struct {
	Model     string          `json:"model"`
	Prompt    string          `json:"prompt"`
	Stream    bool            `json:"stream"`
	Think     bool            `json:"think"`
	KeepAlive string          `json:"keep_alive,omitempty"`
	Format    json.RawMessage `json:"format,omitempty"`
	Options   genOptions      `json:"options"`
}

type genOptions struct {
	Temperature float64  `json:"temperature"`
	NumPredict  int      `json:"num_predict"`
	NumCtx      int      `json:"num_ctx"`
	NumThread   int      `json:"num_thread"`
	Stop        []string `json:"stop,omitempty"`
}

type genResponse struct {
	Response   string `json:"response"`
	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason"`
	Error      string `json:"error"`
}

// Generate runs one non-streaming completion.
//
// When req.JSONSchema is set it is passed through as Ollama's `format` field,
// which constrains decoding to that schema. This is the mechanism that makes a
// small model reliable for structured extraction; without it the model emits
// prose. Unlike a provider that cannot honor a schema, this one does — so it
// never returns ErrSchemaUnsupported.
func (p *Provider) Generate(ctx context.Context, req generation.Request) (generation.Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 256
	}
	body := genRequest{
		Model:     p.model,
		Prompt:    req.Prompt,
		Stream:    false,
		Think:     false,
		KeepAlive: p.keepAlive,
		Options: genOptions{
			Temperature: req.Temperature,
			NumPredict:  maxTokens,
			NumCtx:      p.numCtx,
			NumThread:   p.threads,
			Stop:        req.Stop,
		},
	}
	if s := strings.TrimSpace(req.JSONSchema); s != "" {
		// Ollama accepts either the literal string "json" or a schema object.
		// A caller-supplied schema is passed verbatim; it must be valid JSON.
		if !json.Valid([]byte(s)) {
			return generation.Response{}, fmt.Errorf("ollamagen: JSONSchema is not valid JSON")
		}
		body.Format = json.RawMessage(s)
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return generation.Response{}, fmt.Errorf("ollamagen: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(payload))
	if err != nil {
		return generation.Response{}, fmt.Errorf("ollamagen: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return generation.Response{}, fmt.Errorf("ollamagen: request %s: %w", p.model, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return generation.Response{}, fmt.Errorf("ollamagen: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return generation.Response{}, fmt.Errorf("ollamagen: http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var out genResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return generation.Response{}, fmt.Errorf("ollamagen: decode response: %w", err)
	}
	if out.Error != "" {
		return generation.Response{}, fmt.Errorf("ollamagen: model error: %s", out.Error)
	}
	return generation.Response{
		Text:      out.Response,
		Truncated: out.DoneReason == "length",
	}, nil
}

// Info reports the model id and that schema-constrained output is supported.
func (p *Provider) Info() generation.Info {
	return generation.Info{Model: p.model, SupportsJSONSchema: true}
}

// Close is a no-op: the HTTP client holds no persistent model state. Residency
// is governed by the server's keep_alive, not this process.
func (p *Provider) Close() {}

func optString(cfg generation.Config, key, def string) string {
	if v, ok := cfg.Options[key]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func optInt(cfg generation.Config, key string, def int) int {
	if v, ok := cfg.Options[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

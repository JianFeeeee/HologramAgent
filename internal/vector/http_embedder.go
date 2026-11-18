package vector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

func init() {
	// http 也是一个普通 provider：核心只按名字打开它，不知道它背后是云 API、
	// 自建服务还是别的语言写的模型。
	embedding.Register("http", func(cfg embedding.Config) (embedding.Provider, error) {
		return NewHTTPEmbedder(HTTPEmbedderConfig{
			Endpoint:    cfg.Options["endpoint"],
			APIKey:      cfg.Options["api_key"],
			Model:       cfg.Options["model"],
			Fingerprint: cfg.Options["fingerprint"],
			Dimension:   atoiOrZero(cfg.Options["dimension"]),
			Timeout:     durationOrZero(cfg.Options["timeout"]),
		})
	})
}

func atoiOrZero(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func durationOrZero(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return d
}

// HTTPEmbedderConfig 配置一个外部多模态向量服务。
// 服务契约刻意很小：POST Endpoint，输入 modality/data/mime/side，返回 embedding。
// 任何云 API 或自建服务只需适配这一个协议，即可复用内核全部向量存储与检索链路。
type HTTPEmbedderConfig struct {
	Endpoint    string
	APIKey      string
	Model       string
	Dimension   int
	Timeout     time.Duration
	Fingerprint string
}

// HTTPEmbedder 是 MultimodalEmbedder 的外部 API 实现。
type HTTPEmbedder struct {
	cfg    HTTPEmbedderConfig
	client *http.Client
	mu     sync.Mutex
	closed bool
}

type httpEmbedRequest struct {
	Model    string `json:"model,omitempty"`
	Modality string `json:"modality"`
	Side     string `json:"side"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIME     string `json:"mime,omitempty"`
}

type httpEmbedResponse struct {
	Embedding []float64 `json:"embedding"`
	Data      []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data,omitempty"`
}

func NewHTTPEmbedder(cfg HTTPEmbedderConfig) (*HTTPEmbedder, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("vector: empty HTTP embedding endpoint")
	}
	if cfg.Dimension <= 0 {
		return nil, fmt.Errorf("vector: invalid HTTP embedding dimension %d", cfg.Dimension)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Fingerprint == "" {
		cfg.Fingerprint = "http:" + cfg.Model + fmt.Sprintf(":%d", cfg.Dimension)
	}
	return &HTTPEmbedder{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}, nil
}

func (e *HTTPEmbedder) VectorizeDense(text string) ([]float64, error) {
	return e.embed(context.Background(), httpEmbedRequest{Model: e.cfg.Model, Modality: string(ModalityText), Side: "query", Text: text})
}

func (e *HTTPEmbedder) EmbedImageDense(img []byte, mime string) ([]float64, error) {
	return e.embed(context.Background(), httpEmbedRequest{Model: e.cfg.Model, Modality: string(ModalityImage), Side: "document", Data: base64.StdEncoding.EncodeToString(img), MIME: mime})
}

func (e *HTTPEmbedder) embed(ctx context.Context, payload httpEmbedRequest) ([]float64, error) {
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("vector: HTTP embedder closed")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vector: HTTP embedding request: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vector: HTTP embedding status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out httpEmbedResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("vector: decode HTTP embedding: %w", err)
	}
	v := out.Embedding
	if len(v) == 0 && len(out.Data) > 0 {
		v = out.Data[0].Embedding
	}
	if len(v) != e.cfg.Dimension {
		return nil, fmt.Errorf("vector: HTTP embedding dimension %d, want %d", len(v), e.cfg.Dimension)
	}
	return v, nil
}

func (e *HTTPEmbedder) Fingerprint() string { return e.cfg.Fingerprint }
func (e *HTTPEmbedder) Dim() int            { return e.cfg.Dimension }

// Embed 实现公共 provider 契约：核心只传模态与不透明字节，本实现负责把它
// 翻译成外部服务的协议。
func (e *HTTPEmbedder) Embed(ctx context.Context, in embedding.Input) ([]float64, error) {
	req := httpEmbedRequest{
		Model:    e.cfg.Model,
		Modality: string(in.Modality),
		Side:     string(in.Purpose),
		Text:     in.Text,
		MIME:     in.MIME,
	}
	if in.Modality != embedding.ModalityText {
		req.Data = base64.StdEncoding.EncodeToString(in.Data)
	}
	return e.embed(ctx, req)
}

// Info 声明本 provider 的向量空间身份。外部服务的支持模态无法在本地探测，
// 因此只声明 text/image 这两条内核真正会走到的路径。
func (e *HTTPEmbedder) Info() embedding.Info {
	return embedding.Info{
		Dimension:   e.cfg.Dimension,
		Fingerprint: e.cfg.Fingerprint,
		Modalities:  []embedding.Modality{embedding.ModalityText, embedding.ModalityImage},
	}
}
func (e *HTTPEmbedder) Loaded() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed
}
func (e *HTTPEmbedder) Close() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
}

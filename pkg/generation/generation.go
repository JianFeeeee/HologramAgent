// Package generation defines the public SPI for text-generation providers used
// by background pipelines (notably triple distillation).
//
// The HologramAgent core depends only on this package. Model runtimes, tokenizers,
// prompt templates, sampling, and model-specific configuration belong in
// provider packages registered with Register.
//
// This mirrors pkg/embedding deliberately: a model may implement both SPIs from
// one weight set (an embedding front half + a generation head), registering
// under each. The two registries stay independent so a build can select an
// embedding provider and a generation provider separately — or the same model
// for both.
package generation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Request is the model-neutral generation request.
//
// Prompt is the fully-rendered input text; the core does not apply chat
// templates. A provider whose model needs a chat template applies it from
// Prompt internally. JSONSchema, when non-empty, asks the provider to constrain
// output to that JSON Schema (providers that cannot MUST return
// ErrSchemaUnsupported rather than silently ignoring it — a silently ignored
// schema produces free-form text that the caller parses as JSON and fails on).
type Request struct {
	Prompt      string
	MaxTokens   int
	Temperature float64
	Stop        []string
	// JSONSchema is an optional JSON Schema (as a JSON string). Empty means
	// unconstrained text.
	JSONSchema string
}

// Response is the model-neutral generation result.
type Response struct {
	Text string
	// Truncated reports that generation stopped at MaxTokens rather than a
	// natural stop. Callers parsing structured output should treat a truncated
	// response as suspect.
	Truncated bool
}

// Info describes one generation provider's identity.
type Info struct {
	// Model is a human-readable identifier for diagnostics/logging.
	Model string
	// SupportsJSONSchema reports whether Generate honors Request.JSONSchema.
	SupportsJSONSchema bool
}

// Provider is the public Go extension point for text generation.
//
// Implementations must be safe for concurrent Generate calls unless their
// factory documents otherwise and serializes internally.
type Provider interface {
	Generate(context.Context, Request) (Response, error)
	Info() Info
	Close()
}

// Config contains provider-owned options. The core does not interpret option
// names or values; it passes core.memory.distill.generation.options.* through
// after stripping the prefix.
type Config struct {
	Options map[string]string
}

// Factory constructs a provider instance.
type Factory func(Config) (Provider, error)

var (
	// ErrSchemaUnsupported means the provider cannot constrain output to a JSON
	// Schema. Callers must not assume the returned text is valid JSON.
	ErrSchemaUnsupported = errors.New("generation: json schema not supported")

	registryMu sync.RWMutex
	registry   = make(map[string]Factory)
)

// Register makes a provider factory available under name. It is normally called
// from a provider package's init function. Duplicate names panic so a build
// cannot silently select whichever package initialized last.
func Register(name string, factory Factory) {
	name = strings.TrimSpace(name)
	if name == "" {
		panic("generation: register empty provider name")
	}
	if factory == nil {
		panic("generation: register nil factory for " + name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("generation: provider already registered: " + name)
	}
	registry[name] = factory
}

// Open constructs a registered provider.
func Open(name string, cfg Config) (Provider, error) {
	name = strings.TrimSpace(name)
	registryMu.RLock()
	factory := registry[name]
	registryMu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("generation: unknown provider %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	provider, err := factory(cloneConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("generation: open provider %q: %w", name, err)
	}
	if provider == nil {
		return nil, fmt.Errorf("generation: provider %q returned nil", name)
	}
	return provider, nil
}

// Names returns registered provider names in deterministic order.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cloneConfig(cfg Config) Config {
	out := Config{Options: make(map[string]string, len(cfg.Options))}
	for key, value := range cfg.Options {
		out.Options[key] = value
	}
	return out
}

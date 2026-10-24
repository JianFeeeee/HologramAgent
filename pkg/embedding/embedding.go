// Package embedding defines the public SPI for dense multimodal embedding providers.
//
// The HologramAgent core depends only on this package. Model runtimes, tokenizers,
// preprocessing, media decoding, and model-specific configuration belong in
// provider packages registered with Register.
package embedding

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// Modality identifies the semantic kind of an embedding input. Providers may
// support additional modality strings; the constants are the common values.
type Modality string

const (
	ModalityText  Modality = "text"
	ModalityImage Modality = "image"
	ModalityAudio Modality = "audio"
	ModalityVideo Modality = "video"
)

// Purpose tells a provider how the vector will be used. Providers whose model
// distinguishes query and document prompts can map this value accordingly.
type Purpose string

const (
	PurposeQuery    Purpose = "query"
	PurposeDocument Purpose = "document"
)

// Input is the model-neutral request passed to a provider.
//
// Data is deliberately opaque to the core. MIME describes the encoding; the
// selected provider owns decoding, frame sampling, preprocessing, and all
// other model-specific interpretation. Text is used for textual inputs.
type Input struct {
	Modality Modality
	Purpose  Purpose
	Text     string
	Data     []byte
	MIME     string
	Metadata map[string]string
}

// Info describes one vector space. Fingerprint must change whenever vectors
// cease to be comparable with vectors produced by a previous provider build.
type Info struct {
	Dimension   int
	Fingerprint string
	Modalities  []Modality
}

// Provider is the public Go extension point for a dense multimodal vector
// space. Implementations must be safe for concurrent Embed calls unless their
// factory documents otherwise and serializes internally.
type Provider interface {
	Embed(context.Context, Input) ([]float64, error)
	Info() Info
	Close()
}

// Config contains provider-owned options. The core does not interpret option
// names or values; it only passes core.memory.multimodal_space.options.*
// through after stripping the prefix.
type Config struct {
	Options map[string]string
}

// Factory constructs a provider instance.
type Factory func(Config) (Provider, error)

var (
	// ErrUnsupportedModality means this vector space has no native encoder for
	// the requested modality. Callers must not substitute another model's vector.
	ErrUnsupportedModality = errors.New("embedding: unsupported modality")

	registryMu sync.RWMutex
	registry   = make(map[string]Factory)
)

// Register makes a provider factory available under name. It is normally
// called from a provider package's init function. Duplicate names panic so a
// build cannot silently select whichever package initialized last.
func Register(name string, factory Factory) {
	name = strings.TrimSpace(name)
	if name == "" {
		panic("embedding: register empty provider name")
	}
	if factory == nil {
		panic("embedding: register nil factory for " + name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("embedding: provider already registered: " + name)
	}
	registry[name] = factory
}

// Open constructs a registered provider and validates its vector-space identity.
func Open(name string, cfg Config) (Provider, error) {
	name = strings.TrimSpace(name)
	registryMu.RLock()
	factory := registry[name]
	registryMu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("embedding: unknown provider %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	provider, err := factory(cloneConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("embedding: open provider %q: %w", name, err)
	}
	if provider == nil {
		return nil, fmt.Errorf("embedding: provider %q returned nil", name)
	}
	if err := ValidateInfo(provider.Info()); err != nil {
		provider.Close()
		return nil, fmt.Errorf("embedding: provider %q: %w", name, err)
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

// ValidateInfo checks the stable identity required by vector persistence.
func ValidateInfo(info Info) error {
	if info.Dimension <= 0 {
		return fmt.Errorf("invalid dimension %d", info.Dimension)
	}
	if strings.TrimSpace(info.Fingerprint) == "" {
		return errors.New("empty fingerprint")
	}
	return nil
}

// ValidateVector rejects malformed provider output before it reaches storage.
func ValidateVector(vec []float64, dimension int) error {
	if len(vec) != dimension {
		return fmt.Errorf("embedding: vector dimension %d, want %d", len(vec), dimension)
	}
	for i, value := range vec {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("embedding: vector value %d is not finite", i)
		}
	}
	return nil
}

func cloneConfig(cfg Config) Config {
	out := Config{Options: make(map[string]string, len(cfg.Options))}
	for key, value := range cfg.Options {
		out.Options[key] = value
	}
	return out
}

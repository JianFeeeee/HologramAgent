package test

// pkg/generation 的黑盒判据：Register → Names/Open → 输出。

import (
	"context"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/pkg/generation"
)

type stubProvider struct{}

func (p *stubProvider) Generate(ctx context.Context, r generation.Request) (generation.Response, error) {
	return generation.Response{Text: "generated"}, nil
}
func (p *stubProvider) Info() generation.Info {
	return generation.Info{Model: "stub-model"}
}
func (p *stubProvider) Close() {}

func TestRegisterThenOpen(t *testing.T) {
	const name = "contract-gen-provider"
	generation.Register(name, func(cfg generation.Config) (generation.Provider, error) {
		return &stubProvider{}, nil
	})
	found := false
	for _, n := range generation.Names() {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatal("★ Names() 里没有刚注册的 provider")
	}
	p, err := generation.Open(name, generation.Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	if got := p.Info().Model; got != "stub-model" {
		t.Fatalf("★ Info.Model = %q", got)
	}
	out, err := p.Generate(context.Background(), generation.Request{})
	if err != nil || out.Text != "generated" {
		t.Fatalf("★ Generate = %+v / %v", out, err)
	}
}

func TestOpenUnknownProviderReportsError(t *testing.T) {
	_, err := generation.Open("definitely-not-registered-xyz", generation.Config{})
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("★ 未注册 provider 的报错不可诊断：%v", err)
	}
}

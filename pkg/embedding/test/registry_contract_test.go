package test

// pkg/embedding 的黑盒判据：Register → Names/Open → 输出；Validate* 的输入→输出。
// 不碰 registry map。

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

type stubProvider struct{ dim int }

func (p *stubProvider) Embed(ctx context.Context, in embedding.Input) ([]float64, error) {
	return make([]float64, p.dim), nil
}
func (p *stubProvider) Info() embedding.Info {
	return embedding.Info{Dimension: p.dim, Fingerprint: "stub-fp"}
}
func (p *stubProvider) Close() {}

// TestRegisterThenOpen：注册后 Names/Open 能取回该 provider，且 Info 正确。
func TestRegisterThenOpen(t *testing.T) {
	const name = "contract-embed-provider"
	embedding.Register(name, func(cfg embedding.Config) (embedding.Provider, error) {
		return &stubProvider{dim: 8}, nil
	})
	found := false
	for _, n := range embedding.Names() {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatal("★ Names() 里没有刚注册的 provider")
	}
	p, err := embedding.Open(name, embedding.Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	if got := p.Info().Dimension; got != 8 {
		t.Fatalf("★ Info.Dimension = %d，期望 8", got)
	}
	vec, err := p.Embed(context.Background(), embedding.Input{Text: "x"})
	if err != nil || len(vec) != 8 {
		t.Fatalf("★ Embed = %v / %v", vec, err)
	}
}

// TestOpenUnknownProviderReportsError：未注册名字报错且列出可用项。
func TestOpenUnknownProviderReportsError(t *testing.T) {
	_, err := embedding.Open("definitely-not-registered-xyz", embedding.Config{})
	if err == nil {
		t.Fatal("★ 未注册 provider 却 Open 成功")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("★ 报错信息不可诊断：%v", err)
	}
}

// TestValidateInfo：维度/指纹的输入→输出。
func TestValidateInfo(t *testing.T) {
	cases := []struct {
		info    embedding.Info
		wantErr bool
	}{
		{embedding.Info{Dimension: 3, Fingerprint: "fp"}, false},
		{embedding.Info{Dimension: 0, Fingerprint: "fp"}, true},
		{embedding.Info{Dimension: -1, Fingerprint: "fp"}, true},
		{embedding.Info{Dimension: 3, Fingerprint: "  "}, true},
	}
	for _, c := range cases {
		err := embedding.ValidateInfo(c.info)
		if (err != nil) != c.wantErr {
			t.Errorf("★ ValidateInfo(%+v) err=%v，wantErr=%v", c.info, err, c.wantErr)
		}
	}
}

// TestValidateVector：维度不符 / NaN / Inf 必须报错。
func TestValidateVector(t *testing.T) {
	if err := embedding.ValidateVector([]float64{1, 2, 3}, 3); err != nil {
		t.Fatalf("★ 合法向量被拒：%v", err)
	}
	for _, bad := range [][]float64{{1, 2}, {math.NaN(), 2, 3}, {math.Inf(1), 2, 3}} {
		if err := embedding.ValidateVector(bad, 3); err == nil {
			t.Fatalf("★ 非法向量被接受：%v", bad)
		}
	}
}

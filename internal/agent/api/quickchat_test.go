package api

import (
	"context"
	"os"
	"testing"
	"time"

	luaVM "github.com/JianFeeeee/HologramAgent/internal/lua"
)

func TestQuickChatWithRealKey(t *testing.T) {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY not set — skipping real LLM test")
	}

	tmpDir := t.TempDir()

	vm := luaVM.NewVM(tmpDir + "/adapters")
	if err := vm.Start(); err != nil {
		t.Fatal(err)
	}
	defer vm.Stop()

	pm := NewProviderManager()
	pm.Register("deepseek", NewLuaAdaptedProvider(BaseConfig{
		Model:   "deepseek-v4-flash",
		BaseURL: "https://api.deepseek.com",
		APIKey:  apiKey,
	}, vm, "deepseek", "deepseek"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := pm.QuickChat(ctx, "请回复'OK'，不要输出其他内容")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("QuickChat failed: %v", err)
	}

	if resp.Content == "" {
		t.Fatal("empty response")
	}

	t.Logf("Response: %q", resp.Content)
	t.Logf("Time: %v", elapsed.Round(time.Millisecond))
	if resp.TokenUsage.Total > 0 {
		t.Logf("Tokens: %d (prompt %d + completion %d)",
			resp.TokenUsage.Total, resp.TokenUsage.Prompt, resp.TokenUsage.Completion)
	}
}

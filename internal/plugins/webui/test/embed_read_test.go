package test

import (
	"github.com/JianFeeeee/HologramAgent/internal/plugins/webui"
	"testing"
)

func mustReadEmbedded(t *testing.T, name string) []byte {
	t.Helper()
	b, err := webui.FrontendAsset(name)
	if err != nil {
		t.Fatalf("读 embed %s: %v", name, err)
	}
	return b
}

package webui

import "testing"

func mustReadEmbedded(t *testing.T, name string) []byte {
	t.Helper()
	b, err := dashboardFS.ReadFile(name)
	if err != nil {
		t.Fatalf("读 embed %s: %v", name, err)
	}
	return b
}

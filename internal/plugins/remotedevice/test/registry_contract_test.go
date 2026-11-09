package test

// remotedevice Registry 黑盒判据：通过公开方法观察结果保存、能力查询与媒体落盘。

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugins/remotedevice"
)

func TestSaveResultThenGetResult(t *testing.T) {
	r := remotedevice.NewRegistry()
	want := map[string]interface{}{"status": "ok", "output": "done"}
	r.SaveResult("req-123", want)
	got, ok := r.GetResult("req-123")
	if !ok || got["status"] != "ok" || got["output"] != "done" {
		t.Fatalf("GetResult=%v,%v", got, ok)
	}
	if _, ok := r.GetResult("missing"); ok {
		t.Fatal("unknown request unexpectedly found")
	}
}

func TestSupportsToolForUnknownAndUnknownCapability(t *testing.T) {
	r := remotedevice.NewRegistry()
	if r.SupportsTool("missing-device", "screensee") {
		t.Fatal("unknown device supports tool")
	}
}

func TestSaveInlineMediaRequiresDirectoryAndPersistsBytes(t *testing.T) {
	r := remotedevice.NewRegistry()
	payload := base64.StdEncoding.EncodeToString([]byte("media bytes"))
	if _, err := r.SaveInlineMedia("capture", payload, "application/octet-stream"); err == nil {
		t.Fatal("media saved without configured directory")
	}
	dir := t.TempDir()
	r.SetMediaDir(dir)
	meta, err := r.SaveInlineMedia("capture", payload, "application/octet-stream")
	if err != nil {
		t.Fatalf("SaveInlineMedia: %v", err)
	}
	path, ok := meta["file"].(string)
	if !ok || filepath.Dir(path) != dir {
		t.Fatalf("file metadata=%v", meta)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("media file missing: %v", err)
	}
	if string(got) != "media bytes" || meta["size"] != len(got) || meta["mime"] != "application/octet-stream" {
		t.Fatalf("saved payload/metadata mismatch: %q / %v", got, meta)
	}
	if _, ok := meta["sha256"].(string); !ok {
		t.Fatalf("missing digest: %v", meta)
	}
}

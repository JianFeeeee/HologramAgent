package test

// internal/meta 的黑盒判据：FullVersion 的输出 + 版本常量的对外契约。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/meta"
)

func TestFullVersionContainsKernelAndVersion(t *testing.T) {
	got := meta.FullVersion()
	for _, want := range []string{meta.KernelName, meta.Version, meta.Commit} {
		if !strings.Contains(got, want) {
			t.Fatalf("★ FullVersion()=%q 不含 %q", got, want)
		}
	}
}

func TestSourceURLLooksLikeHTTPS(t *testing.T) {
	if !strings.HasPrefix(meta.SourceURL, "https://") {
		t.Fatalf("★ SourceURL 不是 https：%q", meta.SourceURL)
	}
}

func TestLicenseMetadataNotEmpty(t *testing.T) {
	if meta.License == "" || !strings.HasPrefix(meta.LicenseURL, "https://") {
		t.Fatalf("★ 许可元信息缺失：license=%q url=%q", meta.License, meta.LicenseURL)
	}
}

func TestSDKCompatibleVersionIsSemverLike(t *testing.T) {
	parts := strings.Split(meta.SDKCompatibleVersion, ".")
	if len(parts) != 3 {
		t.Fatalf("★ SDKCompatibleVersion=%q 不是三段版本号", meta.SDKCompatibleVersion)
	}
}

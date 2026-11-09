package test

// internal/sdk 的模块级黑盒判据：公开 SDK 包装 API 的输入→输出。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

func TestEffectiveProxyAuthNormalizesLegacyAndEmpty(t *testing.T) {
	for _, in := range []string{"", sdk.ProxyAuthAgentLegacy} {
		if got := sdk.EffectiveProxyAuth(in); got != sdk.ProxyAuthAgent {
			t.Errorf("EffectiveProxyAuth(%q)=%q", in, got)
		}
	}
	if got := sdk.EffectiveProxyAuth(sdk.ProxyAuthNone); got != sdk.ProxyAuthNone {
		t.Fatalf("explicit no-auth changed: %q", got)
	}
}

func TestProxyHostNormalizationAndValidation(t *testing.T) {
	label := sdk.NormalizeProxyHost("My Plugin")
	if label == "" || !sdk.ValidProxyHostLabel(label) {
		t.Fatalf("normalized host %q is not a valid DNS label", label)
	}
	if sdk.ValidProxyHostLabel("bad..label") || sdk.ValidProxyHostLabel("-leading") {
		t.Fatal("invalid DNS host label accepted")
	}
}

func TestNewConfigGetPutRoundTrip(t *testing.T) {
	first := &types.Config{LLM: types.LLMConfig{Provider: "first"}}
	api := sdk.NewConfig(first)
	if got := api.Get(); got != first {
		t.Fatal("Get did not return configured value")
	}
	second := &types.Config{LLM: types.LLMConfig{Provider: "second"}}
	api.Put(second)
	if got := api.Get(); got != second {
		t.Fatal("Put value not observable via Get")
	}
}

func TestBuiltinProxyRegistrySnapshotsAndClear(t *testing.T) {
	const name = "contract-sdk-proxy"
	sdk.ClearBuiltinProxyDefs(name)
	t.Cleanup(func() { sdk.ClearBuiltinProxyDefs(name) })
	before := sdk.BuiltinProxyVersion()
	sdk.RegisterBuiltinProxy(name, "web", sdk.ProxyDef{Target: "http://127.0.0.1:1234"})
	defs := sdk.BuiltinProxyDefs()
	if len(defs[name]) != 1 || defs[name][0].Name != "web" {
		t.Fatalf("registered proxy not observable: %+v", defs[name])
	}
	if sdk.BuiltinProxyVersion() <= before {
		t.Fatal("registration did not advance public version")
	}
	// Returned slice is a snapshot; mutating it must not mutate the registry.
	defs[name][0].Target = "mutated"
	if got := sdk.BuiltinProxyDefs()[name][0].Target; got != "http://127.0.0.1:1234" {
		t.Fatalf("snapshot mutation leaked into registry: %q", got)
	}
	sdk.ClearBuiltinProxyDefs(name)
	if _, ok := sdk.BuiltinProxyDefs()[name]; ok {
		t.Fatal("ClearBuiltinProxyDefs did not remove entry")
	}
}

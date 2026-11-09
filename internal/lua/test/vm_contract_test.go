package test

// internal/lua 的黑盒判据：NewVM → LoadAdapterSource → 可观察输出。
// 不读适配器源码、不碰 v.pools。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/lua"
)

func newVM(t *testing.T) *lua.VM {
	t.Helper()
	return lua.NewVM(t.TempDir())
}

// TestNewVMExposesAdapterDir：构造参数可从 AdapterDir 读回。
func TestNewVMExposesAdapterDir(t *testing.T) {
	dir := t.TempDir()
	v := lua.NewVM(dir)
	if got := v.AdapterDir(); got != dir {
		t.Fatalf("★ AdapterDir = %q，期望 %q", got, dir)
	}
}

// TestListAdaptersEmptyThenLoaded：加载适配器后 ListAdapters 反映它。
func TestListAdaptersEmptyThenLoaded(t *testing.T) {
	v := newVM(t)
	if got := v.ListAdapters(); len(got) != 0 {
		t.Fatalf("★ 新 VM 的 ListAdapters = %v，期望空", got)
	}
	// 最小可用适配器脚本（静态检查要求声明 name/version）
	code := `-- name: contract-adapter
-- version: 1.0
return {
  transform_request = function(raw) return raw end,
  transform_response = function(raw) return raw end,
  transform_stream_chunk = function(raw) return raw end,
  endpoint = function() return "http://127.0.0.1/v1" end,
  headers = function() return {} end,
}`
	if err := v.LoadAdapterSource("contract-adapter", code); err != nil {
		t.Fatalf("LoadAdapterSource: %v", err)
	}
	got := v.ListAdapters()
	if len(got) != 1 {
		t.Fatalf("★ ListAdapters = %v，期望 1 个", got)
	}
	if got[0].Name != "contract-adapter" {
		t.Fatalf("★ Name = %q", got[0].Name)
	}
}

// TestLoadAdapterSourceRejectsBadScript：明显不合法的脚本必须报错。
func TestLoadAdapterSourceRejectsBadScript(t *testing.T) {
	v := newVM(t)
	if err := v.LoadAdapterSource("bad", "this is not lua at all $$$"); err == nil {
		t.Fatal("★ 非法脚本被接受")
	}
}

// TestCallTransformRequestUnknownAdapter：未加载的适配器调用必须报错。
func TestCallTransformRequestUnknownAdapter(t *testing.T) {
	v := newVM(t)
	_, err := v.CallTransformRequest("ghost", `{}`)
	if err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("★ 未加载适配器的报错不可诊断：%v", err)
	}
}

// TestGetAdapterEndpointUnknownIsEmpty：未知适配器端点为空串、不 panic。
func TestGetAdapterEndpointUnknownIsEmpty(t *testing.T) {
	if got := newVM(t).GetAdapterEndpoint("ghost"); got != "" {
		t.Fatalf("★ 未知适配器端点 = %q，期望空", got)
	}
}

package test

// `admin_child` 三件套的**渲染时机**判据（2026-11-19 实测回归）。
//
// ## 事故
//
// 原先在 `mainagent` 的 `init()` 里登记：
//
//	func init() { for _, td := range registry.ChildAdminTools() { RegisterTool(...) } }
//
// `ChildAdminTools()` 的内容取决于 `registry.Types()`（当前有哪些子 agent 类型），
// 而 `mainagent` 的 init 跑在 `resident`/`rtchild`/`worker` 的 init **之前**
// （那些包 import 本包）⇒ 当时 `Types()` 是空的 ⇒ 返回 nil ⇒
// **整个 admin_child 家族一个都没注册**。
//
// 现场实测（真二进制 /kernel）：219 个工具里 grep `admin_child` = 0
// ⇒ 主 agent 完全无法管理子 agent（父子两条协议之一整条消失）。
//
// ## 判据为什么是源码级
//
// 行为层要真起内核 + 读完 25 个插件才能观察到；而这条约束一旦被改回去，
// 症状是"某个工具悄悄不见了"，最难查的那类 —— 本次就是这么漏掉的。

import (
	"fmt"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/resident"
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/rtchild"
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/worker"
)

// TestChildAdminToolsAreRenderedNotInitRegistered：三件套的 enum **跟着类型表变**。
//
// ★ 黑盒（2026-11-25 改写）：观测 `ChildAdminTools()` 的**返回内容**，
//
//	不读 `tools_register.go` 的源码文本。
//
// ## 原判据为什么不合格
//
// 它 grep `tools_register.go` 里出现了 `ChildAdminTools()` 就报错。两个问题：
//
//	① 换个写法就绕过（`d := ChildAdminTools` / 换行 / 换文件）；
//	② 反过来，把它挪到别的文件做同样的事会误红 —— 判据在测**怎么写代码**。
//
// ## 事故本身是真实的
//
// 三件套曾在 `init()` 里快照 `ChildAdminTools()`，而 `mainagent` 的 init
// 跑在 `resident`/`rtchild`/`worker` 的 init **之前**（那些包 import 本包）
// ⇒ 当时 `registry.Types()` 是空的 ⇒ 返回 nil ⇒ 整个 `admin_child` 家族
// 一个都没注册（真二进制实测 219 个工具里 grep `admin_child` = 0）。
//
// ## 行为判据
//
// 注册一个新类型之后，`ChildAdminTools()` 的 `type` enum **立刻**包含它。
// 缓存的实现会停在旧清单上 —— 那正是要拦的形态。
func TestChildAdminToolsAreRenderedNotInitRegistered(t *testing.T) {
	const probe = "zz_render_probe"
	// 注册一个只有名字的探针类型（Construct 给个能过的假实现）。
	if err := registry.Register(registry.Descriptor{
		Meta: registry.Meta{Type: probe, Description: "探针类型（判据用）"},
		Construct: func(registry.Host, map[string]interface{}) (string, error) {
			return "", fmt.Errorf("探针类型不支持创建")
		},
	}); err != nil {
		t.Fatalf("注册探针类型: %v", err)
	}
	t.Cleanup(func() { _ = registry.Unregister(probe, nil) })

	defs := registry.ChildAdminTools()
	var adminChild *registry.ToolDef
	for i := range defs {
		if defs[i].Name == "admin_child" {
			adminChild = &defs[i]
			break
		}
	}
	if adminChild == nil {
		t.Fatal("★ 没有 admin_child 声明")
	}

	// type 参数是 enum：新类型必须出现在里面。
	enum := typeEnumOf(adminChild)
	if !enum[probe] {
		t.Fatalf("★ 注册 %q 之后 admin_child 的 type enum 没有它 —— "+
			"说明声明是**快照**而不是每轮现算（缓存实现会停在旧清单上）。\n"+
			"enum = %v", probe, enum)
	}
}

// typeEnumOf 从 admin_child 的 type 参数里取出 enum 集合。
func typeEnumOf(d *registry.ToolDef) map[string]bool {
	out := map[string]bool{}
	prop, ok := d.Properties["type"].(map[string]interface{})
	if !ok {
		return out
	}
	list, _ := prop["enum"].([]string)
	if list == nil {
		// 有些构造路径写的是 []interface{}，一并认。
		raw, _ := prop["enum"].([]interface{})
		for _, v := range raw {
			if sv, ok := v.(string); ok {
				out[sv] = true
			}
		}
		return out
	}
	for _, v := range list {
		out[v] = true
	}
	return out
}

// TestChildAdminToolsReflectLiveTypes：渲染结果跟着类型表变。
//
// ★ 这是"每轮现算"的行为证据：注册一个新类型后，`ChildAdminTools()`
//
//	立刻把新类型列进 enum；缓存的实现会停在旧清单上。
func TestChildAdminToolsReflectLiveTypes(t *testing.T) {
	defs := registry.ChildAdminTools()
	if len(defs) == 0 {
		t.Fatal("★ ChildAdminTools 返回空 —— 类型表为空，管理面不存在")
	}
	var adminChild *registry.ToolDef
	for i := range defs {
		if defs[i].Name == "admin_child" {
			adminChild = &defs[i]
			break
		}
	}
	if adminChild == nil {
		t.Fatal("★ 没有 admin_child 声明")
	}
	// 顶层只有四个参数（用户裁定 2026-11-18）。
	for _, k := range []string{"type", "action", "meta", "arg"} {
		if _, ok := adminChild.Properties[k]; !ok {
			t.Fatalf("★ admin_child 缺顶层参数 %q", k)
		}
	}
	if len(adminChild.Properties) != 4 {
		t.Fatalf("★ admin_child 顶层参数应为 4 个，实为 %d 个: %v",
			len(adminChild.Properties), keysOf(adminChild.Properties))
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

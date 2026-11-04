package test

// files 插件的**契约判据**：输入 → 输出，不碰内部实现。
//
// ## 为什么这个包必须有判据（2026-11-04）
//
// `internal/plugins/files` 有 581 行、CI 覆盖率实测 **0.0%** —— 一条判据都没有。
// 而它是**文件读写插件**，包含沙箱逃逸防护（`pathWithinSandbox`）。
//
// 沙箱边界是这个插件唯一真正危险的地方：写错一个字符就是任意文件读写。
// 没有判据意味着没人知道它现在是否还挡得住。
//
// ## 为什么走 handler 而不是直接调 pathWithinSandbox
//
// 后者是**实现方式**。契约是"模型给一个路径，插件决定读不读"：
//
//	输入：{path: "<任意字符串>"}
//	输出：{...} 或 {error: "path outside sandbox"}
//
// 只要外部行为不变，沙箱可以换实现（比如改用 filepath.Rel 判断），
// 判据不该因此变红。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugins/files"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// startFilesPlugin 起一个 files 插件，沙箱指向 dir，返回它的工具表。
//
// 用真实的 `Start()` 装配 —— 不手工构造 Plugin 结构，那样就测不到
// "Start 有没有把配置接上"（engineering-lessons §1.2）。
func startFilesPlugin(t *testing.T, sandbox string) map[string]sdk.ToolHandler {
	t.Helper()
	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(t.TempDir(), "config.db"))
	t.Cleanup(func() { cfgReg.Close() })

	tools := map[string]sdk.ToolHandler{}
	// Config 是必须的：Start 会读 cfg.Daemon.DataDir 决定"写前留档"根目录，
	// 拿不到 cfg 就 nil 解引用（实测 panic 在 plugin.go:66）。
	s := sdk.New("files", sdk.SDKConfig{
		Settings: sdk.NewSettings("files", cfgReg),
		Config:   sdk.NewConfig(&types.Config{Daemon: types.DaemonConfig{DataDir: t.TempDir()}}),
		EventBus: events.NewBus(),
		RegTool: func(name string, def sdk.ToolDef, h sdk.ToolHandler) error {
			tools[name] = h
			return nil
		},
	})
	p := files.New("files")
	if err := p.Start(s); err != nil {
		t.Fatalf("启动 files 插件失败: %v", err)
	}
	// 沙箱必须在 Start 之后设：Start 会从 settings 读 dir 并覆盖 filesDir。
	cfgReg.PluginConfig("files").Set("dir", sandbox)
	// 重新 Start 一次让新配置生效（幂等：RegisterDef 会去重）。
	if err := p.Start(s); err != nil {
		t.Fatalf("重新启动失败: %v", err)
	}
	return tools
}

// TestToolsAreRegistered 钉住基本契约：四个工具都在。
//
// 变异：删掉任一 RegisterTool ⇒ 本例红。
func TestToolsAreRegistered(t *testing.T) {
	tools := startFilesPlugin(t, t.TempDir())
	for _, name := range []string{"files_read", "files_write", "files_edit", "files_ls"} {
		if _, ok := tools[name]; !ok {
			t.Fatalf("★ 工具 %s 没有注册 —— 模型看不到它", name)
		}
	}
}

// TestReadWriteRoundTrip 钉住核心读写契约：写进去的内容能原样读回。
//
// 变异：让 write 不落盘 ⇒ 本例红。
func TestReadWriteRoundTrip(t *testing.T) {
	root := t.TempDir()
	tools := startFilesPlugin(t, root)
	target := filepath.Join(root, "a.txt")
	const body = "hello 契约判据"

	if _, err := tools["files_write"](map[string]interface{}{
		"path": target, "content": body,
	}); err != nil {
		t.Fatalf("write 失败: %v", err)
	}

	got, err := tools["files_read"](map[string]interface{}{"path": target})
	if err != nil {
		t.Fatalf("read 失败: %v", err)
	}
	// 输出形态可能是 map（含 content 字段）或纯字符串，两种都接受 ——
	// 判据只关心"内容能拿回来"。
	s := toStr(got)
	if !strings.Contains(s, body) {
		t.Fatalf("★ 写进去 %q，读回来 %q —— 读写不一致", body, s)
	}
}

// TestSandboxEscapeIsRejected 是**最重要的一条**：沙箱外路径必须被拒。
//
// 这是插件的安全边界。变异：让 pathWithinSandbox 恒返回 true ⇒ 本例红。
func TestSandboxEscapeIsRejected(t *testing.T) {
	root := t.TempDir()
	tools := startFilesPlugin(t, root)

	// 沙箱外造一个真实存在的文件 —— 若判据只拦"不存在的路径"，
	// 这条会假绿。文件真实存在而仍被拒，才证明是沙箱在起作用。
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0644); err != nil {
		t.Fatal(err)
	}

	// 三条典型逃逸：绝对路径越界、../ 上溯、符号链接式的相对跳转
	for _, bad := range []string{
		outside,
		filepath.Join(root, "..", "outside.txt"),
		filepath.Join(root, "sub", "..", "..", "etc", "passwd"),
	} {
		got, err := tools["files_read"](map[string]interface{}{"path": bad})
		if err != nil {
			continue // 直接报 error 也算拒绝
		}
		if !strings.Contains(toStr(got), "outside sandbox") &&
			!strings.Contains(toStr(got), "error") {
			t.Fatalf("★ 沙箱外路径 %q 被接受了，返回 %s ——\n"+
				"这是任意文件读取。沙箱根: %s", bad, toStr(got), root)
		}
	}
}

// TestSandboxAllowsInside 钉住反向：沙箱内路径必须能读。
//
// 与上一条成对 —— 只测"拒绝"会放过"拒绝一切"的实现。
func TestSandboxAllowsInside(t *testing.T) {
	root := t.TempDir()
	tools := startFilesPlugin(t, root)

	inside := filepath.Join(root, "ok.txt")
	if err := os.WriteFile(inside, []byte("visible"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := tools["files_read"](map[string]interface{}{"path": inside})
	if err != nil {
		t.Fatalf("沙箱内文件读取失败: %v", err)
	}
	if !strings.Contains(toStr(got), "visible") {
		t.Fatalf("★ 沙箱内文件读不到内容: %s", toStr(got))
	}
}

// TestEmptyPathIsRejected 钉住读/写的输入校验。
//
// ★ 不含 files_ls：它对空 path 有**明确设计** —— 当成 "."（列当前目录）。
//
//	见 plugin.go:488 `if path == "" { path = "." }`。
//	第一版判据把 ls 也纳进来，实测红：
//	    ★ files_ls 接受了空 path，返回 {"content":"(empty directory)"}
//	那不是缺陷，是我把三条工具的输入契约当成同一条了。分开钉。
func TestEmptyPathIsRejected(t *testing.T) {
	tools := startFilesPlugin(t, t.TempDir())
	for _, name := range []string{"files_read", "files_write"} {
		got, err := tools[name](map[string]interface{}{"path": ""})
		if err != nil {
			continue
		}
		if !strings.Contains(toStr(got), "error") && !strings.Contains(toStr(got), "required") {
			t.Fatalf("★ %s 接受了空 path，返回 %s", name, toStr(got))
		}
	}
}

// TestLsEmptyPathMeansCurrentDir 钉住 ls 的这条设计：空 path = "."。
//
// 与上一条成对：它们把"哪条工具拒绝空 path、哪条把它当默认值"这个差异
// 明确写下来。否则以后有人统一"都拒绝空 path"时会不知道这是有意的。
func TestLsEmptyPathMeansCurrentDir(t *testing.T) {
	root := t.TempDir()
	tools := startFilesPlugin(t, root)
	if err := os.WriteFile(filepath.Join(root, "in_root.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// 不传 path —— 应等价于列出沙箱根
	got, err := tools["files_ls"](map[string]interface{}{})
	if err != nil {
		t.Fatalf("ls 失败: %v", err)
	}
	_ = got // 真正的断言在 TestLsListsDirectory（显式给 path）；这里只要求不报错
}

// TestLsListsDirectory 钉住 ls 的输出契约。
func TestLsListsDirectory(t *testing.T) {
	root := t.TempDir()
	tools := startFilesPlugin(t, root)
	for _, n := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := tools["files_ls"](map[string]interface{}{"path": root})
	if err != nil {
		t.Fatalf("ls 失败: %v", err)
	}
	s := toStr(got)
	for _, n := range []string{"one.txt", "two.txt"} {
		if !strings.Contains(s, n) {
			t.Fatalf("★ ls 输出缺少 %s: %s", n, s)
		}
	}
}

// toStr 把 handler 的返回值转成字符串（形态可能是 string / map / 结构体）。
//
// 判据只关心"内容能不能拿到"，不关心它是哪一层包装 —— 所以统一 JSON 序列化
// 再查找子串。这样 handler 把结果放进 map 还是直接返回 string 都不影响判断。
func toStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

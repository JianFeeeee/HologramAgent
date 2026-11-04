package webui

// webui 文件下发能力的**契约判据**：面向输入 → 输出，不面向内部流程。
//
// ## 为什么重写（2026-11-04）
//
// 原判据（handler_files_test.go）直接调未导出的 `stageWebFile(src, isImage)`，
// 断言它返回的 url 前缀、size、以及中转文件是否落盘。问题：
//
//	① 它测的是**内部函数的形状**。`stageWebFile` 是"怎么实现"，
//	   不是"对外承诺什么"。改成分块上传、改成流式、改成存对象存储，
//	   只要 webui 通道仍然返回可下载的 url，外部行为就没变 —— 但判据会红。
//	② 它断言 `webFilesDir` 这个**包级变量**，那是实现细节。
//	③ 它没有走真实的调用链：真正入口是**输出通道 handler**
//	   （plugin.go RegisterOutputChannel("webui", …)），
//	   agent 发图/发文件时走的就是它。
//
// 所以本判据改为：**通过输出通道 handler 喂输入，断言返回的输出**。
//
//	输入：{payload: "<本地路径或 URL>", type: "image" | "file"}
//	输出：{status: "ok", url: "/files/<名字>.<ext>", size: N}
//
// 这样断言的是**通道契约**：agent 交一个文件，webui 交回一个可访问的 URL。
// 实现怎么中转（拷贝 / 硬链 / 对象存储）都不影响这条判据。
//
// ## 判据的边界
//
// 这里不验证 `/files/` 的鉴权 —— 那是 handler 层的契约，
// 由 handler 的黑箱判据覆盖（见 handler_files_download_test.go）。

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// captureWebUIOutput 起一个 webui 插件，截获它的 "webui" 输出通道 handler。
//
// 这是**测试装配**，不是绕过生产路径：生产里由内核的通道注册表接管
// （regOutput），这里换成捕获函数，为的是拿到真正被注册的 handler 本身。
func captureWebUIOutput(t *testing.T) (handler sdk.ToolHandler, filesDir string) {
	t.Helper()

	filesDir = t.TempDir()
	webFilesDir = filesDir
	t.Cleanup(func() { webFilesDir = "" })

	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(t.TempDir(), "config.db"))
	t.Cleanup(func() { cfgReg.Close() })
	seedWebUIConfig(cfgReg)
	// 绑空闲端口：:8080 是生产端口（见 a752ae1 的教训）。
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	cfgReg.PluginConfig("webui").Set("addr", addr)

	var captured sdk.ToolHandler
	s := testSDK(sdk.SDKConfig{
		Settings: sdk.NewSettings("webui", cfgReg),
		EventBus: events.NewBus(),
		RegOutput: func(name string, caps int, desc string, def sdk.ChannelDef, h sdk.ToolHandler) error {
			if name == "webui" {
				captured = h
			}
			return nil
		},
	})
	p := &Plugin{name: "webui", mux: http.NewServeMux()}
	if err := p.Start(s); err != nil {
		t.Fatalf("启动 webui 插件失败: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	if captured == nil {
		t.Fatal("★ webui 没有注册输出通道 handler —— 文件下发能力不存在")
	}
	return captured, filesDir
}

// TestOutputChannelImagesReturnDownloadableURL 是主判据：图片 → 可下载 URL。
//
// 变异：让 stageWebFile 对图片返回空 url ⇒ 本例红。
func TestOutputChannelImagesReturnDownloadableURL(t *testing.T) {
	handler, _ := captureWebUIOutput(t)

	src := filepath.Join(t.TempDir(), "photo.jpg")
	body := "fake-jpeg-bytes"
	if err := os.WriteFile(src, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := handler(map[string]interface{}{"payload": src, "type": "image"})
	if err != nil {
		t.Fatalf("发图片失败: %v", err)
	}
	m := out.(map[string]interface{})

	if m["status"] != "ok" {
		t.Fatalf("status = %v，期望 ok", m["status"])
	}
	url, _ := m["url"].(string)
	if !strings.HasPrefix(url, "/files/") {
		t.Fatalf("★ url = %q —— 前端按 /files/ 前缀加载，换前缀就点不开", url)
	}
	if !strings.HasSuffix(url, ".jpg") {
		t.Fatalf("★ url = %q —— 扩展名丢了，浏览器不会按图片渲染", url)
	}
	if size, _ := m["size"].(int64); size != int64(len(body)) {
		t.Fatalf("size = %v，期望 %d（前端用它显示文件大小）", m["size"], len(body))
	}
}

// TestOutputChannelRemoteURLPassesThrough 钉住：远程 URL 原样返回、不下载。
//
// 语义：agent 已经有一个公网可访问的图片，不需要中转。
// 变异：改成无条件拷贝 ⇒ 本例红（会返回 /files/ 前缀）。
func TestOutputChannelRemoteURLPassesThrough(t *testing.T) {
	handler, _ := captureWebUIOutput(t)

	const remote = "https://example.com/a.png"
	out, err := handler(map[string]interface{}{"payload": remote, "type": "image"})
	if err != nil {
		t.Fatalf("远端 URL 应放行，实际报错: %v", err)
	}
	url, _ := out.(map[string]interface{})["url"].(string)
	if url != remote {
		t.Fatalf("★ 远端 URL 被改写为 %q —— 浪费带宽且有 SSRF 风险", url)
	}
}

// TestOutputChannelDangerousExtensionIsNeutralized 钉住安全契约。
//
// 一个 .html 文件以图片身份下发时，若保留 .html，浏览器会在 webui 域下
// **执行它的脚本**（存储型 XSS）。所以非白名单扩展名必须被改写。
//
// 变异：删掉扩展名改写 ⇒ 本例红。
func TestOutputChannelDangerousExtensionIsNeutralized(t *testing.T) {
	handler, _ := captureWebUIOutput(t)

	src := filepath.Join(t.TempDir(), "evil.html")
	if err := os.WriteFile(src, []byte("<script>alert(1)</script>"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := handler(map[string]interface{}{"payload": src, "type": "file"})
	if err != nil {
		t.Fatalf("发文件失败: %v", err)
	}
	url, _ := out.(map[string]interface{})["url"].(string)
	if strings.HasSuffix(url, ".html") {
		t.Fatalf("★ html 以 .html 后缀下发（%q）—— 会在 webui 域下执行脚本", url)
	}
}

// TestOutputChannelEmptyPayloadIsRejected 钉住输入校验。
//
// 变异：允许空 payload ⇒ 本例红（会生成一个指向空的 URL）。
func TestOutputChannelEmptyPayloadIsRejected(t *testing.T) {
	handler, _ := captureWebUIOutput(t)

	if _, err := handler(map[string]interface{}{"payload": "", "type": "image"}); err == nil {
		t.Fatal("★ 空 payload 被接受 —— 会产出一个打不开的 URL")
	}
}

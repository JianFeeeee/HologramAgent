package test

// 判据：**敏感配置的揭示面必须不越界**（E-3 的另一半）。
//
// ## 为什么需要这一条
//
// `secret_meta_test.go` 已经钉住两件事：
//   · Secret 声明能到达 meta（前端才不会明文回显）
//   · 列表里是占位符、磁盘上是密文、单键 reveal 能看明文
//
// 但 **reveal 的权限边界零覆盖**。实测变异：把 `canRevealKey` 改成恒 true
// （即任意插件页都能读别的插件密钥）—— 现有 9 条判据**全部仍绿**。
//
// 这是本模块唯一"拒绝"型的安全边界，而它恰恰没被判据保护：
// 前端拿不到的明文，API 却能拿走。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugins/webui"
	internalSDK "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// TestRevealRejectsOtherPluginsSecret：请求**别的插件**的敏感键必须被拒。
//
// 构造：以 webui 自己的身份装配 Handler（`currentPluginOf()` 恒为 "webui"），
// 在另一个插件 `qq` 名下放一个敏感项，然后请 reveal 它。
//
// ★ 断言两件事，缺一不可：
//   - 状态码必须是拒绝（403），不是 200
//   - 响应体里**不得**出现那个明文（就算状态码错了也不能漏）
//
// 只断状态码会放过"403 但仍把值写进 body"的实现。
func TestRevealRejectsOtherPluginsSecret(t *testing.T) {
	dir := t.TempDir()
	reg := internalConfig.NewConfigRegistry(dir + "/config.db")
	reg.SetCodec(internalConfig.NewTestCodec(t))
	defer reg.Close()
	seedWebUIConfig(reg)

	// 另一个插件的密钥
	const foreignSecret = "qq-very-secret-token-999"
	qqSett := internalSDK.NewSettings("qq", reg)
	qqSett.RegisterDef(internalSDK.ConfigDef{Key: "owner_token", Type: "password"})
	if err := qqSett.Set("owner_token", foreignSecret); err != nil {
		t.Fatal(err)
	}

	h := webui.NewHandler(internalSDK.New("webui", internalSDK.SDKConfig{
		Settings: internalSDK.NewSettings("webui", reg),
		EventBus: events.NewBus(),
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/settings/reveal?key=plugin.qq.owner_token", nil)
	w := httptest.NewRecorder()
	serveRoutes(t, h, w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("★ 跨插件 reveal 未被拒（状态码 %d）—— 任意插件页都能读别的插件密钥",
			w.Code)
	}
	if strings.Contains(w.Body.String(), foreignSecret) {
		t.Fatalf("★ 拒绝的响应里仍带出了明文（前端拿不到的密钥被 API 拿走了）")
	}
}

// TestRevealAllowsOwnSecret（成对判据）：本插件自己的敏感键必须能揭示。
//
// 与上一条成对 —— 只测"拒绝"会放过"拒绝一切 reveal"的实现，
// 而那会让设置页的「显示密码」按钮永远失效。
//
// 这一条其实已被 `TestSecretPlainInUIButCipherOnDisk` 覆盖（它 reveal 的是
// plugin.webui.apikey），这里**不再重复写**，只在该文件里说明配对关系。
//
// —— 所以本文件只有一条判据：越界的那一条。

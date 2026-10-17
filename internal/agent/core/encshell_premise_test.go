package core

// 前提验证：插件 enc_shell 的**命令参数**是否已被内核填好明文？
//
// 若已填 ⇒ 插件自己的 lookupAnySecret 对内核别名是**死代码**，
// 且「保留 enc_shell」的理由只剩 argv 保护（脚本走 stdin）。
//
// 这决定了 SDK 该不该暴露 Fill()：
//   · 若插件本来就能从 args 拿到明文 ⇒ 暴露 Fill 不新增任何权限
//   · 若拿不到 ⇒ 暴露 Fill 会削弱边界（插件可枚举 List 再逐个 Fill）

import (
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

func TestPremise_EncShellCommandPrefilledByKernel(t *testing.T) {
	store := newScrubStore(t)
	a, _, _ := newRootForResidents(t)

	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alias, err := store.Submit(tok, "测")
	if err != nil {
		t.Fatal(err)
	}

	var cmdSaw string
	if err := a.stageHost.RegisterTool("probe_encsh", sdk.ToolDef{
		Name: "probe_encsh", Description: "模拟 enc_shell",
		Parameters: map[string]interface{}{"type": "object"},
	}, func(args map[string]interface{}) (interface{}, error) {
		cmdSaw, _ = args["command"].(string)
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := a.stageHost.ExecuteTool("probe_encsh", map[string]interface{}{
		"command": "curl -H 'Authorization: token @{" + alias + "}'",
	}); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(cmdSaw, "@{"+alias+"}") {
		t.Fatal("handler 仍看到别名 ⇒ 内核没填，插件需要自己的填充逻辑")
	}
	if !strings.Contains(cmdSaw, tok) {
		t.Fatalf("handler 应看到明文，实际 %q", cmdSaw)
	}
	t.Logf("✅ 内核已填 ⇒ 插件对**内核别名**的填充是死代码；" +
		"插件仍需自己解析的是**平台凭据**（@ {gitea-local}，含连字符，内核不认）")
}

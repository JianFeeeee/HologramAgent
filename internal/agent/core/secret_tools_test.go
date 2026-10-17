package core

// 判据：agent 面向的变量库工具（文档 C8）。
//
// 设计红线（文档 S1）：**永不返回明文**。
// 一旦有"取回明文"的路径，模型就能把明文写回上下文，整层防护作废。
// 下面的判据把这条红线钉死。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	"github.com/JianFeeeee/HomeAgent/internal/security"
)

const c8Token = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func newSecretToolAgent(t *testing.T) (*Agent, *security.Store) {
	t.Helper()
	store := newScrubStore(t) // 复用已有的测试夹具
	a := New(AgentConfig{
		ID:              "c8",
		ProviderManager: agentAPI.NewProviderManager(),
		StageHost:       NewStageHost(),
	})
	return a, store
}

func callSecret(a *Agent, name string, args map[string]interface{}) string {
	return a.executeSecretTool(agentAPI.ToolCall{Name: name, Arguments: args})
}

// ★★ 判据：secret_submit 登记后**只返回别名，不回显明文**。
func TestSecretSubmitReturnsAliasNotPlaintext(t *testing.T) {
	a, store := newSecretToolAgent(t)

	out := callSecret(a, "secret_submit", map[string]interface{}{
		"value": c8Token, "note": "测试令牌",
	})
	if strings.Contains(out, c8Token) {
		t.Fatalf("★ 违反 S1：secret_submit 回显了明文: %q", out)
	}
	if !strings.Contains(out, "@{sec_") {
		t.Fatalf("应返回别名: %q", out)
	}
	if store.Count() != 1 {
		t.Fatalf("应登记 1 条，实际 %d", store.Count())
	}
	// 别名可用：回填能换回明文
	alias := store.List()[0].Alias
	if got, _, _ := store.Fill("@" + "{" + alias + "}"); !strings.Contains(got, c8Token) {
		t.Error("别名应能回填出明文（这是唯一通道）")
	}
}

// ★★ 判据：**secret_list 绝不泄露明文**（S1 的核心）。
func TestSecretListNeverLeaksPlaintext(t *testing.T) {
	a, _ := newSecretToolAgent(t)
	callSecret(a, "secret_submit", map[string]interface{}{"value": c8Token, "note": "n"})

	out := callSecret(a, "secret_list", nil)
	if strings.Contains(out, c8Token) {
		t.Fatalf("★ 违反 S1：secret_list 泄露了明文: %q", out)
	}
	if !strings.Contains(out, "@{sec_") {
		t.Errorf("应列出别名: %q", out)
	}
	if !strings.Contains(out, "明文不可读回") {
		t.Error("应明确告知明文不可读回（防模型反复尝试）")
	}
}

// ★ 判据：**不存在 secret_get**（防回归：有人加了它，这条会指出为什么不行）。
func TestNoSecretGetTool(t *testing.T) {
	a, _ := newSecretToolAgent(t)
	callSecret(a, "secret_submit", map[string]interface{}{"value": c8Token})

	out := callSecret(a, "secret_get", map[string]interface{}{"alias": "sec_x"})
	if !strings.Contains(out, "未知的 secret 工具") {
		t.Fatalf("不应存在 secret_get（会把明文交回模型）: %q", out)
	}
	// 工具表里也不能有
	for _, d := range secretToolDefs() {
		m, _ := d.(map[string]interface{})
		name, _ := m["name"].(string)
		if name == "secret_get" || name == "secret_show" || name == "secret_reveal" {
			t.Fatalf("工具表里出现了明文读回工具 %q —— 违反 S1", name)
		}
	}
}

// ★★ 判据：secret_forget 删除后别名失效，且**不自动淘汰**。
func TestSecretForgetRemovesAndNoAutoEviction(t *testing.T) {
	a, store := newSecretToolAgent(t)
	callSecret(a, "secret_submit", map[string]interface{}{"value": c8Token})
	alias := store.List()[0].Alias

	// 宽容传参：@{sec_xxx} 与裸别名都可
	if out := callSecret(a, "secret_forget", map[string]interface{}{
		"alias": "@{" + alias + "}",
	}); !strings.Contains(out, "已删除") {
		t.Fatalf("删除应成功: %q", out)
	}
	if store.Count() != 0 {
		t.Fatalf("删除后应为空，实际 %d", store.Count())
	}
	if got, n, _ := store.Fill("@" + "{" + alias + "}"); n != 0 || strings.Contains(got, c8Token) {
		t.Error("删除后别名不应再回填")
	}
	// 重复删 → 明确提示（不是静默成功）
	if out := callSecret(a, "secret_forget", map[string]interface{}{"alias": alias}); !strings.Contains(out, "未找到") {
		t.Errorf("重复删除应明确提示: %q", out)
	}
}

// ★ 判据：空值提交被拒（不是静默登记一个空变量）。
func TestSecretSubmitRejectsEmpty(t *testing.T) {
	a, store := newSecretToolAgent(t)
	for _, v := range []string{"", "   "} {
		if out := callSecret(a, "secret_submit", map[string]interface{}{"value": v}); !strings.Contains(out, "不能为空") {
			t.Errorf("空值应被拒: %q", out)
		}
	}
	if store.Count() != 0 {
		t.Error("空值不应入库")
	}
}

// ★ 判据：安全模块未启用时，工具明确报不可用（不静默失败）。
func TestSecretToolsReportUnavailable(t *testing.T) {
	dir := t.TempDir()
	_ = dir
	security.SetGlobal(nil)
	defer newScrubStore(t) // 复原给后续判据

	a := New(AgentConfig{ID: "c8-off", StageHost: NewStageHost()})
	out := callSecret(a, "secret_list", nil)
	if !strings.Contains(out, "未启用") {
		t.Fatalf("未启用时应明确报不可用: %q", out)
	}
}

// ★ 判据：未启用时**不向模型暴露**这些工具（避免无谓往返）。
func TestSecretToolsHiddenWhenDisabled(t *testing.T) {
	security.SetGlobal(nil)
	a := New(AgentConfig{ID: "c8-hidden", StageHost: NewStageHost()})

	var names []string
	for _, d := range a.buildToolDefs() {
		m, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]interface{})
		if fn == nil {
			fn = m
		}
		if n, _ := fn["name"].(string); strings.HasPrefix(n, "secret_") {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		t.Fatalf("未启用安全模块时不应暴露 secret_* 工具: %v", names)
	}
}

// ★ 判据：启用时**要暴露**（否则模型不知道能用）。
func TestSecretToolsExposedWhenEnabled(t *testing.T) {
	newScrubStore(t)
	a := New(AgentConfig{ID: "c8-shown", StageHost: NewStageHost()})

	got := map[string]bool{}
	for _, d := range a.buildToolDefs() {
		m, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		// toolDef 返回的是 {"type":"function","function":{...}}
		if fn, ok := m["function"].(map[string]interface{}); ok {
			if n, _ := fn["name"].(string); strings.HasPrefix(n, "secret_") {
				got[n] = true
			}
		}
	}
	for _, want := range []string{"secret_submit", "secret_list", "secret_forget"} {
		if !got[want] {
			t.Errorf("启用后应暴露 %s；实际得到 %v", want, got)
		}
	}
}

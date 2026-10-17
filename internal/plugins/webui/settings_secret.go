package webui

// 密钥可见性（E-3）：设置面的明文屏蔽与显式揭示。
//
// ## 问题
//
// `GET /api/v1/settings` 此前直接返回 `Dump()` 的**解密后**结果 ——
// 也就是任何能打开设置页的人都能看到全部密钥（含其他插件的）。
// 落盘的 AES 加密因此只保护了静态文件，对“已运行的服务”毫无作用。
//
// ## 不是靠“能不能进设置页”，而是靠显式揭示
//
// 权威用户的判断是：**任何能进设置页的人都可见**，所以要不要放宽不是重点；
// 重点是**默认不显示**，要看必须显式请求（且留痕），并且只看得到**自己**的。
//
// ## 两条纪律
//
//  1. **默认屏蔽**：列表接口对敏感键只返回占位符，真值要单独揭示。
//  2. **不跨插件**：`plugin.<other>.*` 的敏感值一律不可揭示 ——
//     否则“插件 A 的页面能读到插件 B 的密钥”，比不屏蔽更糟
//     （因为它看起来像是被管住了）。

import (
	"net/http"
	"strings"
)

// SecretPlaceholder 是敏感值在列表里的占位符。
//
// 用固定串而不是“空”或“***”：前端据此显示「已设置（点击查看）」，
// 而空串会被误认成“没配”。
const SecretPlaceholder = "••••••••"

// currentPluginOf 返回请求方所属插件名（webui 的接口是给 WebUI 自己的）。
func (h *Handler) currentPluginOf() string { return "webui" }

// maskSecretValues 把 values 里的敏感键换成占位符。
//
// reveal 为真时**不**屏蔽（调用方已做权限判定）。
func (h *Handler) maskSecretValues(values map[string]interface{}, reveal bool) {
	if reveal || h.settings == nil {
		return
	}
	for k, v := range values {
		if v == nil || v == "" {
			continue // 未设置：保持原样，否则前端会把“没配”显示成“已配”
		}
		if h.settings.IsSecret(k) {
			values[k] = SecretPlaceholder
		}
	}
}

// canRevealKey 判定某键的明文是否允许被揭示给当前调用方。
//
// 规则：核心键（core.*）与**本插件**的键可揭示；其他插件的键不可。
func (h *Handler) canRevealKey(key string) bool {
	if !strings.HasPrefix(key, "plugin.") {
		return true // 核心配置：调用方已是能进设置页的权威用户
	}
	parts := strings.SplitN(key, ".", 3)
	if len(parts) < 3 {
		return false
	}
	return parts[1] == h.currentPluginOf()
}

// handleSettingsReveal 是 `GET /api/v1/settings/reveal?key=...` 的实现。
//
// 显式揭示**单个**键的明文。为何一次只给一个：
// 批量揭示等于把“屏蔽”又变回“一次全给你”，屏蔽就成了装饰。
func (h *Handler) handleSettingsReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.settings == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "config registry not available"})
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key 必填"})
		return
	}
	if !h.settings.IsSecret(key) {
		// 非敏感键无需揭示（但也不报错：前端可能不分类型地调用）
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"key": key, "value": h.getSettingValue(key), "secret": false,
		})
		return
	}
	if !h.canRevealKey(key) {
		// ★ 不跨插件。这里必须拒绝，否则“插件页能读别的插件密钥”。
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "不允许读取其他插件的敏感配置（E-3）",
		})
		return
	}
	// 留痕：揭示明文是特权动作，必须可审计。
	if h.settings != nil {
		_ = h.settings.SetPlugin("webui", "last_secret_reveal", key)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key": key, "value": h.getSettingValue(key), "secret": true,
	})
}

// getSettingValue 按 namespace 取一个配置值（plugin.* 走插件表）。
func (h *Handler) getSettingValue(key string) interface{} {
	if strings.HasPrefix(key, "plugin.") {
		parts := strings.SplitN(key, ".", 3)
		if len(parts) >= 3 {
			v, _ := h.settings.GetPlugin(parts[1], parts[2])
			return v
		}
		return nil
	}
	v, _ := h.settings.GetCore(key)
	return v
}

// IsSecretPlaceholder 报告一个值是否是掩码占位符本身。
//
// ## 为什么写侧必须判它（真实故障，2026-10-16）
//
// E-3 让列表接口把敏感值换成 `••••••••`（防泄漏，这部分是对的），
// 但**写侧此前完全不检查占位符**，于是：
//
//	① 设置页 GET 拿到掩码 `••••••••`
//	② 用户点同一个表单里的「保存」按钮（前端 saveSetting 会一并回传该组的所有字段，
//	   包括刚被掩码的 api_key）——**即使用户只想改别的字段**
//	③ PUT 把 `••••••••` 当真实值写进内核 `core.llm.sources.<n>.api_key`
//	④ 任何读该键的下游（如 commondev 的 pi 源导入器）拿到的是**掩码而不是 key**
//	⑤ pi 用它调 LLM ⇒ 401 invalid api key
//
// 实测证据：内核 `core.llm.api_key`（主键）解出来是**真 key**，
// 而 `core.llm.sources.llmsproxy.api_key`（子键）解出来是 `••••••••`。
// 插件导入器优先读 `sources.*` ⇒ 掩码进了 pi 的 auth.json。
//
// ⇒ 占位符**不是值**，它是"这里有个值但我不告诉你"的标记。
//
//	把它存回去等于用标记覆盖了真值。这不是"校验用户输入"，而是**语义错误**。
func IsSecretPlaceholder(v interface{}) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	return strings.TrimSpace(s) == SecretPlaceholder
}

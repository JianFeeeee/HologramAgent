package proc

// 变量库的 proc 桥（内核侧）。
//
// ## 与 settings 的分工
//
//	settings  —— **每个插件自己的**配置（各自的 config_<name> 表）
//	secrets   —— **全进程共享的**变量库（security.Global() 单例）
//
// 这个区别是本质的：变量库全局唯一（设计 S10），主/子/驻留子 agent
// 与全部插件共用同一套别名。所以它不该走 settings 的命名空间机制。
//
// ## 为何需要这层桥
//
// 外部插件是**子进程**，它的 `sdk.Secrets()` 拿到的是模板里的
// `procSecrets`（见 templates/proc_main.go.tmpl），它把调用转成 RPC
// 发到这里。这条链路有 4 个地方要同时改（协议常量 / 路由 / 分发 / 能力表），
// 少一处就报 "未知 method" —— 真机 e2e 会立刻发现（本次已踩过一次）。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

func (h *coreHandler) secrets(method string, params json.RawMessage) (interface{}, error) {
	sec := h.sdk.Secrets()
	if sec == nil {
		return nil, errUnavailable("secrets")
	}

	switch method {
	case MethodSecretsAvailable:
		// 明确的可用性查询（不靠 List 推断 —— 见 secrets_impl 的注释）
		return map[string]interface{}{"available": sec.Available()}, nil

	case MethodSecretsStore:
		var p struct {
			Value string `json:"value"`
			Note  string `json:"note"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		alias, err := sec.Store(p.Value, p.Note)
		if err != nil {
			return nil, err
		}
		// ★ 只回别名，不回显明文（与 SDK 的契约一致）
		return map[string]interface{}{"alias": alias}, nil

	case MethodSecretsList:
		infos := sec.List()
		if infos == nil {
			infos = []pubsdk.SecretInfo{}
		}
		return map[string]interface{}{"infos": infos}, nil

	case MethodSecretsForget:
		var p struct {
			Alias string `json:"alias"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, sec.Forget(p.Alias)

	case MethodSecretsFill:
		var p struct {
			Text string `json:"text"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		out, n, aliases := sec.Fill(p.Text)
		if aliases == nil {
			aliases = []string{}
		}
		return map[string]interface{}{
			"text": out, "replaced": n, "aliases": aliases,
		}, nil

	case MethodSecretsEncrypt:
		// 二进制负载经 base64 过 RPC（JSON 不能直接带 bytes）
		var p struct {
			PlainB64 string `json:"plain_b64"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(p.PlainB64)
		if err != nil {
			return nil, fmt.Errorf("secrets.encrypt: base64 解码失败: %w", err)
		}
		enc, err := sec.Encrypt(raw)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"encrypted": enc}, nil

	case MethodSecretsDecrypt:
		var p struct {
			Stored string `json:"stored"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		plain, err := sec.Decrypt(p.Stored)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"plain_b64": base64.StdEncoding.EncodeToString(plain),
		}, nil
	}
	return nil, fmt.Errorf("未知 secrets method: %s", method)
}

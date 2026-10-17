package sdk

// SecretsAPI 的内核实现：桥接到 `internal/security` 的进程级变量库。
//
// ## 为何是"桥接"而不是"另建一套"
//
// 变量库是**进程级单例**（`security.Global()`）：主/子/驻留子 agent 共享
// 同一套别名表（设计 S10）。插件若自建一套，两库互不可见 ——
// 内核拦截时登记的变量插件看不到，插件登记的拦截又不认识，
// 同一个凭据可能两处各存一份而使用者不知哪个权威。
//
// 所以这个实现只是把调用转发到那个单例。**没有第二份存储**。
//
// ## 未启用时的行为
//
// `security.Global()` 为 nil 时**不返回 nil**，而是返回一个 Available()
// 为 false 的实现。理由：nil 会让插件到处判空，而判空漏一处就 panic
// （本仓在 `*SensitiveInterceptor` 上吃过这个亏）。给一个明确的
// "不可用" 实现，插件只需问 `Available()`。

import (
	"fmt"

	internalSecurity "github.com/JianFeeeee/HomeAgent/internal/security"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// secretsImpl 实现 pubsdk.SecretsAPI。
type secretsImpl struct{}

// NewSecretsAPI 构造变量库接口（内核装配时注入给每个插件）。
func NewSecretsAPI() pubsdk.SecretsAPI { return &secretsImpl{} }

func (s *secretsImpl) Available() bool { return internalSecurity.Global() != nil }

// AvailableForRPC 是 proc 桥用来**明确报告**可用性的入口。
//
// ★ 为什么不能靠 List 推断：未启用时 List 返回空列表（**不报错**），
// 于是"库是空的"与"库不存在"在 RPC 层长得一样 —— 实测踩到：
// 插件的 Available() 误判为 true，接着 Store 才失败，错误信息变成
// "存储失败"而不是"能力不可用"，排查时方向完全错。
func (s *secretsImpl) AvailableForRPC() bool { return s.Available() }

func (s *secretsImpl) Store(value, note string) (string, error) {
	st := internalSecurity.Global()
	if st == nil {
		// ★ 响亮失败：不静默降级成"存到别处"或"不存"。
		// 插件以为存好了而实际没存，比直接报错危险得多。
		return "", fmt.Errorf("安全模块未启用（core.security.enabled=false），无法登记凭据")
	}
	return st.Submit(value, note)
}

func (s *secretsImpl) List() []pubsdk.SecretInfo {
	st := internalSecurity.Global()
	if st == nil {
		return nil
	}
	infos := st.List()
	out := make([]pubsdk.SecretInfo, len(infos))
	for i, in := range infos {
		out[i] = pubsdk.SecretInfo{
			Alias: in.Alias, Source: in.Source, Note: in.Note,
			CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt, Hits: in.Hits,
		}
	}
	return out
}

func (s *secretsImpl) Forget(alias string) error {
	st := internalSecurity.Global()
	if st == nil {
		return fmt.Errorf("安全模块未启用，变量库不可用")
	}
	return st.Forget(alias)
}

func (s *secretsImpl) Fill(text string) (string, int, []string) {
	st := internalSecurity.Global()
	if st == nil {
		return text, 0, nil
	}
	return st.Fill(text)
}

func (s *secretsImpl) Encrypt(plain []byte) (string, error) {
	c := internalSecurity.ConfigCodec()
	if c == nil || !c.Enabled() {
		// ★ 拒绝明文落盘：插件以为存的是密文而实际是明文，是最危险的静默降级。
		return "", fmt.Errorf("内核未装配加密能力，拒绝以明文存储（core.security.enabled=false？）")
	}
	return c.Seal(string(plain))
}

func (s *secretsImpl) Decrypt(stored string) ([]byte, error) {
	// 非密文原样返回（兼容插件已有的明文文件，避免升级即坏）
	if !internalSecurity.IsEncrypted(stored) {
		return []byte(stored), nil
	}
	c := internalSecurity.ConfigCodec()
	if c == nil || !c.Enabled() {
		return nil, fmt.Errorf("有密文但内核未装配加密能力（密钥换了？）")
	}
	plain, err := c.Open(stored)
	if err != nil {
		return nil, err
	}
	return []byte(plain), nil
}

var _ pubsdk.SecretsAPI = (*secretsImpl)(nil)

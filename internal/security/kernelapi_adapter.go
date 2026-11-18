package security

// kernelapi_adapter.go —— 把 security 的实现面适配成 `internal/kernelapi` 的接口。
//
// ## 为什么需要它（用户裁定 2026-11-17）
//
// > 「注意解耦的目的是让其他模块在不 import 实现的前提下通过 import 接口进行调用」
//
// agent 是内核模块，但它**不该 import security 实现包**：那样它就被绑死在
// 安全模块的内部形状上（Store / Runtime / AgentScope / Round / Patterns 一堆
// 具体类型）。用户还强调：
//
// > 「安全模块是强制的透明层」
//
// ⇒ 透明层意味着消费方**看不见它**：既不该看到它是否装配，也不该看到它的
// 内部类型。所以 agent 只经 `kernelapi.SecurityGate` 调用，由本文件把
// 实现面转成那个接口。
//
// ## 这里的适配不是"多一层包装"
//
// 接口方法集是**按消费方实际用到**的（见 kernelapi.SecurityGate 的注释），
// 所以适配层只有薄薄的转发。它存在的价值是让 `go list -deps ./internal/agent/agent`
// 里**没有 internal/security**。

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// AsGate 把安全运行时暴露为 kernelapi.SecurityGate。
//
// ★ rt 为 nil 时返回 nil（装配期缺依赖 ⇒ 调用方响亮失败）。
// 语义上"未启用"不是 nil：那是 Disabled Runtime（所有判定零成本透传），
// 见 Runtime.Enabled 的注释。
func (rt *Runtime) AsGate() kernelapi.SecurityGate {
	if rt == nil {
		return nil
	}
	return gateAdapter{rt: rt}
}

type gateAdapter struct{ rt *Runtime }

func (g gateAdapter) Enabled() bool { return g.rt.Enabled() }

func (g gateAdapter) ForAgent(agentID string, isChild bool) kernelapi.SecurityScope {
	sc := g.rt.ForAgent(agentID, isChild)
	if sc == nil {
		return nil
	}
	return scopeAdapter{sc: sc}
}

func (g gateAdapter) ReleaseAgent(agentID string) { g.rt.ReleaseAgent(agentID) }

func (g gateAdapter) BeginRound(in model.RoundInput) (kernelapi.SecurityRound, error) {
	rc, err := g.rt.BeginRound(in)
	if rc == nil {
		return nil, err
	}
	return roundAdapter{rc: rc}, err
}

func (g gateAdapter) DeclareChildPolicy(childID string, allow, deny []string) error {
	return g.rt.DeclareChildPolicy(childID, allow, deny)
}

func (g gateAdapter) RevokeChildPolicy(childID string) { g.rt.RevokeChildPolicy(childID) }

func (g gateAdapter) CheckPath(direction, path string) model.Decision {
	return g.rt.CheckPath(direction, path)
}

func (g gateAdapter) CheckNetwork() model.Decision { return g.rt.CheckNetwork() }

func (g gateAdapter) ScanContentForAgent(agentID string, isChild bool, ins model.Inspection, text string) ([]model.Finding, bool, string) {
	return g.rt.ScanContentForAgent(agentID, isChild, ins, text)
}

func (g gateAdapter) DeclareOutputs(agentID string, outputs []string) {
	g.rt.DeclareOutputs(agentID, outputs)
}

func (g gateAdapter) ReleaseOutputs(agentID string) { g.rt.ReleaseOutputs(agentID) }

func (g gateAdapter) CheckOutput(agentID string, channel string) model.Decision {
	return g.rt.CheckOutput(agentID, channel)
}

// Registry 暴露记录表的写面（agent 登记/删除用；未注入 ⇒ nil）。
func (g gateAdapter) Registry() kernelapi.AgentRegistry { return g.rt.agentRegistry }

// Archive 实现 kernelapi.SecurityGate：写前留档（数据面动作，与判定同一层）。
func (g gateAdapter) Archive(dataDir, path string) (bool, error) {
	return ArchiveBeforeWrite(dataDir, path)
}

func (g gateAdapter) ClampPriority(declared string, privileged bool) string {
	return g.rt.ClampPriority(declared, privileged)
}

func (g gateAdapter) Store() kernelapi.SecretStore {
	st := g.rt.Store()
	if st == nil {
		return nil
	}
	return storeAdapter{st: st}
}

// ── 策略视图 ──

type scopeAdapter struct{ sc *AgentScope }

func (s scopeAdapter) BeginRound(in model.RoundInput) (kernelapi.SecurityRound, error) {
	rc, err := s.sc.BeginRound(in)
	if rc == nil {
		return nil, err
	}
	return roundAdapter{rc: rc}, err
}

func (s scopeAdapter) SetLongtimeProfile(name string) error { return s.sc.SetLongtimeProfile(name) }
func (s scopeAdapter) LongtimeProfile() string              { return s.sc.LongtimeProfile() }
func (s scopeAdapter) SessionProfile() string               { return s.sc.SessionProfile() }
func (s scopeAdapter) EffectiveProfile() string             { return s.sc.EffectiveProfile() }
func (s scopeAdapter) CheckTool(tool string) model.Decision {
	return s.sc.CheckTool(tool)
}

// ── 轮次句柄 ──

type roundAdapter struct{ rc *Round }

func (r roundAdapter) Principal() string                    { return r.rc.Principal().ID }
func (r roundAdapter) Profile() string                      { return r.rc.Profile() }
func (r roundAdapter) EffectiveProfile() string             { return r.rc.EffectiveProfile() }
func (r roundAdapter) CheckTool(name string) model.Decision { return r.rc.CheckTool(name) }
func (r roundAdapter) End()                                 { r.rc.End() }
func (r roundAdapter) CheckContent(ins model.Inspection, text string) ([]model.Finding, bool) {
	return r.rc.CheckContent(ins, text)
}

// ── 变量库 ──

type storeAdapter struct{ st *Store }

func (a storeAdapter) Replace(text, where string, _ []model.SecretPattern) (string, []model.Evt) {
	// detect 恒用**实现侧的内置模式库**：调用方不该为"用哪些模式"
	// import security（规则是实现细节）。
	return a.st.Replace(text, where, Patterns)
}

func (a storeAdapter) Fill(text string) (string, int, []string)            { return a.st.Fill(text) }
func (a storeAdapter) FillMap(args map[string]interface{}) (int, []string) { return a.st.FillMap(args) }
func (a storeAdapter) Put(alias, plain, source, note string) (bool, error) {
	return a.st.Put(alias, plain, source, note)
}
func (a storeAdapter) Submit(plain, note string) (string, error) { return a.st.Submit(plain, note) }
func (a storeAdapter) Forget(alias string) error                 { return a.st.Forget(alias) }
func (a storeAdapter) List() []model.SecretInfo {
	infos := a.st.List()
	out := make([]model.SecretInfo, len(infos))
	copy(out, infos) // Info = model.SecretInfo（别名，直接拷贝）
	return out
}
func (a storeAdapter) ErrUnknownAlias() error { return ErrUnknownAlias }

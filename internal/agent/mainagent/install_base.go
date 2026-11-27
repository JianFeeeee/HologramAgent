package mainagent

// install.go —— 类型模块把自己的实现**装进**基座的四条入口。
//
// ## 为什么是"安装"而不是"基座自己 new"
//
// 内核持有基座 ⇒ 基座是唯一的运行载体；但"用哪种上下文 / 有没有调度器 /
// 哪些专属服务"是**类型**的决定。所以基座只提供安装点，具体实现在
// 类型的 Initialize 回调里装进来（`RuntimeHooks.Initialize`）。
//
// 未安装 ⇒ 基座保持 nil，所以"这不是主 agent"不靠类型分支表达，
// 而是靠"没有人给它装过"这个装配事实表达。

import (
	"os"
	"path/filepath"

	mainctx "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/ctx"
	sched "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/sched"
)

// contextFileName 是本类型的上下文持久化文件名（内核只给目录，名字归类型）。
const contextFileName = "context.json"

// InstallContext 装入本类型的上下文实现（主：策略裁剪型；整窗型另装）。
func (m *Module) InstallContext(c *mainctx.RelevanceContext) {
	if m == nil || m.a == nil {
		return
	}
	m.ctx = c
}

// OpenContext 用**内核分配的实例目录**开一份上下文实现。
//
// ## 用户裁定（2026-11-19）
//
// > 「agent 初始化包括运行时的数据目录是内核传入的参数，相当于目录是内核
// >    统一管理的」
//
// 所以路径**不是**配置里的字符串、也不是本类型拼的：
// 内核给 `a.InstanceDir()`，类型只决定"我的上下文文件在其中叫什么"。
//
// dir 为空 ⇒ 不持久化（单测/独立工具里的裸 agent），行为与历史一致。
func (m *Module) OpenContext() {
	if m == nil || m.a == nil {
		return
	}
	dir := m.a.InstanceDir()
	savePath := ""
	if dir != "" {
		savePath = filepath.Join(dir, contextFileName)
		m.a.EnsureWorkDir()
	}
	// ★ embedder 走**基座 syscall**（docs §3.4），不从 AgentConfig 取：
	// 那是内核设施的句柄，`AgentConfig` 里不许出现（§2 结论 4）。
	// 类型侧只持一个转发适配器，资产（模型）仍只有内核那一份。
	rc := mainctx.NewRelevanceContext(savePath, embedderFaceOf(m.a))
	// 裁剪保留条数：从配置传入（0 时保持 NewRelevanceContext 的历史默认）。
	if n := m.a.CtxTuning().ProtectedCount; n > 0 {
		rc = rc.WithProtected(n)
	}
	m.InstallContext(rc)
}

// InstallScheduler 装入本类型的输入调度器（无队列的类型不装）。
func (m *Module) InstallScheduler() {
	if m == nil || m.a == nil {
		return
	}
	if m.sched == nil {
		m.sched = sched.New(256)
	}
	// ★ 交给基座：内核投中断（L4）、停机补终态、状态面统计都要到达它。
	//   实例仍归本类型持有 —— 基座不驱动、不决策。
}

// InjectSecretStore 把安全模块的变量库注入到本 agent 的上下文与 stageHost。
//
// ★ 为什么由类型在 Initialize 里调、而不是基座在 New 里做：
// 它要依赖"类型装进来的上下文实现"。基座在 New 时可能还没有上下文。
func (m *Module) InjectSecretStore() {
	if m == nil || m.a == nil {
		return
	}
	gate := m.a.Gate()
	if gate == nil {
		return
	}
	st := gate.Store()
	if st == nil {
		return
	}
	if m.ctx != nil {
		m.ctx.SetSecretStore(st)
	}
	// ★ stageHost 是**类型自己的**装配（主 agent 的插件面），基座不认识它。
	//   SecretStore 由装配侧分发给需要的设施（谁持有谁接）。
	//
	// 源表句柄归**基座**（`Config().ProviderManager` 是它的公有字段）：
	// 类型的装配点不自己去摸内核设施，用基座的转发。
	m.a.SetProviderSecretStore(st)
}

// RestoreContext 从**内核传入的实例目录**恢复本类型的上下文现场。
//
// ★ 由内核驱动（`InstanceManager.Restore` → `Agent.RestoreInstance` →
//
//	本回调）：类型不决定"何时恢复"，只决定"目录里存的东西怎么读回来"。
//
// 目录里没有文件 ⇒ 首次启动，正常（不报错、不造一个空上下文）。
func (m *Module) RestoreContext(dir string) error {
	if m == nil || m.a == nil || dir == "" {
		return nil
	}
	p := filepath.Join(dir, contextFileName)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	m.OpenContext()
	return nil
}

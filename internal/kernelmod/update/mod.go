// Package update 是 update 模块的适配层：把 internal/update 的能力接进内核模块体系。
//
// ## 为什么分两层（与仓库其他模块同一惯例）
//
//	internal/update/          业务实现（版本比较、解包、替换、回滚、Release 查询）
//	internal/kernelmod/update/ 适配层（读 ks → 调业务包 → 写回 ks）
//
// 参照 supervisor 模块：实现在 internal/supervisor，这里只做装配。
// 分开的理由：internal/update 要被 CLI / WebAPI / agent 工具三端调用，
// 它们都没有 KernelState；而模块体系只有内核启动一条路径。
//
// ## 它为什么必须在第 0 层
//
// 肩负「启动失败回滚上一次升级」的职责（详见 internal/update/rollback.go）。
// 该职责必须早于所有其他模块：若排在后面，升级后新内核在 memory/llm 阶段
// 崩了，而回滚还没跑 ⇒ 机器进崩溃循环，只能靠 guard 的 failback 兜底。
// failback 是最后手段，不该承担"换错二进制"这种已知可回滚的故障。
package update

import (
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	upd "github.com/JianFeeeee/HologramAgent/internal/update"
)

// FacilityVersion 是本模块提供的设施名。
//
// 语义：启动时的版本自检与回滚判定**已完成**。需要按版本行事的模块
// （healthcheck 上报版本、pluginhost 判插件兼容性）应 Requires 它，
// 以确保读到的是回滚后的最终版本，而不是升级前的那个。
const FacilityVersion = "kernel.version"

// Module 实现 kernelboot.Module。
type Module struct{}

func (Module) Name() string       { return "update" }
func (Module) Requires() []string { return nil } // 第 0 层：不依赖任何人
func (Module) Provides() []string { return []string{FacilityVersion} }

// Init 执行版本自检，必要时回滚上一次升级。
func (Module) Init(ks *kernelboot.KernelState) error {
	st := &upd.State{}
	ks.Update = st

	bin := upd.KernelBinaryPath()
	st.BinaryPath = bin

	// ① 自报版本（升级是否真换成功，只有二进制自己能答）
	v, err := upd.ReadBinVersion(bin)
	if err != nil {
		// 读不出来不等于致命：可能是极老版本（无 --version）。
		// 没有证据表明升级失败 ⇒ 不回滚，记录后继续。
		log.Printf("[update] kernel version probe failed: %v", err)
		st.ProbeErr = err
	} else {
		st.Version, st.Commit, st.SDKVersion = v.Version, v.Commit, v.SDKVersion
		log.Printf("[update] kernel %s (commit %s, sdk %s)", v.Version, upd.ShortCommit(v.Commit), v.SDKVersion)
	}

	// ② 上一次升级的回滚判定
	rolled, from, to, rerr := upd.RollbackIfUnconfirmed(ks.DataDir, bin)
	if rerr != nil {
		// 回滚失败必须响亮：继续装配等于用一个已知起不来的版本
		return rerr
	}
	if rolled {
		log.Printf("[update] ROLLED BACK kernel %s → %s（上一次升级未能确认启动）", to, from)
		st.RolledBack = true
		st.RolledFrom, st.RolledTo = from, to
		// 回滚后重读版本：后续模块要看到**回滚后**的版本
		if v2, err2 := upd.ReadBinVersion(bin); err2 == nil {
			st.Version, st.Commit, st.SDKVersion = v2.Version, v2.Commit, v2.SDKVersion
		}
	} else {
		// ③ 版本已确认能启动 ⇒ 清除待确认标记
		//
		// 只依赖"进程活着且执行到第 0 层"，不依赖其他模块 ⇒ 不会误判。
		if err := upd.ClearPendingUpgrade(ks.DataDir); err != nil {
			// 清不掉不阻断启动：后果只是下次启动再判一次（幂等）
			log.Printf("[update] clear pending marker: %v", err)
		}
	}

	// ④ 依赖图路径（真正的 digest 校验在 kernelboot 装载期，那里失败会拒绝启动）
	if dg := filepath.Join(filepath.Dir(bin), "depgraph.bin"); upd.FileExists(dg) {
		st.DepGraphPath = dg
	}

	log.Printf("[update] subsystem ready (version=%s rolled_back=%v)", st.Version, st.RolledBack)
	return nil
}

func init() { kernelmod.Register(Module{}) }

var _ kernelboot.Module = Module{}

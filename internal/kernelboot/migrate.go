package kernelboot

// 数据目录迁移的**编排**：伪关机 → 迁移 → 唤醒。
//
// ## 流程（用户要求的顺序）
//
//	① Freeze 插件工具入口      —— 不再进新的工具调用
//	② Quiesce 各模块写者        —— distiller/textmem 等停下后台写
//	③ WaitDrain 等在途调用归零  —— 不能搬"写了一半的数据"
//	④ （外部脚本执行 rsync）     —— 真搬数据 + 改 systemd + 改 DB
//	⑤ Resume 写者               —— 按启动顺序恢复
//	⑥ Thaw 工具入口             —— 重新接受工具调用
//
// ## ★ 本包只做 ①②③⑤⑥，不执行 ④
//
// 迁移是**破坏性且不可逆**的（21G 数据 + 12 处 DB 改写 + 46 个插件文件改写），
// 而且内核跑在 systemd 沙箱里（ProtectSystem/ProtectHome）**本来就写不了
// /etc/systemd**。⇒ 真迁移交给运维执行生成的脚本（见 internal/systemd/migrate.go），
// 本包负责**把它前后包起来**：确保迁移窗口内没有任何写者。
//
// ## ★ 失败策略：任何一步失败都要**回滚到能跑的状态**
//
// 伪关机失败 ⇒ 立刻唤醒已停的，让内核回到正常态，别卡在半关。
// 迁移失败 ⇒ 调用方负责回滚数据；本包的 Resume 仍然要执行（否则内核永久冻结）。

import (
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

// inflightToolsFn 报告当前在途工具调用数（由装配处注入）。
//
// 用函数注入而不用字段直连：kernelboot 不能依赖 agent/core。
var inflightToolsFn atomic.Value // func() int64

// SetInflightToolCounter 注入在途工具计数（装配处调一次）。
func SetInflightToolCounter(fn func() int64) {
	if fn != nil {
		inflightToolsFn.Store(fn)
	}
}

func hasInflightTools() bool {
	v := inflightToolsFn.Load()
	if v == nil {
		return false
	}
	fn, _ := v.(func() int64)
	if fn == nil {
		return false
	}
	return fn() > 0
}

// PluginQuiescer 是**插件层的伪关机能力**。
//
// ★ 这里刻意复用已有的**运行态停启**（Registry.StopPlugin/StartPlugin），
//
//	  而不是新造"冻结"机制 —— 三态生命周期（自动启动 / 启用但不自动启动 /
//	  禁用）早已存在，且运行态停启**不写任何持久状态**（不碰 disabled_plugins，
//	  也不置 shuttingDown）⇒ 天然可逆，正是伪关机需要的语义。
//
//		StopPlugin  ⇒ 停掉子进程 + **摘掉注册面**（工具/stage/通道）
//		              ⇒ 停完模型看不到、也调不到它们（比"冻结入口"更彻底）
//		StartPlugin ⇒ 重新拉起，清运行态门
//
// ★ 与 StopAll 的区别（用错就是灾难）：StopAll 置 shuttingDown 后
//
//	**插件永远无法再加载**，那是进程退出用的，绝不能用于伪关机。
type PluginQuiescer interface {
	// StopPluginRuntime 运行态停掉一个插件（不改持久状态）。
	StopPluginRuntime(name string) error
	// StartPluginRuntime 运行态拉起一个插件。
	StartPluginRuntime(name string) error
	// RunningPlugins 返回当前正在运行的插件名（用于决定停哪些）。
	RunningPlugins() []string
}

// MigrateSession 表示一次"伪关机窗口"。用它包住外部迁移动作。
//
// ★ 用法必须用 defer 保证唤醒 —— 否则迁移失败时内核永久冻结：
//
//	sess, err := ks.BeginMigration(freezer)
//	if err != nil { return err }
//	defer sess.End()          // 无论成功失败都唤醒
//	// ... 外部脚本迁移 ...
type MigrateSession struct {
	ks      *KernelState
	plugins PluginQuiescer
	paused  []string
	stopped []string // 已运行态停掉的插件（按停止顺序，唤醒时逆序）
	ended   atomic.Bool
}

// BeginMigration 进入伪关机：停插件（运行态）→ 逆序暂停内核写者 → 等在途归零。
//
// 返回的 session 必须由调用方 `defer sess.End()`。
func (s *KernelState) BeginMigration(pq PluginQuiescer) (*MigrateSession, error) {
	sess := &MigrateSession{ks: s, plugins: pq}

	// ① 先停**插件**（运行态停启，可逆）。
	//
	// 为什么插件排第一：插件是最大的写入源（rss 写 feeds、calendar 写 events、
	// commondev 写 git 镜像）。而且 StopPlugin 会**摘掉注册面** ⇒ 停完
	// 模型看不到也调不到它们，比"只挡入口"更彻底。
	//
	// ★ 为什么不用 StopAll：它会置 shuttingDown，之后插件**永久无法加载**。
	//   那是进程退出路径用的，用于伪关机等于"关了就起不来"。
	if pq != nil {
		running := pq.RunningPlugins()
		// 逆序停（最后启动的先停，与依赖相反）
		for i := len(running) - 1; i >= 0; i-- {
			name := running[i]
			if err := pq.StopPluginRuntime(name); err != nil {
				// 停不下来 ⇒ 回滚已停的，别留半关
				for j := len(sess.stopped) - 1; j >= 0; j-- {
					if r := pq.StartPluginRuntime(sess.stopped[j]); r != nil {
						log.Printf("[migrate] rollback start plugin %s failed: %v", sess.stopped[j], r)
					}
				}
				return nil, fmt.Errorf("运行态停止插件 %s 失败（已回滚 %d 个）: %w",
					name, len(sess.stopped), err)
			}
			sess.stopped = append(sess.stopped, name)
		}
		log.Printf("[migrate] stopped %d plugin(s) at runtime (persistent state unchanged, restored on wake)", len(sess.stopped))
	}

	// ② 逆序暂停内核写者（distiller/textmem 等后台定时写）
	paused, err := s.Quiesce()
	if err != nil {
		// 失败 ⇒ 把插件拉回去，内核回到正常态
		for j := len(sess.stopped) - 1; j >= 0; j-- {
			if r := pq.StartPluginRuntime(sess.stopped[j]); r != nil {
				log.Printf("[migrate] rollback start plugin %s failed: %v", sess.stopped[j], r)
			}
		}
		log.Printf("[migrate] pseudo-shutdown failed, rolled back: %v", err)
		return nil, fmt.Errorf("伪关机失败: %w", err)
	}
	sess.paused = paused

	// ③ 等在途工具调用归零（尽力而为，见下）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !hasInflightTools() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hasInflightTools() {
		log.Printf("[migrate] warning: tool calls still in flight (continuing; safe after migration script stops the service)")
	}

	log.Printf("[migrate] pseudo-shutdown complete (%d plugins, %d kernel writers)", len(sess.stopped), len(paused))
	return sess, nil
}

// End 唤醒：逆序恢复内核写者 → 逆序拉起插件。幂等。
func (m *MigrateSession) End() {
	if m == nil || m.ended.Swap(true) {
		return
	}
	// ① 先恢复内核写者（它们是被插件依赖的下层）
	if err := m.ks.Resume(m.paused); err != nil {
		log.Printf("[migrate] * waking kernel writers failed (may be half-awake): %v", err)
	}
	// ② 再拉起插件（按**停止的逆序** = 原启动顺序）
	if m.plugins != nil {
		for i := len(m.stopped) - 1; i >= 0; i-- {
			name := m.stopped[i]
			if err := m.plugins.StartPluginRuntime(name); err != nil {
				log.Printf("[migrate] * starting plugin %s failed: %v", name, err)
			} else {
				log.Printf("[migrate] plugin started: %s", name)
			}
		}
	}
	log.Printf("[migrate] wake complete")
}

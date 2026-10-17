package kernelboot

import (
	"fmt"
	"log"
)

// 内核生命周期：**伪关机（Quiesce）** 与 **唤醒（Resume）**。
//
// ## 为什么需要它（用户 2026-10-17 要求）
//
// 数据目录迁移时**绝不能有模块在写数据**：distiller 每 10 分钟写一次
// memory/graph.db，text memory 滚动写 memory/text，tracker 在 overlay 里写，
// 40 个插件各写自己的 plugin_data。迁移 21G 数据要几分钟，期间任何写入
// 都会造成"迁移到一半的数据"或迁移后新旧不一致。
//
// 用户原话：
//
//	「迁移模块要先让 core 暂停其他模块运行（伪关机）完成迁移后再唤醒」
//
// ## 为什么不能用现成的 CloseAll
//
// `CloseAll` 是**一次性**的：它执行完就把 `Closers` 清空（切片置 nil），
// 且各模块的 Init 有副作用（建库、播种、迁移扫描），**不能简单重跑**。
// 用它做伪关机 = 关掉就再也起不来，等于要求重启进程 —— 那就不是"伪"关机了。
//
// ## 本文件的做法
//
// 分两级，都**可逆**：
//
//	Quiesce()  → 让所有"活动写者"停下来，但**保留连接/句柄**
//	Resume()   → 把停下来的重新启动
//
// 参与方式：模块在 Init 里通过 `AddQuiescer(name, pause, resume)` 登记。
// 与 CloseAll 一样**逆序**唤醒（LIFO）——后启动的先恢复，保证依赖顺序。
//
// ## ★ 关键约束（写进类型里）
//
//  1. pause 必须**幂等**且**不关闭文件/DB 句柄**（关了就无法 resume）
//  2. pause 必须**阻塞到确实不再写**（否则迁移仍有竞态）
//  3. resume 失败必须**可见**（返回错误），不能静默"半醒"
//  4. 只有**持有写句柄**的模块才需要登记 —— 纯读模块不必（如 config）
type Quiescer struct {
	// Name 用于日志与诊断（哪个模块停不下来，一眼看到）。
	Name string
	// Pause 停下活动写入并**等到确实停了**才返回。
	Pause func() error
	// Resume 恢复写入。失败必须返回错误（不能假装成功）。
	Resume func() error
}

// AddQuiescer 登记一个可暂停/恢复的写者（在模块 Init 里调用）。
func (s *KernelState) AddQuiescer(name string, pause, resume func() error) {
	if pause == nil {
		return
	}
	s.quiesceMu.Lock()
	s.Quiescers = append(s.Quiescers, Quiescer{Name: name, Pause: pause, Resume: resume})
	s.quiesceMu.Unlock()
}

// Quiesce 伪关机：**逆序**暂停全部已登记的写者。
//
// 返回成功暂停的清单 —— Resume 只唤醒这些（没有的不要"凭空唤醒"）。
// 任何一个 Pause 失败都会**停止继续暂停**并返回错误：宁可只关一半
// 让调用方放弃迁移，也不要"看起来关了但仍有写者在跑"。
func (s *KernelState) Quiesce() ([]string, error) {
	s.quiesceMu.Lock()
	qs := make([]Quiescer, len(s.Quiescers))
	copy(qs, s.Quiescers)
	s.quiesceMu.Unlock()

	// 逆序暂停：后启动的先停（与启动顺序相反，先停"消费者"再停"生产者"）
	var paused []string
	for i := len(qs) - 1; i >= 0; i-- {
		q := qs[i]
		if err := q.Pause(); err != nil {
			// 已暂停的按**反序**回滚（把它唤醒回去），别留半关状态
			for j := len(paused) - 1; j >= 0; j-- {
				if r := s.resumeOne(qs, paused[j]); r != nil {
					log.Printf("[quiesce] 回滚唤醒 %s 失败: %v", paused[j], r)
				}
			}
			return nil, fmt.Errorf("暂停 %s 失败（已回滚 %d 个）: %w", q.Name, len(paused), err)
		}
		paused = append(paused, q.Name)
		log.Printf("[quiesce] 已暂停: %s", q.Name)
	}
	return paused, nil
}

// Resume 唤醒：按**暂停的逆序**（即原启动顺序）恢复。
func (s *KernelState) Resume(paused []string) error {
	s.quiesceMu.Lock()
	qs := make([]Quiescer, len(s.Quiescers))
	copy(qs, s.Quiescers)
	s.quiesceMu.Unlock()

	var firstErr error
	for i := len(paused) - 1; i >= 0; i-- {
		if err := s.resumeOne(qs, paused[i]); err != nil {
			log.Printf("[quiesce] 唤醒 %s 失败: %v", paused[i], err)
			if firstErr == nil {
				firstErr = fmt.Errorf("唤醒 %s: %w", paused[i], err)
			}
		} else {
			log.Printf("[quiesce] 已唤醒: %s", paused[i])
		}
	}
	return firstErr
}

func (s *KernelState) resumeOne(qs []Quiescer, name string) error {
	for _, q := range qs {
		if q.Name == name {
			if q.Resume == nil {
				return nil
			}
			return q.Resume()
		}
	}
	return fmt.Errorf("未登记的写者: %s", name)
}

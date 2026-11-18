package agent

// archive.go —— 写前留档的**转发**（契约在 kernelapi，实现在安全模块）。
//
// ## 为什么只留一个转发函数
//
// 基座不该 import `internal/security`：那一条 import 就等于把"基座"与
// "安全实现"焊在一起（判据 §8 第 3 条）。留档本身是**数据面**动作
// （写文件前先备份），它的契约 `kernelapi.ArchiveFunc` 属于契约层，
// 实现仍在安全模块，接线在装配点（`kernelmod/agent`）。
//
// nil（未装配 / 单测）⇒ 返回"没留档"，调用方照常继续 ——
// 与"安全模块未启用"同一口径（那里沙箱也是关的）。

import "fmt"

// archiveBeforeWrite 在写文件前留一份备份（返回备份路径）。
func (a *Agent) archiveBeforeWrite(path string) (bool, error) {
	if a == nil {
		return false, nil
	}
	g := a.Gate()
	if g == nil {
		return false, nil
	}
	return g.Archive(a.dataDir, path)
}

var _ = fmt.Sprintf

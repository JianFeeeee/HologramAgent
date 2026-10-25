package proc

import (
	"log"
	"os"
	"strings"
	"sync"
)

// ShmSecurityMode 控制共享内存段的可见性与审计强度。
//
// 设计前提（已确认）：
//   - Unix（Linux/macOS）默认走匿名 memfd / unlink 临时文件，经 fork 继承 fd。
//     没有文件系统名字，只有内核主动 spawn 的子进程能映射 → 安全几乎免费。
//   - Windows 没有 fd 继承语义，段是命名 FileMapping，任何拿到名字的进程都能打开。
//     此处安全边界弱，需要额外措施。
//
// 映射进程数 ≠ 加载插件数：它不是安全判据，只作异常告警。
// 合法时也可能不等（一个插件映射两次、debug 工具挂载），非法时可能恰好相等。
// 它的价值在「发现泄漏句柄或未授权挂载」，不在「授权访问」。
type ShmSecurityMode string

const (
	// ShmModeSafe 默认。Unix 匿名 memfd（fork 继承即唯一访问）；
	// Windows 命名段用 crypto 随机 nonce 名 + 仅记录期望子 PID。
	ShmModeSafe ShmSecurityMode = "safe"
	// ShmModeDebug 暴露 /proc 反查与稳定名字，便于 strace/lldb 挂载。
	ShmModeDebug ShmSecurityMode = "debug"
	// ShmModeFull 允许命名段（/dev/shm 或固定名），多实例可共享。
	ShmModeFull ShmSecurityMode = "full"
)

const envShmSecurityMode = "HOMEAGENT_SHM_SECURITY"

var (
	shmModeOnce sync.Once
	shmModeVal  ShmSecurityMode
)

// ShmSecurityMode 返回当前模式（从 HOMEAGENT_SHM_SECURITY 读，默认 safe）。
func ShmSecurityModeOf() ShmSecurityMode {
	shmModeOnce.Do(func() {
		shmModeVal = ShmModeSafe
		switch strings.ToLower(strings.TrimSpace(os.Getenv(envShmSecurityMode))) {
		case "debug":
			shmModeVal = ShmModeDebug
		case "full":
			shmModeVal = ShmModeFull
		}
	})
	return shmModeVal
}

// ShmAudit 记录一次映射数审计的期望与实测。
//
// expectedPlugins 是内核已 spawn 且握手的插件子进程数；actualMappings 是
// 当前能观测到的、映射了本段的对象数（Linux 下读 /proc，Windows 计数句柄）。
// delta = actual - expected > 0 时可能有未授权挂载或泄漏句柄。
//
// 本结果只用于告警，不用于授权决策：真正阻止未授权访问的是 Unix 匿名 memfd
// + 仅 fork 继承，而非计数相等。
type ShmAudit struct {
	ExpectedPlugins int
	ActualMappings  int
	Delta           int
}

// AuditShmMappings 在 Host 上做一次映射数审计。
//
// 平台无探针时返回 ok=false（调用方应跳过告警，不要误报）。
func (h *Host) AuditShmMappings(expectedPlugins int) (ShmAudit, bool) {
	a := ShmAudit{ExpectedPlugins: expectedPlugins}
	var ok bool
	a.ActualMappings, ok = h.countOurShmMappings()
	a.Delta = a.ActualMappings - a.ExpectedPlugins
	if ok && a.Delta > 0 {
		log.Printf("[proc] shared memory mapping audit: expected %d actual %d (+delta=%d) -- possible leaked handles or unauthorized mappings",
			a.ExpectedPlugins, a.ActualMappings, a.Delta)
	}
	return a, ok
}

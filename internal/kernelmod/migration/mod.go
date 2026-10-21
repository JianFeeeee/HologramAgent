// Package migration 提供**数据目录迁移**的内核能力（伪关机 → 迁移 → 唤醒）。
//
// ## 它提供什么
//
// 两个工具（暴露给模型/WebUI）：
//
//	migrate_data_preflight  → 只读体检：报告能不能迁、要迁多少、有哪些风险
//	migrate_data_plan       → 生成可执行迁移脚本（伪关机窗口由脚本自己 stop 覆盖）
//
// ## ★ 为什么不提供"一键迁移"工具
//
// 迁移是**破坏性且不可逆**的（21G 数据 + 12 处 DB 改写 + 46 个文件改写），
// 而且内核跑在 systemd 沙箱里（ProtectSystem/ProtectHome）**本来就写不了
// /etc/systemd**，更没法把 /home/newqqagent 搬到别的挂载点（跨目录 rename 会失败，
// 必须 rsync 全量）。
//
// ⇒ 内核的职责是：**体检 + 生成正确的脚本 + 提供伪关机/唤醒能力**；
//
//	真正的搬数据由运维执行脚本。这样每一步都可审查、可回滚。
//
// ## 依赖声明
//
// Requires 里**故意不含** plugin-host：本模块只读 fs + 读 config，
// 伪关机能力（PluginQuiescer）由调用方在需要时注入，不硬依赖。
package migration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/systemd"
)

func init() { kernelmod.Register(Module{}) }

// Module 是数据目录迁移模块。
type Module struct{}

func (Module) Name() string { return "migration" }

// Requires 只需要配置（读 data_dir）与 kernel-io（日志）。
func (Module) Requires() []string { return []string{"config", "kernel-io"} }

func (Module) Provides() []string { return []string{"migration"} }

// MigrateReport 是体检结果。
type MigrateReport struct {
	DataDir    string    `json:"data_dir"`
	TotalBytes int64     `json:"total_bytes"`
	Dirs       []DirInfo `json:"dirs"`
	Mounts     []string  `json:"mounts"`       // 必须先卸载的挂载点
	Risks      []string  `json:"risks"`        // 会导致数据丢失/错误的点
	DBPathKeys []string  `json:"db_path_keys"` // 数据库里存绝对路径的键
	SameDevice bool      `json:"same_device"`  // 目标与源是否同一设备（决定能否 rename）
	CanRename  bool      `json:"can_rename"`   // 同设备可直接 rename，否则必须 rsync
	ScriptPath string    `json:"script_path,omitempty"`
}

// DirInfo 是单目录的信息。
type DirInfo struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Kind  string `json:"kind"` // data | runtime | optional | mount
}

func (Module) Init(ks *kernelboot.KernelState) error {
	// 体检结果放在 KernelState 上，供工具/WebUI 读（可选能力，nil 安全）
	ks.MigrationPreflight = func(target string) (interface{}, error) {
		return Preflight(ks.DataDir, target)
	}
	return nil
}

// Preflight 体检：数据目录现在是什么状态、迁到 target 需要注意什么。
func Preflight(dataDir, target string) (*MigrateReport, error) {
	rep := &MigrateReport{DataDir: dataDir}

	// 目录分类（来自 2026-10-17 生产盘点，21G）
	// data=必须迁 runtime=不迁（重启重建） optional=可丢 mount=挂载点需先卸
	classify := map[string]string{
		"agentfs":          "mount",
		"plugindev":        "data",
		"blocklauncher":    "data",
		"browser_profiles": "data",
		"models":           "data",
		"plugins":          "data",
		"mcbridge":         "data",
		"memory":           "data",
		"plugin_data":      "data",
		"knowledge":        "data",
		"skills":           "data",
		"mcp":              "data",
		"nlp":              "data",
		"adapters":         "data",
		"data":             "data",
		"uploads":          "data",
		"device_media":     "data",
		"webui_files":      "data",
		"changesets":       "data",
		"heartbeat":        "runtime",
		"log":              "optional",
		"backups":          "optional",
		"memory.bak":       "optional",
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("读数据目录: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		info := DirInfo{Name: name, Kind: classify[name]}
		if info.Kind == "" {
			info.Kind = "data" // 未知目录默认**必须迁**（宁可多迁，不可漏迁）
		}
		info.Bytes = dirSize(filepath.Join(dataDir, name))
		rep.Dirs = append(rep.Dirs, info)
		rep.TotalBytes += info.Bytes
	}
	sort.Slice(rep.Dirs, func(i, j int) bool { return rep.Dirs[i].Bytes > rep.Dirs[j].Bytes })

	// ① 挂载点：agentfs 是 overlay（lower/upper/work），不卸就搬空壳。
	//
	// ★ 风险说明**无条件**报出，只有"要不要 umount"取决于实际挂载状态。
	//
	// 为什么分开：如果只在挂载时才提风险，那"现在没挂、迁移脚本跑起来时
	// 服务又把它挂上了"这种时序就完全没提示 —— 而 agentfs 恰恰是内核
	// 启动时才挂的。运维在**停机状态**做体检会看不到这条警示，
	// 但迁移脚本里的 stop 又会把它挂回来（见脚本的 umount 步骤）。
	// 判据 TestPreflightReportsKnownRisks 就是为了钉住这条。
	if _, err := os.Stat(filepath.Join(dataDir, "agentfs")); err == nil {
		merged := filepath.Join(dataDir, "agentfs", "merged")
		if isMountpoint(merged) {
			rep.Mounts = append(rep.Mounts, merged)
		}
		rep.Risks = append(rep.Risks,
			"agentfs/merged 是 overlay 挂载点（lower/upper/work 三段）：不先 umount 会搬走'空壳'，"+
				"真正内容仍挂在旧路径 ⇒ 新目录 agentfs 不可用。"+
				"注意：内核启动时才挂它，所以**停机体检看不到挂载**也仍要卸")
	}

	// ② 数据库绝对路径：不改会"看起来记忆全丢"（内核按旧路径找不到就重建空库，且不报错）
	rep.DBPathKeys = []string{
		"core.daemon.data_dir", "core.plugin.dir", "core.memory.graph",
		"core.memory.text", "core.memory.documents", "core.knowledge.path",
		"core.log.path", "core.memory.multimodal_space.options.model_dir",
	}
	rep.Risks = append(rep.Risks,
		fmt.Sprintf("数据库有 %d 个键存绝对路径；不改写会导致重启后按旧路径找数据，"+
			"找不到就**静默重建空库**（看起来像记忆全丢）", len(rep.DBPathKeys)),
		"plugin_data 下实测有 46 个文件写死了绝对路径（典型：commondev 的 git 镜像/workdir）",
		"security.key 不迁 ⇒ 所有加密配置解不开（敏感信息全部失效）",
	)

	// ③ 是否同设备：决定 rename（瞬时）还是 rsync（几分钟）
	if target != "" {
		if src, err := os.Stat(dataDir); err == nil {
			if dst, err := os.Stat(filepath.Dir(target)); err == nil {
				a := src.Sys().(*syscall.Stat_t).Dev
				b := dst.Sys().(*syscall.Stat_t).Dev
				rep.SameDevice = a == b
				rep.CanRename = rep.SameDevice
				if !rep.SameDevice {
					rep.Risks = append(rep.Risks,
						"目标不在同一设备 ⇒ 不能用 rename，必须 rsync 全量拷贝（耗时随数据量增长）")
				}
			}
		}
	}

	// ④ 当前 systemd 权威 vs DB 权威是否一致（两个权威不等价）
	// 运行时权威 = -data（本模块的 dataDir）；配置权威 = DB core.daemon.data_dir
	if st, err := systemd.Check(dataDir, dataDir); err == nil {
		if st.RuntimeDataDir != "" && st.ConfigDataDir != "" && st.RuntimeDataDir != st.ConfigDataDir {
			rep.Risks = append(rep.Risks,
				fmt.Sprintf("★ 数据目录有**两个权威且不一致**：-data=%s（管 memory/graph.db + 插件 DataDir）"+
					" vs DB core.daemon.data_dir=%s（管 memory/text、knowledge、nlp 等 6 个模块）⇒ 数据会分裂",
					st.RuntimeDataDir, st.ConfigDataDir))
		}
	}
	return rep, nil
}

// dirSize 递归统计目录大小（符号链接不跟随，避免重复计算硬链接/环）。
func dirSize(root string) int64 {
	var total int64
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// isMountpoint 判断路径是否为挂载点（读 /proc/mounts，不依赖外部命令）。
func isMountpoint(p string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	clean := filepath.Clean(p)
	for _, line := range splitLines(string(data)) {
		f := splitFields(line)
		if len(f) >= 2 && unescapeMount(f[1]) == clean {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func splitFields(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		j := i
		for j < len(s) && s[j] != ' ' {
			j++
		}
		if j > i {
			out = append(out, s[i:j])
		}
		i = j
	}
	return out
}

// unescapeMount 还原 /proc/mounts 里的八进制转义（空格写作 \040）。
func unescapeMount(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			v := 0
			ok := true
			for k := 1; k <= 3; k++ {
				c := s[i+k]
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				out = append(out, byte(v))
				i += 3
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}

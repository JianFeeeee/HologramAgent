package webui

// 数据目录一致性端点。
//
// ## 为什么需要它
//
// 内核有**两处**数据目录来源，且不等价：
//
//	-data <dir>            ← systemd 单元，决定 ks.DataDir
//	core.daemon.data_dir   ← 数据库，决定 ks.Cfg.Daemon.DataDir
//
// 不一致时数据会**分裂**：图记忆（memory/graph.db）在 -data 那个目录，
// 而文本/知识/文档在 DB 那个目录 ⇒ 检索跨目录、备份会漏。
//
// 生产当前两者一致，所以从没暴露；但 WebUI 允许改 core.daemon.data_dir
// 而**没有任何提示** —— 用户改完重启就会发现"数据丢了"。
//
// ## 只读 + 给命令，绝不自动改
//
// 本端点只返回诊断与**可直接粘贴执行的修复命令**（里面已内联 sed 替换）。
// 不代用户改 systemd 单元：静默改单元比不提示更危险 —— 改错了服务起不来，
// 而用户不知道是谁改的。修复必须由人执行。

import (
	"net/http"

	"github.com/JianFeeeee/HologramAgent/internal/systemd"
)

// dataDirs 取两个数据目录来源。
//
//	runtime ← SettingsAPI.DataDir()（= -data，systemd 单元给的那个）
//	config  ← ConfigAPI.Get().Daemon.DataDir（= DB 里 core.daemon.data_dir）
//
// 任一不可用就返回空串 —— 调用方据此判断"无法判定"，而不是拿空串
// 去比出"不一致"的假警报。
func (h *Handler) dataDirs() (runtimeDir, configDir string) {
	if h.settings != nil {
		runtimeDir = h.settings.DataDir()
	}
	if h.config != nil {
		if cfg := h.config.Get(); cfg != nil {
			configDir = cfg.Daemon.DataDir
		}
	}
	return runtimeDir, configDir
}

// handleDataDirStatus 处理 GET /api/v1/system/data-dir
//
// 返回：
//
//	runtime_dir   当前进程实际用的（-data）
//	config_dir    数据库里 core.daemon.data_dir
//	in_sync       两者是否一致
//	describe      人读的一句话说明
//	sync_command  不一致时的修复命令（内联 sed，可直接复制执行）
func (h *Handler) handleDataDirStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "只接受 GET")
		return
	}
	runtimeDir, configDir := h.dataDirs()
	st := systemd.State{
		RuntimeDataDir: runtimeDir,
		ConfigDataDir:  configDir,
	}
	out := map[string]interface{}{
		"runtime_dir":   runtimeDir,
		"config_dir":    configDir,
		"in_sync":       st.InSync(),
		"describe":      st.Describe(),
		"under_systemd": systemd.IsUnderSystemd(),
	}
	if !st.InSync() {
		// 只在真正不一致时给命令 —— 一致时给命令会让人以为该做点什么
		out["sync_command"] = st.SyncCommand(configDir)
		out["warning"] = "数据目录不一致：图记忆在 runtime_dir，文本/知识/文档在 config_dir。" +
			"不修复则检索跨目录、备份会漏。请执行 sync_command（勿手改单元，生产用 drop-in 覆盖 ExecStart）。"
	}
	writeJSON(w, http.StatusOK, out)
}

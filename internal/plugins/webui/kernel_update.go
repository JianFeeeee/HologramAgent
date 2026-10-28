package webui

// 内核更新的 HTTP 端点（GET 检查 / POST 升级）。
//
// ## 三端共享同一底层（架构约束）
//
// agent 的 kernel_update 工具、本端点、CLI（homed-upgrade）三者共享
// internal/update 的 CheckRelease / Apply —— 三份实现必然漂移，会出现
// "WebUI 说有新版、agent 说没有"的分裂状态。
// 本文件只做 HTTP 形状转换，不含业务逻辑。
//
// ## 包来源二选一
//
//	{"release": true}                  → 从仓库最新 GitHub Release 自动下载
//	{"pkg": "/path/to/pkg.tar.gz"}     → 用本机已有的安装包（优先）
//
// ## 升级 = 换产物 + 重启，两件事**分开做**
//
//	Apply（本进程内，同步）  ：校验 → 备份 → pending → 原子替换
//	重启（交给内核控制通道）  ：kernelCtl.RequestKernelRestart → exit 42
//	                           → guard 用新二进制重拉（不计失败轮次）
//
// ★ 为什么 Apply 必须同步返回：替换是**可失败**的动作（空间不足、权限、
// 包损坏）。异步的话这些错误全被吞掉，用户只看到"已受理"然后毫无变化。
// 而重启可以异步 —— 它让本进程消失，本来就没法回给它结果。
//
// ★ 为什么不用 systemctl restart：见 cmd/homed/kernel_control.go ——
// worker 是被守护方，只有 guard 知道这次退出该不该重拉。

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/update"
)

// handleKernelUpdate 是 /api/v1/kernel/update 的统一入口（GET 检查 / POST 升级）。
func (h *Handler) handleKernelUpdate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.handleKernelUpdateCheck(w, r)
	case http.MethodPost:
		h.handleKernelUpdateApply(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "只接受 GET（检查）或 POST（升级）")
	}
}

// handleKernelUpdateCheck 处理 GET —— 只检查不改动。
func (h *Handler) handleKernelUpdateCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, update.CheckRelease(currentKernelVersion()))
}

// handleKernelUpdateApply 处理 POST /api/v1/kernel/update —— 执行升级。
func (h *Handler) handleKernelUpdateApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pkg     string `json:"pkg"`
		Release bool   `json:"release"`
	}
	// body 可为空（不合法就在下面报"必须指定来源"），所以解码失败不单独报错。
	_ = json.NewDecoder(r.Body).Decode(&body)

	src, err := update.ParseSource(body.Pkg, body.Release)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	binPath := update.KernelBinaryPath()

	tmp, err := os.MkdirTemp("", "kernel-upgrade-*")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "建临时目录失败: "+err.Error())
		return
	}
	defer os.RemoveAll(tmp)

	root, rel, err := src.Resolve(tmp)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "取包失败: "+err.Error())
		return
	}
	newDG := update.FindFile(root, "depgraph.bin")

	// WebUI 侧天然有总线（它就是 SSE 的上游）：升级每一步都让界面看得到
	var bus *events.Bus
	if h.sdk != nil {
		bus = h.sdk.EventBus()
	}

	out, err := update.Apply(root, binPath, newDG, dataDirOf(binPath), bus)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	res := map[string]interface{}{
		"applied": true,
		"from":    out.From.Version,
		"to":      out.To.Version,
		"backup":  out.BackupDir,
		"message": "新内核已就位，正在重启。新版本启动后由第 0 层 update 模块确认；无法启动会自动回滚。",
	}
	if rel != nil {
		res["release"] = rel.Latest
		res["asset"] = rel.Asset
	}

	// 先把响应写回，再让内核消失（同 kernel/restart 的教训：不 Flush 会
	// 变成 connection reset，用户以为出错了）。
	writeJSON(w, http.StatusOK, res)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 重启走**既有**内核控制通道（restartCh → exit 42 → guard 用新二进制重拉）。
	// 不自己 systemctl，也不另开通道。
	if h.kernelCtl == nil {
		log.Printf("[webui] 新内核已就位，但内核控制通道不可用（kernelControlAllowed 未授予 webui），无法请求重启")
		return
	}
	if err := h.kernelCtl.RequestKernelRestart("kernel_update apply"); err != nil {
		log.Printf("[webui] kernel restart request failed (新内核已就位，需手动重启): %v", err)
	}
}

// currentKernelVersion 读本机 homed 的版本（尽力而为）。
func currentKernelVersion() *update.BinVersion {
	bin := update.KernelBinaryPath()
	if _, err := os.Stat(bin); err != nil {
		return nil
	}
	v, err := update.ReadBinVersion(bin)
	if err != nil {
		return nil
	}
	return v
}

// dataDirOf 推导数据目录（升级确认标记的落点）。
//
// 与内核 `--data` 必须一致，否则第 0 层 update 模块读不到 pending 标记。
// 优先用环境变量显式指定（部署方知道自己在哪），否则退回本机生产路径。
func dataDirOf(binPath string) string {
	if d := os.Getenv("HOMEAGENT_DATA_DIR"); d != "" {
		return d
	}
	// ★ 不写死生产路径：迁移后它会指向一个不存在的目录，
	// 让升级 pending 标记落错地方（静默不回滚）。返回空由调用方报错。
	return ""
}

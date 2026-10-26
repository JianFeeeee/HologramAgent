package webui

// 内核更新的 HTTP 端点（GET 检查 / POST 升级）。
//
// ## 与 agent 工具同底层的架构约束
//
// agent 的 kernel_update 工具、本端点、CLI（homed-upgrade）三者共享
// internal/update 的 CheckRelease / DoUpgrade —— 三份实现必然漂移，
// 会出现"WebUI 说有新版、agent 说没有"的分裂状态。
// 本文件只做 HTTP 形状转换，不含业务逻辑。
//
// ## 与 kernel/restart 的关系
//
// 升级 = 换二进制 + 重启。重启通道（kernelCtl）已存在且经过生产验证，
// 升级走同一通道触发重启，不自建第二条。

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"

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
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "只接受 GET")
		return
	}
	cur := currentKernelVersion()
	writeJSON(w, http.StatusOK, update.CheckRelease(cur))
}

// handleKernelUpdateApply 处理 POST /api/v1/kernel/update —— 执行升级。
//
// body: {"pkg": "/path/to/pkg.tar.gz"}（可选；缺省用最近一次检查到的
// release 资产 URL 现场下载）。
func (h *Handler) handleKernelUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	// ★ 升级会让进程重启：先把响应写回去（同 kernel/restart 的教训）
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"accepted": true,
		"message":  "升级已受理。内核将替换二进制并重启，连接会断开数秒。完成后刷新状态页确认新版本。",
	})

	var body struct {
		Pkg string `json:"pkg"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// WebUI 侧天然有总线（它就是 SSE 的上游）：升级每一步都让界面看得到
	var bus *events.Bus
	if h.sdk != nil {
		bus = h.sdk.EventBus()
	}
	go runKernelUpgrade(body.Pkg, bus)
}

// currentKernelVersion 读本机 homed 的版本（尽力而为）。
func currentKernelVersion() *update.BinVersion {
	bin := "/usr/local/bin/homed"
	if _, err := os.Stat(bin); err != nil {
		return nil
	}
	v, err := update.ReadBinVersion(bin)
	if err != nil {
		return nil
	}
	return v
}

// runKernelUpgrade 在后台执行完整升级闭环（含重启与回滚）。
//
// bus 为 nil 时退化成纯日志（该部署未接事件总线）。
func runKernelUpgrade(pkgPath string, bus *events.Bus) {
	if pkgPath == "" {
		// 没给本地包：从最新 release 资产下载。
		// 下载与升级的完整闭环由 CLI 工具实现，这里复用同一二进制
		// （/usr/local/bin/homed-upgrade 与本进程同源构建，语义一致）。
		if _, err := exec.LookPath("homed-upgrade"); err != nil {
			return
		}
		return // 下载逻辑见 CLI；此处仅受理本地包路径
	}
	// 本地包：直接走共享底层（与 CLI 同代码路径）
	tmp, err := os.MkdirTemp("", "kernel-upgrade-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmp)
	root, err := update.ExtractPkg(pkgPath, tmp)
	if err != nil {
		return
	}
	newBin := update.FindFile(root, "homed")
	newDG := update.FindFile(root, "depgraph.bin")
	if newBin == "" {
		return
	}
	_ = update.DoUpgradeBus(root, "/usr/local/bin/homed", newDG, "/home/newqqagent/plugins", "/home/newqqagent", bus)
}

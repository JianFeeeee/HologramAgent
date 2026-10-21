package webui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"encoding/json"
	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/meta"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
	"net/http"
	"path/filepath"
)

// 内核与代理面：状态、kernel 快照、人格、代理列表/快照/回滚/动作。

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agentCount := 0
	if h.supervisor != nil {
		agentCount = len(h.supervisor.ListAgents())
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "running",
		"uptime": time.Since(h.startTime).Round(time.Second).String(),
		"agents": agentCount,
		// 内核版本取 internal/meta（-ldflags 注入点）。
		//
		// 此前这里给的是 `sdk.SDKVersion`，那条链最终指向 **SDK 仓
		// meta.Version 的硬编码值**，与构建时注入的内核版本无关——
		// 两仲版本号碰巧相等时看不出问题，一旦不等就报错。
		// SDK 版本另用 sdk_version 字段并行暴露。
		"version":     meta.Version,
		"commit":      meta.Commit,
		"build_time":  meta.BuildTime,
		"sdk_version": sdk.SDKVersion,
		"startedAt":   h.startTime,
	})
}

func (h *Handler) handleKernel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.status == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "kernel status provider not available"})
		return
	}
	writeJSON(w, http.StatusOK, h.status.GetKernelStatus())
}

// 人格设定：配置项键，以及「首启向导已经问过」的一次性标记。
//
// 为什么需要向导：人格曾经只有 <dataDir>/personal/personal.md 一个来源且无人维护，
// 里面写死的旧版本号反过来让实例自述旧版本（v1.2.0 压测发现）。
// 现在人格是配置项（默认模板不含任何版本号），首启问一次，之后不再打扰。
// handlePersona 是首启人格向导的后端（与内核 persona_set 工具共用 internal/config 的实现）。
//
//	GET  → {initialized, current_prompt, file_override}
//	POST → {"mode":"default"|"custom"|"later","content":"..."}
//	       写入 core.agent.personal_prompt 并打一次性标记，返回 restart_required
//
// 生效时机：人格在 homed 启动时载入（以【人格设定】块拼进系统提示词），
// 所以**自定义内容需重启生效**；选「默认」或「稍后」（保持当前默认）无需重启。
// 不回答就是「稍后」：保留默认并打标记，不阻塞任何流程。
//
// 跨通道：这里只是 WebUI 侧的入口；任何通道的消息到来时，内核都会检查同一枚标记，
// 未确认则在提示词里要求模型主动询问（见 internal/agent/core 的首启门禁）。
func (h *Handler) handlePersona(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "settings not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"initialized":    internalConfig.PersonaInitializedKV(h.settings),
			"current_prompt": internalConfig.CurrentPersonaKV(h.settings),
			"file_override":  h.personaFileExists(),
		})
	case http.MethodPost:
		var req struct {
			Mode    string `json:"mode"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		restart, err := internalConfig.SetPersonaKV(h.settings, req.Mode, req.Content)
		if err != nil {
			// 非法 mode / 空内容 → 400；落库失败 → 500。两者都不打标记。
			code := http.StatusInternalServerError
			if strings.Contains(err.Error(), "unknown mode") || strings.Contains(err.Error(), "content required") {
				code = http.StatusBadRequest
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "ok", "mode": req.Mode, "restart_required": restart,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// personaFileExists 报告是否存在会覆盖配置项的人格文件（存在时它优先）。
// 数据目录取自 core.daemon.data_dir（由播种写入）。
func (h *Handler) personaFileExists() bool {
	v, err := h.settings.GetCore("core.daemon.data_dir")
	if err != nil {
		return false
	}
	dir, _ := v.(string)
	if dir == "" {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, "personal", "personal.md"))
	return err == nil
}

func (h *Handler) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if h.supervisor == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "supervisor not available"})
			return
		}
		agents := h.supervisor.ListAgents()
		writeJSON(w, http.StatusOK, map[string]interface{}{"agents": agents})
	case http.MethodPost:
		var cfg types.AgentConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if cfg.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent id is required"})
			return
		}
		if h.config != nil {
			kcfg := h.config.Get()
			kcfg.Agents = append(kcfg.Agents, cfg)
			h.config.Put(kcfg)
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": string(cfg.ID)})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/agents/")
	parts := strings.Split(path, "/")
	agentID := types.AgentID(parts[0])
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			if h.supervisor == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "supervisor not available"})
				return
			}
			status, err := h.supervisor.GetAgentStatus(string(agentID))
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, status)
		case http.MethodDelete:
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "id": string(agentID)})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	action := parts[1]
	switch action {
	case "snapshots":
		h.handleSnapshots(w, r, agentID, parts)
	case "rollback":
		h.handleRollback(w, r, agentID, parts)
	case "start", "stop", "restart":
		h.handleAgentAction(w, r, agentID, action)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *Handler) handleSnapshots(w http.ResponseWriter, r *http.Request, agentID types.AgentID, parts []string) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"agent_id": agentID, "snapshots": []map[string]interface{}{}})
	case http.MethodPost:
		if h.supervisor == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "supervisor not available"})
			return
		}
		snap, err := h.supervisor.PreActionSnapshot(string(agentID))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, snap)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleRollback(w http.ResponseWriter, r *http.Request, agentID types.AgentID, parts []string) {
	if r.Method != http.MethodPost || len(parts) < 3 {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.supervisor == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "supervisor not available"})
		return
	}
	snapID := types.SnapshotID(parts[2])
	if err := h.supervisor.RollbackAgent(string(agentID), string(snapID)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rollback_initiated", "agent": string(agentID), "snap": string(snapID)})
}

// handleAgentAction 处理 start/stop/restart。
//
// ★ restart 是**热更新入口**（此前是 501 桩）：
// 换掉 /usr/local/bin/homed 之后调它，worker 会以退出码 42 优雅退出，
// guard 随即用**新二进制**重新拉起 —— 不必 systemctl restart，
// 也不打断 guard（双进程架构的父进程）。
//
// stop/start 仍返回 501：它们的语义在「单 worker + guard 自动拉起」
// 架构下没有独立意义（stop 之后 guard 立刻会把它拉回来）。
func (h *Handler) handleAgentAction(w http.ResponseWriter, r *http.Request, agentID types.AgentID, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch action {
	case "restart":
		if h.supervisor == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "supervisor not available"})
			return
		}
		if err := h.supervisor.Restart(string(agentID)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "restarting",
			"agent":  string(agentID),
			"note": "worker 将优雅退出（exit 42），guard 随后用当前二进制重新拉起；" +
				"若刚替换过 /usr/local/bin/homed，新版本即在此刻生效",
		})
	default:
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": fmt.Sprintf("agent %s action not implemented by supervisor", action)})
	}
}

// handleRuntime 返回**只含运行态**的小快照：调度器（排队/四级中断队列/中断栈）、
// 驻留子 agent、通道拓扑。
//
// 为什么不复用 /api/v1/kernel：那份是 30KB 级的全量状态（工具、插件、记忆、LLM…），
// 拿它做秒级刷新既费带宽也费端上算力。运行态要能「实时看」，所以单开一条
// 只读通道，字段直接来自内核暴露的 KernelStatus（internal/sdk），
// 这样任何应用（WebUI / GUI / ArkTS）都能拿同一份数据做展示。
func (h *Handler) handleRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.status == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "kernel status not available"})
		return
	}
	ks := h.status.GetKernelStatus()
	if ks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "kernel status not available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"uptime": ks.Uptime,
		// agent_id 是**根 agent 的 id**。前端靠它把 inputch 的 owner 归类：
		// 归属登记表里根 agent 的 id（如 "main"）与驻留子 id 长得一样，
		// 不区分就会把根自己的通道标成「驻留子 main」。
		"agent_id":  ks.AgentID,
		"scheduler": ks.Scheduler,
		"residents": ks.Residents,
		"channels":  ks.Channels,
		// inputch 的登记与归属（谁注册、划给了哪个 agent、容量、默认回程）。
		// 只给设备能力（channels）无法回答「这条输入归谁」——驻留子存在时这就是缺口。
		"input_channels": ks.InputChannels,
	})
}

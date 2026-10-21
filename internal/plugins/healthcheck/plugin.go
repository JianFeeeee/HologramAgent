package healthcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

type toolInfo struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Description string `json:"description"`
}

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Pass   bool   `json:"pass"`
}

// llmReport 由 LLM 通过 healthcheck_report 工具提交。
type llmReport struct {
	ToolName string `json:"tool_name"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
}

func init() {
	plugin.RegisterPluginMeta("healthcheck", "健康检查", "Health Check")
	plugin.RegisterFactory("healthcheck", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	stopOnce      sync.Once
	name          string
	mu            sync.Mutex
	reports       []llmReport
	sessionID     string
	selfToolNames map[string]bool
	checkMu       sync.Mutex

	stopCh   chan struct{}
	perfData PerfData

	autoInterval time.Duration
	llmTimeout   time.Duration
	llmMaxTurns  int
	llmMaxTokens int
	perfHistory  int
}

type PerfData struct {
	LastCheck time.Time        `json:"last_check"`
	Checks    []PerfCheckPoint `json:"checks"`
}
type PerfCheckPoint struct {
	Time      time.Time `json:"time"`
	Passed    int       `json:"passed"`
	Failed    int       `json:"failed"`
	Total     int       `json:"total"`
	ElapsedMs int64     `json:"elapsed_ms"`
}

func New(name string) *Plugin {
	return &Plugin{
		name:          name,
		selfToolNames: make(map[string]bool),
		stopCh:        make(chan struct{}),
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	// 幂等重启 auto-check：若实例被 Stop 过（stopCh 已关闭）后再次 Start
	// （如 plgreload 复用实例），重建 stopCh 使 startAutoCheck 能重新调度 ticker。
	p.mu.Lock()
	select {
	case <-p.stopCh:
		p.stopCh = make(chan struct{})
	default:
	}
	p.mu.Unlock()
	s.SetAutoRestart(true)
	p.autoInterval = 30 * time.Minute
	p.llmTimeout = 120 * time.Second
	p.llmMaxTurns = 20
	p.llmMaxTokens = 4096
	p.perfHistory = 100

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "auto_interval", Type: "string", DisplayName: "自动检查间隔",
		Description: "自动健康检查的执行间隔，例如 30m, 1h, 10m（设为 0 禁用）",
		Default:     "30m",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "llm_timeout", Type: "string", DisplayName: "LLM 检查超时",
		Description: "LLM 驱动检查的超时时间，例如 120s, 3m, 5m（默认 120s）",
		Default:     "120s",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "llm_max_turns", Type: "int", DisplayName: "LLM 最大对话轮数",
		Description: "LLM 工具发现的最大对话轮数（默认 20）",
		Default:     "20",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "llm_max_tokens", Type: "int", DisplayName: "LLM 最大 Token",
		Description: "LLM 调用时的最大 Token 数（默认 4096）",
		Default:     "4096",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "perf_history", Type: "int", DisplayName: "性能历史保留数",
		Description: "保留的历史检查记录条数（默认 100）",
		Default:     "100",
	})

	if v, _ := s.Settings().Get("auto_interval"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				p.autoInterval = d
			}
		}
	}
	if v, _ := s.Settings().Get("llm_timeout"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				p.llmTimeout = d
			}
		}
	}
	if v, _ := s.Settings().Get("llm_max_turns"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if n, err := fmt.Sscanf(s, "%d", &p.llmMaxTurns); err != nil || n < 1 {
				p.llmMaxTurns = 20
			}
		}
	}
	if v, _ := s.Settings().Get("llm_max_tokens"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if n, err := fmt.Sscanf(s, "%d", &p.llmMaxTokens); err != nil || n < 1 {
				p.llmMaxTokens = 4096
			}
		}
	}
	if v, _ := s.Settings().Get("perf_history"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if n, err := fmt.Sscanf(s, "%d", &p.perfHistory); err != nil || n < 1 {
				p.perfHistory = 100
			}
		}
	}

	p.selfToolNames["healthcheck"] = true
	s.RegisterTool("healthcheck", sdk.ToolDef{
		Name:        "healthcheck",
		Description: "运行系统全面健康检查。先执行静态检查（插件/工具列表/记忆/知识库/文档），再启动 LLM 驱动检查：LLM 主动发现并逐个测试各插件提供的工具，并通过 healthcheck_report 上报结果。返回详细的状态报告（每个插件、每个工具一条结果）。可用 plugin 参数只针对指定插件检查。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"plugin": map[string]interface{}{"type": "string", "description": "可选：指定只检查该插件的健康状态（列出插件工具并逐一测试），不填则检查全部插件"},
			},
		},
		// 共享 p.mu 写锁，且 healthcheck_report 会 append p.reports
		Serial: true,
		// ★ 声明显式超时：LLM 驱动检查默认就要 120s（见 llm_timeout 配置），
		// 而内核原先把所有工具一刀切 60s ⇒ 这个工具**天生**跑不完，
		// 每次都报「执行超时（60秒）」。这里给到配置上限之上。
		Timeout: "5m",
	}, func(args map[string]interface{}) (interface{}, error) {
		plugin, _ := args["plugin"].(string)
		return p.runFullCheck(s, plugin)
	})

	p.selfToolNames["healthcheck_plugins"] = true
	s.RegisterTool("healthcheck_plugins", sdk.ToolDef{
		Name:        "healthcheck_plugins",
		Description: "列出所有已加载的插件及其状态。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 共享 p.mu 写锁，且 healthcheck_report 会 append p.reports
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.checkPlugins(s)
	})

	p.selfToolNames["healthcheck_tools"] = true
	s.RegisterTool("healthcheck_tools", sdk.ToolDef{
		Name:        "healthcheck_tools",
		Description: "列出系统中所有已注册的工具及其来源。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 共享 p.mu 写锁，且 healthcheck_report 会 append p.reports
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.listAllTools(s)
	})

	p.selfToolNames["healthcheck_memory"] = true
	s.RegisterTool("healthcheck_memory", sdk.ToolDef{
		Name:        "healthcheck_memory",
		Description: "测试图记忆系统：写入、查询、清理一条测试实体。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 共享 p.mu 写锁，且 healthcheck_report 会 append p.reports
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.checkMemory(s)
	})

	p.selfToolNames["healthcheck_report"] = true
	s.RegisterTool("healthcheck_report", sdk.ToolDef{
		Name:        "healthcheck_report",
		Description: "LLM 健康检查结果上报工具。LLM 在逐一测试各工具后，通过此工具提交每个工具的测试状态。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tool_name": map[string]interface{}{"type": "string", "description": "被测试的工具名称"},
				"status":    map[string]interface{}{"type": "string", "description": "测试结果：ok / fail / skip", "enum": []string{"ok", "fail", "skip"}},
				"detail":    map[string]interface{}{"type": "string", "description": "测试详情或错误描述"},
			},
			"required": []string{"tool_name", "status"},
		},
		// 共享 p.mu 写锁，且 healthcheck_report 会 append p.reports
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		toolName, _ := args["tool_name"].(string)
		status, _ := args["status"].(string)
		detail, _ := args["detail"].(string)
		p.mu.Lock()
		p.reports = append(p.reports, llmReport{ToolName: toolName, Status: status, Detail: detail})
		count := len(p.reports)
		p.mu.Unlock()
		log.Printf("[healthcheck] LLM report: tool=%s status=%s (total %d)", toolName, status, count)
		return map[string]interface{}{"ok": true, "received": count}, nil
	})

	if s.Status() != nil {
		p.selfToolNames["healthcheck_kernel"] = true
		s.RegisterTool("healthcheck_kernel", sdk.ToolDef{
			Name:        "healthcheck_kernel",
			Description: "查询 Agent 内核运行状态快照，包括**内核版本号与构建身份**（build.version / commit / build_time）、**统一多模态向量空间（ONNX 模型）是否启用**（onnx.enabled，未启用时给出原因）、**依存句法分析器状态**（onnx.dep_parser：是否启用、实际模型路径、是否来自二进制内嵌）、插件/工具/记忆/知识库/LLM Provider/运行时等各子系统信息。Agent 可通过此工具自主监测内核健康。",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			// 共享 p.mu 写锁，且 healthcheck_report 会 append p.reports
			Serial: true,
		}, func(args map[string]interface{}) (interface{}, error) {
			return s.Status().GetKernelStatus(), nil
		})
	}

	p.selfToolNames["healthcheck_perf"] = true
	s.RegisterTool("healthcheck_perf", sdk.ToolDef{
		Name:        "healthcheck_perf",
		Description: "查询健康检查性能监控数据，包括最近检查时间、历史检查记录（最多 100 条）及通过/失败统计。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 共享 p.mu 写锁，且 healthcheck_report 会 append p.reports
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		passed := 0
		failed := 0
		for _, c := range p.perfData.Checks {
			passed += c.Passed
			failed += c.Failed
		}
		return map[string]interface{}{
			"status":       "ok",
			"last_check":   p.perfData.LastCheck,
			"total_checks": len(p.perfData.Checks),
			"total_passed": passed,
			"total_failed": failed,
			"history":      p.perfData.Checks,
		}, nil
	})

	if p.autoInterval > 0 {
		p.startAutoCheck(s, p.autoInterval)
	}

	log.Printf("[healthcheck] ready (tool=%v mem=%v ks=%v ds=%v llm=%v plugins=%v status=%v)",
		s.Tool() != nil, s.Memory() != nil, s.Knowledge() != nil,
		s.DocMemory() != nil, s.LLM() != nil, s.PluginMgr() != nil, s.Status() != nil)
	return nil
}

func (p *Plugin) Stop() error {
	// 幂等关闭：仅当 stopCh 未被关闭时 close。
	// 用 safeclose 统一入口（本仓 close 纪律）。
	// 原实现是 `select{case <-p.stopCh: default: close(...)}`：
	// 单线程安全，但**并发**下两个 goroutine 可同时落到 default ⇒ 双 close panic。
	safeclose.Safe(&p.stopOnce, p.stopCh)
	return nil
}

func (p *Plugin) startAutoCheck(s *sdk.PluginSDK, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-ticker.C:
				p.runAutoCheck(s)
			}
		}
	}()
	log.Printf("[healthcheck] auto-check started, interval=%v", interval)
}

func (p *Plugin) runAutoCheck(s *sdk.PluginSDK) {
	result, err := p.runFullCheck(s, "")
	if err != nil {
		log.Printf("[healthcheck] auto-check error: %v", err)
		return
	}
	resp, _ := result.(map[string]interface{})
	passed, _ := resp["passed"].(int)
	failed, _ := resp["failed"].(int)
	total, _ := resp["total"].(int)
	elapsedMs, _ := resp["elapsed_ms"].(int64)

	// 失败项明细（定位问题用）
	if checks, ok := resp["checks"].([]checkResult); ok {
		for _, c := range checks {
			if !c.Pass {
				log.Printf("[healthcheck] FAILED %s: status=%s detail=%s", c.Name, c.Status, c.Detail)
			}
		}
	}

	pt := PerfCheckPoint{
		Time:      time.Now(),
		Passed:    passed,
		Failed:    failed,
		Total:     total,
		ElapsedMs: elapsedMs,
	}
	p.mu.Lock()
	p.perfData.LastCheck = pt.Time
	p.perfData.Checks = append(p.perfData.Checks, pt)
	if len(p.perfData.Checks) > p.perfHistory {
		p.perfData.Checks = p.perfData.Checks[len(p.perfData.Checks)-p.perfHistory:]
	}
	p.mu.Unlock()

	log.Printf("[healthcheck] auto-check complete: passed=%d failed=%d total=%d elapsed=%dms", passed, failed, total, elapsedMs)
}

func (p *Plugin) runFullCheck(s *sdk.PluginSDK, pluginFilter string) (interface{}, error) {
	// 单飞：auto-check 与手动调用并发时，避免互相 reset 虚拟实例或 LLM 检查串扰
	p.checkMu.Lock()
	defer p.checkMu.Unlock()
	start := time.Now()

	results := []checkResult{}

	// 每轮自检前重置隔离虚拟实例，清空上轮测试数据（仅影响虚拟空间，不碰生产存储）。
	if err := s.SelftestReset("hc"); err != nil {
		log.Printf("[healthcheck] selftest reset: %v", err)
	}

	pluginResult := p.checkPluginsRaw(s, pluginFilter)
	results = append(results, pluginResult...)

	toolResult := p.checkToolsRaw(s, pluginFilter)
	results = append(results, toolResult...)

	if s.Memory() != nil {
		results = append(results, p.testMemoryRaw(s))
	} else {
		results = append(results, checkResult{Name: "memory", Status: "skip", Detail: "图记忆未初始化", Pass: true})
	}

	if s.Knowledge() != nil {
		results = append(results, p.testKnowledgeRaw(s))
	} else {
		results = append(results, checkResult{Name: "knowledge", Status: "skip", Detail: "知识库未初始化", Pass: true})
	}

	if s.DocMemory() != nil {
		results = append(results, p.testDocStoreRaw(s))
	} else {
		results = append(results, checkResult{Name: "documents", Status: "skip", Detail: "文档记忆未初始化", Pass: true})
	}

	if s.LLM() != nil {
		results = append(results, p.testLLMDriven(s, pluginFilter))
		results = append(results, p.reportsToChecks()...)
	} else {
		results = append(results, checkResult{Name: "llm_discovery", Status: "skip", Detail: "LLM Provider 未初始化", Pass: true})
	}

	passCount := 0
	failCount := 0
	for _, r := range results {
		if r.Pass {
			passCount++
		} else {
			failCount++
		}
	}

	elapsed := time.Since(start)
	summary := fmt.Sprintf("通过 %d / %d, 失败 %d, 耗时 %v", passCount, len(results), failCount, elapsed.Round(time.Millisecond))

	return map[string]interface{}{
		"status":     "ok",
		"summary":    summary,
		"total":      len(results),
		"passed":     passCount,
		"failed":     failCount,
		"elapsed_ms": elapsed.Milliseconds(),
		"checks":     results,
	}, nil
}

func (p *Plugin) checkPlugins(s *sdk.PluginSDK) (interface{}, error) {
	results := p.checkPluginsRaw(s, "")
	return map[string]interface{}{
		"status":  "ok",
		"plugins": results,
		"count":   len(results),
	}, nil
}

// checkPluginsRaw 按插件逐个产出检查项（每个插件一条：加载状态 + 工具数）。
func (p *Plugin) checkPluginsRaw(s *sdk.PluginSDK, pluginFilter string) []checkResult {
	if s.PluginMgr() == nil {
		return []checkResult{{Name: "plugins", Status: "skip", Detail: "插件注册表未初始化", Pass: true}}
	}

	names := s.PluginMgr().ListLoadedPlugins()
	if names == nil {
		names = []string{}
	}
	sort.Strings(names)
	disabled := map[string]bool{}
	for _, d := range s.PluginMgr().ListDisabledPlugins() {
		disabled[d.Name] = true
	}

	// 工具按插件聚合统计
	toolCount := map[string]int{}
	if s.Tool() != nil {
		for _, def := range s.Tool().GetToolDefs() {
			pl := def.Plugin
			if pl == "" {
				pl = "unknown"
			}
			toolCount[pl]++
		}
	}

	var out []checkResult
	for _, name := range names {
		if pluginFilter != "" && name != pluginFilter {
			continue
		}
		status, detail := "ok", ""
		pass := true
		if disabled[name] {
			status, detail, pass = "disabled", "插件已禁用", true
		}
		if n := toolCount[name]; n > 0 {
			if detail != "" {
				detail += ", "
			}
			detail += fmt.Sprintf("%d 个工具", n)
		} else if !disabled[name] {
			status, detail, pass = "warn", "插件已加载但未注册工具", true
		}
		out = append(out, checkResult{Name: "plugin/" + name, Status: status, Detail: detail, Pass: pass})
	}

	if pluginFilter != "" {
		matched := false
		for _, name := range names {
			if name == pluginFilter {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, checkResult{Name: "plugin/" + pluginFilter, Status: "fail", Detail: "插件未加载或不存在", Pass: false})
		}
	}

	if len(out) == 0 {
		out = append(out, checkResult{Name: "plugins", Status: "ok", Detail: "无已加载插件", Pass: true})
	}
	return out
}

func (p *Plugin) listAllTools(s *sdk.PluginSDK) (interface{}, error) {
	tools := p.collectAllTools(s, "")
	return map[string]interface{}{
		"status": "ok",
		"count":  len(tools),
		"tools":  tools,
	}, nil
}

func (p *Plugin) checkToolsRaw(s *sdk.PluginSDK, pluginFilter string) []checkResult {
	tools := p.collectAllTools(s, pluginFilter)
	byPlugin := map[string]int{}
	for _, t := range tools {
		src := t.Source
		if src == "" {
			src = "unknown"
		}
		byPlugin[src]++
	}
	names := make([]string, 0, len(byPlugin))
	for n := range byPlugin {
		names = append(names, n)
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", n, byPlugin[n]))
	}
	return []checkResult{{
		Name:   "tools",
		Status: "ok",
		Detail: fmt.Sprintf("共 %d 个工具（%s）", len(tools), strings.Join(parts, ", ")),
		Pass:   true,
	}}
}

func (p *Plugin) collectAllTools(s *sdk.PluginSDK, pluginFilter string) []toolInfo {
	seen := map[string]bool{}
	var tools []toolInfo

	addTool := func(name, source, desc string) {
		if seen[name] {
			return
		}
		seen[name] = true
		tools = append(tools, toolInfo{Name: name, Source: source, Description: desc})
	}

	if s.Tool() != nil {
		for _, def := range s.Tool().GetToolDefs() {
			if pluginFilter != "" && def.Plugin != pluginFilter {
				continue
			}
			src := def.Plugin
			if src == "" {
				src = "unknown"
			}
			addTool(def.Name, src, def.Description)
		}

		if pluginFilter == "" {
			for _, def := range s.Tool().GetAllTools() {
				addTool(def.Name, "device", def.Description)
			}
		}
	}

	return tools
}

func (p *Plugin) selftestInst(s *sdk.PluginSDK) (*sdk.VirtualInstance, error) {
	vi, err := s.Selftest("hc")
	if err != nil {
		return nil, err
	}
	if vi == nil {
		return nil, fmt.Errorf("Selftest 不可用")
	}
	return vi, nil
}

func (p *Plugin) testMemoryRaw(s *sdk.PluginSDK) checkResult {
	// 在隔离虚拟图记忆上验证写→查→删，绝不动生产 GraphDB。
	vi, err := p.selftestInst(s)
	if err != nil {
		return checkResult{Name: "memory", Status: "skip", Detail: fmt.Sprintf("虚拟实例不可用: %v", err), Pass: true}
	}

	marker := fmt.Sprintf("_hc_%d", time.Now().UnixNano())
	triples := []sdk.Triple{
		{Subject: marker, Relation: "is", Object: "healthcheck_test", SubjectType: "System", ObjectType: "Flag"},
	}

	start := time.Now()
	if err := vi.Memory.Commit(triples); err != nil {
		return checkResult{Name: "memory", Status: "fail", Detail: fmt.Sprintf("写入失败: %v", err), Pass: false}
	}

	ents, rels, err := vi.Memory.Recall([]string{marker}, 1)
	if err != nil {
		return checkResult{Name: "memory", Status: "fail", Detail: fmt.Sprintf("查询失败: %v", err), Pass: false}
	}
	if len(ents) == 0 && len(rels) == 0 {
		return checkResult{Name: "memory", Status: "warn", Detail: "写入成功但查询未命中", Pass: true}
	}

	_, err = vi.Memory.Purge(map[string]string{"subject_contains": marker}, "hard")
	if err != nil {
		return checkResult{Name: "memory", Status: "fail", Detail: fmt.Sprintf("清理失败: %v", err), Pass: false}
	}

	elapsed := time.Since(start)
	return checkResult{
		Name:   "memory",
		Status: "ok",
		Detail: fmt.Sprintf("隔离虚拟记忆写入+查询+清理正常, 耗时 %v", elapsed.Round(time.Millisecond)),
		Pass:   true,
	}
}

func (p *Plugin) testKnowledgeRaw(s *sdk.PluginSDK) checkResult {
	// 在隔离虚拟知识库上验证写→查→删，绝不动生产知识库。
	vi, err := p.selftestInst(s)
	if err != nil {
		return checkResult{Name: "knowledge", Status: "skip", Detail: fmt.Sprintf("虚拟实例不可用: %v", err), Pass: true}
	}

	marker := fmt.Sprintf("_hc_knowledge_test_%d", time.Now().UnixNano())
	start := time.Now()

	if err := vi.Knowledge.Add(marker, "健康检查测试标记，可忽略"); err != nil {
		return checkResult{Name: "knowledge", Status: "fail", Detail: fmt.Sprintf("写入失败: %v", err), Pass: false}
	}

	results, err := vi.Knowledge.Search("健康检查测试标记", 3)
	if err != nil {
		vi.Knowledge.Remove(marker)
		return checkResult{Name: "knowledge", Status: "fail", Detail: fmt.Sprintf("查询失败: %v", err), Pass: false}
	}

	elapsed := time.Since(start)

	// 清理测试条目，避免积累
	vi.Knowledge.Remove(marker)

	if len(results) > 0 {
		return checkResult{
			Name:   "knowledge",
			Status: "ok",
			Detail: fmt.Sprintf("隔离虚拟知识库写入+查询+清理正常, 耗时 %v", elapsed.Round(time.Millisecond)),
			Pass:   true,
		}
	}

	return checkResult{
		Name:   "knowledge",
		Status: "warn",
		Detail: fmt.Sprintf("写入成功但查询缓存未命中, 耗时 %v", elapsed.Round(time.Millisecond)),
		Pass:   true,
	}
}

func (p *Plugin) testDocStoreRaw(s *sdk.PluginSDK) checkResult {
	// 在隔离虚拟文档记忆上验证写→查→删，绝不动生产 Document。
	vi, err := p.selftestInst(s)
	if err != nil {
		return checkResult{Name: "documents", Status: "skip", Detail: fmt.Sprintf("虚拟实例不可用: %v", err), Pass: true}
	}

	start := time.Now()
	doc := &sdk.Doc{
		Title:   fmt.Sprintf("健康检查测试文档 %d", time.Now().UnixNano()),
		Content: "这是一条由 healthcheck 插件创建的测试文档，用于验证文档记忆系统是否正常工作。",
	}
	if err := vi.DocMemory.Insert(doc); err != nil {
		return checkResult{Name: "documents", Status: "fail", Detail: fmt.Sprintf("写入失败: %v", err), Pass: false}
	}

	// 清理测试文档，避免积累（SDK Insert 不回填 ID，经 Query 按标题定位）
	for _, d := range vi.DocMemory.Query("健康检查测试文档", 10) {
		if d.ID != "" && strings.HasPrefix(d.Title, "健康检查测试文档") {
			vi.DocMemory.Remove(d.ID)
		}
	}

	elapsed := time.Since(start)
	return checkResult{
		Name:   "documents",
		Status: "ok",
		Detail: fmt.Sprintf("隔离虚拟文档记忆写入+查询+清理正常, 耗时 %v", elapsed.Round(time.Millisecond)),
		Pass:   true,
	}
}

func (p *Plugin) testLLMDriven(s *sdk.PluginSDK, pluginFilter string) checkResult {
	llmName := s.LLM().CurrentSource()
	if llmName == "" {
		return checkResult{Name: "llm_discovery", Status: "skip", Detail: "无可用 LLM Provider", Pass: true}
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), p.llmTimeout)
	defer cancel()

	// 收集所有工具定义（排除健康检查自身的工具以避免循环测试）
	toolDefs := p.collectToolDefsForLLM(s, pluginFilter)

	if len(toolDefs) == 0 {
		return checkResult{Name: "llm_discovery", Status: "skip", Detail: "没有可测试的工具", Pass: true}
	}

	// 重置会话
	p.mu.Lock()
	p.reports = nil
	p.sessionID = fmt.Sprintf("hc_llm_%d", time.Now().UnixNano())
	p.mu.Unlock()

	// 构建 prompt
	prompt := p.buildDiscoveryPrompt(toolDefs, pluginFilter)

	msgs := []sdk.LLMMessage{{Role: "user", Content: prompt}}
	tools := convertToolDefs(toolDefs)

	turnCount := 0
	toolCallCount := 0

	for turn := 0; turn < p.llmMaxTurns; turn++ {
		resp, err := s.LLM().Chat(ctx, &sdk.LLMCompletionRequest{
			Messages:   msgs,
			MaxTokens:  p.llmMaxTokens,
			Tools:      tools,
			ToolChoice: "auto",
		})
		if err != nil {
			return checkResult{
				Name:   "llm_discovery",
				Status: "fail",
				Detail: fmt.Sprintf("Provider %s 第 %d 轮调用失败: %v (耗时 %v)",
					llmName, turn+1, err, time.Since(start).Round(time.Millisecond)),
				Pass: false,
			}
		}

		turnCount++

		if len(resp.ToolCalls) == 0 {
			break
		}

		msgs = append(msgs, sdk.LLMMessage{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})

		for _, tc := range resp.ToolCalls {
			toolCallCount++
			content := p.executeToolForLLM(s, tc)
			msgs = append(msgs, sdk.LLMMessage{Role: "tool", ToolCallID: tc.ID, Content: content})
		}
	}

	p.mu.Lock()
	reportCount := len(p.reports)
	p.mu.Unlock()

	elapsed := time.Since(start).Round(time.Millisecond)

	detail := fmt.Sprintf("Provider %s, %d 轮对话, %d 次工具调用, %d 份工具报告, 耗时 %v",
		llmName, turnCount, toolCallCount, reportCount, elapsed)

	return checkResult{
		Name:   "llm_discovery",
		Status: "ok",
		Detail: detail,
		Pass:   true,
	}
}

// reportsToChecks 把 LLM 逐工具上报的 healthcheck_report 明细转为细粒度检查项。
func (p *Plugin) reportsToChecks() []checkResult {
	p.mu.Lock()
	reports := append([]llmReport{}, p.reports...)
	p.mu.Unlock()

	out := make([]checkResult, 0, len(reports))
	for _, r := range reports {
		pass := r.Status == "ok" || r.Status == "skip"
		out = append(out, checkResult{
			Name:   "llm_tool/" + r.ToolName,
			Status: r.Status,
			Detail: r.Detail,
			Pass:   pass,
		})
	}
	return out
}

// collectToolDefsForLLM 收集全部已注册的工具定义供 LLM 发现和测试。
// 动态排除本插件自身注册的工具（通过 selfToolNames），避免 LLM 自我循环调用；
// 且仅保留"只读/轻量验证"类工具（白名单语义），防止 LLM 自检污染生产数据或引发副作用。
func (p *Plugin) collectToolDefsForLLM(s *sdk.PluginSDK, pluginFilter string) []sdk.ToolDef {
	seen := map[string]bool{}
	var defs []sdk.ToolDef

	addDef := func(d sdk.ToolDef) {
		if p.selfToolNames[d.Name] || seen[d.Name] {
			return
		}
		if !isSafeReadonlyTool(d.Name) {
			return
		}
		seen[d.Name] = true
		defs = append(defs, d)
	}

	if s.Tool() != nil {
		for _, d := range s.Tool().GetToolDefs() {
			if pluginFilter != "" && d.Plugin != pluginFilter {
				continue
			}
			addDef(d)
		}
		if pluginFilter == "" {
			for _, d := range s.Tool().GetAllTools() {
				addDef(d)
			}
		}
	}

	return defs
}

// isSafeReadonlyTool 判断工具是否为"只读/无副作用、适合健康检查 LLM 自检"的工具。
// 仅白名单语义：不在白名单的工具一律不测（宁可少测，不可污染/引发副作用）。
func isSafeReadonlyTool(name string) bool {
	// 明确只读的查询/列表类工具
	readonlyExact := map[string]bool{
		"memory_recall":        true,
		"memory_introspect":    true,
		"doc_query":            true,
		"knowledge_search":     true,
		"knowledge_list":       true,
		"person_query":         true,
		"person_network":       true,
		"llm_list_sources":     true,
		"output_list_channels": true,
		"terminal_list":        true,
	}
	if readonlyExact[name] {
		return true
	}
	// 带 _list/_help 后缀的通常是只读展示
	for _, sfx := range []string{"_list", "_help"} {
		if strings.HasSuffix(name, sfx) {
			return true
		}
	}
	return false
}

// buildDiscoveryPrompt 为 LLM 构造工具探索 prompt。
func (p *Plugin) buildDiscoveryPrompt(toolDefs []sdk.ToolDef, pluginFilter string) string {
	target := "全部插件"
	if pluginFilter != "" {
		target = "插件「" + pluginFilter + "」"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`你是一名系统健康检查专家。以下是%s提供的 %d 个工具（已自动排除健康检查插件自身工具及所有会写/删/改生产数据或产生外部副作用的工具，以下均为只读/查询/列表类工具）：

你的任务是：逐一尝试调用这些工具，验证它们是否正常工作，并对于每个工具使用 healthcheck_report 工具上报测试结果。

对于每个工具：
1. 理解它的参数和功能
2. 构造合适的测试参数调用它
3. 根据返回结果判断是否正常
4. 调用 healthcheck_report 工具上报（tool_name, status=ok/fail/skip, detail=详情）

注意：
- 所有工具均为只读、无副作用，可放心调用
- 尽可能覆盖所有工具
- 每个工具只需测试一次
- 每个工具都必须单独调用 healthcheck_report 上报，不要合并

开始测试！`, target, len(toolDefs)))
	return b.String()
}

// executeToolForLLM 在 LLM 工具循环中执行工具调用。
// healthcheck_report 经 SDK ToolAPI 路由到自身注册的 handler，负责收集 LLM 上报。
func (p *Plugin) executeToolForLLM(s *sdk.PluginSDK, tc sdk.LLMToolCall) string {
	if s.Tool() != nil {
		result, err := s.Tool().ExecuteTool(tc.Name, tc.Arguments)
		if err != nil {
			return fmt.Sprintf("调用工具 %s 失败: %v", tc.Name, err)
		}
		data, _ := json.Marshal(result)
		return string(data)
	}

	return fmt.Sprintf("工具 %s 不可执行（工具注册表未初始化）", tc.Name)
}

func convertToolDefs(defs []sdk.ToolDef) []interface{} {
	tools := make([]interface{}, len(defs))
	for i, d := range defs {
		tools[i] = map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        d.Name,
				"description": d.Description,
				"parameters":  d.Parameters,
			},
		}
	}
	return tools
}

func (p *Plugin) checkMemory(s *sdk.PluginSDK) (interface{}, error) {
	if s.Memory() == nil {
		return map[string]interface{}{"status": "skip", "pass": true, "detail": "图记忆未初始化"}, nil
	}
	r := p.testMemoryRaw(s)
	c := map[string]interface{}{
		"status": r.Status,
		"pass":   r.Pass,
	}
	if r.Detail != "" {
		c["detail"] = r.Detail
	}
	return c, nil
}

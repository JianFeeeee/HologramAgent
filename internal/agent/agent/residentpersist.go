package agent

// 驻留子的**存活登记表**持久化（2026-11-17）。
//
// ## 缺陷：重启后驻留子全部消失，且连"曾经有哪些"都不知道
//
// 驻留子的状态原本有三层，重启时**三层全丢**：
//
//	① 登记表（a.children）      只在内存         ⇒ 重启后父不知道有过哪些子
//	② temp 图记忆（graph.db）   落盘但停机被删    ⇒ 记忆没了
//	③ 普通上下文（context.json）从不落盘         ⇒ 对话现场没了
//
// 用户的判断：「驻留式子 agent 没有持久化导致每次重启都会丢失，这是一个比较
// 严重的问题」。确实：一个"长期派驻"的 agent 每次重启都从零复活，
// 那"驻留"二字就不成立 —— 用户挂了一晚上的子醒来什么都不记得。
//
// ## 本文件负责 ①：登记表
//
// 只存「有哪些驻留子 + 重建它们所需的全部参数」（`ResidentOptions`），
// 不存运行态（轮次、处理表、contextfull 标记）—— 那些属于子自己的
// context.json / graph.db，由它们各自的持久化负责。
//
//	<data>/residents/registry.json
//
// ## 为什么存**创建参数**而不是"当前状态"
//
// 重建 = 用同样的参数再调一次 SpawnResident。参数是创建那一刻父的决定
// （任务提示词、划入的 inputch、授权范围、工具白名单），那才是需要跨重启
// 保住的东西。运行态可以重算，父的决定不能重算。
//
// ## 2026-11-18 上移：登记表改为**实例层**统一持久化
//
// 原先这里自己维护 `<data>/residents/registry.json`（只认识驻留子），
// 而 `RestoreResidents` **零调用方** —— 重启后驻留子全丢。
//
// 现在创建参数落在 `registry` 的实例表（`<data>/agents/instances.json`），
// 由 InstanceManager 统一负责（软重启与进程重启共用同一份快照）。
// 本文件退化为**桥接**：把驻留子的创建参数转成实例表的 arg 形态，
// 并保留它独有的两项磁盘治理（孤儿目录归档 + 旧登记表迁移）。
//
// 这样新增一种可持久化的子 agent 类型**不必再写一遍持久化**。
//
// ## 排除项
//
// `OffloadOwned`（内核为转投而拉的临时分诊助手）**不写进登记表**：
// 它是内核内部的负载调度产物，不是父的决定。重启后负载会自己重新触发
// `ensureOffloadResident`，把上一代的临时助手恢复出来只会留下没人用的孤儿。

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	childpkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
)

// residentRegistryFile 是登记表文件名（相对 <data>/residents/）。
const residentRegistryFile = "registry.json"

// residentRecord 是登记表里的一条：重建这个子所需的全部创建参数。
type residentRecord struct {
	ID                string   `json:"id"`
	TaskPrompt        string   `json:"task_prompt,omitempty"`
	InputChs          []string `json:"input_chs,omitempty"`
	AllowedOutputs    []string `json:"allowed_outputs,omitempty"`
	Capacity          int      `json:"capacity,omitempty"`
	PreferredProvider string   `json:"preferred_provider,omitempty"`
	AllowedTools      []string `json:"allowed_tools,omitempty"`
	DenyTools         []string `json:"deny_tools,omitempty"`
}

/*
*

residentRegistryPath 返回登记表路径。

★ 为什么用 dataDir 而不是子自己的 temp 目录：登记表是**父**的文件
（父是唯一写者，也是重启后重建的执行者）。放进某个子的目录会让
"子被销毁"连带"登记表少一条"，而那正是要避免的耦合。
*/
func residentRegistryPath(dataDir string) string {
	return filepath.Join(dataDir, "residents", residentRegistryFile)
}

// loadResidentRecords 读回登记表（不存在 ⇒ 空，不是错误）。
func loadResidentRecords(dataDir string) []residentRecord {
	if strings.TrimSpace(dataDir) == "" {
		return nil
	}
	raw, err := os.ReadFile(residentRegistryPath(dataDir))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[resident] read registry: %v", err)
		}
		return nil
	}
	var recs []residentRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		// 腐坏不静默：继续启动（丢的是"哪些子该恢复"），但要让人看见。
		log.Printf("[resident] registry corrupted, ignoring: %v", err)
		return nil
	}
	return recs
}

// saveResidentRecords 原子写回登记表。空表 ⇒ 删除文件（不留 `[]` 噪音）。
func saveResidentRecords(dataDir string, recs []residentRecord) {
	if strings.TrimSpace(dataDir) == "" {
		return
	}
	path := residentRegistryPath(dataDir)
	if len(recs) == 0 {
		_ = os.Remove(path)
		return
	}
	// 稳定序：便于 diff 与人工核对（不要按 map 迭代顺序写）。
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		log.Printf("[resident] marshal registry: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("[resident] create registry dir: %v", err)
		return
	}
	// 原子写（临时文件 + rename）：进程在写一半时崩溃不能留下半截 JSON。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("[resident] write registry: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[resident] commit registry: %v", err)
	}
}

// persistResidents 把**当前**登记表写回磁盘。
//
// 何时调用：创建成功后、销毁/回收成功后。不放在轮次循环里 ——
// 登记表的变动是低频事件（创建/销毁），而每轮写一次是白耗 IO。
func (a *Agent) PersistResidents() {
	if a == nil || a.dataDir == "" {
		return
	}
	m := a.EnsureInstanceManager()
	if m == nil {
		return
	}
	seen := map[string]bool{}
	for _, e := range a.children.ByKind(childpkg.KindResident) {
		rc, ok := e.(*residentChild)
		if !ok {
			continue
		}
		// 内核为转投而拉的临时助手不进登记表（见文件头「排除项」）。
		if rc.offloadOwned {
			continue
		}
		seen[rc.ID] = true
		m.remember(kindResident, rc.ID, rc.toAdminArg())
	}
	// 已经不在登记表里的（被销毁/回收）从实例表摘掉。
	for _, r := range m.Records() {
		if r.Type != kindResident {
			continue
		}
		if !seen[r.ID] {
			m.forget(r.ID)
		}
	}
}

// toAdminArg 把驻留子的**创建参数**转成 admin_child 的 arg 形态。
//
// ★ 单一转换点是刻意的：软重启、进程重启恢复、持久化落盘三条路都走它，
//
//	于是"参数在三条路上不一致"这种漂移不可能发生。
func (rc *residentChild) toAdminArg() map[string]interface{} {
	if rc == nil {
		return nil
	}
	arg := map[string]interface{}{
		"id":          rc.ID,
		"task_prompt": rc.taskPrompt,
		"temp_path":   rc.tempPath,
	}
	if len(rc.inputChs) > 0 {
		arg["input_chs"] = strings.Join(rc.inputChs, ",")
	}
	if len(rc.allowed) > 0 {
		arg["allowed_outputs"] = strings.Join(rc.allowed, ",")
	}
	if rc.capacity > 0 {
		arg["capacity"] = float64(rc.capacity)
	}
	if rc.preferredProvider != "" {
		arg["preferred_provider"] = rc.preferredProvider
	}
	if len(rc.allowedTools) > 0 {
		arg["allowed_tools"] = strings.Join(rc.allowedTools, ",")
	}
	if len(rc.denyTools) > 0 {
		arg["deny_tools"] = strings.Join(rc.denyTools, ",")
	}
	return arg
}

// RestoreResidents 按登记表重建驻留子（进程启动时调用一次）。
//
// 返回成功重建的数量。**逐个失败不中断**：一个子建不起来（temp 路径不可写、
// 划入的 inputch 已被别的子占着）不该阻止其余子恢复 —— 那会把一次局部故障
// 放大成"全部驻留子都没了"。
//
// ★ 为什么由调用方显式调、而不塞进 Agent.Start()：
// 重建要在插件就绪之后（inputch 得先存在）。Agent.Start() 的时机由
// bootstrap 决定，本函数跟着同一个时机即可 —— 但**不能**自动跑，
// 因为判据与独立工具构造 Agent 时不希望它去读生产目录。
func (a *Agent) RestoreResidents() int {
	// ① 首次升级路径：老版本只有 <data>/residents/registry.json。
	//    把它迁进实例表后再删旧文件（否则下轮又会被读一遍）。
	migrateLegacyResidentRegistry(a.dataDir, a.EnsureInstanceManager())

	// ② 统一恢复：实例表里的全部类型（不只驻留子）。
	n, _ := a.EnsureInstanceManager().Restore()
	return n
}

// migrateLegacyResidentRegistry 把旧登记表搬进实例表（一次性）。
//
// ★ 为什么保留这段：线上已有部署的 <data>/residents/registry.json 里存着
//
//	用户的驻留子定义。直接换格式会让它们"突然全部消失"—— 与本次要修的
//	缺陷同形，只是成因不同。
func migrateLegacyResidentRegistry(dataDir string, m *InstanceManager) {
	if dataDir == "" || m == nil {
		return
	}
	recs := loadResidentRecords(dataDir)
	if len(recs) == 0 {
		return
	}
	for _, r := range recs {
		if r.ID == "" {
			continue
		}
		tempPath := filepath.Join(dataDir, "residents", r.ID, "graph.db")
		m.remember(kindResident, r.ID, map[string]interface{}{
			"id":                 r.ID,
			"task_prompt":        r.TaskPrompt,
			"input_chs":          strings.Join(r.InputChs, ","),
			"allowed_outputs":    strings.Join(r.AllowedOutputs, ","),
			"capacity":           float64(r.Capacity),
			"temp_path":          tempPath,
			"preferred_provider": r.PreferredProvider,
			"allowed_tools":      strings.Join(r.AllowedTools, ","),
			"deny_tools":         strings.Join(r.DenyTools, ","),
		})
	}
	if err := os.Remove(residentRegistryPath(dataDir)); err != nil && !os.IsNotExist(err) {
		log.Printf("[resident] 迁移旧登记表后删除失败: %v", err)
		return
	}
	log.Printf("[resident] 已把 %d 条旧登记表迁入实例表 instances.json", len(recs))
}

// QuarantineLegacyResidentDirs 把**无法确定归属**的 residents 子目录挪到
// `<data>/residents/_legacy/`。
//
// ## 为什么需要（2026-11-17 实测）
//
// 生产 `<data>/residents/` 里有 3 个历史残留目录（10-16 与 11-04 建的），
// 它们的进程早就不在、登记表里也没有它们 —— 是崩溃/被杀留下的孤儿。
// 若让 RestoreResidents 按目录去发现子，这 3 个会被当成"该恢复的驻留子"
// 复活出来，而父根本不知道它们是谁、该干什么。
//
// 所以：**不按目录发现**（只认登记表），并把这类孤儿归档到 `_legacy/`
// 让人能看见与清理，而不是静默留在原地冒充"某个子的记忆"。
func (a *Agent) QuarantineLegacyResidentDirs() int {
	if a == nil || a.dataDir == "" {
		return 0
	}
	root := filepath.Join(a.dataDir, "residents")
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	known := map[string]bool{}
	for _, rec := range loadResidentRecords(a.dataDir) {
		known[rec.ID] = true
	}
	legacy := filepath.Join(root, "_legacy")
	moved := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == "_legacy" || known[name] {
			continue
		}
		// 只有"看起来像驻留子目录"的才归档（含 graph.db 或 context.json）。
		if !dirHasResidentArtifacts(filepath.Join(root, name)) {
			continue
		}
		if err := os.MkdirAll(legacy, 0o700); err != nil {
			log.Printf("[resident] create _legacy dir: %v", err)
			return moved
		}
		dst := filepath.Join(legacy, name)
		if err := os.Rename(filepath.Join(root, name), dst); err != nil {
			log.Printf("[resident] quarantine %s: %v", name, err)
			continue
		}
		moved++
		log.Printf("[resident] quarantined orphan resident dir %s -> _legacy/ "+
			"(not in registry: previous crash/kill leftover, not a live child)", name)
	}
	if moved > 0 {
		log.Printf("[resident] quarantined %d orphan resident dir(s) into _legacy/", moved)
	}
	return moved
}

// dirHasResidentArtifacts 报告目录里是否有驻留子的持久化产物。
func dirHasResidentArtifacts(dir string) bool {
	for _, name := range []string{"graph.db", "context.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

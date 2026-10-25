package plugin

// 内置插件随包 SKILL 的**统一生命周期管理**。
//
// ## 为什么放在内核（而不是让每个插件自己写）
//
// 第一版我让 seq 自己在 Start() 里写盘 + 发事件。那是错的，有两个硬伤：
//
//	① 每个内置插件都要重复实现一遍「释放 / 清理 / 幂等」，
//	   而且**禁用插件后 skill 还留着** —— 模型看得到 skill，点进去工具却不存在
//	② 内置插件是按需加载的：未 `plugin_start` 时它的工具本就不在，
//	   skill 却已经出现在索引里，等于对模型说谎
//
// 正确的语义：**skill 是插件能力的一部分，生命周期必须与插件同生共死**。
// 所以由内核在插件启停处统一切换，插件作者一行都不用写。
//
// ## 挂载点
//
//	StartPlugin / loadOne 成功  → InstallBuiltinSkill
//	StopPlugin / StopAll       → RemoveBuiltinSkill
//	持久禁用（plugin_disable）  → RemoveBuiltinSkill
//
// ## 来源
//
// 源码树里约定 `<插件源码目录>/skill/<skillName>/SKILL.md`（与 SDK 示例
// third_party/homeagent-sdk/example/editdoc/skill/office-tools 一致）。
// 但内置插件是**编译期内置**，运行时没有源码树，所以 SKILL 正文必须
// 编进二进制（见各插件包里的 embed），由插件通过 SkillProvider 暴露。
//
// 现阶段用一个最小接口从插件实例上取：**插件只需实现一个方法**，
// 不需要自己碰文件系统、不需要发事件、不需要管清理。

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// SkillProvider 是**可选**接口：内置插件实现它即可让自己的 SKILL 随启停出入。
//
// 只需返回 SKILL 文档集合（名字 → SKILL.md 全文），其余全由内核处理：
// 写盘位置、幂等、事件通知、停用时清理。
//
// 不实现该接口的插件不受任何影响（绝大多数插件没有 SKILL）。
type SkillProvider interface {
	// BuiltinSkills 返回本插件随包的 SKILL 文档。
	//
	// map 的键是 skill 名（**必须**与 SKILL.md frontmatter 的 name 一致，
	// 也用作目录名）；值是 SKILL.md 全文（含 frontmatter）。
	BuiltinSkills() map[string]string
}

// skillDirOf 返回某个内置 skill 在磁盘上的约定位置。
//
// 与 skillmgr 的扫描口径对齐：它扫 <data_dir>/skills/*，每个目录里要有 SKILL.md。
func (r *Registry) skillDirOf(skillName string) string {
	r.mu.RLock()
	dataDir := r.dataDir
	r.mu.RUnlock()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "skills", skillName)
}

// InstallBuiltinSkill 把某个插件的随包 SKILL 释放到 <data_dir>/skills/ 并通知 skillmgr。
//
// 幂等：
//   - 目录已存在且 SKILL.md 非空 ⇒ 视为已安装，只补发一次事件（让 skillmgr 接管）
//   - 内容为空/读取失败 ⇒ 重写
//
// 失败只记日志、不返回错误：skill 装不上不该阻止插件启动（工具本身仍可用）。
// 但必须**可见** —— 没有 skill 的插件模型基本不会去用，这是静默失效。
func (r *Registry) InstallBuiltinSkill(pluginName string, p sdk.Plugin) {
	provider, ok := p.(SkillProvider)
	if !ok {
		return // 绝大多数插件没有 SKILL，这是常态，不是错误
	}
	skills := provider.BuiltinSkills()
	if len(skills) == 0 {
		return
	}

	for name, body := range skills {
		if strings.TrimSpace(body) == "" {
			log.Printf("[plugin] %s: skill %q has empty body, skipping (an empty SKILL gives the model nothing to read)", pluginName, name)
			continue
		}
		dir := r.skillDirOf(name)
		if dir == "" {
			log.Printf("[plugin] %s: data_dir not set, cannot install skill %q", pluginName, name)
			continue
		}
		file := filepath.Join(dir, "SKILL.md")

		// 已存在且非空：保留（用户可能改过措辞）。但仍要发事件，
		// 否则重启后 skillmgr 若没扫到，这条 skill 就永久消失。
		cur, readErr := os.ReadFile(file)
		if readErr != nil || len(cur) == 0 {
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Printf("[plugin] %s: creating skill dir failed %s: %v", pluginName, dir, err)
				continue
			}
			if err := os.WriteFile(file, []byte(body), 0644); err != nil {
				log.Printf("[plugin] %s: writing skill failed %s: %v", pluginName, file, err)
				continue
			}
			log.Printf("[plugin] %s: installed skill %q -> %s", pluginName, name, dir)
		}

		r.announceSkill(pluginName, dir)
	}
}

// RemoveBuiltinSkill 在插件停用/禁用时移除其随包 SKILL。
//
// 为什么必须移除：否则模型在 skill 索引里仍看得到它，点进去照做，
// 而工具已被停用 —— 模型会拿到一连串"工具不存在"的错误，
// 且它无从知道原因是"插件被关了"。
func (r *Registry) RemoveBuiltinSkill(pluginName string, p sdk.Plugin) {
	provider, ok := p.(SkillProvider)
	if !ok {
		return
	}
	for name := range provider.BuiltinSkills() {
		dir := r.skillDirOf(name)
		if dir == "" {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("[plugin] %s: removing skill %q failed %s: %v", pluginName, name, dir, err)
			continue
		}
		log.Printf("[plugin] %s: removed skill %q (plugin is stopped)", pluginName, name)
		// 通知 skillmgr 从索引里摘掉（复用同一事件，负载带 removed 标记）。
		r.announceSkillRemoved(pluginName, dir)
	}
}

// announceSkill 发布 skill_detected，让 skillmgr **当场**接管。
//
// 不能只依赖 skillmgr 的启动全扫：它启动早于 plugin-host 层，
// 首次部署/停止后重启插件时都扫不到（实测：skillmgr 在内核第 4 层装配后
// 10s 完成扫描，此时插件才刚被加载）。
func (r *Registry) announceSkill(pluginName, dir string) {
	r.mu.RLock()
	bus := r.evBus
	r.mu.RUnlock()
	if bus == nil {
		return
	}
	bus.Publish(&events.Event{
		Type:   events.EventSkillDetected,
		Source: pluginName,
		Payload: map[string]interface{}{
			"path":        dir,
			"from_plugin": pluginName,
			"builtin":     true,
		},
	})
}

func (r *Registry) announceSkillRemoved(pluginName, dir string) {
	r.mu.RLock()
	bus := r.evBus
	r.mu.RUnlock()
	if bus == nil {
		return
	}
	bus.Publish(&events.Event{
		Type:   events.EventSkillDetected,
		Source: pluginName,
		Payload: map[string]interface{}{
			"path":        dir,
			"from_plugin": pluginName,
			"builtin":     true,
			"removed":     true,
		},
	})
}

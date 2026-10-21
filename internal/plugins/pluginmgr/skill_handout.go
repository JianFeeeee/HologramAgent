package pluginmgr

// 随包 skill 的登记（hand-out）。
//
// ## 为什么需要它
//
// 插件与 skill 是两种分发物，此前**互不相通**：
//
//	插件  → 解压到 <plugin.dir>/<name>/，由插件注册表加载
//	skill → 放在 <data.dir>/skills/，由 skillmgr 扫描注册
//
// 于是 commondev 的 `commondev-discipline`（用这套插件的全部纪律，798 行）
// 装完插件的人**拿不到** —— 除非有人记得手动跑一次命令。
// 这就是「知识在文件里躺着，用户手上没有」的典型断链。
//
// ## 为什么要走事件，而不是直接调 skillmgr
//
// 两个插件之间不该有直接依赖（pluginmgr 不该 import skillmgr）。
// 内核已有现成的**事件式职责链**——原生 skillmgr 订阅 skill_detected
// 并接管注册（`internal/plugins/skillmgr/plugin.go`），
// 而发布者可以是任何持有完整 SDK 的内置插件。
//
// 所以这里只做**发现 + 移交**：解压后扫 `<插件目录>/skill/`，
// 对每个条目发一个 skill_detected。归属与生命周期归 skillmgr。
//
// ## 为什么只对「目录」发
//
// skillmgr 的 loadOne 要求目录里有 SKILL.md 或 skill.json。
// 直接对目录发是它已有的契约（clawhubadapter 也是这么发的）。
//
// ## 为什么失败不阻断安装
//
// 插件能装上、能跑，是主要目的；skill 登记是附带收益。
// 但**不静默**：每个失败都记日志并回报在安装结果里。

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// SkillDirName 是插件包里约定存放 skill 的目录名。
//
// 与 `rawfile/` 并列：rawfile 是"任意随包资源"，skill 是"要登记给
// skillmgr 的纯 SKILL 条目"。分开的理由是**意图不同** ——
// 内核需要知道哪些是要注册的，靠子路径名去猜（如 rawfile/skills/）
// 属于隐式约定，正是本仓反复吃过亏的模式。
const SkillDirName = "skill"

// handOutPluginSkills 扫 `<插件目录>/skill/*` 并把每个条目移交 skillmgr。
//
// 返回 (成功移交数, 失败清单)。失败不阻断安装，但必须回报。
func (p *Plugin) handOutPluginSkills(pluginDir, pluginName string) (int, []string) {
	if p.sdk == nil {
		return 0, nil
	}
	root := filepath.Join(pluginDir, SkillDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		// 没有 skill/ 目录是**常态**（绝大多数插件不带 skill），
		// 不是错误 —— 不记日志、不回报。
		return 0, nil
	}

	var failed []string
	n := 0
	for _, e := range entries {
		name := e.Name()
		// 跳过隐藏文件与占位（.gitkeep/.gitignore 等工程脚手架）
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, name)
		// ★ 只发**看起来是纯 SKILL** 的条目。
		//
		// 不检查的话，一个误放在 skill/ 下的 sidecar 目录（含 main.js）
		// 会被 skillmgr 拒收（它的 loadOne 要求 SKILL.md/skill.json），
		// 于是每次安装都报一条无意义的失败。在源头过滤更干净。
		if !looksLikeNativeSkill(path) {
			log.Printf("[pluginmgr] %s: %s/%s 缺 SKILL.md 或 skill.json，跳过登记",
				pluginName, SkillDirName, name)
			failed = append(failed, name+"（缺 SKILL.md 或 skill.json）")
			continue
		}

		// Source 用插件名：skillmgr 只按 path 处理，Source 只供日志/溯源。
		p.sdk.Publish(&events.Event{
			Type:   events.EventSkillDetected,
			Source: p.name,
			Payload: map[string]interface{}{
				"path": path,
				// 额外字段不改变 skillmgr 的行为（它只读 path），
				// 但让日志能看出这条 skill 是谁带来的。
				"from_plugin": pluginName,
			},
			Timestamp: time.Now().Unix(),
		})
		log.Printf("[pluginmgr] %s: 已移交 skill %s 给 skillmgr", pluginName, name)
		n++
	}
	return n, failed
}

// looksLikeNativeSkill 判断目录是否是 skillmgr 认得的纯 SKILL 条目。
//
// 与 skillmgr.isNativeSkillPath 的判据保持一致（SKILL.md 或 skill.json），
// 但**故意不重复它的排除逻辑**（main.js/openclaw.plugin.json 等）：
// 那些属于兼容层的职责划分，pluginmgr 不该复刻。这里只做"显然不是 skill
// 就别发"的粗筛，精判留给归属者 —— 这样两边的判据不会漂移成两份。
func looksLikeNativeSkill(dir string) bool {
	for _, f := range []string{"SKILL.md", "skill.json"} {
		if st, err := os.Stat(filepath.Join(dir, f)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// reHandOutAllPluginSkills 在**启动时**重新登记所有已装插件的随包 skill。
//
// ## 为什么必须有这个（实测发现的真缺陷）
//
// 移交只发生在**安装那一刻**（installFromData → handOutPluginSkills）。
// 而 skill 在**插件目录**里（`<plugin.dir>/<name>/skill/`），不在
// skillmgr 的扫描目录（`<data.dir>/skills/`）里。于是：
//
//	装插件 → skill 注册 ✅
//	  ↓ worker 重启（部署、崩溃恢复、热更新）
//	skillmgr 只扫 skillsDir → **skill 消失** ❌
//
// 实测：2026-10-15 装完 commondev 后 skill 正常注册；一次热更新
// （worker exit 42 重建）之后 `loaded SKILL: commondev-discipline`
// 不再出现 —— 用户以为 skill 还在，实际已经没了。属于**静默丢失**。
//
// ## 为什么放在 pluginmgr 而不是 skillmgr
//
// 只有 pluginmgr 知道插件装在哪（它管 pluginDir）。skillmgr 去读插件目录
// 会把"插件布局"这个知识复制到第二个地方 —— 正是本仓反复吃亏的模式。
//
// 顺序注意：pluginmgr 的 Start 与 skillmgr 的 Start 谁先跑不确定，
// 而 skillmgr 的订阅在它 Start 里建立。若 pluginmgr 先发事件、skillmgr
// 后订阅，事件就丢了。所以这里**延迟一小段再发**，并且 skillmgr 的
// loadOne 幂等 —— 重复到达无副作用。
func (p *Plugin) reHandOutAllPluginSkills() {
	dir := p.pluginDir
	if dir == "" || p.sdk == nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cnt, _ := p.handOutPluginSkills(filepath.Join(dir, e.Name()), e.Name())
		n += cnt
	}
	if n > 0 {
		log.Printf("[pluginmgr] 启动重扫：已重新移交 %d 个随包 skill（否则重启后会静默丢失）", n)
	}
}

// scheduleStartupSkillRescan 延迟重扫：等 skillmgr 的订阅就绪。
//
// 延时常量 intentionally 小（本机实测 skillmgr 的 Start 在百毫秒级完成）；
// 即便错过，loadOne 幂等 —— 下次安装或重启仍会补上，只是本次会话里晚一点。
func (p *Plugin) scheduleStartupSkillRescan() {
	go func() {
		time.Sleep(2 * time.Second)
		p.reHandOutAllPluginSkills()
	}()
}

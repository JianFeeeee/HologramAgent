package agent

// promptsec_surface.go —— 基座暴露**插件声明的系统提示词段**读取面。
//
// 段注册表是内核设施（`FacPromptSections`，stage-host 提供）。
// 类型要渲染它，但不能拿 kstate —— 用户裁定：类型包看不到内核。
// 所以由基座转一次：类型问基座要，基座问内核要。

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// PromptSections 返回插件声明的提示词段快照（未装配 ⇒ nil）。
func (a *Agent) PromptSections() []model.PromptSection {
	ks := kstate.Get()
	if ks == nil {
		return nil
	}
	impl, ok := ks.Require(kernelapi.FacPromptSections)
	if !ok {
		return nil
	}
	sections, _ := impl.(kernelapi.PromptSections)
	if sections == nil {
		return nil
	}
	return sections.Snapshot()
}

package config

import (
	"fmt"
	"strings"
)

// 人格设定的两个键：内容与「首启向导/工具已经问过」的一次性标记。
//
// 为什么需要标记：人格曾经只有 <dataDir>/personal/personal.md 一个来源且无人维护，
// 里面写死的旧版本号反过来让实例自述旧版本（v1.2.0 压测发现）。
// 现在人格是配置项（默认模板不含任何版本号），任何通道的第一次交互问一次，之后不再打扰。
const (
	// PersonaPromptKey 是人格设定内容（【人格设定】块的正文）。
	PersonaPromptKey = "core.agent.personal_prompt"
	// PersonaInitMarkerKey 是「已经问过/已确认」的一次性标记。
	PersonaInitMarkerKey = "core.internal.persona_initialized"
)

// 三种落库方式，WebUI 首启向导与内核 persona_set 工具共用。
const (
	PersonaModeDefault = "default" // 使用内置默认模板
	PersonaModeCustom  = "custom"  // 使用调用方提供的内容
	PersonaModeLater   = "later"   // 保留当前（默认）人格，只打标记不再问
)

// PersonaKV 是人格落库所需的最小读写面：GetCore/SetCore 的签名与
// 公开 SDK 的 SettingsAPI 一致，因此插件侧（WebUI）可直接传入；
// 内核直连配置注册表时用 registryKV 适配（见文件末）。
type PersonaKV interface {
	GetCore(key string) (interface{}, error)
	SetCore(key string, value interface{}) error
}

// PersonaInitializedKV 报告人格是否已确认（向导或工具已问过）。
func PersonaInitializedKV(kv PersonaKV) bool {
	if kv == nil {
		return false
	}
	v, err := kv.GetCore(PersonaInitMarkerKey)
	if err != nil {
		return false
	}
	s, _ := v.(string)
	return strings.TrimSpace(s) != ""
}

// CurrentPersonaKV 读当前人格内容；未设置时回落到内置默认模板。
func CurrentPersonaKV(kv PersonaKV) string {
	if kv == nil {
		return DefaultPersonaPrompt
	}
	if v, err := kv.GetCore(PersonaPromptKey); err == nil {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return DefaultPersonaPrompt
}

// SetPersonaKV 落库人格并打一次性标记，返回 restartRequired。
//
// 生效时机：人格在 homed 启动时载入（拼成【人格设定】块进系统提示词），
// 所以**自定义内容需重启**；default 与 later 都不改变当前已生效的人格，无需重启。
//
// 非法输入一律在打标记之前拒绝——否则向导会被跳过，用户再也没机会设。
func SetPersonaKV(kv PersonaKV, mode, content string) (restartRequired bool, err error) {
	if kv == nil {
		return false, fmt.Errorf("persona: settings unavailable")
	}
	switch mode {
	case PersonaModeDefault:
		if err := kv.SetCore(PersonaPromptKey, DefaultPersonaPrompt); err != nil {
			return false, err
		}
	case PersonaModeCustom:
		if strings.TrimSpace(content) == "" {
			return false, fmt.Errorf("persona: content required for custom mode")
		}
		if err := kv.SetCore(PersonaPromptKey, content); err != nil {
			return false, err
		}
		restartRequired = true
	case PersonaModeLater:
		// 保持当前人格（通常是默认模板），只打标记
	default:
		return false, fmt.Errorf("persona: unknown mode %q (want default|custom|later)", mode)
	}
	if err := kv.SetCore(PersonaInitMarkerKey, "1"); err != nil {
		return restartRequired, err
	}
	return restartRequired, nil
}

// RegistryPersonaStore 把配置注册表暴露成内核的 core.PersonaStore 接口
// （结构类型：方法集匹配即可，无需 import internal/agent/agent）。
type RegistryPersonaStore struct{ Reg *ConfigRegistry }

func (p RegistryPersonaStore) PersonaInitialized() bool { return PersonaInitialized(p.Reg) }

func (p RegistryPersonaStore) SetPersona(mode, content string) (bool, error) {
	return SetPersona(p.Reg, mode, content)
}

// registryKV 把内核直连的配置注册表适配成 PersonaKV。
type registryKV struct{ reg *ConfigRegistry }

func (r registryKV) GetCore(key string) (interface{}, error) {
	v, err := r.reg.Get(key)
	if err != nil || v == nil {
		// 未设置的键对 PersonaKV 语义等同「没有」，不当作错误
		if s := r.reg.GetString(key, ""); s != "" {
			return s, nil
		}
		return "", nil
	}
	return v, nil
}

func (r registryKV) SetCore(key string, value interface{}) error {
	s, ok := value.(string)
	if !ok {
		return fmt.Errorf("persona: value must be a string")
	}
	return r.reg.Set(key, s)
}

// PersonaInitialized 报告人格是否已确认（内核直连注册表）。
func PersonaInitialized(reg *ConfigRegistry) bool {
	return reg != nil && PersonaInitializedKV(registryKV{reg})
}

// SetPersona 落库人格并打一次性标记（内核直连注册表）。
func SetPersona(reg *ConfigRegistry, mode, content string) (bool, error) {
	if reg == nil {
		return false, fmt.Errorf("persona: config registry unavailable")
	}
	return SetPersonaKV(registryKV{reg}, mode, content)
}

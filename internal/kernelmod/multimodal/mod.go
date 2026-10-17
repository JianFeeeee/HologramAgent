// Package multimodal 是 multimodal 模块：多模态向量空间。
//
// 依赖 config：从配置读"用哪个 provider"。
package multimodal

import (
	"log"
	"strings"

	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
	"github.com/JianFeeeee/HomeAgent/internal/kernelmod"
	"github.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"github.com/JianFeeeee/HomeAgent/pkg/embedding"
)

// Module 提供多模态向量空间。
type Module struct{}

func (Module) Name() string       { return "multimodal" }
func (Module) Requires() []string { return []string{kernelmod.FacConfig} }
func (Module) Provides() []string { return []string{kernelmod.FacMultimodal} }

func (Module) Init(ks *kernelboot.KernelState) error {
	cfgReg := ks.CfgReg
	var multimodalSpace vector.MultimodalEmbedder
	// 这两个值只用于状态报告（healthcheck_kernel 的 onnx 段）：
	// 「配了哪个 provider」与「为什么没启用」，避免只能看到 false 却不知原因。
	var mmProviderName, mmErr string
	var closeAdapted func()

	if mmProvider := cfgReg.GetString("core.memory.multimodal_space.provider", ""); mmProvider != "" {
		mmProviderName = mmProvider
		opts := map[string]string{}
		const optPrefix = "core.memory.multimodal_space.options."
		for _, key := range cfgReg.List(optPrefix) {
			opts[strings.TrimPrefix(key, optPrefix)] = cfgReg.GetString(key, "")
		}
		provider, err := embedding.Open(mmProvider, embedding.Config{Options: opts})
		if err != nil {
			mmErr = err.Error()
			log.Printf("[homed] warning: 多模态向量 provider %q 打开失败: %v（多模态向量检索已禁用；已注册: %s）",
				mmProvider, err, strings.Join(embedding.Names(), ", "))
		} else if adapted, err := vector.AdaptProvider(provider); err != nil {
			provider.Close()
			mmErr = err.Error()
			log.Printf("[homed] warning: 多模态向量 provider %q 元数据不合法: %v（多模态向量检索已禁用）", mmProvider, err)
		} else {
			multimodalSpace = adapted
			closeAdapted = provider.Close
			info := provider.Info()
			// 指纹可能很长（模型文件哈希），日志里只取前 12 个字符便于对照。
			shortFP := info.Fingerprint
			if len(shortFP) > 12 {
				shortFP = shortFP[:12]
			}
			log.Printf("[homed] multimodal space active: provider=%s dim=%d fp=%s modalities=%v",
				mmProvider, info.Dimension, shortFP, info.Modalities)
		}
	}

	ks.MultimodalSpace = multimodalSpace
	ks.MultimodalProviderName = mmProviderName
	ks.MultimodalErr = mmErr
	if closeAdapted != nil {
		ks.AddCloser(closeAdapted)
	}
	return nil
}

func init() { kernelmod.Register(Module{}) }

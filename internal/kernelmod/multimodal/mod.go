// Package multimodal 是 multimodal 模块：多模态向量空间。
//
// 依赖 config：从配置读"用哪个 provider"。
package multimodal

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/semantic"
	"log"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
	"github.com/JianFeeeee/HologramAgent/internal/vector"
	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

// Module 提供多模态向量空间。
type Module struct{}

func (Module) Name() string       { return "multimodal" }
func (Module) Requires() []string { return []string{kernelmod.FacConfig} }
func (Module) Provides() []string {
	// ★ 两条档位同住一个模块：
	//   稠密空间（FacMultimodal，provider 为空 ⇒ 禁用）
	//   稀疏词嵌入（FacEmbedder，永远可用 —— fastText 词向量 + jieba）
	// 它们都是**语义计算能力**（docs §3.4），与记忆无关。
	return []string{kernelmod.FacMultimodal, kernelmod.FacEmbedder}
}

func (Module) Init(ks *kstate.State) error {
	cfgReg := ks.CfgReg
	var multimodalSpace vector.MultimodalEmbedder
	// 这两个值只用于状态报告（healthcheck_kernel 的 onnx 段）：
	// 「配了哪个 provider」与「为什么没启用」，避免只能看到 false 却不知原因。
	var mmProviderName, mmErr string
	var closeAdapted func()

	if mmProvider := cfgReg.GetString("core.multimodal.provider", ""); mmProvider != "" {
		mmProviderName = mmProvider
		opts := map[string]string{}
		const optPrefix = "core.multimodal.options."
		for _, key := range cfgReg.List(optPrefix) {
			opts[strings.TrimPrefix(key, optPrefix)] = cfgReg.GetString(key, "")
		}
		provider, err := embedding.Open(mmProvider, embedding.Config{Options: opts})
		if err != nil {
			mmErr = err.Error()
			log.Printf("[homed] warning: opening multimodal embedding provider %q failed: %v (multimodal vector retrieval disabled; registered: %s)",
				mmProvider, err, strings.Join(embedding.Names(), ", "))
		} else if adapted, err := vector.AdaptProvider(provider); err != nil {
			provider.Close()
			mmErr = err.Error()
			log.Printf("[homed] warning: multimodal embedding provider %q has invalid metadata: %v (multimodal vector retrieval disabled)", mmProvider, err)
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

	ks.Provide(kernelapi.FacMultimodal, multimodalSpace)

	// 稀疏词嵌入：**永远提供**（这是与稠密空间的关键差别）。
	//
	// 稠密空间缺 provider 就禁用（要 0.89GB 模型），而词嵌入是每条记忆路径的
	// 最小可用能力：图记忆的关键词抽取、文档 TF-IDF 分词、实体语义相似度、
	// L1 事件裁剪的稀疏余弦 —— 没有它整条链路退化成"什么都不相关"。
	// 模型缺失时 StaticEmbedder 自己退成 TF-IDF 词频（仍然可算余弦）。
	embedder := semantic.NewStaticEmbedder(
		strings.Split(cfgReg.GetString("core.agent.embedding_model_path", ""), ",")...)
	ks.Provide(kernelapi.FacEmbedder, embedder)
	ks.Provide(kernelapi.FacMultimodalStatus, &kernelapi.MultimodalStatus{Provider: mmProviderName, Error: mmErr})
	if closeAdapted != nil {
		ks.AddCloser(closeAdapted)
	}
	return nil
}

func init() { kernelmod.Register(Module{}) }

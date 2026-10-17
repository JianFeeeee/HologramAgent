// provider 出向装饰器：把明文替换成变量别名。
//
// 落点：`ProviderManager.Register`（唯一入口，已核 —— 见
// docs/zh/kernel-secret-firewall-design.md §5.1）。
//
// 装饰在这里而不是每个调用方，是因为 CodeGraph 交叉验证确认：
// Get / Default / OrderedProviders / ResolveForModel 全都从 Register
// 写入的那张 map 取，装饰 Register 即全覆盖。
package api

import (
	"context"
	"log"

	"github.com/JianFeeeee/HomeAgent/internal/security"
)

// secureProviderDecorator 在**发往 LLM 之前**把消息里的明文换成别名。
//
// ### 为何要逐条消息扫，而不是拼起来扫
//
// 拼接会跨越消息边界产生误报：上一条的结尾与下一条的开头恰好连成
// 一个 token 形状。逐条扫不可能有这种跨边界命中。
//
// ### 为何同时处理 Content 与 Blocks
//
// 内核的消息有多模态 Blocks（截图描述等）。只扫 Content 会漏掉
// 图片 URL 之类的载荷。这里两者都过一遍。
type secureProviderDecorator struct {
	inner Provider
	store *security.Store
}

// WrapProviderForSecurity 给 provider 套上出向改写。
//
// store 为 nil 时原样返回（安全模块未启用 ⇒ **零成本**）。
func WrapProviderForSecurity(p Provider, store *security.Store) Provider {
	if p == nil || store == nil {
		return p
	}
	return &secureProviderDecorator{inner: p, store: store}
}

func (d *secureProviderDecorator) Name() string          { return d.inner.Name() }
func (d *secureProviderDecorator) MaxContextTokens() int { return d.inner.MaxContextTokens() }

// scrubRequest 逐条消息改写（**原地**改，因为 req 是要发出去的那一份）。
func (d *secureProviderDecorator) scrubRequest(req *CompletionRequest) {
	if req == nil {
		return
	}
	for i := range req.Messages {
		m := &req.Messages[i]

		if m.Content != "" {
			out, evts := d.store.Replace(m.Content, "llm_out:"+m.Role, security.Patterns)
			if len(evts) > 0 {
				m.Content = out
				d.logEvts(evts)
			}
		}

		// 多模态 Blocks：文本块要扫；image_url 里的 data URL 也要扫
		// （截图/文档里可能带凭据）。
		for j := range m.Blocks {
			b := &m.Blocks[j]
			if b.Text != "" {
				if out, evts := d.store.Replace(b.Text, "llm_out:"+m.Role+":block", security.Patterns); len(evts) > 0 {
					b.Text = out
					d.logEvts(evts)
				}
			}
			if b.ImageURL != nil && b.ImageURL.URL != "" {
				if out, evts := d.store.Replace(b.ImageURL.URL, "llm_out:"+m.Role+":image", security.Patterns); len(evts) > 0 {
					b.ImageURL.URL = out
					d.logEvts(evts)
				}
			}
		}
	}
}

// logEvts 记录拦截（**只记别名，绝不记明文**）。
func (d *secureProviderDecorator) logEvts(evts []security.Evt) {
	for _, e := range evts {
		log.Printf("[security] 出向拦截 %s ← 模式 %s，%d 字节，合并=%v，位置 %s",
			e.Alias, e.Kind, e.Len, e.Merged, e.Where)
	}
}

func (d *secureProviderDecorator) Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	d.scrubRequest(req)
	return d.inner.Chat(ctx, req)
}

func (d *secureProviderDecorator) ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	d.scrubRequest(req)
	return d.inner.ChatStream(ctx, req)
}

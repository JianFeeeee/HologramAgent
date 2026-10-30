package proc

import (
	"encoding/json"
	"fmt"
)

// handleInject 处理 IO 注入面：文本 / 打断 / 同步 / 媒体块，以及 SetToolBlocks。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleInject(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- IO 注入（原 case 5/6/7/47）----
	//
	// 注入标志位（no_memory / context_policy）由插件在调用点声明，默认
	// 记入记忆 + 不裁剪。策略值在入口校验：静默降级成 none 会让调用方
	// 以为自己声明的裁剪在生效。
	case MethodIOInjectText:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectText", p.ContextPolicy); err != nil {
			return nil, err
		}
		if err := validateRecallPolicy("io.injectText", p.RecallPolicy); err != nil {
			return nil, err
		}
		h.sdk.InjectTextOpts(p.Source, p.Channel, h.resolveText(p), pubSdkInjectOpts(p))
		return nil, nil
	case MethodIOInjectInterrupt:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectInterrupt", p.ContextPolicy); err != nil {
			return nil, err
		}
		if err := validateRecallPolicy("io.injectInterrupt", p.RecallPolicy); err != nil {
			return nil, err
		}
		h.sdk.InjectInterruptTextOpts(p.Source, p.Channel, h.resolveText(p), pubSdkInjectOpts(p))
		return nil, nil
	case MethodIOInjectTextNoMem:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectTextNoMem", p.ContextPolicy); err != nil {
			return nil, err
		}
		if err := validateRecallPolicy("io.injectTextNoMem", p.RecallPolicy); err != nil {
			return nil, err
		}
		// 旧 RPC 语义就是「不进记忆」，显式标志位只可能再叠上 context_policy。
		h.sdk.InjectTextOpts(p.Source, p.Channel, h.resolveText(p), pubSdkInjectOpts(p))
		return nil, nil
	case MethodIOInjectSync:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectInputSync", p.ContextPolicy); err != nil {
			return nil, err
		}
		if err := validateRecallPolicy("io.injectInputSync", p.RecallPolicy); err != nil {
			return nil, err
		}
		reply := h.sdk.InjectInputSyncOpts(p.Source, p.Channel, h.resolveText(p), pubSdkInjectOpts(p))
		return map[string]interface{}{"reply": reply}, nil

	case MethodIOInjectMedia:
		var p injectMediaParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectMedia", p.ContextPolicy); err != nil {
			return nil, err
		}
		if err := validateRecallPolicy("io.injectMedia", p.RecallPolicy); err != nil {
			return nil, err
		}
		blocks, err := h.resolveBlocks(p)
		if err != nil {
			return nil, err
		}
		h.sdk.InjectInputMediaOpts(p.Source, p.Channel, p.Text, blocks, pubSdkInjectOpts(p.asInjectParams()))
		return nil, nil

	case MethodIOInjectMediaSync:
		var p injectMediaParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectMediaSync", p.ContextPolicy); err != nil {
			return nil, err
		}
		if err := validateRecallPolicy("io.injectMediaSync", p.RecallPolicy); err != nil {
			return nil, err
		}
		blocks, err := h.resolveBlocks(p)
		if err != nil {
			return nil, err
		}
		reply := h.sdk.InjectInputMediaSyncOpts(p.Source, p.Channel, p.Text, blocks, pubSdkInjectOpts(p.asInjectParams()))
		return map[string]interface{}{"reply": reply}, nil

	case MethodIOInjectInterruptMedia:
		var p injectMediaParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectInterruptMedia", p.ContextPolicy); err != nil {
			return nil, err
		}
		if err := validateRecallPolicy("io.injectInterruptMedia", p.RecallPolicy); err != nil {
			return nil, err
		}
		blocks, err := h.resolveBlocks(p)
		if err != nil {
			return nil, err
		}
		h.sdk.InjectInterruptMediaOpts(p.Source, p.Channel, p.Text, blocks, pubSdkInjectOpts(p.asInjectParams()))
		return nil, nil

	// ---- 多模态注入 ----
	//
	// 之前这里是桩：返回“待共享段二进制通道落地”。后果是**子进程插件调
	// SetToolBlocks 必然失败**（模板只 log 一行），只有内置插件能用。
	// 现在媒体块经共享内存传递，该能力对两种插件形态等价。
	case MethodIOSetToolBlocks:
		var p injectMediaParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		blocks, err := h.resolveBlocks(p)
		if err != nil {
			return nil, err
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("%s: blocks 为空", method)
		}
		h.sdk.SetToolBlocks(blocks)
		return nil, nil
	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

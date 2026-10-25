package proc

// cancelProbePluginSource 是「取消是否真的送达」的观察装置。
//
// 设计要点：
//   - probe_slow 用 **ToolHandlerCtx** 注册，观察 ctx 并记录结果；
//   - 观察到取消后把状态写进包级变量（同进程，测试可直接读回）；
//   - probe_state 返回该变量，供测试轮询。
//
// 之所以能这么写：模板真的会把 call.cancel 变成 handler 的 ctx cancel
// （见 hmapdev 模板的 trackInflight/cancelInflight）。若那一环没接上，
// probe_slow 永远观察不到取消，测试自然红。
const cancelProbePluginSource = `package main

import (
	"context"
	"sync"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type probePlugin struct {
	name string
	mu   sync.Mutex
	last string
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &probePlugin{name: name}, nil
}

func (p *probePlugin) Name() string { return p.name }

func (p *probePlugin) Stop() error { return nil }

func (p *probePlugin) Start(s *sdk.PluginSDK) error {
	s.RegisterToolCtx("probe_slow", sdk.ToolDef{
		Description: "长任务，用于验证取消是否送达",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.last = "canceled"
			p.mu.Unlock()
			return "canceled", ctx.Err()
		case <-time.After(30 * time.Second):
			p.mu.Lock()
			p.last = "timeout"
			p.mu.Unlock()
			return "finished", nil
		}
	})

	// probe_block：**用旧签名**注册，完全不看 ctx。
	//
	// 这个工具是"取消后不回应答"那条路径的**唯一**观察点：
	// Ctx 版 handler 会因为 ctx 取消而自己提前返回，从而绕过
	// "跑完后检查是否已被取消" 那段代码，测不到它。
	// 旧签名 handler 跑满 300ms 后才返回——此时内核早已取消，
	// 运行时必须靠 inflightCall.canceled 拦住应答。
	s.RegisterTool("probe_block", sdk.ToolDef{
		Description: "旧签名长任务：不看 ctx，用于验证取消后不回应答",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		time.Sleep(300 * time.Millisecond)
		return "finished", nil
	})

	s.RegisterTool("probe_state", sdk.ToolDef{
		Description: "读回上一次长任务的观察结果",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.last == "" {
			return "idle", nil
		}
		return p.last, nil
	})
	return nil
}
`

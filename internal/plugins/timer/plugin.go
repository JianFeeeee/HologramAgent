package timer

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/plugin"
	safeclose "github.com/JianFeeeee/HomeAgent/internal/safeclose"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta("timer", "定时任务", "Timer")
	plugin.RegisterFactory("timer", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name   string
	mu     sync.Mutex
	wg     sync.WaitGroup
	stopCh chan struct{}
	maxDur time.Duration
}

type timerTask struct {
	id      int
	dur     time.Duration
	message string
	doneAt  time.Time
	s       *sdk.PluginSDK
}

func New(name string) *Plugin {
	return &Plugin{name: name, stopCh: make(chan struct{})}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.maxDur = 24 * time.Hour
	// 停止清理（取消倒计时）交由 stop handler：内核在调用 Stop() 之前执行。
	// 幂等（本仓 close 纪律）：stop handler 由内核调用；若将来多一条
	// 停机路径（重载/热更新）就会二次 close ⇒ panic。
	s.RegisterStopHandler(safeclose.SafeFunc(p.stopCh))
	// 删除清理：移除插件自身配置（删除专用回调，重载不触发）。
	s.RegisterOnRemoveHandler(func() {
		if err := s.Settings().Remove("max_duration"); err != nil {
			log.Printf("[timer] onRemove cleanup: %v", err)
		}
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "max_duration", Type: "string", DisplayName: "最大定时时长",
		Description: "允许设置的最大定时时长，例如 24h, 7d, 1h（默认 24h）",
		Default:     "24h",
	})
	if v, _ := s.Settings().Get("max_duration"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				p.maxDur = d
			}
		}
	}

	// timer 通道：定时器到点经它注入 agent（见本文件 InjectInterruptTextOpts 调用）。
	_ = s.RegisterInputChannel("timer", sdk.ChannelDef{})
	s.RegisterTool("timer_set", sdk.ToolDef{
		Name:        "timer_set",
		Description: "设置一个定时提醒。倒计时结束后通过中断通道通知 agent。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"duration": map[string]interface{}{
					"type":        "string",
					"description": "持续时间，例如 5s, 2m, 1h",
				},
				"message": map[string]interface{}{
					"type":        "string",
					"description": "提醒内容",
				},
			},
			"required": []string{"duration", "message"},
		},
		// 写定时器：改 p.timers（持 p.mu），按序更可预期
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		durStr, _ := args["duration"].(string)
		message, _ := args["message"].(string)
		if durStr == "" {
			return map[string]interface{}{"error": "duration is required"}, nil
		}
		if message == "" {
			return map[string]interface{}{"error": "message is required"}, nil
		}

		dur, err := time.ParseDuration(durStr)
		if err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("invalid duration %q: %v", durStr, err)}, nil
		}
		if dur > p.maxDur {
			return map[string]interface{}{"error": fmt.Sprintf("duration %v exceeds max %v", dur, p.maxDur)}, nil
		}

		p.mu.Lock()
		p.wg.Add(1)
		p.mu.Unlock()

		go func() {
			defer p.wg.Done()
			select {
			case <-time.After(dur):
				log.Printf("[timer] firing: %s (%s later)", message, dur)
				// NoMemory：定时提醒是系统通知，不是记忆内容。
				// PriorityL3：定时器是“时钟那种实时工作”——到点就该处理，
				// 比 QQ 那类可无限等待的异步消息高（L3 vs L1）。
				s.InjectInterruptTextOpts("timer", "timer", fmt.Sprintf("timer: %s", message),
					sdk.InjectOptions{NoMemory: true, Priority: sdk.PriorityL3})
			case <-p.stopCh:
				log.Printf("[timer] cancelled: %s", message)
			}
		}()

		doneAt := time.Now().Add(dur)
		return map[string]interface{}{
			"status":   "timer_set",
			"duration": durStr,
			"message":  message,
			"done_at":  doneAt.Format(time.RFC3339),
		}, nil
	})

	return nil
}

func (p *Plugin) Stop() error {
	p.wg.Wait()
	return nil
}

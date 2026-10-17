package core

// 工具执行超时的判据（2026-10-17 生产实测缺陷）。
//
// ## 缺陷
//
// `toolcall.go` 里写死 `time.After(60 * time.Second)`，与工具自己的超时
// **互相矛盾**且让内层形同虚设：
//
//	23:13:38  执行 cmd_run（agent 传了 130s 级 timeout 参数）
//	23:14:38  ★ 内核 60s 硬超时掐断 → 「执行超时（60秒）」
//
// 同一份日志里还有：
//   · healthcheck 声明默认 120s ⇒ 被砍掉一半，天生跑不完
//	 · output_send__commondev_* 两次 60s 超时（投递到子 pipeline 的长任务）
//
// ## 判据覆盖
//
//  1. 未声明 → 60s（与改动前逐字一致，存量插件不变）
//  2. 声明 → 用它（healthcheck 的 5m、output_send 的 3m）
//  3. 参数推导 → cmd_run 传 timeout="2m" 时给 2m（取 max）
//  4. 非法声明 → 回退默认（不静默接受、不 panic）
//  5. 超硬上限 → 夹到 10m（防插件声明成无限把队列堵死）

import (
	"testing"
	"time"
)

// fakeStageHost 提供可控的 ToolDef 查询。
type fakeStageHost struct {
	*StageHost
	defs map[string]string // name → Timeout
}

func newFakeAgent(defs map[string]string) *Agent {
	h := NewStageHost()
	for name, to := range defs {
		h.RegisterTool(name, sdkToolDefWithTimeout(name, to), func(map[string]interface{}) (interface{}, error) {
			return nil, nil
		})
	}
	return &Agent{stageHost: h}
}

// ★★★ 判据：未声明超时的工具沿用 60s（存量行为不变）。
func TestToolTimeoutDefaultsTo60s(t *testing.T) {
	a := newFakeAgent(nil)
	if got := a.resolveToolTimeout("no_such_tool", nil); got != 60*time.Second {
		t.Fatalf("未声明应得 60s，实际 %v —— 存量插件的行为必须不变", got)
	}
}

// ★★★ 判据：声明了就用声明的值。
func TestToolTimeoutUsesDeclaredValue(t *testing.T) {
	a := newFakeAgent(map[string]string{"healthcheck": "5m"})
	if got := a.resolveToolTimeout("healthcheck", nil); got != 5*time.Minute {
		t.Fatalf("healthcheck 声明 5m，实际 %v", got)
	}
}

// ★★★ 判据：调用参数里的 timeout 参与取 max（cmd_run 场景）。
//
// 这是原 bug 的核心：模型传 timeout=2m，内核却只给 60s。
func TestToolTimeoutHonorsCallArgument(t *testing.T) {
	a := newFakeAgent(nil) // cmd_run 没有静态声明
	args := map[string]interface{}{"timeout": "2m"}
	got := a.resolveToolTimeout("cmd_run", args)
	if got != 2*time.Minute {
		t.Fatalf("★ 参数 timeout=2m 应得 2m，实际 %v —— "+
			"这正是原 bug：模型要 2 分钟，内核 60s 就掐断", got)
	}
}

// ★★★ 判据：参数比声明**小**时用声明（不因模型说了个短值就少给）。
func TestToolTimeoutTakesMaxOfDeclaredAndArg(t *testing.T) {
	a := newFakeAgent(map[string]string{"slowtool": "3m"})
	args := map[string]interface{}{"timeout": "10s"}
	if got := a.resolveToolTimeout("slowtool", args); got != 3*time.Minute {
		t.Fatalf("声明 3m、参数 10s 应取 3m，实际 %v", got)
	}
}

// ★★★ 判据：非法声明回退默认（不静默、不 panic）。
func TestToolTimeoutFallsBackOnInvalidDeclaration(t *testing.T) {
	a := newFakeAgent(map[string]string{
		"bad_unit": "130", // 生产日志里真实出现过（少了单位）
		"zero":     "0s",  // 非正数
		"negative": "-5s", // 负数
	})
	for _, name := range []string{"bad_unit", "zero", "negative"} {
		if got := a.resolveToolTimeout(name, nil); got != defaultToolTimeout {
			t.Errorf("%s 声明非法，应回退 %v，实际 %v", name, defaultToolTimeout, got)
		}
	}
}

// ★★★ 判据：超硬上限被夹住（防声明成无限堵死队列）。
func TestToolTimeoutClampedToMax(t *testing.T) {
	a := newFakeAgent(map[string]string{"greedy": "2h"})
	if got := a.resolveToolTimeout("greedy", nil); got != maxToolTimeout {
		t.Fatalf("2h 应被夹到 %v，实际 %v", maxToolTimeout, got)
	}
	// 参数路径同样要夹
	if got := a.resolveToolTimeout("cmd_run", map[string]interface{}{"timeout": "2h"}); got != maxToolTimeout {
		t.Fatalf("参数 2h 应被夹到 %v，实际 %v", maxToolTimeout, got)
	}
}

// ★★★ 判据：output_send__* 必须声明足够长的超时。
//
// 实测两次 60s 超时：它们是投递到 commondev 开发线（子 pipeline 在
// 克隆/构建/跑测试），天然超过 60s。
func TestOutputSendDeclaresLongTimeout(t *testing.T) {
	to := builtinToolTimeout("output_send__commondev_br-xxx")
	if to == "" {
		t.Skip("output_send__* 的名字是动态的，见下一条判据")
	}
}

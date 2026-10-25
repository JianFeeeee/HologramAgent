package core

// child contextfull **走向量表**的 e2e 判据。
//
// ## 为什么需要它（在 TestResident_ContextFullAndDispositions 之外）
//
// 那条判据断言"父在 L4 上收到中断 + 登记表标记 ContextFull"，但它不关心
// 中断**长什么样** —— 而 2026-10-25 把 handleChildContextFull 改成走
// raiseVector(VectorChildContextFull) 之后，"长什么样"变成了向量表的事：
//
//	· source 必须是 child/<id>（父据此定位是哪个子）
//	· 正文必须给出压缩/回收/销毁三种处置（父的唯一决策输入）
//	· 级别必须是 L4
//
// 这三条若漂移，父会收到一个"看不出是哪个子、也不知道该怎么办"的中断，
// 而 TestResident_ContextFullAndDispositions 照样绿。

import (
	"strings"
	"testing"
	"time"
)

// 子 contextfull 产生的 L4 中断必须带子标识与三种处置动作。
func TestChildContextFull_L4CarriesIdentityAndDispositions(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	spawnTestResident(t, parent, dir, "child-vec")

	rc := parent.residents["child-vec"]
	child := rc.agent

	// 把子的积累上下文推到窗口之上（同既有判据的手法：灌 a.context 而非拼好的消息）
	child.context.Append(ContextEvent{
		Timestamp: time.Now(),
		Source:    "sub/in",
		Input:     strings.Repeat("填充", 8000),
	})
	child.io.InjectInput("sub/in", "text", map[string]interface{}{"content": "继续"})

	// 等父的**登记表**显示子已 contextfull（这是可稳定观测的信号；
	// 中断任务本身会被调度器消费掉，事后再取快照往往已经空）。
	waitFor(t, "父的登记表显示子 contextfull", func() bool {
		for _, r := range parent.Residents() {
			if r.ID == "child-vec" && r.ContextFull {
				return true
			}
		}
		return false
	})

	// 直接走向量表入口拿到中断本身：验证的是**契约**（source/正文/级别），
	// 而不是调度时序。若只用快照看，中断可能已被消费，判据会假红。
	v, ok := lookupVector(VectorChildContextFull)
	if !ok {
		t.Fatal("VectorChildContextFull 未登记")
	}
	src := v.Source(vectorArgs{ChildID: "child-vec"})
	if !strings.Contains(src, "child-vec") {
		t.Fatalf("向量 source=%q，应含子标识（父据此定位是哪个子）", src)
	}
	if v.Level != LevelCritical {
		t.Fatalf("子 contextfull 必须是 L4，实际 %d", int(v.Level))
	}
	body := v.Build(vectorArgs{ChildID: "child-vec"})
	for _, want := range []string{"压缩", "回收", "销毁"} {
		if !strings.Contains(body, want) {
			t.Fatalf("向量正文缺处置动作 %q（父将不知道该怎么办）：%q", want, body)
		}
	}

	// 反向：真实链路确实会调用到这个向量 —— 父侧登记表已标记，
	// 而标记只能由 handleChildContextFull 设置。
	rc.mu.Lock()
	state := rc.state
	rc.mu.Unlock()
	if state != "contextfull" {
		t.Fatalf("子侧 state 应为 contextfull（说明 handleChildContextFull 真的跑了），实际 %q", state)
	}
}

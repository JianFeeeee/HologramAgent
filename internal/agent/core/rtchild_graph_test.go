package core

// 阶段 R3 判据：rtchild 的**图记忆接入**（参照驻留子的轻量内核形态）。
//
// ## 为什么这组判据必须存在
//
// 接入前 rt 的 carrier 既没有 Memory 也没有 LightMemory ⇒ `graphMem() == nil`
// ⇒ memory_recall / memory_commit 这类图记忆共同面工具在 rt 里**全部不可达**。
// 而 rt 恰恰需要它：通话里用户会提到人、地点、约定、偏好，那些正是图记忆
// 该记住的东西；rt 的 history 只在**这一通电话**内有效，通话结束就只剩留档。
//
// 不接图记忆 ⇒ 每通电话都从零开始。这不是"少一个优化"，是丢失能力。
//
// ## 判据为什么要走 SpawnRTChild
//
// 与 provider 那组同理（engineering-lessons.md §1.2）：直构 Agent 会让判据假绿。
// 装配（有哪些句柄、可写还是只读）是在 SpawnRTChild 里决定的。

import (
	"path/filepath"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// rtGraphHost 造一个带真实图记忆库的父 agent。
func rtGraphHost(t *testing.T) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("打开父图库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pio := agentIO.NewIOManager()
	host := New(AgentConfig{
		ID: "g-host", IO: pio, Memory: db, DataDir: dir,
	})
	if err := pio.RegisterDevice(&rtRecordingDevice{name: "gout"}); err != nil {
		t.Fatalf("注册输出设备: %v", err)
	}
	for _, n := range []string{"gout", "gin"} {
		if err := pio.ChannelRegistry().Register(agentIO.InputChannel{
			Name: n, Plugin: "gtest",
			Capabilities: agentIO.CapText | agentIO.CapRT,
		}); err != nil {
			t.Fatalf("注册通道 %s: %v", n, err)
		}
	}
	return host, dir
}

// TestRTChild_HasGraphMemory 是核心判据：rt 必须拿到图记忆共同面。
//
// 变红条件：去掉 SpawnRTChild 里的 LightMemory 装配 ⇒ graphMem() == nil ⇒ 红。
func TestRTChild_HasGraphMemory(t *testing.T) {
	host, _ := rtGraphHost(t)
	defer host.StopRTChildren()

	id, err := host.SpawnRTChild(RTChildOptions{
		ID: "g-rt", Input: "gin", Output: "gout",
		TempPath: filepath.Join(t.TempDir(), "rt.db"),
	})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}

	carrier := host.rtChildOf(id).agent
	if carrier.graphMem() == nil {
		t.Fatal("rtchild 的 graphMem() == nil —— 图记忆没有接入，" +
			"memory_recall / memory_commit 在通话里全部不可达")
	}
	// 必须是轻量内核形态：整理面保持 nil（否则 rt 会把记忆整理流水线也拉起来）。
	if carrier.memory != nil {
		t.Fatalf("rtchild 拿到了完整图记忆库（memory != nil）—— " +
			"那会让记忆整理面（块/媒体/蒸馏）对 rt 可达，与「轻量内核」形态不符")
	}
}

// TestRTChild_GraphMemoryIsReadOnlyByDefault 钉住「默认只读」。
//
// 理由：一次通话几十轮高频写入会把图谱灌满噪声
// （与 rtvoice 插件给 inputch 标 NoMemory 同一条考虑）。
// 变红条件：把 AllowGraphWrite 默认成 true ⇒ 红。
func TestRTChild_GraphMemoryIsReadOnlyByDefault(t *testing.T) {
	host, _ := rtGraphHost(t)
	defer host.StopRTChildren()

	id, err := host.SpawnRTChild(RTChildOptions{
		ID: "ro-rt", Input: "gin", Output: "gout",
		TempPath: filepath.Join(t.TempDir(), "rt.db"),
	})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}
	lm, ok := host.rtChildOf(id).agent.graphMem().(*memory.LightMemory)
	if !ok {
		t.Fatalf("rtchild 的图记忆不是 *memory.LightMemory（轻量内核形态）")
	}
	if lm.AllowWrite() {
		t.Fatal("rtchild 默认允许写图记忆 —— 通话高频写入会把图谱灌满噪声")
	}
}

// TestRTChild_GraphWriteWhenEnabled 钉住显式开启后确实可写。
//
// 否则"能开"就只是个没被读取的字段（本仓已有过多例死字段）。
func TestRTChild_GraphWriteWhenEnabled(t *testing.T) {
	host, _ := rtGraphHost(t)
	defer host.StopRTChildren()

	tempPath := filepath.Join(t.TempDir(), "rw.db")
	id, err := host.SpawnRTChild(RTChildOptions{
		ID: "rw-rt", Input: "gin", Output: "gout",
		TempPath:        tempPath,
		AllowGraphWrite: true,
	})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}
	lm, ok := host.rtChildOf(id).agent.graphMem().(*memory.LightMemory)
	if !ok {
		t.Fatal("rtchild 的图记忆不是 *memory.LightMemory")
	}
	if !lm.AllowWrite() {
		t.Fatal("AllowGraphWrite=true 但 LightMemory 仍拒绝写 —— 开关没被读取")
	}
	if lm.Temp() == nil {
		t.Fatal("允许写但没有 temp 实例 —— 写入会无处落地")
	}
	// 真写一条，证明链路通（不是只有 bool 对）。
	if _, _, err := lm.Commit(nil, "s1", 1); err != nil {
		t.Fatalf("允许写却 Commit 失败: %v", err)
	}
}

// TestRTChild_GraphMemoryHandlesAreClosed 钉住「不泄漏 SQLite 句柄」。
//
// 变红条件：Dispose 里不关 light/mainRO ⇒ 句柄随 create/stop 累积。
// 做法：连续建停多轮，断言 temp 文件可被重新创建/删除（Windows 之外
// 更可靠的判据是 light/mainRO 字段被置 nil）。
func TestRTChild_GraphMemoryHandlesAreClosed(t *testing.T) {
	host, _ := rtGraphHost(t)
	defer host.StopRTChildren()

	for i := 0; i < 3; i++ {
		id := "cycle-rt"
		rc := host.rtChildOf(id)
		if rc != nil {
			t.Fatalf("第 %d 轮：上一轮的 rtchild 仍在登记表里", i)
		}
		_, err := host.SpawnRTChild(RTChildOptions{
			ID: id, Input: "gin", Output: "gout",
			TempPath:        filepath.Join(t.TempDir(), "cycle.db"),
			AllowGraphWrite: true,
		})
		if err != nil {
			t.Fatalf("第 %d 轮 SpawnRTChild: %v", i, err)
		}
		rc = host.rtChildOf(id)
		if rc == nil {
			t.Fatalf("第 %d 轮：创建后取不到实例", i)
		}
		if err := host.StopRTChild(id); err != nil {
			t.Fatalf("第 %d 轮 StopRTChild: %v", i, err)
		}
		if rc.light != nil || rc.mainRO != nil {
			t.Fatalf("第 %d 轮停止后图记忆句柄未释放（light=%v mainRO=%v）—— "+
				"会随 create/stop 累积泄漏", i, rc.light != nil, rc.mainRO != nil)
		}
	}
}

// TestRTChild_NoGraphMemoryWhenParentHasNone 钉住降级不 panic。
//
// 父没有图记忆（独立工具/单测常见）时，rt 应当照常创建，
// 只是 graphMem() == nil。变红条件：装配处对 nil 解引用 ⇒ 红。
func TestRTChild_NoGraphMemoryWhenParentHasNone(t *testing.T) {
	pio := agentIO.NewIOManager()
	host := New(AgentConfig{ID: "nog-host", IO: pio}) // 无 Memory
	if err := pio.RegisterDevice(&rtRecordingDevice{name: "nout"}); err != nil {
		t.Fatalf("注册输出设备: %v", err)
	}
	for _, n := range []string{"nout", "nin"} {
		if err := pio.ChannelRegistry().Register(agentIO.InputChannel{
			Name: n, Plugin: "ntest", Capabilities: agentIO.CapText | agentIO.CapRT,
		}); err != nil {
			t.Fatalf("注册通道 %s: %v", n, err)
		}
	}
	defer host.StopRTChildren()
	id, err := host.SpawnRTChild(RTChildOptions{ID: "n-rt", Input: "nin", Output: "nout"})
	if err != nil {
		t.Fatalf("父无图记忆时 SpawnRTChild 不该失败: %v", err)
	}
	if gm := host.rtChildOf(id).agent.graphMem(); gm != nil {
		t.Fatalf("父无图记忆，子却拿到了 graphMem()=%v", gm)
	}
}

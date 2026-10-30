package core

// 判据：驻留子销毁时残余任务的**四种处置**，以及它持有通道的**三种处置**。
//
// 用户 2026-10-30 要求：
//
//	「对于驻留式子 agent 销毁时，残余任务主 agent 应当选择**暂存在对应通道**、
//	  **丢弃**、或**接手**。原本驻留子持有的通道也应该支持**挂起或接回**主 agent」
//
// ## 为什么这事不能交给内核悄悄决定
//
// 已经在 ResidentPolicy 的注释里钉过：吞掉一条用户消息比多处理一条更糟。
// 新增的 stash/suspend 把这件事细分到"不丢、也不立刻压给父"。

import (
	"path/filepath"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// pushToChild 把一条输入直接塞进驻留子的待处理队列（模拟通道投递）。
//
// 不用真的走通道投递：本文件验的是**销毁时如何处置队列里的残余任务**，
// 而不是路由本身（路由已有独立判据）。直接入队让"这条任务来自哪个通道"
// 可控。
func pushToChild(t *testing.T, root *Agent, childID, channel, text string) {
	t.Helper()
	root.residentMu.Lock()
	rc := root.residents[childID]
	root.residentMu.Unlock()
	if rc == nil || rc.agent == nil {
		t.Fatalf("驻留子 %s 不存在", childID)
	}
	evt := mustTextEvent(channel, text)
	evt.OutputChannel = channel
	rc.agent.sched.enqueue(newInputTask(evt))
}

// spawnQuietResident 创建驻留子但**不注入 TaskPrompt**。
//
// ★ 本文件不能用 spawnTestResident：它会带 TaskPrompt，而那条提示词是
// **异步**投进子队列的（探针实测：它在 parkChild 清空队列**之后**才到达，
// 约 4/40 的概率混进来）。后果是残余条数变成 3 而非 2，
// 判据偶发红 —— 而它测的根本不是那条提示词。
//
// 本文件验的是**销毁时的处置逻辑**，让子的初始队列为空才是确定的起点。
func spawnQuietResident(t *testing.T, root *Agent, dir, id string, inputChs ...string) ResidentInfo {
	t.Helper()
	info, err := root.SpawnResident(ResidentOptions{
		ID:       id,
		InputChs: inputChs,
		TempPath: filepath.Join(dir, "residents", id, "graph.db"),
	})
	if err != nil {
		t.Fatalf("创建驻留子失败: %v", err)
	}
	return info
}

// parkChild 停住驻留子的调度器，让"残余任务"处于**确定静止**状态。
//
// ★ 必须在入队前调用，否则判据不是确定性的。
//
// SpawnResident 末尾会 child.Start()（产品行为：创建即开工），于是子会在
// 两次 enqueue 之间取走一条开始执行 —— 而"正在执行"的那条不属于"尚未处理"
// （takeAllPendingEvents 只取 queue/interrupt/immediate，不取 running）。
// 实测约 1/30 概率失败于"暂存 0 条"。
//
// 这不是产品缺陷：真实场景里残余任务是"子已停止、队列里剩下的"，天然静止。
// 本文件验的是**销毁时的处置逻辑**，不是子的生命周期，所以先停住即可。
//
// （`TestResidualKeepReturnsTasksToParent` 早就踩过并记录了同一个坑，
//
//	这里复用它的做法：cancel + 等静止，不用任何自定义标志位。）
func parkChild(t *testing.T, root *Agent, childID string) {
	t.Helper()
	root.residentMu.Lock()
	rc := root.residents[childID]
	root.residentMu.Unlock()
	if rc == nil || rc.agent == nil {
		t.Fatalf("驻留子 %s 不存在", childID)
	}
	rc.agent.cancel()
	rc.agent.sched.waitSchedulerStopped(schedStopTimeout, childID)

	// 清掉停在队列里的**既有**项（通常是 SpawnResident 注入的 task_prompt）。
	//
	// ★ 不清的话残余条数不确定：子被停住时 task_prompt 可能还在队列里、
	// 也可能已被取走执行 —— 实测表现为"应处置 1 条，实际 2 条"。
	// 本文件只验处置逻辑，不从这条初始任务得到任何信息，所以直接倒掉，
	// 让每个判据从"空队列"这个确定状态开始。
	for _, leftover := range rc.agent.sched.takeAllPendingEvents() {
		_ = leftover
	}
}

// TestResidualStash_ParksOnOriginatingChannel 钉住 stash 的核心语义：
// 任务被放到**它自己来的那条通道**上，而不是随便一条、也不是父的队列。
func TestResidualStash_ParksOnOriginatingChannel(t *testing.T) {
	root, _, dir := newRootWith(t, &countingProvider{})
	reg := root.io.ChannelRegistry()
	for _, ch := range []string{"qq", "webui"} {
		if err := reg.Register(agentIO.InputChannel{Name: ch, Plugin: "test"}); err != nil {
			t.Fatalf("注册 %s: %v", ch, err)
		}
	}
	info := spawnQuietResident(t, root, dir, "stash1", "qq", "webui")
	parkChild(t, root, info.ID)

	// 两条任务分别来自不同通道。
	pushToChild(t, root, info.ID, "qq", "qq-来的消息")
	pushToChild(t, root, info.ID, "webui", "webui-来的消息")

	n, msg, err := root.FinishResidentWith(info.ID, ResidualStash, ChannelSuspend, nil)
	if err != nil {
		t.Fatalf("FinishResidentWith: %v", err)
	}
	if n != 2 {
		t.Fatalf("应处置 2 条，实际 %d（%s）", n, msg)
	}

	// ★ 必须各回各家：qq 的进 qq，webui 的进 webui。
	// 整批塞进一条通道会让"接回 qq 的人"拿到本该给 webui 的活。
	if got := reg.StashLen("qq"); got != 1 {
		t.Errorf("qq 通道暂存 = %d 条，期望 1 条（整批塞进单通道会导致错投）", got)
	}
	if got := reg.StashLen("webui"); got != 1 {
		t.Errorf("webui 通道暂存 = %d 条，期望 1 条", got)
	}
}

// TestResidualStash_NotInParentQueue 钉住 stash 与 keep 的**行为差别**。
//
// 只断言"暂存了"不够：如果 stash 顺带也塞进了父队列，那它和 keep 就没区别，
// 只不过多留了份副本 —— 任务会被处理两次。
func TestResidualStash_NotInParentQueue(t *testing.T) {
	root, _, dir := newRootWith(t, &countingProvider{})
	reg := root.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "qq", Plugin: "test"}); err != nil {
		t.Fatal(err)
	}
	info := spawnQuietResident(t, root, dir, "stash2", "qq")
	parkChild(t, root, info.ID)
	pushToChild(t, root, info.ID, "qq", "别进父队列")

	before := root.sched.queueLen()
	if _, _, err := root.FinishResidentWith(info.ID, ResidualStash, ChannelSuspend, nil); err != nil {
		t.Fatalf("FinishResidentWith: %v", err)
	}
	if got := root.sched.queueLen(); got != before {
		t.Fatalf("父队列从 %d 变成 %d —— stash 必须**只**暂存通道，不能同时塞给父（会重复处理）",
			before, got)
	}
	if reg.StashLen("qq") != 1 {
		t.Fatalf("qq 通道未暂存到任务")
	}
}

// TestResidualStash_ReleasedOnChannelTakeover 钉住"接回通道即释放"。
//
// ★ 这是整个特性里最容易静默失效的一环：
// 若接回时只改 Owner 不取暂存，通道看起来"已经接回来了"，
// 但活永远躺在里面，且没有任何报错。
func TestResidualStash_ReleasedOnChannelTakeover(t *testing.T) {
	root, _, dir := newRootWith(t, &countingProvider{})
	reg := root.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "qq", Plugin: "test"}); err != nil {
		t.Fatal(err)
	}
	info := spawnQuietResident(t, root, dir, "stash3", "qq")
	parkChild(t, root, info.ID)
	pushToChild(t, root, info.ID, "qq", "接回时该被释放")

	if _, _, err := root.FinishResidentWith(info.ID, ResidualStash, ChannelSuspend, nil); err != nil {
		t.Fatalf("FinishResidentWith: %v", err)
	}
	if reg.StashLen("qq") != 1 {
		t.Fatalf("前置条件不成立：qq 未暂存")
	}

	// 接回：Owner 改为父，暂存必须一并交出。
	drained, err := reg.AssignAndDrainStash("qq", "parent", 0)
	if err != nil {
		t.Fatalf("AssignAndDrainStash: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("接回时释放了 %d 条，期望 1 条（暂存被吞，活永远没人做）", len(drained))
	}
	if reg.StashLen("qq") != 0 {
		t.Fatalf("释放后暂存仍剩 %d 条（应清空，否则会重复释放）", reg.StashLen("qq"))
	}
	if got := reg.ListByOwner("parent"); len(got) != 1 || got[0].Name != "qq" {
		t.Fatalf("通道未归父：%+v", got)
	}
}

// TestResidualStash_SuspendKeepsStash 钉住 suspend 的语义：
// 挂起**不释放**暂存（那是接回才做的事），否则挂起等于悄悄丢弃。
func TestResidualStash_SuspendKeepsStash(t *testing.T) {
	root, _, dir := newRootWith(t, &countingProvider{})
	reg := root.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "qq", Plugin: "test"}); err != nil {
		t.Fatal(err)
	}
	info := spawnQuietResident(t, root, dir, "stash4", "qq")
	parkChild(t, root, info.ID)
	pushToChild(t, root, info.ID, "qq", "挂起后仍要在")

	if _, _, err := root.FinishResidentWith(info.ID, ResidualStash, ChannelSuspend, nil); err != nil {
		t.Fatalf("FinishResidentWith: %v", err)
	}
	// 挂起 = 归属清空（无人处理），但暂存保留。
	if got := reg.ListByOwner(""); len(got) != 1 || got[0].Name != "qq" {
		t.Fatalf("挂起后通道应在「未分配」里：%+v", got)
	}
	if reg.StashLen("qq") != 1 {
		t.Fatalf("挂起把暂存也弄丢了 —— suspend 必须保留暂存")
	}

	// 再挂起一次（幂等）也不能丢。
	if _, err := reg.AssignAndDrainStash("qq", "", 0); err != nil {
		t.Fatal(err)
	}
	if reg.StashLen("qq") != 1 {
		t.Fatalf("对空 owner 再 Assign 不应释放暂存（那是 suspend 的语义）")
	}
}

// TestResidualTakeover_PutsTasksInParentQueue 钉住"接手"。
func TestResidualTakeover_PutsTasksInParentQueue(t *testing.T) {
	root, _, dir := newRootWith(t, &countingProvider{})
	reg := root.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "qq", Plugin: "test"}); err != nil {
		t.Fatal(err)
	}
	info := spawnQuietResident(t, root, dir, "take1", "qq")
	parkChild(t, root, info.ID)
	pushToChild(t, root, info.ID, "qq", "父接手这条")

	n, msg, err := root.FinishResidentWith(info.ID, ResidualTakeover, ChannelTakeover, nil)
	if err != nil {
		t.Fatalf("FinishResidentWith: %v", err)
	}
	if n != 1 {
		t.Fatalf("应处置 1 条，实际 %d（%s）", n, msg)
	}
	// 通道也接了回来。
	if got := reg.ListByOwner("parent"); len(got) != 1 || got[0].Name != "qq" {
		t.Fatalf("通道未接回父：%+v", got)
	}
}

// TestResidualDrop_StillNotifiesSyncCaller 是既有语义的回归钉子：
// 新增 stash/suspend 后，drop 仍必须给同步调用方终态。
func TestResidualDrop_StillNotifiesSyncCaller(t *testing.T) {
	root, _, dir := newRootWith(t, &countingProvider{})
	reg := root.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "qq", Plugin: "test"}); err != nil {
		t.Fatal(err)
	}
	info := spawnQuietResident(t, root, dir, "drop1", "qq")

	respCh := make(chan *agentIO.OutputEvent, 1)
	root.rtMu.Lock()
	root.rtMu.Unlock() // 只是保持锁序一致（无实际作用）
	child := root.residents[info.ID]
	child.agent.sched.enqueue(newInputTask(&agentIO.InputEvent{
		Source: "cli", OutputChannel: "qq", Payload: map[string]interface{}{"content": "答我"},
		ResponseCh: respCh,
	}))

	if _, _, err := root.FinishResidentWith(info.ID, ResidualDrop, ChannelRelease, nil); err != nil {
		t.Fatalf("FinishResidentWith: %v", err)
	}
	select {
	case out := <-respCh:
		if out == nil || !out.Done {
			t.Error("drop 应给同步调用方终态（否则 cli/a2a 永久挂起）")
		}
	default:
		t.Error("★ drop 未通知同步调用方：调用方会永久挂起")
	}
	if reg.StashLen("qq") != 0 {
		t.Errorf("drop 不应留下暂存")
	}
}

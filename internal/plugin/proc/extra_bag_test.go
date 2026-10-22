package proc

// Extra 通用透传的判据（2026-10-12，fix/proc-extra-frame-passthrough）。
//
// 背景：shm 桥此前只透传 4 个具名 Extra 键（media_blocks/media_type/
// input_source/output_channel），插件自建的键（如 qq 的 qq_auth 绑帧身份）
// 在 stage 之间静默丢失 —— 权限门绑帧机制在 proc 插件下整体失效。
// 修复：4 键之外的 Extra 键装入 fExtraBag（JSON），跨 stage 与跨进程往返。
//
// 判据即契约：
//  1. 具名 4 键走原字段、原语义（含 nil/未设置区分），bag 不碰它们。
//  2. 任意 JSON 可序列化的插件键经 WriteAll→ReadInto 往返后值等价
//     （map/bool/string/数字/嵌套）。
//  3. 不可序列化的键被跳过并返回错误——宁可显式失败也不静默丢键
//     （丢键对 qq 是权限失效，静默等于把安全退化成运气）。
//  4. WriteDirty：bag 变了才写；没变零写入（lost update 防线不破）。
//  5. 插件写回的 bag 在内核 ctx.Extra 里以 map 形态存活，下一个 stage
//     WriteAll 原样下发（帧态跨 stage 存活）。

import (
	"encoding/json"
	"testing"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

func newTestSeg(t *testing.T) *Segment {
	t.Helper()
	buf := make([]byte, 1<<20)
	seg, err := NewSegment(buf)
	if err != nil {
		t.Fatalf("NewSegment: %v", err)
	}
	return seg
}

// 判据 2：插件自建键（qq_auth 形态：bool+int64 混合 map）完整往返。
func TestExtraBag_PluginKeyRoundTrip(t *testing.T) {
	seg := newTestSeg(t)
	src := &pubsdk.StageContext{
		RawMessage: "hello",
		Extra: map[string]interface{}{
			"input_source": "qq", // 具名键，语义不变
			"qq_auth": map[string]interface{}{
				"active":     true,
				"owner":      true,
				"message_id": int64(1125391573),
				"user_id":    int64(2198972886),
				"group_id":   int64(0),
				"is_group":   false,
			},
		},
	}
	if err := seg.WriteAll(src); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	var dst pubsdk.StageContext
	if err := seg.ReadInto(&dst); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if dst.Extra["input_source"] != "qq" {
		t.Fatalf("具名键 input_source 应原样往返，got %v", dst.Extra["input_source"])
	}
	auth, ok := dst.Extra["qq_auth"].(map[string]interface{})
	if !ok {
		t.Fatalf("qq_auth 应以 map 形态往返，got %T", dst.Extra["qq_auth"])
	}
	if auth["active"] != true || auth["owner"] != true {
		t.Fatalf("qq_auth bool 字段失真: %+v", auth)
	}
	// JSON 数字统一 float64 —— 插件侧按此解码
	if id, ok := auth["user_id"].(float64); !ok || int64(id) != 2198972886 {
		t.Fatalf("qq_auth user_id 失真: %v (%T)", auth["user_id"], auth["user_id"])
	}
	if id, ok := auth["message_id"].(float64); !ok || int64(id) != 1125391573 {
		t.Fatalf("qq_auth message_id 失真: %v", auth["message_id"])
	}
	if auth["is_group"] != false {
		t.Fatalf("qq_auth is_group 失真（false 不应丢失）: %v", auth["is_group"])
	}
}

// 判据 3：不可序列化的键显式报错，不静默丢。
func TestExtraBag_UnserializableKeyFails(t *testing.T) {
	seg := newTestSeg(t)
	src := &pubsdk.StageContext{
		Extra: map[string]interface{}{
			"func_val": func() {}, // 函数不可 JSON
		},
	}
	if err := seg.WriteAll(src); err == nil {
		t.Fatal("不可序列化的 Extra 值应使 WriteAll 报错，而不是静默丢键")
	}
}

// 判据 1：具名 4 键的 nil/空 语义不受 bag 影响（回归既有行为）。
func TestExtraBag_NamedKeysUnaffected(t *testing.T) {
	seg := newTestSeg(t)
	// 无 Extra：全部未设置
	if err := seg.WriteAll(&pubsdk.StageContext{RawMessage: "x"}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	var dst pubsdk.StageContext
	if err := seg.ReadInto(&dst); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if dst.Extra != nil {
		t.Fatalf("无 Extra 时应保持 nil，got %v", dst.Extra)
	}
	// 只有具名键：与旧行为一致
	if err := seg.WriteAll(&pubsdk.StageContext{
		Extra: map[string]interface{}{"output_channel": "qq"},
	}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	dst = pubsdk.StageContext{}
	if err := seg.ReadInto(&dst); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if dst.Extra["output_channel"] != "qq" || len(dst.Extra) != 1 {
		t.Fatalf("具名键行为变化: %v", dst.Extra)
	}
}

// 判据 4：bag 未变时 WriteDirty 零写入；变了才写。
func TestExtraBag_WriteDirty(t *testing.T) {
	seg := newTestSeg(t)
	base := &pubsdk.StageContext{
		Extra: map[string]interface{}{"qq_auth": map[string]interface{}{"active": true, "owner": true}},
	}
	if err := seg.WriteAll(base); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	var local pubsdk.StageContext
	if err := seg.ReadInto(&local); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	snap := TakeSnapshot(&local)

	// 只读插件：bag 没动 → 零写入
	n, err := seg.WriteDirty(&local, snap)
	if err != nil {
		t.Fatalf("WriteDirty: %v", err)
	}
	if n != 0 {
		t.Fatalf("只读插件应零写入，got %d 字段", n)
	}

	// 改了身份（降权）：bag 变 → 恰好 1 个字段
	local.Lock()
	local.Extra["qq_auth"] = map[string]interface{}{"active": true, "owner": false}
	local.Unlock()
	n, err = seg.WriteDirty(&local, snap)
	if err != nil {
		t.Fatalf("WriteDirty: %v", err)
	}
	if n != 1 {
		t.Fatalf("bag 变更应恰好写回 1 个字段，got %d", n)
	}
	var dst pubsdk.StageContext
	if err := seg.ReadInto(&dst); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	auth, ok := dst.Extra["qq_auth"].(map[string]interface{})
	if !ok || auth["owner"] != false {
		t.Fatalf("降权应往返成功，got %v", dst.Extra["qq_auth"])
	}
}

// 判据 5：帧态跨 stage 存活 —— 内核从共享段读回后，下一次 WriteAll
// 把插件写的 bag 原样下发（这是绑帧能成立的必要条件）。
func TestExtraBag_FrameSurvivesAcrossStages(t *testing.T) {
	seg := newTestSeg(t)
	// stage1: 内核只有 input_source；插件 on_input 绑了 qq_auth
	if err := seg.WriteAll(&pubsdk.StageContext{
		Extra: map[string]interface{}{"input_source": "qq"},
	}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	var pluginCtx pubsdk.StageContext
	if err := seg.ReadInto(&pluginCtx); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	// 快照在 handler 执行前抓（真实 stage 顺序）；之后插件 on_input 绑身份。
	snap := TakeSnapshot(&pluginCtx)
	pluginCtx.Lock()
	if pluginCtx.Extra == nil {
		pluginCtx.Extra = map[string]interface{}{}
	}
	pluginCtx.Extra["qq_auth"] = map[string]interface{}{"active": true, "owner": true, "user_id": int64(2198972886)}
	pluginCtx.Unlock()
	if n, err := seg.WriteDirty(&pluginCtx, snap); err != nil || n == 0 {
		t.Fatalf("插件写回 qq_auth: n=%d err=%v", n, err)
	}

	// 内核回读到自己的 ctx（coordinator.finish 语义）
	var kernelCtx pubsdk.StageContext
	if err := seg.ReadInto(&kernelCtx); err != nil {
		t.Fatalf("内核回读: %v", err)
	}
	if _, ok := kernelCtx.Extra["qq_auth"]; !ok {
		t.Fatal("内核回读后 qq_auth 必须存活（否则绑帧失效）")
	}

	// stage2: 内核把同一 ctx 写下去（新段模拟下一 stage 的 WriteAll）
	if err := seg.WriteAll(&kernelCtx); err != nil {
		t.Fatalf("stage2 WriteAll: %v", err)
	}
	var pluginCtx2 pubsdk.StageContext
	if err := seg.ReadInto(&pluginCtx2); err != nil {
		t.Fatalf("stage2 ReadInto: %v", err)
	}
	auth, ok := pluginCtx2.Extra["qq_auth"].(map[string]interface{})
	if !ok || auth["owner"] != true {
		t.Fatalf("下一 stage 插件必须还能读到帧身份（绑帧核心判据），got %v", pluginCtx2.Extra["qq_auth"])
	}
	if pluginCtx2.Extra["input_source"] != "qq" {
		t.Fatalf("具名键跨 stage 也应存活: %v", pluginCtx2.Extra)
	}
}

// bag 的 JSON 编解码等价性（float64 数字约定）——qq 插件解码侧依赖它。
func TestExtraBag_JSONNumberConvention(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{"user_id": int64(2198972886)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f, ok := out["user_id"].(float64); !ok || int64(f) != 2198972886 {
		t.Fatalf("int64 经 JSON 应变 float64 且无损: %v %T", out["user_id"], out["user_id"])
	}
}

// 判据 1b：bag 里混入具名键时，具名字段必须赢（ReadInto 的覆盖顺序契约）。
// 直接往共享段写原始 JSON 构造冲突值，绕过 WriteAll 的过滤 —— 模拟
// "写侧 bug / 旧版本内核把具名键也装袋" 的现实场景。
func TestExtraBag_NamedKeyConflict_BagLoses(t *testing.T) {
	seg := newTestSeg(t)
	if err := seg.WriteAll(&pubsdk.StageContext{
		Extra: map[string]interface{}{"input_source": "qq"},
	}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	// 绕过 captureLocal，手工把 bag 描述符指向伪造的 JSON（含同名异值具名键）。
	bagJSON, _ := json.Marshal(map[string]interface{}{
		"input_source": "bogus-from-bag", // 具名键污染
		"qq_auth":      map[string]interface{}{"active": true},
	})
	sl, err := seg.write(bagJSON)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	seg.setDesc(fExtraBag, sl)

	var dst pubsdk.StageContext
	if err := seg.ReadInto(&dst); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if dst.Extra["input_source"] != "qq" {
		t.Fatalf("具名键必须覆盖 bag 污染值: got %v", dst.Extra["input_source"])
	}
	if _, ok := dst.Extra["qq_auth"]; !ok {
		t.Fatal("bag 的正常键（qq_auth）应存活")
	}
}

// 判据 4b：WriteAll 后 bag 描述符里不得出现具名键（写侧过滤契约，
// 用 raw JSON 检查——语义判据测不到"键从未进袋"）。
func TestExtraBag_WriteAll_NeverPutsNamedKeysInBag(t *testing.T) {
	seg := newTestSeg(t)
	src := &pubsdk.StageContext{
		Extra: map[string]interface{}{
			"input_source": "qq",
			"media_blocks": "x",
			"qq_auth":      map[string]interface{}{"active": true},
			"custom":       123,
		},
	}
	if err := seg.WriteAll(src); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	desc := seg.getDesc(fExtraBag)
	if desc.IsUnset() {
		t.Fatal("有自建键时 bag 描述符不应为空")
	}
	raw, err := seg.read(desc)
	if err != nil {
		t.Fatalf("read bag: %v", err)
	}
	var bag map[string]interface{}
	if err := json.Unmarshal(raw, &bag); err != nil {
		t.Fatalf("bag 不是合法 JSON: %v (%s)", err, raw)
	}
	for k := range bag {
		if namedExtraKeys[k] {
			t.Fatalf("具名键 %s 混进了 bag（写侧过滤失效）", k)
		}
	}
	if bag["qq_auth"] == nil || bag["custom"] == nil {
		t.Fatalf("自建键应全在 bag: %v", bag)
	}
}

// 变异哨兵：TakeSnapshot/WirteDirty 对 bag 的脏检测（bag 变了必须计 1 字段）。
func TestExtraBag_SnapshotDetectsBagChange(t *testing.T) {
	seg := newTestSeg(t)
	if err := seg.WriteAll(&pubsdk.StageContext{
		Extra: map[string]interface{}{"qq_auth": map[string]interface{}{"active": true, "owner": false}},
	}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	var local pubsdk.StageContext
	if err := seg.ReadInto(&local); err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	snap := TakeSnapshot(&local)
	local.Lock()
	// 模拟 qq on_input 绑帧/合并降权：owner 翻转
	local.Extra["qq_auth"] = map[string]interface{}{"active": true, "owner": true}
	local.Unlock()
	n, err := seg.WriteDirty(&local, snap)
	if err != nil {
		t.Fatalf("WriteDirty: %v", err)
	}
	if n != 1 {
		t.Fatalf("bag 身份翻转应恰好脏 1 字段（WriteDirty 漏检=绑帧写不回）, got %d", n)
	}
}

// ★★★ 判据：内核写的 `comefrom` 必须能过 proc 桥到插件。
//
// 为什么必须单独判这一条：QQ 是 **proc 插件**（跨进程、共享段）。
// 内核侧写对了 comefrom 不等于插件拿得到它 —— 若这个键在桥上丢失，
// 插件会按"缺失 = user"处理 ⇒ 驻留子**照旧被拦**，而内核侧的判据全绿。
// 这正是本仓反复吃过的"通道两端各修一半"的坑。
//
// 实测背景（2026-10-16）：驻留子的工具调用被 QQ 的用户权限门拦下，
// 修法是内核在帧上写 comefrom、插件据此跳过用户权限模型。
// 两端之间的**唯一**通路就是这条。
func TestExtraBag_ComeFromRoundTrips(t *testing.T) {
	for _, want := range []string{"internal", "user"} {
		seg := newTestSeg(t)
		src := &pubsdk.StageContext{
			Extra: map[string]interface{}{
				"input_source": "qq", // 与 comefrom 共存（真实场景就是这样）
				"comefrom":     want,
			},
		}
		if err := seg.WriteAll(src); err != nil {
			t.Fatalf("WriteAll(%s): %v", want, err)
		}
		var dst pubsdk.StageContext
		if err := seg.ReadInto(&dst); err != nil {
			t.Fatalf("ReadInto(%s): %v", want, err)
		}
		got, ok := dst.Extra["comefrom"]
		if !ok {
			t.Fatalf("★ comefrom=%s 在 proc 桥上丢失 —— 插件会按 user 处理，"+
				"驻留子照旧被拦（内核侧判据却是绿的）", want)
		}
		if s, _ := got.(string); s != want {
			t.Errorf("comefrom 失真: 期望 %q 实际 %q (%T)", want, got, got)
		}
		// 与 input_source 共存时不得互相覆盖
		if dst.Extra["input_source"] != "qq" {
			t.Errorf("comefrom 写入了却把 input_source 弄丢了: %v", dst.Extra["input_source"])
		}
	}
}

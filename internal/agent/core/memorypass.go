package core

import (
	"log"
	"strconv"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// sceneKeysFor 推导本轮输入的**当前场景**。
//
// 场景是“这场面正在发生”的机器可读描述，用于把带条件的记忆（规则/约定）
// 取回来。优先级：
//  1. 注入点显式声明（payload.scene）——插件最清楚自己在什么场面里
//  2. 通道（evt.Source → chan:qq）
//  3. 工具（tool:qq_get_message）——工具输出触发的召回只知道这一步
//
// 多个场景是**并列命中**（取回任一场景的记忆），不是交集：
// 「在 QQ 上」与「刚取回消息正文」是两个都能独立成立的触发条件。
func (a *Agent) sceneKeysFor(evt *agentIO.InputEvent, toolName string) []string {
	var keys []string
	// 通道/注入点声明不参与场面识别时，**连派生场景键也不给**。
	// 只停掉指纹采集而留着声明路，等于给「不参与场面」这个口子开了后门。
	if a.sceneSuppressed(evt) {
		return nil
	}
	seen := make(map[string]bool)
	add := func(k string) {
		// 显式声明的场景键来自插件，大小写/空白/标点都不可控；归一化后再去重，
		// 否则「chan:QQ」与「chan:qq」会变成两个场景，各自只召回一半记忆。
		k = memory.NormalizeSceneKey(k)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		keys = append(keys, k)
	}

	if evt != nil && evt.Payload != nil {
		switch v := evt.Payload["scene"].(type) {
		case string:
			add(v)
		case []string:
			for _, s := range v {
				add(s)
			}
		case []interface{}:
			for _, item := range v {
				if s, ok := item.(string); ok {
					add(s)
				}
			}
		}
	}

	if evt != nil {
		add(memory.ChannelScene(evt.Source))
	}
	if toolName != "" {
		add(memory.ToolScene(toolName))
	}
	return keys
}

// mergeSceneKeys 把「声明路」与「涌现场景」两路合并成一个**无重复**的场景集合。
//
// 为什么需要它：两路各自都去重过（sceneKeysFor 内部有 seen、resolveTurnScenes
// 内部也有），但**两路之间**没有共同的 seen。而声明路与通道派生路会产出
// 同一个键（chan:qq 既是声明的、也是从 evt.Source 派生的）——现网日志实测到
// `scenes=[chan:qq chan:qq]`。
//
// 功能上 RecallByScene 内部会再去重，所以这不是 bug，但有两个实际代价：
// 日志里的 scenes=[...] 会误导排查；每次白走一遍前缀匹配。
func mergeSceneKeys(declared, emergent []string) []string {
	out := make([]string, 0, len(declared)+len(emergent))
	seen := make(map[string]bool, len(declared)+len(emergent))
	for _, k := range declared {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	for _, k := range emergent {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// memoryPassOut 是一次记忆操作（取进来 / 踢出去）的结果。
type memoryPassOut struct {
	// Archived 是被归档进文档记忆的低相关 L0 事件数（prune 的输出）。
	Archived int
	// RecallText 是可注入 prompt 的记忆索引文本（recall 的输出，空串表示无）。
	RecallText string
}

// memoryPass 是「取进来（召回）」与「踢出去（裁剪）」的**唯一入口**。
//
// prune 与 recall 是两根正交的声明轴（默认值刻意相反：裁剪是破坏性的、
// 默认关；召回是只读增量、默认开），但两者都建立在**同一份清洗后的 query**
// 之上。调用点只负责解析声明，这里统一做三件各写一遍就会写歪的事：
//
//  1. 同一 query：prune 与 recall 用同一个查询向量来源，避免「裁错事件、
//     召回错记忆」——原始内容里的 ANSI/base64/JSON 噪声会把相关性打分带偏。
//  2. 同一次预算：召回文本按 token 预算截断只做一次（见 recallText）。
//  3. 同一条审计：谁（trigger）据什么触发了哪种操作都落一条日志，
//     否则又是一个「幕后发生、查不出是谁」的机制（对齐 prune 的设计初衷）。
//
// 边界：prune 与 recall 目前仍走**各自的相关性空间**（prune 用 L0 事件的
// 稠密/词向量给已有事件打分，recall 用图 + TF-IDF 实体索引）。真正的
// 「一次打分」要先统一打分空间（后续步骤）；这里统一的是**入口、query、
// 预算与审计**——这已是「一个过程」的可审计外壳，剩下的差在打分空间。
func (a *Agent) memoryPass(query, trigger string, prune, recall bool, scenes []string) memoryPassOut {
	var out memoryPassOut
	if a == nil || (!prune && !recall) {
		return out
	}
	if prune {
		out.Archived = a.pruneByQuery(query)
	}
	if recall && query != "" {
		out.RecallText = a.recallTextFor(query, trigger, scenes)
	}
	if out.Archived > 0 || out.RecallText != "" {
		log.Printf("[agent] memory pass (%s): archived=%d recalled=%d chars",
			trigger, out.Archived, len(out.RecallText))
	}
	return out
}

// pruneByQuery 按相关性把低相关 L0 事件归档进文档记忆，返回归档数。
//
// **不做声明判定**——声明已由调用方（pruneOnInput / stepToolAfter）解析，
// 这里只负责执行。放在 memoryPass 内部是为了让裁剪与召回共享入口。
func (a *Agent) pruneByQuery(query string) int {
	if a == nil || a.context == nil {
		return 0
	}
	// **动态上下文**是父 agent 专属能力：轻量内核（驻留子）用传统上下文，
	// 不做按相关度的裁剪与向 doc 记忆的归档（子也没有 doc 记忆）。
	if a.isLightKernel() {
		return 0
	}
	topK := a.contextTopK()
	return a.context.PruneWithProtected(query, topK, a.docStore, a.effectiveProtectedCount())
}

// contextTopK 推导裁剪后应保留的**条数**。
//
// ★ 为什么不能直接用 maxContextSize（2026-10-01 跑分实测出的根因）：
// max_context_size 是**条数**、与窗口 token 毫无换算关系。实测 50k 窗口下
// prompt 峰值达窗口 7 倍、每轮都在「超限→修剪→再超限」——容量 30 条对上
// 实际占用毫无约束力。而只调 context_window 无效：改窗口不会改这30。
//
// 正确做法：用已按窗口算好的 ContextTokens 预算**反推条数**。
// 一个事件平均占多少 token 由实际数据决定，不用常量猜：
// 取本 agent 当前积累的实际平均（accumulatedTokens / 事件数），
// 至少给 1 个 token 的下限（极短事件不会让除零或把预算除成 0）。
//
// max_context_size 仍作为**上限**保留：它是运维给的「活跃条数」硬约束，
// 预算算出来的条数超过它时以它为准（否则改了配置不生效会让人困惑）。
func (a *Agent) contextTopK() int {
	hardCap := a.maxContextSize - 1 // 运维配置的上限（历史语义：-1 因为要留 protected）
	if hardCap < 1 {
		hardCap = 1
	}

	budget := a.computeTokenBudget()
	tokens := budget.ContextTokens
	if tokens <= 0 {
		return hardCap
	}

	// 平均事件 token：优先用实际积累，没有积累时退回保守值。
	avg := 0
	if a.context != nil {
		if n := a.context.Len(); n > 0 {
			avg = a.accumulatedTokens() / n
		}
	}
	if avg <= 0 {
		avg = defaultAvgEventTokens
	}

	topK := tokens / avg
	if topK < 1 {
		topK = 1
	}
	if topK > hardCap {
		topK = hardCap
	}
	// ★ 下限必须是 protectedCount+1，否则裁剪会**无效**：Prune 里
	//   keepCount = topK - len(protected)，若 topK <= protected 则 keep 为空、
	//   全部事件被归档，裁完一轮上下文还是超页（实测 topK 被算成 1 时）。
	//
	// 什么时候会算成 1：单条事件就超过整个 ContextTokens 预算（实测 v4 的
	// 工具大回执型事件平均 48000 token，而预算只有 40000）。此时正确的做法
	// 不是「只留 1 条」（等于清空记忆），而是至少留够 protected 条 ——
	// 宁可暂时超页，等 budget 或事件尺寸回到正常区间再裁。
	if min := a.effectiveProtectedCount() + 1; topK < min {
		topK = min
	}
	if topK > hardCap {
		topK = hardCap
	}
	return topK
}

// protectedContextCount 返回当前保护条数（最近 N 条永不换出）。
func (a *Agent) protectedContextCount() int {
	if a == nil || a.context == nil {
		return defaultProtectedCount
	}
	if n := a.context.protectedCount; n > 0 {
		return n
	}
	return defaultProtectedCount
}

// defaultAvgEventTokens 是「没有积累样本时」用于反推的事件平均 token。
//
// 取值依据：一条运维叙事事件通常 1500-3000 token（含工具回灌），
// 保守取 2000 —— 偏大意味着预算算出的条数偏少、裁得更狠，
// 宁可多裁也不要让积累量重新涨过窗口。
const defaultAvgEventTokens = 2000

// ──────────────────────────────────────────────
// 场面指纹：场景**涌现**的原料
//
// 场景不是谁声明的，而是从交互流里长出来的。长出来的原料就是每轮可观察的
// 场面指纹——在哪个通道、跟谁、在做什么、聊什么、什么时段。全部取自运行时
// 已有量，不需要模型配合，也不需要人工标注。
// ──────────────────────────────────────────────

// sceneSuppressed 报告本次输入是否被声明为**不参与场面识别**。
//
// 读取面与其它记忆声明完全一致：先看注入点 payload（单次覆盖），
// 再看通道定义（ChannelDef.ScenePolicy），都没声明 = 参与（保持既有行为）。
// 优先级与 pruneDeclared / recallDeclared 同构。
//
// 为什么要一个显式开关：场面指纹只要 evt.Source != "" 就无条件产出一个 chan
// 特征，于是内核自循环（system）、心跳（timer）、内部状态汇报（kernel）这类
// **纯信噪通道**也在撑场面——它们每次触发都让一个不相干的场景长出来或变强，
// 而召回时又会把「内核在跑定时器」当成「用户在这类场景下说过的话」取回。
func (a *Agent) sceneSuppressed(evt *agentIO.InputEvent) bool {
	if evt == nil {
		return false
	}
	if p, ok := evt.Payload["scene_policy"].(string); ok && p != "" {
		return p == pubsdk.ScenePolicyNone
	}
	if a.io != nil {
		if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok && chDef.ScenePolicy != "" {
			return chDef.ScenePolicy == pubsdk.ScenePolicyNone
		}
	}
	return false
}

// sceneFeaturesFor 采集一轮交互的场面指纹。
//
// 特征权重由种类决定（见 memory.SituationFeature.Weight）：通道与对象是
// 「同一个场面」最强的同一性信号，工具是行为信号，话题是软信号。
func (a *Agent) situationFeaturesFor(evt *agentIO.InputEvent, cleanInput, tool string) []memory.SituationFeature {
	// 声明不参与场面识别：连时段特征都不产——一个不参与的面孔
	// 不该在 situation_evidence / scene_features 里留下任何足迹。
	if a.sceneSuppressed(evt) {
		return nil
	}
	var feats []memory.SituationFeature
	if evt != nil {
		if evt.Source != "" {
			feats = append(feats, memory.SituationFeature{Kind: "chan", Value: evt.Source})
		}
		// 对话对象：插件在 payload 里给的群/用户标识（有则用，无则退化为仅有通道）
		for _, k := range []string{"peer", "peer_id", "group_id", "user_id", "chat_id"} {
			if v, ok := evt.Payload[k]; ok {
				if s := payloadString(v); s != "" {
					// 群与私聊要能区分：同一 id 在两种场景下不是同一个对象
					kind := "peer"
					if k == "group_id" {
						kind = "peer_group"
					}
					feats = append(feats, memory.SituationFeature{Kind: kind, Value: s})
					break
				}
			}
		}
		// 时段：弱信号。人的记忆确实带时间气味（「早上那件事」），
		// 但它不该主导场面判定，所以权重最低。
		feats = append(feats, memory.SituationFeature{Kind: "part", Value: partOfDay(time.Now())})
	}
	if tool != "" {
		feats = append(feats, memory.SituationFeature{Kind: "tool", Value: tool})
	}
	// 话题：取清洗后输入的内容词做软特征（最多 3 个）。
	if cleanInput != "" {
		for i, kw := range memory.ExtractKeywords(memory.CleanText(cleanInput)) {
			if i >= 3 {
				break
			}
			feats = append(feats, memory.SituationFeature{Kind: "topic", Value: kw})
		}
	}
	return feats
}

// payloadString 从 payload 值里取字符串（可能是 string / float64 / json.Number）。
func payloadString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	default:
		return ""
	}
}

// partOfDay 把时刻归成时段（场面指纹里最弱的一维）。
func partOfDay(t time.Time) string {
	switch h := t.Hour(); {
	case h < 6:
		return "night"
	case h < 12:
		return "morning"
	case h < 18:
		return "afternoon"
	default:
		return "evening"
	}
}

// resolveTurnScenes 解析本轮的场景集合，**同时走主动与被动两条路**：
//
//	主动（声明）：注入点/通道/工具声明了"这是哪个场面" → 场景存在化并喂入
//	              本轮指纹（声明场景因此慢慢学会自己认自己）
//	被动（涌现）：场面指纹聚类 → 同类指纹重复出现时自己长出场景
//
// 返回结果的 Primary 用于**写**（优先细粒度的涌现场景，首次交互退到声明场景
// 兜底），Keys 用于**读**（两条路的并集，去重）。
//
// 解析会**写库**（场景强化/长出），所以必须一轮一次：多调一次就多给场景记
// 一次强度，"工具调得多"会被误读成"这个场面更常出现"。
func (a *Agent) resolveTurnScenes(f *TaskFrame, tool string) memory.TurnScene {
	var out memory.TurnScene
	if a == nil || a.memory == nil {
		return out
	}
	if f != nil && f.sceneDone {
		return f.turnScene
	}

	declared := a.sceneKeysFor(evtOf(f), tool)
	feats := a.situationFeaturesFor(evtOf(f), cleanInputOf(f), tool)
	sig := memory.NewSituation(feats...)

	turn, err := a.memory.EnterSceneWithHint(sig, declared)
	if err != nil {
		log.Printf("[agent] scene enter failed: %v", err)
		// 出错时至少把声明场景交给召回，不让整条召回链一起失效
		turn = memory.TurnScene{Keys: declared}
		if len(declared) > 0 {
			turn.Primary = declared[0]
		}
	}
	if turn.Emergent {
		log.Printf("[agent] 场景涌现/命中: %q（指纹 %v）", turn.Primary, sig.Keys())
	} else if len(turn.DeclaredCreated) > 0 {
		log.Printf("[agent] 声明场景成立: %v（指纹 %v）", turn.DeclaredCreated, sig.Keys())
	}
	if f != nil {
		f.turnScene = turn
		f.Scene = turn.Primary
		f.sceneDone = true
	}
	return turn
}

// cleanInputOf 安全取出清洗后输入（f 为 nil 时为空）。
func cleanInputOf(f *TaskFrame) string {
	if f == nil {
		return ""
	}
	return f.CleanInput
}

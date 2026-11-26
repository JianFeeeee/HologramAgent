package memory

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ──────────────────────────────────────────────
// 场景的**涌现**：从交互流自己长出场面来
//
// 上一版场景是「声明/派生」的：要么调用方写 `scene="chan:qq"`，要么由通道机械
// 派生。那不是涌现，那是给记忆贴标签，标签谁定、怎么定全靠人。
//
// 这里换成人的记忆那种机制：
//
//	每轮交互都有一个可观察的**场面指纹**（在哪个通道、跟谁、在干什么、聊什么）；
//	指纹反复重合的交互，会自己聚成一个场景（没人声明过它）；
//	场景里写下的记忆自动挂上去；
//	下次指纹再次重合，挂在这个场景上的记忆**自动被唤起**，与措辞无关。
//
// 三条与生俱来的性质：
//   - 自动：指纹全部来自运行时可观察量，无需模型配合、无需人工标注；
//   - 涌现：场景在重复中长出来（首次不建场景，见 minSceneEvidence）；
//   - 强化与遗忘：场景每次重现强度 +1，记忆的挂载权重按重现次数与置信度累积，
//     长期不用的按半衰期衰减——与人的记忆一样，不用就淡。
// ──────────────────────────────────────────────

// 特征权重。决定「哪些特征算同一个场面」：通道与对象是最强的同一性信号
// （在 QQ 上、对老大），工具是行为信号，话题是软信号（同一场面的不同话题
// 不该被拆开，所以权重低），时段最弱。
const (
	wFeatChan  = 1.0
	wFeatPeer  = 1.0
	wFeatTool  = 0.8
	wFeatTopic = 0.4
	wFeatPart  = 0.2
)

// 聚类阈值。
//
// joinSceneThreshold 是「这轮属于既有场景」的下限——定在 0.5 意味着
// 「要么主体特征重合，要么好几条信号一起重合」才算同一个场面。
// recallSceneThreshold 比它低：**唤起**比**归属**宽松，想不起来是损失，
// 多想起一条只是多几行上下文（与人的联想一致）。
const (
	joinSceneThreshold   = 0.5
	recallSceneThreshold = 0.35
	// minSceneEvidence 是长成场景所需的最少重现次数。
	//
	// 为什么首次不建场景：一次性的交互不是「场面」，给它建场景会让图库被
	// 一次性事件撑满，之后每次路过都要召回一堆只发生过一次的事。
	// 第 2 次出现同类指纹时才认定「这事会重复」。
	minSceneEvidence = 2
	// maxSituationFeatures 是单轮指纹的特征上限（防长输入把相似度算糊涂）。
	maxSituationFeatures = 24
)

// SituationFeature 是一轮交互里的一个可观察信号，形如 `chan:qq`、`peer:group_1027`。

// FeatureKey 返回规范化的特征串。Kind 与 Value 都过场景键归一化，
// 保证 `chan:QQ` 与 `chan:qq` 是同一个特征。
//
// ★ 它原本是 `SituationFeature` 的方法。DTO 的声明已搬到
// internal/kernelapi/model（纯数据、零业务依赖），而 Go 不允许在别的包上
// 定义方法 ⇒ 按用户裁定（«funcs_in_memory»）降为**本包的包级函数**，
// 行为逐字不变（归一化与权重表都属于本实现侧的语义知识）。
func FeatureKey(f SituationFeature) string {
	kind := NormalizeSceneKey(f.Kind)
	val := NormalizeSceneKey(f.Value)
	if kind == "" || val == "" {
		return ""
	}
	return kind + ":" + val
}

// Weight 返回该特征的权重（按 Kind）。
func WeightOf(f SituationFeature) float64 {
	switch NormalizeSceneKey(f.Kind) {
	case "chan":
		return wFeatChan
	case "peer", "peer_group":
		// 群与私聊都是「对话对象」这一维：都是最强的同一性信号。
		// 分开 kind 是为了让 `peer:group_1` 与 `peer:user_1` 不互相命中，
		// 不是让群身份降级成软信号（漏掉这里它就只剩 topic 权重 0.4）。
		return wFeatPeer
	case "tool":
		return wFeatTool
	case "topic":
		return wFeatTopic
	case "part":
		return wFeatPart
	default:
		return wFeatTopic
	}
}

// Situation 是一轮交互的场面指纹（去重、上限裁剪后的特征集合）。

// NewSituation 由若干特征构造指纹：归一化、去重、按权重降序裁剪到上限。
func NewSituation(features ...SituationFeature) Situation {
	seen := make(map[string]bool, len(features))
	out := make([]SituationFeature, 0, len(features))
	for _, f := range features {
		k := FeatureKey(f)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return WeightOf(out[i]) > WeightOf(out[j]) })
	if len(out) > maxSituationFeatures {
		out = out[:maxSituationFeatures]
	}
	return Situation{Features: out}
}

// Keys 返回指纹的特征串列表。
func SituationKeys(s Situation) []string {
	out := make([]string, 0, len(s.Features))
	for _, f := range s.Features {
		out = append(out, FeatureKey(f))
	}
	return out
}

// Empty 表示指纹里没有任何可判定的信号。
func SituationEmpty(s Situation) bool { return len(s.Features) == 0 }

// labelFeatureWeight 是参与**场景身份**的最低特征权重。
//
// 为什么设门槛：part（时段）权重只有 0.2，是场面里最弱的维度——
// 「在 QQ 上」和「在 QQ 上且是早上」是同一个场面，时段不该把它切成两个。
// 早期实现直接取 Label(2)，只有 chan 一个强特征时 part 必然挤进第二位，
// 于是键名变成 auto:chan:qq+part:morning：既是「时段成了身份」，
// 又让加权 Jaccard 把它当成另一个场面（实测：morning 场景吞掉 evening 指纹，
// 共享 chan:qq 权重 1.0、并集含 part 0.2×2，相似度 1.0/1.4=0.714 > 0.5）。
const labelFeatureWeight = 0.5

// Label 用权重达标的主导特征给场景起个**可读名**（`chan:qq+tool:qq_get_message`）。
//
// 两条硬约束（缺一就会造出写侧匹配不上的键）：
//  1. 只取权重 ≥ labelFeatureWeight 的特征：时段/话题不进身份。
//  2. 结果**必须过 NormalizeSceneKey**：'+' 会被 normalizeSceneSegment 归一成
//     '_'，而 EnsureScene / effectiveScenes / RecallByScene 三处都过了归一化。
//     建键路径漏掉这一步，库中就会并存 auto:chan:qq+part:morning 与
//     auto:chan:qq_part:morning 两个键——key UNIQUE 拦不住（两个不同字符串），
//     于是「有 features 却 0 条记忆」与「有记忆却不参与聚类」两个半死节点并存
//     （生产实测 strength=270 / 6 features / 0 refs 对 strength=1 / 0 / 201）。
func SituationLabel(s Situation, max int) string {
	if max <= 0 {
		max = 2
	}
	var picked []string
	for _, f := range s.Features {
		if WeightOf(f) < labelFeatureWeight {
			continue
		}
		picked = append(picked, FeatureKey(f))
		if len(picked) >= max {
			break
		}
	}
	if len(picked) == 0 {
		// 全部特征都弱于门槛（纯 topic/part 的轮次）：退回最强的一批特征，
		// 宁可名字信息量低，也不要没有名字——没名字就没有键，场景根本长不出来。
		n := max
		if n > len(s.Features) {
			n = len(s.Features)
		}
		for _, f := range s.Features[:n] {
			picked = append(picked, FeatureKey(f))
		}
	}
	return NormalizeSceneKey(strings.Join(picked, "+"))
}

// emergentScene 是一次聚类计算中的场景视图。
type emergentScene struct {
	ID       int64
	Key      string
	Strength int
	Weights  map[string]float64
}

// similarity 是加权 Jaccard：共享特征的权重和 / 并集特征的权重和。
//
// 为什么加权：`chan:qq` 与 `topic:排班` 对「是不是同一个场面」的证据力差 2.5 倍，
// 不加权会让一次偶然的话题重合把两个不同场面并成一个。
func (a emergentScene) similarity(b emergentScene) float64 {
	if len(a.Weights) == 0 || len(b.Weights) == 0 {
		return 0
	}
	shared, union := 0.0, 0.0
	for k, w := range a.Weights {
		if w2, ok := b.Weights[k]; ok {
			shared += min(w, w2)
			union += max(w, w2)
		} else {
			union += w
		}
	}
	for k, w := range b.Weights {
		if _, ok := a.Weights[k]; !ok {
			union += w
		}
	}
	if union == 0 {
		return 0
	}
	return shared / union
}

// situationSimilarity 计算指纹与既有场景的相似度。
func situationSimilarity(sig Situation, sc emergentScene) float64 {
	cur := make(map[string]float64, len(sig.Features))
	for _, f := range sig.Features {
		cur[FeatureKey(f)] = WeightOf(f)
	}
	return emergentScene{Weights: cur}.similarity(sc)
}

// EnterScene 是本机制的主入口：给一轮交互的指纹找到（或长出）它的场景。
//
// 返回解析出的场景键与是否新建。调用方拿这个键去做两件事：
//  1. 本轮写下的记忆自动挂到它上面（Triple.Scene）
//  2. 本轮召回按它（以及相似场景）取回记忆
//
// 「首次不建场景」的例外：指纹只出现一次时返回空键——一次性交互不该有场面，
// 见 minSceneEvidence 的说明。
func (g *GraphDB) EnterScene(sig Situation) (string, bool, error) {
	if SituationEmpty(sig) {
		return "", false, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	scenes, err := g.loadEmergentScenesLocked()
	if err != nil {
		return "", false, err
	}

	bestIdx, bestSim := -1, 0.0
	for i, sc := range scenes {
		if sim := situationSimilarity(sig, sc); sim > bestSim {
			bestIdx, bestSim = i, sim
		}
	}

	// 命中既有场景：强化（并入新特征、强度 +1、时间刷新）
	if bestIdx >= 0 && bestSim >= joinSceneThreshold {
		sc := scenes[bestIdx]
		if err := g.reinforceSceneLocked(sc.ID, sig); err != nil {
			return "", false, err
		}
		return sc.Key, false, nil
	}

	// 未命中：看有没有「同类指纹的足迹」——首次出现只登记线索，不建场景
	evidence, err := g.recordSituationEvidenceLocked(sig)
	if err != nil {
		return "", false, err
	}
	if evidence < minSceneEvidence {
		return "", false, nil
	}

	key, err := g.createSceneLocked(sig)
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// loadEmergentScenesLocked 读入参与**被动聚类**的场景（origin='emergent'）及其特征权重。
//
// 为什么不带上声明场景：声明场景若也进相似度空间，它一旦吸收了整轮指纹就会
// 以接近 1.0 的相似度吃掉后续所有同类轮次，被动路再也长不出更细的场面。
// 声明路的泛化靠**层级键前缀**（chan:qq 覆盖 chan:qq/peer:x），各有各的机制。
func (g *GraphDB) loadEmergentScenesLocked() ([]emergentScene, error) {
	rows, err := g.db.Query(`SELECT id, key, COALESCE(strength, 1) FROM scenes
		WHERE COALESCE(origin, 'emergent') = 'emergent'`)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*emergentScene)
	var out []emergentScene
	for rows.Next() {
		var sc emergentScene
		if err := rows.Scan(&sc.ID, &sc.Key, &sc.Strength); err != nil {
			rows.Close()
			return nil, err
		}
		sc.Weights = make(map[string]float64)
		byID[sc.ID] = &sc
		out = append(out, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}

	frows, err := g.db.Query(`SELECT scene_id, feature, weight FROM scene_features`)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	for frows.Next() {
		var sid int64
		var feat string
		var w float64
		if err := frows.Scan(&sid, &feat, &w); err != nil {
			return nil, err
		}
		if sc, ok := byID[sid]; ok {
			sc.Weights[feat] = w
		}
	}
	if err := frows.Err(); err != nil {
		return nil, err
	}
	// 回填（out 里的元素是值拷贝，Weights 是同一 map，指针内容已更新）
	for i := range out {
		if sc, ok := byID[out[i].ID]; ok {
			out[i].Weights = sc.Weights
		}
	}
	return out, nil
}

// reinforceSceneLocked 把一轮指纹并入既有场景并强化它。
func (g *GraphDB) reinforceSceneLocked(sceneID int64, sig Situation) error {
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range sig.Features {
		if _, err := tx.Exec(
			`INSERT INTO scene_features (scene_id, feature, weight) VALUES (?, ?, ?)
			 ON CONFLICT(scene_id, feature) DO UPDATE SET weight = MAX(weight, excluded.weight)`,
			sceneID, FeatureKey(f), WeightOf(f)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`UPDATE scenes SET strength = COALESCE(strength, 1) + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		sceneID); err != nil {
		return err
	}
	return tx.Commit()
}

// createSceneLocked 用指纹长出一个新场景（键由主导特征派生，仅作可读名）。
func (g *GraphDB) createSceneLocked(sig Situation) (string, error) {
	// Label 已保证：过滤弱特征 + 过 NormalizeSceneKey。
	// 这里再过一次防御性归一化：键的唯一性是整个场景层的地基，
	// 不能依赖「上游一定调对了 Label」——生产库里已经存在双胞胎键，
	// 任何一条新路径再漏归一化就会再生产一批（见 Label 的注释）。
	base := NormalizeSceneKey("auto:" + SituationLabel(sig, 2))
	if base == "auto:" {
		// Label 退化到空（指纹被裁空）：不建无主场景，否则所有空指纹会堆进同一行。
		return "", fmt.Errorf("situation label 为空，拒绝建无名场景")
	}
	key := base

	tx, err := g.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// 键冲突（同一可读名已被占）时加后缀，不合并——真正的合并交给相似度判定。
	//
	// ★ 这里加出来的 #N 后缀**必须与原键一样合法**：它会被写进 scenes.key，
	// 而写侧（effectiveScenes）与读侧（RecallByScene）都会对它做归一化。
	// '#' 不在 normalizeSceneSegment 的白名单里，会被归一成 '_'——
	// 于是 auto:chan:qq#2 在库里存在，而写侧归一化后去找 auto:chan:qq_2，
	// 又是一对匹配不上的双胞胎（生产库已有 auto:chan:mc:event+topic:mc#2 这类）。
	// 所以后缀改用不会触发归一化改写的字符。
	for i := 2; ; i++ {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM scenes WHERE key = ?`, key).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			break
		}
		key = fmt.Sprintf("%s.%d", base, i)
	}

	res, err := tx.Exec(`INSERT INTO scenes (key, strength, origin) VALUES (?, 1, 'emergent')`, key)
	if err != nil {
		return "", err
	}
	sceneID, _ := res.LastInsertId()
	for _, f := range sig.Features {
		if _, err := tx.Exec(
			`INSERT INTO scene_features (scene_id, feature, weight) VALUES (?, ?, ?)
			 ON CONFLICT(scene_id, feature) DO UPDATE SET weight = MAX(weight, excluded.weight)`,
			sceneID, FeatureKey(f), WeightOf(f)); err != nil {
			return "", err
		}
	}
	// 场景成立后，**只清本指纹那个桶**的线索（它们已被这次长出吸收）。
	//
	// 曾经是 `DELETE FROM situation_evidence`（全表清），后果不是「多清一点」：
	// 多通道共用一个库，qq 的场景一长出来，就把 mc / webui / cli 尚未攒够
	// minSceneEvidence 的证据一并抹掉——它们的计数被反复清零，于是
	// **永远**攒不到 2 次，场景永远长不出来。实测：6 个通道各来 3 次，
	// 只长出 2 个场景。
	//
	// ★ 桶键是 SituationLabel(sig, 2) 本身，**不带 "auto:" 前缀**（见
	// recordSituationEvidenceLocked）——base 是带前缀的场景键，两者不是一回事。
	if _, err := tx.Exec(`DELETE FROM situation_evidence WHERE label = ?`, SituationLabel(sig, 2)); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return key, nil
}

// recordSituationEvidenceLocked 登记一次「同类指纹出现过」，返回累计次数。
//
// 用 Label(2) 做粗聚类桶（Label 已过归一化、不含时段），只服务于
// 「首次不建场景」的门槛判定，不参与后续匹配——匹配永远走 EnterScene 的相似度。
func (g *GraphDB) recordSituationEvidenceLocked(sig Situation) (int, error) {
	label := SituationLabel(sig, 2)
	if _, err := g.db.Exec(
		`INSERT INTO situation_evidence (label, count, updated_at) VALUES (?, 1, CURRENT_TIMESTAMP)
		 ON CONFLICT(label) DO UPDATE SET count = count + 1, updated_at = CURRENT_TIMESTAMP`, label); err != nil {
		return 0, err
	}
	var n int
	if err := g.db.QueryRow(`SELECT count FROM situation_evidence WHERE label = ?`, label).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// RecallBySituation 按**场面相似**取回记忆：不是键相等，而是「像不像同一个场面」。
//
// 命中多个场景时取并集，跨场景去重（同一关系只出现一次）；命中的场景先按
// 相似度排序再交给 RecallByScene。最终顺序以**场景内引用权重**（写入时的
// 置信度）为准，相似度只决定哪些场景参与、不参与每条关系的排序。
// 这正是「类似的场景自动唤起对应的记忆」那一下。
func (g *GraphDB) RecallBySituation(sig Situation, limit int) (*SceneRecall, error) {
	if SituationEmpty(sig) {
		return &SceneRecall{}, nil
	}
	scenes, err := g.loadEmergentScenesLocked()
	if err != nil {
		return nil, err
	}
	if len(scenes) == 0 {
		return &SceneRecall{}, nil
	}

	type hit struct {
		key string
		sim float64
	}
	var hits []hit
	for _, sc := range scenes {
		sim := situationSimilarity(sig, sc)
		if sim >= recallSceneThreshold {
			hits = append(hits, hit{key: sc.Key, sim: sim})
		}
	}
	if len(hits) == 0 {
		return &SceneRecall{}, nil
	}
	// 相似度高的场景排前面；同相似度时强度高的优先（更常重现的场面更可信）
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].sim != hits[j].sim {
			return hits[i].sim > hits[j].sim
		}
		return hits[i].key < hits[j].key
	})

	keys := make([]string, 0, len(hits))
	for _, h := range hits {
		keys = append(keys, h.key)
	}
	// 复用按场景键的取回逻辑（前缀语义 + weight 排序）
	out, err := g.RecallByScene(keys, limit)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DecaySceneRefs 让久未重现的场景记忆按半衰期淡出，返回删除的引用数。
//
// 人的记忆是靠「用进废退」维持秩序的：不做衰减，一次性的巧合关联会
// 永远留在场景里，每次路过都被注入，越攒越多直到注入预算被吃光。
// 权重按半衰期折半；低于 floor 的引用直接删除（关联已无信息量）。
//
// 关键在「按半衰期」：每个引用**至多每 halfLife 衰减一次**，计时起点记在
// scene_refs.decayed_at 上。只按 created_at 判龄会在每次心跳都把老引用对半
// 砍——archive 心跳默认 60 分钟、halfLife 传 30 天，于是 30 天前的关联会在
// 几小时内被砍到 floor 以下清空。那不是半衰期，是骤死。
func (g *GraphDB) DecaySceneRefs(halfLife time.Duration, floor float64) (int, error) {
	if halfLife <= 0 {
		return 0, nil
	}
	if floor <= 0 {
		floor = 0.05
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// 时间基准必须与 CURRENT_TIMESTAMP 一致（SQLite 用 UTC）：如果在 Go 侧用
	// 本地时间拼字符串比较，东八区会凭空多出 8 小时的“年龄”，刚刷新的
	// decayed_at 会被判定为还没到点。这里交给 SQLite 的 datetime('now', …)。
	mod := fmt.Sprintf("-%d seconds", int(halfLife.Seconds()))
	if _, err := g.db.Exec(
		`UPDATE scene_refs SET weight = weight * 0.5, decayed_at = CURRENT_TIMESTAMP
		 WHERE decayed_at < datetime('now', ?) AND id NOT IN (
			SELECT sr.id FROM scene_refs sr JOIN scenes s ON sr.scene_id = s.id
			WHERE s.updated_at >= datetime('now', ?))`, mod, mod); err != nil {
		return 0, err
	}
	res, err := g.db.Exec(`DELETE FROM scene_refs WHERE weight < ?`, floor)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// TurnScene 是一轮交互解析出来的场景集合。

// EnterSceneWithHint 同时走**主动声明**与**被动涌现**两条路。
//
// 主动路（declaredKeys 非空）：确保这些场景存在（不存在即建，不等第二次涌现
// ——人明确说了"这是哪个场面"，就不该再等它自己涌现），强度 +1，并给它记下
// 从**键自身**解析出的特征。声明路刻意**不吸收本轮整场指纹**：一旦吸收，它
// 会在相似度上压过一切，被动聚类再也长不出更细的场面（见 EnsureScene）。
//
// 被动路（始终执行）：EnterScene 的聚类，指纹重复到 minSceneEvidence 次时
// 自己长出场景。首次交互这里返回空，此时 Primary 落到声明场景兜底——
// 这正是"只挂一条会丢东西"的那一半。
func (g *GraphDB) EnterSceneWithHint(sig Situation, declaredKeys []string) (TurnScene, error) {
	out := TurnScene{}

	// 主动路：声明场景存在化 + 学特征 + 强化
	seen := make(map[string]bool)
	for _, raw := range declaredKeys {
		key := NormalizeSceneKey(raw)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		learned, err := g.EnsureScene(key)
		if err != nil {
			return out, err
		}
		if learned {
			out.DeclaredCreated = append(out.DeclaredCreated, key)
		}
		out.Keys = append(out.Keys, key)
	}
	if len(out.Keys) > 0 {
		out.Primary = out.Keys[0]
	}

	// 被动路：指纹聚类（可能返回已聚合的场景、也可能首次为空）
	//
	// EnterScene 只在真的命中/新建时返回非空键；返回空键时 created 必为
	// false（首次只登记足迹），所以这里无需再判 created。
	if !SituationEmpty(sig) {
		key, _, err := g.EnterScene(sig)
		if err != nil {
			return out, err
		}
		if key != "" {
			if !seen[key] {
				out.Keys = append(out.Keys, key)
			}
			out.Primary = key
			out.Emergent = true
		}
	}
	return out, nil
}

// EnsureScene 让一个**声明出来的**场景存在（不存在则建），并给它记一次强度。
//
// 特征只从**键自身**解析（`chan:qq/peer:group_1` → {chan:qq, peer:group_1}），
// 不吸收本轮的整轮指纹。这条边界很关键：声明场景若吸收整轮指纹，它会在相似度
// 上压过一切，被动聚类再也长不出更细的场面（实测过，见 loadEmergentScenesLocked）。
// 声明路的泛化靠层级键前缀，不需要靠学指纹。
func (g *GraphDB) EnsureScene(key string) (bool, error) {
	key = NormalizeSceneKey(key)
	if key == "" {
		return false, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	var sceneID int64
	err := g.db.QueryRow(`SELECT id FROM scenes WHERE key = ?`, key).Scan(&sceneID)
	created := false
	if err == sql.ErrNoRows {
		res, ierr := g.db.Exec(`INSERT INTO scenes (key, strength, origin) VALUES (?, 1, 'declared')`, key)
		if ierr != nil {
			return false, ierr
		}
		sceneID, _ = res.LastInsertId()
		created = true
	} else if err != nil {
		return false, err
	}

	if _, err := g.db.Exec(
		`UPDATE scenes SET strength = COALESCE(strength,1) + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		sceneID); err != nil {
		return created, err
	}
	for _, f := range FeaturesFromSceneKey(key) {
		if _, err := g.db.Exec(
			`INSERT INTO scene_features (scene_id, feature, weight) VALUES (?, ?, ?)
			 ON CONFLICT(scene_id, feature) DO UPDATE SET weight = MAX(weight, excluded.weight)`,
			sceneID, FeatureKey(f), WeightOf(f)); err != nil {
			return created, err
		}
	}
	return created, nil
}

// FeaturesFromSceneKey 从场景键解析它自身蕴含的场面特征。
//
//	chan:qq                          → {chan:qq}
//	chan:qq/peer:group_1027          → {chan:qq, peer:group_1027}
//	老大2026-09-04_12:27_qq私聊图片   → {}（无 kind:value 结构，不猜）
//
// 只有 `kind:value` 形态的层才算特征——猜不出结构的键宁可留空，
// 也不要往特征空间里灌进会污染相似度的东西。
func FeaturesFromSceneKey(key string) []SituationFeature {
	parts := strings.Split(NormalizeSceneKey(key), "/")
	var out []SituationFeature
	for _, p := range parts {
		i := strings.Index(p, ":")
		if i <= 0 || i == len(p)-1 {
			continue
		}
		kind, val := p[:i], p[i+1:]
		// 白名单，不猜：`老大2026-09-04_12:27_qq私聊图片` 里的 `12:27` 也是
		// `kind:value` 形态，放进特征空间就是往相似度里灌垃圾。
		if !knownFeatureKinds[kind] {
			continue
		}
		feat := SituationFeature{Kind: kind, Value: val}
		if FeatureKey(feat) == "" {
			continue
		}
		out = append(out, feat)
	}
	return out
}

// knownFeatureKinds 是允许进入场面指纹的特征种类（新的种类在这里登记，
// 并在 SituationFeature.Weight 里给权重）。
var knownFeatureKinds = map[string]bool{
	"chan": true, "peer": true, "peer_group": true,
	"tool": true, "topic": true, "part": true,
}

package pipeline

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/distill"
	"github.com/JianFeeeee/HologramAgent/internal/nlp"
	"github.com/JianFeeeee/HologramAgent/internal/safeclose"
)

type RawRecord struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	Distilled bool      `json:"distilled"`

	// persisted 表示该记录已经写在磁盘 raw 文件里。
	// 未导出：只影响本进程的落盘行为，不进 JSON。
	// 作用：flush 只写**尚未落盘**的记录，否则 loadExisting 载入的记录
	// 会被再写一份，重启后同一批记录从新旧两个文件各读回一次。
	persisted bool
}

type DistillerConfig struct {
	Interval      time.Duration `json:"interval"`
	RetentionDays int           `json:"retention_days"`
	BatchSize     int           `json:"batch_size"`
}

type Distiller struct {
	pauseMu  sync.Mutex
	paused   bool
	pauseCh  chan struct{}
	inflight sync.WaitGroup
	mu       sync.Mutex
	db       *memory.GraphDB
	rawPath  string
	records  []RawRecord
	nextID   int64
	cfg      DistillerConfig
	ctx      context.Context
	cancel   context.CancelFunc
	onMemory func(input, response string)
	embedder nlp.Vectorizer

	// embed 把块文本变成向量与指纹；nil 表示不带向量（不编造）。
	embed EmbedFunc

	// splitter 是小模型拆分器（pkg/generation 提供的 provider）。
	//
	// ★ 为什么要有它：jieba/ONNX 那条自动蒸馏路实测产出 **0 条**
	//（defaultParser 为 nil，POS 模板抽不出），于是图记忆里的 188 个实体
	// 全部是模型主动调 memory_commit 写进来的**整句复合值**
	//（admin服务端口8861·billing服务端口8499·oauth服务端口8271）。
	// 后果是跨维度检索全失效：严格判据下基线 0/5。
	//
	// splitter 为 nil 时回退 extractKeyTriples（现有 jieba 路），
	// 保持蒸馏不停摆 —— 哪怕产出 0 条也比整体失败好。
	splitter RecordSplitter
}

// RecordSplitter 把一条原始记录拆成三元组。
//
// 抽成接口是为了让 pipeline 不依赖 internal/memory/distill（那个包依赖
// pkg/generation，而 pipeline 在早期启动阶段不该拉起生成侧依赖）。
type RecordSplitter interface {
	Split(ctx context.Context, record string) ([]memory.Triple, error)
}

// SetSplitter 注入小模型拆分器。为 nil 时蒸馏回退 jieba 路。
func (d *Distiller) SetSplitter(s RecordSplitter) { d.splitter = s }

// SetEmbedFunc 注入块向量计算函数（由持有 embedding provider 的一方提供）。
func (d *Distiller) SetEmbedFunc(fn EmbedFunc) { d.embed = fn }

// SetEmbedder 注入词嵌入器，供既有 jieba/ONNX 抽取路做 TransE 语义验证。
// 与 SetSplitter 并存：splitter 存在时优先生效，embedder 仍用于回退路径。
func (d *Distiller) SetEmbedder(ev nlp.Vectorizer) { d.embedder = ev }

func NewDistiller(db *memory.GraphDB, dataDir string, cfg DistillerConfig) *Distiller {
	ctx, cancel := context.WithCancel(context.Background())
	return &Distiller{
		db:      db,
		rawPath: filepath.Join(dataDir, "memory", "raw"),
		cfg:     cfg,
		ctx:     ctx,
		cancel:  cancel,
		pauseCh: make(chan struct{}, 1),
	}
}

func (d *Distiller) OnMemoryCandidate(fn func(input, response string)) {
	d.onMemory = fn
}

func (d *Distiller) Start() {
	if err := os.MkdirAll(d.rawPath, 0755); err != nil {
		log.Printf("[memory] create raw path: %v", err)
	}
	d.loadExisting()
	log.Printf("[memory] distiller started (interval: %v, retention: %d days)", d.cfg.Interval, d.cfg.RetentionDays)
	go d.distillLoop()
}

func (d *Distiller) Stop() {
	d.cancel()
	d.flush()
}

// Stopped 报告蒸馏循环是否已被 Stop() 取消。
//
// Stop() 里的 cancel() 是同步生效的，所以本方法在 Stop() 返回后立即为 true，
// 不受循环 goroutine 何时退出的影响。启动自检、健康检查用它确认
// 「Start 之后没有被立即 Stop 掉」——历史回归：main() 拆分时
// initMemoryStack 里残留一句 defer distiller.Stop()，函数一返回就把刚起的
// 循环杀了，10min 心跳从不运行。Start() 之前返回 false（尚未被停）。
func (d *Distiller) Stopped() bool {
	select {
	case <-d.ctx.Done():
		return true
	default:
		return false
	}
}

func (d *Distiller) Append(sessionID string, role string, content string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	d.records = append(d.records, RawRecord{
		ID: d.nextID, SessionID: sessionID, Role: role,
		Content: content, CreatedAt: time.Now(),
	})
}

func (d *Distiller) flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushLocked()
}

// flushLocked 把**尚未落盘**的记录追加写入一个新的 raw 文件（原子写）。
//
// 只写 !persisted 的记录：loadExisting 载入的记录已经在磁盘上，若 flush 再把
// 它们整体重写一份，重启后同一批记录会同时从旧文件与新文件被读回，
// 实体 mention_count 与关系被重复蒸馏。
func (d *Distiller) flushLocked() {
	var pending []RawRecord
	for _, r := range d.records {
		if !r.persisted {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return
	}
	path := filepath.Join(d.rawPath, fmt.Sprintf("raw_%d.tsv", time.Now().UnixNano()))
	var sb strings.Builder
	for _, r := range pending {
		fmt.Fprintf(&sb, "%d\t%s\t%s\t%s\t%d\n", r.ID, r.SessionID, r.Role, r.Content, r.CreatedAt.Unix())
	}
	if err := writeFileAtomic(path, []byte(sb.String())); err != nil {
		log.Printf("[memory] flush error: %v", err)
		return
	}
	for i := range d.records {
		d.records[i].persisted = true
	}
}

func (d *Distiller) loadExisting() {
	entries, err := os.ReadDir(d.rawPath)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -d.cfg.RetentionDays)
	type fileInfo struct {
		name string
		mod  time.Time
	}
	var files []fileInfo
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if ext != ".tsv" && ext != ".jsonl" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(d.rawPath, entry.Name()))
			continue
		}
		files = append(files, fileInfo{name: entry.Name(), mod: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	loaded := 0
	const maxStartupRecords = 5000
	for _, entry := range files {
		if loaded >= maxStartupRecords {
			break
		}
		path := filepath.Join(d.rawPath, entry.name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		func() {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			buf := make([]byte, 0, 64*1024)
			scanner.Buffer(buf, 1024*1024)
			for scanner.Scan() {
				if loaded >= maxStartupRecords {
					break
				}
				line := scanner.Text()
				parts := splitLine(line)
				if len(parts) < 5 {
					continue
				}
				ts, err := strconv.ParseInt(parts[4], 10, 64)
				if err != nil {
					continue
				}
				createdAt := time.Unix(ts, 0)
				if createdAt.Before(cutoff) {
					continue
				}
				d.records = append(d.records, RawRecord{
					ID: d.nextID, SessionID: parts[1], Role: parts[2], Content: parts[3], CreatedAt: createdAt,
					persisted: true,
				})
				d.nextID++
				loaded++
			}
		}()
	}
	if loaded >= maxStartupRecords {
		log.Printf("[memory] distiller startup load capped at %d recent records", loaded)
	}
}

func (d *Distiller) distillLoop() {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		// ★ 伪关机门（Quiesce/Resume）：迁移数据目录时必须完全不写。
		//
		// 为什么不用 cancel：cancel 是**不可逆**的，恢复只能重建整个 Distiller
		// （而它持有 rawPath/records 状态）。迁移要"停下来→搬→再起来" ⇒ 需要可逆。
		if paused := d.waitNotPaused(); paused {
			return
		}
		select {
		case <-ticker.C:
			if d.isPaused() {
				continue // 暂停期间跳过本轮，避免"刚好卡在检查后开始写"
			}
			d.inflight.Add(1)
			d.distillOnce()
			d.inflight.Done()
		case <-d.ctx.Done():
			return
		}
	}
}

// waitNotPaused 阻塞到未被暂停；返回 true 表示 ctx 已取消（应退出循环）。
func (d *Distiller) waitNotPaused() bool {
	for d.isPaused() {
		select {
		case <-d.pauseCh: // 被 Resume 唤醒，重新检查
		case <-d.ctx.Done():
			return true
		}
	}
	return false
}

func (d *Distiller) isPaused() bool {
	d.pauseMu.Lock()
	defer d.pauseMu.Unlock()
	return d.paused
}

// Pause 伪关机：停止写入并**等待在途蒸馏完成**（可逆）。
//
// 语义保证（迁移依赖这三条）：
//  1. 返回后**不会再有新的** distillOnce 开始
//  2. 已在跑的那次 distillOnce 会**跑完**（inflight 归零才返回）
//     ——否则迁移会搬走一个写了一半的库
//  3. 幂等：重复 Pause 不报错
func (d *Distiller) Pause() error {
	d.pauseMu.Lock()
	if d.paused {
		d.pauseMu.Unlock()
		return nil
	}
	d.paused = true
	d.pauseMu.Unlock()

	// 等在途的那一次结束（最多等到 ctx 取消）
	//
	// ★ 用 safeclose.Safe 而不是裸 close：本仓 close 纪律要求"只要调 close
	// 就要有前置检查"（见 internal/safeclose 的判据）。这里虽然当前只有
	// 一个 goroutine 会 close，但将来加一条错误/超时/重连路径就可能变成两次，
	// 而那次改动看起来与 close 无关 ⇒ 退出路径 panic。
	done := make(chan struct{})
	var doneOnce sync.Once
	go func() { d.inflight.Wait(); safeclose.Safe(&doneOnce, done) }()
	select {
	case <-done:
		return nil
	case <-d.ctx.Done():
		return nil
	}
}

// Resume 唤醒：允许再次写入。
func (d *Distiller) Resume() error {
	d.pauseMu.Lock()
	was := d.paused
	d.paused = false
	d.pauseMu.Unlock()
	if was {
		// 唤醒所有在 waitNotPaused 上等的循环（非阻塞广播）
		select {
		case d.pauseCh <- struct{}{}:
		default:
		}
	}
	return nil
}

func (d *Distiller) distillOnce() {
	d.mu.Lock()
	batchSize := d.cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 50
	}
	// 每 tick 取前 N 条未蒸馏记录（无 RetentionDays 门槛），蒸馏成功才标记/移除
	var toDistill []RawRecord
	var remaining []RawRecord
	for _, r := range d.records {
		if !r.Distilled && len(toDistill) < batchSize {
			toDistill = append(toDistill, r)
		} else {
			remaining = append(remaining, r)
		}
	}
	d.records = remaining
	d.mu.Unlock()

	if len(toDistill) == 0 {
		return
	}

	distilled := 0
	for i := 0; i < len(toDistill); i += batchSize {
		end := i + batchSize
		if end > len(toDistill) {
			end = len(toDistill)
		}
		if d.distillBatch(toDistill[i:end]) {
			distilled += end - i
			// 成功即从磁盘 raw 文件里删除这些行，否则重启后 loadExisting
			// 会把它们当未蒸馏记录重新读回，每次启动重蒸同一批历史。
			d.removeRawRecords(toDistill[i:end])
		} else {
			// 蒸馏失败：记录写回待处理队列，下次 tick 重试
			d.mu.Lock()
			d.records = append(toDistill[i:end], d.records...)
			d.mu.Unlock()
		}
	}
	d.cleanupRawFiles()
	if distilled > 0 {
		log.Printf("[memory] distilled %d records", distilled)
	}
}

// distillSplitTimeout 是单次小模型拆分调用的上限。
//
// 实测单条 4-9s（qwen3:1.7b + schema 约束，CPU）。超时不能太短，
// 否则每条都超时 = splitter 恒失败 = 静默退化成 0 产出的 jieba 路；
// 也不能太长，否则一批 50 条会把 30 分钟的蒸馏间隔吃穿。
const distillSplitTimeout = 90 * time.Second

// distillBatch 蒸馏一批记录，全部成功返回 true，任一失败返回 false（调用方重试）
func (d *Distiller) distillBatch(batch []RawRecord) bool {
	var userContent, assistantContent string
	sessionIDs := make(map[string]bool)
	for _, r := range batch {
		sessionIDs[r.SessionID] = true
		if r.Role == "user" {
			userContent += r.Content + " "
		} else {
			assistantContent += r.Content + " "
		}
	}
	// 块路径优先（正确形态），Triple 路保留为对照期回退。
	//
	// ★ 为什么改（本会话认知纠错）：拆解产物的正确落库形态是
	//【句子】【contains 边】【块】—— 节点已从纯文本实体升级为带向量的
	// memory_blocks（46f833c）。把拆解结果 Commit 成 entities 是把新产出
	// 灌进正在退场的旧形态；而旧 jieba 路实测产出 0 条，保留它只为
	//「块路不可用时记忆不丢」的底线。
	if d.splitter != nil {
		if wb, ok := d.splitter.(BlockSplitter); ok {
			if d.writeBlocks(wb, userContent, assistantContent) {
				return true
			}
			// 块路失败（模型错误/超时）：不要在这里 return false ——
			// 那会让同一批记录无限重试。落回 Triple 路（jieba），
			// 失败原因已在 writeBlocks 里记日志。
		}
	}

	triples := d.extractTriples(userContent, assistantContent)
	if len(triples) > 0 {
		sessionID := ""
		for sid := range sessionIDs {
			sessionID = sid
			break
		}
		if _, _, err := d.db.Commit(triples, sessionID, 0); err != nil {
			log.Printf("[memory] distill commit: %v", err)
			return false
		}
	}
	return true
}

// BlockSplitter 是能产出块形态的拆解器（distill.Extractor 实现）。
//
// 与 RecordSplitter 并存的原因：RecordSplitter.Split 返回 Triple（对照期
// 仍被 memoryface 接口使用），块形态经 Blocks 返回。两个方法由同一个
// Extractor 实现，共用同一套闸门，不重复实现。
type BlockSplitter interface {
	RecordSplitter
	Blocks(ctx context.Context, record string) (*distill.BlockPayload, error)
}

// EmbedFunc 由持有 embedding provider 的一方注入；nil 表示不带向量
// （块仍入库，只是不参与向量召回——不编造零向量）。
type EmbedFunc func(text string) (vec []float64, fingerprint string)

// writeBlocks 逐条拆解并落块。全部成功返回 true；任一条失败记日志并
// 返回 false（调用方落回 Triple 路），已成功的块保留（幂等 ID 保证
// 重试不会产生重复）。
func (d *Distiller) writeBlocks(bs BlockSplitter, userContent, assistantContent string) bool {
	text := userContent
	if assistantContent != "" {
		text += " " + assistantContent
	}
	// 按句切分：块的语义单位是句子，整段混合会让 contains 边失去指向。
	for _, sent := range splitSentences(text) {
		sent = strings.TrimSpace(sent)
		if len([]rune(sent)) < 4 {
			continue
		}
		ctx, cancel := context.WithTimeout(d.ctx, distillSplitTimeout)
		payload, err := bs.Blocks(ctx, sent)
		cancel()
		if err != nil {
			log.Printf("[memory] distill blocks: %v", err)
			return false
		}
		// 用 d.ctx 而非 context.Background()：Distiller.Stop() 会 cancel 它，
		// 用 Background 会让停机时正在写库的蒸馏循环继续跑。
		if _, err := distill.WritePayload(d.ctx, d.db, payload, d.embed); err != nil {
			log.Printf("[memory] distill write blocks: %v", err)
			return false
		}
	}
	return true
}

func (d *Distiller) cleanupRawFiles() {
	entries, err := os.ReadDir(d.rawPath)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -(d.cfg.RetentionDays + 1))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(d.rawPath, entry.Name()))
		}
	}
}

// writeFileAtomic 写临时文件再 rename，避免进程在写一半时崩溃留下半个文件。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rawKey 唯一标识一条原始记录，用于在 raw 文件里按内容定位并删除。
//
// 为什么按内容而不是 ID：loadExisting 读回记录时会重新分配连续 ID，
// 文件里的 ID 与内存 ID 并不一一对应。
func rawKey(session, role, content string, ts int64) string {
	return session + "\x00" + role + "\x00" + content + "\x00" + strconv.FormatInt(ts, 10)
}

// removeRawRecords 从磁盘 raw 文件中删除已成功蒸馏的记录。
//
// 蒸馏成功后记录若只从内存移除、磁盘文件不动，下次启动 loadExisting 会把
// 它们当未蒸馏记录重新读回，导致每次重启都重蒸同一批历史（实体
// mention_count 膨胀，且 distillBatch 的 sessionID 取自 map 首个键，
// 不确定性会放大重复）。
func (d *Distiller) removeRawRecords(batch []RawRecord) {
	if len(batch) == 0 {
		return
	}
	drop := make(map[string]bool, len(batch))
	for _, r := range batch {
		drop[rawKey(r.SessionID, r.Role, r.Content, r.CreatedAt.Unix())] = true
	}
	entries, err := os.ReadDir(d.rawPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if ext != ".tsv" && ext != ".jsonl" {
			continue
		}
		path := filepath.Join(d.rawPath, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var kept []string
		removed := false
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			parts := splitLine(line)
			if len(parts) >= 5 {
				if ts, err := strconv.ParseInt(parts[4], 10, 64); err == nil &&
					drop[rawKey(parts[1], parts[2], parts[3], ts)] {
					removed = true
					continue
				}
			}
			kept = append(kept, line)
		}
		if !removed {
			continue
		}
		if len(kept) == 0 {
			os.Remove(path)
			continue
		}
		if err := writeFileAtomic(path, []byte(strings.Join(kept, "\n")+"\n")); err != nil {
			log.Printf("[memory] rewrite raw %s: %v", entry.Name(), err)
		}
	}
}

func extractKeyTriples(userContent, assistantContent string, embedder nlp.Vectorizer) []memory.Triple {
	var triples []memory.Triple

	e := nlp.NewExtractor(nil)
	if embedder != nil {
		e.SetEmbedder(embedder)
	}
	text := userContent
	if assistantContent != "" {
		text += assistantContent
	}
	result := e.Extract(text)
	if result != nil {
		for _, nt := range result.Triples {
			mt := nlp.ToMemoryTriple(nt)
			if mt.Subject != "" && mt.Relation != "" && mt.Object != "" {
				triples = append(triples, mt)
			}
		}
	}

	// 注意：这里**不**做噪音过滤。对话蒸馏的抽取器（与 doc→graph 共用一个
	// NLP 提取器）历史上就没有常用词闸门：CutExact 那层当年只挂在 doc→graph 上。
	// 而且 pipeline_test 明确断言「我 --读书--> 杭州」必须被抽出（代词作主语是
	// 该路的既定行为）。要不要在对话路也拦常用词是行为决策，不在此处擅改，
	// 参见 memory.IsNoiseEntity 的说明。
	return triples
}

// extractTriples 按「有 splitter 用 splitter，否则回退 jieba」的顺序抽取。
//
// 必须能区分「splitter 跑了但没拆出东西」与「splitter 没跑/失败」：
//   - 跑出 0 条 = 正常（这类记录本就无字段可拆，例如纯叙述句）
//   - 报错     = 异常，回退 jieba 路并保留记录等下次重试
func (d *Distiller) extractTriples(userContent, assistantContent string) []memory.Triple {
	if d.splitter != nil {
		text := userContent
		if assistantContent != "" {
			text += " " + assistantContent
		}
		ctx, cancel := context.WithTimeout(d.ctx, distillSplitTimeout)
		defer cancel()
		triples, err := d.splitter.Split(ctx, text)
		if err == nil {
			return triples
		}
		// 回退前记一笔：splitter 长期失败会静默退化成 jieba 路的 0 条产出，
		// 那种情况从外部看不出来（蒸馏照常「成功」，只是没写进任何东西）。
		log.Printf("[memory] splitter failed, falling back to jieba path: %v", err)
	}
	return extractKeyTriples(userContent, assistantContent, d.embedder)
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func splitLine(line string) []string {
	if line == "" {
		return nil
	}
	return strings.SplitN(line, "\t", 5)
}

func (d *Distiller) GetRecentRecords(limit int) []RawRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.records)
	if n == 0 {
		return nil
	}
	if limit > 0 && limit < n {
		n = limit
	}
	result := make([]RawRecord, n)
	copy(result, d.records[len(d.records)-n:])
	return result
}

func (d *Distiller) Stats() map[string]interface{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]interface{}{
		"raw_records":    len(d.records),
		"interval":       d.cfg.Interval.String(),
		"retention_days": d.cfg.RetentionDays,
	}
}

// splitSentences 按中英文句读切分文本。
//
// 蒸馏的块语义单位是句子；这里只做粗切（。！?；\n），不做 NLP 级
// 句法分析 —— 粗切足够给块提供「同一场对话里的一个片段」边界。
func splitSentences(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == '。' || r == '！' || r == '?' || r == '；' ||
			r == '\n' || r == '？' || r == '!'
	})
}

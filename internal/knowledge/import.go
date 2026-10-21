package knowledge

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// ===== 从目录导入知识 =====

// ImportOptions 是 ImportDir 的参数。
type ImportOptions struct {
	// Dir 是源目录，**必须是绝对路径**。
	//
	// 为什么强制绝对：agent 调工具时 cwd 不受控（内核 daemon 的 cwd 是
	// 工作目录，不是 agent 心智模型里的那个"项目目录"）。相对路径会
	// 静默导到错误的地方，而用户以为导的是他指定的目录。
	Dir string

	// Category 是**前缀叠加**在源目录结构之上的分类路径。
	//
	// 选"叠加"而非"替换"：替换会丢掉源目录本身的层级信息
	//（导入 ~/docs/go/x.md 配 category=tech 得到 tech/x，
	//  而"go" 这一层正是这份资料最有价值的组织信息）。
	Category string

	// DryRun 只统计不落盘。
	DryRun bool

	// MaxItems 限制本次导入条目数，0 用默认值。
	// 防呆：agent 误传 "/" 或整个家目录时不会一次把盘灌满。
	MaxItems int

	// IncludeMedia 是否把图片/音视频一并导入（进媒体库、按 digest 引用）。
	IncludeMedia bool
}

// ImportStats 是导入结果。
type ImportStats struct {
	// Imported 成功写入的条目数。
	Imported int
	// Skipped 因同名冲突跳过的条目数（**不覆盖已有知识**）。
	Skipped int
	// Failed 失败的条目数。
	Failed int
	// Media 是导入的媒体文件数。
	Media int
	// Truncated 表示因 MaxItems 被截断（agent 据此知道"没导完"）。
	Truncated bool
	// Names 是本次导入的知识名（dry_run 与成功路径都给，便于 agent 复核）。
	Names []string
	// Errors 记前若干条失败原因（不全量：失败成百上千时返回上万行没意义）。
	Errors []string
}

// defaultImportMaxItems 是单次导入的默认上限。
const defaultImportMaxItems = 500

// importTextExt 是被当作正文导入的文本扩展名。
//
// 刻意白名单而非"非二进制都算"：.exe/.zip/.so 这类即便不是二进制，
// 塞进知识库也只是污染检索面（还会被误算进 IDF）。
var importTextExt = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".text": true,
	".rst": true, ".org": true, ".adoc": true,
}

// importMediaExt 是被当作媒体导入的扩展名（对应 media.Kind）。
var importMediaExt = map[string]string{
	".png": "image", ".jpg": "image", ".jpeg": "image", ".gif": "image",
	".webp": "image", ".bmp": "image",
	".mp3": "audio", ".wav": "audio", ".m4a": "audio", ".flac": "audio", ".ogg": "audio",
	".mp4": "video", ".mov": "video", ".webm": "video", ".mkv": "video", ".avi": "video",
}

// maxImportErrors 限制 Errors 的条数。
const maxImportErrors = 20

// ImportDir 把一个目录里的文档复制进知识库，并完成索引与向量化。
//
// 语义是**复制**不是引用：
//   - 文本经 Write 整份写入 <知识根>/<分类>/<名>/content.md
//   - 媒体按 sha256 进媒体库（内容寻址，天然去重），条目只存 digest 引用
//
// 源目录之后删掉/改动都不影响已导入的副本。
//
// 目录约定（自动适配两种，不要求用户改造资料）：
//  1. 目录里有 content.md ⇒ 整个目录是一个条目（与 scanDir 的既有语义一致，
//     所以知识库自身的目录能被原样再导入而不会被拆散）；
//  2. 否则目录里的 .md/.txt 等文件各是一个条目，**目录路径即分类**。
//
// 安全边界（批量操作，缺一道就可能把不该读的东西读进来）：
//   - 必须绝对路径
//   - 符号链接不跟随（否则一个软链就能把知识根之外的文件导进来）
//   - 拒绝把知识库自身当源（自导会无限自我复制）
func (s *Store) ImportDir(opt ImportOptions) (ImportStats, error) {
	var st ImportStats
	if strings.TrimSpace(opt.Dir) == "" {
		return st, fmt.Errorf("knowledge: 导入目录不能为空")
	}
	if !filepath.IsAbs(opt.Dir) {
		return st, fmt.Errorf("knowledge: 导入目录必须是绝对路径（agent 的 cwd 不可控，相对路径会导到别处）")
	}
	// 符号链接不跟随：Resolve 拿到的是**逻辑**路径，不碰文件系统。
	// 但目录内部可能藏软链，所以还要在 walk 里逐个 EvalSymlinks 校验。
	srcRoot := filepath.Clean(opt.Dir)
	realSrc, err := filepath.EvalSymlinks(srcRoot)
	if err != nil {
		return st, fmt.Errorf("knowledge: 导入目录不可达: %w", err)
	}
	// 拒绝自导：源目录在知识根之内（含知识根本身）。
	realRoot := filepath.Clean(s.root)
	if realSrc == realRoot || strings.HasPrefix(realSrc, realRoot+string(filepath.Separator)) {
		return st, fmt.Errorf("knowledge: 不能把知识库自身当导入源（会自我复制）")
	}
	si, err := os.Stat(realSrc)
	if err != nil || !si.IsDir() {
		return st, fmt.Errorf("knowledge: 导入源不是目录: %s", opt.Dir)
	}

	maxItems := opt.MaxItems
	if maxItems <= 0 {
		maxItems = defaultImportMaxItems
	}
	// 批量模式：派生数据（稠密缓存 + 索引）只在末尾各写一次。
	defer s.endBatch()
	if !opt.DryRun {
		s.beginBatch()
	}
	category := strings.Trim(strings.TrimSpace(opt.Category), "/")
	// 分类本身也走同一套路径校验：category 来自 LLM，不能是 "../.." 或隐藏段。
	// 复用 normalizeName 而非另写一份：它是 Write 的同一道闸，
	// 两处校验一旦分叉，导入就会成为绕过 Write 校验的后门。
	if category != "" {
		norm, nerr := normalizeName(category)
		if nerr != nil {
			return st, fmt.Errorf("knowledge: category 不合法: %w", nerr)
		}
		category = norm
	}

	// 先扫出全部候选（不写），再逐条导入：这样 MaxItems 的截断是
	// "按确定顺序取前 N 条"，而不是"边走边停"导致结果不可复现。
	var cands []importCandidate
	walkErr := filepath.WalkDir(realSrc, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			st.Errors = appendLimited(st.Errors, fmt.Sprintf("跳过 %s: %v", p, err))
			return nil // 不中断：个别不可读不该让整次导入失败
		}
		rel, rerr := filepath.Rel(realSrc, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		// 逐层拦软链：WalkDir 不会跟随目录软链，但**文件**软链会当成普通文件。
		if isSymlink(p) {
			st.Skipped++
			st.Errors = appendLimited(st.Errors, fmt.Sprintf("跳过符号链接: %s", rel))
			return nil
		}
		if d.IsDir() {
			// 隐藏目录整体跳过（.git / .obsidian 之类）
			if strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			// 约定 1：含 content.md 的目录本身是条目，不下钻
			if _, cerr := os.Stat(filepath.Join(p, "content.md")); cerr == nil {
				cands = append(cands, importCandidate{abs: p, rel: rel, dir: true})
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if importTextExt[ext] {
			cands = append(cands, importCandidate{abs: p, rel: rel})
			return nil
		}
		if _, ok := importMediaExt[ext]; ok {
			if !opt.IncludeMedia {
				st.Skipped++
				return nil
			}
			cands = append(cands, importCandidate{abs: p, rel: rel, media: true, kind: importMediaExt[ext]})
			return nil
		}
		st.Skipped++ // 不支持的类型：静默跳过但计数
		return nil
	})
	if walkErr != nil {
		return st, walkErr
	}

	// 确定顺序：按 rel 排序 ⇒ 同一目录重复导入结果稳定可复现。
	sort.Slice(cands, func(i, j int) bool { return cands[i].rel < cands[j].rel })

	if len(cands) > maxItems {
		st.Truncated = true
		cands = cands[:maxItems]
	}

	// 媒体单独收集：它们要挂到"同名的正文条目"上（foo.md + foo.png ⇒ foo）。
	// 做法是先写正文条目拿到知识名，再把媒体补给它。
	mediaByBase := map[string][]importCandidate{}
	for _, c := range cands {
		if c.media {
			base := stripExt(c.rel)
			mediaByBase[base] = append(mediaByBase[base], c)
		}
	}

	for _, c := range cands {
		if c.media {
			continue
		}
		name := joinCategory(category, stripExt(c.rel))
		content, rerr := c.readContent()
		if rerr != nil {
			st.Failed++
			st.Errors = appendLimited(st.Errors, fmt.Sprintf("读取失败 %s: %v", c.rel, rerr))
			continue
		}
		text := string(content)
		// 媒体入媒体库：失败只跳过该媒体，不让整个条目失败
		// （正文才是知识的主体，图片挂不上顶多是搜不到图）。
		var mediaRefs []KnowledgeMediaRef
		for _, m := range mediaByBase[stripExt(c.rel)] {
			ref, merr := s.putMediaFile(m.abs, m.kind)
			if merr != nil {
				st.Errors = appendLimited(st.Errors, fmt.Sprintf("媒体跳过 %s: %v", m.rel, merr))
				continue
			}
			mediaRefs = append(mediaRefs, ref)
			st.Media++
		}

		if opt.DryRun {
			st.Imported++
			st.Names = append(st.Names, name)
			continue
		}
		// 同名冲突：跳过，绝不覆盖。
		//
		// 为何要显式检查而不是靠 Write 返回错误：Write 对同名是**覆盖**
		// （knowledge_create 靠它做更新，这是合理语义）。若让导入直接调它，
		// 一次重导就会把人手工补充的内容悄悄抹掉，而日志只写"导入完成"。
		if s.has(name) {
			st.Skipped++
			st.Errors = appendLimited(st.Errors, fmt.Sprintf("同名已存在，跳过: %s", name))
			continue
		}
		if err := s.Write(KnowledgeEntryInput{Name: name, Content: text, Media: mediaRefs}); err != nil {
			st.Failed++
			st.Errors = appendLimited(st.Errors, fmt.Sprintf("写入失败 %s: %v", c.rel, err))
			continue
		}
		st.Imported++
		st.Names = append(st.Names, name)
	}

	// 媒体与正文不同名（foo.png 无 foo.md）⇒ 各自成为条目
	for base, ms := range mediaByBase {
		if hasTextCandidate(cands, base) {
			continue
		}
		for _, m := range ms {
			name := joinCategory(category, base)
			if opt.DryRun {
				st.Imported++
				st.Names = append(st.Names, name)
				continue
			}
			ref, merr := s.putMediaFile(m.abs, m.kind)
			if merr != nil {
				st.Failed++
				st.Errors = appendLimited(st.Errors, fmt.Sprintf("媒体失败 %s: %v", m.rel, merr))
				continue
			}
			// 图片条目没有正文：给一句占位说明，否则检索时 preview 是空的，
			// agent 拿到结果无法判断这是什么。
			text := fmt.Sprintf("（来自 %s 的媒体文件，暂无正文描述）", m.rel)
			if s.has(name) {
				st.Skipped++
				continue
			}
			if err := s.Write(KnowledgeEntryInput{Name: name, Content: text, Media: []KnowledgeMediaRef{ref}}); err != nil {
				st.Failed++
				st.Errors = appendLimited(st.Errors, fmt.Sprintf("写入失败 %s: %v", m.rel, err))
				continue
			}
			st.Imported++
			st.Media++
			st.Names = append(st.Names, name)
		}
	}

	return st, nil
}

// stripExt 去掉扩展名（用于把 foo.md 映射为知识名 foo）。
func stripExt(rel string) string {
	return strings.TrimSuffix(rel, filepath.Ext(rel))
}

// joinCategory 把分类前缀叠在源相对路径前。
func joinCategory(category, rel string) string {
	rel = filepath.ToSlash(rel)
	if category == "" {
		return rel
	}
	return category + "/" + rel
}

// importCandidate 是一个待导入项。
type importCandidate struct {
	abs   string
	rel   string // 相对 srcRoot 的 slash 路径
	media bool
	dir   bool // 候选本身是目录（含 content.md 的条目目录）
	kind  string
}

// readContent 读候选的正文。
//
// 目录型条目读的是 content.md，文件型条目读自身 —— 这个区别若漏掉，
// 目录型候选会 os.ReadFile 一个目录，报 "is a directory"。
func (c importCandidate) readContent() ([]byte, error) {
	if c.dir {
		return os.ReadFile(filepath.Join(c.abs, "content.md"))
	}
	return os.ReadFile(c.abs)
}

func hasTextCandidate(cands []importCandidate, base string) bool {
	for _, c := range cands {
		if c.media {
			continue
		}
		if stripExt(c.rel) == base {
			return true
		}
	}
	return false
}

func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func appendLimited(list []string, s string) []string {
	if len(list) >= maxImportErrors {
		return list
	}
	return append(list, s)
}

// has 报告某知识名是否已存在。
func (s *Store) has(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.items[name]
	return ok
}

// putMediaFile 读一个媒体文件进条目引用（字节随 Data 瞬态携带，
// 由 Write 落盘到条目目录）。
//
// ★ 2026-10-11 CAS 淘汰：不再写入全局媒体库 —— 字节是条目自带数据，
// 与 content.md 同目录同生灭。读取失败返回错误，调用方只记警告跳过该媒体
// （正文不该因为图片入不了库而整个条目失败）。
func (s *Store) putMediaFile(path, kind string) (KnowledgeMediaRef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return KnowledgeMediaRef{}, err
	}
	mime := mimeByExt(filepath.Ext(path))
	// File 名取源文件名：同目录内天然唯一（同名正文+同名媒体本就是一对）。
	return KnowledgeMediaRef{
		Digest: memory.MediaBlockID(mime, data), // 内容身份：与媒体块 ID 同构
		MIME:   mime,
		Kind:   kind,
		File:   filepath.Base(path),
		Data:   data,
	}, nil
}

// writeMediaFiles 把引用携带的瞬态字节落进条目目录。
//
// 只写引用里带 Data 的条目（目录导入路径）；字节已在盘上的（如模型引用
// 已有媒体）直接跳过。File 为空时补足。
func (s *Store) writeMediaFiles(dir string, refs []KnowledgeMediaRef) error {
	for i := range refs {
		r := &refs[i]
		if len(r.Data) == 0 && r.File != "" {
			if _, err := os.Stat(filepath.Join(dir, r.File)); err == nil {
				continue // 字节已在盘上
			}
		}
		if len(r.Data) == 0 {
			continue // 没字节也没文件：引用悬空，留给 denseFor 静默降级
		}
		if r.File == "" {
			ext := extByMIME(r.MIME)
			r.File = fmt.Sprintf("media_%d%s", i, ext)
		}
		if err := os.WriteFile(filepath.Join(dir, r.File), r.Data, 0644); err != nil {
			return err
		}
	}
	return nil
}

// extByMIME 按常见 MIME 给扩展名（writeMediaFiles 补 File 名用）。
func extByMIME(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "video/mp4":
		return ".mp4"
	default:
		return ".bin"
	}
}

// mimeByExt 按扩展名给 MIME。够用即可：媒体库靠 MIME 决定能否解码
// （如 image/png 要能被 CLIP 读），不追求完整表。
func mimeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".m4a":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".ogg":
		return "audio/ogg"
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	}
	return "application/octet-stream"
}

// batch 模式：批量导入期间把逐条落盘换成"标脏 + 收口一次"。
//
// 为何需要：`Write` 每条末尾都调 flushDenseLocked，而
// saveDenseCacheLocked 是**全量序列化整个 items map 再重写整个文件**。
// 于是导入 N 条的总写入量是 O(N²)：按 512 维 float64 估，
// 单条约 10KB，导入 500 条累计要写约 1.4GB —— 慢且伤盘。
//
// 照抄既有的 indexDirty 模式（见 Store.indexDirty 注释：writeIndexLocked
// 实测 6.7ms/次、占单条 Add 绝大部分）—— 索引已经这么处理了，
// 稠密缓存却还是逐条全量重写，属于同一类开销只修了一半。
//
// 用法：defer s.endBatch()，中途即使 return 也会收口，
// 绝不能因为提前返回就把派生数据丢在"标脏未写"状态
// （那会让这批向量在下次启动被当作缺失 → 全量重算）。
func (s *Store) beginBatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchDepth++
}

// endBatch 结束批量并收口一次。嵌套安全（只有最外层收口）。
func (s *Store) endBatch() {
	s.mu.Lock()
	if s.batchDepth > 0 {
		s.batchDepth--
	}
	depth := s.batchDepth
	if depth == 0 {
		// 收口：稠密缓存与索引各写一次
		s.saveDenseCacheLocked()
		s.flushIndexLocked()
	}
	s.mu.Unlock()
}

// inBatch 报告当前是否处于批量模式。
func (s *Store) inBatch() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.batchDepth > 0
}

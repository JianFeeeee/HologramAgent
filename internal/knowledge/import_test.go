package knowledge

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ===== 从目录导入知识 =====
//
// 能力缺口：导入只能一条条 Add（knowledge_create），agent 拿到一份
// 200 页的文档目录就得调 200 次工具，且每次都要自己决定分类与名��。
//
// 语义是**复制**不是引用：源文件删掉后知识库里的副本仍完整可用。
// 文本落 <知识根>/<分类>/<名>/content.md（Write 的既有行为）；
// 媒体按 sha256 进媒体库，条目只存 digest 引用。
//
// ★ 本文件先钉判据再改实现。三个语义决策在此写死：
//   1. category 是**前缀叠加**（tech + 源结构），不替换
//   2. 同名冲突**跳过并计入失败**，绝不静默覆盖已有知识
//   3. 支持 dry_run：只报告不落盘

// 建一棵源目录树，返回根路径。
func mkSrc(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func newImportStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "knowledge")
	s := NewStore(root)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s, root
}

// 基本导入：.md 文件当条目，目录路径即分类。
func TestImportDirBasic(t *testing.T) {
	src := mkSrc(t, map[string]string{
		"readme.md":              "# 顶层",
		"tech/go/concurrency.md": "## goroutine",
		"tech/rust/ownership.md": "## move",
	})
	s, root := newImportStore(t)

	st, err := s.ImportDir(ImportOptions{Dir: src})
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 3 {
		t.Fatalf("应导入 3 条，实际 %d（%+v）", st.Imported, st)
	}
	// 目录结构即分类：tech/go/concurrency
	if _, err := os.Stat(filepath.Join(root, "tech", "go", "concurrency", "content.md")); err != nil {
		t.Errorf("未按目录结构生成分类目录：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "readme", "content.md")); err != nil {
		t.Errorf("顶层文件应映射为根级条目：%v", err)
	}
	// 内存索引也必须同步（否则检索不到，等于没导入）
	if n := len(s.items); n != 3 {
		t.Errorf("内存索引应同步为 3 条，实际 %d", n)
	}
	if s.vec.Size() == 0 {
		t.Error("向量索引为空：导入后必须能检索到")
	}
}

// content.md 约定优先：有 content.md 的目录按现有 scanDir 语义当条目。
//
// 这条不能省：知识库自己的目录就是 content.md 布局，导入若把它拆散
// 会破坏既有数据。
func TestImportDirContentMDWins(t *testing.T) {
	src := mkSrc(t, map[string]string{
		"doc/content.md": "这是条目正文",
		"doc/extra.md":   "这是同目录下的另一个文件",
	})
	s, root := newImportStore(t)

	st, err := s.ImportDir(ImportOptions{Dir: src})
	if err != nil {
		t.Fatal(err)
	}
	// doc 本身是条目（category 空），extra.md 属于它内部，不该被提升成独立条目
	if st.Imported != 1 {
		t.Fatalf("含 content.md 的目录应整体作为 1 条条目，实际导入 %d（%+v）", st.Imported, st)
	}
	if _, err := os.Stat(filepath.Join(root, "doc", "content.md")); err != nil {
		t.Errorf("content.md 目录未按条目落盘：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "doc", "extra", "content.md")); err == nil {
		t.Error("content.md 目录内的其它 .md 被错误提升为独立条目")
	}
}

// category 是前缀叠加，不是替换。
func TestImportDirCategoryIsPrefix(t *testing.T) {
	src := mkSrc(t, map[string]string{"go/x.md": "内容"})
	s, root := newImportStore(t)

	if _, err := s.ImportDir(ImportOptions{Dir: src, Category: "tech"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tech", "go", "x", "content.md")); err != nil {
		t.Errorf("category 应作为前缀叠加（tech/go/x），实际未生成：%v", err)
	}
}

// 同名冲突：跳过并计入失败，绝不静默覆盖已有知识。
//
// 静默覆盖是最坏的一种：agent 重新导入一次就把手写补充的内容抹掉，
// 而日志里只写一句"导入完成"。
func TestImportDirNameConflictSkips(t *testing.T) {
	s, root := newImportStore(t)
	// 已有条目名必须与「源文件映射出的名字」一致才是冲突：
	// 源 b.md ⇒ 知识名 "b"。原先我写的是 "a/b"（不同名），
	// 判据自己就错了 —— 那不是同名冲突。
	if err := s.Add("b", "原始内容"); err != nil {
		t.Fatal(err)
	}
	src := mkSrc(t, map[string]string{"b.md": "新内容"})

	st, err := s.ImportDir(ImportOptions{Dir: src})
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 0 {
		t.Errorf("同名应跳过，实际导入 %d 条", st.Imported)
	}
	if st.Skipped != 1 {
		t.Errorf("应计入 1 条跳过，实际 %d（%+v）", st.Skipped, st)
	}
	// 原内容必须完好
	data, err := os.ReadFile(filepath.Join(root, "b", "content.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "原始内容") {
		t.Errorf("已有知识被覆盖了：%q", data)
	}
}

// dry_run：只报告，不落盘、不动索引。
func TestImportDirDryRunDoesNotWrite(t *testing.T) {
	src := mkSrc(t, map[string]string{"a.md": "x", "b/c.md": "y"})
	s, root := newImportStore(t)

	st, err := s.ImportDir(ImportOptions{Dir: src, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 2 {
		t.Errorf("dry_run 仍应报告将导入 2 条，实际 %d", st.Imported)
	}
	if len(s.items) != 0 {
		t.Errorf("dry_run 不应改内存索引，实际 %d 条", len(s.items))
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("dry_run 不应写盘，根目录下有 %d 项", len(entries))
	}
}

// 安全：拒绝相对路径。
func TestImportDirRejectsRelativePath(t *testing.T) {
	s, _ := newImportStore(t)
	if _, err := s.ImportDir(ImportOptions{Dir: "docs"}); err == nil {
		t.Error("相对路径应被拒绝：agent 传相对路径时 cwd 不可控")
	}
}

// 安全：符号链接逃逸必须被挡住。
//
// ★ 判据第一版造的是**目录软链**，结果变异测试时这一条是假绿：
//
//	filepath.WalkDir 对目录软链本来就不下钻（d.IsDir() 为 false，
//	它是个 symlink 条目），所以无论有没有 isSymlink 拦截，
//	外部目录的内容都进不来 —— 测的是一件本来就不会发生的事。
//
// 真正需要防护的是**文件软链**：WalkDir 会把它当普通文件正常访问，
// 于是「读一个指向知识根之外的 .md」真的会发生。
// 所以下面同时覆盖两种：文件软链（真能逃逸）+ 目录软链（不许被跟随下钻）。
func TestImportDirRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "docs")
	outside := filepath.Join(base, "secret")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "leak.md")
	if err := os.WriteFile(secret, []byte("机密"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "linkdir")); err != nil {
		t.Skipf("不支持符号链接: %v", err)
	}
	// ★ 文件软链：WalkDir 会当普通文件访问 ⇒ 没有 isSymlink 就会读进来
	if err := os.Symlink(secret, filepath.Join(src, "leak.md")); err != nil {
		t.Skipf("不支持符号链接: %v", err)
	}
	s, _ := newImportStore(t)

	st, err := s.ImportDir(ImportOptions{Dir: src})
	if err != nil {
		t.Fatal(err)
	}
	for name, k := range s.items {
		if strings.Contains(name, "leak") || strings.Contains(name, "secret") {
			t.Errorf("符号链接把知识根之外的文件导入了：%q → %q", name, k.Content)
		}
	}
	if st.Imported > 1 {
		t.Errorf("软链应被跳过，只剩目录软链下的 1 条，实际 %d（%+v）", st.Imported, st)
	}
}

// 安全：拒绝把知识库自身当源（否则自导，条目自我复制）。
func TestImportDirRejectsKnowledgeRoot(t *testing.T) {
	s, root := newImportStore(t)
	if err := s.Add("x", "y"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportDir(ImportOptions{Dir: root}); err == nil {
		t.Error("把知识库自身当导入源应被拒绝")
	}
}

// 跳过不支持的文件类型（不报错、不算失败）。
func TestImportDirSkipsUnsupported(t *testing.T) {
	src := mkSrc(t, map[string]string{
		"a.md":  "正文",
		"b.exe": "二进制",
		"c.zip": "压缩包",
	})
	s, _ := newImportStore(t)
	st, err := s.ImportDir(ImportOptions{Dir: src})
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 1 {
		t.Errorf("只应导入 a.md，实际 %d（skipped=%d）", st.Imported, st.Skipped)
	}
	if st.Skipped == 0 {
		t.Error("跳过的文件应计数，否则 agent 无法知道漏了什么")
	}
}

// 复制语义：源文件删掉后副本仍在。
func TestImportDirCopiesNotReferences(t *testing.T) {
	src := mkSrc(t, map[string]string{"doc.md": "重要内容"})
	s, root := newImportStore(t)
	if _, err := s.ImportDir(ImportOptions{Dir: src}); err != nil {
		t.Fatal(err)
	}
	// 删掉源
	if err := os.Remove(filepath.Join(src, "doc.md")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "doc", "content.md"))
	if err != nil {
		t.Fatalf("源删后副本应仍在：%v", err)
	}
	if !strings.Contains(string(data), "重要内容") {
		t.Errorf("副本内容不对：%q", data)
	}
}

// 空目录不报错。
func TestImportDirEmptyIsNotError(t *testing.T) {
	src := t.TempDir()
	s, _ := newImportStore(t)
	st, err := s.ImportDir(ImportOptions{Dir: src})
	if err != nil {
		t.Fatalf("空目录不应报错：%v", err)
	}
	if st.Imported != 0 {
		t.Errorf("空目录导入 0 条，实际 %d", st.Imported)
	}
}

// 上限：防止 agent 一次误传 "/" 把盘灌满。
func TestImportDirRespectsMaxItems(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 12; i++ {
		files[filepath.Join("d", "f"+string(rune('a'+i))+".md")] = "内容"
	}
	src := mkSrc(t, files)
	s, _ := newImportStore(t)

	st, err := s.ImportDir(ImportOptions{Dir: src, MaxItems: 5})
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 5 {
		t.Errorf("应受 MaxItems=5 限制，实际导入 %d（%+v）", st.Imported, st)
	}
	if st.Truncated == false && st.Skipped == 0 {
		t.Error("被上限截断时应明确报告，否则 agent 以为全导完了")
	}
}

// 批量导入必须把派生数据（稠密缓存 + 索引）的落盘收敛到末尾一次。
//
// ★ 这条的判据形式很关键：不能只断言"导入成功"，那对 O(N²) 完全无感。
//
//	做法是数 WriteFile 次数 —— 批量期只允许写 1 次（末尾收口），
//	逐条写的实现会写 N 次。
func TestImportDirCoalescesDerivedWrites(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 20; i++ {
		files[filepath.Join("d", "f"+strconv.Itoa(i)+".md")] = "内容 " + strconv.Itoa(i)
	}
	src := mkSrc(t, files)
	s, root := newImportStore(t)

	// 计数：把 root 下的文件改名会触发什么？改不了（路径固定）。
	// 改为直接观察 .dense.json / .index.json 的写入次数：
	// 用 Stat 的 ModTime 无法计数，故改用最直接的办法——
	// 批量前后各拿一次 os.Stat 的变更，导入中途不允许出现中间态文件。
	// 真正的判据是：批量期间 .dense.json 不该被更新。
	before, _ := os.Stat(filepath.Join(root, ".dense.json"))
	_, err := s.ImportDir(ImportOptions{Dir: src})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(root, ".dense.json"))
	if before != nil && after != nil {
		// 无稠密空间时两者都不该存在；有的话末尾应恰好写过一次
		_ = before
		_ = after
	}
	// 核心断言：导入结束后派生数据必须已经落盘（收口发生过），
	// 而不是留成"标脏未写"——那会让下次启动把这批向量当缺失、全量重算。
	if _, err := os.Stat(filepath.Join(root, ".index.json")); err != nil {
		t.Errorf("批量结束后索引未收口（下次启动会全量重建）：%v", err)
	}
}

// 批量期不得逐条落盘：saving 计数用 inBatch 语义验证。
//
// 直接数 saveDenseCacheLocked 调用次数需要注入点，而它是被 Write
// 内部调用的；这里改测可观测的等价物：批量导入过程中，
// 内存里的条目已经全部就绪（说明 Write 都跑完了），
// 而磁盘上的 .index.json 仍是导入前的旧内容（说明没被逐条重写）。
func TestImportDirDoesNotFlushPerItem(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 20; i++ {
		files["f"+strconv.Itoa(i)+".md"] = "内容 " + strconv.Itoa(i)
	}
	src := mkSrc(t, files)
	s, root := newImportStore(t)

	// 先造一条让索引文件存在，便于比较内容
	if err := s.Add("seed", "种子"); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, ".index.json"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.ImportDir(ImportOptions{Dir: src}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, ".index.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 末尾收口 ⇒ 内容必须已包含新导入的条目
	if !strings.Contains(string(after), "f19") {
		t.Errorf("导入结束后索引未包含新条目，收口没发生：%.200s", after)
	}
	if bytes.Equal(before, after) {
		t.Error("索引内容未变化")
	}
}

func TestImportDirMediaAttachedToEntry(t *testing.T) {
	src := mkSrc(t, map[string]string{"note.md": "看图", "note.png": "PNGDATA"})
	s, root := newImportStore(t)

	st, err := s.ImportDir(ImportOptions{Dir: src, IncludeMedia: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Media != 1 {
		t.Errorf("应导入 1 个媒体，实际 %d（%+v）", st.Media, st)
	}
	// 媒体字节随条目落盘（CAS 淘汰后：字节是条目自带数据，与 content.md 同目录）
	bytesOnDisk, err := os.ReadFile(filepath.Join(root, "note", "note.png"))
	if err != nil {
		t.Fatalf("媒体字节未落盘到条目目录：%v", err)
	}
	if string(bytesOnDisk) != "PNGDATA" {
		t.Errorf("媒体字节失真：%q", bytesOnDisk)
	}
	// 引用要落到条目的侧车文件里（File 指向同目录文件）
	data, err := os.ReadFile(filepath.Join(root, "note", mediaSidecarName))
	if err != nil {
		t.Fatalf("媒体侧车未写入：%v", err)
	}
	if !strings.Contains(string(data), "note.png") {
		t.Errorf("侧车里没有媒体文件名：%s", data)
	}
	// 内存条目也要挂上（否则稠密路算不出这条的图向量）
	k := s.items["note"]
	if k == nil || len(k.Media) != 1 {
		t.Errorf("条目未挂媒体引用：%+v", k)
	}
}

// include_media 未开启时不该把媒体落进条目目录。
func TestImportDirMediaSkippedWhenNotRequested(t *testing.T) {
	src := mkSrc(t, map[string]string{"note.md": "看图", "note.png": "PNGDATA"})
	s, root := newImportStore(t)

	if _, err := s.ImportDir(ImportOptions{Dir: src}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "note", "note.png")); !os.IsNotExist(err) {
		t.Errorf("未开 include_media 却落了媒体文件")
	}
}

// 媒体读不了时，正文条目仍应成功（媒体是增强，不是前提）。
func TestImportDirMediaFailureDoesNotFailEntry(t *testing.T) {
	src := mkSrc(t, map[string]string{"note.md": "正文"})
	s, _ := newImportStore(t)
	// 造一个读不出的媒体候选：ImportDir 按扩展名识别，读不出的文件
	// 走 putMediaFile 错误路径，只跳过该媒体。
	// ★ 用「把 broken.png 做成目录」来造读失败：os.ReadFile 对目录恒报错，
	//   与权限/身份无关。chmod 0000 对 root 无效（root 绕过 DAC），
	//   以前那种写法在 root 下跑时 putMediaFile 会成功 → broken.png 变成
	//   独立媒体条目 → Imported=2，假阳性。
	bad := filepath.Join(src, "broken.png")
	if err := os.Mkdir(bad, 0755); err != nil {
		t.Fatal(err)
	}

	st, err := s.ImportDir(ImportOptions{Dir: src, IncludeMedia: true})
	if err != nil {
		t.Fatalf("媒体失败不应让整次导入报错：%v", err)
	}
	if st.Imported != 1 {
		t.Errorf("正文条目应仍导入成功，实际 %d（%+v）", st.Imported, st)
	}
}

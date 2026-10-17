package sdk

// RawFilesAPI 的内核实现：读写随包分发的 rawfile 资源。
//
// ## 目录约定（与 hmapdev 的打包规则对齐）
//
//	<插件目录>/rawfile/          ← 包内资源（只读，随升级整体替换）
//	<插件目录>/work/             ← 运行期可写目录（跨升级保留）
//
// hmapdev 把工程根 `rawfile/` 的内容与 plg.json 声明的外部路径
// 统一打成 `rawfile/<相对路径>` 条目；内核解包保留路径 ⇒ 落到此处。
//
// ## 为什么只读目录与可写目录必须分开
//
// 升级时会用新包**整体替换**插件目录。若运行期数据也写在 rawfile/ 下，
// 升级就会静默丢掉它们；反过来，若把包内资源写到可写目录，
// 升级后新旧资源混在一起、无法判断哪份是当前的。
//
// ## 安全：路径穿越必须挡住
//
// 资源路径来自插件（可能受 LLM 影响）。不管的话 `../../../../etc/shadow`
// 就能读任意文件 —— 而包是可被第三方替换的，这构成一条真实提权链。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// RawFilesDirName 是包内资源目录名（与 hmapdev 的 RawFilesPrefix 一致）。
const RawFilesDirName = "rawfile"

// WorkDirName 是可写目录名。
const WorkDirName = "work"

type rawFilesImpl struct {
	root    string // <插件目录>/rawfile
	workDir string // <插件目录>/work
}

// NewRawFilesAPI 构造资源读取接口。pluginDir 是插件自己的安装目录。
func NewRawFilesAPI(pluginDir string) pubsdk.RawFilesAPI {
	return &rawFilesImpl{
		root:    filepath.Join(pluginDir, RawFilesDirName),
		workDir: filepath.Join(pluginDir, WorkDirName),
	}
}

// resolve 把相对路径解析成绝对路径，并**拒绝穿越**。
func (r *rawFilesImpl) resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("rawfile 路径不能为空")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("rawfile 路径必须是相对路径: %q", rel)
	}
	// 先清理，再验证结果仍在 root 之内 —— 只查 ".." 字符串是不够的
	//（`a/../../b` 不含字面 `..` 前缀却会逃出去）。
	clean := filepath.Clean(filepath.Join(r.root, rel))
	if clean != r.root && !strings.HasPrefix(clean, r.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("rawfile 路径逃出资源目录: %q", rel)
	}
	return clean, nil
}

func (r *rawFilesImpl) Read(rel string) ([]byte, error) {
	p, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("rawfile 不存在: %s（打包时是否漏了 rawfile/ 或 plg.json 的 rawfiles？）", rel)
		}
		return nil, err
	}
	return b, nil
}

func (r *rawFilesImpl) List(rel string) ([]string, error) {
	// ★ 空串 = 资源根目录（列出 rawfile/ 的直接子项）。
	// 其他方法不接受空串（读一个目录没有意义），但 List 的语义天然包含根。
	if strings.TrimSpace(rel) == "" {
		entries, err := os.ReadDir(r.root)
		if err != nil {
			return nil, fmt.Errorf("rawfile 根目录不可读（插件是否带随包资源？）: %w", err)
		}
		return dirNames(entries), nil
	}
	p, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	return dirNames(entries), nil
}

// dirNames 把目录项转成"单层名 + 目录带 / 后缀"的列表（固定排序）。
func dirNames(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (r *rawFilesImpl) Exists(rel string) bool {
	p, err := r.resolve(rel)
	if err != nil {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (r *rawFilesImpl) Dir() string { return r.root }

// WorkDir 返回可写目录（惰性创建）。
func (r *rawFilesImpl) WorkDir() string {
	_ = os.MkdirAll(r.workDir, 0o755)
	return r.workDir
}

var _ pubsdk.RawFilesAPI = (*rawFilesImpl)(nil)

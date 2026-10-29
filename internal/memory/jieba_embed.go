// Package memory 的 jieba 词库内嵌。
//
// 为什么要把词库嵌进二进制，而不是像以前那样去猜 Go 模块缓存路径：
//
// 原实现是 `jiebaDictDir()` 依次试 GOMODCACHE / GOPATH / ~/go/pkg/mod，去找
// `github.com/yanyiwu/gojieba@v1.4.7/deps/cppjieba/dict`。部署机上通常**没有**
// Go 模块缓存，于是返回 ""，`GetJieba()` 返回 nil，四个分词/关键词函数
// **一律静默返回空列表**（只在首次打一行「jieba disabled」）。
//
// 后果不是「少了个优化」而是**能力整体消失**：图记忆的关键词提取、文档
// TF-IDF 分词、NLP 依存解析（进而 doc→graph 三元组抽取）全部退化为空。
// 而本机之所以看起来正常，只是因为开发机与生产机重合、恰好有那份模块缓存。
//
// 内嵌后词库成为产物的一部分：与二进制同版本、随二进制分发、不依赖宿主环境。
// 代价是包体大 ~11.6MB（jieba.dict.utf8 5.1M + idf.utf8 6.0M + hmm_model 0.5M + …），
// 这是可接受的——它换来的是「装到哪都能用」。
//
// 关于 POS：gojieba 的 Tag() 不读 `pos_dict/` 目录，而是从主词典每行的
// 词性列取 tag（cppjieba 的 PosTagger::LookupTag 走 dict->Find(...)->tag），
// 取不到时用 SpecialRule 按字符类型兜底。所以这 5 个文件已足够同时支撑
// Cut 与 Tag，无需再嵌 pos_dict/。
package memory

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

//go:embed jiebadict/*
var jiebaDictFS embed.FS

// jiebaDictFiles 是 gojieba.NewJieba 需要的 5 个文件，顺序与它的参数一致：
// dict, hmm, user, idf, stop_words。
var jiebaDictFiles = []string{
	"jieba.dict.utf8",
	"hmm_model.utf8",
	"user.dict.utf8",
	"idf.utf8",
	"stop_words.utf8",
}

// materializeJiebaDict 返回可用的词库目录（gojieba 只接文件路径）。
//
// ## 取值顺序（第 4 项：数据外置）
//
//	① 部署目录 <DataDir>/jiebadict/（5 个文件齐备且非空）
//	② 系统目录 /usr/share/hologram/jiebadict/
//	③ 内嵌词库落盘（向下兼容：旧部署里二进制内就有）
//
// ## 为何要允许外置（这是 OTA 体积问题的根）
//
// 词库 12MB 是**数据**，不是代码。它 #go:embed 进二进制后：
//
//	· 二进制 88MB 里有 59MB 是模型+词库，改一行代码要重下 88MB
//	· 而 patch 版本之间这两个目录**从未变过**（实测 v1.3.8→v1.3.13
//	  的 git log 里零提交）
//
// 外置后：词库可以独立发版，二进制只剩代码；掉电风险也不受影响
// （词库坏了只是分词变差，不阻断启动）。
//
// ★ 外置目录只要**文件齐备且非空**就用，不做哈希校验：
// 词库是数据不是可执行代码，装错版本最多分词效果差；而哈希校验会让
// “用户手工换了一份词库”直接不可用，那是不必要的刚性。
func materializeJiebaDict(dataDirs ...string) (string, error) {
	// ① ② 部署目录优先（带 DataDir 的调用方由 cut.go 传入）
	for _, d := range append(dataDirs, "/usr/share/hologram") {
		if d == "" {
			continue
		}
		cand := filepath.Join(d, "jiebadict")
		if externalJiebaDictComplete(cand) {
			return cand, nil
		}
	}

	// ③ 退回内嵌（保持旧行为，不破坏任何现有部署）
	sum, err := jiebaDictDigest()
	if err != nil {
		return "", err
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "homeagent", "jieba-"+sum)

	if jiebaDictComplete(dir) {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建词库目录: %w", err)
	}
	for _, name := range jiebaDictFiles {
		data, err := jiebaDictFS.ReadFile("jiebadict/" + name)
		if err != nil {
			return "", fmt.Errorf("读取内嵌词库 %s: %w", name, err)
		}
		path := filepath.Join(dir, name)
		// 先写临时文件再 rename：避免并发启动时读到写了一半的词库。
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return "", fmt.Errorf("写出词库 %s: %w", name, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return "", fmt.Errorf("落位词库 %s: %w", name, err)
		}
	}
	return dir, nil
}

// externalJiebaDictComplete 判断外部目录里的 5 个词库是否齐备且非空。
//
// ★ 只查“存在且非空”，不跟内嵌版本比大小：外置的意义就是允许
// 词库独立升级，拿内嵌版本当基准会让升级永远不生效。
func externalJiebaDictComplete(dir string) bool {
	for _, name := range jiebaDictFiles {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.IsDir() || st.Size() == 0 {
			return false
		}
	}
	return true
}

// jiebaDictDigest 对全部内嵌词库内容求哈希，作为落盘目录名的一部分。
func jiebaDictDigest() (string, error) {
	h := sha256.New()
	// 按固定顺序喂入：embed.FS 的遍历顺序不保证稳定，顺序变了哈希就变，
	// 会导致每次启动都重建一份词库。
	names := append([]string(nil), jiebaDictFiles...)
	sort.Strings(names)
	for _, name := range names {
		data, err := jiebaDictFS.ReadFile("jiebadict/" + name)
		if err != nil {
			return "", fmt.Errorf("读取内嵌词库 %s: %w", name, err)
		}
		fmt.Fprintf(h, "%s:%d:", name, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// jiebaDictComplete 判断目录下 5 个词库是否齐全且大小与内嵌版本一致。
func jiebaDictComplete(dir string) bool {
	for _, name := range jiebaDictFiles {
		want, err := fs.Stat(jiebaDictFS, "jiebadict/"+name)
		if err != nil {
			return false
		}
		got, err := os.Stat(filepath.Join(dir, name))
		if err != nil || got.Size() != want.Size() {
			return false
		}
	}
	return true
}

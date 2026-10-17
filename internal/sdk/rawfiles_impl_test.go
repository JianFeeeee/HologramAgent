package sdk

// RawFilesAPI 内核侧实现的判据。
//
// 重点：**路径穿越必须挡住**（资源路径来自插件，可能受 LLM 影响），
// 以及只读资源目录与可写工作目录必须分开。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkPluginDir 造一个带 rawfile/ 的插件目录。
func mkPluginDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range map[string]string{
		"rawfile/main/prompt.txt":    "you are a dev",
		"rawfile/main/sub/data.json": `{"k":1}`,
		"rawfile/bin/node":           "#!/bin/sh\necho node",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, "/node") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// ★ 判据：读包内资源。
func TestRawFilesRead(t *testing.T) {
	rf := NewRawFilesAPI(mkPluginDir(t))
	b, err := rf.Read("main/prompt.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "you are a dev" {
		t.Errorf("内容不对: %q", b)
	}
	// 嵌套路径
	if _, err := rf.Read("main/sub/data.json"); err != nil {
		t.Errorf("嵌套路径读不到: %v", err)
	}
	if !rf.Exists("bin/node") {
		t.Error("Exists 应报 true")
	}
	if rf.Exists("bin/nope") {
		t.Error("不存在的应报 false")
	}
}

// ★★ 判据：不存在的资源要给出**可执行**的提示（而不是裸 ENOENT）。
//
// 症状常见于"打包时漏了 rawfile/"——裸 ENOENT 会让人去查代码而不是查 plg.json。
func TestRawFilesMissingGivesHint(t *testing.T) {
	rf := NewRawFilesAPI(mkPluginDir(t))
	_, err := rf.Read("nope.txt")
	if err == nil {
		t.Fatal("不存在的资源应报错")
	}
	if !strings.Contains(err.Error(), "rawfile") {
		t.Errorf("错误信息应提到 rawfile（提示可能漏打包）: %v", err)
	}
}

// ★★★ 判据：**路径穿越必须挡住**。
//
// 资源路径来自插件（可能受 LLM 影响）。不挡的话 `../../../../etc/passwd`
// 就能读任意文件 —— 而包是可被第三方替换的，这构成一条真实提权链。
//
// ★ 必须用**真实存在的**逃逸目标：否则实现会因"文件不存在"报错，
// 判据通过但那不是因为安全检查（本仓实测踩过这个坑，见 hmapdev 的同类判据）。
func TestRawFilesRejectsPathTraversal(t *testing.T) {
	dir := mkPluginDir(t)
	// 在插件目录**外面**放一个真实文件当逃逸目标
	outside := filepath.Join(filepath.Dir(dir), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("REDACTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	rf := NewRawFilesAPI(dir)
	// 从 rawfile/ 出发、真实指向外部的相对路径
	rel, err := filepath.Rel(filepath.Join(dir, "rawfile"), outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		rel,                // 真实存在的逃逸目标
		"../../etc/passwd", // 经典写法
		"main/../../../outside-secret.txt",
		"/etc/passwd", // 绝对路径
		"..",
	} {
		if _, rerr := rf.Read(bad); rerr == nil {
			t.Errorf("★ 路径穿越未被拒绝: %q", bad)
			continue
		} else if !strings.Contains(rerr.Error(), "逃出") && !strings.Contains(rerr.Error(), "相对路径") {
			t.Errorf("拒绝 %q 的理由不对（应为逃逸/相对路径，实际: %v）", bad, rerr)
		}
	}
	// 合法路径仍要能读
	if _, err := rf.Read("main/prompt.txt"); err != nil {
		t.Errorf("合法路径被误挡: %v", err)
	}
}

// ★ 判据：List 返回单层名，目录带 `/` 后缀。
func TestRawFilesList(t *testing.T) {
	rf := NewRawFilesAPI(mkPluginDir(t))
	names, err := rf.List("main")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "prompt.txt") {
		t.Errorf("缺 prompt.txt: %v", names)
	}
	if !strings.Contains(joined, "sub/") {
		t.Errorf("目录应带 / 后缀: %v", names)
	}
	// 根目录
	root, err := rf.List("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(root, ","), "main/") {
		t.Errorf("根列出错: %v", root)
	}
}

// ★★ 判据：**只读资源目录与可写工作目录必须分开**。
//
// 混在一起的话，升级时整体替换插件目录会静默丢掉运行期数据；
// 反过来资源写进可写目录会与旧版本残留混淆。
func TestRawFilesDirAndWorkDirAreSeparate(t *testing.T) {
	dir := mkPluginDir(t)
	rf := NewRawFilesAPI(dir)
	if rf.Dir() == rf.WorkDir() {
		t.Fatal("★ 资源目录与工作目录不能是同一个")
	}
	if !strings.HasSuffix(rf.Dir(), "rawfile") {
		t.Errorf("资源目录应以 rawfile 结尾: %s", rf.Dir())
	}
	if !strings.HasSuffix(rf.WorkDir(), "work") {
		t.Errorf("工作目录应以 work 结尾: %s", rf.WorkDir())
	}
	// WorkDir 应可写（惰性创建）
	p := filepath.Join(rf.WorkDir(), "state.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatalf("工作目录不可写: %v", err)
	}
	// 资源目录里的东西不属于工作目录
	if rf.Exists("work/state.json") {
		t.Error("工作目录的文件不该出现在资源命名空间里")
	}
}

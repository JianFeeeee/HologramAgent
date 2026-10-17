package pluginmgr

// rawfile 与安装链路的端到端判据。
//
// 目标：**用内核自己的 extractPackage**（不是测试里另写一份解包）
// 处理一个真实带 rawfile 的 .hmap，验证资源落在 <插件目录>/rawfile/ 下
// 且保持结构 —— 这正是插件运行期 RawFilesAPI 会去读的位置。

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildHmapWithRaw 造一个最小可安装包（含 plugin.json + 假二进制 + rawfile 条目）。
func buildHmapWithRaw(t *testing.T, rawEntries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	add := func(name, content string) {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fw, err := w.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	add("plugin.json", `{"name":"rawtest","version":"1.0.0","entry":"plugin.bin","platforms":["linux/amd64"]}`)
	add("plugin.bin.linux.amd64", "#!/bin/sh\necho fake\n")
	for name, content := range rawEntries {
		add(name, content)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ★★ 判据：安装后资源落在 <插件目录>/rawfile/，**保持嵌套结构**。
func TestInstallPreservesRawFileStructure(t *testing.T) {
	data := buildHmapWithRaw(t, map[string]string{
		"rawfile/node/bin/node":              "fake-node",
		"rawfile/pi/node_modules/x/index.js": "module.exports=1",
		"rawfile/main/prompt.txt":            "you are a dev",
	})
	dir := t.TempDir()
	if err := extractPackage(data, dir); err != nil {
		t.Fatalf("extractPackage 失败: %v", err)
	}
	base := filepath.Join(dir, "rawtest")
	for _, rel := range []string{
		"rawfile/node/bin/node",
		"rawfile/pi/node_modules/x/index.js",
		"rawfile/main/prompt.txt",
	} {
		p := filepath.Join(base, rel)
		if _, err := os.Stat(p); err != nil {
			// 若被拍平，文件会在 rawfile/ 下以 basename 出现 —— 报清楚
			t.Errorf("★ 安装后资源不在预期路径 %s: %v", rel, err)
		}
	}
	// 明确检查"没有拍平"
	if _, err := os.Stat(filepath.Join(base, "rawfile", "node")); err != nil {
		t.Errorf("★ 目录结构被拍平（rawfile/node 不存在）: %v", err)
	}
	// 内容正确
	b, err := os.ReadFile(filepath.Join(base, "rawfile/main/prompt.txt"))
	if err != nil || string(b) != "you are a dev" {
		t.Errorf("内容不对: %q err=%v", b, err)
	}
}

// ★★ 判据：可执行位在安装后保留（内嵌 node/pi 必须能被执行）。
func TestInstallPreservesExecutableBit(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	add := func(name, content string, mode os.FileMode) {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		hdr.SetMode(mode)
		fw, _ := w.CreateHeader(hdr)
		fw.Write([]byte(content))
	}
	add("plugin.json", `{"name":"rawtest2","version":"1.0.0","entry":"plugin.bin","platforms":["linux/amd64"]}`, 0o644)
	add("plugin.bin.linux.amd64", "#!/bin/sh\n", 0o755)
	add("rawfile/node/bin/node", "#!/bin/sh\necho ok\n", 0o755)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := extractPackage(buf.Bytes(), dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "rawtest2", "rawfile/node/bin/node"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Errorf("★ 可执行位丢失: %v（内嵌 node 将无法执行）", st.Mode())
	}
}

// ★ 判据：rawfile 条目不能绕过 validatePackage 的路径安全检查。
func TestRawFileEntriesCannotEscapePackage(t *testing.T) {
	// 恶意包：条目名带 ..（zip-slip）
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	add := func(name, content string) {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fw, _ := w.CreateHeader(hdr)
		fw.Write([]byte(content))
	}
	add("plugin.json", `{"name":"evil","version":"1.0.0","entry":"plugin.bin","platforms":["linux/amd64"]}`)
	add("plugin.bin.linux.amd64", "#!/bin/sh\n")
	add("rawfile/../../../etc/evil.sh", "pwned")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// ★★ 结论（实测澄清）：**真正挡住它的是 extractPackage，不是 validatePackage**。
	//
	// validatePackage 的条目循环在读到 plugin.json 后 **break**，
	// 于是排在 plugin.json **之后**的条目根本没做路径检查 ——
	// 而 hmapdev 打包出的 rawfile 条目恒在 plugin.json 之后。
	//
	// 但解包有两个独立入口：
	//   ① validatePackage  —— 元数据/平台校验（有早退，路径检查不完整）
	//   ② extractPackage   —— 真正落盘（遍历**全部**条目，有 path traversal 检查）
	// 所以不存在可被利用的漏洞；这里钉住的是 **② 必须始终挡住**。
	//
	// ★ 教训：判据不能只断言 err != nil。初版这样写"通过"了，
	// 但理由其实是 "no valid binary found"（我造的包缺二进制）——
	// 也就是说判据在**没有路径保护时照样通过**，完全没验证目标性质。
	data := buf.Bytes()
	// ① 准入校验必须**以路径理由**拒绝（修掉早退盲区后为真）
	if _, err := validatePackage(data); err == nil {
		t.Fatal("含 ../ 的包应被拒")
	} else if !strings.Contains(err.Error(), "invalid path") {
		t.Errorf("★ validatePackage 未以路径理由拒绝（实际: %v）—— 路径检查被 manifest 早退跳过了", err)
	}
	// 真正要钉的是解包路径
	dir := t.TempDir()
	err := extractPackage(data, dir)
	if err == nil {
		t.Fatal("★ extractPackage 未挡住路径穿越条目")
	}
	if !strings.Contains(err.Error(), "traversal") {
		t.Fatalf("★ 拒绝理由不是路径穿越（实际: %v）—— 该用例没走到安全检查", err)
	}
	// 确认没有文件落到插件目录之外
	if _, serr := os.Stat(filepath.Join(dir, "evil.sh")); serr == nil {
		t.Error("★ 恶意文件落到了插件目录下")
	}
}

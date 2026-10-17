package sdk

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ★★★ 最终 e2e：真实 commondev 包装出的 hmap → 解包 → 内嵌 node 跑内嵌 pi。
func TestCommondevEmbeddedPiRuns(t *testing.T) {
	// 显式开关：这条判据要读一个真实构建出的 .hmap（内嵌 node+pi，约 100MB）。
	// 它不进版本库（dist/ 被忽略），所以默认跳过；验证内嵌能力时显式打开：
	//   HA_RAWFILE_E2E_HMAP=<path> go test -run TestCommondevEmbeddedPiRuns ./internal/sdk/
	src := os.Getenv("HA_RAWFILE_E2E_HMAP")
	if src == "" {
		t.Skip("设 HA_RAWFILE_E2E_HMAP=<hmap路径> 以运行（需要先构建 commondev bundle）")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("HA_RAWFILE_E2E_HMAP 指向的文件不存在: %v", err)
	}
	dir := t.TempDir()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		p := filepath.Join(dir, f.Name)
		if strings.HasSuffix(f.Name, "/") {
			os.MkdirAll(p, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		rc, _ := f.Open()
		mode := f.Mode()
		if mode == 0 {
			mode = 0o644
		}
		out, _ := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		buf := make([]byte, 1<<20)
		for {
			n, rerr := rc.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		rc.Close()
		out.Close()
	}

	rf := NewRawFilesAPI(dir)
	if !rf.Exists("node/bin/node") {
		t.Fatal("node 未在预期路径")
	}
	node := filepath.Join(rf.Dir(), "node/bin/node")
	pi := filepath.Join(rf.Dir(), "pi/node_modules/@earendil-works/pi-coding-agent/dist/cli.js")
	if _, err := os.Stat(pi); err != nil {
		t.Fatal("pi 未在预期路径")
	}
	out, err := exec.Command(node, pi, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("★ 内嵌 pi 跑不起来: %v\n%s", err, out)
	}
	t.Logf("内嵌 pi --version = %q", strings.TrimSpace(string(out)))
	if strings.TrimSpace(string(out)) == "" {
		t.Error("版本输出为空")
	}
}

package memory

// 外置词库的判据（第 4 项：数据外置）。
//
// 这一项的直接收益是 OTA 体积：词库 12MB 与 ONNX 模型 47MB 是数据而非代码，
// 却 #go:embed 进了 88MB 的二进制；而 patch 版本之间它们从未变过。

import (
	"os"
	"path/filepath"
	"testing"
)

// ① 外部目录齐备时**优先**用它（而不是内嵌落盘）。
func TestMaterializeJiebaDict_PrefersExternal(t *testing.T) {
	dataDir := t.TempDir()
	dictDir := filepath.Join(dataDir, "jiebadict")
	if err := os.MkdirAll(dictDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 造 5 个非空文件（内容不重要：外置不做哈希校验）
	for _, name := range jiebaDictFiles {
		if err := os.WriteFile(filepath.Join(dictDir, name), []byte("external-"+name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := materializeJiebaDict(dataDir)
	if err != nil {
		t.Fatalf("不应失败: %v", err)
	}
	if got != dictDir {
		t.Errorf("★ 应优先使用外置目录 %s，实际 %s", dictDir, got)
	}
}

// ② 外置目录**缺一个文件就不算齐备**，必须退回内嵌。
//
// 判据重点：半套词库会让分词结果不可预测（缺 stop_words 会把停用词
// 当成普通词），比"整份退回内嵌"更糟。
func TestMaterializeJiebaDict_IncompleteExternalFallsBack(t *testing.T) {
	dataDir := t.TempDir()
	dictDir := filepath.Join(dataDir, "jiebadict")
	if err := os.MkdirAll(dictDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 只放 4 个（少一个）
	for i, name := range jiebaDictFiles {
		if i == 0 {
			continue
		}
		if err := os.WriteFile(filepath.Join(dictDir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := materializeJiebaDict(dataDir)
	if err != nil {
		t.Fatalf("退回内嵌不应失败: %v", err)
	}
	if got == dictDir {
		t.Error("★ 缺文件的外置目录不得被采用")
	}
}

// ③ 空文件等同缺失（磁盘写坏的典型形态）。
func TestExternalJiebaDictComplete_RejectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range jiebaDictFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte{}, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if externalJiebaDictComplete(dir) {
		t.Error("★ 全零字节的词库不得被判为齐备")
	}
	// 填上内容后应变齐备
	for _, name := range jiebaDictFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if !externalJiebaDictComplete(dir) {
		t.Error("非空词库应判为齐备")
	}
}

// ④ SetJiebaDataDir 的读写不产生数据竞争（配合 -race）。
func TestSetJiebaDataDir_RaceFree(t *testing.T) {
	defer SetJiebaDataDir("")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			SetJiebaDataDir("/tmp/x")
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		_ = jiebaDataDirValue()
	}
	<-done
}

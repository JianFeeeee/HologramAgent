package tracker

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRollbackFromDiskIsIrreversible 把「agentfs 回滚**不可逆**」钉成事实。
//
// # 为什么这条重要
//
// cmd/homed 的 guard 在 failback 连续耗尽后会调用 rollbackAgentFS，
// 它走的正是 RollbackFromDisk。而该函数做两件事：
//
//  1. 对每个 changeset 逆应用（**删除/还原用户文件**）
//  2. **再把 changeset 记录本身与内容 blob 一并删掉**
//
// 第二步让整个操作**不可逆** —— 没有「反悔」的余地。
//
// 这不是缺陷，是设计（磁盘上的记录用掉了就没了）。
// 但它是用户数据级的破坏性操作，而触发路径是「LLM 连不上三轮」
// 这种完全可能发生的日常故障。所以必须有一条判据把它的行为钉死：
// 一旦将来有人改坏 RollbackFromDisk（比如漏删 blob、或漏了逆应用），
// 这里会立刻红。
//
// 现有 disk_test.go 只断言「回滚后内容正确」，没有断言「记录被清除」——
// 后者才是不可逆性的证据。
func TestRollbackFromDiskIsIrreversible(t *testing.T) {
	dataDir := t.TempDir()
	workDir := t.TempDir()

	trk := NewTracker(dataDir, workDir)
	if err := trk.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	os.MkdirAll(trk.upperDir, 0755)

	// 「修改」类变更：回滚后应恢复为原始内容
	modFile := filepath.Join(trk.upperDir, "user_note.txt")
	if err := os.WriteFile(modFile, []byte("改过的内容"), 0644); err != nil {
		t.Fatal(err)
	}
	trk.saveChangeSet(&ChangeSet{
		ID: "cs_mod",
		Files: []FileChange{{
			Path:    "user_note.txt",
			Type:    ChangeFileModified,
			Content: []byte("原始内容"),
		}},
	})

	// 前置条件：确实有 changeset 落盘
	if got := trk.ChangesetsOnDisk(); got == 0 {
		t.Fatal("前置条件失败：changeset 未落盘，回滚将无事可做")
	}

	n, err := trk.RollbackFromDisk()
	if err != nil {
		t.Fatalf("RollbackFromDisk: %v", err)
	}
	if n != 1 {
		t.Fatalf("回滚数量=%d, want 1", n)
	}

	// 断言一：用户文件被还原
	got, err := os.ReadFile(modFile)
	if err != nil {
		t.Fatalf("回滚后文件应存在: %v", err)
	}
	if string(got) != "原始内容" {
		t.Errorf("回滚后内容=%q, want 原始内容", got)
	}

	// 断言二（不可逆性的证据）：changeset 记录已被删除
	if left := trk.ChangesetsOnDisk(); left != 0 {
		var names []string
		if entries, err := os.ReadDir(filepath.Join(dataDir, "changesets")); err == nil {
			for _, e := range entries {
				names = append(names, e.Name())
			}
		}
		t.Errorf("回滚后磁盘上仍有 %d 个 changeset（%v）—— 记录本应被清除，"+
			"否则重复调用会把同一批变更再逆应用一次", left, names)
	}
}

// TestRollbackFromDiskTwiceIsNoOp 保证重复回滚是安全的空操作。
//
// 为何要这条：guard 的状态机在「failback 轮次耗尽」后会调
// rollbackAgentFS 然后 continue 到下一轮；若上一轮已经清过记录，
// 第二次调用必须是 no-op，否则会「回滚到一个更早的状态」——
// 而那个状态已经不存在了。
func TestRollbackFromDiskTwiceIsNoOp(t *testing.T) {
	dataDir := t.TempDir()
	workDir := t.TempDir()

	trk := NewTracker(dataDir, workDir)
	if err := trk.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	os.MkdirAll(trk.upperDir, 0755)

	modFile := filepath.Join(trk.upperDir, "f.txt")
	os.WriteFile(modFile, []byte("改过的"), 0644)
	trk.saveChangeSet(&ChangeSet{
		ID:    "cs1",
		Files: []FileChange{{Path: "f.txt", Type: ChangeFileModified, Content: []byte("原始")}},
	})

	if n, err := trk.RollbackFromDisk(); err != nil || n != 1 {
		t.Fatalf("首次回滚 n=%d err=%v", n, err)
	}

	// 第二次：必须是干净的 no-op
	n2, err2 := trk.RollbackFromDisk()
	if err2 != nil {
		t.Errorf("二次回滚报错: %v", err2)
	}
	if n2 != 0 {
		t.Errorf("二次回滚 n=%d，应为 0（记录已被清除，不能重复逆应用）", n2)
	}

	// 内容必须仍是「原始」，没有被二次破坏
	if got, _ := os.ReadFile(modFile); string(got) != "原始" {
		t.Errorf("二次回滚后内容=%q, want 原始", got)
	}
}

// TestRollbackRestoresDeletedFile 覆盖「新建 → 回滚 = 删除」这条最危险的情形。
//
// agent 写出的文件在回滚后被删掉，这是数据丢失最直接的形式。
func TestRollbackRestoresDeletedFile(t *testing.T) {
	dataDir := t.TempDir()
	workDir := t.TempDir()

	trk := NewTracker(dataDir, workDir)
	if err := trk.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	os.MkdirAll(trk.upperDir, 0755)

	// 一个「新建」类变更：回滚后该文件应消失
	os.WriteFile(filepath.Join(trk.upperDir, "generated.txt"), []byte("生成物"), 0644)
	trk.saveChangeSet(&ChangeSet{
		ID:    "cs_new",
		Files: []FileChange{{Path: "generated.txt", Type: ChangeFileCreated}},
	})

	if _, err := trk.RollbackFromDisk(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(trk.upperDir, "generated.txt")); err == nil {
		t.Error("回滚后新建的文件仍存在 —— 回滚没有撤销「新建」")
	}
}

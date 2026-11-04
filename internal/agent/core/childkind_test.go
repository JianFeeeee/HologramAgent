package core

// 判据：**子 agent 的类型分叉不能再长回来**（源码扫描，与
// TestAgentCoreHoldsNoPolicySemantics 同型）。
//
// ## 为什么要这条
//
// 本仓头号缺陷形态是「机制写对了、判据全绿、生产里长歪」。子 agent 的
// 分叉尤其容易回潮：每加一种类型，往 inputroute 加一段、
// 往 agent 加一张表、往 toolcall 加几个 case —— 每次都很"顺手"。
//
// 实测代价（2026-11-04）：新增一种子 agent 要改 18 个生产文件，
// 并已因复制而漂移过一次（StopRTChildren 漏注销入站登记，
// engineering-lessons.md §2.3）。
//
// 这条判据扫**源码文本**，禁止这些字段/调用在 core 里重新出现。
// 它不是风格洁癖：`a.rtChildren` 一旦回来，说明有人绕过统一登记表
// 又建了一张表 —— 那正是要消灭的形态。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenChildSplits 是**已并入统一登记表**、不得再出现的类型专属入口。
//
// 每一条都对应一次真实的迁移：
//
//	rtMu / rtChildren        → children child.Registry（R2）
//	residentMu / residents   → 同上（R3）
//
// 注意：`rtClosedTranscripts` **不在此列** —— 它是"已回收实例的留档"，
// 不属于登记表（实例已经不在了），有独立的 rtArchiveMu 保护。
var forbiddenChildSplits = []string{
	".rtChildren[",
	".rtMu.Lock()",
	".residentMu.Lock()",
	".residents[",
}

// TestNoChildTypeTablesInCore 钉住：core 里不得再有按类型的专用登记表。
//
// 变红条件：把任一类型改回自己的 map + 锁 ⇒ 红。
func TestNoChildTypeTablesInCore(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("没有扫到任何 .go 文件 —— 判据跑错了目录？")
	}
	var hits []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		src := string(data)
		for _, bad := range forbiddenChildSplits {
			if strings.Contains(src, bad) {
				// 允许出现在注释里（迁移说明需要提到旧字段名），
				// 所以逐行判：只有"非注释行含它"才算违规。
				for i, line := range strings.Split(src, "\n") {
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "//") {
						continue
					}
					if strings.Contains(line, bad) {
						hits = append(hits, f+":"+itoa(i+1)+"  "+trimmed)
					}
				}
			}
		}
	}
	if len(hits) > 0 {
		t.Fatalf("子 agent 的类型专属登记表又出现了（应走 children 统一登记表）：\n  %s",
			strings.Join(hits, "\n  "))
	}
}

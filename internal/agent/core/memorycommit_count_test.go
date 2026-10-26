package core

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// TestMemoryCommitReportsRealCounts 钉住「memory_commit 如实报告写入量」。
//
// ★ 缺陷来源（2026-10-05 隔离实例实测）：
//
//	一次对话实际写入 7 块 9 边，工具输出却是
//	  「已写入 0 个实体和 0 条关系」
//	  「提交了 1 条三元组但全部被拒（未写入）」
//
//	⇒ 判据用 newBlocks（对），文案用 ec/rc（错）——
//	  同一份代码里两套口径打架。
//
//	★ 后果比「数字难看」严重：模型收到「写不进去」就放弃。
//	  实测模型原话："the write is being rejected, so let me check
//	  whether the store itself is working" —— 它开始怀疑存储坏了，
//	  而其实一切正常。
//
// ★ 判定方式：真调一次工具，看回文案里有没有 0，
//
//	同时看库里块数是否真的涨了。两者必须一致。
func TestMemoryCommitReportsRealCounts(t *testing.T) {
	g, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })

	a := New(AgentConfig{
		ID:              types.AgentID("memcommit"),
		SystemPrompt:    "助手",
		Provider:        &scriptProvider{},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		Memory:          g,
		StageHost:       NewStageHost(),
	})

	before, err := g.MemoryBlockCount()
	if err != nil {
		t.Fatal(err)
	}

	out := a.executeMemoryTool(agentAPI.ToolCall{
		ID: "c1", Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{
					"subject":  "值班室门禁密码",
					"relation": "等于",
					"object":   "7788",
				},
			},
		},
	}, nil)

	after, err := g.MemoryBlockCount()
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("工具回: %q", out)
	t.Logf("块数 %d → %d（Δ%d）", before, after, after-before)

	if after == before {
		t.Fatalf("块数没涨（Δ0），无法验证计数文案；工具回 %q", out)
	}

	// ① 写入成功时，文案不能说「被拒」。
	if strings.Contains(out, "被拒") || strings.Contains(out, "未写入") {
		t.Errorf("实际写入了 %d 个块，工具却说没写入：%q —— 模型会据此放弃并怀疑存储坏了",
			after-before, out)
	}
	// ② 文案里的块数必须等于实际增量。
	if !strings.Contains(out, strconv.Itoa(after-before)) {
		t.Errorf("实际新增 %d 块，文案里应含该数字；实际 %q", after-before, out)
	}
	// ③ ★ 不能出现「0 个」—— 那正是旧表口径的症状。
	if strings.Contains(out, "0 个") {
		t.Errorf("文案含「0 个」：%q —— 又在用恒为 0 的 ec/rc", out)
	}
}

// TestMemoryCommitRejectsStillSaysRejected 钉住「真失败时仍然报失败」。
//
// ★ 与上一条成对：修「谎报成功」最常见的副作用是把失败也报成成功。
//
//	这条保证 0 写入时仍然明说「被拒」。
func TestMemoryCommitRejectsStillSaysRejected(t *testing.T) {
	g, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })

	a := New(AgentConfig{
		ID:              types.AgentID("memcommit2"),
		SystemPrompt:    "助手",
		Provider:        &scriptProvider{},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		Memory:          g,
		StageHost:       NewStageHost(),
	})

	// 空主语 ⇒ 实体名校验必然拒绝 ⇒ 真的什么都没写。
	out := a.executeMemoryTool(agentAPI.ToolCall{
		ID: "c2", Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{"subject": "", "relation": "等于", "object": "x"},
			},
		},
	}, nil)

	t.Logf("工具回: %q", out)

	n, err := g.MemoryBlockCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("前提不成立：空主语不该写入任何块，实际 %d", n)
	}
	// ★ 断言语义（「没写进去」）而不是字面（「被拒」）：
	//
	//	实测 0 写入有两条拦截路径 ——
	//	  ① 工具参数解析层：回「没有有效的三元组」
	//	  ② 实体名校验层：回「提交了 N 条三元组但全部被拒（未写入）」
	//	两条都是如实的失败报告，措辞不同而已。
	//	钉死字面会让 ① 路径下这条判据假红。
	if !strings.Contains(out, "rejected") &&
		!strings.Contains(out, "nothing written") &&
		!strings.Contains(out, "no valid triples") {
		t.Errorf("0 写入时必须明说没写进去（否则模型当成成功而不重试），实际 %q", out)
	}
	// ★ 尤其不能出现「已写入」这种成功措辞。
	if strings.Contains(out, "Wrote") || strings.Contains(out, "written") && !strings.Contains(out, "nothing written") {
		t.Errorf("0 写入却回「已写入」：%q", out)
	}
}

// itoa 免引入 strconv（判据里只用到一处）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

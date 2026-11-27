// Package colddoc 是**冷文档的只读派生函数**（叶子包，零内部依赖）。
//
// ## 为什么它必须是独立叶子包
//
// 基座（`agent/context.go`）要给冷文档估 token、按页取回内容、拼相关度查询，
// 而它需要三件事：
//
//	HasEvents(d)           有没有事件（有就按事件算，没有退回 Content）
//	InOut(ev)              从事件里还原「输入 / 输出 / 持有块」
//	EntryTextBlockID(...)  文本块的派生 ID + 角色常量
//
// 它们今天住在 `internal/memory/coldcontext`（一个带 SQLite 的实现包）。
// 也不能放进 `kernelapi/model`：那会把"块 ID 的哈希规则"塞进纯 DTO 包。
//
// ## 与实现侧的关系
//
// 规则**只在这里**（`coldcontext` 转调本包）。块 ID 的算法是**持久化契约**
// （存量归档文档的块 ID 由它派生），所以规则必须唯一 —— 两边各算一遍迟早漂移。
package colddoc

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// 文本块在一对事件里的角色。
const (
	RoleInput     = "input"
	RoleResponse  = "response"
	RoleReasoning = "reasoning"
)

// HasEvents 报告冷文档是否有事件（有 ⇒ 按事件算正文；无 ⇒ 退回 Content）。
func HasEvents(d *model.ColdDoc) bool { return d != nil && len(d.Events) > 0 }

// EntryTextBlockID 是「事件里某段文本」的**派生块 ID**。
//
// 入参是事件的**规范时间戳**（UnixNano）、来源、角色与正文 —— 同四项必同 ID。
func EntryTextBlockID(evtTS time.Time, source, role, text string) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(evtTS.UnixNano(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(source))
	h.Write([]byte{0})
	h.Write([]byte(role))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return "blk_txt_" + hex.EncodeToString(h.Sum(nil)[:12])
}

// InOut 从一条冷事件里还原「输入 / 输出 / 持有块」。
func InOut(ev *model.ColdEvent) (input, response string, held []model.MemoryBlock) {
	i, r, _, h := InOutWithReasoning(ev)
	return i, r, h
}

// InOutWithReasoning 与 InOut 相同，但额外还原思考内容。
//
// ★ 为什么单列一个函数而不是合并多返回值：它被 8 个调用点用，其中只有 2 处
//
//	需要思考内容。让另外 6 处跟着改，会把"哪些地方真的关心思考"这个信号
//	淹没在一堆样板修改里（实现侧的原注释，逐字保留这条判断）。
func InOutWithReasoning(ev *model.ColdEvent) (input, response, reasoning string, held []model.MemoryBlock) {
	if ev == nil {
		return "", "", "", nil
	}
	for i := range ev.Blocks {
		b := ev.Blocks[i]
		if b.Modality != model.BlockText {
			held = append(held, b)
			continue
		}
		switch {
		case input == "" && b.ID == EntryTextBlockID(ev.Timestamp, ev.Source, RoleInput, b.Text):
			input = b.Text
		case response == "" && b.ID == EntryTextBlockID(ev.Timestamp, ev.Source, RoleResponse, b.Text):
			response = b.Text
		case reasoning == "" && b.ID == EntryTextBlockID(ev.Timestamp, ev.Source, RoleReasoning, b.Text):
			reasoning = b.Text
		case b.PageKind == model.PageToolRound:
			// ★ 工具页的块**不是**事件的"持有块"，而是 ToolCalls/ToolResults
			//   的页级表示（P2 派生出来的）。若把它当持有块塞回去，
			//   回灌后事件会多出一堆无关块，且下次归档再派生一遍 ⇒ 越滚越多。
			//
			//   权威数据仍是 ev.ToolCalls / ev.ToolResults。
			continue
		default:
			// 不是本事件派生出的文本块（例如蒸馏来的原句块）：当作持有块。
			held = append(held, b)
		}
	}
	return input, response, reasoning, held
}

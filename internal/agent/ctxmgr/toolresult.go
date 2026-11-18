package ctxmgr

// 超长工具结果的「全文落盘 + 按范围回读」（纯逻辑部分）。
//
// ## 为什么需要它
//
// 工具结果进上下文时没有任何长度上限（task.go 直接 `Content: result`）。
// 实测生产一次 36783 token 的单条结果直接冲破整个对话预算，overflow 判定
// 「单条超限，裁剪救不了」→ 整轮终止。模型看到的是「对话没反应」。
//
// 之前有两条错的路子，这里都不走：
//
//   - **截断**（方案 A）：模型拿到残缺信息且**不知道**被截过，会基于残缺
//     数据下结论。与本仓反复吃亏的「静默降级」同族。
//   - **只统计不裁剪**（方案 B，toolresult_budget.go）：设计上是对的（不欺骗
//     模型），但停在「报告」这一步 —— oversizeTools 计数没有任何消费者，
//     所以那条超大结果照样整条进上下文，照样撞窗口。
//
// 本文件补的是方案 B 缺的那一半：结果**全文保存**，上下文里只放
// 「预览 + 句柄 + 总量导航」，模型用 toolresult_read 按 offset/limit
// 取任意区间。
//
//   - 模型不会被骗：它明确知道看到的是预览，并有手段取全文
//   - 取哪一段由**模型**决定，不是内核武断裁一刀
//   - 上下文预算可控，且**信息不丢**（判据要求分段拼回逐字节相同）
//
// ## 存活期：跟随事件，不引入 TTL
//
// 全文存在**事件自己的目录**里（与 Blocks 同一套处置，见 mediaref.go 的
// 「块存储在事件自身，不维护引用账本」）。所以：
//
//	事件在 L1  ⇒ 全文可读
//	事件被淘汰 ⇒ 全文随事件目录一并删除
//
// 刻意**不做**独立的过期时间/清理定时器：那会让「一件事记两处」，
// 且两处一旦不一致，就会出现「事件还在但全文没了」或反过来的静默错误。
//
// ★ 磁盘读写不在这里，而在 context.go（persistToolResult / readToolResultBody /
//   purgeToolResultDir）。原因：本包有架构守卫 TestKernelHasNoMediaFileReading，
//   内核除「记忆面持久化」（context.go）外不得做本地文件读。这本来就是一件
//   「记住一份东西」的事，归到持久化文件既满足守卫也符合职责。

import (
	"fmt"
	"strings"
	"time"
)

// 工具结果全文的存储目录名（与 context.json 同级）。
const toolResultDirName = "toolresults"

// toolResultStoreHardCap 是单条结果落盘的硬上限（1 GiB）。
//
// 为什么还要上限：不设的话一个 `cat /dev/urandom` 就能把磁盘写满。
// 但它**不是**静默截断 —— 超限部分记进 TruncatedBytes 并明确告诉模型。
const toolResultStoreHardCap = 1 << 30

// attachToolResultStore 把落盘记录挂到事件上（与 bindEventMedia 同构）。
func attachToolResultStore(evt *ContextEvent, st *ToolResultStore) {
	if evt == nil || st == nil {
		return
	}
	if evt.ID == "" {
		evt.ID = NewEventID()
	}
	evt.ToolResultStores = append(evt.ToolResultStores, *st)
}

// headLinesOf 截出前 n 行（按 rune 安全，不切断多字节字符）。
func HeadLinesOf(s string, n int) string {
	if n <= 0 {
		n = PreviewHeadLines
	}
	lines := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines++
			if lines >= n {
				return s[:i+1]
			}
		}
	}
	return s
}

// PreviewHeadLines 是预览默认保留的行数。
const PreviewHeadLines = 40

// findToolResultStore 按事件 ID 找到落盘记录（在 L1 事件里查）。
func (c *RelevanceContext) FindToolResultStore(evtID string) *ToolResultStore {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, evt := range c.events {
		if evt == nil || evt.ID != evtID {
			continue
		}
		if len(evt.ToolResultStores) == 0 {
			return nil
		}
		return &evt.ToolResultStores[0]
	}
	// 事件还没进 L1（同一帧内刚落盘、尚未 append）：查待登记表。
	//
	// ★ 不能只看 c.events：落盘发生在 toolMsg 定稿时，而事件 append 在更晚。
	//   少了这一支，模型拿到句柄立刻去读会得到"未找到"——判据
	//   TestReadBackReassemblesByteIdentical 抓到的正是这个。
	if sts, ok := c.pendingToolResults[evtID]; ok && len(sts) > 0 {
		return &sts[0]
	}
	return nil
}

// hasAnyToolResultStore 扫一遍 L1 事件，看有没有落盘记录。
func (c *RelevanceContext) HasAnyToolResultStore() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, evt := range c.events {
		if evt != nil && len(evt.ToolResultStores) > 0 {
			return true
		}
	}
	return false
}

// registerToolResultStore 把落盘记录登记到对应事件上。
//
// 若该事件还没进 L1（同一帧内多条超长结果时可能先于事件 append），
// 先挂在 context 的待登记表里，等事件进来后再认领——否则句柄会悬空。
func (c *RelevanceContext) RegisterToolResultStore(evtID string, st *ToolResultStore) {
	if c == nil || st == nil || evtID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, evt := range c.events {
		if evt != nil && evt.ID == evtID {
			evt.ToolResultStores = append(evt.ToolResultStores, *st)
			c.dirty = true
			return
		}
	}
	if c.pendingToolResults == nil {
		c.pendingToolResults = map[string][]ToolResultStore{}
	}
	c.pendingToolResults[evtID] = append(c.pendingToolResults[evtID], *st)
}

// newEventID 生成事件 ID（`evt_<纳秒>`）。
//
// ★ 与基座同名函数内联重复（两者不能互相 import）；判据 `TestEventIDsAgree`
//
//	只钉**形状**（前缀 + 单调递增），因为纳秒值本身不可比。
func NewEventID() string {
	return fmt.Sprintf("evt_%d", time.Now().UnixNano())
}

// FormatReadBack 渲染回读结果的头（工具名 / 区间 / 是否到末尾）。
func FormatReadBack(st *ToolResultStore, offset int, body string, atEnd bool) string {
	if st == nil {
		return body
	}
	suffix := ""
	if atEnd {
		suffix = "，已到末尾"
	}
	return fmt.Sprintf("[%s 的第 %d-%d 字节，共 %d 字节%s]\n%s",
		st.Tool, offset, offset+len(body), st.TotalBytes, suffix, body)
}

// PreviewToolResult 生成进上下文的预览：头部若干行 + 句柄 + 读法。
//
// ★ 文案三要素（缺一不可，判据钉住）：
//  1. 明确说明「这不是全文」以及总大小 —— 否则模型会当成完整内容下结论
//  2. 给出句柄（事件 ID）与工具名 —— 否则它没法取
//  3. 给出可用区间导航 —— 否则它不知道能读到哪
func PreviewToolResult(evtID string, st *ToolResultStore, head string, headLines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[工具 %s 返回内容过大，已完整保存，此处仅为预览]\n", st.Tool)
	fmt.Fprintf(&b, "全文：%d 字节 / %d 行；句柄 event_id=%s\n", st.TotalBytes, st.TotalLines, evtID)
	if st.TruncatedBytes > 0 {
		fmt.Fprintf(&b, "⚠️ 落盘时超出上限，有 %d 字节**未保存**（不是全文）\n", st.TruncatedBytes)
	}
	fmt.Fprintf(&b, "\n--- 预览（前 %d 行）---\n%s\n--- 预览结束 ---\n", headLines, head)
	fmt.Fprintf(&b, "\n如需其它部分，调用 toolresult_read(event_id=%q, offset=<字节偏移>, limit=<字节数>)。\n", evtID)
	return b.String()
}

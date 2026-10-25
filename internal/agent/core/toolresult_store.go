package core

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
	"log"
	"strings"
)

// 工具结果全文的存储目录名（与 context.json 同级）。
const toolResultDirName = "toolresults"

// ToolResultStore 是一条「超大工具结果」的落盘记录。
//
// 它**只存元信息**进 context.json；正文在
//
//	<context.json 所在目录>/toolresults/<eventID>/tool_<index>.txt
//
// 这样 context.json 不会因为几条大结果膨胀成几 MB（它每 5s 全量重写一次）。
type ToolResultStore struct {
	// Tool 是产出这条结果的工具名（模型据此判断"是哪个工具给的"）。
	Tool string `json:"tool"`
	// File 是正文相对事件目录的文件名（如 tool_0.txt），不含绝对路径——
	// 数据目录整体搬迁后仍然有效。
	File string `json:"file"`
	// TotalBytes / TotalLines 供模型规划怎么读（先读尾部？还是按行定位？）。
	TotalBytes int `json:"total_bytes"`
	TotalLines int `json:"total_lines"`
	// TruncatedBytes 是**没有**落盘的字节数。非 0 表示落盘时触到了硬上限，
	// 必须让模型知道（否则它以为全文就这么长）。默认 0。
	TruncatedBytes int `json:"truncated_bytes,omitempty"`
}

// toolResultStoreHardCap 是单条结果落盘的硬上限（1 GiB）。
//
// 为什么还要上限：不设的话一个 `cat /dev/urandom` 就能把磁盘写满。
// 但它**不是**静默截断 —— 超限部分记进 TruncatedBytes 并明确告诉模型。
const toolResultStoreHardCap = 1 << 30

// storeToolResult 把超长结果正文落盘，返回元信息。IO 委托给 context.go。
func (a *Agent) storeToolResult(evtID, tool, text string) (*ToolResultStore, error) {
	if a == nil || a.context == nil {
		return nil, fmt.Errorf("toolresult: agent 未就绪")
	}
	if a.context.savePath == "" {
		return nil, fmt.Errorf("toolresult: 未配置持久化目录，无法落盘")
	}
	return a.context.persistToolResult(evtID, tool, text)
}

// previewToolResult 生成进上下文的预览：头部若干行 + 句柄 + 读法。
//
// ★ 文案三要素（缺一不可，判据钉住）：
//  1. 明确说明「这不是全文」以及总大小 —— 否则模型会当成完整内容下结论
//  2. 给出句柄（事件 ID）与工具名 —— 否则它没法取
//  3. 给出可用区间导航 —— 否则它不知道能读到哪
func previewToolResult(evtID string, st *ToolResultStore, head string, headLines int) string {
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

// attachToolResultStore 把落盘记录挂到事件上（与 bindEventMedia 同构）。
func attachToolResultStore(evt *ContextEvent, st *ToolResultStore) {
	if evt == nil || st == nil {
		return
	}
	if evt.ID == "" {
		evt.ID = newEventID()
	}
	evt.ToolResultStores = append(evt.ToolResultStores, *st)
}

// headLinesOf 截出前 n 行（按 rune 安全，不切断多字节字符）。
func headLinesOf(s string, n int) string {
	if n <= 0 {
		n = previewHeadLines
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

// previewHeadLines 是预览默认保留的行数。
const previewHeadLines = 40

// offloadToolResult 把超长结果落盘，返回替换后的预览文本。
//
// 返回 ok=false 时调用方保持原内容不变（落盘失败不能吞掉结果——
// 宁可撞窗口也不能让模型看不到任何东西，那是更严重的静默降级）。
func (a *Agent) offloadToolResult(f *TaskFrame, tool, content string) (string, bool) {
	if a == nil || a.context == nil || content == "" {
		return "", false
	}
	evtID := ""
	if f != nil {
		evtID = f.EventID
	}
	if evtID == "" {
		evtID = newEventID()
		if f != nil {
			f.EventID = evtID
		}
	}
	st, err := a.storeToolResult(evtID, tool, content)
	if err != nil {
		// 落盘失败就保持原样：模型至少还能看到完整内容（哪怕撞窗口），
		// 这比"什么都没看到"好。日志留痕供运维发现磁盘/权限问题。
		log.Printf("[agent] toolresult offload failed (tool=%s): %v — 保持原内容", tool, err)
		return "", false
	}
	// ★ 登记到上下文里，否则 toolresult_read 找不到句柄。
	//
	// storeToolResult 只负责写文件；"这条结果属于哪个事件、能被回读"
	// 是上下文侧的记录。两者必须一起成立——只有文件没有登记，模型拿到
	// 句柄去读会得到"未找到"，那正是本判据抓到的缺口。
	a.context.registerToolResultStore(evtID, st)
	return previewToolResult(evtID, st, headLinesOf(content, previewHeadLines), previewHeadLines), true
}

// readStoredToolResult 实现 toolresult_read：按事件句柄 + 字节区间读回全文。
//
// 这是「工具环」设计里缺的那一半——超长结果不再整条进上下文，而是存起来
// 由模型自己决定读哪一段。取哪一段是**模型**的判断，不是内核的武断裁剪。
func (a *Agent) readStoredToolResult(args map[string]interface{}) (string, error) {
	evtID, _ := args["event_id"].(string)
	if evtID == "" {
		return "", fmt.Errorf("缺少 event_id（预览里给出的句柄）")
	}
	offset := 0
	if v, ok := args["offset"].(float64); ok {
		offset = int(v)
	}
	limit := 0
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	if offset < 0 {
		return "", fmt.Errorf("offset 不能为负")
	}
	// 单次读取上限：防止模型一次要 500MB 把上下文再撞一次。
	// 64KB ≈ 16K token，留出余量给其它内容。
	const maxReadBytes = 64 * 1024
	if limit <= 0 {
		limit = maxReadBytes
	}
	if limit > maxReadBytes {
		limit = maxReadBytes
	}

	if a.context == nil {
		return "", fmt.Errorf("上下文不可用")
	}
	st := a.context.findToolResultStore(evtID)
	if st == nil {
		// 三种可能：句柄打错、事件已被淘汰、或这条结果当时落盘失败。
		// 都给出**明确**答复，不静默返回空串（空串会被误当成"内容就是空"）。
		return "", fmt.Errorf("未找到 event_id=%s 的工具结果记录：可能句柄有误，或该事件已随上下文淘汰（全文跟随事件生命周期，不单独保留）", evtID)
	}
	body, atEnd, err := a.context.readToolResultBody(evtID, st, offset, limit)
	if err != nil {
		return "", err
	}
	header := fmt.Sprintf("[%s 的第 %d-%d 字节，共 %d 字节%s]",
		st.Tool, offset, offset+len(body), st.TotalBytes, map[bool]string{true: "，已到末尾", false: ""}[atEnd])
	return header + "\n" + body, nil
}

// findToolResultStore 按事件 ID 找到落盘记录（在 L1 事件里查）。
func (c *RelevanceContext) findToolResultStore(evtID string) *ToolResultStore {
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

// hasStoredToolResults 判断当前上下文里是否有可回读的超长工具结果。
//
// 供 buildToolDefs 决定要不要暴露 toolresult_read（见那里的说明）。
func (a *Agent) hasStoredToolResults() bool {
	if a == nil || a.context == nil {
		return false
	}
	return a.context.hasAnyToolResultStore()
}

// hasAnyToolResultStore 扫一遍 L1 事件，看有没有落盘记录。
func (c *RelevanceContext) hasAnyToolResultStore() bool {
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
func (c *RelevanceContext) registerToolResultStore(evtID string, st *ToolResultStore) {
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

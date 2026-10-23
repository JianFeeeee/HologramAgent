package proc

import (
	"encoding/json"
	"fmt"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// StageContext 的跨进程编解码（§3.3 数据面 / §3.4 SDK 封装全部复杂度）。
//
// 设计要点：
//
//  1. **插件作者永远不接触 Slice{off,len}**。插件进程内保留原生
//     *pubsdk.StageContext，handler 照常读写字段；stage 入口从共享段
//     反序列化成本地对象，handler 返回时把**脏字段**写回共享段。
//
//  2. **字段级描述符消除 lost update**。只改 FinalText 的插件不触碰
//     ToolResults 的描述符，故不存在"只读插件把旧快照写回覆盖他人改写"
//     （今日副本模型实测 35.8~36.8% 丢失，§8.4）。
//
//  3. **全部 16 个字段可见**。今日经 C ABI 只下发 10 个字段，外部插件永远
//     看不到 ContextMsgs/ReasoningContent/TokenUsage/Memory/Extra/Errors（§8.3）；
//     共享内存下全部可见可改——接口形式不变，能力变强。
//
//  4. **Extra 的 4 个键提升为具名字段**（§3.3 已核实全部使用点仅这 4 个）：
//     media_blocks / media_type / input_source / output_channel。
//     它们都是内核写、插件读，无并发改写需求；真正需要多插件并发改的
//     （LLMText/FinalText/ToolCalls/Errors）全是强类型字段。

// extra 中被提升为具名字段的键。
const (
	ExtraKeyMediaBlocks   = "media_blocks"
	ExtraKeyMediaType     = "media_type"
	ExtraKeyInputSource   = "input_source"
	ExtraKeyOutputChannel = "output_channel"
)

// WriteAll 把整个 StageContext 写入共享段（内核侧在 stage 开始前调用一次）。
// 调用方须持有写锁。
func (s *Segment) WriteAll(sc *pubsdk.StageContext) error {
	sc.RLock()
	snap := captureLocal(sc)
	sc.RUnlock()
	return s.writeLocal(snap)
}

// localCtx 是 StageContext 的值快照，用于在不持有 sc 锁的情况下做编解码。
type localCtx struct {
	RawMessage       string
	UserID           string
	GroupID          string
	LLMText          string
	ReasoningContent string
	FinalText        string
	Response         *string
	Phase            string
	NoMemory         bool
	ContextMsgs      []map[string]interface{}
	ToolCalls        []pubsdk.ToolCall
	ToolResults      []pubsdk.ToolResult
	Memory           []pubsdk.MemItem
	TokenUsage       map[string]int
	Errors           []string
	ExtraMediaBlocks interface{}
	ExtraMediaType   interface{}
	ExtraInputSource interface{}
	ExtraOutputChan  interface{}
	// ExtraBag 是 Extra 里具名 4 键之外的全部键（插件自建，如 qq_auth）。
	// nil 表示无额外键；非 nil 时整体 JSON 进 fExtraBag。
	ExtraBag map[string]interface{}
}

// namedExtraKeys 是被提升为具名字段的 4 个 Extra 键。bag 必须排除它们，
// 否则同一键会被具名字段与 bag 各带一份，回读时语义冲突。
var namedExtraKeys = map[string]bool{
	ExtraKeyMediaBlocks:   true,
	ExtraKeyMediaType:     true,
	ExtraKeyInputSource:   true,
	ExtraKeyOutputChannel: true,
}

// bagFromExtra 把 Extra 里非具名键收集成 bag（无额外键时返回 nil）。
func bagFromExtra(extra map[string]interface{}) map[string]interface{} {
	if len(extra) == 0 {
		return nil
	}
	var bag map[string]interface{}
	for k, v := range extra {
		if namedExtraKeys[k] {
			continue
		}
		if bag == nil {
			bag = make(map[string]interface{})
		}
		bag[k] = v
	}
	return bag
}

func captureLocal(sc *pubsdk.StageContext) *localCtx {
	l := &localCtx{
		RawMessage:       sc.RawMessage,
		UserID:           sc.UserID,
		GroupID:          sc.GroupID,
		LLMText:          sc.LLMText,
		ReasoningContent: sc.ReasoningContent,
		FinalText:        sc.FinalText,
		Response:         sc.Response,
		Phase:            string(sc.Phase),
		NoMemory:         sc.NoMemory,
		ContextMsgs:      sc.ContextMsgs,
		ToolCalls:        sc.ToolCalls,
		ToolResults:      sc.ToolResults,
		Memory:           sc.Memory,
		TokenUsage:       sc.TokenUsage,
		Errors:           sc.Errors,
	}
	if sc.Extra != nil {
		l.ExtraMediaBlocks = sc.Extra[ExtraKeyMediaBlocks]
		l.ExtraMediaType = sc.Extra[ExtraKeyMediaType]
		l.ExtraInputSource = sc.Extra[ExtraKeyInputSource]
		l.ExtraOutputChan = sc.Extra[ExtraKeyOutputChannel]
		l.ExtraBag = bagFromExtra(sc.Extra)
	}
	return l
}

func (s *Segment) writeLocal(l *localCtx) error {
	putStr := func(f stageField, v string) error {
		sl, err := s.write([]byte(v))
		if err != nil {
			return fmt.Errorf("写入字段 %s: %w", f, err)
		}
		s.setDesc(f, sl)
		return nil
	}
	putJSON := func(f stageField, v interface{}) error {
		if v == nil {
			s.setDesc(f, Slice{})
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("序列化字段 %s: %w", f, err)
		}
		sl, err := s.write(b)
		if err != nil {
			return fmt.Errorf("写入字段 %s: %w", f, err)
		}
		s.setDesc(f, sl)
		return nil
	}

	for _, step := range []struct {
		f stageField
		v string
	}{
		{fRawMessage, l.RawMessage},
		{fUserID, l.UserID},
		{fGroupID, l.GroupID},
		{fLLMText, l.LLMText},
		{fReasoningContent, l.ReasoningContent},
		{fFinalText, l.FinalText},
		{fPhase, l.Phase},
	} {
		if err := putStr(step.f, step.v); err != nil {
			return err
		}
	}

	// Response 是 *string：用标志位表达 nil，避免 "" 与 nil 混淆
	if l.Response != nil {
		if err := putStr(fResponse, *l.Response); err != nil {
			return err
		}
		s.setFlag(flagResponseSet, true)
	} else {
		s.setDesc(fResponse, Slice{})
		s.setFlag(flagResponseSet, false)
	}
	s.setFlag(flagNoMemory, l.NoMemory)

	// 切片/映射字段：nil 与空切片都写成"未设置"，避免插件收到 [] 后误以为
	// 内核显式清空过（与今日 writable 的 len>0 才下发语义一致）。
	jsonFields := []struct {
		f stageField
		v interface{}
	}{
		{fContextMsgs, sliceOrNil(len(l.ContextMsgs), l.ContextMsgs)},
		{fToolCalls, sliceOrNil(len(l.ToolCalls), l.ToolCalls)},
		{fToolResults, sliceOrNil(len(l.ToolResults), l.ToolResults)},
		{fMemory, sliceOrNil(len(l.Memory), l.Memory)},
		{fTokenUsage, sliceOrNil(len(l.TokenUsage), l.TokenUsage)},
		{fErrors, sliceOrNil(len(l.Errors), l.Errors)},
		{fExtraMediaBlocks, l.ExtraMediaBlocks},
		{fExtraMediaType, l.ExtraMediaType},
		{fExtraInputSource, l.ExtraInputSource},
		{fExtraOutputChannel, l.ExtraOutputChan},
		// bag 空时写成未设置（sliceOrNil）；非空时整体 marshal。
		// 不可序列化的值在这里让 putJSON 显式报错（宁失败不静默丢键）。
		{fExtraBag, sliceOrNil(len(l.ExtraBag), l.ExtraBag)},
	}
	for _, step := range jsonFields {
		if err := putJSON(step.f, step.v); err != nil {
			return err
		}
	}
	s.bumpSeq()
	return nil
}

// sliceOrNil 让长度为 0 的容器写成 nil（未设置），非零则原样返回。
func sliceOrNil(n int, v interface{}) interface{} {
	if n == 0 {
		return nil
	}
	return v
}

// ReadInto 从共享段读出全部字段填充到 sc（插件进程侧 stage 入口调用）。
// 调用方须持有读锁或写锁。
func (s *Segment) ReadInto(sc *pubsdk.StageContext) error {
	getStr := func(f stageField) (string, error) {
		b, err := s.read(s.getDesc(f))
		if err != nil {
			return "", fmt.Errorf("读取字段 %s: %w", f, err)
		}
		return string(b), nil
	}
	getJSON := func(f stageField, out interface{}) error {
		b, err := s.read(s.getDesc(f))
		if err != nil {
			return fmt.Errorf("读取字段 %s: %w", f, err)
		}
		if len(b) == 0 {
			return nil
		}
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("反序列化字段 %s: %w", f, err)
		}
		return nil
	}

	raw, err := getStr(fRawMessage)
	if err != nil {
		return err
	}
	uid, err := getStr(fUserID)
	if err != nil {
		return err
	}
	gid, err := getStr(fGroupID)
	if err != nil {
		return err
	}
	llm, err := getStr(fLLMText)
	if err != nil {
		return err
	}
	reason, err := getStr(fReasoningContent)
	if err != nil {
		return err
	}
	final, err := getStr(fFinalText)
	if err != nil {
		return err
	}
	phase, err := getStr(fPhase)
	if err != nil {
		return err
	}

	var ctxMsgs []map[string]interface{}
	var toolCalls []pubsdk.ToolCall
	var toolResults []pubsdk.ToolResult
	var mem []pubsdk.MemItem
	var usage map[string]int
	var errs []string
	if err := getJSON(fContextMsgs, &ctxMsgs); err != nil {
		return err
	}
	if err := getJSON(fToolCalls, &toolCalls); err != nil {
		return err
	}
	if err := getJSON(fToolResults, &toolResults); err != nil {
		return err
	}
	if err := getJSON(fMemory, &mem); err != nil {
		return err
	}
	if err := getJSON(fTokenUsage, &usage); err != nil {
		return err
	}
	if err := getJSON(fErrors, &errs); err != nil {
		return err
	}

	extra := map[string]interface{}{}
	// bag 先打平，具名键随后覆盖 → 具名键永远赢。
	// 顺序是防御：即便 bag 里混入具名键（写侧 bug/旧数据），
	// 也不会用 bag 的陈旧值顶掉具名字段的新值。
	var bag map[string]interface{}
	if err := getJSON(fExtraBag, &bag); err != nil {
		return err
	}
	for k, v := range bag {
		extra[k] = v
	}
	for _, pair := range []struct {
		f   stageField
		key string
	}{
		{fExtraMediaBlocks, ExtraKeyMediaBlocks},
		{fExtraMediaType, ExtraKeyMediaType},
		{fExtraInputSource, ExtraKeyInputSource},
		{fExtraOutputChannel, ExtraKeyOutputChannel},
	} {
		var v interface{}
		if err := getJSON(pair.f, &v); err != nil {
			return err
		}
		if v != nil {
			extra[pair.key] = v
		}
	}

	var respPtr *string
	if s.getFlag(flagResponseSet) {
		r, err := getStr(fResponse)
		if err != nil {
			return err
		}
		respPtr = &r
	}

	sc.Lock()
	defer sc.Unlock()
	sc.RawMessage = raw
	sc.UserID = uid
	sc.GroupID = gid
	sc.LLMText = llm
	sc.ReasoningContent = reason
	sc.FinalText = final
	sc.Phase = pubsdk.Stage(phase)
	sc.NoMemory = s.getFlag(flagNoMemory)
	sc.Response = respPtr
	sc.ContextMsgs = ctxMsgs
	sc.ToolCalls = toolCalls
	sc.ToolResults = toolResults
	sc.Memory = mem
	sc.TokenUsage = usage
	sc.Errors = errs
	if len(extra) > 0 {
		sc.Extra = extra
	} else {
		sc.Extra = nil
	}
	return nil
}

// WriteDirty 只把与 base 快照不同的字段写回共享段（插件进程侧 handler 返回后调用）。
//
// **这是消除 lost update 的关键**：只读插件的 dirty 集为空 → 零写入 →
// 不可能覆盖其他插件的改写。对比今日副本模型无条件回传 10 个字段的行为
// （§8.4 实测 35.8~36.8% 丢失，现网量级百分之几的脏数据进 LLM）。
//
// 返回实际写回的字段数，便于诊断与测试断言。
func (s *Segment) WriteDirty(sc *pubsdk.StageContext, base *Snapshot) (int, error) {
	sc.RLock()
	cur := captureLocal(sc)
	sc.RUnlock()

	now := newSnapshotFromLocal(cur)
	changed := 0

	putStr := func(f stageField, v string) error {
		sl, err := s.write([]byte(v))
		if err != nil {
			return fmt.Errorf("写回字段 %s: %w", f, err)
		}
		s.setDesc(f, sl)
		changed++
		return nil
	}
	putRaw := func(f stageField, raw string) error {
		if raw == "" {
			s.setDesc(f, Slice{})
			changed++
			return nil
		}
		sl, err := s.write([]byte(raw))
		if err != nil {
			return fmt.Errorf("写回字段 %s: %w", f, err)
		}
		s.setDesc(f, sl)
		changed++
		return nil
	}

	for _, step := range []struct {
		f stageField
		v string
	}{
		{fRawMessage, cur.RawMessage},
		{fUserID, cur.UserID},
		{fGroupID, cur.GroupID},
		{fLLMText, cur.LLMText},
		{fReasoningContent, cur.ReasoningContent},
		{fFinalText, cur.FinalText},
		{fPhase, cur.Phase},
	} {
		if base.strs[step.f] != now.strs[step.f] {
			if err := putStr(step.f, step.v); err != nil {
				return changed, err
			}
		}
	}

	// JSON 字段：比较序列化结果
	for f := range now.jsons {
		if base.jsons[f] != now.jsons[f] {
			if err := putRaw(f, now.jsons[f]); err != nil {
				return changed, err
			}
		}
	}

	// Response 的 nil 语义变化也算脏
	if base.responseSet != now.responseSet || base.response != now.response {
		if now.responseSet {
			if err := putStr(fResponse, now.response); err != nil {
				return changed, err
			}
			s.setFlag(flagResponseSet, true)
		} else {
			// 插件把 Response 置回 nil：短路语义不应被撑销，故不清空内核已设的值。
			// 与 C ABI 路径 applyStageResult 的行为保持一致。
			s.setFlag(flagResponseSet, s.getFlag(flagResponseSet))
		}
	}
	if base.noMemory != now.noMemory {
		s.setFlag(flagNoMemory, now.noMemory)
		changed++
	}

	if changed > 0 {
		s.bumpSeq()
	}
	return changed, nil
}

// Snapshot 是 handler 运行前的字段快照，用于计算脏字段。
//
// ❗ 必须存**序列化后的字符串**而非 Go 值：StageContext 的切片字段与
// 调用方共享底层数组，handler 原地改元素（sc.ToolResults[0].Result = x）
// 时直接持有的 Go 值快照会跟着变，脏字段计算失效——这个坑在 C ABI 侧
// 修 11.3 时已经踩过一次（见 SDK 仓 stagediff_test.go 的注释）。
type Snapshot struct {
	strs        map[stageField]string
	jsons       map[stageField]string
	response    string
	responseSet bool
	noMemory    bool
}

// Snapshot 抓取当前 StageContext 的快照（插件进程侧 handler 前调用）。
func TakeSnapshot(sc *pubsdk.StageContext) *Snapshot {
	sc.RLock()
	l := captureLocal(sc)
	sc.RUnlock()
	return newSnapshotFromLocal(l)
}

func newSnapshotFromLocal(l *localCtx) *Snapshot {
	sn := &Snapshot{
		strs:  map[stageField]string{},
		jsons: map[stageField]string{},
	}
	sn.strs[fRawMessage] = l.RawMessage
	sn.strs[fUserID] = l.UserID
	sn.strs[fGroupID] = l.GroupID
	sn.strs[fLLMText] = l.LLMText
	sn.strs[fReasoningContent] = l.ReasoningContent
	sn.strs[fFinalText] = l.FinalText
	sn.strs[fPhase] = l.Phase

	marshal := func(v interface{}) string {
		if v == nil {
			return ""
		}
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
	sn.jsons[fContextMsgs] = marshal(sliceOrNil(len(l.ContextMsgs), l.ContextMsgs))
	sn.jsons[fToolCalls] = marshal(sliceOrNil(len(l.ToolCalls), l.ToolCalls))
	sn.jsons[fToolResults] = marshal(sliceOrNil(len(l.ToolResults), l.ToolResults))
	sn.jsons[fMemory] = marshal(sliceOrNil(len(l.Memory), l.Memory))
	sn.jsons[fTokenUsage] = marshal(sliceOrNil(len(l.TokenUsage), l.TokenUsage))
	sn.jsons[fErrors] = marshal(sliceOrNil(len(l.Errors), l.Errors))
	sn.jsons[fExtraMediaBlocks] = marshal(l.ExtraMediaBlocks)
	sn.jsons[fExtraMediaType] = marshal(l.ExtraMediaType)
	sn.jsons[fExtraInputSource] = marshal(l.ExtraInputSource)
	sn.jsons[fExtraOutputChannel] = marshal(l.ExtraOutputChan)
	sn.jsons[fExtraBag] = marshal(sliceOrNil(len(l.ExtraBag), l.ExtraBag))

	if l.Response != nil {
		sn.response = *l.Response
		sn.responseSet = true
	}
	sn.noMemory = l.NoMemory
	return sn
}

// String 让字段枚举在错误信息里可读。
func (f stageField) String() string {
	switch f {
	case fRawMessage:
		return "raw_message"
	case fUserID:
		return "user_id"
	case fGroupID:
		return "group_id"
	case fLLMText:
		return "llm_text"
	case fReasoningContent:
		return "reasoning_content"
	case fFinalText:
		return "final_text"
	case fResponse:
		return "response"
	case fPhase:
		return "phase"
	case fContextMsgs:
		return "context_msgs"
	case fToolCalls:
		return "tool_calls"
	case fToolResults:
		return "tool_results"
	case fMemory:
		return "memory"
	case fTokenUsage:
		return "token_usage"
	case fErrors:
		return "errors"
	case fExtraMediaBlocks:
		return "extra." + ExtraKeyMediaBlocks
	case fExtraMediaType:
		return "extra." + ExtraKeyMediaType
	case fExtraInputSource:
		return "extra." + ExtraKeyInputSource
	case fExtraOutputChannel:
		return "extra." + ExtraKeyOutputChannel
	case fExtraBag:
		return "extra.bag"
	}
	return fmt.Sprintf("field(%d)", int(f))
}

package agent

// dep_parser.go —— 依存句法分析器的**注入面**（契约在 kernelapi，实现在 nlp）。
//
// ## 为什么经注入而不是 import nlp
//
// 状态面要报"依存句法分析器启用了吗、模型路径是什么"（`ONNXStatus.DepParser`）。
// 而 `internal/nlp` 是带 ONNX 的实现包 —— 基座为一句状态查询 import 它，
// 就是判据 §8 第 3 条要消灭的形态。
//
// 形态与 `Extractor` 相同：**契约在叶子、实现在 nlp、接线在装配点**。

// DepParserInfo 是分析器的**只读事实**（状态面用）。
//
// ★ 用两个标量而不是一个接口：状态面只需要"启用了吗、模型在哪"，
//
//	它不需要认识 Parser 这个类型（那会把基座焊在 nlp 的接口上）。
type DepParserInfo struct {
	Enabled   bool
	ModelPath string
}

// depParser 返回分析器的事实（未注入 ⇒ nil = 未启用）。
// DepParser 返回注入的依存句法分析器信息（类型状态面用）。
func (a *Agent) DepParser() *DepParserInfo {
	if a == nil || a.depParserFace() == nil {
		return nil
	}
	enabled, modelPath := a.depParserFace()()
	if !enabled {
		return nil
	}
	return &DepParserInfo{Enabled: true, ModelPath: modelPath}
}

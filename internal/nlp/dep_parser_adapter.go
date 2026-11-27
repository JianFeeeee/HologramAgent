package nlp

// dep_parser_adapter.go —— 把包级分析器适配成基座的状态查询面。
//
// 契约是 `agent.DepParserInfo`（由装配点传入闭包，见 kernelmod/agent）。
// 本函数在**实现侧**，所以基座不必 import nlp（判据 §8 第 3 条）。

// DepParserInfo 返回分析器的只读事实（nil = 未装配）。
//
// ★ 用一个返回函数的函数而不是直接返回结构：包级单例可能**在装配点之后**
//
//	才被装上（`SetDefaultParser`），闭包保证读的是**当下**的事实而不是
//	装配那一刻的快照。
func DepParserInfo() func() (enabled bool, modelPath string) {
	return func() (bool, string) {
		p := GetDefaultParser()
		if p == nil {
			return false, ""
		}
		if mp, ok := p.(interface{ ModelPath() string }); ok {
			return true, mp.ModelPath()
		}
		return true, ""
	}
}

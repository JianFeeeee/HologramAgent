package agent

// kernelapi_adapters.go —— 把 agent 依赖的实现面适配成 `kernelapi` 的接口。
//
// ## 为什么需要适配（用户裁定 2026-11-17）
//
// > 「不是说了挂载 kstate 里暴露接口」
//
// 对：设施由各模块 `ks.Provide(名, 实现)` 挂上挂载点，消费方经接口取用。
//
// 而 agent 内部有**两处形态差异**需要一次薄转发：
//
//	① `vector.Vector` 是 `map[string]float64` 的**命名类型**，
//	   接口包与本包都不 import vector 实现包 ⇒ 用 Map 形态的方法名。
//	② 七公共面第 2 项要求"可选记忆接口"只有 Recall/RecallSorted/Commit，
//	   而完整图库还多 28 个整理方法 ⇒ 需要一个窄适配（零逻辑转发）。
//
// 适配器只做转发，不含任何逻辑；多出来的实现方法 Agent 本来也不该用
// （用了就是把实现的内部形状当契约）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	"github.com/JianFeeeee/HologramAgent/internal/vector"
	"reflect"
)

// ── 图记忆：完整面（kernelapi） → 七公共面的窄面 ──

type graphFace struct{ g kernelapi.GraphMemory }

func (f graphFace) Recall(keywords, seedEntities []string, depth int, sessionFilter string) (*model.RecallResult, error) {
	return f.g.Recall(keywords, seedEntities, depth, sessionFilter)
}

func (f graphFace) RecallSorted(keywords, seedEntities []string, depth int, sessionFilter, fingerprint string, mode model.SortMode) (*model.RecallResult, error) {
	return f.g.RecallSorted(keywords, seedEntities, depth, sessionFilter, fingerprint, mode)
}

func (f graphFace) Commit(triples []model.Triple, sessionID string, turnID int) (int, int, error) {
	return f.g.Commit(triples, sessionID, turnID)
}

// ── 词嵌入：Embedder（Map 形态） → nlp.Vectorizer（vector.Vector 形态） ──

type vectorizerFace struct{ e Embedder }

func (v vectorizerFace) Vectorize(text string) vector.Vector {
	if v.e == nil {
		return nil
	}
	return vector.Vector(v.e.VectorizeMap(text))
}

// normalizeNilServices 把"装着 nil 具体指针的接口字段"折成真 nil。
//
// ## 为什么必须有它（不是洁癖）
//
// Go 的接口值 = (类型, 数据指针)。把一个 nil 的 `*plugin.Registry` 赋给
// `kernelapi.PluginHost` 之后，接口值**不是 nil**（类型信息非空）。
// 于是 `if a.MainState().pluginReg != nil` 会放行，随后 `a.MainState().pluginReg.List()` 解引用
// 空指针 —— 实测 `TestGetKernelStatusNeverNil` 就是这样 panic 的。
//
// 装配期（`kernelmod/*`）传入的字段大多允许"这一项没装配"，所以这种
// typed-nil 必须在这里一次性归一，而不是让 20 个调用点各自判断。
func (a *Agent) NormalizeNilServices() {
	if a == nil {
		return
	}
	if a.MainState().indexer != nil && isNilValue(a.MainState().indexer) {
		a.MainState().indexer = nil
	}
	if a.MainState().coldcontextStore != nil && isNilValue(a.MainState().coldcontextStore) {
		a.MainState().coldcontextStore = nil
	}
	if a.MainState().social != nil && isNilValue(a.MainState().social) {
		a.MainState().social = nil
	}
	if a.MainState().textMem != nil && isNilValue(a.MainState().textMem) {
		a.MainState().textMem = nil
	}
	if a.MainState().tracker != nil && isNilValue(a.MainState().tracker) {
		a.MainState().tracker = nil
	}
	if a.MainState().pluginReg != nil && isNilValue(a.MainState().pluginReg) {
		a.MainState().pluginReg = nil
	}
	if a.MainState().multimodalSpace != nil && isNilValue(a.MainState().multimodalSpace) {
		a.MainState().multimodalSpace = nil
	}
	if a.MainState().embedder != nil && isNilValue(a.MainState().embedder) {
		a.MainState().embedder = nil
	}
	if a.graph != nil && isNilValue(a.graph) {
		a.graph = nil
	}
	if a.cfg.Security != nil && isNilValue(a.cfg.Security) {
		a.cfg.Security = nil
	}
}

// isNilValue 报告接口里装的是不是 nil 指针（用反射，唯一可靠的办法）。
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

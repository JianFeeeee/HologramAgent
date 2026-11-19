package agent

// substates.go —— **类型状态分区表**（基座只存 `any`，不认识形状）。
//
// §6.2 #3 #4：`rtState` / `residentState` / `workerState` / `mainState` 全是
// **类型状态**，形状由类型包自己定义。基座只提供格子。
//
// ## 为什么五格固定、不用 map
//
// map 的键是运行期字符串：`Sub("wrok")` 静默返回 nil，故障在很远的地方
// 才显现。五个固定字段 + 常量键 ⇒ 拼错是**编译错误**。
//
// ## 为什么**不加锁**
//
// 分区表是**装配期写入、运行期只读**（类型在构造/Initialize 里 SetSub 一次，
// 之后各处 GetSub 只读）。与 `Agent` 其它字段同一纪律（见 agent.go 的说明：
// 基座按设计没有保护整轮的互斥量）。给无并发证据的读加锁只是噪音。
//
// ## 新增类型要动这里吗
//
// 要 —— **加一行字段**。这是刻意的：一行代价换来编译期检查，
// 比"预留一个无类型 map 让新类型随便塞"更可靠（后者会把拼错变成运行时 nil）。

type subStates struct {
	rt   any // *rtchild.M
	arch any // *rtchild.Archive
	res  any // *resident.M
	wrk  any // *worker.M
	mn   any // *mainagent.Module
}

// 分区键（存取点用）。
const (
	SubKeyRT       = "rt"
	SubKeyArchive  = "archive"
	SubKeyResident = "resident"
	SubKeyWorker   = "worker"
	SubKeyMain     = "main"
)

// Sub 取某个类型的状态格（键见 SubKey*）。
func (a *Agent) Sub(key string) any {
	if a == nil {
		return nil
	}
	s := a.ensureSub()
	if s == nil {
		return nil
	}
	switch key {
	case SubKeyRT:
		return s.rt
	case SubKeyArchive:
		return s.arch
	case SubKeyResident:
		return s.res
	case SubKeyWorker:
		return s.wrk
	case SubKeyMain:
		return s.mn
	}
	return nil
}

// SetSub 存某个类型的状态格（由类型在构造/Initialize 时调用一次）。
func (a *Agent) SetSub(key string, v any) {
	if a == nil {
		return
	}
	s := a.ensureSub()
	if s == nil {
		return
	}
	switch key {
	case SubKeyRT:
		s.rt = v
	case SubKeyArchive:
		s.arch = v
	case SubKeyResident:
		s.res = v
	case SubKeyWorker:
		s.wrk = v
	case SubKeyMain:
		s.mn = v
	}
}

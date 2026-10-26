package sdk

// KernelControlAPI：插件请求**内核本身**重启或关机。
//
// ## 为什么需要它
//
// 有些状态变更只能靠重启内核生效（典型：改了 LLM 源、换了主密钥、
// 升级了要重新装配的配置）。没有这条通道时，用户只能自己去 ssh 上敲
// systemctl —— 而 WebUI 是大多数人唯一会用的界面。
//
// ## 语义边界（重要，别指望它做更多）
//
// 这两个方法都是**异步**的：它们只把请求投递给内核，立刻返回。
// 调用方（WebUI）应当先给用户一个明确回应（"正在重启，页面稍后自动重连"），
// 而不是等待返回 —— 内核真的会在这几秒内消失。
//
// 进程会怎么走：
//
//	RequestKernelRestart ⇒ 内核清理（停插件、flush、CloseAll）→ exit 42
//	                       → guard 用**当前二进制**重拉，不经过 systemd、
//	                         不计失败轮次（这就是"热更新"那条路）
//	RequestKernelShutdown ⇒ 内核清理 → 通知 guard 主动停机 → systemd 不再拉起
//
// ★ 关机为什么不能用"guard 退出"实现：systemd 是 Restart=always，
//
//	guard 退出会被它无条件重新拉起。必须显式 `systemctl stop`）
//
// ## 失败语义
//
// 返回错误只表示**请求没被接受**（例如内核未装配控制通道、或该功能被禁用），
// 不表示"没重启成功"——成功投递后本进程就会退出，返回值本身没有后续。
type KernelControlAPI interface {
	// RequestKernelRestart 请求重启内核（热重启，guard 重拉）。
	RequestKernelRestart(reason string) error
	// RequestKernelShutdown 请求关闭内核（systemd 不再拉起）。
	RequestKernelShutdown(reason string) error
}

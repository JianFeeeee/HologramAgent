package sdk

// SecretsAPI 是插件访问**内核变量库**的接口。
//
// ## 为什么要有它（去重）
//
// 在此之前，commondev 插件自带了一整套加密存储（`secrets.json` +
// AES-256-GCM + 别名表 + `${ENV}` 解析），与内核的 `internal/security`
// **完全重复**：
//
//	             内核 security.Store        插件 unifiedSecretFile
//	  落盘        secrets.json               secrets.json
//	  加密        AES-256-GCM                AES-256-GCM
//	  别名        HMAC 自动派生              用户指定
//	  引用语法     @{sec_xxx}                 @{name}
//
// 重复的代价不只是代码量：**两套变量库互不可见** —— 内核拦截时登记的
// 变量，插件看不到；插件登记的，内核拦截不认识。结果是同一个凭据可能
// 在两处各存一份，而使用者不知道哪个才是权威。
//
// ## ★ 安全设计：明文的边界
//
// 内核的硬约束 S1 是「不提供取回明文的接口」—— 理由是一旦 LLM 能取回，
// 它就能把明文写回上下文，整层防护作废。
//
// 那为什么插件可以拿到明文？因为**边界不在插件侧**：
//
//	① 内核在**插件 handler 之前**就把工具参数里的 @{别名} 填好了
//	   （`StageHost.ExecuteTool` → `security.FillMap`）—— 插件本来
//	   就能从 args 里读到明文，这是设计使然（否则插件无法用凭据）。
//	② 明文若被插件**返回**给内核，会被两处拦回别名：
//	   · `ContextEvent` 的 scrub（记忆/历史侧路）
//	   · provider 装饰器（出向，LLM 永不看到）
//
// 两条都有判据钉住（`leakback_test.go`）。所以：
//
//	**暴露 Fill 不新增泄漏面** —— 它不是「开了一个口子」，
//	而是把「本来就存在的能力」从各插件自己实现改为统一接口。
//
// ⚠️ 但绝不能暴露 `Reveal(alias) -> 明文` 这样的**按名字直取**接口：
// 那会让插件（以及通过插件操纵它的 LLM）枚举别名并逐个取值。
// 本接口刻意**没有**这样的方法 —— 见下面 Fill 与 List 的签名。
//
// ## 与 Encrypt/Decrypt 的区别
//
// 插件常把凭据存进自己的配置文件（如 pi 的 `auth.json`）。那些文件权限
// 0600 但**明文落盘**。`Encrypt`/`Decrypt` 提供 at-rest 加密，
// 密钥由内核统一管理，插件不必自持密钥（自持会让密钥散落、无法轮换）。
type SecretsAPI interface {
	// Store 把一个凭据登记进内核变量库，返回其**别名**。
	//
	// 之后在工具参数里写 @{别名}，内核会在调用插件前换回真实值。
	//
	// ★ 只返回别名，不回显明文 —— 避免插件（或它转述给 LLM 的内容）
	// 顺带把明文带出去。
	//
	// note 是给人看的用途说明（如「Gitea 推送令牌」）。
	Store(value, note string) (alias string, err error)

	// List 列出变量库的元信息（别名/来源/备注/命中次数）—— **不含明文**。
	//
	// ★ 刻意返回元信息而非值：调用方拿不到明文，只能引用。
	List() []SecretInfo

	// Forget 按别名删除一个变量。
	//
	// ★ 变量库**不自动淘汰**（用户明确）：泄漏控制靠主动删除，不靠超时。
	// 所以凭据用完后应主动删。
	Forget(alias string) error

	// Fill 把一段文本里的 @{别名} 换成明文。
	//
	// 用途：插件需要把明文喂给**不经过内核参数回填**的地方 ——
	// 例如 commondev 的 enc_shell 要把命令经 **stdin** 送给 bash
	//（不是 argv，避免本机其他进程从 /proc/<pid>/cmdline 读到明文）。
	//
	// ★ 为什么这个方法是安全的（而不是"开了个取回明文的口子"）：
	// 插件**本来就**能从 `args` 里拿到内核填好的明文 ——
	// 内核在 handler 之前就填了。Fill 只是让插件能对**自己构造的文本**
	// 做同样的事（如读文件内容、拼一段脚本）。
	//
	// 它**不是**按名字直取：要填谁必须先在文本里写出那个别名，
	// 而别名本身不泄漏明文（HMAC 派生）。想枚举就得先 List，
	// 而 List 只给元信息 —— 拿到别名后仍只能用，不能读回。
	//
	// 返回替换处数与用到的别名（便于插件做输出脱敏时的比对）。
	Fill(text string) (out string, replaced int, aliases []string)

	// Encrypt 加密一段字节用于插件自己落盘（密钥由内核管理）。
	// 返回的串带版本前缀，可直接存文件。
	Encrypt(plain []byte) (string, error)

	// Decrypt 解密由 Encrypt 产生的串。
	Decrypt(stored string) ([]byte, error)

	// Available 报告变量库是否可用（内核未启用安全模块时为 false）。
	//
	// ★ 插件应据此**响亮失败**而不是静默降级 —— 例如
	// 「密钥不可用 ⇒ 拒绝明文存储」。
	Available() bool
}

// SecretInfo 是一条变量的元信息（**不含明文**）。
type SecretInfo struct {
	Alias     string `json:"alias"`
	Source    string `json:"source"`               // intercepted | submitted
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Hits      int    `json:"hits,omitempty"`
}

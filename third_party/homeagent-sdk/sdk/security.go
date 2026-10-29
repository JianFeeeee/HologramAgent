package sdk

// SecurityAPI 是插件**声明**自身安全策略的接口。
//
// ## 为什么需要它（用户裁定 2026-10-26，2026-10-29 补齐实现）
//
// > 插件可以声明自身安全限制策略，并在插件自身 inputch 触发时携带参数
// > 选择其中一种 —— 正好对应 QQ 插件内部 owner 与非 owner 的差别对待。
//
// > 这是可选参数，不声明安全策略则使用内核默认安全策略。
//
// ## 分工：插件只声明，内核只执行
//
//	插件：DeclareProfile / DeclareProfiles            （只声明，不判定）
//	内核：CheckTool / CheckToolFor / ActivateProfile  （只执行，不信任插件）
//
// ★ 为什么判定必须在内核：插件崩溃或被重载时，声明不能跟着消失
// （否则"挂掉插件"就是绕过策略的手段）；而且判定要与全局基线、主体策略
// 走同一条链 —— 分散到各插件就是"被约束者自己决定约束"，
// 本仓已有实测教训（QQ 的 beforeToolcall 自己判身份/白名单/硬私有，
// 结果每个插件各写一套、拒绝语义散落、插件崩了策略就没了）。
//
// ## 声明 vs 全局基线：两层，职责不重叠
//
//	① 全局基线（部署方配置 core.security.*）：不受插件影响，防"插件自己放水"
//	② 档位（本接口）：单次请求的**收窄**
//
// 档位**只能收窄**：内核的判定顺序是「全局 deny → 档位 deny → 主体 deny
// → 全局 allow → 档位 allow」，全局黑名单永远先判，档位绕不过去。
//
// ## 缺省语义（重要）
//
//	不声明 / 不传 = 不使用档位，一切判定沿用内核默认策略
//
// 也就是说这是**可选**能力：不用的插件行为与之前完全一致。
// 而一旦传了**未声明**的档位名，内核会**报错**而不是静默回落 ——
// 拼错档位名却默默用默认策略是安全事故的经典形态。
//
// ## 典型用法（QQ 插件）
//
//	// Start 阶段：声明两个档位（只声明，不判定）
//	s.Security().DeclareProfiles([]SecurityProfile{
//	    {Name: "qq.owner", Owner: "qq", AllowTools: []string{"*"}},
//	    {Name: "qq.guest", Owner: "qq",
//	     DenyTools: []string{"cmd_*", "files_write"},
//	     SensitiveWords: []string{"内部代号"}},
//	})
//
//	// 注入阶段：按发送者身份选择档位
//	s.InjectTextOpts(source, channel, text, sdk.InjectOptions{
//	    SecurityProfile: "qq.guest",
//	})
//
// ## 名字约束
//
// 档位名必须是 `<owner>.<档位>`，其中 owner 等于插件名。这条在内核侧
// 强制（否则插件 A 能声明 `qq.owner` 并夺走插件 B 的档位语义）。
type SecurityAPI interface {
	// DeclareProfile 声明一个命名档位。同名重复声明会覆盖（同一 owner
	// 只能覆盖自己的档位，不能覆盖别人的）。
	//
	// 返回 error 的情形：
	//   · 名字不符合 `<owner>.<档位>` 形式
	//   · owner 与本插件名不一致（试图声明别人的档位）
	//   · 安全模块未启用（此时没有可声明的策略容器）
	DeclareProfile(p SecurityProfile) error

	// DeclareProfiles 批量声明（任一失败即整体失败，便于早失败）。
	//
	// 为什么提供批量版：QQ 这类插件有 3~4 个档位，逐个判断 error 会让
	// Start 里堆满重复代码；而"声明到一半失败"是更难排查的状态。
	DeclareProfiles(ps []SecurityProfile) error
}

// SecurityProfile 是一个**命名安全档位**的声明。
//
// 与 ToolPolicy 类配置的区别：档位是**运行时可选**的（按 inputch 输入
// 携带的参数选择），不是全局生效的。
type SecurityProfile struct {
	// Name 是档位名，必须形如 `<owner>.<档位>`（如 "qq.guest"）。
	Name string
	// Owner 是声明者（插件名）。与 Name 的前缀必须一致。
	Owner string

	// AllowTools / DenyTools 是本档位下的工具白/黑名单（支持尾部 `*` 前缀）。
	//
	// 语义（与全局策略一致，且**只能收窄**）：
	//   · DenyTools 命中 ⇒ 拒绝（且它叠加在全局黑名单之上，不能反向放行）
	//   · AllowTools 非空且未命中 ⇒ 拒绝
	//   · 两者都空 ⇒ 本档位不额外限制
	AllowTools []string
	DenyTools  []string

	// SensitiveWords 是本档位额外追加的敏感词（在全局敏感词之上追加）。
	//
	// 用途：某些词只在特定身份下才敏感（如非所有者对话时不该看到内部代号）。
	SensitiveWords []string
}

package sdk

// SecurityAPI 的内核实现：把插件声明的安全档位登记到进程级 security.Runtime。
//
// ## 分工（用户裁定 2026-10-26 / 2026-10-29）
//
//	插件：DeclareProfile / DeclareProfiles      （只声明，不判定）
//	内核：Runtime.CheckTool / ActivateProfile   （只执行，不信任插件）
//
// 本文件是**声明面的转发层**，只做三件事：
//
//	① 用**内核侧真实插件名**填 Owner（插件无法伪造自己的身份）
//	② 校验名字前缀是 `<插件名>.`（防插件 A 声明 `qq.owner` 抢插件 B 的语义）
//	③ 转发到 security.Runtime.DeclareProfile
//
// ## 为什么 Owner 必须由内核填，而不是信任插件传的值
//
// 档位名是**全局唯一**的（`qq.owner` / `qq.guest`），而 Runtime 拒绝跨 owner
// 覆盖。若允许插件自报 owner，那么任何拿到声明面的插件都能声明
// `qq.owner` 并给它 `AllowTools: ["*"]` —— 直接把 QQ 的 owner 档位改成全放行。
//
// 所以 `SecurityProfile.Owner` 字段在**内核侧一律被覆盖**为真实插件名，
// 插件传什么都无效（包括传空、传错）。这不是"校验后拒绝"，而是
// **根本不采信** —— 后者没有可绕过的边界。
//
// ## 未启用时的行为
//
// security 未装配（`RuntimeGlobal()` 为 nil）时返回明确错误，而不是静默成功。
// 理由：插件会以为自己的档位生效了，而实际什么都没登记 ——
// 等到注入时带 `SecurityProfile: "qq.guest"` 才会响亮报"未声明档位"，
// 那时排查方向已经跑偏到"档位名拼错"。早失败比晚失败好。

import (
	"fmt"
	"strings"

	internalSecurity "github.com/JianFeeeee/HologramAgent/internal/security"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// securityImpl 实现 pubsdk.SecurityAPI。
type securityImpl struct {
	// pluginName 是内核注入的**真实**插件名，不来自插件自述。
	pluginName string
}

// NewSecurityAPI 构造策略声明面（内核装配时注入给每个插件）。
//
// pluginName 必须是内核侧的插件标识（Registry 里的 name），
// **不能**取自插件自己的 Name() 返回值。
func NewSecurityAPI(pluginName string) pubsdk.SecurityAPI {
	return &securityImpl{pluginName: pluginName}
}

func (s *securityImpl) DeclareProfile(p pubsdk.SecurityProfile) error {
	rt := internalSecurity.RuntimeGlobal()
	if rt == nil {
		return fmt.Errorf("security: declare profile %q failed - security module is not assembled "+
			"(the profile would be silently unregistered)", p.Name)
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return fmt.Errorf("security: declare profile failed - empty profile name")
	}

	// ★ Owner 用内核侧真实插件名覆盖，不采信 p.Owner。
	owner := s.pluginName
	if p.Owner != "" && p.Owner != owner {
		// 不静默改：显式告知插件作者"你填的 owner 被忽略了"，
		// 否则他照着错误认知继续写（如以为能给别的插件声明档位）。
		return fmt.Errorf("security: declare profile %q failed - Owner is %q but this plugin is %q "+
			"(a plugin may only declare profiles under its own name)", name, p.Owner, owner)
	}

	// 名字必须是 `<本插件名>.` 开头。前缀约束在 Runtime 里也有一份，
	// 这里提前判是为了给出**能指向插件作者**的错误文案。
	if !strings.HasPrefix(name, owner+".") {
		return fmt.Errorf("security: declare profile %q failed - name must be prefixed with %q "+
			"(e.g. %q)", name, owner+".", owner+".guest")
	}

	if err := rt.DeclareProfile(internalSecurity.Profile{
		Name:  name,
		Owner: owner,
		Policy: internalSecurity.PrincipalPolicy{
			AllowTools: p.AllowTools,
			DenyTools:  p.DenyTools,
		},
		SensitiveWords: p.SensitiveWords,
	}); err != nil {
		return fmt.Errorf("security: declare profile %q: %w", name, err)
	}
	return nil
}

func (s *securityImpl) DeclareProfiles(ps []pubsdk.SecurityProfile) error {
	// 先全量校验再逐个登记：部分成功比全不成功更难排查
	//（"为什么 qq.owner 生效了 qq.guest 没生效"这类问题很难定位）。
	// 但 Runtime 的 DeclareProfile 是幂等覆盖的，所以即使中途失败，
	// 已登记的那些也只是"多登记了"，不会留下半个档位。
	for i, p := range ps {
		if err := s.DeclareProfile(p); err != nil {
			return fmt.Errorf("profile[%d]: %w", i, err)
		}
	}
	return nil
}

// 编译期确认实现了公开接口。
var _ pubsdk.SecurityAPI = (*securityImpl)(nil)

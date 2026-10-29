package update

// 升级的**执行分派**：同一个"升级"请求，按安装形态走不同通道。
//
// ## 为什么需要这一层（这是 update 独立成模块的核心价值）
//
// 「升级」不是一件动作，而是一个**按环境分派**的决定：
//
//	deb/rpm   → 包管理器执行（文件归 dpkg/rpm 所有，绕过它会破坏数据库）
//	tar + 可写 → 自替换（本模块直接动手）
//	tar + 不可写 → 只告知命令（服务以非 root 运行，动不了 /usr/bin）
//	container → 重拉镜像（容器文件系统随销毁还原）
//
// 在这层存在之前，三个调用点（WebUI / agent 工具 / CLI）各自假定
// "升级 = 替换二进制"，于是 deb 形态下要么 EACCES、要么（root 时）
// 把 dpkg 数据库搞脏。
//
// ## 设计约定
//
// 本函数**不重启**、**不弹交互**。它要么完成、要么返回一个带
// 可执行命令的"需要委托"结果。重启与提示由调用方负责。

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// UpgradePlan 是一次升级的**执行计划**（在动手之前确定）。
type UpgradePlan struct {
	Capabilities Capabilities   `json:"capabilities"`
	Channel      UpgradeChannel `json:"channel"`
	// Executable 表示本模块能否自己完成（true 时才可调用 Execute）。
	Executable bool `json:"executable"`
	// Commands 是交给人的命令（Executable=false 时非空）。
	Commands []string `json:"commands,omitempty"`
	// Reason 解释为什么不能自己执行。
	Reason string `json:"reason,omitempty"`
}

// PlanUpgrade 决定这次升级该怎么执行。
//
// target 是本地包路径（可选）；包管理器通道下它会作为"直接装这个包"的备选。
func PlanUpgrade(c Capabilities, target string) UpgradePlan {
	p := UpgradePlan{Capabilities: c, Channel: c.Channel()}
	switch p.Channel {
	case ChannelSelfReplace:
		if c.CanWriteBin {
			p.Executable = true
			return p
		}
		p.Reason = c.Reason
	case ChannelPackageManager:
		p.Reason = "文件归包管理器所有：直接替换会让 dpkg/rpm 数据库与实际内容不一致"
	case ChannelContainerImage:
		p.Reason = "运行在容器内：文件系统随容器销毁还原，升级应重拉镜像"
	default:
		p.Reason = c.Reason
		if p.Reason == "" {
			p.Reason = "无法确定安装形态，拒绝自动升级"
		}
	}

	// 给出可执行命令（这是"不能自己做"时的唯一有用产出）。
	if cmd := c.UpgradeCommand(target); cmd != "" {
		p.Commands = []string{cmd}
	}
	if p.Channel == ChannelContainerImage {
		p.Commands = []string{
			"docker pull <镜像>:<新tag>  # 或你们的镜像仓库地址",
			"docker compose up -d       # 用新镜像重建容器",
		}
	}
	return p
}

// PackageManagerUpgrade 用本机包管理器升级本包（deb/rpm 通道）。
//
// ## 为什么需要 root，以及为什么本函数不自己提权
//
// dpkg/rpm 都要 root。而内核以非 root 服务账号运行（ProtectSystem=strict）。
// **本函数不自带提权**：sudo/polkit 的形态因发行版与部署策略而异，
// 在代码里猜一个（例如 exec sudo）会在无 tty 的服务环境里静默挂起等密码。
//
// 所以：能在 root 下调用就调（CLI 用 sudo 跑），否则返回一个
// ErrNeedPrivilege，由调用方转成"请执行这条命令"。
func PackageManagerUpgrade(c Capabilities, target string) error {
	// 预检：非 root 直接返回可执行错误，不要等到 dpkg 报一堆权限噪音。
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w：需要 root 执行 `%s`", ErrNeedPrivilege, c.UpgradeCommand(target))
	}

	switch c.Kind {
	case KindDeb:
		return upgradeDeb(c, target)
	case KindRPM:
		return upgradeRPM(c, target)
	default:
		return fmt.Errorf("包管理器升级不适用于形态 %q", c.Kind)
	}
}

// ErrNeedPrivilege 表示这条路径需要提权（调用方应转成可执行提示）。
var ErrNeedPrivilege = fmt.Errorf("需要提权")

func upgradeDeb(c Capabilities, target string) error {
	pkg := c.PackageName
	if pkg == "" {
		pkg = "hologramagent"
	}
	// 有本地包 ⇒ 先试 dpkg -i（离线/指定版本场景最可靠）。
	// 没本地包 ⇒ 走 apt（从配置的源取最新）。
	if target != "" {
		if err := runPkgCmd("dpkg", "-i", target); err != nil {
			// dpkg -i 常因依赖缺失失败；退回 apt -f install 补齐。
			// ★ 不吞错：第二次也失败就把两个错误一起报。
			if fixErr := runPkgCmd("apt-get", "install", "-f", "-y"); fixErr != nil {
				return fmt.Errorf("dpkg -i 失败（%v），且 apt -f install 也失败: %w", err, fixErr)
			}
		}
		return nil
	}
	return runPkgCmd("apt-get", "install", "--only-upgrade", "-y", pkg)
}

func upgradeRPM(c Capabilities, target string) error {
	pkg := c.PackageName
	if pkg == "" {
		pkg = "hologramagent"
	}
	if target != "" {
		// dnf 优先（能解依赖），没有就退回 rpm -U
		if _, err := exec.LookPath("dnf"); err == nil {
			return runPkgCmd("dnf", "install", "-y", target)
		}
		return runPkgCmd("rpm", "-U", target)
	}
	if _, err := exec.LookPath("dnf"); err == nil {
		return runPkgCmd("dnf", "upgrade", "-y", pkg)
	}
	return fmt.Errorf("找不到 dnf：请手工升级 %s", pkg)
}

// pkgCmdRunner 是包管理器命令的**执行器**（可注入）。
//
// ★ 为什么提为变量：包管理器命令会真的改动系统，而单元测试**绝不能**
// 真的跑 `apt-get install`（实测踩到：root 下跑真 apt，报
// "Unable to locate package"，还被当成业务逻辑失败）。
// 测试把它换成一个记录调用的假执行器，从而能断言“选了哪条命令”。
// 生产行为完全不变（默认就是下面那个真跑的实现）。
var pkgCmdRunner = realRunPkgCmd

// runPkgCmd 走当前执行器（生产 = realRunPkgCmd，测试可替换）。
func runPkgCmd(name string, args ...string) error { return pkgCmdRunner(name, args...) }

// realRunPkgCmd 执行包管理器命令，带输出上限与超时。
//
// ★ 为什么要超时：dpkg 在等锁（另一个 apt 在跑）时会长时间挂住，
// 而调用方（WebUI 的 HTTP handler）会因此一直持有连接。
func realRunPkgCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var sb strings.Builder
	cmd.Stdout = &sb
	cmd.Stderr = &sb

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s: %w", name, err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s %s 失败: %w\n输出尾部:\n%s",
				name, strings.Join(args, " "), err, tailLines(sb.String(), 20))
		}
		return nil
	case <-time.After(10 * time.Minute):
		_ = cmd.Process.Kill()
		return fmt.Errorf("%s %s 超时（10 分钟）：可能卡在包管理器锁上",
			name, strings.Join(args, " "))
	}
}

// tailLines 取文本末尾 n 行（错误消息里只需要尾部）。
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return "…（前略）\n" + strings.Join(lines[len(lines)-n:], "\n")
}

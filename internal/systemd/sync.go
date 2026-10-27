// Package systemd 负责「数据库配置 ↔ systemd 单元」的**双向一致性**。
//
// ## 要解决的问题（用户 2026-10-17 指出）
//
// 内核有两处"数据目录"来源，且**不等价**：
//
//	-data <dir>                    ← systemd 单元，决定 ks.DataDir
//	core.daemon.data_dir           ← 数据库，决定 ks.Cfg.Daemon.DataDir
//
// 实测（把副本 DB 的 data_dir 改成 /tmp/DB_SAYS，-data 仍给 /tmp/ddtest）：
//
//	知识库迁移扫描失败: open /tmp/DB_SAYS/knowledge: no such file
//	text memory active at /tmp/DB_SAYS/memory/text
//
// 而 ks.DataDir（-data）管的是 memory/graph.db（数据最多的图记忆）与
// 插件 SettingsAPI.DataDir()。⇒ **两者不一致时数据会分裂**：图记忆在 A、
// 文本/知识/文档在 B，检索跨目录、备份会漏。
//
// 生产当前两者都是 /home/newqqagent，所以没暴露。但 WebUI / CLI 允许改
// core.daemon.data_dir 而**没有任何提示** —— 用户改完重启就会发现数据"丢了"。
//
// ## 本包的职责
//
//  1. 检测不一致（Check）
//  2. 生成**可以直接复制执行**的修复命令（SyncCommand）
//
// ## ★ 关键设计：命令必须内联替换，不让用户手工编辑
//
// 用户明确要求：
//
//	「指令应当直接包含替换部分而不是让用户手动修改」
//
// 所以生成的是**一条幂等的 sed 原地替换 + daemon-reload + restart**，
// 而不是"请编辑文件把 -data 改成 xxx"。理由：
//
//	· 手工编辑会漏掉 drop-in（生产用 drop-in 覆盖 ExecStart，改主单元无效）
//	· 手工编辑容易把 --role=guard 一起改坏
//	· 一条命令可审计、可复制、可回滚（带 .bak）
package systemd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// UnitName 是内核的服务单元名。
const UnitName = "hologramagent.service"

// State 描述一次一致性检查的结果。
type State struct {
	// RuntimeDataDir 是当前进程**实际**用的数据目录（来自 -data）。
	RuntimeDataDir string
	// ConfigDataDir 是数据库里 core.daemon.data_dir 的值。
	ConfigDataDir string
	// UnitPath 是主单元文件路径（通常 /etc/systemd/system/homeagent.service）。
	UnitPath string
	// UnitDataDir 从单元文件里解析出的 -data 值（可能为空=没解析到）。
	UnitDataDir string
	// DropIns 是覆盖 ExecStart 的 drop-in 路径（非空时改主单元无效）。
	DropIns []string
}

// InSync 报告运行期与数据库配置是否一致。
func (s State) InSync() bool {
	return s.RuntimeDataDir == "" || s.ConfigDataDir == "" ||
		filepath.Clean(s.RuntimeDataDir) == filepath.Clean(s.ConfigDataDir)
}

// Describe 给出人类可读的诊断（用于启动日志 / WebUI 提示）。
func (s State) Describe() string {
	if s.InSync() {
		return ""
	}
	return fmt.Sprintf(
		"数据目录不一致：内核按 -data 用的是 %q，而数据库 core.daemon.data_dir 是 %q。\n"+
			"这两者**不等价**：\n"+
			"  · -data                    → memory/graph.db（图记忆）、插件数据根目录\n"+
			"  · core.daemon.data_dir     → memory/text、memory/documents、knowledge、nlp、adapters、agentfs\n"+
			"⇒ 不一致会让数据分裂，检索跨目录、备份会漏。",
		s.RuntimeDataDir, s.ConfigDataDir)
}

// Check 探测当前一致性。任何一步失败都返回已收集到的部分信息 + 错误，
// 由调用方决定"降级为只告警"还是"当失败处理"。
func Check(runtimeDataDir, configDataDir string) (State, error) {
	st := State{RuntimeDataDir: runtimeDataDir, ConfigDataDir: configDataDir}

	unitPath := filepath.Join("/etc/systemd/system", UnitName)
	if _, err := os.Stat(unitPath); err == nil {
		st.UnitPath = unitPath
		if v := parseDataFlagFromFile(unitPath); v != "" {
			st.UnitDataDir = v
		}
	} else {
		// 发行版可能装在 /lib/systemd/system
		alt := filepath.Join("/lib/systemd/system", UnitName)
		if _, err := os.Stat(alt); err == nil {
			st.UnitPath = alt
			st.UnitDataDir = parseDataFlagFromFile(alt)
		}
	}

	// drop-in 里若有 `ExecStart=`（清空）后重设，改主单元无效 —— 必须一并处理。
	dropDir := filepath.Join("/etc/systemd/system", UnitName+".d")
	if entries, err := os.ReadDir(dropDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			p := filepath.Join(dropDir, e.Name())
			if b, err := os.ReadFile(p); err == nil {
				if reExecStartReset.Match(b) {
					st.DropIns = append(st.DropIns, p)
				}
			}
		}
	}
	return st, nil
}

var (
	reDataFlag       = regexp.MustCompile(`(?:^|\s)-data[= ]([^\s"']+)`)
	reExecStartLn    = regexp.MustCompile(`(?m)^\s*ExecStart\s*=.*$`)
	reExecStartReset = regexp.MustCompile(`(?m)^\s*ExecStart\s*=\s*$`)
)

func parseDataFlagFromFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if m := reDataFlag.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// SyncCommand 生成**可直接复制执行**的修复命令。
//
// ## 为什么是一条内联 sed 而不是"请编辑文件"
//
// 用户明确要求「指令应当直接包含替换部分」。所以这里把新值直接拼进命令：
//
//	sudo sed -i.bak 's|-data [^ ]*|-data /新路径|' /etc/systemd/system/homeagent.service
//	sudo systemctl daemon-reload && sudo systemctl restart homeagent
//
// 特性：
//
//	· **幂等**：再跑一次结果相同
//	· **带 .bak**：可回滚（cp xxx.bak xxx）
//	· **只替换 -data 的值**，不碰 --role=guard 等其他参数
//	· drop-in 里若重设了 ExecStart，会**指出具体文件**并同样给出 sed 命令
//
// want 为空时用 ConfigDataDir。
func (s State) SyncCommand(want string) string {
	if want == "" {
		want = s.ConfigDataDir
	}
	want = strings.TrimSpace(want)
	if want == "" {
		return ""
	}
	var b strings.Builder

	b.WriteString("# 1) 把 systemd 单元的数据目录改成 ")
	b.WriteString(want)
	b.WriteString("（幂等，自动留 .bak）\n")

	if s.UnitPath != "" {
		fmt.Fprintf(&b,
			"sudo sed -i.bak -E 's|(-data[= ])[^ \"'\"']+|\\1%s|' %s\n",
			sedEscapeRepl(want), s.UnitPath)
	}
	// drop-in 覆盖了 ExecStart 的，必须**也**改它，否则主单元改了也没用。
	for _, d := range s.DropIns {
		fmt.Fprintf(&b,
			"sudo sed -i.bak -E 's|(-data[= ])[^ \"'\"']+|\\1%s|' %s\n",
			sedEscapeRepl(want), d)
	}

	b.WriteString("\n# 2) 让 systemd 重新加载并重启（guard 会用新参数拉起 worker）\n")
	b.WriteString("sudo systemctl daemon-reload && sudo systemctl restart ")
	b.WriteString(strings.TrimSuffix(UnitName, ".service"))
	b.WriteString("\n")

	b.WriteString("\n# 3) 校验（应看到 ExecStart 里的 -data 已是新值）\n")
	b.WriteString("systemctl show ")
	b.WriteString(UnitName)
	b.WriteString(" -p ExecStart --no-pager\n")

	b.WriteString("\n# 回滚（若需）\n")
	if s.UnitPath != "" {
		fmt.Fprintf(&b, "sudo cp %s.bak %s && sudo systemctl daemon-reload && sudo systemctl restart %s\n",
			s.UnitPath, s.UnitPath, strings.TrimSuffix(UnitName, ".service"))
	}
	return b.String()
}

// sedEscapeRepl 转义 sed 替换段里的特殊字符（分隔符用 |）。
func sedEscapeRepl(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "&", `\&`)
	return s
}

// RestartCommand 返回「热更新」命令：只重启（不动 systemd 配置）。
//
// 与 SyncCommand 的区别：配置没变、只换了二进制时用这个。
// 走 WebUI 的 agent restart 端点即可（exit 42 → guard 用新二进制重拉），
// 这里给出不依赖 WebUI 的等价 curl。
func RestartCommand(addr, apiKey string) string {
	return fmt.Sprintf(`curl -sS -X POST \
  -H "X-API-Key: %s" \
  "http://%s/api/v1/agents/agent/restart"`, apiKey, addr)
}

// IsUnderSystemd 报告当前进程是否由 systemd 拉起（用于决定是否提示提权）。
func IsUnderSystemd() bool {
	// INVOCATION_ID 由 systemd 注入，是最可靠的判据（容器里也可能有 systemd）。
	if os.Getenv("INVOCATION_ID") != "" {
		return true
	}
	// 回退：父进程是 pid 1 且 /run/systemd/system 存在。
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	return os.Getppid() == 1
}

// HasSudo 报告当前进程能否免密 sudo（决定提示里要不要写 sudo）。
func HasSudo() bool {
	if os.Geteuid() == 0 {
		return true
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return false
	}
	return exec.Command("sudo", "-n", "true").Run() == nil
}

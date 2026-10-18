package systemd

// sync.go 的判据：数据目录一致性检测与修复命令生成。
//
// ## 为什么需要（用户 2026-10-17 指出）
//
// 内核有**两处**数据目录来源，且不等价：
//
//	-data <dir>            ← systemd 单元，决定 ks.DataDir → memory/graph.db
//	core.daemon.data_dir   ← 数据库 → memory/text、documents、knowledge
//
// 不一致时**数据分裂**：图记忆在 A、文本/知识/文档在 B，检索跨目录、备份会漏。
//
// 用户还明确要求：「指令应当直接包含替换部分而不是让用户手动修改」
// ⇒ SyncCommand 必须是**可直接粘贴执行的 sed 原地替换**，
//   而不是"请把 -data 改成 xxx"。
//
// 这条要求不是风格偏好：生产用 drop-in 覆盖 ExecStart，
// 手工改主单元**完全无效**，而用户不会知道这一点。

import (
	"regexp"
	"strings"
	"testing"
)

// ★★★ 判据：一致/不一致的判定。
func TestInSync(t *testing.T) {
	cases := []struct {
		runtime, config string
		want            bool
		why             string
	}{
		{"/a/b", "/a/b", true, "完全相同"},
		{"/a/b/", "/a/b", true, "尾斜杠不算不一致（Clean 后相同）"},
		{"/a/b", "/a/c", false, "不同目录"},
		{"", "/a/b", true, "runtime 未知时不报警（宁可漏，不误报）"},
		{"/a/b", "", true, "config 未知时不报警"},
	}
	for _, c := range cases {
		st := State{RuntimeDataDir: c.runtime, ConfigDataDir: c.config}
		if got := st.InSync(); got != c.want {
			t.Errorf("InSync(%q, %q) = %v，应为 %v（%s）",
				c.runtime, c.config, got, c.want, c.why)
		}
	}
}

// ★★★ 判据：SyncCommand 必须内联替换，不能让用户手工编辑。
func TestSyncCommandInlinesReplacement(t *testing.T) {
	// 必须给 UnitPath：真实环境下 Check() 会解析出来。
	// 第一版没给，于是 sed 行不生成 —— 判据误报"没有内联替换"。
	st := State{
		RuntimeDataDir: "/old/dir",
		ConfigDataDir:  "/new/dir",
		UnitPath:       "/etc/systemd/system/homeagent.service",
	}
	cmd := st.SyncCommand("/new/dir")
	if cmd == "" {
		t.Fatal("不一致时应给出修复命令")
	}

	// ① 必须含 sed 原地替换（内联目标目录），而不是"请编辑文件"
	if !regexp.MustCompile(`sed -i`).MatchString(cmd) {
		t.Fatalf("★ 修复命令没有内联 sed -i 替换：\n%s\n"+
			"用户明确要求「指令应当直接包含替换部分而不是让用户手动修改」", cmd)
	}
	if !strings.Contains(cmd, "/new/dir") {
		t.Fatalf("★ 修复命令里没有出现目标目录 /new/dir：\n%s", cmd)
	}

	// ② 必须备份（改坏了要能回滚）
	if !strings.Contains(cmd, ".bak") {
		t.Fatalf("★ 修复命令没有备份原文件：\n%s", cmd)
	}

	// ③ 必须 daemon-reload（否则改了单元也不生效）
	if !strings.Contains(cmd, "daemon-reload") {
		t.Fatalf("★ 修复命令没有 systemctl daemon-reload：\n%s", cmd)
	}

	// ④ 必须真的重启服务（否则新目录不生效）
	if !strings.Contains(cmd, "restart") && !strings.Contains(cmd, "start") {
		t.Fatalf("★ 修复命令没有重启服务：\n%s", cmd)
	}
}

// ★★★ 判据：drop-in 也必须改（生产用 drop-in 覆盖 ExecStart，改主单元无效）。
func TestSyncCommandCoversDropIns(t *testing.T) {
	st := State{
		RuntimeDataDir: "/old",
		ConfigDataDir:  "/new",
		UnitPath:       "/etc/systemd/system/homeagent.service",
		DropIns:        []string{"/etc/systemd/system/homeagent.service.d/override.conf"},
	}
	cmd := st.SyncCommand("/new")
	if !strings.Contains(cmd, "override.conf") {
		t.Fatalf("★ 修复命令没有处理 drop-in —— 生产用 drop-in 覆盖 ExecStart，"+
			"只改主单元**完全无效**，而用户不会知道这一点：\n%s", cmd)
	}
}

// ★★★ 判据：目标目录为空时不给命令（避免生成一条把 -data 改成空串的命令）。
func TestSyncCommandEmptyTargetIsNoop(t *testing.T) {
	st := State{RuntimeDataDir: "/a", ConfigDataDir: ""}
	if got := st.SyncCommand(""); got != "" {
		t.Fatalf("目标目录为空时应返回空命令，实际：\n%s", got)
	}
}

// ★★★ 判据：sed 替换串里 sed 的**元字符**必须转义，否则生成坏命令。
//
// 注意分隔符是 `|`（见 SyncCommand），所以 `/` **不需**转义。
// 第一版判据断言 `/` 必须转义 —— 那是错的（分隔符是 | 而非 /）。
func TestSedEscapeRepl(t *testing.T) {
	// & 在 sed 替换段里表示"整个匹配" ⇒ 必须转义，否则目录里的 & 会被替换成匹配内容
	if got := sedEscapeRepl("/a&b"); !strings.Contains(got, `\&`) {
		t.Errorf("& 未转义（sed 会把它当成整个匹配）：%q", got)
	}
	// | 是分隔符本身 ⇒ 必须转义
	if got := sedEscapeRepl("/a|b"); !strings.Contains(got, `\|`) {
		t.Errorf("分隔符 | 未转义：%q", got)
	}
	// 反斜杠要双写
	if got := sedEscapeRepl(`/a\b`); !strings.Contains(got, `\\`) {
		t.Errorf("反斜杠未双写：%q", got)
	}
}

// ★★★ 判据：Describe 在不一致时给出两个具体路径，一致时为空。
func TestDescribe(t *testing.T) {
	ok := State{RuntimeDataDir: "/same", ConfigDataDir: "/same"}
	if got := ok.Describe(); got != "" {
		t.Errorf("一致时 Describe 应为空，实际：%s", got)
	}

	bad := State{RuntimeDataDir: "/a", ConfigDataDir: "/b"}
	d := bad.Describe()
	if !strings.Contains(d, "/a") || !strings.Contains(d, "/b") {
		t.Fatalf("Describe 必须给出两个具体路径：\n%s", d)
	}
}

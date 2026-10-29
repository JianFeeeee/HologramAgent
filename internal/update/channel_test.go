package update

// 升级通道分派的判据。
//
// 这一层的存在理由：同一个"升级"在不同安装形态下必须走不同通道。
// 判据要钉住的不是"函数能跑"，而是**分派结果是否正确**——
// 分派错了的后果是破坏包管理器数据库，那是要人工上机器救的故障。

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// ① deb/rpm 一律走包管理器，且**不可自替换**（即便当前是 root）。
func TestPlanUpgrade_DebGoesToPackageManager(t *testing.T) {
	c := Capabilities{
		Kind: KindDeb, Variant: VariantServer,
		BinPath: "/usr/bin/homed", PackageName: "hologramagent-server",
		CanWriteBin: false, Reason: "由 dpkg 安装",
	}
	p := PlanUpgrade(c, "")
	if p.Channel != ChannelPackageManager {
		t.Fatalf("deb 应走包管理器，实际 %s", p.Channel)
	}
	if p.Executable {
		t.Fatal("★ deb 形态绝不允许自替换（会破坏 dpkg 数据库）")
	}
	if len(p.Commands) == 0 {
		t.Fatal("不能自执行时必须给出可执行命令")
	}
	if !strings.Contains(p.Commands[0], "hologramagent-server") {
		t.Errorf("命令应带包名，实际: %s", p.Commands[0])
	}
	if p.Reason == "" {
		t.Error("拒绝自执行必须说明原因")
	}
}

// ② tar 且可写 ⇒ 自替换。
func TestPlanUpgrade_TarWritableSelfReplaces(t *testing.T) {
	c := Capabilities{Kind: KindTar, Variant: VariantServer, CanWriteBin: true}
	p := PlanUpgrade(c, "")
	if p.Channel != ChannelSelfReplace || !p.Executable {
		t.Fatalf("tar+可写应自替换，实际 channel=%s executable=%v", p.Channel, p.Executable)
	}
	if len(p.Commands) != 0 {
		t.Error("能自己做的事不该给命令（会让人以为要手动）")
	}
}

// ③ tar 但不可写 ⇒ 不可执行，但必须给出命令（这是本机的主要现实形态）。
func TestPlanUpgrade_TarUnwritableGivesCommand(t *testing.T) {
	c := Capabilities{
		Kind: KindTar, Variant: VariantServer, CanWriteBin: false,
		Reason: "当前用户对 /usr/local/bin 没有写权限",
	}
	p := PlanUpgrade(c, "")
	if p.Executable {
		t.Fatal("不可写时不得声称可执行")
	}
	if len(p.Commands) == 0 {
		t.Fatal("★ 不可写时必须给出可执行命令，否则用户无从下手")
	}
	if !strings.Contains(strings.Join(p.Commands, " "), "homed-upgrade") {
		t.Errorf("tar 形态应建议 homed-upgrade，实际: %v", p.Commands)
	}
}

// ④ 容器 ⇒ 重拉镜像，不给包管理器命令。
func TestPlanUpgrade_ContainerPullsImage(t *testing.T) {
	c := Capabilities{Kind: KindContainer, Variant: VariantServer}
	p := PlanUpgrade(c, "")
	if p.Channel != ChannelContainerImage {
		t.Fatalf("容器应走镜像通道，实际 %s", p.Channel)
	}
	if p.Executable {
		t.Fatal("容器内不得自替换（重启即还原）")
	}
	if len(p.Commands) < 2 {
		t.Errorf("应给出 pull + 重建两条命令，实际: %v", p.Commands)
	}
}

// ⑤ 源码运行/未知 ⇒ 拒绝，且必须说明。
func TestPlanUpgrade_SourceRefuses(t *testing.T) {
	for _, kind := range []InstallKind{KindSource, KindUnknown} {
		c := Capabilities{Kind: kind, Variant: VariantUnknown}
		p := PlanUpgrade(c, "")
		if p.Executable {
			t.Errorf("%s 形态不得自替换", kind)
		}
		if p.Reason == "" {
			t.Errorf("%s 拒绝时必须说明原因（不能只有 bool）", kind)
		}
	}
}

// ⑥ 非 root 时 PackageManagerUpgrade 返回 ErrNeedPrivilege，而不是去跑 dpkg。
//
// 判据重点是**没有执行命令**：非 root 下 dpkg 会输出一堆权限噪音，
// 而真正该告诉用户的是"你需要 sudo"。
// ⑥ 包管理器执行**不**真跑命令，而是通过可注入执行器验证选了什么。
//
// ## 为什么必须注入（真实事故）
//
// 本用例最初直接调 PackageManagerUpgrade，在 root 下真的执行了
// `apt-get install --only-upgrade -y hologramagent-full` —— 报
// "Unable to locate package" 并把真实的系统查询当成了业务失败。
// 单元测试**绝不能**真的调用包管理器。
//
// 现在把 pkgCmdRunner 换掉，从而验证的是“选了哪条命令”，
// 而不是“命令能不能跑”。
func TestPackageManagerUpgrade_ChoosesRightCommand(t *testing.T) {
	var got []string
	old := pkgCmdRunner
	pkgCmdRunner = func(name string, args ...string) error {
		got = append(got, name+" "+strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { pkgCmdRunner = old })

	// deb 无本地包 ⇒ 走 apt-get --only-upgrade
	c := Capabilities{Kind: KindDeb, PackageName: "hologramagent-full"}
	if err := PackageManagerUpgrade(c, ""); err != nil {
		if errors.Is(err, ErrNeedPrivilege) {
			t.Skip("非 root：本用例需要 root 才能走到执行分支")
		}
		t.Fatalf("执行失败: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "--only-upgrade") {
		t.Errorf("deb 应选 apt-get --only-upgrade，实际: %v", got)
	}
	if !strings.Contains(got[0], "hologramagent-full") {
		t.Errorf("命令应带包名，实际: %v", got)
	}
}

// ⑥b 非 root 时必须返回 ErrNeedPrivilege（而不是去跑命令）。
//
// 判据重点是**没有执行命令**：非 root 下 dpkg 只会输出权限噪音，
// 而真正该告诉用户的是“你需要 sudo”。
func TestPackageManagerUpgrade_NonRootReturnsNeedPrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 环境无法验证非 root 分支")
	}
	called := false
	old := pkgCmdRunner
	pkgCmdRunner = func(name string, args ...string) error { called = true; return nil }
	t.Cleanup(func() { pkgCmdRunner = old })

	c := Capabilities{Kind: KindDeb, PackageName: "hologramagent-full"}
	err := PackageManagerUpgrade(c, "")
	if err == nil {
		t.Fatal("非 root 时应报错（不得静默成功）")
	}
	if !errors.Is(err, ErrNeedPrivilege) {
		t.Errorf("应返回 ErrNeedPrivilege 以便调用方转成提示，实际: %v", err)
	}
	if called {
		t.Error("★ 非 root 时不应去执行包管理器命令")
	}
	if !strings.Contains(err.Error(), "apt") {
		t.Errorf("错误里应含可执行命令，实际: %v", err)
	}
}

// ⑦ detectVariant 不能因为缺文件就 panic，且 client（无 homed）要能识别。
func TestDetectVariant_NoKernelIsClientOrUnknown(t *testing.T) {
	v := detectVariant("/nonexistent/homed")
	if v != VariantClient && v != VariantUnknown {
		t.Errorf("无 homed 时应为 client 或 unknown，实际 %s", v)
	}
}

// ⑧ isGoBuildTempPath 只认 go build 的临时目录形态，不认任意 /tmp。
//
// 这一条防的是"用 /tmp/ 判源码运行"的宽判据：那会把
// "解包到 /tmp 试装"和 CI 集成测都误判成 dev 环境而拒绝升级。
func TestIsGoBuildTempPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/go-build123/b001/exe/homed", true},
		{"/root/.cache/go-build/ab/cd-homed", false}, // 构建缓存，不是 go run 产物
		{"/tmp/somewhere/homed", false},              // 普通 /tmp 路径不算
		{"/usr/local/bin/homed", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isGoBuildTempPath(c.path); got != c.want {
			t.Errorf("isGoBuildTempPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

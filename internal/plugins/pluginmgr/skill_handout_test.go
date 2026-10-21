package pluginmgr

// 随包 skill 移交的判据。
//
// 覆盖的缺口：插件与 skill 此前是两种**互不相通**的分发物 —— 插件解压到
// <plugin.dir>/<name>/，skill 由 skillmgr 扫 <data.dir>/skills/。
// 于是随插件分发的 skill 装完就到不了用户手上。
//
// 修法：pluginmgr（内置插件，持有完整 SDK）在解压后扫 <插件目录>/skill/，
// 对每个条目发 skill_detected；skillmgr 已订阅该事件并接管注册。
//
// 判据必须钉住**三个环节**，缺一个链路就不通：
//   ① 发现：只认真正的纯 SKILL，不误发别的目录
//   ② 移交：事件类型/载荷正确（skillmgr 只读 payload["path"]）
//   ③ 贯通：事件真的会被 skillmgr 的订阅者收到（用真事件总线验证）

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// buildHmapWithSkills 造一个带 skill/ 目录的插件包。
func buildHmapWithSkills(t *testing.T, files map[string]string) []byte {
	return buildHmapWithSkillsVer(t, "1.0.0", files)
}

// buildHmapWithSkillsVer 同前，但可指定版本号。
//
// ★ 不能对已生成的 zip 做字节替换来改版本 —— 那会破坏 zip 的校验和，
// 报 "zip: checksum error"（实测踩到）。必须**重新打包**。
func buildHmapWithSkillsVer(t *testing.T, version string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	add := func(name, content string) {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fw, err := w.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte(content))
	}
	// ★ 不带 platforms 字段 ⇒ legacy 模式：entry 直接指向 plugin.bin。
	//
	// 初版写了 platforms 却没提供对应名字的二进制（platformBinary() 期望
	// plugin.bin.<goos>.<goarch>），于是 validatePackage 直接以
	// "no valid binary found" 拒绝 —— 判据全红但并非功能缺失。
	// 这是**判据自己的缺陷**（造的包不合规），不是被测代码的问题。
	add("plugin.json", `{"name":"skilltest","version":"`+version+`","entry":"plugin.bin"}`)
	add("plugin.bin", "#!/bin/sh\n")
	for n, c := range files {
		add(n, c)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newTestPluginWithBus 造一个挂了**真事件总线**的 pluginmgr。
func newTestPluginWithBus(t *testing.T, dir string) (*Plugin, *events.Bus) {
	t.Helper()
	bus := events.NewBus()
	sdkInst := sdk.New("pluginmgr", sdk.SDKConfig{
		Settings: &pmSettings{},
		EventBus: bus,
	})
	return &Plugin{name: "pluginmgr", pluginDir: dir, sdk: sdkInst}, bus
}

// ★★★ 判据：安装带 skill 的插件 → **skill_detected 事件真的发出来了**。
//
// 这是整条链路的核心：接收端（skillmgr）是现成的，要看的是发布端真的发。
func TestInstallPublishesSkillDetected(t *testing.T) {
	dir := t.TempDir()
	p, bus := newTestPluginWithBus(t, dir)

	// 订阅（模拟 skillmgr 的订阅）
	type got struct {
		path string
		src  string
	}
	ch := make(chan got, 8)
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) {
		path, _ := e.Payload["path"].(string)
		ch <- got{path, e.Source}
	})

	data := buildHmapWithSkills(t, map[string]string{
		"skill/my-skill/SKILL.md": "---\nname: my-skill\n---\n# hi\n",
		"skill/other/skill.json":  `{"name":"other"}`,
	})
	if _, err := p.installFromData(data, false); err != nil {
		t.Fatalf("安装失败: %v", err)
	}

	// 必须收到恰好 2 个事件
	var paths []string
	for i := 0; i < 2; i++ {
		select {
		case g := <-ch:
			paths = append(paths, g.path)
			if g.src == "" {
				t.Error("事件缺少 Source（无法溯源是谁带来的 skill）")
			}
		default:
			t.Fatalf("只收到 %d 个 skill_detected 事件，期望 2 个（已收: %v）", i, paths)
		}
	}
	joined := strings.Join(paths, ",")
	if !strings.Contains(joined, "my-skill") || !strings.Contains(joined, "other") {
		t.Errorf("移交的路径不对: %v", paths)
	}
	// 路径必须指向**解压后**的位置（而不是包里的相对路径）
	for _, pa := range paths {
		if !strings.HasPrefix(pa, filepath.Join(dir, "skilltest")) {
			t.Errorf("★ 路径不在插件目录下（skillmgr 会找不到）: %s", pa)
		}
		if _, err := os.Stat(pa); err != nil {
			t.Errorf("★ 移交的路径不存在: %s", pa)
		}
	}
}

// ★★ 判据：**没有 skill/ 目录的插件不发任何事件**（绝大多数插件如此）。
//
// 不过滤的话每次装插件都会发噪音事件，skillmgr 反复收到无效路径。
func TestPluginWithoutSkillsPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	p, bus := newTestPluginWithBus(t, dir)
	n := 0
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) { n++ })

	data := buildHmapWithSkills(t, map[string]string{
		"plugin.json":           `{"name":"x","version":"1.0.0","entry":"plugin.bin"}`,
		"rawfile/some/data.txt": "x",
	})
	if _, err := p.installFromData(data, false); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if n != 0 {
		t.Errorf("★ 无 skill 的插件不该发事件，实际发了 %d 个", n)
	}
}

// ★★ 判据：**不是纯 SKILL 的目录不发**（避免 skillmgr 报无意义失败）。
//
// skillmgr 的 loadOne 要求 SKILL.md 或 skill.json。误放在 skill/ 下的
// sidecar 目录（含 main.js）会被它拒收 —— 与其发出去再失败，
// 不如在源头过滤并如实回报。
func TestNonSkillDirsAreNotPublished(t *testing.T) {
	dir := t.TempDir()
	p, bus := newTestPluginWithBus(t, dir)
	var paths []string
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) {
		pa, _ := e.Payload["path"].(string)
		paths = append(paths, pa)
	})

	data := buildHmapWithSkills(t, map[string]string{
		"skill/good/SKILL.md":       "x",
		"skill/bad-sidecar/main.js": "x", // 不是纯 SKILL
		"skill/empty/notes.txt":     "x", // 没有 SKILL.md/skill.json
	})
	res, _ := p.installFromData(data, false)
	m := res.(map[string]interface{})

	if len(paths) != 1 {
		t.Errorf("★ 只应移交 1 个（good），实际 %d: %v", len(paths), paths)
	}
	if !strings.HasSuffix(paths[0], "good") {
		t.Errorf("移交错了条目: %v", paths)
	}
	// 被跳过的必须**如实回报**，不静默吞掉
	failed, _ := m["skills_failed"].([]string)
	if len(failed) != 2 {
		t.Errorf("★ 被跳过的 2 个应回报在 skills_failed，实际 %v", m["skills_failed"])
	}
}

// ★★ 判据：脚手架文件不触发移交（.gitkeep/.gitignore）。
func TestScaffoldFilesDoNotTriggerPublish(t *testing.T) {
	dir := t.TempDir()
	p, bus := newTestPluginWithBus(t, dir)
	var paths []string
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) {
		pa, _ := e.Payload["path"].(string)
		paths = append(paths, pa)
	})
	data := buildHmapWithSkills(t, map[string]string{
		"skill/.gitkeep":   "",
		"skill/.gitignore": "*\n",
		// ★ 隐藏**目录**：仅靠 IsDir() 挡不住，必须显式跳过 "."
		// （隐藏文件是文件，会被 IsDir() 过滤掉，所以单测它们**测不到**这条规则）。
		"skill/.hidden-dir/SKILL.md": "x",
	})
	res, _ := p.installFromData(data, false)
	if len(paths) != 0 {
		t.Errorf("★ 脚手架不应触发移交: %v", paths)
	}
	m := res.(map[string]interface{})
	if n, _ := m["skills_registered"].(int); n != 0 {
		t.Errorf("skills_registered 应为 0，实际 %v", m["skills_registered"])
	}
}

// ★★★ 判据：**端到端贯通** —— pluginmgr 发的事件，skillmgr 的订阅者收得到。
//
// 前面几条验证"发出来了"，这条验证"真的能被接收端收到"。
// 用真实事件总线 + 模拟 skillmgr 的处理逻辑（读 payload["path"] 并校验可加载）。
func TestEventReachesSkillmgrConsumer(t *testing.T) {
	dir := t.TempDir()
	p, bus := newTestPluginWithBus(t, dir)

	// 模拟 skillmgr 的订阅者：它只读 path，然后 loadOne
	loaded := make(chan string, 4)
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) {
		path, _ := e.Payload["path"].(string)
		if path == "" {
			return // skillmgr 的实际行为：空路径直接返回
		}
		// 复刻 skillmgr 的准入判据
		for _, f := range []string{"SKILL.md", "skill.json"} {
			if _, err := os.Stat(filepath.Join(path, f)); err == nil {
				loaded <- path
				return
			}
		}
	})

	data := buildHmapWithSkills(t, map[string]string{
		"skill/discipline/SKILL.md": "# 纪律\n",
	})
	if _, err := p.installFromData(data, false); err != nil {
		t.Fatal(err)
	}

	select {
	case path := <-loaded:
		if !strings.HasSuffix(path, "discipline") {
			t.Errorf("加载的路径不对: %s", path)
		}
		// 内容必须真的可读（证明路径指向解压后的真实文件）
		b, err := os.ReadFile(filepath.Join(path, "SKILL.md"))
		if err != nil || !strings.Contains(string(b), "纪律") {
			t.Errorf("★ skill 内容不可读: %v %q", err, b)
		}
	default:
		t.Fatal("★ 端到端不通：skillmgr 的订阅者没收到事件")
	}
}

// ★★ 判据：**升级路径也移交**（不只在全新安装时）。
//
// 漏掉升级路径的话，已装插件的用户**永远拿不到**新版的 skill ——
// 而升级是最常见的更新方式。
func TestUpgradeAlsoHandsOutSkills(t *testing.T) {
	dir := t.TempDir()
	p, bus := newTestPluginWithBus(t, dir)
	var paths []string
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) {
		pa, _ := e.Payload["path"].(string)
		paths = append(paths, pa)
	})

	v1 := buildHmapWithSkills(t, map[string]string{"skill/s1/SKILL.md": "v1"})
	if _, err := p.installFromData(v1, false); err != nil {
		t.Fatal(err)
	}
	first := len(paths)
	if first == 0 {
		t.Fatal("首次安装未移交")
	}

	// 升级到 v2：**重新打包**（不是替换字节 —— 那会坏校验和）
	v2 := buildHmapWithSkillsVer(t, "2.0.0", map[string]string{"skill/s2/SKILL.md": "v2"})
	res, _ := p.installFromData(v2, true)
	m, _ := res.(map[string]interface{})
	if e, bad := m["error"]; bad {
		t.Fatalf("升级失败: %v (%v)", e, m["details"])
	}
	if len(paths) <= first {
		t.Fatalf("★ 升级路径未移交 skill（首次 %d，升级后 %d）", first, len(paths))
	}
	// 升级后新的 skill 也要在
	found := false
	for _, pa := range paths[first:] {
		if strings.HasSuffix(pa, "s2") {
			found = true
		}
	}
	if !found {
		t.Errorf("升级后未移交新 skill: %v", paths[first:])
	}
}

// ★ 判据：安装结果里如实回报 skill 数（不静默）。
func TestInstallResultReportsSkillCount(t *testing.T) {
	dir := t.TempDir()
	p, _ := newTestPluginWithBus(t, dir)
	data := buildHmapWithSkills(t, map[string]string{"skill/a/SKILL.md": "x"})
	res, _ := p.installFromData(data, false)
	m := res.(map[string]interface{})
	if n, ok := m["skills_registered"].(int); !ok || n != 1 {
		t.Errorf("★ skills_registered 应为 1，实际 %#v", m["skills_registered"])
	}
	// 该字段必须能被 JSON 序列化（外部调用方要读它）
	if _, err := json.Marshal(m); err != nil {
		t.Errorf("安装结果无法序列化: %v", err)
	}
}

// ★★★ 判据：**启动时必须重新登记随包 skill**（重启不该丢）。
//
// 这条是实测发现的真缺陷（2026-10-15）：
//
//	装插件 → skill 注册 ✅
//	  ↓ worker 重启
//	skillmgr 只扫 skillsDir（skill 在**插件目录**里）→ skill 静默消失 ❌
//
// 移交只发生在安装那一刻，重启后没有任何重新发现机制。
// 后果是"用户以为 skill 还在，实际已经没了"——典型的静默丢失。
func TestStartupReHandsOutPluginSkills(t *testing.T) {
	dir := t.TempDir()
	p, bus := newTestPluginWithBus(t, dir)
	p.pluginDir = dir

	// 造一个"已装插件"的目录布局：<pluginDir>/<name>/skill/<s>/SKILL.md
	// （模拟上一次安装留下的产物，进程重启后就是这个状态）
	skillDir := filepath.Join(dir, "someplugin", "skill", "my-discipline")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# 纪律"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 订阅（模拟 skillmgr 已在运行）
	got := make(chan string, 4)
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) {
		pa, _ := e.Payload["path"].(string)
		got <- pa
	})

	// 调启动重扫（不启 goroutine，直接同步调便于断言）
	p.reHandOutAllPluginSkills()

	select {
	case path := <-got:
		if !strings.HasSuffix(path, "my-discipline") {
			t.Errorf("移交路径不对: %s", path)
		}
		// 路径必须真实可读（否则 skillmgr 会失败）
		if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
			t.Errorf("★ 移交路径不可读: %v", err)
		}
	default:
		t.Fatal("★ 启动重扫没有重新移交随包 skill（重启后会静默丢失）")
	}
}

// ★★ 判据：启动重扫对**没有 skill 的插件**不发噪音事件。
func TestStartupRescanIgnoresPluginsWithoutSkills(t *testing.T) {
	dir := t.TempDir()
	p, bus := newTestPluginWithBus(t, dir)
	p.pluginDir = dir
	// 两个没有 skill/ 的插件目录
	for _, n := range []string{"plain1", "plain2"} {
		os.MkdirAll(filepath.Join(dir, n), 0o755)
	}
	n := 0
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) { n++ })
	p.reHandOutAllPluginSkills()
	if n != 0 {
		t.Errorf("无 skill 的插件不该发事件，实际 %d 个", n)
	}
}

package test

// 适配器同步策略判据（2026-11-11）。
//
// ## 背景：一个真实事故
//
// `adapters/*.lua` 落在**用户的数据目录**里，但它们是内核自带的协议适配层，
// 会随内核修复而变。内核该不该动用户盘上的文件？
//
// 本机实测的现状：8 个适配器（openai/anthropic/github/groq/mistral/ollama/
// kimicode/server）落后内置版 3000~6500 字节，缺的是用量归一化
// （cache_read / reasoning_tokens）、流式 tool_calls 等真实修复。
// 而日志只说一句「保留用户版本」——看起来像用户自己的选择，
// 完全看不出是「永久落后」，症状最终表现为「模型不听话」「工具调不动」。
//
// ## 为什么默认 off
//
// 「旧发行版残留」与「用户手工改过」在文件内容上**长得一模一样**：
// 两者都只是「hash 不等于内置版」。既然无法区分，就不该猜 ——
// 擅自覆盖用户文件比落后一个版本更糟。
//
// 所以：
//
//	off（默认）  不碰文件，但告警必须**可操作**（列出哪些、说明影响、给处理办法）
//	auto         差异即备份为 `<name>.lua.bak-<hash8>` 后覆盖，永不落后
//
// 用户要长期定制有正道：`core.llm.sources.<name>.adapter_path` 是每源独立可配的。
//
// ## 判据怎么读
//
// AdapterDir() 与 SetAdapterSync()/AdapterSyncMode() 都是公开的，
// 完全黑盒：起 VM → 读文件系统 → 断言。

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/lua"
)

func readManifest(t *testing.T, dir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".bundled"))
	if err != nil {
		t.Fatalf("读清单: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			t.Fatalf("清单行格式异常: %q", line)
		}
		out[parts[0]] = parts[1]
	}
	return out
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func countBackups(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var n int
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak-") {
			n++
		}
	}
	return n
}

// drift 造一次「盘上与内置不一致」，返回漂移后的 hash。
func drift(t *testing.T, dir, name string) string {
	t.Helper()
	target := filepath.Join(dir, name+".lua")
	orig, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读 %s: %v", target, err)
	}
	if err := os.WriteFile(target, append([]byte("-- drifted (旧发行版残留)\n"), orig...), 0644); err != nil {
		t.Fatalf("写: %v", err)
	}
	return hashFile(t, target)
}

// 判据：默认模式必须是 off。
//
// 这条钉的是「忘记注入配置时的行为」——必须是保守的，不能默认覆盖用户文件。
func TestAdapterSyncDefaultsToOff(t *testing.T) {
	v := lua.NewVM(t.TempDir())
	if got := v.AdapterSyncMode(); got != "off" {
		t.Fatalf("★ 默认模式 = %q，期望 off（默认覆盖用户文件太危险）", got)
	}
	// 大小写/空白不应影响判定
	v.SetAdapterSync("  AUTO  ")
	if got := v.AdapterSyncMode(); got != "auto" {
		t.Fatalf("★ 大小写归一化失效：%q", got)
	}
}

// 判据：off 模式下**不碰**用户的适配器文件。
//
// 变异验证（实测）：把 off 分支改成也覆盖 ⇒ 本判据红。
func TestOffModeLeavesDiskUntouched(t *testing.T) {
	dir := t.TempDir()
	v1 := lua.NewVM(dir) // 默认 off
	if err := v1.Start(); err != nil {
		t.Fatalf("首次 Start: %v", err)
	}
	v1.Stop()

	want := drift(t, dir, "openai")

	v2 := lua.NewVM(dir)
	if err := v2.Start(); err != nil {
		t.Fatalf("二次 Start: %v", err)
	}
	v2.Stop()

	if got := hashFile(t, filepath.Join(dir, "openai.lua")); got != want {
		t.Fatal("★ off 模式仍改动了盘上文件")
	}
	if n := countBackups(t, dir); n != 0 {
		t.Fatalf("★ off 模式产生了 %d 个备份（说明它动过文件）", n)
	}
}

// 判据：auto 模式下，差异必须被更新，且留下可回滚的备份。
//
// 这条钉的是「永不落后」这条路：选 auto 的人要的是跟随内核。
func TestAutoModeUpdatesWithBackup(t *testing.T) {
	dir := t.TempDir()
	v1 := lua.NewVM(dir)
	if err := v1.Start(); err != nil {
		t.Fatalf("首次 Start: %v", err)
	}
	v1.Stop()

	driftHash := drift(t, dir, "groq")
	driftedContent, _ := os.ReadFile(filepath.Join(dir, "groq.lua"))

	v2 := lua.NewVM(dir)
	v2.SetAdapterSync("auto")
	if err := v2.Start(); err != nil {
		t.Fatalf("二次 Start: %v", err)
	}
	v2.Stop()

	now := hashFile(t, filepath.Join(dir, "groq.lua"))
	if now == driftHash {
		t.Fatal("★ auto 模式下盘上仍是旧版")
	}
	// 旧内容必须能在备份里找到（不无声丢改动）
	bak := filepath.Join(dir, "groq.lua.bak-"+driftHash[:8])
	old, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("★ 没留下备份（用户改动被无声丢弃）: %v", err)
	}
	if string(old) != string(driftedContent) {
		t.Fatal("★ 备份内容与覆盖前不一致")
	}
}

// 判据：auto 模式下，与内置一致时不得重复备份（否则每次启动堆一个 .bak）。
func TestAutoModeNoRedundantBackup(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		v := lua.NewVM(dir)
		v.SetAdapterSync("auto")
		if err := v.Start(); err != nil {
			t.Fatalf("第 %d 次 Start: %v", i+1, err)
		}
		v.Stop()
	}
	if n := countBackups(t, dir); n != 0 {
		t.Fatalf("★ 无漂移却产生 %d 个备份（每次启动都会堆积）", n)
	}
}

// 判据：反复启停后状态稳定（两种模式都不该在启停间漂移）。
//
// 这是旧设计的另一半症状：清单基线被来回改写，导致每次启动都重判一遍。
func TestAdapterStateStableAcrossRestarts(t *testing.T) {
	for _, mode := range []string{"off", "auto"} {
		dir := t.TempDir()
		v := lua.NewVM(dir)
		v.SetAdapterSync(mode)
		if err := v.Start(); err != nil {
			t.Fatalf("[%s] Start: %v", mode, err)
		}
		v.Stop()

		drift(t, dir, "mistral")

		var prev string
		for i := 0; i < 3; i++ {
			vv := lua.NewVM(dir)
			vv.SetAdapterSync(mode)
			if err := vv.Start(); err != nil {
				t.Fatalf("[%s] 第 %d 次 Start: %v", mode, i+1, err)
			}
			vv.Stop()

			m := readManifest(t, dir)
			cur := ""
			for _, name := range []string{"openai", "groq", "mistral", "deepseek"} {
				disk := hashFile(t, filepath.Join(dir, name+".lua"))
				if m[name] != disk {
					t.Fatalf("[%s] 第 %d 次启动后 %s 清单与盘上不一致", mode, i+1, name)
				}
				cur += m[name]
			}
			if prev != "" && cur != prev {
				t.Fatalf("[%s] 清单在启停间漂移（第 %d 次）", mode, i+1)
			}
			prev = cur
		}
	}
}

// 判据：清单覆盖全部落盘的适配器（漏记 = 那个适配器永远不会被体检到）。
func TestManifestCoversAllBundledAdapters(t *testing.T) {
	dir := t.TempDir()
	v := lua.NewVM(dir)
	if err := v.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer v.Stop()

	m := readManifest(t, dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var n int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		n++
		name := strings.TrimSuffix(e.Name(), ".lua")
		if _, ok := m[name]; !ok {
			t.Errorf("★ 适配器 %s 落盘了但没进清单", name)
		}
	}
	if n == 0 {
		t.Fatal("★ 没有落盘任何适配器")
	}
}

// 判据：首次安装（盘上无文件）必须落盘，两种模式都一样。
//
// 反向保护：off 的语义是「不动**已存在**的用户文件」，不是「不安装」。
func TestFirstInstallHappensInBothModes(t *testing.T) {
	for _, mode := range []string{"off", "auto"} {
		dir := t.TempDir()
		v := lua.NewVM(dir)
		v.SetAdapterSync(mode)
		if err := v.Start(); err != nil {
			t.Fatalf("[%s] Start: %v", mode, err)
		}
		v.Stop()

		for _, name := range []string{"openai", "deepseek", "server"} {
			if _, err := os.Stat(filepath.Join(dir, name+".lua")); err != nil {
				t.Fatalf("[%s] 首次安装缺 %s.lua", mode, name)
			}
		}
	}
}

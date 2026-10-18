package pluginmgr

// 插件部署**必须走 plugin_install**，不能手工 install/cp 二进制。
//
// ## 我犯的错（2026-10-18）
//
// 改完 commondev 后，我是这么"部署"的：
//
//	python3 -c "zipfile...extractall('/tmp/x')"
//	install -m 0755 /tmp/x/plugin.bin <插件目录>/plugin.bin
//	install -m 0644 /tmp/x/rawfile/runtime/pi/sidecar.mjs <插件目录>/...
//	systemctl restart homeagent
//
// 用户一句「为什么不打包为 hmap 安装，而是手动替换」点破了问题。
//
// ## 手动替换丢了什么
//
// 走 installFromData 能拿到、而手工 cp 拿不到的：
//
//   · **整包校验和**：返回 checksum="13a46f87…"，正是 .hmap 的 sha256。
//     手工替换只动了 plugin.bin，包与安装结果之间没有任何可核对的凭据。
//   · **备份 + 失败回滚**：安装是「先备份旧目录 → 解包 → 失败则回滚到
//     旧版本」。手工 install 半途失败会留下**半新半旧**的目录，
//     那比彻底失败更难查（插件能加载、某个功能诡异失效）。
//   · **全量路径校验**：validatePackage 遍历**所有**条目拒绝 `..` 与绝对
//     路径（该循环曾因 break 而漏检 rawfile 条目，已修）。
//   · **rawfile 全量解包**：我手工只覆盖了 2 个 sidecar，其余 rawfile 内容
//     （node/pi 运行时）仍是旧的。实测 `rawfile/` 目录 mtime 停在 10-16，
//     而标准安装后整个目录 mtime 才更新 —— 这就是漏解包的证据。
//   · **配置保留与 skill 注册**：返回 config_kept=true / skills_registered=1。
//
// ## 判据做什么
//
// 代码层面无法禁止人手工 cp；但可以把**唯一正确入口**钉住：插件包的
// 落盘必须经过 extractPackage / installFromData 这条链。这里断言
// installFromData 仍然具备"先备份、失败回滚、校验路径"这三件事 ——
// 一旦有人把安装简化成"直接解压覆盖"，判据立刻变红。

import (
	"os"
	"strings"
	"testing"
)

// ★★★ 判据：安装路径必须保留「备份 → 解包 → 失败回滚」。
func TestInstallKeepsBackupAndRollback(t *testing.T) {
	b, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	// 定位 installFromData 函数体
	i := strings.Index(src, "func (p *Plugin) installFromData(")
	if i < 0 {
		t.Fatal("找不到 installFromData —— 安装入口被改名/移走了")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}

	// ① 必须备份旧目录
	if !strings.Contains(body, "backup old plugin dir failed") {
		t.Fatal("★ 安装不再备份旧目录 —— 失败就没法回滚")
	}
	// ② 解包失败必须回滚
	if !strings.Contains(body, "rolled back to") {
		t.Fatal("★ 安装失败时没有回滚 —— 会留下半新半旧的插件目录")
	}
	// ③ 必须走统一解包（而不是就地 zip 展开）
	if !strings.Contains(body, "extractPackage") {
		t.Fatal("★ 安装没有经过 extractPackage —— 路径校验与解包语义会分叉")
	}
}

// ★★★ 判据：路径校验必须遍历**全部**条目（历史上因 break 漏检 rawfile）。
func TestValidatePackageChecksAllEntries(t *testing.T) {
	b, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, "for _, f := range reader.File {\n\t\tif strings.Contains(f.Name, \"..\")") {
		// 允许 gofmt 对齐差异，用更宽松的判定
		if !strings.Contains(src, `strings.Contains(f.Name, "..")`) {
			t.Fatal("★ 包校验没有再检查路径穿越")
		}
	}
	// 关键：这段检查必须**独立于**找 manifest 的循环（不能有 break 在前）
	i := strings.Index(src, `strings.Contains(f.Name, "..")`)
	if i < 0 {
		t.Fatal("找不到路径校验")
	}
	head := src[:i]
	lastLoop := strings.LastIndex(head, "for _, f := range reader.File {")
	if lastLoop < 0 {
		t.Fatal("路径校验不在遍历条目的循环里")
	}
	seg := head[lastLoop:]
	if strings.Contains(seg, "break") {
		t.Fatal("★ 路径校验所在的循环里有 break —— " +
			"排在 manifest 之后的条目（rawfile 恒在其后）会被漏检")
	}
}

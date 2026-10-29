package meta

// SDKModule 与 go.mod 的一致性命中判据。
//
// ## 为什么必须有（2026-10-29 实测缺陷，代价是"模板工程开箱编不出"）
//
// 事实经过：
//
//	go.mod: module github.com/JianFeeeee/hologramagentsdk   ← 已改名
//	meta.SDKModule = "github.com/JianFeeeee/homeagentsdk"   ← 停在中间名
//
// 两者不一致时，`hmapdev init` 生成的工程会**同时**出现两个模块名：
//
//	require github.com/JianFeeeee/homeagentsdk v1.4.0     （来自本常量）
//	而 z_proc_gen.go 里 import .../hologramagentsdk/sdk   （来自 SDK 真实模块名）
//
// 结果 `hmapdev build` 报
//
//	no required module provides package github.com/JianFeeeee/hologramagentsdk/sdk
//
// 而用户照提示执行 `go get` 只会**再拉一份** SDK —— 两个模块名并存，
// 问题原样保留。这类故障的恶劣之处：报错指向 import，真因在 meta.go。
//
// 所以本判据钉死"生成用的模块名 == go.mod 的模块名"。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSDKModuleMatchesGoMod(t *testing.T) {
	// meta/ 的上一级就是 SDK 仓根。
	data, err := os.ReadFile(filepath.Join("..", "go.mod"))
	if err != nil {
		t.Fatalf("读 SDK go.mod 失败: %v", err)
	}

	var mod string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			mod = strings.TrimSpace(line[len("module "):])
			break
		}
	}
	if mod == "" {
		t.Fatal("SDK go.mod 里没有 module 行")
	}

	if SDKModule != mod {
		t.Fatalf("★ meta.SDKModule 与 go.mod 不一致：\n"+
			"  meta.SDKModule = %q\n"+
			"  go.mod module  = %q\n"+
			"两者不一致会让 hmapdev 生成的插件工程同时依赖两个 SDK 模块名，"+
			"表现为 `no required module provides package .../sdk`。"+
			"改 module 名时必须同时改这两处。", SDKModule, mod)
	}

	// 反向保护：LegacySDKModule 不得与当前名相同（否则"识别旧名"失去意义）。
	if LegacySDKModule == SDKModule {
		t.Fatal("LegacySDKModule 与 SDKModule 相同 —— 迁移前的名字应当不同")
	}
}

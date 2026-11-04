package test

// 本文件是 test 包的共享测试辅助（不属于任何单个判据）。
//
// 放在这里而不是某个 *_test.go 里：多个判据文件都要用它，
// 而在"测试与源码分离"之后，它们不能再从包内符号借用。

import (
	"github.com/JianFeeeee/HologramAgent/internal/plugins/webui"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
)

// stripJSComments 剥掉 // 行注释与 /* */ 块注释（粗粒度，但足够用于词频统计）。
//
// 不做完整的 JS 词法分析：本判据只用于「统计某个标识符出现次数」，
// 粗粒度剥离已经足够，且不会误伤字符串里的 //（本文件没有这类内容，
// 若将来有，宁可少剥也不能多剥 —— 多剥会让判据漏报）。
func stripJSComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inLine, inBlock := false, false
	var inStr byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inLine {
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
			continue
		}
		if inBlock {
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if c == '\n' {
			b.WriteByte(c)
			continue
		}
		if inStr != 0 {
			b.WriteByte(c)
			if c == '\\' {
				if i+1 < len(s) {
					b.WriteByte(s[i+1])
					i++
				}
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			// 反引号也是字符串定界符，但它可跨行 —— 本判据只统计标识符，
			// 跨行模板串里出现标识符属正常，此处按普通字符串处理即可。
			inStr = c
			b.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			inLine = true
			i++
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			inBlock = true
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// dashboardJSSource 取出待测的前端脚本原文。
//
// 经导出的 webui.FrontendAsset 读 —— 与"资源存在哪个变量、怎么 embed"
// 完全无关，测试只关心「实际会被发出去的那份文本」。
func dashboardJSSource(t *testing.T) string {
	t.Helper()
	b, err := webui.FrontendAsset("dashboard.js")
	if err != nil {
		t.Fatalf("读 dashboard.js: %v", err)
	}
	return string(b)
}

// dashboardJS 与 dashboardJSSource 同义，保留两个名字是因为
// 原来的判据文件各自用其中一个（迁移时不做无谓重命名）。
func dashboardJS(t *testing.T) string {
	t.Helper()
	return dashboardJSSource(t)
}

// testAPIKey 与 seedWebUIConfig 写进配置的值一致。
const testAPIKey = "test-api-key"

// seedWebUIConfig 在 webui 插件配置里登记并设置 api_key / username / password。
//
// 与包内 handler_test.go 的同名辅助保持一致 —— requireAPI 会读它，
// 没配就返回 503「webui api_key not configured」。
func seedWebUIConfig(reg *internalConfig.ConfigRegistry) {
	c := reg.PluginConfig("webui")
	c.RegisterDef(internalConfig.ConfigDef{Key: "api_key", Default: ""})
	c.RegisterDef(internalConfig.ConfigDef{Key: "username", Default: ""})
	c.RegisterDef(internalConfig.ConfigDef{Key: "password", Default: ""})
	c.Set("api_key", testAPIKey)
	c.Set("username", "admin")
	c.Set("password", "pw-for-test")
}

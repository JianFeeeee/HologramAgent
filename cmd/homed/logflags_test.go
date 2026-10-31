package main

import (
	"log"
	"testing"
)

// TestLogFlagsNoSourceByDefault 钉住「默认日志不带源文件位置」。
//
// 起因（2026-10-31，用户看生产日志时提出）：
//
//	2026/10/31 18:47:03 bootstrap.go:320: [homed] shutting down...
//	2026/10/31 18:47:03 builtin_skill.go:163: [plugin] seq: removed skill ...
//
// 生产日志不需要 `file.go:NNN`：多数日志本身带模块前缀（`[homed]` / `[plugin]` /
// `[kbtree]`），定位能力并不缺；而 Lshortfile 每行都多一次 runtime.Caller。
//
// 判据形状：直接测 logFlags()，因为它在 setupLogging 里被原样交给 log.SetFlags。
// 不去跑 setupLogging 本身——那会建目录、开文件，判据会变重且依赖文件系统。
func TestLogFlagsNoSourceByDefault(t *testing.T) {
	t.Setenv(logSourceEnv, "")

	flags := logFlags()
	if flags&log.Lshortfile != 0 {
		t.Error("默认不得带 Lshortfile（生产日志不该出现 file.go:NNN）")
	}
	if flags&log.Ldate == 0 || flags&log.Ltime == 0 {
		t.Error("日期与时间必须保留：日志没有时间戳就没法关联事件")
	}
}

// TestLogFlagsSourceOptIn 钉住开关本身仍然可用。
//
// 为什么保留它：排查插件/装配问题时，同名文件很多（每个插件都有自己的
// plugin.go），行号是唯一能区分的东西。删掉开关等于把那条排查路径也删了。
func TestLogFlagsSourceOptIn(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " 1 "} {
		t.Setenv(logSourceEnv, v)
		if logFlags()&log.Lshortfile == 0 {
			t.Errorf("%s=%q 应打开 Lshortfile", logSourceEnv, v)
		}
	}
	// 明确关掉的写法不能被当成开
	for _, v := range []string{"0", "false", "off", "no", ""} {
		t.Setenv(logSourceEnv, v)
		if logFlags()&log.Lshortfile != 0 {
			t.Errorf("%s=%q 不应打开 Lshortfile", logSourceEnv, v)
		}
	}
}

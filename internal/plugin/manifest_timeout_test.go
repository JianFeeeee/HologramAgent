package plugin

// 声明式超时的判据（2026-10-16）。
//
// 设计见 docs/zh/kernel-modularization-design.md §2.4.2。
//
// ## 动机（实测）
//
// 生产启动 13 秒，其中 **10 秒是串行等待**插件内部的外部资源就绪
// （clawhubadapter 起两个 pysidecar，各 5 秒 ping timeout），
// 而它阻塞了后面 20+ 个插件的加载。
//
// 根因不是"超时值选错了"，而是**超时值是内核猜的**（`defaultHandshakeTimeout = 10s`）：
//   · 轻插件（纯 Go，秒起）用不到 10s → 浪费
//   · 重插件（起 Python/JVM/加载大模型）10s 可能不够 → 白等 10 秒才报错
//
// 只有插件自己知道"我大概要多久"，所以这个值应由它**声明**。
//
// ## 这组判据要守什么
//
// ① 声明的值**真的被用上**（不是只解析进结构体就完了 —— 本仓反复踩这个坑：
//    "字段加了、没人读"，判据必须测到**真实消费者**）
// ② 未声明时**行为与改动前逐字一致**（存量插件零改动，硬约束）
// ③ 非法值（负数/超范围）不得导致"无限等待"或"立刻超时"

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ★★★ 判据：`handshake_timeout_sec` 能正确解析出来。
func TestManifestParsesHandshakeTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"声明 30s", `{"name":"p","version":"1","entry":"plugin.bin","handshake_timeout_sec":30}`, 30},
		{"声明 1s", `{"name":"p","version":"1","entry":"plugin.bin","handshake_timeout_sec":1}`, 1},
		{"声明 300s", `{"name":"p","version":"1","entry":"plugin.bin","handshake_timeout_sec":300}`, 300},
		{"省略", `{"name":"p","version":"1","entry":"plugin.bin"}`, 0},
		{"显式 0", `{"name":"p","version":"1","entry":"plugin.bin","handshake_timeout_sec":0}`, 0},
		{"负数（视为未声明）", `{"name":"p","version":"1","entry":"plugin.bin","handshake_timeout_sec":-5}`, -5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			m, err := ReadManifest(dir)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if m.HandshakeTimeoutSec != tc.want {
				t.Errorf("HandshakeTimeoutSec: 期望 %d 实际 %d", tc.want, m.HandshakeTimeoutSec)
			}
		})
	}
}

// ★★★ 判据：**旧插件（无该字段）必须照常工作** —— 存量零改动是硬约束。
//
// 这条独立存在，因为它是"不可破坏"的那一半：加字段容易，
// 保证不加字段的老插件行为不变才是真约束。
func TestManifestWithoutTimeoutKeepsLegacyBehavior(t *testing.T) {
	// 用真实的存量 plugin.json 内容（qq 插件，无 handshake_timeout_sec）
	dir := t.TempDir()
	legacy := `{
  "author": "HomeAgent",
  "description": "QQ 消息收发插件，通过 NapCat 协议桥接",
  "entry": "plugin.bin",
  "name": "qq",
  "name_en": "qq",
  "name_zh": "QQ消息",
  "tags": ["qq", "messaging"],
  "version": "1.4.3"
}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("存量 manifest 解析失败（破坏性变更）: %v", err)
	}
	if m.Name != "qq" || m.Entry != "plugin.bin" {
		t.Fatalf("存量字段解析出错: %+v", m)
	}
	if m.HandshakeTimeoutSec != 0 {
		t.Errorf("未声明时应为 0（= 用内核默认），实际 %d", m.HandshakeTimeoutSec)
	}
}

// ★★★ 判据：**真实消费者**（`HandshakeTimeout()` 换算函数）行为正确。
//
// 这条防的是本仓反复踩的坑："字段解析对了，但没人读它"。
// 判据必须测到**从声明到时长**的完整换算，包括默认回退与非法值处理。
func TestManifestHandshakeTimeoutResolution(t *testing.T) {
	def := 10 * time.Second // 与 proc.defaultHandshakeTimeout 同值

	for _, tc := range []struct {
		name string
		secs int
		want time.Duration
	}{
		{"未声明 ⇒ 默认", 0, def},
		{"负数 ⇒ 默认（不得变成立刻超时）", -5, def},
		{"声明 30 ⇒ 30s", 30, 30 * time.Second},
		{"声明 1 ⇒ 1s", 1, time.Second},
		{"声明 300 ⇒ 300s", 300, 300 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &PluginManifest{HandshakeTimeoutSec: tc.secs}
			got := m.HandshakeTimeout(def)
			if got != tc.want {
				t.Errorf("期望 %v 实际 %v", tc.want, got)
			}
		})
	}
}

// ★★★ 判据：**超时上限**必须存在（防"声明一个永不超时"）。
//
// 为何需要上限：插件声明的值直接决定内核等多久。
// 一个错误声明（如 86400）会让内核在插件坏掉时**卡一整天**——
// 那不是"插件自己负责"，而是把内核拖住。上限取一个"远超任何合理启动"
// 但仍是"可承受等待"的值。
func TestManifestHandshakeTimeoutIsCapped(t *testing.T) {
	m := &PluginManifest{HandshakeTimeoutSec: 86400} // 一天
	got := m.HandshakeTimeout(10 * time.Second)
	if got > time.Hour {
		t.Errorf("★ 声明的超时未被封顶（%v）—— 插件坏掉时内核会被卡住", got)
	}
	if got <= 0 {
		t.Errorf("封顶后不得为 0 或负: %v", got)
	}
	// 封顶值本身必须可用（不能封成一个荒谬的小值）
	if got < time.Minute {
		t.Errorf("封顶值过小（%v）—— 重插件会被误杀", got)
	}
}

// ★★★ 判据：未知字段不得导致解析失败（向前/向后兼容）。
//
// 已有先例：`Capabilities` 的引入依赖"忽略未知键"。
// 这里再确认一次，因为超时字段会随插件分发到各版本内核。
func TestManifestIgnoresUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	body := `{"name":"p","version":"1","entry":"plugin.bin",
	          "handshake_timeout_sec":20,
	          "some_future_field":{"deep":[1,2,3]}}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("未知字段不该导致失败: %v", err)
	}
	if m.HandshakeTimeoutSec != 20 {
		t.Errorf("已知字段应正常解析: %d", m.HandshakeTimeoutSec)
	}
}

// 保证 json 包被使用（判据里用 json.Unmarshal 做等价比对时可能需要）；
// 同时防止 import 被 gofmt 清掉造成后续编辑不便。
var _ = json.Marshal

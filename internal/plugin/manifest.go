package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

const PackageExt = ".hmap"

// SDKProxyDef 是 SDK 反代声明在本包的别名（避免调用方两处 import）。
type SDKProxyDef = pubsdk.ProxyDef

// PluginManifest 每个插件目录中的 plugin.json 元数据。
type PluginManifest struct {
	Name        string   `json:"name"`
	NameZh      string   `json:"name_zh,omitempty"`
	NameEn      string   `json:"name_en,omitempty"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	Author      string   `json:"author,omitempty"`
	License     string   `json:"license,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Repository  string   `json:"repository,omitempty"`
	Entry       string   `json:"entry"`               // "plugin.bin"(子进程) | "main.lua" | "SKILL.md"
	Platforms   []string `json:"platforms,omitempty"` // 声明的支持平台: ["linux","darwin","windows"]
	MinVersion  string   `json:"min_version,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Deprecated  bool     `json:"deprecated,omitempty"`

	// HandshakeTimeoutSec 是本插件**自行声明**的建链（握手）超时，单位秒。
	//
	// 省略或 <=0 时用内核默认值（proc.defaultHandshakeTimeout，10s）。
	//
	// ## 为何要让插件自己声明
	//
	// 实测（2026-10-16）：生产启动 13 秒，其中 **10 秒是串行等待**插件内部的
	// 外部资源就绪（clawhubadapter 起两个 pysidecar，各 5 秒 ping timeout），
	// 而它阻塞了后面 20+ 个插件的加载。
	//
	// 根因不是“超时值选错了”，而是**超时值是内核猜的**：
	//   · 轻插件（纯 Go，秒起）用不到 10s
	//   · 重插件（要起 Python/JVM/加载大模型）10s 可能不够，
	//     于是它会“失败”且失败得很晚（白等 10 秒才报错）
	// 只有插件自己知道“我大概要多久”，所以这个值应由它声明。
	//
	// ## 为什么不叫 Timeout 而叫 HandshakeTimeout
	//
	// 插件内部等待**自己的子资源**（如 pysidecar ping）不属于 SDK 契约，
	// 那是插件自己的实现细节（内置插件应走自己的 ConfigDef）。
	// 本字段只管**内核与插件之间**的建链——那才是内核能干预的边界。
	HandshakeTimeoutSec int `json:"handshake_timeout_sec,omitempty"`

	// RequiresFacilities 声明本插件需要**内核哪些设施就绪**后才能启动。
	//
	// 这决定插件的**启动时机**（设计 docs/zh/kernel-modularization-design.md §2.6）：
	//   · **不声明（本字段缺省）** ⇒ 等内核**完全**初始化完成后拉起 ← **现行为**
	//   · 声明如 ["config","llm"]   ⇒ 这些设施一就绪就拉起（**尽早启动**）
	//
	// ## ★ 为何"不声明"必须等于现行为（存量插件零改动）
	//
	// 生产有 42 个外部插件，它们全都不声明 ⇒ **启动时机逐字不变**。
	// 若"不声明"被当成"无依赖/依赖已满足"，它们会在第 0 层完成后就被拉起，
	// 早于现在得多 ⇒ 42 个插件里任何一个若假设"我 Start 时内核已就绪"，
	// 就会出现**静默竞态**（系统看着正常，偶发才炸）。
	//
	// 所以判据里有一条专门守这个：未声明的插件在内核完成前**不得**被拉起。
	//
	// ## 设施名从哪来
	//
	// 内核模块在 `Provides()` 里声明（见 internal/kernelboot）：
	// config / crypto / keystore / memory / lua / llm / tracker / textmem /
	// docmem / knowledge / multimodal / stagehost / agent ...
	//
	// 拼错的名字**不会**静默忽略：它永远等不到就绪 ⇒ 插件最终由
	// 「内核完成 + 超时兜底」拉起，并留下"还差 X"的日志（见调度器 WaitString）。
	RequiresFacilities []string `json:"requires_facilities,omitempty"`

	// AutoStart 声明是否**伴随内核启动**（true/缺省）还是等显式启用（false）。
	//
	// 用 *bool 而非 bool，因为要区分三种情形：
	//   · 缺省（nil）  ⇒ 视为 true（伴随启动）← 与现状一致
	//   · 显式 true    ⇒ 伴随启动
	//   · 显式 false   ⇒ **不**伴随启动，等 agent 经 plugin_enable 启用
	//
	// ★ 为何不能用普通 bool：JSON 里 bool 的零值是 false，
	// 于是"没写这个字段"与"写了 false"**无法区分**，
	// 前者会被误判成"不自动启动"⇒ 存量插件全都不启动了（灾难性）。
	// 这个坑与本仓 handshake_timeout_sec 用 int 而非 *int 是同一类；
	// 那里能接受（0 与"未声明"语义相同），这里**不能**。
	AutoStart *bool `json:"auto_start,omitempty"`

	// Proxies 声明本插件需要 HomeAgent 反代出去的服务（自带 Web UI / HTTP API）。
	//
	// 契约定义在公开 SDK（sdk.ProxyDecl），这里只做载体：HomeAgent 加载插件时
	// 读取并聚合，按 Host 子域路由从 webui 的同一端口转发出去。**不声明 = 不被反代**。
	//
	// 字段解析忽略未知键（本仓无 DisallowUnknownFields），因此加这个字段
	// 对「旧内核读新插件」与「新内核读旧插件」都是无害的。
	Proxies []SDKProxyDef `json:"proxies,omitempty"`

	// Capabilities 声明本插件需要的内核能力组（§3.8 权限梯度）。
	//
	// 取值见 internal/plugin/proc.KnownCapabilities()：
	// io / memory / doc_memory / knowledge / text_memory / llm / social /
	// events / plugin_mgr / settings_cross
	//
	// **省略或为空 = 不受限**，而不是「只有基础能力」。
	// 理由：17 个存量插件的 plugin.json 都没有这个字段，若空声明当作最小权限，
	// 它们会全部失去 IO 注入、记忆读写等能力而**静默降级**——
	// 违反「外部插件零改动」的硬约束。收紧的路径是让插件显式声明。
	//
	// core（注册自身工具/阶段/通道 + 读写自己的配置）无需声明，始终可用。
	Capabilities []string `json:"capabilities,omitempty"`
}

// ShouldAutoStart 返回本插件是否应伴随内核启动（缺省 = true）。
//
// 与 RequiresFacilities 配合决定启动时机；两者正交：
//
//	· AutoStart=false  ⇒ 无论依赖是否就绪都**不**自动拉起
//	· AutoStart=true（默认）+ 声明依赖   ⇒ 依赖齐了就拉起
//	· AutoStart=true（默认）+ 不声明依赖 ⇒ 等内核完全初始化
func (m *PluginManifest) ShouldAutoStart() bool {
	if m == nil || m.AutoStart == nil {
		return true // ★ 缺省即伴随启动（存量插件零改动）
	}
	return *m.AutoStart
}

// HandshakeTimeout 把声明的秒数换算成时长，并施加**上限**。
//
// ## 默认回退（存量插件零改动的硬约束）
//
// 未声明（0）或非法（负）⇒ 返回 def（调用方传 proc.DefaultHandshakeTimeout）。
//
// ★ 为何负数也回退默认而不是报错：“0 表示未声明”是 JSON 的常见约定，
// 而手写配置里出现 `-1` 很可能只是“想表达无/未设”的笔误。
// 若因此拒绝加载整个插件，代价远大于“按默认值处理”。
// 但**绝不**把负数直接当时长用（那会变成“立即超时”，有隐蔽的破坏性）。
//
// ## 为何必须有上限
//
// 插件声明的值直接决定**内核**等多久。一个错误声明（如 86400 = 一天）
// 会让内核在插件坏掉时**卡一整天**——那不是“插件自己负责”，
// 而是把内核拖住（而插件加载是启动路径，会阻塞后续插件）。
//
// 上限取 5 分钟：远超任何合理的插件启动耗时（实测最重的也只需数十秒），
// 但仍在“可承受的人工等待”范围内（远小于“一天”）。
func (m *PluginManifest) HandshakeTimeout(def time.Duration) time.Duration {
	if def <= 0 {
		def = proc.DefaultHandshakeTimeout
	}
	if m == nil || m.HandshakeTimeoutSec <= 0 {
		return def
	}
	d := time.Duration(m.HandshakeTimeoutSec) * time.Second
	if d > maxHandshakeTimeout {
		return maxHandshakeTimeout
	}
	return d
}

// maxHandshakeTimeout 是插件可声明的建链超时上限。
//
// 见 HandshakeTimeout 的说明：没有上限时，一个错误声明能把内核卡住。
const maxHandshakeTimeout = 5 * time.Minute

func ReadManifest(dir string) (*PluginManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return nil, err
	}
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// IsPluginDir 判断目录是否为有效的插件目录（包含 plugin.json）
func IsPluginDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "plugin.json"))
	return err == nil
}

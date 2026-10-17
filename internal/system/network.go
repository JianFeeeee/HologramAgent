// Package system 提供 L0/L1 层的系统级网络配置基线：捕获与还原本机的
// DNS（resolv.conf）/ hosts / 代理环境，供 failback 在救援源恢复前还原被主 agent
// 改坏的网络配置。
package system

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// NetworkBaseline 一张系统网络配置快照：捕获时点 + DNS/hosts/代理环境原文。
type NetworkBaseline struct {
	CapturedAt string            `json:"captured_at"`
	ResolvConf string            `json:"resolv_conf,omitempty"` // /etc/resolv.conf 原文
	Hosts      string            `json:"hosts,omitempty"`       // /etc/hosts 原文
	ProxyEnv   map[string]string `json:"proxy_env,omitempty"`   // http_proxy/https_proxy/no_proxy 等
}

const baselineFileName = "network_baseline.json"

// relevantPath 本机网络相关文件，与代理环境变量一同纳入基线。
// 默认值面向主流 Linux 发行版（systemd-resolved 等也写 /etc/resolv.conf）。
// 若目标发行版布局不同（如 resolv.conf 在 /run/systemd/resolve/），由
// SetNetworkPaths 在启动时注入，避免硬编码失效。
var relevantPath = []string{
	"/etc/resolv.conf",
	"/etc/hosts",
	"/etc/environment",
}

// SetNetworkPaths 覆盖纳入网络基线的文件路径列表（发行版适配入口）。
// resolv.conf / hosts 用于还原与"被改动"判定；其余文件仅捕获（诊断用）。
// 传 nil/空则恢复 Linux 默认。
func SetNetworkPaths(paths []string) {
	if len(paths) == 0 {
		relevantPath = []string{"/etc/resolv.conf", "/etc/hosts", "/etc/environment"}
		return
	}
	ns := make([]string, 0, len(paths))
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			ns = append(ns, p)
		}
	}
	relevantPath = ns
}

// NetworkPaths 返回当前纳入基线的网络文件路径（副本）。
func NetworkPaths() []string {
	out := make([]string, len(relevantPath))
	copy(out, relevantPath)
	return out
}

// envProxyKeys 纳入基线的代理相关环境变量（大小写归一，捕获时同时保存小写与大写形式）。
var envProxyKeys = []string{
	"http_proxy", "https_proxy", "no_proxy",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"all_proxy", "ALL_PROXY",
}

// fileKey 映射路径 → 基线字段。已知可还原字段：resolv_conf / hosts。
func fileKey(p string) string {
	switch {
	case strings.Contains(p, "resolv"):
		return "resolv"
	case strings.Contains(p, "hosts"):
		return "hosts"
	}
	return ""
}

// CaptureNetwork 捕获当前系统网络配置成基线对象（不落盘）。
func CaptureNetwork() (*NetworkBaseline, error) {
	b := &NetworkBaseline{
		CapturedAt: time.Now().Format(time.RFC3339),
		ProxyEnv:   make(map[string]string),
	}
	for _, p := range relevantPath {
		if data, err := os.ReadFile(p); err == nil {
			switch fileKey(p) {
			case "resolv":
				b.ResolvConf = string(data)
			case "hosts":
				b.Hosts = string(data)
			default:
				if b.ProxyEnv == nil {
					b.ProxyEnv = make(map[string]string)
				}
				b.ProxyEnv["file:"+p] = string(data)
			}
		}
	}
	for _, k := range envProxyKeys {
		if v, ok := os.LookupEnv(k); ok {
			b.ProxyEnv[k] = v
		}
	}
	return b, nil
}

// SaveNetworkBaseline 把网络基线写入 <dataDir>/network_baseline.json。
func SaveNetworkBaseline(dataDir string, b *NetworkBaseline) error {
	path := NetworkBaselinePath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadNetworkBaseline 从 <dataDir>/network_baseline.json 读回网络基线。
func LoadNetworkBaseline(dataDir string) (*NetworkBaseline, error) {
	path := NetworkBaselinePath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b NetworkBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// NetworkBaselinePath 返回网络基线文件路径。
func NetworkBaselinePath(dataDir string) string {
	return filepath.Join(dataDir, "recovery", baselineFileName)
}

// ApplyProxyEnv 把基线中的代理环境变量写回当前进程环境（guard 在拉起 worker 前调用，
// 使 worker 继承干净代理）。
func (b *NetworkBaseline) ApplyProxyEnv() {
	for _, k := range envProxyKeys {
		if v, ok := b.ProxyEnv[k]; ok {
			os.Setenv(k, v)
		}
	}
}

// ExtraProxyKeys 返回基线中额外捕获的键（如 ENVIRONMENT），排序后供诊断/报告使用。
func (b *NetworkBaseline) ExtraProxyKeys() []string {
	var keys []string
	for k := range b.ProxyEnv {
		found := false
		for _, p := range envProxyKeys {
			if k == p {
				found = true
				break
			}
		}
		if !found {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// restoreTargets 返回可还原的网络文件（键→基线内容）。由注入路径派生，
// 适配不同发行版的 resolv.conf/hosts 位置。
func (b *NetworkBaseline) restoreTargets() map[string]string {
	m := map[string]string{}
	for _, p := range relevantPath {
		switch fileKey(p) {
		case "resolv":
			if b.ResolvConf != "" {
				m[p] = b.ResolvConf
			}
		case "hosts":
			if b.Hosts != "" {
				m[p] = b.Hosts
			}
		}
	}
	return m
}

// RestoreFiles 把基线中的 resolv.conf / hosts 写回原路径。仅当该路径被主 agent
// 改动过（当前内容与基线不同）时才覆盖，返回改动项列表。
func (b *NetworkBaseline) RestoreFiles() ([]string, error) {
	var changed []string
	for path, want := range b.restoreTargets() {
		cur, err := os.ReadFile(path)
		if err != nil || string(cur) == want {
			continue
		}
		if err := os.WriteFile(path, []byte(want), 0644); err != nil {
			return changed, fmt.Errorf("restore %s: %w", path, err)
		}
		changed = append(changed, path)
	}
	return changed, nil
}

// MismatchedFiles 返回与基线不一致的系统网络文件（guard 判定"网络是否被破坏"）。
// 返回名字列表；无则空。忽略读取失败的文件。
func (b *NetworkBaseline) MismatchedFiles() []string {
	var out []string
	for path, want := range b.restoreTargets() {
		if cur, err := os.ReadFile(path); err != nil {
			out = append(out, path+"(unreadable)")
		} else if string(cur) != want {
			out = append(out, path+"(modified)")
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// IsNetworkMismatch 判断主机网络配置与基线是否不一致（resolv.conf/hosts 中任一改动）。
func (b *NetworkBaseline) IsNetworkMismatch() bool {
	return len(b.MismatchedFiles()) > 0
}

// Summary 生成基线摘要（用于 failback 报告）。
func (b *NetworkBaseline) Summary() string {
	parts := []string{fmt.Sprintf("captured=%s", b.CapturedAt)}
	if b.ResolvConf != "" {
		parts = append(parts, fmt.Sprintf("resolv=%dbytes", len(b.ResolvConf)))
	}
	if b.Hosts != "" {
		parts = append(parts, fmt.Sprintf("hosts=%dbytes", len(b.Hosts)))
	}
	if n := len(b.ProxyEnv); n > 0 {
		parts = append(parts, fmt.Sprintf("proxy_vars=%d", n))
	}
	return strings.Join(parts, " ")
}

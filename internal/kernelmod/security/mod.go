// Package security 是 security 模块：**安全防火墙**（Secret Firewall + 边界闸门）。
//
// ## 它是什么（用户裁定 2026-10-26）
//
// > 安全模块不是独立的模块吗？core 发出的请求和 device 返回的请求都会
// > 经过安全模块审查，通过才能进行下一步，否则会被拦截 —— 相当于安全模块
// > 就是横在其他所有模块与 core 之间的一堵墙。
//
// 所以本模块不是"某项能力"，而是**数据面边界**：
//
//	Core ──出向审查──▶ 外部（device / plugin / channel）
//	Core ◀─入向审查── 外部
//
// 落地形态是**显式代理**（用户确认的说法只是方便解耦）：把 KernelState 上
// 的公共挂载点换成本模块产出的安全对象，业务模块继续认为自己直接对端交互。
//
// ## 依赖与分层位置
//
//	config → security → memory / knowledge / agent / device / pluginhost
//
// 只依赖 config：它需要 `core.security.enabled` 与 `ks.DataDir`。
// ★ 刻意**不依赖 agent**：agent 之后要用 `ks.Security`，反之成环。
//
// ## 与 Group 0 的关系（重要）
//
// 密钥与变量库必须在**任何读 core.llm.* 的操作之前**就绪（config 模块的
// 注释记录了 401 invalid api key 的实测故障）。而读 `core.llm.*` 发生在
// `cfgReg.ToConfig()`（Group 0）里，**早于**本调度器。
//
// ⇒ 本模块把密钥与 Store 的装配放进 Init，但**同时**提供
// `Prepare()` 供 Group 0 在 ToConfig 之前调用（幂等）。
// 这正是原先 cmd/homed 里那段"必须前移"的代码的归宿。
package security

import (
	"fmt"
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	sec "github.com/JianFeeeee/HologramAgent/internal/security"
)

// Module 提供安全防火墙设施。
type Module struct{}

func (Module) Name() string       { return "security" }
func (Module) Requires() []string { return []string{kernelmod.FacConfig} }
func (Module) Provides() []string { return []string{kernelmod.FacSecurity} }

func (Module) Init(ks *kernelboot.KernelState) error {
	if ks.DataDir == "" {
		return fmt.Errorf("security module: ks.DataDir is empty - Group 0 must set the data dir first")
	}
	if ks.CfgReg == nil {
		return fmt.Errorf("security module: ks.CfgReg is nil (requires the config facility)")
	}
	rt, err := Prepare(ks.DataDir, ks.CfgReg)
	if err != nil {
		return err
	}
	ks.Security = rt
	if ks.EventBus != nil && rt.Enabled() {
		ks.EventBus.Publish(secReadyEvent(rt))
	}
	return nil
}

func init() { kernelmod.Register(Module{}) }

// Prepare 装配安全运行时（**幂等**）。
//
// 可在 Group 0（`cfgReg.ToConfig()` 之前）先调一次，也可以由模块 Init 调。
// 第二次调用直接返回已装好的实例 —— 因为 `InitGlobal` 会重读并重建变量库，
// 而那会在 ToConfig 已经解出密文之后把 Store 换掉，徒增风险。
//
// 返回的 Runtime 里带着 Store 与 Gate（边界闸门）。
func Prepare(dataDir string, cfgReg *config.ConfigRegistry) (*sec.Runtime, error) {
	if rt := sec.RuntimeGlobal(); rt != nil {
		return rt, nil
	}
	if cfgReg == nil {
		return nil, fmt.Errorf("security: cfgReg is nil")
	}
	if !cfgReg.GetBool("core.security.enabled", true) {
		// 显式关闭：装一个空 Runtime（所有闸门零成本透传），
		// 而不是留 nil 让调用点各自判断。
		rt := sec.NewDisabledRuntime()
		sec.SetRuntimeGlobal(rt)
		log.Printf("[security] module disabled by config (core.security.enabled=false)")
		return rt, nil
	}

	// ★ 先收敛敏感文件权限，再读密钥。
	//
	// 顺序有意为之：内嵌主密钥把密钥放进了 0755 的二进制里，而 config.db
	// 出厂是 0644 ——「能读二进制就能解密数据库」。收紧到 0600/0750 是本机
	// 缓解手段，必须在服务开始读写之前生效。失败不致命（权限问题不该让服务
	// 不可用），但要逐条响亮记录。
	if hr := sec.HardenDataDir(dataDir); len(hr.Changed) > 0 {
		log.Printf("[security] hardened %d file permissions (necessary mitigation for embedded-key scenario): %v",
			len(hr.Changed), hr.Changed)
	}

	key, keySrc, err := sec.LoadOrCreateKey(dataDir)
	if err != nil {
		return nil, fmt.Errorf("security key init failed (refusing to start in degraded mode): %w", err)
	}
	oldKeys, err := sec.LoadKeyring(dataDir, key)
	if err != nil {
		return nil, fmt.Errorf("security keyring load failed (refusing to silently pick wrong key): %w", err)
	}
	// ★ 内嵌钥上台时，磁盘钥必须继续参与**解密**。
	// 否则用磁盘钥加密过的老机器升级后会全部解不开 ⇒ 服务起不来；
	// 用户此时若"重建库"，凭据一次性全丢 —— 恰好与内嵌的初衷相反。
	if extra := sec.AdditionalDecryptKeys(dataDir, key); len(extra) > 0 {
		oldKeys = append(oldKeys, extra...)
	}
	if len(oldKeys) > 0 {
		log.Printf("[security] loaded %d historical/extra key(s) (smooth rotation + embedded migration)", len(oldKeys))
	}

	store, err := sec.InitGlobal(sec.Options{
		Path:    filepath.Join(dataDir, "security", "variables.json"),
		KeyFn:   func() ([]byte, error) { return key, nil },
		OldKeys: oldKeys,
		Enabled: true,
	})
	if err != nil {
		return nil, fmt.Errorf("security variable store load failed (will not silently rebuild): %w", err)
	}

	// 配置落盘用的子密钥（域隔离：与别名密钥不共用）。
	if _, err := sec.InitConfigCodecWithOldKeys(key, oldKeys); err != nil {
		return nil, fmt.Errorf("config-encryption subkey derivation failed: %w", err)
	}
	// 把库里仍是明文的敏感项就地加密（幂等；升级路径必须跑）。
	if n, merr := cfgReg.EncryptExistingSecrets(); merr != nil {
		log.Printf("[security] existing sensitive config encryption incomplete (some entries still plaintext): %v", merr)
	} else if n > 0 {
		log.Printf("[security] encrypted %d existing sensitive config entries", n)
	}

	// E-2：备份跨机器恢复的诊断（症状极具误导性，一次性说清事实）。
	if okN, badN, aerr := cfgReg.AuditSecrets(); aerr == nil && badN > 0 {
		log.Printf("[security] WARNING: %d ciphertext entries cannot be decrypted with current master key (%d decryptable) — "+
			"likely a config.db restored from another machine without its master key", badN, okN)
	}

	rt := sec.NewRuntime(store)
	sec.SetRuntimeGlobal(rt)
	log.Printf("[security] module enabled (key source %s, %d variables, config-at-rest encryption on)",
		keySrc, store.Count())
	return rt, nil
}

// secReadyEvent 构造"安全防火墙已装配"事件。
//
// 为什么带 enabled 与 variables：状态面要能回答"审查是否真的在生效"，
// 而"配置开了但装配失败"与"配置显式关闭"是两件不同的事。
func secReadyEvent(rt *sec.Runtime) *events.Event {
	return &events.Event{
		Type:   events.EventSecurityReady,
		Source: "kernelmod/security",
		Payload: map[string]interface{}{
			"enabled":   rt.Enabled(),
			"variables": rt.Count(),
		},
	}
}

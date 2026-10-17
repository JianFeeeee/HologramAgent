//go:build linux

// L1 failback 恢复 worker（Linux 专属）：在受限 boot 下执行恢复梯子，
// 依赖 DNS/代理网络基线、LLM 配置快照与 reload 等 Linux 语义。
// 非 Linux 平台不编译，由 guard_windows.go 提供桩。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	luaVM "github.com/JianFeeeee/HomeAgent/internal/lua"
	"github.com/JianFeeeee/HomeAgent/internal/recovery"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/internal/system"
)

// runFailbackRecovery 在 failback worker 内执行 L1 恢复梯子：
//
//	probe(rescue 源) → 还原 DNS/proxy → probe → 还原 LLM 配置快照+ReloadFromConfig → probe
//
// 成功后写 recovery_result.json 并以 exitRecovered 退出；失败以 exitRecoveryFailed 退出。
// 全程确定性、无 token 消耗（除 rescue 源 QuickChat 复检外）。
func runFailbackRecovery(dataDir string, cfgReg *internalConfig.ConfigRegistry, lua *luaVM.VM, providerMgr *agentAPI.ProviderManager, baseAPIKey string) {
	applySystemConfig(loadGuardConfig(dataDir)) // 发行版/部署路径适配：guard.yaml system.*
	task, err := recovery.LoadTask(recovery.TaskPath(dataDir))
	if err != nil {
		log.Printf("[failback] no recovery task, skipping: %v", err)
		os.Exit(exitRecoveryFailed)
	}
	if task.RescueSource.Name == "" || task.RescueSource.BaseURL == "" {
		log.Printf("[failback] rescue source missing in task, cannot recover")
		os.Exit(exitRecoveryFailed)
	}
	log.Printf("[failback] recovery task: attempt %d/%d rescue=%s", task.Attempt, task.MaxAttempts, task.RescueSource.Name)

	timeout := 120 * time.Second
	if d, err := time.ParseDuration(task.AttemptTimeout); err == nil && d > 0 {
		timeout = d
	}

	// 注册 rescue 源并设为默认（锚定 IP 直连，绕开被破坏的 DNS/代理）
	key := task.RescueSource.APIKey
	if key == "" {
		key = baseAPIKey
	}
	rescueProvider := agentAPI.NewLuaAdaptedProvider(agentAPI.BaseConfig{
		Model:         task.RescueSource.Model,
		BaseURL:       task.RescueSource.BaseURL,
		APIKey:        key,
		Temperature:   0.7,
		MaxTokens:     512,
		ContextWindow: 8192,
	}, lua, task.RescueSource.Name, task.RescueSource.Adapter)
	providerMgr.Register("rescue", rescueProvider)
	if err := providerMgr.SetDefault("rescue"); err != nil {
		log.Printf("[failback] set rescue default: %v", err)
	}

	probe := func(ctx context.Context, label string) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		resp, err := providerMgr.QuickChat(ctx, "回复 OK")
		if err != nil {
			log.Printf("[failback] probe(%s): %v", label, err)
			return false, nil
		}
		log.Printf("[failback] probe(%s): reachable, reply=%q", label, truncateStr(resp.Content, 60))
		return true, nil
	}

	restoreNet := func() ([]string, error) {
		base, err := system.LoadNetworkBaseline(dataDir)
		if err != nil {
			return nil, fmt.Errorf("load network baseline: %w", err)
		}
		changed, err := base.RestoreFiles()
		if err != nil {
			return nil, err
		}
		if len(changed) > 0 {
			log.Printf("[failback] restored network files: %v", changed)
		}
		return changed, nil
	}

	restoreCfg := func() (int, error) {
		snapPath := filepath.Join(dataDir, "llm_snapshot.json")
		snap, err := internalConfig.LoadLLMSnapshot(snapPath)
		if err != nil {
			return 0, fmt.Errorf("load llm snapshot: %w", err)
		}
		if err := cfgReg.RestoreCoreLLM(snap); err != nil {
			return 0, err
		}
		if err := sdk.NewLLM(providerMgr, cfgReg, lua, baseAPIKey).ReloadFromConfig(); err != nil {
			return 0, err
		}
		log.Printf("[failback] restored %d llm keys and reloaded from config", len(snap))
		return len(snap), nil
	}

	ladder := &recovery.Ladder{
		ResultFile:     recovery.ResultPath(dataDir),
		Attempt:        task.Attempt,
		Probe:          probe,
		RestoreNetwork: restoreNet,
		RestoreConfig:  restoreCfg,
		Log:            log.Printf,
	}
	res := ladder.Run(context.Background())
	log.Printf("[failback] recovery round result: %s", res.Quote())

	if res.Success {
		os.Exit(exitRecovered)
	}
	os.Exit(exitRecoveryFailed)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// lastDiagSummary 读取最近一次 recovery_result 生成一行自诊断摘要（worker IPC ACK 上报）。
func lastDiagSummary(dataDir string) string {
	res, err := recovery.LoadResult(recovery.ResultPath(dataDir))
	if err != nil {
		return ""
	}
	return res.Quote()
}

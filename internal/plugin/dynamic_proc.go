package plugin

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 本文件平台中立。
//
// 曾经拆成 dynamic_proc_unix.go + dynamic_proc_windows.go（Part 1，610e9d0），
// 当时共享内存只有 POSIX mmap 实现，故 Windows 侧只放了个报「尚未实现」的桩。
// 但那个桩只定义了 tryLoadProc，而平台中立的 registry.go 还在调 loadProc /
// closeProcHost —— **Windows 下整个 homed 从那时起就编译不过**
// （历史核实：曾有「交叉编译通过」的说法，但实际只验证了 proc 子包）。
//
// Part 6.2（d027c96）补齐了 Windows 共享内存（CreateFileMappingW）、事件通知
// （CreateEventW）与段传递（命名对象经环境变量），桩却没人回头删。
//
// 现在合回一个文件：本文件里没有任何平台专属调用，全部差异封装在 proc 包的
// shmalloc_* / evtfd_* / shmpass_* / procattr_* 里，那些文件各自带构建标签。

// tryLoadProc 只做静态校验（供双通道探测与测试），不构造插件实体。
//
// 真正加载走 Registry.loadProc：子进程插件需要共享段 Host，
// 而 Host 必须是**全部 .bin 插件共用的那一个**，只能由 Registry 持有。
//
// 返回 nil,nil 表示目录中没有 plugin.bin（交由后续探测通道）；
// 找到二进制但不可用时返回明确错误——不静默回退到 cabi。
func tryLoadProc(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	_, err := validateProcBinary(dir, name)
	return nil, err
}

// 子进程插件加载（plugin.bin）——外部插件多进程化的加载入口。
//
// 三个通信面：stdio JSON-RPC（控制）+ 共享内存段（数据）+ eventfd 通知环。
// 设计取舍见 assets/docs/zh/ARCHITECTURE.md「子进程插件的三个通信面」。

// validateProcBinary 校验 plugin.bin 是否存在且可执行。
// 返回 ("", nil) 表示该目录不是 proc 插件。
func validateProcBinary(dir, name string) (string, error) {
	binPath := filepath.Join(dir, binEntry)
	st, err := os.Stat(binPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("proc plugin %s: 检查 %s: %w", name, binEntry, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("proc plugin %s: %s 是目录，不是可执行文件", name, binEntry)
	}
	if st.Mode()&0o111 == 0 {
		// 常见于经 zip/hmap 分发丢失权限位——给出可直接执行的修复指令
		return "", fmt.Errorf("proc plugin %s: %s 缺少可执行权限（chmod +x %s）",
			name, binEntry, binPath)
	}
	return binPath, nil
}

// loadProc 构造子进程插件实体（不 spawn）。
//
// 共享段 Host 在此惰性创建：**全部 .bin 插件共用一块段**。
// 若每插件一段，多插件同阶段并发时会退化成副本模型，
// lost update 原样复现（§8.4 实测 35.8~36.8%）。
func (r *Registry) loadProc(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	binPath, err := validateProcBinary(dir, name)
	if err != nil {
		return nil, err
	}
	if binPath == "" {
		return nil, nil
	}

	host, err := r.ensureProcHost()
	if err != nil {
		return nil, fmt.Errorf("proc plugin %s: %w", name, err)
	}

	// manifest 声明的能力集（§3.8 权限梯度）。
	// 无 manifest 或未声明 capabilities 时不限制，保存存量插件行为。
	var caps []string
	// ★ 建链超时由插件**自行声明**（见 docs/zh/kernel-modularization-design.md §2.4.2）。
	//
	// 为何要读它而不是用内核默认 10s：实测生产启动 13 秒里 10 秒是串行等待
	// 插件内部的重型初始化，而内核"猜"的 10s 对重插件不够、对轻插件浪费。
	// 未声明时为 0 ⇒ HandshakeTimeout 回退到 proc.DefaultHandshakeTimeout，
	// 与改动前**逐字一致**（存量插件零改动的硬约束）。
	hsTimeout := time.Duration(0)
	if mft := readManifest(dir); mft != nil {
		caps = mft.Capabilities
		if len(caps) > 0 {
			log.Printf("[plugin] %s 声明能力: %v", name, caps)
		}
		hsTimeout = mft.HandshakeTimeout(proc.DefaultHandshakeTimeout)
		if mft.HandshakeTimeoutSec > 0 {
			log.Printf("[plugin] %s 声明建链超时 %s（内核默认 %s）",
				name, hsTimeout, proc.DefaultHandshakeTimeout)
		}
	}

	return procPluginAdapter{
		Plugin: proc.NewWithTimeout(name, binPath, dir, config, host, r.onProcCrash, hsTimeout, caps...),
	}, nil
}

// ensureProcHost 惰性创建共享段 Host（全进程唯一）。
func (r *Registry) ensureProcHost() (*proc.Host, error) {
	r.procHostMu.Lock()
	defer r.procHostMu.Unlock()
	if r.procHost != nil {
		return r.procHost, nil
	}
	host, err := proc.NewHost()
	if err != nil {
		return nil, err
	}
	r.procHost = host

	// 事件环适配层：Bus 发布 → 写 EvtRing slot → eventfd 通知子进程
	if bus := r.eventBusOrNil(); bus != nil {
		er := NewEventRing(host.EvtRing(), int(host.Evtfd().Fd()), bus)
		host.SetEvtSubscriber(er)
		log.Printf("[plugin] 事件环已创建（Bus → EvtRing → eventfd）")
	}

	log.Printf("[plugin] 共享段已创建（全部子进程插件共用一块，%d KB）", host.ShmSize()/1024)
	return host, nil
}

// closeProcHost 释放共享段（仅在内核关停时调用）。
func (r *Registry) closeProcHost() {
	r.procHostMu.Lock()
	defer r.procHostMu.Unlock()
	if r.procHost == nil {
		return
	}
	if err := r.procHost.Close(); err != nil {
		log.Printf("[plugin] 关闭共享段: %v", err)
	}
	r.procHost = nil
}

// onProcCrash 在子进程插件异常退出时回调。
//
// **崩溃隔离**：子进程死亡只影响自己，homed 继续服务——对比 C ABI 下
// 插件 panic 直接带崩整个进程（§1.2，现网已发生）。
//
// 但「homed 没崩」不等于「内核状态干净」。此前本函数只发了一个事件，
// 而全仓没有任何订阅者，于是生产上出现过 editdoc 被 kill 后：
//   - `edit_document` 仍留在 StageHost 的工具表里，模型照旧看得到、照旧调用，
//     每次都吃到 `proc: 插件进程已退出`；
//   - 该插件的 stage handler 仍在每轮 RunStage 里被并发调起并失败；
//   - 没有任何路径把它拉回来，插件永久缺席直到重启 homed。
//
// 所以崩溃回调必须做三件事：摘注册面、喂健康计数、排一次重启。
func (r *Registry) onProcCrash(name string, err error) {
	log.Printf("[plugin] 子进程插件 %s 异常退出: %v（homed 未受影响）", name, err)

	// 1) 摘掉工具/stage/通道。**必须先做**：从这一刻起模型就不该再看到这些工具，
	// 否则在重启完成前的窗口里每次调用都是确定的失败。
	r.detachPlugin(name)

	// 2) 从注册表移除。不做的后果：scheduleProcRestart 里的“已被其他路径重新加载”
	// 复核会误判（旧条目还在，See plugins[name] != nil），跳过真正的自动重启。
	// Plugin 对象本身仍被 proc 持有，Kill/回收不受影响。
	r.mu.Lock()
	delete(r.plugins, name)
	delete(r.sdkRefs, name)
	for i, inst := range r.instances {
		if inst.Name() == name {
			r.instances = append(r.instances[:i], r.instances[i+1:]...)
			break
		}
	}
	r.mu.Unlock()

	// 2) 事件通知（webui/诊断插件可订阅）。
	if bus := r.eventBusOrNil(); bus != nil {
		bus.Publish(&events.Event{
			Type:   events.EventSystem,
			Source: "plugin",
			Payload: map[string]interface{}{
				"event":  "plugin_crashed",
				"plugin": name,
				"error":  err.Error(),
			},
			Timestamp: time.Now().Unix(),
		})
	}

	// 3) 排一次重启。**必须异步**：本回调由 proc.markExited 在 readLoop 的
	// goroutine 里触发，而 ReloadOne 要拿 registry 锁、还要 Kill 并 join 同一个
	// readLoop（Process.Kill 里 readerWG.Wait），同步调用会自锁死。
	go r.scheduleProcRestart(name, err)
}

// scheduleProcRestart 在崩溃后按退避重启子进程插件。
//
// 退避与阈值语义与 agent 侧 plugin_health 对齐（窗口内 3 次即判定不健康），
// 但重启动作落在 registry：崩溃事实产生于此，agent 的 distillLoop 默认 30 分钟
// 才转一次（生产实配 2d），靠它兜底等于插件缺席数小时。
func (r *Registry) scheduleProcRestart(name string, cause error) {
	if r.shuttingDown.Load() {
		return // 内核正在关停，不再拉起
	}
	if !r.AutoRestartEnabled(name) {
		log.Printf("[plugin] %s 声明了不自动重启，保持缺席状态", name)
		return
	}

	n := r.noteCrash(name)
	if n > procMaxRestarts {
		log.Printf("[plugin] %s 在 %v 内崩溃 %d 次，停止自动重启（需人工介入）",
			name, procCrashWindow, n)
		return
	}

	// 线性退避：1 次→1s，2 次→2s，3 次→3s。
	// 目的只是崩溃循环时不至于打满 CPU。
	// 注意这**不是**无感恢复：首次就要等 1s，期间该插件的工具是缺席的，
	// 调用会直接报错。需要秒级就位的插件应在 OnStart 里自建重连与状态重建。
	delay := time.Duration(n) * procRestartBackoff
	time.Sleep(delay)

	// 期间可能已被 Disable/Remove/手工 plgreload 处理掉，重启前复核。
	if r.shuttingDown.Load() {
		return
	}
	if r.isDisabled(name) {
		log.Printf("[plugin] %s 已被禁用，取消自动重启", name)
		return
	}
	r.mu.RLock()
	already := r.plugins[name] != nil
	r.mu.RUnlock()
	if already {
		log.Printf("[plugin] %s 已被其他路径重新加载，取消自动重启", name)
		return
	}

	log.Printf("[plugin] 自动重启 %s（第 %d 次，退避 %v，起因: %v）", name, n, delay, cause)
	if err := r.ReloadOne(name); err != nil {
		log.Printf("[plugin] %s 自动重启失败: %v", name, err)
		return
	}
	log.Printf("[plugin] %s 自动重启成功", name)
}

// noteCrash 记录一次崩溃并返回窗口内的累计次数。
func (r *Registry) noteCrash(name string) int {
	now := time.Now()
	r.crashMu.Lock()
	defer r.crashMu.Unlock()
	if r.procCrashes == nil {
		r.procCrashes = make(map[string]*procCrashRecord)
	}
	rec := r.procCrashes[name]
	if rec == nil || now.Sub(rec.last) > procCrashWindow {
		rec = &procCrashRecord{}
		r.procCrashes[name] = rec
	}
	rec.count++
	rec.last = now
	return rec.count
}

// ResetProcCrashCount 清空某插件的崩溃计数（人工 plgreload / 重新启用后调用）。
func (r *Registry) ResetProcCrashCount(name string) {
	r.crashMu.Lock()
	defer r.crashMu.Unlock()
	delete(r.procCrashes, name)
}

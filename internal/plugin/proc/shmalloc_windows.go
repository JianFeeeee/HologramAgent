//go:build windows

package proc

import (
	"crypto/rand"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 侧共享段：命名 FileMapping + 命名 Event。
//
// ⚠️ 本文件**已不是可用路径**：homed 已放弃 Windows 原生支持
// （见 cmd/homed/platform_windows.go）。原因：插件体系依赖「继承的 fd」与
// 「统一共享内存区的段内偏移解引用」，而 Windows 既没有 fd 继承语义
// （os/exec 的 ExtraFiles 在 Windows 不支持），本文件描述的也仍是**旧的**
// 两段布局（StageContext 段 + 事件环段），跟不上 §13.1 的单块统一区域。
//
// 保留本文件只为让 GOOS=windows 仍能编译：否则平台门根本跑不起来，
// 用户看到的会是「产物缺失」而不是一句「请用 WSL」。
// allocShm 因此在入口直接报错，不返回一个「看起来能用」的段——
// 让它跑起来只会得到无法解释的握手失败，这比启动失败难查得多
// （与 shmalloc_other.go 的处理方式一致）。
//
// Windows 用户的正确路径：WSL2（在 WSL 里就是普通 linux/amd64）。
var shmNameSeq atomic.Uint64

const (
	shmNamePrefix   = "Local\\HologramAgentShm"
	evtRingNamePfx  = "Local\\HologramAgentEvtRing"
	evtEventNamePfx = "Local\\HologramAgentEvtSignal"
	envStageShmName = "HOMEAGENT_SHM_STAGE"
	envEvtRingName  = "HOMEAGENT_SHM_EVTRING"
	envEvtEventName = "HOMEAGENT_EVT_EVENT"
)

// namedShm 持有一块命名共享段。
//
// 不用 *os.File 承载：Windows 的 FileMapping 句柄不是文件句柄，
// 包进 os.File 后 Close 语义不对（会尝试当文件关）。故用独立类型，
// 由 shmHandles 表按 mmap 地址反查——freeShm 只拿到 (*os.File, []byte)。
type namedShm struct {
	name    string
	mapping windows.Handle
	addr    uintptr
	size    int
}

// shmHandles 记录已分配的段，供 freeShm 按数据指针反查句柄。
//
// 为何需要这张表：allocShm 的跨平台签名返回 (*os.File, []byte)，
// Windows 没有对应的 fd，只能把句柄存在旁路。key 用切片首地址。
var (
	shmHandles   = map[uintptr]*namedShm{}
	shmHandlesMu sync.Mutex
)

// allocShm 在 Windows 上明确报错：homed 不支持 Windows 原生运行。
//
// 不返回「能用的段」：本文件实现的是 §13.1 之前的**两段**布局，
// 与当前内核的单块统一区域不兼容。静默返回只会在握手阶段变成一句
// 无法解释的魔数不匹配。报错文案直接给出行动：用 WSL2。
func allocShm(size int) (*os.File, []byte, error) {
	return nil, nil, fmt.Errorf("proc: homed 不支持 Windows 原生运行" +
		"（插件体系依赖 fd 继承与统一共享内存区的段内偏移解引用）——请使用 WSL2；" +
		"详见 cmd/homed/platform_windows.go")
}

// shmNameForMode 按安全模式生成命名段名。
//
// safe/debug：随机 nonce 名，防猜测；唯一在 debug 下额外暴露到 stderr。
// full：固定后缀，便于多实例按名共享。
func shmNameForMode() string {
	seq := shmNameSeq.Add(1)
	switch ShmSecurityModeOf() {
	case ShmModeFull:
		return fmt.Sprintf("%s_%d_%d", shmNamePrefix, os.Getpid(), seq)
	default: // safe / debug
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			// 退化为 PID+seq（极端情况，crypto rand 几乎不会失败）
			return fmt.Sprintf("%s_%d_%d", shmNamePrefix, os.Getpid(), seq)
		}
		return fmt.Sprintf("%s_%d_%x", shmNamePrefix, os.Getpid(), b)
	}
}

// createNamedMapping 建命名段并映射为 []byte。
func createNamedMapping(name string, size int) (*namedShm, []byte, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, nil, fmt.Errorf("proc: 共享段名字非法 %q: %w", name, err)
	}

	// INVALID_HANDLE_VALUE + 命名 → 由系统页文件支撑的匿名段（不落盘）
	mapping, err := windows.CreateFileMapping(
		windows.InvalidHandle, nil, windows.PAGE_READWRITE,
		uint32(uint64(size)>>32), uint32(size), namePtr)
	if err != nil {
		return nil, nil, fmt.Errorf("proc: 创建命名共享段 %q: %w", name, err)
	}

	addr, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_WRITE, 0, 0, uintptr(size))
	if err != nil {
		windows.CloseHandle(mapping)
		return nil, nil, fmt.Errorf("proc: 映射共享段 %q: %w", name, err)
	}

	return &namedShm{name: name, mapping: mapping, addr: addr, size: size},
		unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), nil
}

// freeShm 解除映射并关闭段句柄。
func freeShm(f *os.File, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	key := uintptr(unsafe.Pointer(&data[0]))
	shmHandlesMu.Lock()
	shm, ok := shmHandles[key]
	delete(shmHandles, key)
	shmHandlesMu.Unlock()
	if !ok {
		return nil
	}
	var firstErr error
	if err := windows.UnmapViewOfFile(shm.addr); err != nil {
		firstErr = err
	}
	if err := windows.CloseHandle(shm.mapping); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// shmNameOf 返回某块已分配段的名字（供注入子进程环境变量）。
func shmNameOf(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	shmHandlesMu.Lock()
	defer shmHandlesMu.Unlock()
	if shm, ok := shmHandles[uintptr(unsafe.Pointer(&data[0]))]; ok {
		return shm.name
	}
	return ""
}

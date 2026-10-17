//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
	procGetStdHandle   = kernel32.NewProc("GetStdHandle")
)

const (
	stdInputHandle                  = ^uint32(9) + 1 // -10
	enableVirtualTerminalProcessing = 0x0004
	enableProcessedOutput           = 0x0001
	enableWrapAtEOLOutput           = 0x0002
	disableNewlineAutoReturn        = 0x0008
)

func setRawMode(fd int) (func(), error) {
	if fd == 0 {
		fd = int(os.Stdin.Fd())
	}
	if !isTerminal(fd) {
		return func() {}, nil
	}
	// Enable virtual terminal processing for ANSI escape sequences on Windows
	h, _, _ := procGetStdHandle.Call(uintptr(^uint32(10) + 1)) // STD_OUTPUT_HANDLE = -11
	if h != 0 && h != ^uintptr(0) {
		var mode uint32
		procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode)))
		newMode := mode | enableVirtualTerminalProcessing | enableProcessedOutput | enableWrapAtEOLOutput
		procSetConsoleMode.Call(h, uintptr(newMode))
	}
	return func() {}, nil
}

func isTerminal(fd int) bool {
	var mode uint32
	h, _, _ := procGetStdHandle.Call(uintptr(^uint32(10) + 1)) // STD_OUTPUT_HANDLE = -11
	if h == 0 || h == ^uintptr(0) {
		return false
	}
	ret, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode)))
	return ret != 0
}

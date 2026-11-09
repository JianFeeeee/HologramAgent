package test

// internal/safeclose 的黑盒判据：只用导出 API，输入 → 可观察输出。
// 不构造内部状态、不用任何测试专用钩子。

import (
	"sync"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/safeclose"
)

// TestSafeFuncIsIdempotent：同一个返回函数连调多次不得 panic，channel 确已关闭。
func TestSafeFuncIsIdempotent(t *testing.T) {
	ch := make(chan struct{})
	stop := safeclose.SafeFunc(ch)
	for i := 0; i < 5; i++ {
		stop()
	}
	select {
	case <-ch:
	default:
		t.Fatal("★ channel 未关闭 —— SafeFunc 的返回函数没生效")
	}
}

// TestSafeReturnsFiredOnce：Safe 的返回值报告「本次是否真关闭」，
// 同一把 Once 只有第一次返回 true。
func TestSafeReturnsFiredOnce(t *testing.T) {
	ch := make(chan struct{})
	var once sync.Once
	if !safeclose.Safe(&once, ch) {
		t.Fatal("★ 第一次 Safe 应返回 true")
	}
	if safeclose.Safe(&once, ch) {
		t.Fatal("★ 第二次 Safe 仍返回 true —— 幂等语义坏了")
	}
}

// TestSafeRejectsNilInputs：nil once / nil chan 必须安全返回 false。
func TestSafeRejectsNilInputs(t *testing.T) {
	var once sync.Once
	if safeclose.Safe[struct{}](&once, nil) {
		t.Fatal("★ nil chan 被接受")
	}
	ch := make(chan struct{})
	if safeclose.Safe[struct{}](nil, ch) {
		t.Fatal("★ nil once 被接受")
	}
	select {
	case <-ch:
		t.Fatal("★ nil once 却关闭了 channel")
	default:
	}
}

// TestConcurrentCloseIsSafe：8 个 goroutine 同时关同一个 channel 不得 panic/死锁。
func TestConcurrentCloseIsSafe(t *testing.T) {
	for round := 0; round < 100; round++ {
		ch := make(chan struct{})
		var once sync.Once
		done := make(chan struct{}, 8)
		for i := 0; i < 8; i++ {
			go func() {
				safeclose.Safe(&once, ch)
				done <- struct{}{}
			}()
		}
		for i := 0; i < 8; i++ {
			<-done
		}
		select {
		case <-ch:
		default:
			t.Fatal("★ 并发结束后 channel 未关闭")
		}
	}
}

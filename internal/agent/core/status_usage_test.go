package core

import (
	"encoding/json"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 用户实测「无法统计缓存命中与 token」时，除了回包 usage（已由
// emitresponse_usage_test.go / emitresponse_e2e_test.go 覆盖），还有一处缺口：
// **运行态里看不到累计账目**。
//
// 内核已有 GetKernelStatus()（/kernel、/api/v1/kernel、healthcheck_kernel 都走它），
// 里面有 Scheduler / ONNX / Residents 等运行时段落，唯独没有用量。
// 于是「这个实例到底花了多少、缓存省了多少」只能靠翻日志。

// TestKernelStatusExposesUsage 断言用量进入内核状态快照。
func TestKernelStatusExposesUsage(t *testing.T) {
	a := &Agent{}
	a.usageLedger.record(tk(1000, 200, 1200, 768, 232, 50, true))
	a.usageLedger.record(tk(500, 100, 600, 0, 0, 0, false))

	ks := a.GetKernelStatus()
	if ks == nil {
		t.Fatal("GetKernelStatus 返回 nil")
	}
	if ks.Usage.Calls != 2 {
		t.Errorf("usage.calls=%d，期望 2", ks.Usage.Calls)
	}
	if ks.Usage.Prompt != 1500 || ks.Usage.Total != 1800 {
		t.Errorf("usage.prompt=%d total=%d，期望 1500 / 1800", ks.Usage.Prompt, ks.Usage.Total)
	}
	if ks.Usage.CacheRead != 768 {
		t.Errorf("usage.cache_read=%d，期望 768", ks.Usage.CacheRead)
	}
	// 命中率：分母只算「上游报过缓存」的调用 ⇒ 768/(768+232)=0.768
	if ks.Usage.CacheHitRate == nil {
		t.Fatal("有缓存数据时 cache_hit_rate 必须给出")
	}
	if got := *ks.Usage.CacheHitRate; got < 0.767 || got > 0.769 {
		t.Errorf("cache_hit_rate=%v，期望 ≈0.768", got)
	}
}

// TestKernelStatusUsageOmitsHitRateWhenNothingReported 守住「无数据 ≠ 0%」。
//
// 判据：上游一次都没报缓存时，命中率字段必须**缺席**（而非 0），
// 否则消费方会把「没开缓存」画成「命中率 0%」——那是拿假数据做优化决定。
func TestKernelStatusUsageOmitsHitRateWhenNothingReported(t *testing.T) {
	a := &Agent{}
	a.usageLedger.record(tk(1000, 200, 1200, 0, 0, 0, false))

	ks := a.GetKernelStatus()
	if ks.Usage.CacheHitRate != nil {
		t.Errorf("没有任何调用报过缓存时不该给命中率，实际 %v", *ks.Usage.CacheHitRate)
	}

	// 序列化后也必须真的没有这个键（omitempty 语义）。
	b, err := json.Marshal(ks.Usage)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "cache_hit_rate") {
		t.Errorf("JSON 里不该出现 cache_hit_rate：%s", b)
	}
}

// TestKernelStatusUsageZeroIsHonest 空实例不应凭空造数。
func TestKernelStatusUsageZeroIsHonest(t *testing.T) {
	a := &Agent{}
	ks := a.GetKernelStatus()
	if ks.Usage.Calls != 0 {
		t.Errorf("空实例 calls=%d，期望 0", ks.Usage.Calls)
	}
	if ks.Usage.CacheHitRate != nil {
		t.Error("空实例不该给命中率")
	}
}

// 保证测试里用到的类型确实来自 agentAPI（避免 tk() 帮忙函数类型漂移）。
var _ = agentAPI.TokenUsage{}

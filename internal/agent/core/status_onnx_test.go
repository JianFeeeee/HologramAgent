package core

import (
	"strings"
	"testing"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
)

// statusSpace 是带模态元数据的假统一空间（ProviderAdapter 的 Modalities() 形状）。
type statusSpace struct{ fakeSpace }

func (statusSpace) Modalities() []string { return []string{"text", "image"} }

// statusSpaceUnloaded 模拟「provider 建了但没加载成功」。
type statusSpaceUnloaded struct{ fakeSpace }

func (statusSpaceUnloaded) Loaded() bool { return false }

// healthcheck_kernel 必须能回答两件事：内核版本号、以及**是否真的启用了 ONNX 模型**。
//
// 判据要点：只有 provider 真正 Loaded() 才算启用 ——「配置里写了 provider」不算，
// 否则模型缺失/运行时缺失时会报成已启用（假绿）。
func TestKernelStatusReportsVersionAndONNX(t *testing.T) {
	t.Run("已启用：带出 provider/维度/指纹/模态", func(t *testing.T) {
		a := &Agent{
			io:                agentIO.NewIOManager(),
			multimodalSpace:   statusSpace{},
			embeddingProvider: "chineseclip",
		}
		st := a.GetKernelStatus()
		if !st.ONNX.Enabled {
			t.Fatal("Loaded() 为真时 onnx.enabled 必须为真")
		}
		if st.ONNX.Provider != "chineseclip" || st.ONNX.Dim != 2 || st.ONNX.Fingerprint != "fake-space" {
			t.Fatalf("onnx 身份字段不对: %+v", st.ONNX)
		}
		if len(st.ONNX.Modalities) != 2 || st.ONNX.Reason != "" {
			t.Fatalf("模态/原因不对: %+v", st.ONNX)
		}
		// 内核版本号必须随状态一起报：人格卡要求「版本以运行时快照为准」靠的就是这一项
		if st.Build.Version == "" || st.Build.KernelName == "" {
			t.Fatalf("build 段缺少版本/内核名: %+v", st.Build)
		}
	})

	t.Run("打开失败：enabled=false 且给出具体原因", func(t *testing.T) {
		a := &Agent{
			io:                agentIO.NewIOManager(),
			embeddingProvider: "chineseclip",
			embeddingError:    `embedding: open provider "chineseclip": model dir missing`,
		}
		st := a.GetKernelStatus()
		if st.ONNX.Enabled {
			t.Fatal("打开失败时不能报 enabled")
		}
		if st.ONNX.Provider != "chineseclip" {
			t.Fatalf("未启用时仍应带出配置的 provider: %+v", st.ONNX)
		}
		if !strings.Contains(st.ONNX.Reason, "model dir missing") {
			t.Fatalf("原因应包含具体错误: %q", st.ONNX.Reason)
		}
	})

	t.Run("未配置：说明会走回退路径", func(t *testing.T) {
		a := &Agent{io: agentIO.NewIOManager()}
		st := a.GetKernelStatus()
		if st.ONNX.Enabled || st.ONNX.Reason == "" {
			t.Fatalf("未配置时应 enabled=false 且有原因: %+v", st.ONNX)
		}
	})

	t.Run("provider 存在但未加载", func(t *testing.T) {
		a := &Agent{
			io:                agentIO.NewIOManager(),
			multimodalSpace:   statusSpaceUnloaded{},
			embeddingProvider: "qwen3vl",
		}
		st := a.GetKernelStatus()
		if st.ONNX.Enabled {
			t.Fatal("Loaded() 为假时不能报 enabled")
		}
		if st.ONNX.Reason == "" {
			t.Fatalf("应给出未加载的原因: %+v", st.ONNX)
		}
	})
}

// collectKernelStatus 的 knowledge 参数是**接口**类型，而 (*knowledge.Store)(nil)
// 塞进接口后 `ks != nil` 仍为真 → 调 List() 直接 panic。
// 这条测试钉住这个成因：一旦不再 panic，说明参数形状变了，
// GetKernelStatus 里的 typed-nil 守卫就该同步删掉（否则它变成无意义代码）。
func TestCollectKernelStatusTypedNilKnowledgePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("typed-nil 交给接口参数却未 panic：成因已变，请更新守卫与本测试")
		}
	}()
	var nilStore *knowledge.Store
	_ = collectKernelStatus(time.Now(), "a", "", 0, nil, nil, nil, nil, nilStore, nil, nil, nil, nil)
}

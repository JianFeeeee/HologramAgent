package seemedia

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 真跑 ffmpeg 的集成/压力/变异测试；无 ffmpeg 时自动跳过。
//
// 单测（plugin_test.go）钉纯逻辑（来源解析、clamp、MIME）；这里钉真实行为：
//   - 抽帧真能从一段真视频拿到正确数量的合法 PNG
//   - 探不到时长时走单遍兜底仍能拿到帧
//   - 损坏输入如实报错、不 panic
//   - 已取消的 ctx 必须让 ffmpeg 立刻失败（取消传播真生效）
//   - 并发 16 路抽帧不串台、不泄漏临时目录
//   - 块引用视频走真记忆库 → 临时文件 → cleanup 删文件

// sdkWithVideo 把一段视频字节写成**真记忆块**（内容派生 ID），返回带记忆面的
// PluginSDK 与真实块引用（块 ID 或其唯一前缀，与生产同一取字节路径）。
//
// ★ CAS 淘汰后不再有媒体存储可 Put：字节进块（MemoryBlock.Data），
//
//	插件用 sdk.Memory().MediaBytes(ref) 取回 —— 这正是测试要钉的路径。
func sdkWithVideo(t *testing.T, raw []byte) (*sdk.PluginSDK, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	block := memory.MemoryBlock{
		ID:       memory.MediaBlockID("video/mp4", raw),
		Modality: memory.BlockVideo,
		Data:     raw,
		MIME:     "video/mp4",
		Size:     int64(len(raw)),
		Tool:     "test",
	}
	if err := db.PutMemoryBlocks([]memory.MemoryBlock{block}); err != nil {
		t.Fatal(err)
	}
	return sdk.New("seemedia", sdk.SDKConfig{
		Memory: sdk.NewGraphMemoryWithPlugin("seemedia", db),
	}), block.ID
}

func mkTestVideo(t *testing.T, dur int) string {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("无 ffmpeg，跳过集成测试")
	}
	out := filepath.Join(t.TempDir(), "clip.mp4")
	cmd := exec.Command(ff, "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=duration=%d:size=320x240:rate=10", dur),
		"-pix_fmt", "yuv420p", out, "-y")
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg 造测试视频失败（环境问题，跳过）: %v", err)
	}
	return out
}

func newProbedPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := &Plugin{videoFrames: defaultVideoFrames, timeout: 60 * time.Second}
	p.ffmpeg, _ = exec.LookPath("ffmpeg")
	p.ffprobe, _ = exec.LookPath("ffprobe")
	if p.ffmpeg == "" {
		t.Skip("无 ffmpeg，跳过集成测试")
	}
	return p
}

func isPNG(b []byte) bool {
	return len(b) >= 8 &&
		b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G' &&
		b[4] == 0x0D && b[5] == 0x0A && b[6] == 0x1A && b[7] == 0x0A
}

func countSeemediaTemp() int {
	frames, _ := filepath.Glob(filepath.Join(os.TempDir(), "seemedia-frames-*"))
	vids, _ := filepath.Glob(filepath.Join(os.TempDir(), "seemedia-*.video"))
	return len(frames) + len(vids)
}

// ① 真视频 → 请求 N 帧就拿到 N 帧，且每帧都是合法 PNG。
func TestExtractFrames_RealVideoExactCount(t *testing.T) {
	p := newProbedPlugin(t)
	video := mkTestVideo(t, 10)

	for _, n := range []int{1, 3, 6, 10} {
		frames, err := p.extractFrames(video, n)
		if err != nil {
			t.Fatalf("n=%d 抽帧失败: %v", n, err)
		}
		if len(frames) != n {
			t.Errorf("请求 %d 帧，实得 %d 帧", n, len(frames))
		}
		for i, f := range frames {
			if !isPNG(f) {
				t.Errorf("n=%d 第 %d 帧不是合法 PNG（%d 字节）", n, i, len(f))
			}
		}
	}
}

// ② 探不到时长（ffprobe 置空）时走单遍兜底，仍能拿到帧。
func TestExtractFrames_FallbackPathWorks(t *testing.T) {
	p := newProbedPlugin(t)
	video := mkTestVideo(t, 6)

	p.ffprobe = "" // 强制走兜底：模拟探时长失败
	frames, err := p.extractFrames(video, 4)
	if err != nil {
		t.Fatalf("兜底路径抽帧失败: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("兜底路径一帧都没抽到")
	}
	for i, f := range frames {
		if !isPNG(f) {
			t.Errorf("兜底第 %d 帧不是 PNG", i)
		}
	}
}

// ③ 损坏/非视频文件：如实报错，不 panic、不返回半成品。
func TestExtractFrames_CorruptInput(t *testing.T) {
	p := newProbedPlugin(t)
	bad := filepath.Join(t.TempDir(), "notvideo.mp4")
	if err := os.WriteFile(bad, []byte("this is definitely not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	frames, err := p.extractFrames(bad, 3)
	if err == nil && len(frames) > 0 {
		t.Errorf("损坏输入不应抽出帧，却得到 %d 帧", len(frames))
	}
}

// ④ 取消传播真生效：已取消的 ctx 必须让 ffmpeg 立刻失败，而不是照跑。
//
// 为什么不计时测 p.timeout：合成视频抽帧本来就快（~0.1s），「1ns 超时」和
// 「忽略超时」在计时上分不出（此前实测这种变异未被杀，正是判据设计缺陷）。
// 真正要防的是「完全不用 ctx → 无限挂」，用已取消 ctx 的行为判据来钉：
// exec.CommandContext 在 ctx 已取消时 Start 即失败；换成 exec.Command 则照跑。
func TestFrameGrabHonorsCanceledContext(t *testing.T) {
	p := newProbedPlugin(t)
	video := mkTestVideo(t, 4)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	b, err := p.grabFrameAt(ctx, video, 1.0)
	if err == nil {
		t.Fatalf("已取消的 ctx 必须让抽帧失败，却成功返回 %d 字节（ffmpeg 未被 ctx 约束）", len(b))
	}
}

// ⑤ 压力：并发 16 路抽帧，各自独立临时目录，不串帧、不泄漏临时目录。
func TestExtractFrames_ConcurrentNoLeak(t *testing.T) {
	p := newProbedPlugin(t)
	video := mkTestVideo(t, 8)

	tmpBefore := countSeemediaTemp()

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			n := (id % 10) + 1
			frames, err := p.extractFrames(video, n)
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", id, err)
				return
			}
			if len(frames) == 0 {
				errs <- fmt.Errorf("worker %d: 0 帧", id)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	time.Sleep(200 * time.Millisecond)
	tmpAfter := countSeemediaTemp()
	if tmpAfter > tmpBefore {
		t.Errorf("并发抽帧泄漏临时目录: before=%d after=%d", tmpBefore, tmpAfter)
	}
}

// ⑥ 块引用视频来源：真记忆块取字节 → 临时文件 → 抽帧 → cleanup 删文件。
func TestResolveVideoInput_DigestCleanup(t *testing.T) {
	p := newProbedPlugin(t)
	video := mkTestVideo(t, 4)
	raw, err := os.ReadFile(video)
	if err != nil {
		t.Fatal(err)
	}
	s, ref := sdkWithVideo(t, raw)
	p.sdk = s

	input, cleanup, err := p.resolveVideoInput(map[string]interface{}{"digest": ref})
	if err != nil {
		t.Fatalf("块引用解析失败: %v", err)
	}
	if _, serr := os.Stat(input); serr != nil {
		t.Fatalf("临时视频文件应存在: %v", serr)
	}
	frames, err := p.extractFrames(input, 2)
	if err != nil || len(frames) == 0 {
		t.Errorf("从 digest 临时文件抽帧失败: err=%v frames=%d", err, len(frames))
	}
	cleanup()
	if _, serr := os.Stat(input); !os.IsNotExist(serr) {
		t.Errorf("cleanup 后临时文件应被删除，stat=%v", serr)
	}
}

// ⑦ 静态判据：所有 ffmpeg/ffprobe 子进程必须用 exec.CommandContext。
//
// CommandContext 是「任何情况下都能被 ctx 中断」的契约；换成 exec.Command
// 就失去取消能力。这条跨实现有效：谁把 CommandContext 改回 Command，谁就引入
// 「网络流/坏文件上无限挂」的风险。
func TestFrameCmdsUseCommandContext(t *testing.T) {
	src, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "exec.Command(") && !strings.Contains(line, "exec.CommandContext(") {
			t.Errorf("plugin.go:%d 用裸 exec.Command 跑子进程 —— 必须 exec.CommandContext（ctx 取消才杀得掉）: %s", i+1, strings.TrimSpace(line))
		}
	}
}

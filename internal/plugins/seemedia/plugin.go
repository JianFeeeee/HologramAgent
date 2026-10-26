// Package seemedia 让模型**主动一手**去看图/看视频/听音频。
//
// # 与 multimodal 插件的分工（2026-10-11 新增）
//
// `multimodal`（describe_image / transcribe_audio / ocr_image）走的是**二手**路：
// 插件自己起一次 VLM 子调用，把结果**文字**喂回主模型。省 token、能入记忆正文、
// 主模型非视觉也能用，但模型看到的是「别人转述的描述」，细节丢失不可逆。
//
// 本插件走的是**一手**路：把图/帧/音频的字节**直接投给主模型**，经
// `PluginSDK.SetToolBlocks` → 内核在紧随的 user message 里带上 image_url/audio_url
// 块（task.go `ConsumeToolBlocks`）。模型是**自己看到**原始像素，而不是读描述。
//
// 两者刻意并存（用户 2026-10-11 决策 coexist）：
//   - 要省 token、要把结论写进记忆、主模型不支持该模态 → describe_image（二手）
//   - 要模型亲自判读细节（看清一张截图、逐帧看一段操作） → see_image/see_video（一手）
//
// # 直视不可用时会怎样（内核已兜底，插件不必关心）
//
// 主模型不支持视觉/音频时，内核 `prepareToolBlocks` 会把这些块走回退链批量
// 转写成文字再并入上下文（modalfallback.go），**不会**把 image_url 静默塞给不支持
// 的网关（那会被剥离后返回 200，模型答「没看到图」而内核以为成功）。所以本插件
// 只管「把字节交给内核」，支不支持、要不要回退由内核决定。
//
// # 媒体来源（用户决策 path + digest + url 三者都支持）
//
//	path   —— 设备/工具回传的落盘文件（local_screensee、browser_screenshot、mc 截图…）
//	digest —— 用户上传已入库的媒体，用 sdk.Memory().MediaBytes 按块取回，不在内核与插件间传 base64
//	url    —— http(s) 直链；图/音频原样透传给模型（不下载，不入 CAS，交给上游拉取），
//	          视频则交给 ffmpeg 直接读取（ffmpeg 原生支持 http 输入）
//
// # 媒体入记忆（天然打通，本插件无需自己写库）
//
// seemedia 经 SetToolBlocks 注入的块，内核在 task.go `captureBlockMedia` 处**无条件**
// 落进 CAS 并转成一等记忆块挂到本轮 ContextEvent（「无论直视还是回退转写，媒体
// 本体都该进记忆」）。所以记忆载体块化之后，一手看到的图/帧**同时**进了记忆，
// 之后能按多模态向量被 coldcontext_query / memory_recall 召回。本插件不写任何入库代码。
//
// ★ 但有一个边界：`captureBlockMedia` 只对 **data URL** 做 CAS 入库，http(s) URL
// 会被跳过（不下载、不入记忆、不算向量——避免下载引入超时/SSRF）。因此：
//
//	path / digest 来源 → 转 data URL → 进记忆块（可召回）
//	url（http 直链）  → 原样透传给模型看 → 模型看得到，但**不进记忆**
//
// 这是刻意取舍，不是缺陷：要让一份 http 媒体长期可召回，应先把它落盘再传 path。
//
// # 为什么是内置插件而不是子进程插件（用户决策 builtin）
//
// 与 multimodal 并排，形态一致、最省心。代价是延续了「内核进程内读任意本地文件」
// 这个 a02d8b00 本想消除的张力——但既然 multimodal 已是内置且同样读 path，这里
// 保持一致比单独走子进程沙箱更不易让人困惑。若日后要收紧，两个插件应一起搬。
package seemedia

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

const (
	// defaultVideoFrames 是 see_video 默认抽取的帧数。
	defaultVideoFrames = 6
	// maxVideoFrames 是硬上限。对齐内核 modalfallback 的「see_video 一次最多 10 帧」
	// 约束：帧越多上游越慢、越易超单请求体积限制，回退转写也只处理前若干块。
	maxVideoFrames = 10
)

func init() {
	plugin.RegisterPluginMeta("seemedia", "多模态直视", "See Media")
	plugin.RegisterFactory("seemedia", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

// Plugin 提供 see_image / see_video / hear_audio：把媒体字节直接投给主模型。
type Plugin struct {
	name string
	sdk  *sdk.PluginSDK

	videoFrames int
	timeout     time.Duration
	ffmpeg      string // ffmpeg 可执行路径（空=未探测到，see_video 会以 error 上抛）
	ffprobe     string
}

// New 构造插件。
func New(name string) *Plugin { return &Plugin{name: name} }

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	p.videoFrames = defaultVideoFrames
	p.timeout = 120 * time.Second

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "video_frames", Default: strconv.Itoa(defaultVideoFrames), Type: "int",
		DisplayName: "see_video 抽帧数",
		Description: fmt.Sprintf("see_video 默认从视频里均匀抽取多少帧投给模型（1-%d）。"+
			"帧越多模型越能看清过程变化，但上游越慢、越易超体积限制。", maxVideoFrames),
		Category: "seemedia",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "timeout_seconds", Default: "120", Type: "int",
		DisplayName: "媒体处理超时（秒）",
		Description: "单次取媒体/抽帧的时间上限。",
		Category:    "seemedia",
	})
	p.loadSettings()

	// ffmpeg/ffprobe 只在 see_video 时需要；探测结果缓存，缺失时在工具描述里说清。
	p.ffmpeg, _ = exec.LookPath("ffmpeg")
	p.ffprobe, _ = exec.LookPath("ffprobe")

	srcParam := map[string]interface{}{
		"path": map[string]interface{}{
			"type": "string",
			"description": "媒体文件路径（设备/工具回传的落盘文件，如 screensee / browser_screenshot 的截图）。" +
				"path / digest / url 三者给其一。",
		},
		"digest": map[string]interface{}{
			"type":        "string",
			"description": "媒体在记忆里的**块引用**（块 ID 或其唯一前缀；用户上传已入库的媒体）。",
		},
		"url": map[string]interface{}{
			"type":        "string",
			"description": "媒体的 http(s) 直链。图片/音频原样交给模型，视频交给 ffmpeg 读取。",
		},
	}

	if err := s.RegisterTool("see_image", sdk.ToolDef{
		Name: "see_image",
		Description: "【亲自看图】把一张图片**原样投给你自己**去看——你会在下一步直接看到这张图的真实画面，" +
			"而不是别人转述的文字描述。需要判读截图、看清图里的细节/布局/颜色时用它。" +
			"（只想要一段文字描述、或你本身不支持视觉时，用 describe_image 更省。）" +
			"来源三选一：path / digest / url。",
		Parameters:   srcObject(srcParam),
		ParallelSafe: false,
	}, p.handleSeeImage); err != nil {
		return err
	}

	videoDesc := "【亲自看视频】从一段视频里均匀抽若干帧**原样投给你自己**去看，你会直接看到这些画面。" +
		"适合判读一段操作录屏、监控片段、动图的过程变化。来源三选一：path / digest / url。" +
		"可选 frames 指定抽帧数（1-" + strconv.Itoa(maxVideoFrames) + "，默认 " + strconv.Itoa(defaultVideoFrames) + "）。"
	if p.ffmpeg == "" {
		videoDesc += " ★本机未检测到 ffmpeg，调用会失败；请先安装 ffmpeg。"
	}
	if err := s.RegisterTool("see_video", sdk.ToolDef{
		Name:         "see_video",
		Description:  videoDesc,
		Parameters:   p.videoParams(srcParam),
		ParallelSafe: false,
	}, p.handleSeeVideo); err != nil {
		return err
	}

	if err := s.RegisterTool("hear_audio", sdk.ToolDef{
		Name: "hear_audio",
		// ★ 不得无条件承诺「你会听到」（2026-10-11 e2e 抓出）。
		//
		// 实测：当前部署里**所有** LLM 源都是 audio=false，且
		// core.input_processing.audio.fallback_provider 为空 ⇒ 内核既不能直视
		// 也无法回退转写，只会发一条 notice 说「拿不到音频」。此前描述写死
		// 「你会直接听到它」，与内核随后发的 notice 自相矛盾，模型据此困惑
		// （实测它转而去数 WAV 字节）。描述必须如实说明可能拿不到。
		Description: "【亲自听音频】把一段音频**原样投给你自己**去听。若当前模型支持音频" +
			"（或配了可用的音频回退源），你会在下一步直接听到它。" +
			"★若当前模型与回退源都不支持音频，内核会明确告诉你拿不到音频（不会假装听到）——" +
			"那时请改用 transcribe_audio 拿文字转写，或请用户配置音频回退源" +
			"（core.input_processing.audio.fallback_provider + 该源 audio=true）。" +
			"来源三选一：path / digest / url。",
		Parameters:   srcObject(srcParam),
		ParallelSafe: false,
	}, p.handleHearAudio); err != nil {
		return err
	}
	return nil
}

func (p *Plugin) Stop() error { return nil }

func srcObject(props map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": props}
}

func (p *Plugin) videoParams(src map[string]interface{}) map[string]interface{} {
	props := map[string]interface{}{}
	for k, v := range src {
		props[k] = v
	}
	props["frames"] = map[string]interface{}{
		"type":        "integer",
		"description": fmt.Sprintf("可选：抽取帧数（1-%d，默认 %d）。", maxVideoFrames, defaultVideoFrames),
	}
	return srcObject(props)
}

func (p *Plugin) loadSettings() {
	s := p.sdk.Settings()
	if s == nil {
		return
	}
	if v, _ := s.Get("video_frames"); v != nil {
		if n := toInt(v); n > 0 {
			p.videoFrames = clampFrames(n)
		}
	}
	if v, _ := s.Get("timeout_seconds"); v != nil {
		if n := toInt(v); n > 0 {
			p.timeout = time.Duration(n) * time.Second
		}
	}
}

// ── 工具处理 ───────────────────────────────────────────────

func (p *Plugin) handleSeeImage(args map[string]interface{}) (interface{}, error) {
	url, err := p.resolveMediaURL(args, "image")
	if err != nil {
		return nil, err
	}
	p.sdk.SetToolBlocks([]sdk.ContentBlock{{
		Type:     "image_url",
		ImageURL: &sdk.ImageURL{URL: url, Detail: "auto"},
	}})
	return map[string]interface{}{
		"status": "ok",
		"note":   "图片已投给你——请在下一步直接查看画面本身（这不是文字描述）。",
	}, nil
}

func (p *Plugin) handleHearAudio(args map[string]interface{}) (interface{}, error) {
	url, err := p.resolveMediaURL(args, "audio")
	if err != nil {
		return nil, err
	}
	p.sdk.SetToolBlocks([]sdk.ContentBlock{{
		Type:     "audio_url",
		AudioURL: &sdk.AudioURL{URL: url},
	}})
	// ★ note 同样不得承诺「你会听到」：插件在调用时无法得知当前模型/回退源
	// 是否支持音频（SDK 未暴露能力查询），支持与否由内核在下一步判定。
	// 这里只如实说「已投递」，并指明没收到时怎么办。
	return map[string]interface{}{
		"status": "ok",
		"note": "音频已投递——请在下一步确认是否真的收到音频内容。" +
			"若收到「不支持音频」之类的说明，说明当前模型与回退源都听不了，" +
			"请改用 transcribe_audio 拿文字转写。",
	}, nil
}

func (p *Plugin) handleSeeVideo(args map[string]interface{}) (interface{}, error) {
	if p.ffmpeg == "" {
		return nil, fmt.Errorf("本机未安装 ffmpeg，无法从视频抽帧；请先安装 ffmpeg（apt install ffmpeg 或等价命令）")
	}
	n := p.videoFrames
	if v, ok := args["frames"]; ok {
		if k := toInt(v); k > 0 {
			n = clampFrames(k)
		}
	}

	input, cleanup, err := p.resolveVideoInput(args)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	frames, err := p.extractFrames(input, n)
	if err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("未能从视频抽到任何帧（文件可能损坏或不是视频）")
	}

	blocks := make([]sdk.ContentBlock, 0, len(frames))
	for _, f := range frames {
		dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(f)
		blocks = append(blocks, sdk.ContentBlock{
			Type:     "image_url",
			ImageURL: &sdk.ImageURL{URL: dataURL, Detail: "auto"},
		})
	}
	p.sdk.SetToolBlocks(blocks)
	return map[string]interface{}{
		"status": "ok",
		"frames": len(blocks),
		"note": fmt.Sprintf("已抽取 %d 帧并投给你——请在下一步直接查看这些画面，"+
			"按时间顺序理解过程变化。", len(blocks)),
	}, nil
}

// ── 来源解析 ───────────────────────────────────────────────

// mediaBytes 按媒体块引用取回字节与 MIME。
//
// ★ CAS 淘汰后插件取媒体的唯一路径：块 ID（内容派生）就是媒体身份。
//
//	取不到时返回 ok=false（“这个引用没媒体”是常态，不是错误）。
func (p *Plugin) mediaBytes(ref string) (data []byte, mime string, ok bool, err error) {
	if p.sdk == nil || p.sdk.Memory() == nil {
		return nil, "", false, fmt.Errorf("记忆系统不可用")
	}
	return p.sdk.Memory().MediaBytes(ref)
}

// resolveMediaURL 把 path/ref/url 解析成可直接放进 image_url/audio_url 的字符串。
//
//	url → 原样返回（http 直链，交给上游拉取；不下载）
//	path → 读本地文件，转 data URL
//	ref  → 按**媒体块引用**取回字节，转 data URL
//
// kind 仅用于在扩展名/元信息都缺失时猜一个兜底 MIME（image 或 audio）。
func (p *Plugin) resolveMediaURL(args map[string]interface{}, kind string) (string, error) {
	path := strings.TrimSpace(argStr(args, "path"))
	ref := strings.TrimSpace(argStr(args, "digest"))
	rawURL := strings.TrimSpace(argStr(args, "url"))

	switch {
	case rawURL != "":
		if !isHTTPURL(rawURL) {
			return "", fmt.Errorf("url 必须是 http(s) 直链，实际: %q", rawURL)
		}
		return rawURL, nil

	case path != "":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("读取文件失败 %s: %w", path, err)
		}
		if len(b) == 0 {
			return "", fmt.Errorf("文件为空: %s", path)
		}
		return dataURL(mimeByExtOr(path, kind), b), nil

	case ref != "":
		// ★ CAS 淘汰：digest 参数现收**媒体块引用**（块 ID 或其唯一前缀），
		//   由记忆层按块取字节 —— 不再有独立的 digest 寻址层。
		b, mime, ok, err := p.mediaBytes(ref)
		if err != nil {
			return "", fmt.Errorf("按块引用取媒体失败 %s: %w", ref, err)
		}
		if !ok {
			return "", fmt.Errorf("媒体块引用 %s 无对应块或无负载；"+
				"请传 path（设备/工具回传的文件）或 url（http(s) 直链）", ref)
		}
		if mime == "" {
			mime = fallbackMIME(kind)
		}
		return dataURL(mime, b), nil
	}
	return "", fmt.Errorf("没有可处理的媒体。请传 path（设备/工具回传的文件，如 screensee 的截图）、" +
		"digest（记忆里的媒体块引用），或 url（http(s) 直链）三者之一")
}

// resolveVideoInput 为 see_video 解析出一个 ffmpeg 能读的输入。
//
//	url    → 原样交给 ffmpeg（ffmpeg 原生支持 http 输入），无需下载
//	path   → 直接用该路径
//	digest → 从 CAS 取字节落到临时文件（ffmpeg 需要可寻址的输入）
//
// 返回的 cleanup 负责删掉临时文件（path/url 情形是 no-op）。
func (p *Plugin) resolveVideoInput(args map[string]interface{}) (input string, cleanup func(), err error) {
	noop := func() {}
	path := strings.TrimSpace(argStr(args, "path"))
	ref := strings.TrimSpace(argStr(args, "digest"))
	rawURL := strings.TrimSpace(argStr(args, "url"))

	switch {
	case rawURL != "":
		if !isHTTPURL(rawURL) {
			return "", noop, fmt.Errorf("url 必须是 http(s) 直链，实际: %q", rawURL)
		}
		return rawURL, noop, nil

	case path != "":
		if st, e := os.Stat(path); e != nil {
			return "", noop, fmt.Errorf("视频文件不可读 %s: %w", path, e)
		} else if st.Size() == 0 {
			return "", noop, fmt.Errorf("视频文件为空: %s", path)
		}
		return path, noop, nil

	case ref != "":
		// ★ CAS 淘汰：digest 参数现收**媒体块引用**，由记忆层取字节。
		b, _, ok, e := p.mediaBytes(ref)
		if e != nil {
			return "", noop, fmt.Errorf("按块引用取视频失败 %s: %w", ref, e)
		}
		if !ok {
			return "", noop, fmt.Errorf("媒体块引用 %s 无对应块或无负载；请传 path 或 url", ref)
		}
		f, e := os.CreateTemp("", "seemedia-*.video")
		if e != nil {
			return "", noop, fmt.Errorf("创建临时文件失败: %w", e)
		}
		if _, e := f.Write(b); e != nil {
			f.Close()
			os.Remove(f.Name())
			return "", noop, fmt.Errorf("写入临时视频失败: %w", e)
		}
		f.Close()
		name := f.Name()
		return name, func() { os.Remove(name) }, nil
	}
	return "", noop, fmt.Errorf("没有可处理的视频。请传 path、digest 或 url 之一")
}

// ── 抽帧 ───────────────────────────────────────────────────

// extractFrames 从视频里均匀抽 n 帧，返回每帧的 PNG 字节。
//
// 策略：先用 ffprobe 探时长，再在 duration*(i+0.5)/n 处逐帧快速定位（-ss 在 -i 前，
// 关键帧 seek，快）。探时长失败（直播流/损坏头）时退回单遍 fps 滤镜抽前 n 帧。
func (p *Plugin) extractFrames(input string, n int) ([][]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	if dur := p.probeDuration(ctx, input); dur > 0 {
		var frames [][]byte
		for i := 0; i < n; i++ {
			ts := dur * (float64(i) + 0.5) / float64(n)
			b, err := p.grabFrameAt(ctx, input, ts)
			if err != nil || len(b) == 0 {
				continue // 单帧失败不致命：有几帧投几帧
			}
			frames = append(frames, b)
		}
		if len(frames) > 0 {
			return frames, nil
		}
		// 定位抽帧全失败 → 落到单遍兜底
	}
	return p.grabFramesFPS(ctx, input, n)
}

func (p *Plugin) probeDuration(ctx context.Context, input string) float64 {
	if p.ffprobe == "" {
		return 0
	}
	out, err := exec.CommandContext(ctx, p.ffprobe,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		input,
	).Output()
	if err != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if d < 0 {
		return 0
	}
	return d
}

// grabFrameAt 在 ts 秒处抓 1 帧 PNG，输出到 stdout。
func (p *Plugin) grabFrameAt(ctx context.Context, input string, ts float64) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.ffmpeg,
		"-nostdin", "-loglevel", "error",
		"-ss", strconv.FormatFloat(ts, 'f', 3, 64),
		"-i", input,
		"-frames:v", "1",
		"-f", "image2pipe", "-c:v", "png",
		"pipe:1",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// grabFramesFPS 单遍抽前 n 帧（兜底，用于探不到时长的流）。
func (p *Plugin) grabFramesFPS(ctx context.Context, input string, n int) ([][]byte, error) {
	dir, err := os.MkdirTemp("", "seemedia-frames-")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(dir)

	cmd := exec.CommandContext(ctx, p.ffmpeg,
		"-nostdin", "-loglevel", "error",
		"-i", input,
		"-vf", "thumbnail",
		"-frames:v", strconv.Itoa(n),
		filepath.Join(dir, "f-%03d.png"),
	)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg 抽帧失败: %w", err)
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "f-*.png"))
	var frames [][]byte
	for _, e := range entries {
		if b, err := os.ReadFile(e); err == nil && len(b) > 0 {
			frames = append(frames, b)
		}
	}
	return frames, nil
}

// ── 纯助手 ─────────────────────────────────────────────────

func clampFrames(n int) int {
	if n < 1 {
		return defaultVideoFrames
	}
	if n > maxVideoFrames {
		return maxVideoFrames
	}
	return n
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func dataURL(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func mimeByExtOr(path, kind string) string {
	if m := mimeByExt(path); m != "" {
		return m
	}
	return fallbackMIME(kind)
}

func fallbackMIME(kind string) string {
	if kind == "audio" {
		return "audio/wav"
	}
	return "image/png"
}

func mimeByExt(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a", ".aac":
		return "audio/aac"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	}
	return ""
}

func argStr(args map[string]interface{}, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	}
	return 0
}

package seemedia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seemedia 让模型**一手**看图/看视频/听音频（SetToolBlocks 直投主模型），
// 与 multimodal（二手文字描述）并存。
//
// 这些判据钉住的是**不变量**，不是某次实现：
//
//	① 来源解析三条路（url 透传 / path 读盘转 data URL / digest 取 CAS）与优先级
//	② 没有任何来源时报错，且错误**可操作**（说清传什么）
//	③ 抽帧数永远被 clamp 到 [1, maxVideoFrames]——对齐内核 modalfallback 的约束
//	④ 直投工具必须声明 ParallelSafe:false（SetToolBlocks 写的是"当前 call"的块，
//	   并发多工具会串台）

// ---- ① / ② 来源解析 ----

func TestResolveMediaURL_URLPassthrough(t *testing.T) {
	p := &Plugin{}
	got, err := p.resolveMediaURL(map[string]interface{}{"url": "https://x/y.png"}, "image")
	if err != nil {
		t.Fatalf("http 直链应原样透传: %v", err)
	}
	if got != "https://x/y.png" {
		t.Errorf("url 应原样返回（不下载、不转 data URL），实际: %q", got)
	}
	// 非 http 的 url 要被拒（避免 file:// 之类绕过 path 分支的本地读）
	if _, err := p.resolveMediaURL(map[string]interface{}{"url": "file:///etc/passwd"}, "image"); err == nil {
		t.Error("非 http(s) 的 url 必须被拒绝")
	}
}

func TestResolveMediaURL_PathWinsOverDigest(t *testing.T) {
	p := &Plugin{}
	dir := t.TempDir()
	fp := filepath.Join(dir, "shot.jpg")
	if err := os.WriteFile(fp, []byte{0xFF, 0xD8, 0xFF, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	// path 与 digest 同时给：path 优先，且**不碰 CAS**（p.sdk 为 nil 也不该 panic）
	got, err := p.resolveMediaURL(map[string]interface{}{"path": fp, "digest": "deadbeef"}, "image")
	if err != nil {
		t.Fatalf("path 优先时应成功: %v", err)
	}
	if !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Errorf("应按扩展名给 image/jpeg data URL，实际前缀: %.40s", got)
	}
	// url 又比 path 优先
	got2, err := p.resolveMediaURL(map[string]interface{}{"url": "http://a/b", "path": fp}, "image")
	if err != nil || got2 != "http://a/b" {
		t.Errorf("url 应比 path 优先，实际: %q err=%v", got2, err)
	}
}

func TestResolveMediaURL_NoSourceIsActionable(t *testing.T) {
	p := &Plugin{}
	_, err := p.resolveMediaURL(map[string]interface{}{}, "image")
	if err == nil {
		t.Fatal("无任何来源时必须报错")
	}
	msg := err.Error()
	for _, want := range []string{"path", "digest", "url"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息要指明可传 %s，实际: %v", want, err)
		}
	}
}

func TestResolveMediaURL_FileErrorsSpecific(t *testing.T) {
	p := &Plugin{}
	if _, err := p.resolveMediaURL(map[string]interface{}{"path": "/nope/x.jpg"}, "image"); err == nil ||
		!strings.Contains(err.Error(), "/nope/x.jpg") {
		t.Errorf("读取失败应带路径，实际: %v", err)
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "e.png")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.resolveMediaURL(map[string]interface{}{"path": empty}, "image"); err == nil ||
		!strings.Contains(err.Error(), "空") {
		t.Errorf("空文件应明说「空」，实际: %v", err)
	}
}

func TestFallbackMIME(t *testing.T) {
	if fallbackMIME("audio") != "audio/wav" {
		t.Error("audio 兜底应为 audio/wav")
	}
	if fallbackMIME("image") != "image/png" {
		t.Error("image 兜底应为 image/png")
	}
	// 未知 kind 落到 image（图是最常见的一手媒体）
	if fallbackMIME("??") != "image/png" {
		t.Error("未知 kind 应兜底 image/png")
	}
}

// ---- ③ 抽帧数 clamp ----

func TestClampFrames(t *testing.T) {
	cases := map[int]int{
		-5:  defaultVideoFrames, // 非正数回默认，而不是 0 帧（0 帧=白调一次）
		0:   defaultVideoFrames,
		1:   1,
		6:   6,
		10:  10,
		11:  maxVideoFrames, // 超限夹到上限，不放任上游超体积
		999: maxVideoFrames,
	}
	for in, want := range cases {
		if got := clampFrames(in); got != want {
			t.Errorf("clampFrames(%d)=%d，期望 %d", in, got, want)
		}
	}
	// 不变量：上限必须 ≥ 默认，否则默认值会被自己夹掉
	if maxVideoFrames < defaultVideoFrames {
		t.Fatal("maxVideoFrames 不能小于 defaultVideoFrames")
	}
}

func TestMimeByExt(t *testing.T) {
	for p, want := range map[string]string{
		"a.jpg": "image/jpeg", "a.JPEG": "image/jpeg", "a.png": "image/png",
		"a.webp": "image/webp", "a.gif": "image/gif", "a.bmp": "image/bmp",
		"a.wav": "audio/wav", "a.mp3": "audio/mpeg", "a.flac": "audio/flac",
		"a.bin": "", "noext": "",
	} {
		if got := mimeByExt(p); got != want {
			t.Errorf("mimeByExt(%q)=%q，期望 %q", p, got, want)
		}
	}
}

func TestIsHTTPURL(t *testing.T) {
	for s, want := range map[string]bool{
		"http://a":   true,
		"https://a":  true,
		"ftp://a":    false,
		"file:///x":  false,
		"/local/x":   false,
		"data:image": false,
	} {
		if got := isHTTPURL(s); got != want {
			t.Errorf("isHTTPURL(%q)=%v，期望 %v", s, got, want)
		}
	}
}

// ---- ④ 直投工具的并发契约 ----
//
// SetToolBlocks 写的是"当前 call"的媒体块（IOManager.SetToolBlocks 无 call_id
// 时的兼容入口），并发执行多个直投工具会把彼此的块串到一起。所以三个工具都
// 必须 ParallelSafe:false。这条判据跨实现有效：谁把它改回 true，谁就引入串台。
func TestSeeToolsAreNotParallelSafe(t *testing.T) {
	src, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	// 不得出现 ParallelSafe:true（本插件所有工具都直投，必须串行）
	if strings.Contains(strings.ReplaceAll(s, " ", ""), "ParallelSafe:true") {
		t.Error("seemedia 的直投工具不得声明 ParallelSafe:true —— " +
			"SetToolBlocks 写的是当前 call 的块，并发会串台")
	}
}

// ---- ⑤ 不得承诺「你会听到」 ----
//
// 2026-10-11 e2e 抓出：hear_audio 的注册描述写死「你会直接听到它」，而当前
// 部署里所有 LLM 源都是 audio=false、且音频回退源为空 ⇒ 内核既不能直视也无法
// 回退，只发一条「拿不到音频」的 notice。模型看到「你会听到」+「拿不到」两句
// 自相矛盾的话，转而去数 WAV 字节（实测）。
//
// 插件在调用时**无法**得知当前模型是否支持音频（SDK 未暴露能力查询），
// 所以描述必须如实说「可能拿不到」，并把出路（transcribe_audio / 配置回退源）
// 写清楚。这条判据跨实现有效：谁把承诺改回去，谁就重建了那个自相矛盾。
func TestHearAudioDoesNotPromiseAudibility(t *testing.T) {
	src, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)

	// 钉住 RegisterTool("hear_audio" 这一段（到该注册调用结束为止）。
	// ★ 不能用「下一个 RegisterTool」做终点：hear_audio 是**最后一个**注册，
	//   那样会一直截到文件末尾，把 handler 的 note 也吞进来 —— 于是
	//   note 里残留的 transcribe_audio 会让本判据误判为「描述合格」。
	//   （这个 bug 是变异测试抓出来的：改掉描述里的出路却仍 PASS。）
	i := strings.Index(s, `RegisterTool("hear_audio"`)
	if i < 0 {
		t.Fatal("找不到 hear_audio 注册")
	}
	seg := s[i:]
	// 终点：该 RegisterTool 调用对应的 "}, p.handleHearAudio)" 之后。
	en := strings.Index(seg, "p.handleHearAudio)")
	if en < 0 {
		t.Fatal("找不到 hear_audio 注册调用的结尾")
	}
	seg = seg[:en]
	// ★ 剔除注释行：本文件的注释里会**引用旧文案**说明为何不得这么写，
	// 那不是承诺。判据只应检查真正会送到模型面前的字符串（Description/note）。
	var code []string
	for _, line := range strings.Split(seg, "\n") {
		t2 := strings.TrimSpace(line)
		if strings.HasPrefix(t2, "//") {
			continue
		}
		code = append(code, line)
	}
	seg = strings.Join(code, "\n")

	// ① 不得出现无条件承诺
	for _, bad := range []string{"你会直接听到它", "你会直接听到"} {
		if strings.Contains(seg, bad) {
			t.Errorf("hear_audio 描述不得无条件承诺可听性（当前配置下会与内核 notice 矛盾）: 含 %q", bad)
		}
	}
	// ② 必须说明「拿不到时怎么办」——即指向 transcribe_audio
	if !strings.Contains(seg, "transcribe_audio") {
		t.Error("hear_audio 描述必须指明拿不到音频时的出路（transcribe_audio）")
	}
	// ③ 必须点明可能不支持（诚实降级），不能只说好话
	if !strings.Contains(seg, "不支持") {
		t.Error("hear_audio 描述必须如实说明「当前模型/回退源可能不支持音频」")
	}
}

// TestHearAudioNoteIsHonest 钉住 handler 返回的 note 同样不做虚假承诺。
func TestHearAudioNoteIsHonest(t *testing.T) {
	src, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func (p *Plugin) handleHearAudio(")
	if i < 0 {
		t.Fatal("找不到 handleHearAudio")
	}
	seg := s[i:]
	if j := strings.Index(seg, "\nfunc "); j > 0 {
		seg = seg[:j]
	}
	if strings.Contains(seg, "请在下一步直接聆听") {
		t.Error("handleHearAudio 的 note 不得说「直接聆听」——插件无法保证当前模型支持音频")
	}
	if !strings.Contains(seg, "transcribe_audio") {
		t.Error("handleHearAudio 的 note 必须指明听不到时的替代（transcribe_audio）")
	}
}

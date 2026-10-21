package remotedevice

// 设备输出通道（outputch.go）的测试：
//   - caps 映射词表
//   - 上下线 → 通道登记/注销 + push 真能落到设备（走真 WS 帧）
//   - 聚合通道 devicectl 的出站寻址

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func TestDeviceOutputCapsMapping(t *testing.T) {
	full := agentIO.CapText | agentIO.CapFile | agentIO.CapImage | agentIO.CapAudio | agentIO.CapStructured
	cases := []struct {
		name string
		caps []string
		kind string
		want agentIO.OutputCapability
	}{
		{"未声明已知能力 ⇒ 全能力（旧设备兼容）", nil, "", full},
		{"未知 caps ⇒ 全能力", []string{"whatever"}, "", full},
		{"cmd ⇒ 历史全能力", []string{"cmd"}, "", full},
		{"speaker ⇒ 文本+音频", []string{"speaker"}, "", agentIO.CapText | agentIO.CapAudio},
		{"screen ⇒ 文本+图+文件", []string{"screen"}, "", agentIO.CapText | agentIO.CapImage | agentIO.CapFile},
		{"clipboard ⇒ 文本+文件", []string{"clipboard"}, "", agentIO.CapText | agentIO.CapFile},
		{"kind=computer 兜底（未声明 caps）", nil, "computer", agentIO.CapText | agentIO.CapImage | agentIO.CapFile | agentIO.CapStructured},
		{"kind=speaker 兜底", nil, "speaker", agentIO.CapText | agentIO.CapAudio},
	}
	for _, c := range cases {
		if got := deviceOutputCaps(c.caps, c.kind); got != c.want {
			t.Errorf("%s: deviceOutputCaps(%v,%q)=%s，期望 %s", c.name, c.caps, c.kind, got, c.want)
		}
	}
}

// channelRecorder 记录通道注册/注销。
//
// 必须加锁：注册/注销发生在设备 WS 的处理 goroutine（上下线回调）里，
// 而测试在主线读 —— 裸 map/slice 会被 -race 抓住（第一版就是这么被抓住的）。
type channelRecorder struct {
	mu           sync.Mutex
	registered   map[string]int
	unregistered []string
}

func (r *channelRecorder) caps(name string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.registered[name]
	return c, ok
}

func (r *channelRecorder) unregList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.unregistered...)
}

// recordingSDK 造一个能记账的 PluginSDK：通道注册/注销都落到 recorder 里。
func recordingSDK(t *testing.T, iom *agentIO.IOManager, rec *channelRecorder) *sdk.PluginSDK {
	t.Helper()
	return sdk.New("remotedevice", sdk.SDKConfig{
		IOManager: iom,
		RegOutput: func(name string, caps int, desc string, def sdk.ChannelDef, handler sdk.ToolHandler) error {
			rec.mu.Lock()
			rec.registered[name] = caps
			rec.mu.Unlock()
			// 通道 handler 也要真的可调用 —— 记进 iom 才能从外面触发。
			return iom.RegisterDevice(&recordingDevice{name: name, handler: handler})
		},
		RegOutputUnreg: func(name string) error {
			rec.mu.Lock()
			rec.unregistered = append(rec.unregistered, name)
			rec.mu.Unlock()
			iom.UnregisterDevice(name)
			return nil
		},
		RegInput: func(name string, def sdk.ChannelDef) error {
			iom.RegisterInputChannel(name, agentIO.ChannelDef(def))
			return nil
		},
	})
}

// recordingDevice 把插件注册的输出通道在 io 层落地，便于用 Execute("output") 触发。
type recordingDevice struct {
	name    string
	handler sdk.ToolHandler
}

func (d *recordingDevice) Name() string                                 { return d.name }
func (d *recordingDevice) Type() agentIO.DeviceType                     { return agentIO.DeviceIO }
func (d *recordingDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *recordingDevice) Description() string                          { return "recording device" }
func (d *recordingDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }
func (d *recordingDevice) Start() error                                 { return nil }
func (d *recordingDevice) Stop() error                                  { return nil }
func (d *recordingDevice) Tools() []agentIO.ToolDef                     { return nil }
func (d *recordingDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return d.handler(args)
}

func TestDeviceChannelLifecycleAndPush(t *testing.T) {
	reg := NewRegistry()
	token := "tk"
	reg.SetAcceptToken(func(p string) bool { return p == token })

	iom := agentIO.NewIOManager()
	rec := &channelRecorder{registered: map[string]int{}}

	p := &Plugin{registry: reg}
	p.sdk = recordingSDK(t, iom, rec)
	p.wireDeviceChannels() // 走 Start 的同一条接线

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	// 设备上线（caps=speaker ⇒ 通道能力应为 文本+音频）
	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"spk-1","name":"音箱","kind":"speaker","caps":["speaker"]}}`))
	cli.readHelloAckAndBind(t, token)

	ch := p.deviceChannelName("spk-1")
	deadline := time.Now().Add(3 * time.Second)
	caps, ok := rec.caps(ch)
	for !ok && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		caps, ok = rec.caps(ch)
	}
	if !ok {
		t.Fatalf("设备上线后应注册输出通道 %s，实际: %v", ch, rec.unregList())
	}
	if want := int(agentIO.CapText | agentIO.CapAudio); caps != want {
		t.Fatalf("通道能力位应为 文本+音频(%d)，实际 %d", want, caps)
	}
	// 入站 inputch 同名登记（父 agent 才能把"这台设备"划给驻留子）
	if _, ok := iom.LookupInputChannel(ch); !ok {
		t.Fatalf("设备上线后应同时登记同名 inputch %s", ch)
	}

	// 触发一次出站：走 io 层的 output 分发（与 output_send__<通道> 同一条路）
	dev := iom.GetDevice(ch)
	if dev == nil {
		t.Fatalf("输出通道 %s 未在 io 层注册", ch)
	}
	if _, err := dev.Execute("output", map[string]interface{}{"payload": "你好，设备", "type": "text"}); err != nil {
		t.Fatalf("向设备发送失败: %v", err)
	}

	// 设备侧应收到 op=push 的帧
	got := make(chan map[string]interface{}, 1)
	go func() {
		_, payload, err := cli.readMsg()
		if err != nil {
			return
		}
		var m map[string]interface{}
		if json.Unmarshal(payload, &m) == nil {
			got <- m
		}
	}()
	select {
	case m := <-got:
		if m["op"] != "push" {
			t.Fatalf("设备应收到 op=push，实际 %v", m)
		}
		if m["payload"] != "你好，设备" || m["type"] != "text" {
			t.Fatalf("push 帧内容不符: %v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("设备未收到 push 帧")
	}

	// 设备下线 ⇒ 注销通道（不留死通道）
	cli.close()
	deadline = time.Now().Add(3 * time.Second)
	for len(rec.unregList()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := rec.unregList(); len(got) == 0 || got[0] != ch {
		t.Fatalf("设备下线后应注销 %s，实际 %v", ch, got)
	}
	if iom.GetDevice(ch) != nil {
		t.Fatalf("注销后 io 层不应还有 %s", ch)
	}
}

func TestDevicectlAggregateOutputAddressing(t *testing.T) {
	reg := NewRegistry()
	token := "tk2"
	reg.SetAcceptToken(func(p string) bool { return p == token })
	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	dev := &devicectlDevice{reg: reg}

	// ① 没指定设备 ⇒ 报错要**可执行**（列出在线设备），而不是含糊失败
	if _, err := dev.Execute("output", map[string]interface{}{"payload": "x", "type": "text"}); err == nil {
		t.Fatal("无 device_id 时应报错")
	}

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()
	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"pc-1","name":"PC","kind":"computer","caps":["cmd"]}}`))
	cli.readHelloAckAndBind(t, token)

	deadline := time.Now().Add(3 * time.Second)
	for !reg.Online("pc-1") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !reg.Online("pc-1") {
		t.Fatal("设备未上线")
	}

	// ② meta 是 JSON 且含 device_id ⇒ 投递到该设备
	if _, err := dev.Execute("output", map[string]interface{}{
		"payload": "hi", "type": "text", "meta": `{"device_id":"pc-1"}`,
	}); err != nil {
		t.Fatalf("按 meta.device_id 投递失败: %v", err)
	}
	m := make(chan map[string]interface{}, 1)
	go func() {
		_, payload, err := cli.readMsg()
		if err != nil {
			return
		}
		var got map[string]interface{}
		if json.Unmarshal(payload, &got) == nil {
			m <- got
		}
	}()
	select {
	case got := <-m:
		if got["op"] != "push" {
			t.Fatalf("应为 push 帧，实际 %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("聚合通道未投递到设备")
	}

	// ③ 设备在线但指定了不存在的设备 ⇒ 报错（online 列表里有 pc-1）
	if _, err := dev.Execute("output", map[string]interface{}{
		"payload": "hi", "type": "text", "device_id": "ghost",
	}); err == nil {
		t.Fatal("不存在的设备应报错")
	}
}

// 通道名合规性：设备通道名会被内核拼进 LLM **函数名**（output_send__<通道名>），
// 而上游函数名规范是 ^[a-zA-Z0-9_-]{1,64}$ —— 违规会让**整条请求**被 400 拒绝
// （实测把生产打挂：device/<id> 里的 `/` 触发 Invalid 'tools[299].function.name'，
// 网关 auto tier 全链条失败，整个 agent 不说话了）。
//
// 通道名是**插件自己的声明**，所以这条判据钉在插件侧。
func TestDeviceChannelNameIsLLMFunctionNameSafe(t *testing.T) {
	re := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	// 含**恶意/异常** id：空格、符号、非 ASCII、超长、以及会折成同一个名字的两个 id
	ids := []string{"waiter-fnnas", "1", "a b!c", "中文设备", strings.Repeat("x", 120), "a b", "a-b"}
	p := &Plugin{}
	seen := map[string]string{}
	for _, id := range ids {
		ch := p.deviceChannelName(id)
		if prev, dup := seen[ch]; dup {
			t.Errorf("不同设备 id（%q 与 %q）派生出同一个通道名 %q", prev, id, ch)
		}
		seen[ch] = id
		if !re.MatchString(ch) {
			t.Errorf("设备通道名 %q 违反上游函数名规范 %s", ch, re)
		}
		toolName := "output_send__" + ch
		if !re.MatchString(toolName) {
			t.Errorf("派生出的工具名 %q 违反上游函数名规范 %s", toolName, re)
		}
	}
}

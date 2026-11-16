package test

// remotedevice 的**设备桥闭环**判据 —— 真 WebSocket，两端都是生产代码。
//
// ## 为什么必须真跑协议
//
// 现有 3 条判据覆盖 Registry 的进程内表（结果存取、能力查询、媒体落盘），
// 但**上线/下线/建链/命令往返**这条主线一条都没有。而这条主线的每个环节
// 都出过真实缺陷：
//
//	· bind_ack 的 ok 没人读 ⇒ 服务端拒绝 bind 后设备"连接成功但失联"
//	  （GUI 侧同样病灶，见 cmd/gui/main.js 的 bind_ack 注释）
//	· 能力声明与工具裁剪不一致（caps 空 ⇒ 判成"全能力"，设计如此，
//	  但一旦写错就是静默放大权限）
//
// 判据形态：起一个 httptest 服务端跑 `Registry.ServeWS`（真代码），
// 客户端用 `devicebridge/client.Bridge`（真代码）连上来，
// 断言只从公开方法读：`Online` / `Get` / `List` / `OnCmd` 回调 / `SendResult`。
//
// ★ 两侧都是生产代码 —— 不写 mock 对端。这样"协议改了一边没改另一边"
// 一定红，而不是两边一起漂。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bridgeclient "github.com/JianFeeeee/HologramAgent/internal/devicebridge/client"
	"github.com/JianFeeeee/HologramAgent/internal/plugins/remotedevice"
)

// newBridgeServer 起一个真 WebSocket 网关，返回 ws URL。
func newBridgeServer(t *testing.T, reg *remotedevice.Registry) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	t.Cleanup(srv.Close)
	return "ws://" + srv.Listener.Addr().String()
}

// waitOnline 是**判据用的等待**，不是被测代码。
//
// 设备上线是异步的（建链 → hello → bind_ack → register），所以判据必须等
// 一个可观察状态出现，而不是 sleep 一个魔数秒数。
func waitOnline(t *testing.T, reg *remotedevice.Registry, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reg.Online(id) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("★ 设备 %s 在 3 秒内没上线（Online()=false，List()=%d 台）", id, len(reg.List()))
}

// ---- 判据 1：设备 bind 后必须真的登记上线 ----

// TestDeviceComesOnlineAfterBind：客户端完成 hello + bind，网关必须把它记为在线。
//
// ★ 这条判据的由来：bind 是被**双条件**把关的（token 通过 AND id 与 hello
// 里声明的一致）。任一条不满足就回 `bind_ack ok=false` 并立即断开。
// 而历史的客户端实现**不读 ok** —— 于是"连接已建立"被当成"设备已在线"，
// 命令永远下发不到，日志上却一切正常。
//
// 断言只看 `Online`/`Get`：那是网关的可观察事实，不是内部字段。
func TestDeviceComesOnlineAfterBind(t *testing.T) {
	reg := remotedevice.NewRegistry()
	reg.SetAcceptToken(func(tok string) bool { return tok == "good-token" })
	url := newBridgeServer(t, reg)

	b := bridgeclient.New(url, "good-token", "dev-1", "测试设备", []string{"screensee"}, nil)
	// 客户端本地授权（用户在设备上手动开启）—— 与网关的 acceptToken 是两件事。
	b.SetAuthorized(true)
	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { b.Stop() })

	waitOnline(t, reg, "dev-1")

	m, ok := reg.Get("dev-1")
	if !ok {
		t.Fatal("★ Online 为真但 Get 取不到该设备 —— 两个读面不一致")
	}
	if m.DeviceID != "dev-1" || m.Name != "测试设备" {
		t.Fatalf("★ 登记的身份不对（hello 声明的没被采用）：%+v", m)
	}
	if !registryListHas(reg.List(), "dev-1") {
		t.Fatalf("★ List() 里没有该设备（WebUI 设备列表会漏显示）")
	}
}

// ---- 判据 2：token 不对 ⇒ 建链阶段就必须拒绝 ----

// TestBadTokenIsRejectedAtDial：token 校验失败时，客户端**连不上**（而不是
// 连上后被当成在线）。
//
// 实测协议：`ServeWS` 在握手前就校验 token（查 query / X-API-Key /
// `homeagent.<token>` 子协议三种来源），不通过直接 401，连 WS upgrade
// 都不做。所以它比 bind 更早 —— 闸在入口。
//
// ★ 这条断言的是"拒绝是响亮的"：`Start()` 返回错误、设备不在线。
//
//	反例形态（历史缺陷）是"连接成功但设备失联，日志一切正常"。
func TestBadTokenIsRejectedAtDial(t *testing.T) {
	reg := remotedevice.NewRegistry()
	reg.SetAcceptToken(func(tok string) bool { return tok == "good-token" })
	url := newBridgeServer(t, reg)

	b := bridgeclient.New(url, "bad-token", "dev-2", "冒名设备", nil, nil)
	b.SetAuthorized(true)
	err := b.Start()
	if err == nil {
		b.Stop()
		t.Fatal("★ 错误 token 的设备 Start() 成功了 —— 准入闸失效")
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("★ 拒绝理由不可辨认（调用方无从得知是凭据问题）：%v", err)
	}
	if reg.Online("dev-2") {
		t.Fatal("★ 被拒的设备仍出现在在线列表")
	}
	if len(reg.List()) != 0 {
		t.Fatalf("★ 被拒的设备留下了登记：%v", reg.List())
	}
}

// ---- 判据 3：能力声明决定工具可用性 ----

// TestCapabilityDeclarationsGateTools：设备的 caps 决定网关是否放行某个工具。
//
// 语义（源码注释写明的三条，这里逐条钉住可观察结果）：
//   - 未声明任何已知能力 ⇒ 视为全能力（旧设备兼容，不能把它们弄失联）
//   - 声明了兼容全能力标记（cmd/cmdrun）⇒ 全能力
//   - 否则严格按声明匹配
//
// ★ 为什么这条重要：caps 写错的方向决定了是"功能不可用"还是"权限被放大"。
// 前一种用户会来报，后一种没人会报。
func TestCapabilityDeclarationsGateTools(t *testing.T) {
	reg := remotedevice.NewRegistry()
	url := newBridgeServer(t, reg)

	reg.SetAcceptToken(func(tok string) bool { return tok == "tok" })
	cases := []struct {
		deviceID string
		caps     []string
		want     map[string]bool // tool → 期望是否放行
	}{
		{
			deviceID: "dev-screen",
			caps:     []string{"screensee"},
			// 声明了 screensee ⇒ 只有映射表里属于 screensee 的工具可用。
			want: map[string]bool{"screensee": true},
		},
		{
			deviceID: "dev-legacy",
			caps:     nil, // 一个都没声明 ⇒ 旧设备兼容，全能力
			want:     map[string]bool{"screensee": true, "camerasue": true},
		},
		{
			deviceID: "dev-full",
			caps:     []string{"cmdrun"}, // 兼容全能力标记
			want:     map[string]bool{"screensee": true, "camerasue": true},
		},
	}

	var bridges []*bridgeclient.Bridge
	for _, c := range cases {
		b := bridgeclient.New(url, "tok", c.deviceID, c.deviceID, c.caps, nil)
		b.SetAuthorized(true)
		if err := b.Start(); err != nil {
			t.Fatalf("%s Start: %v", c.deviceID, err)
		}
		bridges = append(bridges, b)
		waitOnline(t, reg, c.deviceID)
	}
	t.Cleanup(func() {
		for _, b := range bridges {
			b.Stop()
		}
	})

	for _, c := range cases {
		for tool, want := range c.want {
			if got := reg.SupportsTool(c.deviceID, tool); got != want {
				t.Errorf("★ %s(caps=%v) 的 %q 可用性=%v，期望 %v",
					c.deviceID, c.caps, tool, got, want)
			}
		}
	}
	// 反例：声明 screensee 的设备不该被当成全能力。
	if reg.SupportsTool("dev-screen", "camerasue") {
		t.Fatal("★ 只声明 screensee 的设备却放行了 camerasue —— 能力裁剪失效")
	}
}

// ---- 判据 4：命令下发 → 设备执行 → 结果回传 ----

// TestCommandRoundTripReachesDeviceAndReturnsResult：网关下发命令，设备侧真的
// 收到回调；设备回执后，网关侧能查到结果。
//
// 这条把整条主线串起来：PushCmd → WS 帧 → 客户端 readLoop → OnCmd 回调
// → SendResult → 服务端路由到对应 reqID → GetResult。
//
// ★ 只断言**收到的是什么**和**结果能查回**，不注入任何 mock 中间层。
func TestCommandRoundTripReachesDeviceAndReturnsResult(t *testing.T) {
	reg := remotedevice.NewRegistry()
	url := newBridgeServer(t, reg)
	reg.SetAcceptToken(func(tok string) bool { return tok == "tok" })

	b := bridgeclient.New(url, "tok", "dev-cmd", "执行端", []string{"cmdrun"}, nil)
	b.SetAuthorized(true)

	gotCmd := make(chan [2]string, 1) // {command, cmdType}
	b.OnCmd(func(reqID, command, cmdType string) {
		select {
		case gotCmd <- [2]string{command, cmdType}:
		}
		// 设备执行完回执（真实现里是子进程跑完再来这里，判据里直接回）。
		b.SendResult(reqID, "ok", "executed: "+command, "")
	})
	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { b.Stop() })
	waitOnline(t, reg, "dev-cmd")

	if err := reg.PushCmd("dev-cmd", "req-1", "ls -la", "shell"); err != nil {
		t.Fatalf("PushCmd: %v", err)
	}

	select {
	case c := <-gotCmd:
		if c[0] != "ls -la" || c[1] != "shell" {
			t.Fatalf("★ 设备收到的命令不符：cmd=%q type=%q", c[0], c[1])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("★ 3 秒内设备没收到命令 —— 下发链路断了（连接在但注册/路由有问题）")
	}

	// 回执必须能按 reqID 查回（remotedevice 的工具实现就是这么取结果的）。
	// 等待是必要的：回执是异步回来的另一条消息。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if res, ok := reg.GetResult("req-1"); ok {
			if res["status"] != "ok" {
				t.Fatalf("★ 回执状态不对：%+v", res)
			}
			if out, _ := res["output"].(string); out != "executed: ls -la" {
				t.Fatalf("★ 回执输出不对：%q", out)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("★ 3 秒内查不到 req-1 的结果 —— 回执链路断了")
}

// registryListHas 判断 List() 结果里有没有某设备。
func registryListHas(list []remotedevice.DeviceMeta, id string) bool {
	for _, m := range list {
		if m.DeviceID == id {
			return true
		}
	}
	return false
}

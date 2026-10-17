//go:build ignore

// slowshakeplugin 故意**延迟回复握手**，用于验证「插件自行声明的建链超时」真的生效。
//
// 为什么需要这个测试替身（而不是复用现有的 echoplugin）：
// 现有插件都**立即**回握手，于是"超时值"这个参数无论取多少都不会改变行为 ——
// 用它们写的判据必然假绿（删掉超时逻辑也不报红）。
// 要真正观测超时，被观测的插件必须**慢到能让不同超时值产生不同结果**。
//
// 延迟由环境变量 SLOW_SHAKE_MS 控制（毫秒），由判据按用例设置：
//
//	· 声明超时 1s + 延迟 3s  ⇒ 必须**失败**（证明短超时真的被用上）
//	· 声明超时 10s + 延迟 3s ⇒ 必须**成功**（证明长超时真的被用上）
//
// 握手契约见 internal/plugin/proc/process.go 的 handshake()：
// 内核发 method="handshake"，插件回 {result:{sdk_version:...}}；
// 主版本号必须与内核一致，否则握手被拒（那会让判据因无关原因失败）。
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"time"
)

type request struct {
	ID     uint64          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     uint64      `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

func main() {
	delay := 0
	if v := os.Getenv("SLOW_SHAKE_MS"); v != "" {
		delay, _ = strconv.Atoi(v)
	}

	in := bufio.NewScanner(bufio.NewReader(os.Stdin))
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	send := func(v interface{}) {
		b, _ := json.Marshal(v)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			continue
		}
		switch req.Method {
		case "handshake":
			// ★ 关键：故意延迟，且**在延迟期间不读下一帧** ——
			// 真实的慢插件就是这样（忙着初始化，来不及应答）。
			if delay > 0 {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
			// sdk_version 的主版本必须与内核一致（否则握手会因版本不匹配被拒，
			// 判据就测不到超时这个变量了）。
			send(response{ID: req.ID, Result: map[string]interface{}{
				"sdk_version": "1.4.0",
			}})
		default:
			send(response{ID: req.ID, Result: map[string]interface{}{}})
		}
	}
}

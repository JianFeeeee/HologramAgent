package lua

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

//go:embed adapters/*.lua
var bundledAdapters embed.FS

// adapterGlobal 是每个 worker state 中保存适配器表的保留全局名。
const adapterGlobal = "__ha_adapter"

type APIAdapter struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// worker 封装一个独立的 gopher-lua 解释器。LState 非并发安全，
// 同一时刻只能被一个 goroutine 使用（从池中取出）。
type worker struct {
	L *lua.LState
}

func (w *worker) close() {
	if w.L != nil {
		w.L.Close()
		w.L = nil
	}
}

// staticInfo 缓存适配器脚本在加载时提取出的不可变字段，
// Endpoint/Headers 等读取不需要占用池 worker。
type staticInfo struct {
	name     string
	version  string
	endpoint string
	headers  map[string]string
}

// adapterPool 管理单个适配器的 worker 池。空闲 worker 存放在 idle 切片，
// 按 target 惰性创建；同一 worker 同一时刻只被一个 goroutine 使用。
type adapterPool struct {
	name   string
	script string
	static staticInfo

	mu      sync.Mutex
	cond    *sync.Cond
	idle    []*worker
	created int
	target  int
	closed  bool
}

func newAdapterPool(name, script string, static staticInfo) *adapterPool {
	p := &adapterPool{name: name, script: script, static: static, target: 1}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *adapterPool) setTarget(n int) {
	p.mu.Lock()
	p.target = n
	p.cond.Broadcast()
	p.mu.Unlock()
}

// boot 创建全新 gopher-lua 状态：注册共享全局、执行适配器脚本、
// 把返回表存到保留全局。compile Lua 的成本较高，尽量复用池。
func (p *adapterPool) boot() (*worker, error) {
	L := lua.NewState()
	setupGlobals(L)
	if err := L.DoString(p.script); err != nil {
		L.Close()
		return nil, fmt.Errorf("compile adapter %s: %w", p.name, err)
	}
	tbl, ok := L.Get(-1).(*lua.LTable)
	L.Pop(1)
	if !ok {
		L.Close()
		return nil, fmt.Errorf("adapter %s must return a table", p.name)
	}
	L.SetGlobal(adapterGlobal, tbl)
	return &worker{L: L}, nil
}

func (p *adapterPool) acquire() (*worker, error) {
	p.mu.Lock()
	for {
		if p.closed {
			p.mu.Unlock()
			return nil, fmt.Errorf("adapter %s pool closed", p.name)
		}
		if n := len(p.idle); n > 0 {
			w := p.idle[n-1]
			p.idle = p.idle[:n-1]
			p.mu.Unlock()
			return w, nil
		}
		if p.created < p.target {
			p.created++
			break
		}
		p.cond.Wait()
	}
	p.mu.Unlock()

	w, err := p.boot()
	if err != nil {
		p.mu.Lock()
		p.created--
		p.cond.Signal()
		p.mu.Unlock()
		return nil, err
	}
	return w, nil
}

func (p *adapterPool) release(w *worker) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		w.close()
		return
	}
	p.idle = append(p.idle, w)
	p.cond.Signal()
	p.mu.Unlock()
}

func (p *adapterPool) shutdown() {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.cond.Broadcast()
	p.mu.Unlock()
	for _, w := range idle {
		w.close()
	}
}

// VM 聚合各适配器的 worker 池。所有导出方法并发安全。
type VM struct {
	mu    sync.RWMutex
	dir   string
	pools map[string]*adapterPool
}

func NewVM(dir string) *VM {
	return &VM{dir: dir, pools: map[string]*adapterPool{}}
}

func (v *VM) AdapterDir() string { return v.dir }

func (v *VM) Start() error {
	if err := os.MkdirAll(v.dir, 0755); err != nil {
		return fmt.Errorf("mkdir adapter dir: %w", err)
	}
	if err := v.writeBundledAdapters(); err != nil {
		return fmt.Errorf("write bundled adapters: %w", err)
	}

	entries, err := os.ReadDir(v.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".lua" {
			continue
		}
		path := filepath.Join(v.dir, entry.Name())
		if err := v.LoadAdapter(path); err != nil {
			fmt.Printf("[lua] preload %s: %v\n", entry.Name(), err)
		}
	}
	return nil
}

func (v *VM) Stop() {
	v.mu.Lock()
	pools := v.pools
	v.pools = map[string]*adapterPool{}
	v.mu.Unlock()
	for _, p := range pools {
		p.shutdown()
	}
}

// ConfigureConcurrency sets each adapter's worker target to the sum of every
// source's max concurrent calls that share the adapter. Clamped >= 1.
func (v *VM) ConfigureConcurrency(adapterConcurrency map[string]int) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	for name, n := range adapterConcurrency {
		if p, ok := v.pools[name]; ok {
			if n < 1 {
				n = 1
			}
			p.setTarget(n)
		}
	}
}

func (v *VM) LoadAdapter(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read adapter: %w", err)
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return v.LoadAdapterSource(name, string(data))
}

func (v *VM) LoadAdapterSource(name, code string) error {
	static, err := inspectScript(name, code)
	if err != nil {
		return err
	}
	target := 1
	v.mu.RLock()
	if p, ok := v.pools[static.name]; ok {
		target = p.target
	}
	v.mu.RUnlock()

	v.mu.Lock()
	if p, ok := v.pools[static.name]; ok {
		p.shutdown()
	}
	p := newAdapterPool(static.name, code, *static)
	p.setTarget(target)
	v.pools[static.name] = p
	v.mu.Unlock()

	// 立即 boot 一个 worker，把编译成本放到加载阶段而不是首个请求。
	w, err := p.boot()
	if err != nil {
		return err
	}
	p.release(w)
	return nil
}

func (v *VM) RemoveAdapter(name string) {
	v.mu.Lock()
	p := v.pools[name]
	delete(v.pools, name)
	v.mu.Unlock()
	if p != nil {
		p.shutdown()
	}
}

func (v *VM) ListAdapters() []APIAdapter {
	v.mu.RLock()
	defer v.mu.RUnlock()
	list := make([]APIAdapter, 0, len(v.pools))
	for name, p := range v.pools {
		list = append(list, APIAdapter{Name: name, Version: p.static.version})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

func (v *VM) pool(name string) *adapterPool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.pools[name]
}

// callMethod 在池 worker 上执行 adapter 的 fn(raw) 并返回字符串结果。
func callMethod(p *adapterPool, fn, raw string) (string, error) {
	w, err := p.acquire()
	if err != nil {
		return "", err
	}
	defer p.release(w)
	L := w.L
	adapter := L.GetGlobal(adapterGlobal)
	tbl, ok := adapter.(*lua.LTable)
	if !ok {
		return "", fmt.Errorf("adapter %s has no table", p.name)
	}
	f := tbl.RawGetString(fn)
	if _, ok := f.(*lua.LFunction); !ok {
		return "", fmt.Errorf("adapter %s missing %s", p.name, fn)
	}
	L.Push(f)
	L.Push(lua.LString(raw))
	if err := L.PCall(1, 1, nil); err != nil {
		return "", fmt.Errorf("%s: %w", fn, err)
	}
	out := L.Get(-1)
	L.Pop(1)
	return out.String(), nil
}

func (v *VM) CallTransformRequest(name, rawJSON string) (string, error) {
	p := v.pool(name)
	if p == nil {
		return "", fmt.Errorf("adapter %s not loaded", name)
	}
	return callMethod(p, "transform_request", rawJSON)
}

func (v *VM) CallTransformResponse(name, rawJSON string) (string, error) {
	p := v.pool(name)
	if p == nil {
		return rawJSON, nil
	}
	out, err := callMethod(p, "transform_response", rawJSON)
	if err != nil {
		return "", err
	}
	return out, nil
}

// TransformError 执行可选的 adapter.transform_error(status, body) 钩子。
// 返回 reason: 适配器提取的错误原因；ok=false 表示适配器未定义此钩子。
func (v *VM) TransformError(name string, status int, body string) (reason string, ok bool, err error) {
	p := v.pool(name)
	if p == nil {
		return "", false, nil
	}
	w, err := p.acquire()
	if err != nil {
		return "", false, err
	}
	defer p.release(w)
	L := w.L
	adapter := L.GetGlobal(adapterGlobal)
	tbl, ok2 := adapter.(*lua.LTable)
	if !ok2 {
		return "", false, nil
	}
	f := tbl.RawGetString("transform_error")
	if _, ok := f.(*lua.LFunction); !ok {
		return "", false, nil
	}
	L.Push(f)
	L.Push(lua.LNumber(status))
	L.Push(lua.LString(body))
	if err := L.PCall(2, 1, nil); err != nil {
		return "", false, fmt.Errorf("transform_error: %w", err)
	}
	res := L.Get(-1)
	L.Pop(1)
	if res.Type() != lua.LTString {
		return "", false, nil
	}
	return res.String(), true, nil
}

func (v *VM) CallTransformStreamChunk(name, rawLine string) (string, error) {
	p := v.pool(name)
	if p == nil {
		return rawLine, nil
	}
	out, err := callMethod(p, "transform_stream_chunk", rawLine)
	if err != nil {
		return rawLine, nil
	}
	if out == "" {
		return "", nil
	}
	return out, nil
}

func (v *VM) GetAdapterEndpoint(name string) string {
	p := v.pool(name)
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.static.endpoint
}

func (v *VM) GetAdapterHeaders(name string) map[string]string {
	p := v.pool(name)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	hdr := make(map[string]string, len(p.static.headers))
	for k, h := range p.static.headers {
		hdr[k] = h
	}
	return hdr
}

// BuildHeaders executes adapter.build_headers(meta); falls back to the static
// adapter.headers table when no dynamic hook is defined.
func (v *VM) BuildHeaders(name string, meta map[string]interface{}) (map[string]string, error) {
	p := v.pool(name)
	if p == nil {
		return nil, fmt.Errorf("adapter %s not loaded", name)
	}
	w, err := p.acquire()
	if err != nil {
		return nil, err
	}
	defer p.release(w)
	L := w.L
	adapter := L.GetGlobal(adapterGlobal)
	tbl, ok := adapter.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("adapter %s has no table", name)
	}
	f := tbl.RawGetString("build_headers")
	if _, ok := f.(*lua.LFunction); !ok {
		// no dynamic hook -> static headers
		p.mu.Lock()
		hdr := make(map[string]string, len(p.static.headers))
		for k, h := range p.static.headers {
			hdr[k] = h
		}
		p.mu.Unlock()
		return hdr, nil
	}
	L.Push(f)
	L.Push(goValueToLua(L, meta))
	if err := L.PCall(1, 1, nil); err != nil {
		return nil, fmt.Errorf("build_headers: %w", err)
	}
	res := L.Get(-1)
	L.Pop(1)
	tbl2, ok := res.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("build_headers returned non-table")
	}
	headers := make(map[string]string)
	tbl2.ForEach(func(key, val lua.LValue) {
		headers[key.String()] = val.String()
	})
	return headers, nil
}

// ---- static inspection (compile-once at load time) ----

func inspectScript(name, code string) (*staticInfo, error) {
	L := lua.NewState()
	setupGlobals(L)
	if err := L.DoString(code); err != nil {
		L.Close()
		return nil, fmt.Errorf("compile adapter: %w", err)
	}
	tbl, ok := L.Get(-1).(*lua.LTable)
	L.Pop(1)
	if !ok {
		L.Close()
		return nil, fmt.Errorf("adapter script must return a table")
	}
	info := &staticInfo{name: name, headers: map[string]string{}}
	if n := readString(tbl, "name"); n != "" {
		info.name = n
	}
	info.version = readString(tbl, "version")
	info.endpoint = readString(tbl, "endpoint")
	if ht, ok := tbl.RawGetString("headers").(*lua.LTable); ok {
		ht.ForEach(func(k, val lua.LValue) {
			info.headers[k.String()] = val.String()
		})
	}
	L.Close()
	return info, nil
}

func readString(tbl *lua.LTable, field string) string {
	v := tbl.RawGetString(field)
	switch x := v.(type) {
	case lua.LString:
		return string(x)
	case lua.LNumber:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	default:
		return ""
	}
}

// ---- shared globals installed into every worker state ----

func setupGlobals(L *lua.LState) {
	L.SetGlobal("log", L.NewFunction(func(L *lua.LState) int {
		level := L.ToString(1)
		msg := L.ToString(2)
		fmt.Printf("[lua/%s] %s\n", level, msg)
		return 0
	}))

	jsonTable := L.NewTable()
	L.SetGlobal("json", jsonTable)
	L.SetField(jsonTable, "encode", L.NewFunction(func(L *lua.LState) int {
		val := L.CheckAny(1)
		goVal := luaValueToGo(val)
		b, err := json.Marshal(goVal)
		if err != nil {
			L.Push(lua.LString("null"))
			return 1
		}
		L.Push(lua.LString(string(b)))
		return 1
	}))
	L.SetField(jsonTable, "decode", L.NewFunction(func(L *lua.LState) int {
		str := L.CheckString(1)
		var val interface{}
		if err := json.Unmarshal([]byte(str), &val); err != nil {
			L.Push(lua.LNil)
			return 1
		}
		L.Push(goValueToLua(L, val))
		return 1
	}))

	L.SetGlobal("http_get", L.NewFunction(func(L *lua.LState) int {
		url := L.ToString(1)
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			errJSON, _ := json.Marshal(map[string]interface{}{"url": url, "error": err.Error()})
			L.Push(lua.LString(string(errJSON)))
			return 1
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		result, _ := json.Marshal(map[string]interface{}{"url": url, "status": resp.StatusCode, "body": string(body)})
		L.Push(lua.LString(string(result)))
		return 1
	}))

	L.SetGlobal("http_post", L.NewFunction(func(L *lua.LState) int {
		url := L.ToString(1)
		body := L.ToString(2)
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			errJSON, _ := json.Marshal(map[string]interface{}{"url": url, "error": err.Error()})
			L.Push(lua.LString(string(errJSON)))
			return 1
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		result, _ := json.Marshal(map[string]interface{}{"url": url, "body": string(respBody), "status": resp.StatusCode})
		L.Push(lua.LString(string(result)))
		return 1
	}))

	// 签名辅助（kimicode 等上游签名型 adapter 需要）
	L.SetGlobal("hmac_sha256_hex", L.NewFunction(func(L *lua.LState) int {
		key := L.ToString(1)
		data := L.ToString(2)
		m := hmac.New(sha256.New, []byte(key))
		m.Write([]byte(data))
		L.Push(lua.LString(hex.EncodeToString(m.Sum(nil))))
		return 1
	}))
	L.SetGlobal("sha256_hex", L.NewFunction(func(L *lua.LState) int {
		h := sha256.Sum256([]byte(L.ToString(1)))
		L.Push(lua.LString(hex.EncodeToString(h[:])))
		return 1
	}))
	L.SetGlobal("base64_encode", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(base64.StdEncoding.EncodeToString([]byte(L.ToString(1)))))
		return 1
	}))
	L.SetGlobal("tohex", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(hex.EncodeToString([]byte(L.ToString(1)))))
		return 1
	}))
}

// bundledAdapterNames 是内核内置的适配器清单。
//
// 单独提出来：writeBundledAdapters 与体检判据共用，避免两处各写一份而漏掉
// 某个（漏掉的后果是该适配器永远不会被更新）。
var bundledAdapterNames = []string{
	"openai", "anthropic", "deepseek", "gemini",
	"github", "groq", "mistral", "ollama", "kimicode",
	"server",
}

// writeBundledAdapters 把内嵌适配器落到 DataDir/adapters。
//
// ★ 原本是 `if 文件已存在 { continue }` —— 后果是「修了适配器 → 升级二进制
//
//	→ 已部署实例上的文件不更新」。这正是仓库 openai.lua 缺 stream_index
//	透传、而生产早有（2026-08-26 15:46 手工补上，比入库早 32 分钟）却长期
//	没人发现的机制性原因。
//
// 现在的判据（按内容，不按存在）：
//
//	文件不存在                        ⇒ 写
//	有历史清单且盘上 == 上次内嵌       ⇒ 用新的覆盖（只是没跟上新版本）
//	有历史清单但盘上 != 上次内嵌       ⇒ 不动（用户改过，静默覆盖等于丢修改）
//	无历史清单（首跑/从旧版本升级）    ⇒ 不动，只补缺失的文件
//
// "上次内嵌的版本"记在 DataDir/adapters/.bundled（`<name>\t<sha256>`）。
//
// ⚠️ 代价：升级到本版本的**那一次**，已部署实例上的适配器不会更新
//
//	（没有历史清单可比）。从第二次升级起自动生效。要立刻生效就删掉
//	DataDir/adapters 让内核重新解包。
//
// adapterVersion 从适配器源码里取 `adapter.version = "x"`（取不到返回 "?"）。
//
// 用途只有一个：让漂移告警能说清"盘上是哪一版、内置是哪一版"，
// 而不是只让运维看两个 sha。判据不依赖它。
func adapterVersion(src string) string {
	const key = "adapter.version"
	i := strings.Index(src, key)
	if i < 0 {
		return "?"
	}
	rest := src[i+len(key):]
	if j := strings.Index(rest, "\""); j < 0 {
		return "?"
	} else {
		rest = rest[j+1:]
	}
	if k := strings.Index(rest, "\""); k >= 0 {
		return rest[:k]
	}
	return "?"
}

func (v *VM) writeBundledAdapters() error {
	prev := v.readBundledManifest()
	cur := map[string]string{}
	updated := 0

	for _, name := range bundledAdapterNames {
		data, err := bundledAdapters.ReadFile("adapters/" + name + ".lua")
		if err != nil {
			continue
		}
		sum := sha256Hex(data)
		dstPath := filepath.Join(v.dir, name+".lua")

		if old, rerr := os.ReadFile(dstPath); rerr == nil {
			onDisk := sha256Hex(old)
			prevHash, known := prev[name]
			switch {
			case !known:
				// 无历史清单：无法判断是否被用户改过 ⇒ 不动（与旧行为一致）。
				//
				// ★ 但这种"不动"必须**说出来**（2026-10-26 生产故障）：
				// 盘上是一份**早于本次功能修复**的适配器时，内核一声不响地继续用它。
				// 现场：deepseek.lua 的 transform_stream_chunk 缺 tool_calls 处理
				// ⇒ 流式模式下模型调不动任何工具、且零报错，症状看起来像"模型不听话"。
				// 仓库里的内置版本已修好，且 deepseek_stream_test.go 一直是绿的 ——
				// 因为判据测的是**内置**文件，而运行时用的是**盘上**这份。
				if onDisk != sum {
					fmt.Printf("[lua] WARNING: adapter %s on disk (v%s) differs from bundled (v%s) and has no recorded baseline; "+
						"NOT overwriting (user edits are preserved). If tool calls or streaming misbehave, this drift is the first suspect — "+
						"back up and delete %s to adopt the bundled version.\n",
						name+".lua", adapterVersion(string(old)), adapterVersion(string(data)), name+".lua")
				}
			case onDisk == sum:
				// 已是当前版本，无需写
			case onDisk != prevHash:
				// 与"上次内嵌"不同 ⇒ 用户改过 ⇒ 保留，并记下盘上真实版本
				fmt.Printf("[lua] adapter %s 已被修改，保留用户版本（内核不覆盖）\n", name+".lua")
				cur[name] = onDisk
			default:
				// onDisk == prevHash != sum ⇒ 只是没跟上新版本，覆盖是安全的
				if werr := os.WriteFile(dstPath, data, 0644); werr != nil {
					return fmt.Errorf("write %s: %w", name, werr)
				}
				updated++
			}
		} else {
			if werr := os.WriteFile(dstPath, data, 0644); werr != nil {
				return fmt.Errorf("write %s: %w", name, werr)
			}
			updated++
			fmt.Printf("[lua] installed bundled adapter: %s\n", name+".lua")
		}
		cur[name] = sum
	}

	if err := v.writeBundledManifest(cur); err != nil {
		// 清单写失败只影响下次的判别，不该让启动失败
		fmt.Printf("[lua] 写内嵌清单失败（下次按不覆盖处理）: %v\n", err)
	}
	if updated > 0 {
		fmt.Printf("[lua] updated %d bundled adapter(s)\n", updated)
	}
	return nil
}

func (v *VM) manifestPath() string { return filepath.Join(v.dir, ".bundled") }

// readBundledManifest 读上次运行时的内嵌清单（name → sha256）。
func (v *VM) readBundledManifest() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(v.manifestPath())
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) == 2 && parts[0] != "" {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func (v *VM) writeBundledManifest(m map[string]string) error {
	var sb strings.Builder
	for _, name := range bundledAdapterNames {
		if h, ok := m[name]; ok {
			sb.WriteString(name + "\t" + h + "\n")
		}
	}
	tmp := v.manifestPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, v.manifestPath())
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func luaValueToGo(lv lua.LValue) interface{} {
	switch v := lv.(type) {
	case lua.LString:
		return string(v)
	case lua.LNumber:
		return float64(v)
	case lua.LBool:
		return bool(v)
	case *lua.LTable:
		if v.MaxN() > 0 {
			arr := make([]interface{}, 0, v.MaxN())
			v.ForEach(func(_, val lua.LValue) {
				arr = append(arr, luaValueToGo(val))
			})
			return arr
		}
		m := make(map[string]interface{})
		v.ForEach(func(key, val lua.LValue) {
			m[key.String()] = luaValueToGo(val)
		})
		return m
	default:
		return nil
	}
}

func goValueToLua(L *lua.LState, val interface{}) lua.LValue {
	switch v := val.(type) {
	case string:
		return lua.LString(v)
	case float64:
		return lua.LNumber(v)
	case int:
		return lua.LNumber(v)
	case int64:
		return lua.LNumber(v)
	case bool:
		return lua.LBool(v)
	case nil:
		return lua.LNil
	case []interface{}:
		tbl := L.NewTable()
		for i, item := range v {
			tbl.RawSetInt(i+1, goValueToLua(L, item))
		}
		return tbl
	case map[string]interface{}:
		tbl := L.NewTable()
		for k, item := range v {
			tbl.RawSetString(k, goValueToLua(L, item))
		}
		return tbl
	default:
		b, jerr := json.Marshal(v)
		if jerr == nil {
			var iv interface{}
			if json.Unmarshal(b, &iv) == nil {
				return goValueToLua(L, iv)
			}
		}
		return lua.LNil
	}
}

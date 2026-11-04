package security

// 判据：**不存在包级单例**（2026-11-04 T3.1）。
//
// ## 这些判据守的是什么
//
// 用户裁定：
//
// > secutiry 的实现也是错误的，所有共享数据统一挂载一个 kernel 提供的
// > 挂载点上暴露。
//
// > store也应该调用security的加密能力！
//
// 本包原有**三份**独立包级单例：
//
//	global      *Store   变量库（凭据别名）
//	runtime     *Runtime 安全防火墙
//	globalCodec *Codec   配置落盘加密
//
// 它们各自被 InitXxx / SetXxx 装入，各自有 Global() / RuntimeGlobal() /
// ConfigCodec() 访问器。三份都绕开了内核的统一挂载点 `kstate.State`。
//
// ## 为什么单例是错的（不是"多一个全局变量"这么轻）
//
// 本 agent 的 `a.security` 与 `security.Global()` 是**两个独立变量**。
// 正常装配下它们恰好指向同一份数据，但"恰好"不是保证：
//
//   - 测试替身：测试装单例、被测代码用 agent 字段 ⇒ 测的是单例
//   - 多 Runtime：两个内核实例（或热重载）时单例只有一份，另一方被覆盖
//   - 装配顺序：单例装晚了，先注册的 provider 就没被装饰（静默漏一条出网路径）
//
// 最后一条**曾真实存在**：ProviderManager.Register 在注册时读单例套装饰器，
// 而 llm 模块原本不声明 FacSecurity 依赖 —— 顺序正确纯属巧合。
//
// ## 判据为什么是源码扫描
//
// 这是"不存在某物"的判据，没有可执行的行为可测。但**变异可验证**：
// 把 `var global *Store` 加回来 ⇒ 立刻红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenPackageLevelVars 是**不得出现**的包级单例变量名。
//
// 每一条都对应一次真实事故或一次可复现的错配：
//
//	global  → 变量库：测试装单例、代码用 agent 字段，测的与跑的不是同一份
//	runtime → 防火墙：多实例时被覆盖
//
// ★ globalCodec 不在本表 —— 它属于 T3.2（Store 复用 Codec + Codec 归 Runtime）。
// 那条改动还牵涉 config 的 6 个调用点与惰性密文迁移，单独一步做。
// 本判据只钉已经完成的部分，避免"半成品状态让整套判据长期红"
// （长期红的判据会被人习惯性忽略，那时它就等于不存在）。
func forbiddenPackageLevelVars() []string {
	return []string{"global", "runtime"}
}

// TestNoPackageLevelSingletons 钉住本包不得声明这三个包级变量。
//
// 变红条件：把 `var global *Store`（或另外两个）加回来 ⇒ 红。
func TestNoPackageLevelSingletons(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}

	banned := make(map[string]bool, len(forbiddenPackageLevelVars()))
	for _, n := range forbiddenPackageLevelVars() {
		banned[n] = true
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, ident := range vs.Names {
					if banned[ident.Name] {
						t.Fatalf("★ %s:%d 声明了包级单例 %q。\n\n"+
							"本包的单例已按用户裁定删除 —— 共享数据统一挂在\n"+
							"kstate.State 上，各消费方从自己的注入面取：\n\n"+
							"\tAgentConfig.Security      → a.security.Store()\n"+
							"\tProviderManager.SetSecretStore\n"+
							"\tplugin.Registry.SetSecretStore\n\n"+
							"单例的问题不是'多一个全局变量'，而是它与 a.security\n"+
							"是两个独立变量，正常装配下'恰好'同一份，但测试替身/\n"+
							"多 Runtime/装配顺序任一路径都可能分离。",
							name, fset.Position(ident.Pos()).Line, ident.Name)
					}
				}
			}
		}
	}
}

// TestMustStoreDoesNotRegisterGlobally 钉住 MustStore 只是构造+加载。
//
// 变异可验证：在 MustStore 里加一行 `global = s` ⇒ 上面的判据红
// （因为 global 变量本身不存在，得先加变量）。
//
// 本判据补的是另一半：**即使变量名换了**（比如叫 defaultStore），
// MustStore 也不得把结果存进任何包级位置。
func TestMustStoreDoesNotRegisterGlobally(t *testing.T) {
	dir := t.TempDir()
	s1, err := MustStore(Options{
		Path:    filepath.Join(dir, "a.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("MustStore: %v", err)
	}
	// 第二次调用必须得到**另一个**实例 —— 若内部有包级缓存，
	// 第二次会返回同一个（那就是单例还活着）。
	s2, err := MustStore(Options{
		Path:    filepath.Join(dir, "b.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("MustStore: %v", err)
	}
	if s1 == s2 {
		t.Fatal("★ MustStore 两次返回同一实例 —— 说明它偷偷存了包级单例。\n" +
			"它应当只做 New + Load，把实例交还调用方注入。")
	}
}

// TestStoreEncryptsWithOwnKey 钉住变量库确实是密文落盘（加密能力归属本模块）。
//
// 这条是 T3.2（Store 复用 Codec）的前置事实：现在 Store 有自己的 seal/open。
// T3.2 之后它会改为委托 Codec，本判据仍然成立（落盘必须是密文）。
func TestStoreEncryptsWithOwnKey(t *testing.T) {
	dir := t.TempDir()
	const key = "0123456789abcdef0123456789abcdef"
	path := filepath.Join(dir, "v.json")

	s, err := MustStore(Options{
		Path:    path,
		KeyFn:   func() ([]byte, error) { return []byte(key), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("MustStore: %v", err)
	}

	const plain = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if _, err := s.Submit(plain, "test"); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读落盘文件: %v", err)
	}
	if strings.Contains(string(raw), plain) {
		t.Fatal("★ 变量库落盘里有明文 —— 加密没有生效")
	}

	// 重新加载必须能解回来（密钥一致）。
	s2, err := MustStore(Options{
		Path:    path,
		KeyFn:   func() ([]byte, error) { return []byte(key), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if s2.Count() != 1 {
		t.Fatalf("重新加载后应有 1 条，实际 %d", s2.Count())
	}
}

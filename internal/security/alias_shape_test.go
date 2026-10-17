package security

import "testing"

// 判据：内核别名是 HMAC 派生 ⇒ **用户不能自选名字**。
//
// 这决定了去重的形态：插件若要保留可读名（@{gitea-local}），
// 它必须另外存一个 name→alias 映射（**只存映射，不存值**）。
func TestKernelAliasIsDerivedNotUserChosen(t *testing.T) {
	s := newTestStore(t, true)
	a1, _ := s.Alias("some-plain-value")
	a2, _ := s.Alias("some-plain-value")
	if a1 != a2 {
		t.Fatal("别名必须确定性派生（否则失去自动合并）")
	}
	// 没有任何 API 允许指定别名内容
	if a1 == "gitea-local" {
		t.Fatal("别名不该等于用户想要的名字")
	}
	if len(a1) != 20 || a1[:4] != "sec_" {
		t.Fatalf("别名形态固定: %s", a1)
	}
	t.Logf("内核别名是派生值: %s（用户无法自选 ⇒ 插件要保留可读名必须自己存映射）", a1)
}

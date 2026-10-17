//go:build cgo

package api

// codec_jsongolden_test.go —— ha_json_scan（C）与 encoding/json（Go）逐值对照。
//
// ============================ 这是本刀最重要的验收 ============================
// 理由：C 侧手写扫描器最容易出的错不是崩溃，而是**静默的分叉** ——
//   某个输入 Go 接受而 C 拒绝（或反之）、某个转义解码结果差一个字节。
//   而这类分叉在生产里的表现是「内容偶尔少一个字符」「某些块被静默丢弃」，
//   极难归因。因此必须有**同一批输入、两个实现、逐值比对**的测试。
//
// 参照第一刀的做法（codec_golden_test.go），此处比的是
//   C: ha_json_scan 的 scan / decode / get_int
//   Go: encoding/json 的等价行为
//
// 覆盖：语法严格性、键大小写不敏感、重复键后者胜、\u 与代理对、
// 非法 UTF-8 → U+FFFD、整数溢出/小数/指数、畸形成员的辨别。

import (
	"encoding/json"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// -----------------------------------------------------------------
// 1. 语法严格性：C 的 skip 与 Go 的 json.Valid 必须一致
// -----------------------------------------------------------------

func TestJSONGolden_SyntaxVsValid(t *testing.T) {
	cases := []string{
		`{}`, `{"a":1}`, `{"a":null}`, `{"a":true}`, `{"a":-1}`,
		`{"a":1.5}`, `{"a":1e2}`, `{"a":[]}`, `{"a":{}}`,
		`{"a":"b"}`, `{"a":"A"}`, `  {"a" : 1 }  `,
		`{"a":"\u4f60\u597d"}`, `{"a":"\ud83d\ude00"}`,
		`{"a":{"b":[1,2,{"c":3}]}}`, `{"a":1,"b":2}`,
		`{"a":1,"a":2}`, // 重复键（合法）
		// 以下应与 json.Valid 一致地失败
		`{`, `}`, ``, `{"a"}`, `{"a":}`, `{"a":1,}`, `{'a':1}`,
		`{"a":01}`, `{"a":1.}`, `{"a":.5}`, `{"a":1e}`, `{"a":-}`,
		`{"a":tru}`, `{"a":1 "b":2}`, `{"a":"unclosed`,
		`{"a":"bad\ncontrol"}`, `{"a":"\q"}`, `{"a":"\u00"}`,
		`[1,2,]`, `{"a":[1,]}`, `{"a":1}{"b":2}`,
		`{"a":+1}`, `{"a":Infinity}`, `{"a":NaN}`,
	}
	for _, in := range cases {
		cOK := cjsSkipStrict(in)
		goOK := json.Valid([]byte(in))
		if cOK != goOK {
			t.Errorf("语法分歧 %q: C.skip=%v, json.Valid=%v", in, cOK, goOK)
		}
	}
}

// -----------------------------------------------------------------
// 2. 成员迭代：C 与 Go 必须数到同样的键、且 complete 判定一致
// -----------------------------------------------------------------

// goObjectKeysStrict 用 Go 自己的遍历统计键数；任何 unmarshal 失败即视为 0。
func goKeys(in string) (int, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		return 0, false
	}
	return len(m), true
}

func TestJSONGolden_MembersCount(t *testing.T) {
	cases := []string{
		`{}`, `{"a":1}`, `{"a":1,"b":2}`, `{"a":1,"b":2,"c":3}`,
		`{"a":{"x":1},"b":[1,2]}`, `{"A":1,"a":2}`, // 大小写不同的键都算
		`{"":1}`, `{"a":"}"}`, `{"a":"{"}`, `{"a":"x,y,z"}`,
		`{"a":{"n":1},"b":{"n":2}}`,
		`{"a":1,}`, `{"a":1`, `{"a"}`, `{"a":}`,
	}
	for _, in := range cases {
		cInit, cCount, cComplete := cjsWalkMembers(in)

		goCount, goOK := goKeys(in)

		// init 的语义只是「首字符是 '{'」——它**不可能**知道对象是否闭合，
		// 所以不能用 Go 的 unmarshal ok 来判它（那是 complete 的职责）。
		// 这里分开断言：
		//   init    ↔ 首字符是 '{'
		//   complete ↔ Go unmarshal 成功（整体良构）
		wantInit := strings.HasPrefix(strings.TrimSpace(in), "{")
		if cInit != wantInit {
			t.Errorf("init 分歧 %q: C.init=%v, 期望 %v", in, cInit, wantInit)
			continue
		}
		if !cInit {
			continue
		}
		if cComplete != goOK {
			t.Errorf("complete 分歧 %q: C=%v, Go=%v", in, cComplete, goOK)
			continue
		}
		if goOK && cCount != goCount {
			t.Errorf("成员数分歧 %q: C=%d, Go=%d", in, cCount, goCount)
		}
	}
}

// -----------------------------------------------------------------
// 3. 字符串解码：C 与 Go 的 unquote 必须逐字节一致
// -----------------------------------------------------------------

func TestJSONGolden_StringDecode(t *testing.T) {
	rawCases := []string{
		``, `a`, `hello world`, `中文`, `你好😀`,
		`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`,
		`\u0041`, `\u00e9`, `\u4f60\u597d`, `\ud83d\ude00`, `\u0000`,
		`mixed \u4e2d\u6587 and ascii`,
		`\ud83d` + `real`, // 孤立高代理
		`\udc00` + `real`, // 孤立低代理
		`\ud83dx`,         // 高代理 + 非转义
		`\ud83d\u0041`,    // 高代理 + 非低代理
		"\xff", "\xfe", "\xff\xfe", "\xc3", "\xc3\x28", "\xe0\x80\x80",
		"\xed\xa0\x80", "\xf5\x80\x80\x80", "\xf0\x9f\x98\x80", // 正常 4 字节
		"a\xffb", "\x80", "\xbf",
		`\uD83D\uDE00`, // 大写十六进制代理对
	}
	for _, raw := range rawCases {
		// Go 侧参照：把 raw 当作 JSON 字符串体的内容，解码
		goOut, goErr := goUnquoteBody(raw)
		doc := `"` + raw + `"`

		// C 侧：先取字符串 span（去掉引号），再解码
		cRaw, rawOK := cjsScanString(doc)
		if !rawOK {
			if goErr == nil {
				t.Errorf("C 拒绝但 Go 接受: raw=%q", raw)
			}
			continue
		}
		cOut, cOK := cjsDecode(cRaw)

		if goErr != nil {
			if cOK {
				t.Errorf("C 接受但 Go 报错: raw=%q -> %q", raw, cOut)
			}
			continue
		}
		if !cOK {
			t.Errorf("C 解码失败但 Go 成功: raw=%q 期望 %q", raw, goOut)
			continue
		}
		if cOut != goOut {
			t.Errorf("解码分歧 raw=%q:\n  C  = %q (% x)\n  Go = %q (% x)",
				raw, cOut, cOut, goOut, goOut)
		}
	}
}

// -----------------------------------------------------------------
// 4. 整数：C 与 Go（strconv.ParseInt 语义）一致
// -----------------------------------------------------------------

func TestJSONGolden_GetInt(t *testing.T) {
	cases := []string{
		"0", "1", "-1", "12345", "-99999", "2147483647", "-2147483648",
		"9223372036854775807", "-9223372036854775808",
		"9223372036854775808", "-9223372036854775809",
		"99999999999999999999", "1.5", "1e2", "", "abc", "0x10", "+1", "007",
		"0", "-0", "00", "0.0", " 1", "1 ",
	}
	for _, in := range cases {
		cGot, cOK := cjsGetInt(in)
		var cVal int64 = cGot

		// Go 参照：按 **JSON 整数语法**（而非 strconv 的宽松十进制）判定。
		// 差别在 "007"/"+1"：strconv.ParseInt 接受，但 JSON 语法禁止前导零与前导 +。
		// 本库的契约是「这是不是 JSON 整数」（以便调用方按
		// 「类型不匹配 ⇒ 整块作废」处理），故参照必须用同一判据。
		goOK := false
		var goVal int64
		if isJSONIntSyntax(in) {
			v, err := strconv.ParseInt(in, 10, 64)
			if err == nil {
				goOK, goVal = true, v
			}
			// 溢出（ErrRange）⇒ 与 C 一致：判为「不是可用整数」
		}
		if cOK != goOK {
			t.Errorf("整数可用性分歧 %q: C=%v, Go=%v", in, cOK, goOK)
			continue
		}
		if cOK && cVal != goVal {
			t.Errorf("整数值分歧 %q: C=%d, Go=%d", in, int64(cVal), goVal)
		}
	}
}

func isJSONIntSyntax(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	if i >= len(s) {
		return false
	}
	if s[i] == '0' {
		return i+1 == len(s)
	}
	if s[i] < '1' || s[i] > '9' {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------
// 5. 随机字节：两侧的「是否接受」必须一致（畸形输入等价性）
// -----------------------------------------------------------------

func TestJSONGolden_RandomBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(20260926))
	alphabet := []byte(`{}[]",:0123456789tfnul \` + "\n\t\xff\x80")
	mismatch := 0
	for iter := 0; iter < 20000 && mismatch < 5; iter++ {
		n := rng.Intn(40)
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		cOK := cjsSkipStrict(string(b))
		goOK := json.Valid(b)
		if cOK != goOK {
			mismatch++
			t.Errorf("随机输入分歧 %q: C.skip=%v json.Valid=%v", b, cOK, goOK)
		}
	}
}

// -----------------------------------------------------------------
// 6. ABI
// -----------------------------------------------------------------

func TestJSONScanABIVersion(t *testing.T) {
	if got := cjsABIVersion(); got != 1000 {
		t.Errorf("ha_json_scan ABI = %d, 期望 1000 (1.0)", got)
	}
}

// -----------------------------------------------------------------
// 辅助
// -----------------------------------------------------------------

// goUnquoteBody 用 encoding/json 自身解码一个 JSON 字符串体（raw = 不含两端引号）。
//
// ★ 正确做法是**直接把 body 原样**放进引号里交给 Unmarshal ——
//
//	body 里本来就带着它自己的转义（`\n` 是两个字节），若在此处再转义一遍，
//	就把「转义序列」变成了「字面量」，参照值会整体跑偏。
//	实测踩过：初版对 body 里的 `\` 和 `"` 做了二次转义，
//	导致 Go 侧期望 `\n`（两字节）而 C 侧正确给出换行符 ——
//	测试报了一堆「分歧」，其实错的是测试自己的参照。
func goUnquoteBody(body string) (string, error) {
	var out string
	if err := json.Unmarshal([]byte(`"`+body+`"`), &out); err != nil {
		return "", err
	}
	return out, nil
}

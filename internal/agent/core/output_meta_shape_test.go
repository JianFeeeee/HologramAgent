package core

// meta / payload 传参形态的判据。
//
// ## 真实事故（2026-10-23 起，持续到 10-25）
//
// 生产实测：`output_send__qq` 失败率从 1.3% 涨到 34%（132 次/天）。
//
// 根因链：
//
//	模型对 meta 的传参形态不稳定：
//	  有时传字符串  "{\"user_id\":123}"     ← 能正常工作
//	  有时直接传对象 {"user_id":123}         ← 从 10-22 "一 call 多发" 之后变多
//
//	stringOrArray 只认 string / []string / []interface{}，遇到对象返回 nil
//	  ⇒ parseOutputItems 对 len(metas)==0 的处理是**静默填空数组**
//	  ⇒ 插件收到 meta="" ⇒ "meta 中需要 group_id 或 user_id 字段"
//
// 用户侧表现：agent 答应了却没发到，且它完全不知道参数被吞了
// （表现为"忘了刚说过的话"）。
//
// ## 正确边界
//
// 内核**不知道** meta 的语义，但它知道"模型给出了一个值"。
// 该值不是字符串时必须**序列化保留**，不能当"没提供"处理。

import "testing"

// 模型把 meta 传成对象时，必须序列化成 JSON 字符串保留下来。
func TestParseOutputItems_MetaAsObjectIsPreserved(t *testing.T) {
	args := map[string]interface{}{
		"payload": "你好",
		"type":    "text",
		"meta":    map[string]interface{}{"user_id": float64(2198972886)},
	}
	items, err := parseOutputItems(args)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应有 1 条待发送项，实际 %d", len(items))
	}
	if items[0].meta == "" {
		t.Fatal("meta 被静默丢弃（插件将报「需要 group_id 或 user_id」）")
	}
	// 必须是可被插件 json.Unmarshal 的 JSON 字符串
	if items[0].meta != `{"user_id":2198972886}` {
		t.Fatalf("meta 应序列化为 JSON 字符串，实际 %q", items[0].meta)
	}
}

// 字符串形态必须**原样透传**（既有行为不能变）。
func TestParseOutputItems_MetaAsStringUnchanged(t *testing.T) {
	const raw = `{"user_id":123}`
	items, err := parseOutputItems(map[string]interface{}{
		"payload": "你好",
		"meta":    raw,
	})
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if items[0].meta != raw {
		t.Fatalf("字符串形态应原样透传，实际 %q", items[0].meta)
	}
}

// meta 数组里的对象元素也必须保留（一 call 多发时每条的 meta 可能不同）。
func TestParseOutputItems_MetaArrayWithObjects(t *testing.T) {
	items, err := parseOutputItems(map[string]interface{}{
		"payload": []interface{}{"a", "b"},
		"meta": []interface{}{
			map[string]interface{}{"user_id": float64(1)},
			map[string]interface{}{"user_id": float64(2)},
		},
	})
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应有 2 条，实际 %d", len(items))
	}
	for i, it := range items {
		if it.meta == "" {
			t.Fatalf("第 %d 条的 meta 被丢弃", i+1)
		}
	}
	if items[0].meta == items[1].meta {
		t.Fatal("两条的 meta 不应相同（一一对应关系丢失）")
	}
}

// 确实没提供 meta 时，仍按"缺失"处理（空字符串），不能凭空造值。
func TestParseOutputItems_MissingMetaStaysEmpty(t *testing.T) {
	items, err := parseOutputItems(map[string]interface{}{"payload": "你好"})
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if items[0].meta != "" {
		t.Fatalf("未提供 meta 时应为空，实际 %q", items[0].meta)
	}
}

package seq

// 开机序列的判据。
//
// 背景（用户裁定）：seq 的核心价值不是"单次任务省几次调用"，而是
// **定时/开机复用**。e2e 实测证明前者不成立（一次性多文件读直接并行
// files_read 就是最优解），所以 Boot 才是这个插件存在的理由。

import (
	"strings"
	"testing"
)

// boot 必须能从 JSON 里反序列化出来（序列文件手写 "boot": true 的路径）。
func TestBootMarkerParsesFromJSON(t *testing.T) {
	text := `{
	  "name": "boot-check",
	  "boot": true,
	  "groups": [
	    {"name": "g1", "parallel": true, "out": {"r": "string"},
	     "tools": "{\"tool\":\"cmd_run\",\"args\":{},\"as\":\"r\"};"}
	  ]
	}`
	sq, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !sq.Boot {
		t.Fatal("boot:true 未被解析（开机序列会静默不执行）")
	}
}

// 未标 boot 的序列默认不参与开机执行 —— 否则每条序列都会在启动时乱跑。
func TestBootDefaultsToFalse(t *testing.T) {
	text := `{"name":"normal","groups":[{"name":"g","parallel":true,"out":{"r":"string"},
	  "tools":"{\"tool\":\"cmd_run\",\"args\":{},\"as\":\"r\"};"}]}`
	sq, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sq.Boot {
		t.Fatal("未声明 boot 的序列不应标记为开机序列")
	}
}

// runBootSequences 必须能在没有任何 boot 序列时安静返回（不能 panic、不能乱跑）。
func TestRunBootSequencesNoopWhenNone(t *testing.T) {
	p := New("seq")
	p.store = NewStore(t.TempDir())
	// 不 panic、不报错即为通过
	p.runBootSequences()
}

// 工具描述必须告知模型 boot 的语义（否则没人会想到标它）。
func TestCreateToolAdvertisesBoot(t *testing.T) {
	defs := New("seq").toolDefs()
	def, ok := defs["seq_create"]
	if !ok {
		t.Fatal("缺 seq_create 定义")
	}
	props, _ := def.Parameters["properties"].(map[string]interface{})
	boot, ok := props["boot"].(map[string]interface{})
	if !ok {
		t.Fatal("seq_create 必须暴露 boot 参数（否则模型无从知道可标记开机序列）")
	}
	desc, _ := boot["description"].(string)
	if !strings.Contains(desc, "启动") {
		t.Fatalf("boot 描述应说明「系统就绪/启动时执行」，实际：%q", desc)
	}
}

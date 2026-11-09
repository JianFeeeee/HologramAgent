package test

// internal/devicebridge/client 的黑盒判据：BaseResult 输入 → map 输出。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/devicebridge/client"
)

// TestBaseResultAlwaysCarriesIdentity：op/req_id/status 始终存在。
func TestBaseResultAlwaysCarriesIdentity(t *testing.T) {
	r := client.BaseResult("req-1", "ok", "", "")
	if r["op"] != "cmd_result" || r["req_id"] != "req-1" || r["status"] != "ok" {
		t.Fatalf("★ 身份字段缺失：%v", r)
	}
}

// TestBaseResultOmitsEmptyOptionalFields：空 output/error 不进结果。
func TestBaseResultOmitsEmptyOptionalFields(t *testing.T) {
	r := client.BaseResult("r", "ok", "", "")
	if _, ok := r["output"]; ok {
		t.Fatal("★ 空 output 被写进结果")
	}
	if _, ok := r["error"]; ok {
		t.Fatal("★ 空 error 被写进结果")
	}
}

// TestBaseResultCarriesOutputAndError：非空时分别落到对应键。
func TestBaseResultCarriesOutputAndError(t *testing.T) {
	r := client.BaseResult("r", "error", "stdout-line", "boom")
	if r["output"] != "stdout-line" || r["error"] != "boom" {
		t.Fatalf("★ 可选字段不符：%v", r)
	}
}

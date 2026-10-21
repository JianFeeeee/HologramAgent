package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

type toolCapture struct {
	handlers map[string]sdk.ToolHandler
	defs     map[string]sdk.ToolDef
}

func newToolCapture() *toolCapture {
	return &toolCapture{
		handlers: make(map[string]sdk.ToolHandler),
		defs:     make(map[string]sdk.ToolDef),
	}
}

func (tc *toolCapture) RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	tc.handlers[name] = handler
	tc.defs[name] = def
	return nil
}

func (tc *toolCapture) RegisterStage(stage sdk.Stage, handler sdk.StageHandler) {}

func (tc *toolCapture) RegisterAPI(name string) error { return nil }

func setupPlugin() (*Plugin, *toolCapture, error) {
	p := New("cmd")
	tc := newToolCapture()
	sdk := sdk.New("cmd", sdk.SDKConfig{
		RegTool:  tc.RegisterTool,
		RegStage: tc.RegisterStage,
		RegAPI:   tc.RegisterAPI,
		Settings: sdk.NewSettings("cmd", nil),
	})
	if err := p.Start(sdk); err != nil {
		return nil, nil, err
	}
	return p, tc, nil
}

func TestCmdRunEcho(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler, ok := tc.handlers["cmd_run"]
	if !ok {
		t.Fatal("cmd_run tool not registered")
	}

	result, err := handler(map[string]interface{}{
		"command": "echo hello world",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
	if resp["stdout"] != "hello world" {
		t.Fatalf("expected 'hello world', got %v", resp["stdout"])
	}
	if resp["exit_code"].(float64) != 0 {
		t.Fatalf("expected exit code 0, got %v", resp["exit_code"])
	}
}

func TestCmdRunWithStderr(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["cmd_run"]
	// ls with a nonexistent path writes to stderr and returns non-zero exit code
	result, err := handler(map[string]interface{}{
		"command": "ls /tmp/cmd_test_nonexistent_xxxxx",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
	if stderr, ok := resp["stderr"].(string); !ok || stderr == "" {
		t.Fatalf("expected stderr output, got %q", stderr)
	}
	if exitCode, ok := resp["exit_code"].(float64); !ok || exitCode == 0 {
		t.Fatalf("expected non-zero exit code, got %v", exitCode)
	}
}

func TestCmdRunTimeout(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["cmd_run"]
	result, err := handler(map[string]interface{}{
		"command": "sleep 10",
		"timeout": "1s",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "timeout" {
		t.Fatalf("expected status timeout, got %v", resp["status"])
	}
}

func TestCmdRunWorkdir(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	tmpDir, err := os.MkdirTemp("", "cmd_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	marker := filepath.Join(tmpDir, "marker.txt")
	if err := os.WriteFile(marker, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["cmd_run"]
	result, err := handler(map[string]interface{}{
		"command": "cat marker.txt",
		"workdir": tmpDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
	if resp["stdout"] != "ok" {
		t.Fatalf("expected 'ok', got %v", resp["stdout"])
	}
}

func TestCmdRunMissingCommand(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["cmd_run"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error for missing command")
	}
}

func TestCmdRunNonZeroExit(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["cmd_run"]
	result, err := handler(map[string]interface{}{
		"command": "false",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
	if resp["exit_code"].(float64) != 1 {
		t.Fatalf("expected exit code 1, got %v", resp["exit_code"])
	}
}

func TestTruncateOutput(t *testing.T) {
	p, _, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	short := "hello"
	if s := p.truncateOutput(short); s != short {
		t.Fatalf("expected %q, got %q", short, s)
	}

	long := make([]byte, 40000)
	for i := range long {
		long[i] = 'x'
	}
	s := p.truncateOutput(string(long))
	if len(s) >= 40000 {
		t.Fatal("expected truncation")
	}
	if len(s) > 32100 {
		t.Fatalf("truncated string too long: %d", len(s))
	}
}

func TestCmdRunPipeFail(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["cmd_run"]
	result, err := handler(map[string]interface{}{
		"command": "false",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
	if resp["exit_code"].(float64) != 1 {
		t.Fatalf("expected exit code 1, got %v", resp["exit_code"])
	}
}

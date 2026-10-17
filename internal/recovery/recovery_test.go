package recovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeProber struct {
	results map[string]bool // label → ok
	mu      chan struct{}
}

func newFakeProber() *fakeProber { return &fakeProber{results: map[string]bool{}} }

func (f *fakeProber) set(label string, ok bool) {
	if f.results == nil {
		f.results = map[string]bool{}
	}
	f.results[label] = ok
}

func (f *fakeProber) probe(ctx context.Context, label string) (bool, error) {
	if f.results == nil {
		return false, nil
	}
	ok, _ := f.results[label]
	return ok, nil
}

func TestLadderHealthy(t *testing.T) {
	dir := t.TempDir()
	p := newFakeProber()
	p.set("initial", true) // 初始即通，不触发任何恢复
	l := &Ladder{
		ResultFile: ResultPath(dir),
		Attempt:    1,
		Probe:      p.probe,
		RestoreNetwork: func() ([]string, error) {
			t.Fatal("network restore should not be called when healthy")
			return nil, nil
		},
		RestoreConfig: func() (int, error) {
			t.Fatal("config restore should not be called when healthy")
			return 0, nil
		},
	}
	res := l.Run(context.Background())
	if !res.Success {
		t.Fatalf("expected success, got %s", res.Quote())
	}
	if res.Cause != "healthy" {
		t.Fatalf("cause = %q, want healthy", res.Cause)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(res.Steps))
	}
	// 结果落盘
	onDisk, err := LoadResult(ResultPath(dir))
	if err != nil {
		t.Fatalf("LoadResult: %v", err)
	}
	if !onDisk.Success {
		t.Fatal("disk result not success")
	}
}

func TestLadderNetworkRecovery(t *testing.T) {
	dir := t.TempDir()
	p := newFakeProber()
	p.set("initial", false)
	p.set("after-network", true) // 还原网络后通了
	var netCalls int
	l := &Ladder{
		ResultFile: ResultPath(dir),
		Attempt:    2,
		Probe:      p.probe,
		RestoreNetwork: func() ([]string, error) {
			netCalls++
			return []string{"/etc/resolv.conf"}, nil
		},
		RestoreConfig: func() (int, error) {
			t.Fatal("config restore should not run when network fixes it")
			return 0, nil
		},
	}
	res := l.Run(context.Background())
	if !res.Success {
		t.Fatalf("expected success, got %s", res.Quote())
	}
	if res.Cause != "network_config" {
		t.Fatalf("cause = %q, want network_config", res.Cause)
	}
	if netCalls != 1 {
		t.Fatalf("network restore calls = %d, want 1", netCalls)
	}
	if len(res.RestoredNet) != 1 {
		t.Fatalf("RestoredNet = %v, want 1 entry", res.RestoredNet)
	}
}

func TestLadderConfigRecovery(t *testing.T) {
	dir := t.TempDir()
	p := newFakeProber()
	p.set("initial", false)
	p.set("after-network", false)
	p.set("after-config", true)
	var cfgCalls int
	var netCalls int
	l := &Ladder{
		ResultFile: ResultPath(dir),
		Attempt:    3,
		Probe:      p.probe,
		RestoreNetwork: func() ([]string, error) {
			netCalls++
			return nil, nil
		},
		RestoreConfig: func() (int, error) {
			cfgCalls++
			return 7, nil
		},
	}
	res := l.Run(context.Background())
	if !res.Success {
		t.Fatalf("expected success, got %s", res.Quote())
	}
	if res.Cause != "config" {
		t.Fatalf("cause = %q, want config", res.Cause)
	}
	if cfgCalls != 1 || netCalls != 1 {
		t.Fatalf("cfg=%d net=%d, want both 1", cfgCalls, netCalls)
	}
	if !res.ConfigRestored {
		t.Fatal("ConfigRestored should be true")
	}
}

func TestLadderExhausted(t *testing.T) {
	dir := t.TempDir()
	p := newFakeProber()
	p.set("initial", false)
	p.set("after-network", false)
	p.set("after-config", false)
	l := &Ladder{
		ResultFile:     ResultPath(dir),
		Attempt:        1,
		Probe:          p.probe,
		RestoreNetwork: func() ([]string, error) { return nil, nil },
		RestoreConfig:  func() (int, error) { return 0, nil },
	}
	res := l.Run(context.Background())
	if res.Success {
		t.Fatal("expected failure when ladder exhausted")
	}
	if res.Cause != "unreachable" {
		t.Fatalf("cause = %q, want unreachable", res.Cause)
	}
	if res.QuickChatOK {
		t.Fatal("QuickChatOK should be false on failure")
	}
	if len(res.Steps) < 6 {
		t.Fatalf("steps = %d, want >= 6", len(res.Steps))
	}
}

func TestTaskFileRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := TaskPath(dir)
	task := &Task{
		Attempt:        2,
		MaxAttempts:    4,
		AttemptTimeout: "120s",
		TriggerPrompt:  "你是恢复 agent",
		RescueSource:   RescueSource{Name: "rescue", BaseURL: "http://1.2.3.4:8080", Adapter: "openai"},
		PluginList:     []string{"webui", "recoverydiag"},
	}
	if err := SaveTask(path, task); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	loaded, err := LoadTask(path)
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if loaded.RescueSource.BaseURL != task.RescueSource.BaseURL {
		t.Fatal("rescue base_url mismatch")
	}
	if len(loaded.Plugins()) != 2 {
		t.Fatalf("plugins = %v, want 2", loaded.Plugins())
	}
	_ = os.Remove(path)
}

func TestResultFileRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recovery", "result.json")
	r := &Result{Attempt: 1, Success: true, Cause: "network_config", QuickChatOK: true}
	if err := SaveResult(path, r); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
	loaded, err := LoadResult(path)
	if err != nil {
		t.Fatalf("LoadResult: %v", err)
	}
	if !loaded.Success || loaded.Cause != "network_config" {
		t.Fatalf("roundtrip mismatch: %+v", loaded)
	}
}

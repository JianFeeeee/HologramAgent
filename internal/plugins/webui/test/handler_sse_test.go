package test

import (
	"encoding/json"
	"github.com/JianFeeeee/HologramAgent/internal/plugins/webui"
	"reflect"
	"testing"
)

func TestSSEEventRing_Append_Capacity(t *testing.T) {
	r := webui.NewSSEEventRing(3)
	data := json.RawMessage(`{"a":1}`)
	for i := 0; i < 5; i++ {
		r.Append("id-"+string(rune('0'+i)), "agent_output", data)
	}
	// cap=3，只保留 id-2,id-3,id-4
	if len(r.After("")) != 3 {
		t.Fatalf("expected 3, got %d", len(r.After("")))
	}
	if r.After("")[0].ID != "id-2" {
		t.Fatalf("expected id-2, got %s", r.After("")[0].ID)
	}
}

func TestSSEEventRing_After_Mid(t *testing.T) {
	r := webui.NewSSEEventRing(10)
	data := json.RawMessage(`{"x":"y"}`)
	r.Append("id-1", "reasoning", data)
	r.Append("id-2", "agent_output", data)
	r.Append("id-3", "tool_call", data)

	after := r.After("id-2")
	if len(after) != 1 || after[0].ID != "id-3" {
		t.Fatalf("expected [id-3], got %v", after)
	}

	after = r.After("id-3")
	if len(after) != 0 {
		t.Fatalf("expected 0, got %d", len(after))
	}
}

func TestSSEEventRing_After_NotFound_ReturnsAll(t *testing.T) {
	r := webui.NewSSEEventRing(3)
	data := json.RawMessage(`{}`)
	r.Append("id-1", "agent_output", data)
	r.Append("id-2", "agent_output", data)

	after := r.After("id-0")
	if len(after) != 2 {
		t.Fatalf("expected 2 (fallback to all), got %d", len(after))
	}
}

func TestSSEEventRing_After_Last_Tip(t *testing.T) {
	r := webui.NewSSEEventRing(5)
	data := json.RawMessage(`{"a":1}`)
	for i := 0; i < 5; i++ {
		r.Append("id-"+string(rune('0'+i)), "agent_output", data)
	}
	after := r.After("id-4")
	if len(after) != 0 {
		t.Fatalf("expected 0 after tip, got %d", len(after))
	}
}

func TestSSEEventRing_Concurrent(t *testing.T) {
	r := webui.NewSSEEventRing(100)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			data := json.RawMessage(`{}`)
			for j := 0; j < 100; j++ {
				r.Append("w"+string(rune('0'+n))+"/"+string(rune('0'+j)), "agent_output", data)
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				_ = r.After("w0/0")
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}

func TestSSEEventRecord_Type(t *testing.T) {
	var got interface{} = webui.SSEEventRecord{}
	if reflect.TypeOf(got).Kind() != reflect.Struct {
		t.Fatal("webui.SSEEventRecord should be a struct")
	}
}

package test

// internal/network 的黑盒判据：NewMonitor → CheckOnce → EndpointStatus。
// 用 httptest 提供真实输入，不断言内部结构。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/network"
)

func newMonitor() *network.Monitor { return network.NewMonitor(time.Second) }

// TestCheckOnceReachableServer：2xx 端点判可达，且带回 URL/延迟/时间。
func TestCheckOnceReachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st := newMonitor().CheckOnce(context.Background(), srv.URL)
	if !st.Reachable {
		t.Fatalf("★ 2xx 端点判不可达：%+v", st)
	}
	if st.URL != srv.URL {
		t.Fatalf("★ URL = %q，期望 %q", st.URL, srv.URL)
	}
	if st.LastCheck.IsZero() {
		t.Fatal("★ LastCheck 未填")
	}
}

// TestCheckOnceServerError：5xx 判不可达。
func TestCheckOnceServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	st := newMonitor().CheckOnce(context.Background(), srv.URL)
	if st.Reachable {
		t.Fatalf("★ 5xx 端点判可达：%+v", st)
	}
}

// TestCheckOnceUnreachableEndpoint：连不上的端点必须带 Error，不 panic。
func TestCheckOnceUnreachableEndpoint(t *testing.T) {
	// 127.0.0.1:1 上没有服务，连接会立刻失败
	st := newMonitor().CheckOnce(context.Background(), "http://127.0.0.1:1/health")
	if st.Reachable {
		t.Fatal("★ 连不上的端点判可达")
	}
	if st.Error == "" {
		t.Fatal("★ 失败时没给出 Error")
	}
	if st.URL != "http://127.0.0.1:1/health" {
		t.Fatalf("★ URL = %q", st.URL)
	}
}

// TestCheckOnceInvalidURL：非法 URL 必须报错而不是 panic。
func TestCheckOnceInvalidURL(t *testing.T) {
	st := newMonitor().CheckOnce(context.Background(), "://not-a-url")
	if st.Reachable || st.Error == "" {
		t.Fatalf("★ 非法 URL 没有得到失败状态：%+v", st)
	}
}

// TestFreshMonitorStatusIsEmpty：未跑过 Start 时状态为空、聚合结果可用。
func TestFreshMonitorStatusIsEmpty(t *testing.T) {
	m := newMonitor()
	if got := m.Status(); len(got) != 0 {
		t.Fatalf("★ 新 Monitor 的 Status = %v，期望空", got)
	}
	_ = m.AggregateResult() // 输出必须可调用
}

// metrics_test.go /metrics 端点行为：默认开启、可关闭、渲染包含池仪表。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/livecfg"
	"github.com/linguo2625469/l3api/internal/upstream"
)

func TestMetricsEndpointEnabled(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), MetricsEnabled: true, Version: "test"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"l3api_chat_requests_total",
		"l3api_pool_accounts",
		"l3api_build_info{version=\"test\"} 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	// /metrics 不需要鉴权（Prometheus 免配 token）。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/metrics", nil))
	if rec2.Code != 200 {
		t.Fatalf("unauth metrics code=%d want 200", rec2.Code)
	}
}

func TestMetricsEndpointDisabled(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), MetricsEnabled: false})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 404 {
		t.Fatalf("code=%d want 404 when disabled", rec.Code)
	}
}

func TestMetricsLiveWired(t *testing.T) {
	// livecfg 快照（多 key）与 /metrics 组合装配可用。
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:           p,
		Upstream:       upstream.New(),
		MetricsEnabled: true,
		Live:           livecfg.New(livecfg.Snapshot{APIKey: "k"}),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "l3api_uptime_seconds") {
		t.Error("missing uptime gauge")
	}
}

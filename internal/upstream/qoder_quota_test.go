package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// quotaServer 模拟 GET /api/v2/quota/usage。handler 返回注入的响应。
func quotaServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v2/quota/usage" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	return srv, &gotAuth
}

// TestQoderQuotaAggregatesBuckets 主额度 + 加油包求和（抓包实测响应形状）。
func TestQoderQuotaAggregatesBuckets(t *testing.T) {
	body := `{"userId":"u1","usageType":"credits","isQuotaExceeded":false,` +
		`"userQuota":{"total":300.0,"used":120.5,"remaining":179.5,"unit":"credits"},` +
		`"addOnQuota":{"total":100.0,"used":0.0,"remaining":100.0,"unit":"credits"}}`
	srv, gotAuth := quotaServer(t, 200, body)
	defer srv.Close()

	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	a := newQoderTestAuth()

	remain, total, err := c.QoderQuota(context.Background(), a)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	// remain = 179.5 + 100 = 279.5 → 四舍五入 280；total = 300 + 100 = 400。
	if remain != 280 {
		t.Errorf("remain = %d, want 280", remain)
	}
	if total != 400 {
		t.Errorf("total = %d, want 400", total)
	}
	if *gotAuth != "Bearer jt-test" {
		t.Errorf("Authorization = %q, want Bearer jt-test", *gotAuth)
	}
}

// TestQoderQuotaAddOnMissing addOnQuota 缺失时只算主额度（字段零值不报错）。
func TestQoderQuotaAddOnMissing(t *testing.T) {
	body := `{"userQuota":{"total":300.0,"used":300.0,"remaining":0.0,"unit":"credits"},"isQuotaExceeded":true}`
	srv, _ := quotaServer(t, 200, body)
	defer srv.Close()

	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	a := newQoderTestAuth()
	remain, total, err := c.QoderQuota(context.Background(), a)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if remain != 0 || total != 300 {
		t.Errorf("remain=%d total=%d, want 0/300", remain, total)
	}
}

// TestQoderQuotaHTTPError 非 200 透传状态码。
func TestQoderQuotaHTTPError(t *testing.T) {
	srv, _ := quotaServer(t, 401, `{"code":"105","message":"Login expired"}`)
	defer srv.Close()
	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	a := newQoderTestAuth()
	if _, _, err := c.QoderQuota(context.Background(), a); err == nil {
		t.Fatal("expect error")
	}
}

// TestUserResourceDetailedRoutesQoder UserResourceDetailed 对 qoder 品牌走 QoderQuota，
// 不碰 codebuddy 计费端点（endpoint 路径验证）。
func TestUserResourceDetailedRoutesQoder(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"userQuota":{"total":300.0,"used":0.0,"remaining":300.0,"unit":"credits"}}`)
	}))
	defer srv.Close()
	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	a := newQoderTestAuth()

	remain, total, expiring, err := c.UserResourceDetailed(a, 24*time.Hour)
	if err != nil {
		t.Fatalf("UserResourceDetailed: %v", err)
	}
	if path != "/api/v2/quota/usage" {
		t.Errorf("path = %q, want /api/v2/quota/usage (qoder route)", path)
	}
	if remain != 300 || total != 300 {
		t.Errorf("remain=%d total=%d, want 300/300", remain, total)
	}
	if expiring != 0 {
		t.Errorf("expiring = %d, want 0 (qoder 无快过期概念)", expiring)
	}
}

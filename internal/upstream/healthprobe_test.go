package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/l3api/internal/auth"
)

// TestProbeOneOK 正常链路：SSE delta 聚合成 "OK"，品牌缺省 codebuddy → glm-5.2。
func TestProbeOneOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v2/chat/completions") {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", Nickname: "n1", AccessToken: "at"}
	res := ProbeOne(context.Background(), c, a)
	if !res.OK {
		t.Fatalf("res=%+v want ok", res)
	}
	if res.Model != "glm-5.2" || res.Reply != "OK" {
		t.Errorf("model=%q reply=%q want glm-5.2/OK", res.Model, res.Reply)
	}
	if res.UID != "u1" || res.Nickname != "n1" || res.Brand != "codebuddy" {
		t.Errorf("identity fields wrong: %+v", res)
	}
}

// TestProbeOneHTTPError 上游 4xx → OK=false 且错误串带 http 状态。
func TestProbeOneHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", 401)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	res := ProbeOne(context.Background(), c, &auth.Auth{UID: "u1", AccessToken: "at"})
	if res.OK || res.Skipped {
		t.Fatalf("res=%+v want failure", res)
	}
	// 401 经 upstream.Error 归类为 client kind（含状态码上下文），错误串必须非空透出。
	if res.Err == "" || !strings.Contains(res.Err, "client") {
		t.Errorf("err=%q want upstream client error surfaced", res.Err)
	}
}

// TestProbeModelFor 品牌探测模型选择。
func TestProbeModelFor(t *testing.T) {
	mk := func(uid, brand string) *auth.Auth {
		a := &auth.Auth{UID: uid}
		if brand != "" {
			if _, err := auth.BackfillBrandFor(a, brand); err != nil {
				t.Fatalf("set brand: %v", err)
			}
		}
		return a
	}
	cases := []struct {
		name      string
		auth      *auth.Auth
		wantModel string
	}{
		{"codebuddy 缺省", mk("a", ""), "glm-5.2"},
		{"qoder", mk("b", "qoder"), "auto"},
		{"trae", mk("c", "trae"), "glm-5.3-flashx"},
	}
	for _, c := range cases {
		m, reason := ProbeModelFor(c.auth)
		if m != c.wantModel || reason != "" {
			t.Errorf("%s: ProbeModelFor=(%q,%q) want (%q,\"\")", c.name, m, reason, c.wantModel)
		}
	}
}

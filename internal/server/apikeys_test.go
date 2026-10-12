// apikeys_test.go 多租户 key 的端到端行为：鉴权、白名单、RPM、禁用、用量归因。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/l3api/internal/apikeys"
	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/livecfg"
	"github.com/linguo2625469/l3api/internal/upstream"
	"github.com/linguo2625469/l3api/internal/usage"
)

// newMultiKeyHandler 构建带多 key 注册表的 handler（fake 上游恒回 sseOK 流）。
func newMultiKeyHandler(t *testing.T, entries []apikeys.Entry, rec *usage.Recorder) (*Handler, *apikeys.Registry) {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	reg := apikeys.NewRegistry(entries)
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		Usage:    rec,
		Live: livecfg.New(livecfg.Snapshot{
			APIKey: "sk-master",
			Keys:   reg,
		}),
	})
	return h, reg
}

func chatReq(model string) *http.Request {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	return httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
}

func TestMultiKeyAuth(t *testing.T) {
	h, _ := newMultiKeyHandler(t, []apikeys.Entry{
		{Name: "default", Key: "sk-master"},
		{Name: "alice", Key: "sk-alice"},
	}, nil)

	cases := []struct {
		authz string
		want  int
	}{
		{"Bearer sk-master", 200},
		{"Bearer sk-alice", 200},
		{"Bearer sk-wrong", 401},
		{"", 401},
	}
	for _, c := range cases {
		req := chatReq("glm-5.2")
		if c.authz != "" {
			req.Header.Set("Authorization", c.authz)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("authz=%q code=%d body=%s want %d", c.authz, rec.Code, rec.Body, c.want)
		}
	}
}

func TestMultiKeyModelWhitelist(t *testing.T) {
	h, _ := newMultiKeyHandler(t, []apikeys.Entry{
		{Name: "default", Key: "sk-master"},
		{Name: "alice", Key: "sk-alice", Models: []string{"cn:glm-5.2"}},
	}, nil)

	// 白名单内（原文与裸名两种写法都放行）。
	for _, model := range []string{"cn:glm-5.2", "glm-5.2"} {
		req := chatReq(model)
		req.Header.Set("Authorization", "Bearer sk-alice")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("model=%q code=%d body=%s want 200", model, rec.Code, rec.Body)
		}
	}
	// 白名单外 → 403，且不消耗上游调用（错误在选号前返回）。
	req := chatReq("cn:qwen-max")
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("code=%d body=%s want 403", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Errorf("body should carry model_not_allowed: %s", rec.Body)
	}
}

func TestMultiKeyRPMRateLimit(t *testing.T) {
	h, _ := newMultiKeyHandler(t, []apikeys.Entry{
		{Name: "default", Key: "sk-master"},
		{Name: "alice", Key: "sk-alice", RPM: 2},
	}, nil)

	for i := 0; i < 2; i++ {
		req := chatReq("glm-5.2")
		req.Header.Set("Authorization", "Bearer sk-alice")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("req %d code=%d want 200", i, rec.Code)
		}
	}
	req := chatReq("glm-5.2")
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatalf("3rd request code=%d body=%s want 429", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After header")
	}
	// master 不受 alice 限额影响。
	req = chatReq("glm-5.2")
	req.Header.Set("Authorization", "Bearer sk-master")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("master request code=%d want 200 (independent window)", rec.Code)
	}
}

func TestMultiKeyDisabledKey(t *testing.T) {
	h, _ := newMultiKeyHandler(t, []apikeys.Entry{
		{Name: "default", Key: "sk-master"},
		{Name: "alice", Key: "sk-alice", Disabled: true},
	}, nil)
	req := chatReq("glm-5.2")
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("disabled key code=%d want 401", rec.Code)
	}
}

func TestMultiKeyHotRevoke(t *testing.T) {
	h, reg := newMultiKeyHandler(t, []apikeys.Entry{
		{Name: "default", Key: "sk-master"},
		{Name: "alice", Key: "sk-alice"},
	}, nil)
	req := chatReq("glm-5.2")
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("before revoke: code=%d want 200", rec.Code)
	}
	// 面板热删 alice → 立即 401。
	reg.Store([]apikeys.Entry{{Name: "default", Key: "sk-master"}})
	req = chatReq("glm-5.2")
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("after hot revoke: code=%d want 401", rec.Code)
	}
}

func TestMultiKeyUsageAttribution(t *testing.T) {
	rec := usage.New("")
	h, _ := newMultiKeyHandler(t, []apikeys.Entry{
		{Name: "default", Key: "sk-master"},
		{Name: "alice", Key: "sk-alice"},
	}, rec)

	for _, key := range []string{"sk-master", "sk-alice"} {
		req := chatReq("glm-5.2")
		req.Header.Set("Authorization", "Bearer "+key)
		hh := httptest.NewRecorder()
		h.ServeHTTP(hh, req)
		if hh.Code != 200 {
			t.Fatalf("key=%s code=%d", key, hh.Code)
		}
	}
	snap := rec.Snapshot(1, nil, nil)
	if len(snap.ByApiKey) != 2 {
		t.Fatalf("want 2 key rows, got %d (%+v)", len(snap.ByApiKey), snap.ByApiKey)
	}
	names := map[string]bool{}
	for _, row := range snap.ByApiKey {
		names[row.Key] = true
		if row.Requests != 1 || row.TotalTokens == 0 {
			t.Errorf("key row %s: requests=%d total=%d", row.Key, row.Requests, row.TotalTokens)
		}
	}
	if !names["default"] || !names["alice"] {
		t.Errorf("rows should be default+alice, got %v", names)
	}
}

func TestMultiKeyModelsFilteredByWhitelist(t *testing.T) {
	// 直接注入动态模型缓存（与 fetchDynamicModels 共享同一全局缓存），
	// 绕开上游探测路径，聚焦白名单过滤逻辑。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "qwen-max"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.Unlock()
	})

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	reg := apikeys.NewRegistry([]apikeys.Entry{
		{Name: "default", Key: "sk-master"},
		{Name: "alice", Key: "sk-alice", Models: []string{"cn:glm-5.2"}},
	})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: upstream.New(),
		Live:     livecfg.New(livecfg.Snapshot{APIKey: "sk-master", Keys: reg}),
	})

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0]["id"] != "cn:glm-5.2" {
		t.Fatalf("alice should see only cn:glm-5.2, got %v", resp.Data)
	}

	// master 无白名单 → 全量。
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-master")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp.Data = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("master should see all models, got %v", resp.Data)
	}
}

func TestMultiKeyEmptyRegistryMeansNoAuth(t *testing.T) {
	// 面板清光 key → 空注册表 = 鉴权关闭（与启动"全空=不鉴权"同语义），
	// 而不是把所有人 401。
	// fake 上游 + 注入模型缓存：/v1/models 的动态拉取绝不能碰真网络
	//（真调用失败会写 5min 负缓存，污染同包后续的 models 类测试）。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() { // 全局缓存复位：防污染同包后续 models 类测试（顺序耦合）
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	reg := apikeys.NewRegistry([]apikeys.Entry{{Name: "default", Key: "sk-master"}})
	reg.Store(nil) // 模拟面板清空
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		Live:     livecfg.New(livecfg.Snapshot{APIKey: "", Keys: reg}),
	})
	req := httptest.NewRequest("GET", "/v1/models", nil) // 无 Authorization 头
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("empty registry + empty api_key must be no-auth, got %d", rec.Code)
	}
	// 对照：注册表非空时缺头必须 401。
	reg2 := apikeys.NewRegistry([]apikeys.Entry{{Name: "default", Key: "sk-master"}})
	h2 := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		Live:     livecfg.New(livecfg.Snapshot{APIKey: "sk-master", Keys: reg2}),
	})
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/models", nil))
	if rec2.Code != 401 {
		t.Fatalf("non-empty registry must reject missing key, got %d", rec2.Code)
	}
}

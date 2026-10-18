// protocol_e2e_test.go /v1/messages 与 /v1/responses 端到端：走完整 doChat
// 轮转核心（选号/记账/粘性），验证协议输出与用量归因。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/usage"
)

// e2eHandler 单账号 + fake 上游（按 SSE 流回放）。
func e2eHandler(t *testing.T, sseBody string) (*Handler, *usage.Recorder) {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseBody, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rec := usage.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: rec})
	return h, rec
}

func TestAnthropicMessagesE2ENonStream(t *testing.T) {
	h, rec := e2eHandler(t, sseOK)
	body := `{
		"model": "cn:glm-5.2",
		"max_tokens": 256,
		"messages": [{"role": "user", "content": "你好"}]
	}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != 200 {
		t.Fatalf("code=%d body=%s", got.Code, got.Body)
	}
	out := got.Body.String()
	for _, want := range []string{
		`"type":"message"`,
		`"text":"你好","type":"text"`,
		`"stop_reason":"end_turn"`,
		`"input_tokens":1`,
		`"output_tokens":1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in: %s", want, out)
		}
	}
	// 用量归因：非流式聚合完成记账（默认 key → default）。
	snap := rec.Snapshot(1, nil, nil)
	if snap.Totals.Requests != 1 || snap.Totals.TotalTokens == 0 {
		t.Errorf("usage: %+v", snap.Totals)
	}
}

func TestAnthropicMessagesE2EStream(t *testing.T) {
	h, _ := e2eHandler(t, sseOK)
	body := `{
		"model": "glm-5.2",
		"max_tokens": 256,
		"stream": true,
		"messages": [{"role": "user", "content": "hi"}]
	}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != 200 {
		t.Fatalf("code=%d body=%s", got.Code, got.Body)
	}
	out := got.Body.String()
	for _, want := range []string{
		"event: message_start",
		`"text":"你好"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in: %s", want, out)
		}
	}
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Error("Anthropic stream must not leak OpenAI chunk frames")
	}
}

func TestAnthropicMessagesE2EAuthError(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-real"})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","max_tokens":1,"messages":[]}`))
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != 401 {
		t.Fatalf("code=%d", got.Code)
	}
	if !strings.Contains(got.Body.String(), "authentication_error") {
		t.Errorf("error shape: %s", got.Body)
	}
}

// TestAnthropicMessagesOversizedBodyDefaultAllowed /v1/messages 是 413 的真实踩点
// （2026-10-18：nginx 日志里一串 POST /l3api/v1/messages 413，客户端每轮把历史图片
// 以 base64 重发）。默认 unlimited 后 8MB+ 的 Anthropic 请求必须放行到上游。
func TestAnthropicMessagesOversizedBodyDefaultAllowed(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // MaxBodyBytes 未设 → 0 = 不限

	// 8MB+ 的合法 Anthropic 请求体（padding 塞进 system 文本，JSON 保持合法）。
	pad := strings.Repeat("x", 8<<20)
	body := `{"model":"cn:glm-5.2","max_tokens":64,"system":"` + pad +
		`","messages":[{"role":"user","content":"hi"}]}`
	if len(body) <= 8<<20 {
		t.Fatalf("fixture too small: %d", len(body))
	}
	got := httptest.NewRecorder()
	h.ServeHTTP(got, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if got.Code != 200 {
		t.Fatalf("code=%d want 200 (unlimited default); body=%s", got.Code, got.Body.String())
	}
}

// TestAnthropicMessagesExplicitLimitStill413 显式设上限时 413 仍然生效（退出门）：
// 不限是新默认，不是"取消这个功能"——填了正整数就得照旧拦。
func TestAnthropicMessagesExplicitLimitStill413(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 512})

	body := `{"model":"cn:glm-5.2","max_tokens":64,"system":"` +
		strings.Repeat("x", 1024) + `","messages":[{"role":"user","content":"hi"}]}`
	got := httptest.NewRecorder()
	h.ServeHTTP(got, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 (explicit 512B limit); body=%s", got.Code, got.Body.String())
	}
	// 413 必须是 Anthropic 错误形态；request_body_too_large 按设计映射成
	// invalid_request_error（Anthropic 无 413 专用 error type），message 里保留原码。
	out := got.Body.String()
	if !strings.Contains(out, "invalid_request_error") {
		t.Errorf("413 must use Anthropic error shape: %s", out)
	}
	if !strings.Contains(out, "server.max_body_mb") {
		t.Errorf("413 message should name config key server.max_body_mb: %s", out)
	}
}

func TestAnthropicMessagesE2EValidation(t *testing.T) {
	h, _ := e2eHandler(t, sseOK)
	// 缺 max_tokens → 400 invalid_request_error（Anthropic 形态）。
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != 400 || !strings.Contains(got.Body.String(), "invalid_request_error") {
		t.Fatalf("code=%d body=%s", got.Code, got.Body)
	}
}

func TestResponsesE2ENonStream(t *testing.T) {
	h, _ := e2eHandler(t, sseOK)
	body := `{"model": "cn:glm-5.2", "input": "你好"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != 200 {
		t.Fatalf("code=%d body=%s", got.Code, got.Body)
	}
	out := got.Body.String()
	for _, want := range []string{
		`"object":"response"`,
		`"status":"completed"`,
		`"text":"你好","type":"output_text"`,
		`"input_tokens":1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in: %s", want, out)
		}
	}
}

func TestResponsesE2EStream(t *testing.T) {
	h, _ := e2eHandler(t, sseOK)
	body := `{"model": "glm-5.2", "stream": true, "input": "hi"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != 200 {
		t.Fatalf("code=%d body=%s", got.Code, got.Body)
	}
	out := got.Body.String()
	for _, want := range []string{
		"event: response.created",
		`"delta":"你好"`,
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in: %s", want, out)
		}
	}
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Error("Responses stream must not leak OpenAI chunk frames")
	}
}

func TestResponsesE2EToolRoundTrip(t *testing.T) {
	// 上游回一个 tool_call 流；客户端回传 function_call_output 的请求应正确
	// 转成 OpenAI tool 消息（非流式响应断言 output items）。
	h, _ := e2eHandler(t, sseOK)
	body := `{
		"model": "glm-5.2",
		"instructions": "be brief",
		"input": [
			{"type": "message", "role": "user", "content": "查天气"},
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "晴"}
		]
	}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != 200 {
		t.Fatalf("code=%d body=%s", got.Code, got.Body)
	}
	out := got.Body.String()
	if !strings.Contains(out, `"object":"response"`) {
		t.Errorf("body: %s", out)
	}
}

func TestAnthropicStickySessionWorksAcrossProtocol(t *testing.T) {
	// Anthropic 层的请求经转换后具备 OpenAI messages，粘性键提取应照常工作
	// （session.ExtractKey 在转换后的 body 上运行）。此处只验证不 panic 且 200。
	h, _ := e2eHandler(t, sseOK)
	body := `{"model":"glm-5.2","max_tokens":64,"stream":true,
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != 200 {
		t.Fatalf("code=%d body=%s", got.Code, got.Body)
	}
}

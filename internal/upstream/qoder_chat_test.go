package upstream

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// ── qoderContentText ─────────────────────────────────────────────────────────

func TestQoderContentText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", `""`, ""},
		{"plain", `"你好"`, "你好"},
		{"string with escapes", `"a\nb\t\"c\""`, "a\nb\t\"c\""},
		{"multimodal single text", `[{"type":"text","text":"hello"}]`, "hello"},
		{"multimodal text+image", `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"u"}},{"type":"text","text":"b"}]`, "ab"},
		{"multimodal no text", `[{"type":"image_url","image_url":{"url":"u"}}]`, ""},
		{"null", `null`, ""},
	}
	for _, c := range cases {
		if got := qoderContentText(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("%s: qoderContentText(%s) = %q, want %q", c.name, c.raw, got, c.want)
		}
	}
}

// ── qoderBuildChatBody ──────────────────────────────────────────────────────

func TestQoderBuildChatBodyShape(t *testing.T) {
	in := `{"model":"qwen3.8-flash","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}]}`
	out, err := qoderBuildChatBody([]byte(in), "conv-1", "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var p qoderChatPayload
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 会话级 id：同 conversationID 应稳定复用（非随机）。
	if p.SessionID == "" || p.RequestSetID != p.SessionID {
		t.Fatalf("session id reuse: request_set_id=%q session_id=%q", p.RequestSetID, p.SessionID)
	}
	if p.RequestID == "" || p.ChatRecordID == "" {
		t.Fatalf("missing ids: request_id=%q chat_record_id=%q", p.RequestID, p.ChatRecordID)
	}
	if !p.Stream || p.ChatTask != "FREE_INPUT" || p.AgentID != "agent_common" {
		t.Fatalf("fixed fields wrong: %+v", p)
	}
	if p.ModelConfig.Key != "qfmodel" || p.ModelConfig.Format != "openai" {
		t.Fatalf("model_config wrong: %+v", p.ModelConfig)
	}
	// system 提到顶层、不进 messages。
	if p.System != "你是助手" {
		t.Fatalf("system = %q", p.System)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != "user" || p.Messages[0].Content != "你好" {
		t.Fatalf("messages wrong: %+v", p.Messages)
	}
	if len(p.Messages[0].Contents) != 1 || p.Messages[0].Contents[0].Text != "你好" {
		t.Fatalf("contents wrong: %+v", p.Messages[0].Contents)
	}
}

func TestQoderBuildChatBodyNoSystem(t *testing.T) {
	in := `{"messages":[{"role":"user","content":"hi"}]}`
	out, err := qoderBuildChatBody([]byte(in), "", "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var p qoderChatPayload
	_ = json.Unmarshal(out, &p)
	if p.System != "" {
		t.Fatalf("system should be empty, got %q", p.System)
	}
}

func TestQoderBuildChatBodyErrors(t *testing.T) {
	if _, err := qoderBuildChatBody([]byte(`{"messages":[]}`), "", ""); err == nil {
		t.Fatal("empty messages should error")
	}
	if _, err := qoderBuildChatBody([]byte(`{"messages":[{"role":"system","content":"only"}]}`), "", ""); err == nil {
		t.Fatal("system-only should error")
	}
}

// ── qoderClassifyHTTP ───────────────────────────────────────────────────────

func TestQoderClassifyHTTP(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{401, ``, ErrSessionDead},
		{403, ``, ErrSessionDead},
		{400, `{"code":"101","message":"Signature invalid"}`, ErrClient},
		{400, `Signature invalid`, ErrClient},
		{429, ``, ErrSoftRate},
		{400, `{"msg":"rate limit"}`, ErrSoftRate},
		{402, ``, ErrHardCredit},
		{400, `{"msg":"quota exceeded"}`, ErrHardCredit},
		{404, ``, ErrNotFound},
		{500, ``, ErrServer},
		{400, `{"msg":"bad request"}`, ErrClient},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := qoderClassifyHTTP(c.status, c.body); got != c.want {
			t.Errorf("(%d,%q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// ── qoderSSEUnwrapper ───────────────────────────────────────────────────────

// qoderSSE 客户端真实形态（已验证）：data:{"body":"{\"choices\":[...]}"}。
func TestQoderSSEUnwrapperHappyPath(t *testing.T) {
	inner1 := `{"choices":[{"delta":{"content":"Hi","reasoning_content":""},"index":0}],"id":"chatcmpl-x"}`
	inner2 := `{"choices":[{"delta":{"content":" there"},"index":0}],"id":"chatcmpl-x"}`
	in := "data:{\"body\":" + jsonStr(inner1) + ",\"statusCodeValue\":200}\n\n" +
		"data:{\"body\":" + jsonStr(inner2) + ",\"statusCodeValue\":200}\n\n" +
		"data: [DONE]\n\n"
	u := &qoderSSEUnwrapper{rc: io.NopCloser(strings.NewReader(in))}
	out, err := io.ReadAll(u)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "data: "+inner1) || !strings.Contains(got, "data: "+inner2) {
		t.Fatalf("missing inner frames:\n%s", got)
	}
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Fatalf("missing DONE:\n%q", got)
	}
}

// 流内认证失败：statusCodeValue=403 → *Error ErrSessionDead。
func TestQoderSSEUnwrapperInStreamAuthError(t *testing.T) {
	in := "data:{\"body\":\"{\\\"code\\\":\\\"105\\\",\\\"message\\\":\\\"Login expired\\\"}\",\"statusCodeValue\":403}\n\n"
	u := &qoderSSEUnwrapper{rc: io.NopCloser(strings.NewReader(in))}
	_, err := io.ReadAll(u)
	if err == nil {
		t.Fatal("expect error")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type %T, want *Error", err)
	}
	if ue.Kind != ErrSessionDead {
		t.Fatalf("kind = %v, want ErrSessionDead", ue.Kind)
	}
	if ue.Status != 403 {
		t.Fatalf("status = %d, want 403", ue.Status)
	}
}

// 流内认证失败：statusCodeValue=401。
func TestQoderSSEUnwrapperInStream401(t *testing.T) {
	in := "data:{\"body\":\"{}\",\"statusCodeValue\":401}\n\n"
	u := &qoderSSEUnwrapper{rc: io.NopCloser(strings.NewReader(in))}
	_, err := io.ReadAll(u)
	if ue, ok := err.(*Error); !ok || ue.Kind != ErrSessionDead || ue.Status != 401 {
		t.Fatalf("err = %v", err)
	}
}

// 正常帧里 body.code 非 105 不触发认证错误。
func TestQoderSSEUnwrapperNoFalsePositive(t *testing.T) {
	inner := `{"choices":[{"delta":{"content":"ok"}}],"code":"0"}`
	in := "data:{\"body\":" + jsonStr(inner) + ",\"statusCodeValue\":200}\n\n"
	u := &qoderSSEUnwrapper{rc: io.NopCloser(strings.NewReader(in))}
	out, err := io.ReadAll(u)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"ok"`) {
		t.Fatalf("content lost: %s", out)
	}
}

// 信封解包后 id/created/choices 原样保留（handler 聚合依赖这些字段）。
func TestQoderSSEUnwrapperPreservesOpenAIFields(t *testing.T) {
	inner := `{"id":"chatcmpl-abc","created":1790023304,"choices":[{"delta":{"content":"x"},"index":0,"finish_reason":null}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	in := "data:{\"body\":" + jsonStr(inner) + "}\n\n"
	u := &qoderSSEUnwrapper{rc: io.NopCloser(strings.NewReader(in))}
	out, err := io.ReadAll(u)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"chatcmpl-abc"`) || !strings.Contains(string(out), `"total_tokens":3`) {
		t.Fatalf("fields lost:\n%s", out)
	}
}

// jsonStr 把一段 JSON 文本转义成 JSON 字符串字面量（外层信封 body 的值形态）。
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

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

// ── 工具调用透传（2026-10-21）────────────────────────────────────────────────

// TestQoderBuildChatBodyCarriesToolCalls 工具往返的配对信息必须原样进上游载荷。
//
// 回归背景：此前本桥只透传 role/content，把 assistant 的 tool_calls 与 tool
// 结果的 tool_call_id 整个丢了。Qoder 网关按 OpenAI 规范严格校验配对，缺
// tool_call_id 直接反序列化失败并回 provider_error（实测原文："messages[2]:
// missing field `tool_call_id`"）——第二轮请求必然失败，客户端表现为「模型
// 跑几秒就断开」且无任何报错。
func TestQoderBuildChatBodyCarriesToolCalls(t *testing.T) {
	in := `{"messages":[
		{"role":"user","content":"北京天气怎么样？"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"晴，26℃"}
	]}`
	out, err := qoderBuildChatBody([]byte(in), "conv-1", "dfmodel")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var p qoderChatPayload
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Messages) != 3 {
		t.Fatalf("messages count = %d, want 3", len(p.Messages))
	}
	// assistant 的 tool_calls 必须完整带出（id/type/function.name/arguments）。
	asst := p.Messages[1]
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant tool_calls lost: %+v", asst)
	}
	tc := asst.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" ||
		tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("tool_call fields wrong: %+v", tc)
	}
	// tool 结果消息的 tool_call_id 必须带出（缺它上游直接拒）。
	tool := p.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" {
		t.Fatalf("tool message wrong: %+v", tool)
	}
	if tool.Content != "晴，26℃" {
		t.Fatalf("tool content wrong: %+v", tool)
	}
}

// 无工具的普通请求不应凭空长出 tool_calls/tool_call_id 字段（omitempty）。
func TestQoderBuildChatBodyNoToolFieldsWhenAbsent(t *testing.T) {
	in := `{"messages":[{"role":"user","content":"hi"}]}`
	out, err := qoderBuildChatBody([]byte(in), "conv-1", "dfmodel")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(string(out), "tool_calls") || strings.Contains(string(out), "tool_call_id") {
		t.Fatalf("empty tool fields should be omitted: %s", out)
	}
}

// ── 流内 provider_error 识别（2026-10-21）──────────────────────────────────

// TestQoderSSEUnwrapperProviderError 流内业务错误必须转成 *Error。
//
// 回归背景：Qoder 网关把上游模型侧错误包在 HTTP 200 的 SSE 信封里回传
// （statusCodeValue=400 + body.code="provider_error"）。此前这类帧被当成
// 正常数据透传，客户端只收到空帧 + [DONE]，既看不到原因也无法换号重试。
func TestQoderSSEUnwrapperProviderError(t *testing.T) {
	detail := `{\"error\":{\"message\":\"Failed to deserialize the JSON body into the target type: messages[2]: missing field tool_call_id\"}}`
	inner := `{"code":"provider_error","message":"Error in upstream response","type":"provider_error","details":"` + detail + `"}`
	in := "data:{\"body\":" + jsonStr(inner) + ",\"statusCodeValue\":400,\"statusCode\":\"BAD_REQUEST\"}\n\n"
	u := &qoderSSEUnwrapper{rc: io.NopCloser(strings.NewReader(in))}
	_, err := io.ReadAll(u)
	if err == nil {
		t.Fatal("expect error for provider_error frame")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type %T, want *Error", err)
	}
	// 请求体畸形 → ErrBadParams（不罚号：换任何号都会失败）。
	if ue.Kind != ErrBadParams {
		t.Fatalf("kind = %v, want ErrBadParams", ue.Kind)
	}
	if ue.Status != 400 {
		t.Fatalf("status = %d, want 400", ue.Status)
	}
	// 真实原因（上游原文）必须带进 Msg——客户端要看到可排查的信息。
	if !strings.Contains(ue.Msg, "missing field") {
		t.Fatalf("Msg lost upstream detail: %q", ue.Msg)
	}
}

// provider_error 但原因不是请求体畸形 → 按上游语义归类（不硬判 ErrBadParams）。
func TestQoderSSEUnwrapperProviderErrorNonParams(t *testing.T) {
	inner := `{"code":"provider_error","message":"Error in upstream response","type":"provider_error","details":"{\"error\":{\"message\":\"upstream model temporarily unavailable\"}}"}`
	in := "data:{\"body\":" + jsonStr(inner) + ",\"statusCodeValue\":500}\n\n"
	u := &qoderSSEUnwrapper{rc: io.NopCloser(strings.NewReader(in))}
	_, err := io.ReadAll(u)
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type %T, want *Error", err)
	}
	if ue.Kind != ErrServer {
		t.Fatalf("kind = %v, want ErrServer", ue.Kind)
	}
}

// 正常 OpenAI 兼容帧（含 code 字段）不得被误判成 provider_error。
func TestQoderSSEUnwrapperNoFalsePositiveProviderError(t *testing.T) {
	inner := `{"id":"chatcmpl-x","choices":[{"delta":{"content":"ok"},"index":0}],"code":"0"}`
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

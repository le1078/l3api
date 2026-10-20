// trae_chat_test.go Trae chat SSE 桥的请求体构造与事件翻译测试。
package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

func TestTraeBuildChatBody_Basic(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","messages":[
		{"role":"system","content":"你是助手"},
		{"role":"user","content":[{"type":"text","text":"你好"}]}
	]}`)
	out, err := traeBuildChatBody(body, "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"function":"chat_v3"`, `"stream":true`,
		`"config_name":"glm-5.3"`, `"model":"glm-5.3"`, `"role":"system"`, `你好`} {
		if !strings.Contains(s, want) {
			t.Fatalf("body missing %s: %s", want, s)
		}
	}
}

func TestTraeBuildChatBody_NoModel(t *testing.T) {
	// 空模型 → 不传 config_name/model（上游默认模型通道）
	out, err := traeBuildChatBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "config_name") || strings.Contains(s, `"model"`) {
		t.Fatalf("empty model should omit config_name/model: %s", s)
	}
}

func TestTraeBuildChatBody_DeveloperRole(t *testing.T) {
	// developer → system（OpenAI 新名归一）
	out, err := traeBuildChatBody([]byte(`{"messages":[{"role":"developer","content":"S"},{"role":"user","content":"u"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"role":"system"`) {
		t.Fatalf("developer not normalized: %s", out)
	}
}

// traeFakeStream 构造一条事件流供翻译器测试。
func traeFakeStream(events ...string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(strings.Join(events, "\n") + "\n"))
}

func TestTraeTranslator_HappyPath(t *testing.T) {
	tr := &traeSSETranslator{
		rc: traeFakeStream(
			"event:metadata\ndata: {\"session_id\":\"s1\"}\n",
			"event:output\ndata: {\"response\":\"你好\",\"reasoning_content\":\"想一下\"}\n",
			"event:output\ndata: {\"response\":\"世界\"}\n",
			"event:token_usage\ndata: {\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}\n",
			"event:done\ndata: {\"finish_reason\":\"stop\"}\n",
		),
		cancel: func() {},
		model:  "glm-5.3",
		id:     "chatcmpl-trae-test",
	}
	if ue := tr.preread(); ue != nil {
		t.Fatalf("preread: %v", ue)
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	// preread 缓冲 + 后续 Read 拼接 = 全部输出
	all := string(tr.pending) + string(raw)
	for _, want := range []string{`"content":"你好"`, `"reasoning_content":"想一下"`, `"content":"世界"`,
		`"prompt_tokens":10`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(all, want) {
			t.Fatalf("translated stream missing %s:\n%s", want, all)
		}
	}
	// 角色帧只发一次（首片）
	if strings.Count(all, `"role":"assistant"`) != 1 {
		t.Fatalf("role should appear once:\n%s", all)
	}
}

func TestTraeTranslator_PrereadErrorClassified(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		kind ErrKind
	}{
		{1001, "auth expired", ErrSessionDead},
		{1005, "quota exhausted", ErrHardCredit},
		{4008, "quota exhausted", ErrHardCredit},
		{4011, "requests have exceeded the rate limit", ErrSoftRate},
		{3003, "all models failed", ErrClient},
		{4023, "model is unknown", ErrClient},
		{9999, "mystery", ErrServer},
	}
	for _, tc := range cases {
		tr := &traeSSETranslator{
			rc: traeFakeStream(
				"event:progress_notice\ndata: {\"name\":\"construct_prompt\"}\n",
				"event:error\ndata: {\"code\":"+itoa(tc.code)+",\"message\":"+quote(tc.msg)+"}\n",
				"event:done\ndata: {\"finish_reason\":\"\"}\n",
			),
			cancel: func() {},
			id:     "t",
		}
		ue := tr.preread()
		if ue == nil {
			t.Fatalf("code %d: preread should fail", tc.code)
		}
		if ue.Kind != tc.kind {
			t.Fatalf("code %d: kind=%v want %v", tc.code, ue.Kind, tc.kind)
		}
	}
}

func TestTraeTranslator_LateErrorPassthrough(t *testing.T) {
	// 正文已经开始后到达的 error 帧：翻译为 OpenAI error 帧透传 + [DONE] 收尾
	// （客户端能看到真实上游错误文案），pending 放完后 Read 返回 lateErr
	// （2026-10-20：让 handler 据此分类罚号——此前 lateErr 只写不读）。
	tr := &traeSSETranslator{
		rc: traeFakeStream(
			"event:output\ndata: {\"response\":\"前半截\"}\n",
			"event:error\ndata: {\"code\":3003,\"message\":\"all models failed\"}\n",
		),
		cancel: func() {},
		id:     "t",
	}
	if ue := tr.preread(); ue != nil {
		t.Fatalf("preread: %v", ue)
	}
	raw, err := io.ReadAll(tr)
	if err == nil {
		t.Fatal("Read 应返回 lateErr（pending 放完后）——否则罚号路径收不到")
	}
	all := string(tr.pending) + string(raw)
	if !strings.Contains(all, `"error"`) || !strings.Contains(all, "all models failed") {
		t.Fatalf("late error not passthrough:\n%s", all)
	}
	if !strings.Contains(all, "data: [DONE]") {
		t.Fatalf("missing [DONE]:\n%s", all)
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrClient {
		t.Fatalf("lateErr kind = %v, want ErrClient", err)
	}
	// 二次 Read：一次性语义（不重复报错，恒 EOF）。
	if _, err2 := tr.Read(make([]byte, 8)); err2 != io.EOF {
		t.Fatalf("second read err = %v, want EOF", err2)
	}
}

// TestTraeTranslator_LateSessionDeadPenalized 流内 session 失效（code 1001）
// 走 ErrSessionDead——handler 侧 NoteSessionDead 阈值化禁用。这是本次修复的
// 核心场景：cookie 失效后每次请求都开流吐几帧再报错，账号必须被识别出来。
func TestTraeTranslator_LateSessionDeadPenalized(t *testing.T) {
	tr := &traeSSETranslator{
		rc: traeFakeStream(
			"event:output\ndata: {\"response\":\"x\"}\n",
			"event:error\ndata: {\"code\":1001,\"message\":\"auth expired\"}\n",
		),
		cancel: func() {},
		id:     "t",
	}
	if ue := tr.preread(); ue != nil {
		t.Fatalf("preread: %v", ue)
	}
	_, err := io.ReadAll(tr)
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrSessionDead {
		t.Fatalf("kind = %v, want ErrSessionDead", err)
	}
}

func TestTraeTranslator_EOFWithoutDone(t *testing.T) {
	// 上游断流无 done：补 [DONE] 收尾（StreamHint 依赖它停读）。
	tr := &traeSSETranslator{
		rc:     traeFakeStream("event:output\ndata: {\"response\":\"x\"}\n"),
		cancel: func() {},
		id:     "t",
	}
	if ue := tr.preread(); ue != nil {
		t.Fatalf("preread: %v", ue)
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tr.pending)+string(raw), "data: [DONE]") {
		t.Fatalf("missing [DONE] on EOF")
	}
}

// 小工具。
func itoa(i int) string { return strconv.Itoa(i) }

func quote(s string) string { return `"` + s + `"` }

// ── 工具调用（2026-10-21）────────────────────────────────────────────────────

// TestTraeBuildChatBody_SendsTools 请求体必须带上 tools 声明。
//
// 回归背景：此前 traeBuildChatBody **从不发送 tools**，上游因此看不到任何工具
// 声明，模型只会用自然语言说「我没有 get_weather 工具」——用户侧表现为
// 「Trae 完全不调用工具」。上游本身是支持的（实测带 tools 时上游会正常吐
// tool_calls）。
func TestTraeBuildChatBody_SendsTools(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"get weather",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`)
	out, err := traeBuildChatBody(body, "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"tools"`) || !strings.Contains(s, `"get_weather"`) {
		t.Fatalf("tools missing from body: %s", s)
	}
}

// TestTraeBuildChatBody_ToolsParametersMustBeString parameters 必须是 JSON 字符串。
//
// 上游按 Go 结构体 FunctionDefinition 反序列化，其 parameters 字段类型是
// string——传对象会得到 400（实测 code 4001："cannot unmarshal object into Go
// struct field FunctionDefinition.tools.function.parameters of type string"）。
func TestTraeBuildChatBody_ToolsParametersMustBeString(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"get weather",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`)
	out, err := traeBuildChatBody(body, "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	var req traeChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(req.Tools))
	}
	got := req.Tools[0].Function.Parameters
	if !strings.HasPrefix(got, "{") {
		t.Fatalf("parameters must be a JSON string, got %q", got)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(got), &probe); err != nil {
		t.Fatalf("parameters not valid JSON: %v", err)
	}
	if probe["type"] != "object" {
		t.Fatalf("parameters content wrong: %v", probe)
	}
	// name/description 必须保留（上游靠它们把工具喂给模型）。
	if req.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tool name lost: %+v", req.Tools[0])
	}
	if req.Tools[0].Function.Description != "get weather" {
		t.Fatalf("tool description lost: %+v", req.Tools[0])
	}
}

// 没有 tools 的请求不得凭空多出 tools 字段（omitempty）。
func TestTraeBuildChatBody_NoToolsOmitted(t *testing.T) {
	out, err := traeBuildChatBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"tools"`) {
		t.Fatalf("tools should be omitted: %s", out)
	}
}

// TestTraeBuildChatBody_ToolCallRoundTrip assistant 的 tool_calls 与 tool 结果
// 必须带出，且用 Trae 的**原生** function_call 形态（不是 OpenAI 的 function）。
//
// 实测：传 OpenAI 形态上游报 "read field 4 'FunctionCall' error: required field
// Name is not set"——上游的 ToolCall 第 4 号字段是 FunctionCall。
func TestTraeBuildChatBody_ToolCallRoundTrip(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"北京天气怎么样？"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function",
		 "function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"晴，26℃"}
	]}`)
	out, err := traeBuildChatBody(body, "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	var req traeChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages count = %d, want 3", len(req.Messages))
	}
	asst := req.Messages[1]
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant tool_calls lost: %+v", asst)
	}
	if asst.ToolCalls[0].FunctionCall.Name != "get_weather" {
		t.Fatalf("function_call.name lost: %+v", asst.ToolCalls[0])
	}
	if asst.ToolCalls[0].FunctionCall.Arguments != `{"city":"北京"}` {
		t.Fatalf("function_call.arguments lost: %+v", asst.ToolCalls[0])
	}
	if asst.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool_call id lost: %+v", asst.ToolCalls[0])
	}
	if req.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("tool_call_id lost: %+v", req.Messages[2])
	}
	// 原生形态必须是 function_call（不能是 OpenAI 的 function）。
	if !strings.Contains(string(out), `"function_call"`) {
		t.Fatalf("native function_call shape missing: %s", out)
	}
}

// TestTraeTranslator_ToolCalls 上游 tool_calls 必须翻译成 OpenAI delta.tool_calls。
//
// 上游实测形态：首片带 id/name，后续片只有 arguments 片段（id/name 为空串）。
// 翻译成 OpenAI 规范：id/name 只在首片出现，后续片**删键**（置空会让客户端
// 看到同一次调用的 id 分裂）。
func TestTraeTranslator_ToolCalls(t *testing.T) {
	tr := &traeSSETranslator{
		rc: traeFakeStream(
			"event:output\ndata: {\"response\":\"\",\"reasoning_content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call_bb69\",\"type\":\"function\",\"function_call\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"\"}}]}\n",
			"event:output\ndata: {\"response\":\"\",\"reasoning_content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"\",\"type\":\"function\",\"function_call\":{\"name\":\"\",\"arguments\":\"city\\\":\\\"北京\\\"}\"}}]}\n",
			"event:done\ndata: {\"finish_reason\":\"stop\"}\n",
		),
		cancel: func() {},
		model:  "glm-5.3",
		id:     "chatcmpl-trae-test",
	}
	if ue := tr.preread(); ue != nil {
		t.Fatalf("preread: %v", ue)
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	all := string(tr.pending) + string(raw)
	if !strings.Contains(all, `"tool_calls"`) {
		t.Fatalf("tool_calls not translated:\n%s", all)
	}
	// 首片必须带 id + name。
	if !strings.Contains(all, `"id":"call_bb69"`) {
		t.Fatalf("tool_call id missing:\n%s", all)
	}
	if !strings.Contains(all, `"name":"get_weather"`) {
		t.Fatalf("tool_call name missing:\n%s", all)
	}
	// arguments 片段必须逐片透传（拼起来是完整 JSON）。
	if !strings.Contains(all, `arguments`) {
		t.Fatalf("tool_call arguments missing:\n%s", all)
	}
	// 工具回合的 finish_reason 必须是 tool_calls（上游恒给 stop，但客户端靠
	// finish_reason 判断该执行工具）。
	if !strings.Contains(all, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason should be tool_calls for tool turn:\n%s", all)
	}
}

// 工具调用的后续片不得重复带 name（OpenAI 规范：name 只在首片）。
func TestTraeTranslator_ToolCallNameOnlyFirstSlice(t *testing.T) {
	tr := &traeSSETranslator{
		rc: traeFakeStream(
			"event:output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function_call\":{\"name\":\"g\",\"arguments\":\"{\\\"\"}}]}\n",
			"event:output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"\",\"type\":\"function\",\"function_call\":{\"name\":\"\",\"arguments\":\"a\\\":1}\"}}]}\n",
		),
		cancel: func() {},
		id:     "t",
	}
	if ue := tr.preread(); ue != nil {
		t.Fatalf("preread: %v", ue)
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	all := string(tr.pending) + string(raw)
	if strings.Count(all, `"name":"g"`) != 1 {
		t.Fatalf("name should appear once:\n%s", all)
	}
}

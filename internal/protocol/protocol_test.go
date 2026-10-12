// protocol_test.go 兼容层的协议转换测试：Anthropic/Responses 的请求解析、
// 非流式转换、流式事件序列。
package protocol

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// sse 回放一个 OpenAI SSE 流（\n\n 分帧）。
func sse(frames ...string) string {
	out := ""
	for _, f := range frames {
		out += "data: " + f + "\n\n"
	}
	return out + "data: [DONE]\n\n"
}

const chunkText1 = `{"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}`
const chunkText2 = `{"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"，世界"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
const chunkFinish = `{"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
const chunkTool = `{"id":"chatcmpl-2","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`
const chunkTool2 = `{"id":"chatcmpl-2","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}`
const chunkFinishTool = `{"id":"chatcmpl-2","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
const chunkUsage = `{"id":"chatcmpl-2","model":"glm-5.2","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}`

func TestAnthropicRequestConversion(t *testing.T) {
	reqBody := `{
		"model": "cn:glm-5.2",
		"max_tokens": 1024,
		"system": "你是助手",
		"stream": false,
		"stop_sequences": ["END"],
		"messages": [
			{"role": "user", "content": "天气如何？"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "我来查"},
				{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {"city": "北京"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": "晴 30 度"}
			]}
		],
		"tools": [{"name": "get_weather", "description": "查天气", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "auto"}
	}`
	req, chatBody, err := ParseAnthropicRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatalf("chat body: %v", err)
	}
	if chat["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens: %v", chat["max_tokens"])
	}
	if chat["system"] != "你是助手" {
		t.Errorf("system: %v", chat["system"])
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 3 { // user / assistant(tool) / tool（tool_result 无伴随文本块 → 不产生 user 消息）
		t.Fatalf("messages: %d (%v)", len(msgs), msgs)
	}
	// assistant 的 tool_calls 转换。
	asst, _ := msgs[1].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls: %v", asst)
	}
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if tc["id"] != "call_1" || fn["name"] != "get_weather" {
		t.Errorf("tool call: %v", tc)
	}
	if args, _ := fn["arguments"].(string); !strings.Contains(args, "北京") {
		t.Errorf("arguments: %v", args)
	}
	// tool_result → tool message。
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "晴 30 度" {
		t.Errorf("tool message: %v", toolMsg)
	}
	// tools 转换。
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %v", chat["tools"])
	}
	tool0, _ := tools[0].(map[string]any)
	if tool0["type"] != "function" {
		t.Errorf("tool type: %v", tool0)
	}
	_ = req
}

func TestAnthropicMaxTokensRequired(t *testing.T) {
	if _, _, err := ParseAnthropicRequest([]byte(`{"model":"glm-5.2","messages":[]}`)); err == nil {
		t.Fatal("missing max_tokens must be rejected")
	}
}

func TestAnthropicStreamEvents(t *testing.T) {
	w := &testResponseWriter{header: http.Header{}}
	stream := sse(chunkText1, chunkText2, chunkFinish)
	if err := WriteAnthropicStream(w, strings.NewReader(stream), "glm-5.2"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := w.buf.String()
	for _, want := range []string{
		"event: message_start",
		`"content_block":{"index":0,"text":"","type":"text"}`,
		`"text":"你好","type":"text_delta"`,
		`"text":"，世界","type":"text_delta"`,
		"event: content_block_stop",
		`"stop_reason":"end_turn"`,
		`"output_tokens":5`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestAnthropicStreamToolUse(t *testing.T) {
	w := &testResponseWriter{header: http.Header{}}
	stream := sse(chunkTool, chunkTool2, chunkFinishTool, chunkUsage)
	if err := WriteAnthropicStream(w, strings.NewReader(stream), "glm-5.2"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := w.buf.String()
	for _, want := range []string{
		`"content_block":{"id":"call_abc","index":0,"input":{},"name":"get_weather","type":"tool_use"}`,
		`"partial_json":"{\"city\":","type":"input_json_delta"`,
		`"partial_json":"\"北京\"}","type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		`"output_tokens":8`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestAnthropicStreamErrorFrame(t *testing.T) {
	w := &testResponseWriter{header: http.Header{}}
	stream := sse(chunkText1, `{"error":{"code":6004,"message":"rate limited"}}`)
	err := WriteAnthropicStream(w, strings.NewReader(stream), "glm-5.2")
	if err == nil {
		t.Fatal("error frame must surface as error")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should carry upstream message: %v", err)
	}
	if !strings.Contains(w.buf.String(), `"type":"api_error"`) || !strings.Contains(w.buf.String(), "rate limited") {
		t.Errorf("error event not emitted: %s", w.buf.String())
	}
}

func TestResponsesFinalFromOpenAI(t *testing.T) {
	agg := map[string]any{
		"id":      "chatcmpl-9",
		"model":   "glm-5.2",
		"created": float64(1753600000),
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":    "assistant",
				"content": "查询中",
				"tool_calls": []any{map[string]any{
					"id": "call_x", "type": "function",
					"function": map[string]any{"name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": float64(12), "completion_tokens": float64(7), "total_tokens": float64(19)},
	}
	out := ResponsesFinalFromOpenAI(agg, "glm-5.2")
	if out["object"] != "response" || out["status"] != "completed" || out["model"] != "glm-5.2" {
		t.Fatalf("response envelope: %v", out)
	}
	output, _ := out["output"].([]any)
	if len(output) != 2 { // message + function_call
		t.Fatalf("output items: %d", len(output))
	}
	msg, _ := output[0].(map[string]any)
	if msg["type"] != "message" {
		t.Errorf("item0: %v", msg)
	}
	fc, _ := output[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_x" || fc["name"] != "get_weather" {
		t.Errorf("item1: %v", fc)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != int64(12) || usage["output_tokens"] != int64(7) {
		t.Errorf("usage: %v", usage)
	}
}

func TestResponsesRequestConversion(t *testing.T) {
	reqBody := `{
		"model": "cn:glm-5.2",
		"instructions": "你是助手",
		"input": [
			{"type": "message", "role": "user", "content": "天气如何"},
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "晴 30 度"}
		],
		"tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}],
		"stream": true
	}`
	req, chatBody, err := ParseResponsesRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !req.Stream {
		t.Error("stream flag lost")
	}
	var chat map[string]any
	_ = json.Unmarshal(chatBody, &chat)
	msgs, _ := chat["messages"].([]any)
	// system(instructions) / user / assistant(tool_call) / tool(output)
	if len(msgs) != 4 {
		t.Fatalf("messages: %d (%v)", len(msgs), msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "你是助手" {
		t.Errorf("instructions: %v", sys)
	}
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
		t.Errorf("function_call_output: %v", toolMsg)
	}
}

func TestResponsesPreviousResponseIDRejected(t *testing.T) {
	if _, _, err := ParseResponsesRequest([]byte(`{"model":"m","input":"hi","previous_response_id":"resp_1"}`)); err == nil {
		t.Fatal("previous_response_id must be rejected (stateless gateway)")
	}
}

func TestResponsesStreamEvents(t *testing.T) {
	w := &testResponseWriter{header: http.Header{}}
	stream := sse(chunkText1, chunkText2, chunkFinish)
	if err := WriteResponsesStream(w, strings.NewReader(stream), "glm-5.2"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := w.buf.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.content_part.added",
		`"delta":"你好"`,
		`"delta":"，世界"`,
		"event: response.content_part.done",
		"event: response.output_item.done",
		`"type":"response.completed"`,
		`"input_tokens":10`,
		`"output_tokens":5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestResponsesStreamToolEvents(t *testing.T) {
	w := &testResponseWriter{header: http.Header{}}
	stream := sse(chunkTool, chunkTool2, chunkFinishTool, chunkUsage)
	if err := WriteResponsesStream(w, strings.NewReader(stream), "glm-5.2"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := w.buf.String()
	for _, want := range []string{
		`"type":"function_call"`,
		`"call_id":"call_abc"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"arguments":"{\"city\":\"北京\"}"`,
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// testResponseWriter httptest 不可用的轻量 writer（Flush 兼容）。
func httpHeaderNew() http.Header { return http.Header{} }

// testResponseWriter httptest 不可用的轻量 writer（Flush 兼容）。
type testResponseWriter struct {
	header http.Header
	buf    strings.Builder
}

func (w *testResponseWriter) Header() http.Header         { return w.header }
func (w *testResponseWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *testResponseWriter) WriteHeader(status int)      {}
func (w *testResponseWriter) Flush()                      {}

// failWriter 所有写入都失败的 ResponseWriter（模拟客户端断连）。
type failWriter struct{ header http.Header }

func (w *failWriter) Header() http.Header         { return w.header }
func (w *failWriter) Write(b []byte) (int, error) { return 0, errors.New("broken pipe") }
func (w *failWriter) WriteHeader(status int)      {}
func (w *failWriter) Flush()                      {}

func TestAnthropicStreamWriteFailureReturnsError(t *testing.T) {
	// 客户端断连（写失败）必须以错误返回（调用方不记成功），与 OpenAI 路径同语义。
	w := &failWriter{header: http.Header{}}
	err := WriteAnthropicStream(w, strings.NewReader(sse(chunkText1, chunkFinish)), "glm-5.2")
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("write failure must surface as error, got %v", err)
	}
	err = WriteResponsesStream(w, strings.NewReader(sse(chunkText1, chunkFinish)), "glm-5.2")
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("responses write failure must surface as error, got %v", err)
	}
}

func TestResponsesLengthMeansIncomplete(t *testing.T) {
	agg := map[string]any{
		"id":      "chatcmpl-1",
		"model":   "glm-5.2",
		"created": float64(1753600000),
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "length",
			"message": map[string]any{"role": "assistant", "content": "截断的回答"},
		}},
	}
	out := ResponsesFinalFromOpenAI(agg, "glm-5.2")
	if out["status"] != "incomplete" {
		t.Fatalf("status: %v", out["status"])
	}
	details, _ := out["incomplete_details"].(map[string]any)
	if details == nil || details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details: %v", out["incomplete_details"])
	}
	// 流式路径同样识别 length。
	w := &testResponseWriter{header: httpHeaderNew()}
	stream := sse(`{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"部分"}}],"usage":null}`,
		`{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`)
	if err := WriteResponsesStream(w, strings.NewReader(stream), "glm-5.2"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.Contains(w.buf.String(), `"status":"incomplete"`) {
		t.Errorf("stream completed event should carry incomplete status:\n%s", w.buf.String())
	}
}

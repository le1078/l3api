// trae_chat_test.go Trae chat SSE 桥的请求体构造与事件翻译测试。
package upstream

import (
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
	// 正文已经开始后到达的 error 帧：翻译为 OpenAI error 帧透传 + [DONE] 收尾，
	// 不返回 Read 错误（客户端能看到真实上游错误文案）。
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
	if err != nil {
		t.Fatal(err)
	}
	all := string(tr.pending) + string(raw)
	if !strings.Contains(all, `"error"`) || !strings.Contains(all, "all models failed") {
		t.Fatalf("late error not passthrough:\n%s", all)
	}
	if !strings.Contains(all, "data: [DONE]") {
		t.Fatalf("missing [DONE]:\n%s", all)
	}
	if tr.lateErr == nil || tr.lateErr.Kind != ErrClient {
		t.Fatalf("lateErr not recorded: %+v", tr.lateErr)
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

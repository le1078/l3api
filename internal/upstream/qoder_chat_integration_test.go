package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

// newQoderTestAuth 构造一个 brand=qoder、不过期的测试账号（uid/jobToken 固定）。
func newQoderTestAuth() *auth.Auth {
	a := &auth.Auth{
		UID:          "u-test",
		Nickname:     "t",
		AccessToken:  "jt-test",
		RefreshToken: "jrt-test",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	_, _ = auth.BackfillBrandFor(a, "qoder")
	return a
}

// TestQoderChatStream_EndToEnd 完整链路：模拟上游校验签名/头族/body 编码，
// 返回真实形态的 SSE 信封，断言网关解包出 OpenAI 帧流。
func TestQoderChatStream_EndToEnd(t *testing.T) {
	var gotAuth, gotCosyKey, gotCosyDate, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCosyKey = r.Header.Get("Cosy-Key")
		gotCosyDate = r.Header.Get("Cosy-Date")
		gotBody, _ = io.ReadAll(r.Body)
		// 校验关键头族存在（抓包形态）。
		for _, k := range []string{"Cosy-MachineId", "Cosy-Version", "X-Model-Key", "Login-Version"} {
			if r.Header.Get(k) == "" {
				t.Errorf("missing header %s", k)
			}
		}
		// 校验 Authorization 形态。
		if !strings.HasPrefix(gotAuth, "Bearer COSY.") {
			t.Errorf("Authorization = %q", gotAuth[:min(30, len(gotAuth))])
		}
		// 回两帧真实形态的信封。
		w.Header().Set("Content-Type", "text/event-stream")
		inner := `{"choices":[{"delta":{"content":"Hi","reasoning_content":""},"index":0}],"id":"chatcmpl-t"}`
		fmt.Fprintf(w, "data:{\"body\":%s,\"statusCodeValue\":200}\n\n", jsonStr(inner))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New()
	a := newQoderTestAuth()
	// 用测试服务器地址覆盖 chat base（路径仍走真实 qoderChatPath，服务端按该路径断言）。
	// 这里用完整 URL 替换 base：QoderChatStream 内部用 qoderChatURL 常量，无法注入，
	// 故通过自定义 Transport 把 host 重写到测试服务器（不改代码注入点，保持生产路径真实）。
	c.ChatHTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	c.HTTP = c.ChatHTTP

	rc, status, _, err := c.QoderChatStream(context.Background(), a, []byte(
		`{"messages":[{"role":"user","content":"你好"}]}`), "conv-e2e", "qfmodel")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	out, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"content":"Hi"`) {
		t.Fatalf("inner content lost:\n%s", got)
	}
	if !strings.Contains(got, "data: [DONE]\n\n") {
		t.Fatalf("missing DONE:\n%s", got)
	}
	// 签名公式在服务端可复算：payload_b64 是 base64(JSON{...info...})，
	// 用同一 cosy_key/cosy_date/body/path 重算应一致（验证我们发出的签名与公式一致）。
	rest := strings.TrimPrefix(gotAuth, "Bearer COSY.")
	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 {
		t.Fatalf("no sig separator in %q", rest)
	}
	payload, sig := rest[:dot], rest[dot+1:]
	if len(sig) != 32 {
		t.Fatalf("sig len = %d", len(sig))
	}
	// 服务端侧复算：用同一公式应得同一 sig（证明我们发的签名能被对端复现）。
	want := QoderSign(payload, gotCosyKey, gotCosyDate, string(gotBody), QoderSigPath("https://gateway.qoder.com.cn"+gotPath))
	if sig != want {
		t.Fatalf("signature not reproducible: got %s want %s", sig, want)
	}
	// body 是 Encode=1 编码：能解出 messages。
	dec, err := QoderDecode(string(gotBody))
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	var payloadObj map[string]any
	if err := json.Unmarshal(dec, &payloadObj); err != nil {
		t.Fatalf("unmarshal decoded body: %v", err)
	}
	if payloadObj["agent_id"] != "agent_common" {
		t.Fatalf("agent_id = %v", payloadObj["agent_id"])
	}
	// payload_b64 解码出 identity（info 是 base64(AES(...))，无法本地解，但形态可校验）。
	payloadJSON, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode payload b64: %v", err)
	}
	var pj map[string]any
	if err := json.Unmarshal(payloadJSON, &pj); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if pj["version"] != "v1" || pj["cosyVersion"] != qoderCosyVersion {
		t.Fatalf("payload fields wrong: %v", pj)
	}
	if _, ok := pj["info"].(string); !ok {
		t.Fatalf("payload missing info: %v", pj)
	}
}

// TestQoderChatStream_AuthError HTTP 401 → *Error ErrSessionDead（handler 据此禁用账号）。
func TestQoderChatStream_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"code":"105","message":"Login expired"}`)
	}))
	defer srv.Close()
	c := New()
	c.ChatHTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	c.HTTP = c.ChatHTTP
	a := newQoderTestAuth()
	_, status, _, err := c.QoderChatStream(context.Background(), a, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), "", "")
	if status != 401 {
		t.Fatalf("status = %d", status)
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type %T, want *Error", err)
	}
	if ue.Kind != ErrSessionDead {
		t.Fatalf("kind = %v, want ErrSessionDead", ue.Kind)
	}
}

// TestQoderChatStream_TransportError 连接被拒 → 裸 error（非 *Error，handler 走抖动换号）。
func TestQoderChatStream_TransportError(t *testing.T) {
	c := New()
	c.ChatHTTP = &http.Client{Transport: rewriteHost("http://127.0.0.1:1")} // 拒绝连接
	c.HTTP = c.ChatHTTP
	a := newQoderTestAuth()
	_, _, _, err := c.QoderChatStream(context.Background(), a, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), "", "")
	if err == nil {
		t.Fatal("expect error")
	}
	if _, ok := err.(*Error); ok {
		t.Fatalf("transport error should NOT be *Error, got %T", err)
	}
}

// TestQoderChatStream_RefreshOnExpiry jobToken 过期时先滚动刷新再发请求。
func TestQoderChatStream_RefreshOnExpiry(t *testing.T) {
	refreshHit := false
	chatHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/refresh":
			refreshHit = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"jt-new","refresh_token":"jrt-new","expires_at":"`+
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+
				`","refresh_token_expires_at":"`+time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339)+`"}`)
		case qoderChatPath:
			chatHit = true
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New()
	// 同时覆盖 HTTP（refresh 用 c.HTTP）与 ChatHTTP（chat 用 c.ChatHTTP）。
	tr := rewriteHost(srv.URL)
	c.HTTP = &http.Client{Transport: tr}
	c.ChatHTTP = c.HTTP

	a := newQoderTestAuth()
	a.ExpiresAt = time.Now().Add(-time.Minute).Unix() // 强制过期
	rc, status, _, err := c.QoderChatStream(context.Background(), a, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), "", "")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	rc.Close()
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if !refreshHit || !chatHit {
		t.Fatalf("refresh=%v chat=%v", refreshHit, chatHit)
	}
	if a.AccessToken != "jt-new" {
		t.Fatalf("token not rolled: %q", a.AccessToken)
	}
}

// rewriteHost 把所有请求重写到指定 host（测试用：不改生产代码的 URL 常量）。
type hostRewrite struct{ base string }

func (h hostRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme = "http"
	u.Host = strings.TrimPrefix(h.base, "http://")
	u.Host = strings.TrimPrefix(u.Host, "https://")
	u.Host = strings.TrimSuffix(u.Host, "/")
	req = req.Clone(req.Context())
	req.URL = &u
	req.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func rewriteHost(base string) http.RoundTripper { return hostRewrite{base: base} }

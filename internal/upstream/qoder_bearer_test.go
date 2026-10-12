package upstream

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// qoderChatSample testdata/qoder_chat_sample.json 的载体。
type qoderChatSample struct {
	URL        string `json:"url"`
	SigPath    string `json:"sig_path"`
	PayloadB64 string `json:"payload_b64"`
	CosyKey    string `json:"cosy_key"`
	CosyDate   string `json:"cosy_date"`
	Body       string `json:"body"`
	ExpectSig  string `json:"expect_sig"`
}

func loadQoderChatSample(t *testing.T) qoderChatSample {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "qoder_chat_sample.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var s qoderChatSample
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return s
}

// TestQoderSignRealCapture 用真实抓包（HTTP 200 的 chat 请求）回归签名公式。
// 这是整个 Qoder provider 的地基：只要这个测试挂了，说明上游改了签名算法。
func TestQoderSignRealCapture(t *testing.T) {
	s := loadQoderChatSample(t)

	if got := QoderSigPath(s.URL); got != s.SigPath {
		t.Fatalf("QoderSigPath = %q, want %q", got, s.SigPath)
	}
	got := QoderSign(s.PayloadB64, s.CosyKey, s.CosyDate, s.Body, s.SigPath)
	if got != s.ExpectSig {
		t.Fatalf("signature mismatch:\n got  %s\n want %s", got, s.ExpectSig)
	}
}

// TestQoderSigPath 路径归一化：去 query、去前导 /algo。
func TestQoderSigPath(t *testing.T) {
	const base = "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation"
	cases := []struct{ in, want string }{
		{base + "?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
			"/api/v2/service/pro/sse/agent_chat_generation"},
		{base, "/api/v2/service/pro/sse/agent_chat_generation"},
		{"https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1", "/api/v2/model/list"},
		{"https://gateway.qoder.com.cn/api/v2/model/list", "/api/v2/model/list"},
	}
	for _, c := range cases {
		if got := QoderSigPath(c.in); got != c.want {
			t.Errorf("QoderSigPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestQoderCosyPayloadFieldOrder payload JSON 的键顺序必须与客户端一致。
// 顺序本身不影响 MD5，但服务端解包依赖它，所以钉死。
func TestQoderCosyPayloadFieldOrder(t *testing.T) {
	got, err := qoderBuildPayloadB64("INFO", "REQ", "1.1.57", "")
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	want := `{"version":"v1","requestId":"REQ","info":"INFO","cosyVersion":"1.1.57","ideVersion":""}`
	if string(raw) != want {
		t.Fatalf("payload JSON:\n got  %s\n want %s", raw, want)
	}
}

// TestQoderIdentitySizeMatchesCapture identity 尺寸必须与抓包吻合。
// 这是「identity 是 2 字段而非 9 字段」的证据链，防止有人照抄参考实现改回 9 字段。
func TestQoderIdentitySizeMatchesCapture(t *testing.T) {
	// 抓包实测值（uid + 27 字符 jobToken）。
	ident := qoderCosyIdentity{
		UID:                "01a0c4f2-091a-7c73-9a45-8ff200bdd9d0",
		SecurityOauthToken: "jt-lJSFGt2BamfbbzFTIGhsjbdJ",
	}
	raw, err := json.Marshal(ident)
	if err != nil {
		t.Fatalf("marshal identity: %v", err)
	}
	// 99 字节明文 → PKCS7 填充到 112 字节 → base64 152 字符，与抓包一致。
	if len(raw) != 99 {
		t.Fatalf("identity json len = %d, want 99", len(raw))
	}
	ct := (len(raw)/16 + 1) * 16
	if ct != 112 {
		t.Fatalf("ciphertext len = %d, want 112", ct)
	}
	if n := len(base64.StdEncoding.EncodeToString(make([]byte, ct))); n != 152 {
		t.Fatalf("info b64 len = %d, want 152", n)
	}
}

// TestQoderTempKeyIsHex16 钉死 temp_key 的**字符集**：必须是 16 个 ASCII 十六进制
// 字符（0-9a-f），不能是 16 个任意随机字节。
//
// 背景（2026-09-23 定位的 code 101 "Signature invalid" 根因）：
// 客户端是 uuid4().hex[:16]。早前实现用 crypto/rand.Read 直接取 16 字节，
// 字节数同样是 16、AES/RSA 也能解开，identity→info→payload 逐步比对与参考实现
// 完全一致，签名公式也一致——但网关侧仍判 101。把 Go 原样重放参考实现生成的
// 请求（含其 Authorization）可以通过，换成自己生成就失败，两者唯一差异就是
// 这个字符集。改成 hex16 后活体请求立即通过（HTTP 200 + 真实 SSE）。
//
// 这个测试存在的意义：防止后人「优化」回 rand.Read 而重新踩坑。
func TestQoderTempKeyIsHex16(t *testing.T) {
	for i := 0; i < 200; i++ {
		key, err := qoderNewTempKey()
		if err != nil {
			t.Fatalf("qoderNewTempKey: %v", err)
		}
		if len(key) != 16 {
			t.Fatalf("temp key len = %d, want 16", len(key))
		}
		for _, c := range key {
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHex {
				t.Fatalf("temp key byte %q is not an ASCII hex char (full key: %q)", c, key)
			}
		}
	}
}

// TestQoderAESEncryptMatchesReference 与参考实现（Python/Node）逐字节对齐。
// 固定 temp_key 时 info 密文必须完全一致——这是「字符集差异之外、AES 参数也正确」
// 的证据。若哪天 codec 或填充方式被改坏，这里会先炸。
func TestQoderAESEncryptMatchesReference(t *testing.T) {
	// 参考实现的实测输入/输出（2026-09-23 dump_request.py 快照）。
	tempKey := []byte("16985022d1d5406e")
	ident := qoderCosyIdentity{
		UID:                "01a0c4f2-091a-7c73-9a45-8ff200bdd9d0",
		SecurityOauthToken: "jt-LqiCngMKQTac655AgrVy3I02",
	}
	want := "UmEie4M9kqRJHEWa4prRc/EH1AwBfn4/Wn/Qwj4WkiknrGtDq4Cm5q6amCCT8wC5QJlhN1wRCf7mVr5F4SXH/d0OewVZc9wsw4zIKJf77lkE3fIJxBxpMnP/SQ+mAtI6g1f7ni2gZXMjICmZbdaaEA=="

	identJSON, err := json.Marshal(ident)
	if err != nil {
		t.Fatalf("marshal identity: %v", err)
	}
	ct, err := qoderAESEncrypt(tempKey, identJSON)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	if got := base64.StdEncoding.EncodeToString(ct); got != want {
		t.Fatalf("info ciphertext mismatch:\n got  %s\n want %s", got, want)
	}
}

// TestQoderNewCosySession RSA/AES 产物形态与可解密性。
// RSA 是随机的无法比对，但 AES 是同一个 temp_key，可以解开验证。
func TestQoderNewCosySession(t *testing.T) {
	ident := qoderCosyIdentity{UID: "u-1", SecurityOauthToken: "jt-abc"}
	sess, err := QoderNewCosySession(ident)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if len(sess.TempKey) != 16 {
		t.Fatalf("temp key len = %d, want 16", len(sess.TempKey))
	}
	// RSA-1024 密文 128 字节 → base64 172 字符（抓包实测同长度）。
	keyRaw, err := base64.StdEncoding.DecodeString(sess.CosyKey)
	if err != nil {
		t.Fatalf("decode cosy key: %v", err)
	}
	if len(keyRaw) != 128 {
		t.Fatalf("cosy key bytes = %d, want 128 (b64 %d chars)", len(keyRaw), len(sess.CosyKey))
	}
	infoRaw, err := base64.StdEncoding.DecodeString(sess.Info)
	if err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if len(infoRaw)%16 != 0 {
		t.Fatalf("info ciphertext %d bytes, not 16-aligned", len(infoRaw))
	}

	// 用同一个 temp_key 解回来，确认 AES 参数（key=iv、PKCS7）与客户端一致。
	block, err := aes.NewCipher(sess.TempKey)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	plain := make([]byte, len(infoRaw))
	cipher.NewCBCDecrypter(block, sess.TempKey).CryptBlocks(plain, infoRaw)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > 16 {
		t.Fatalf("bad PKCS7 pad byte %d", pad)
	}
	plain = plain[:len(plain)-pad]
	want, _ := json.Marshal(ident)
	if string(plain) != string(want) {
		t.Fatalf("decrypted identity = %s, want %s", plain, want)
	}
}

// TestQoderBuildBearer 端到端构造出的 Authorization 能被同一套公式验证通过。
func TestQoderBuildBearer(t *testing.T) {
	ident := qoderCosyIdentity{UID: "u-1", SecurityOauthToken: "jt-abc"}
	const rawURL = "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?Encode=1"
	body := "ENCODED-BODY"

	bearer, cosyKey, cosyDate, err := QoderBuildBearer(ident, rawURL, body, "req-1")
	if err != nil {
		t.Fatalf("build bearer: %v", err)
	}
	if !strings.HasPrefix(bearer, "Bearer COSY.") {
		t.Fatalf("bearer prefix wrong: %q", bearer[:20])
	}
	rest := strings.TrimPrefix(bearer, "Bearer COSY.")
	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 {
		t.Fatalf("bearer missing sig separator: %q", rest)
	}
	payload, sig := rest[:dot], rest[dot+1:]
	if len(sig) != 32 {
		t.Fatalf("sig len = %d, want 32", len(sig))
	}
	// 用签名公式自校验：换任何一项都应失败。
	if want := QoderSign(payload, cosyKey, cosyDate, body, QoderSigPath(rawURL)); want != sig {
		t.Fatalf("sig mismatch: got %s want %s", sig, want)
	}
	if QoderSign(payload, cosyKey, cosyDate, "OTHER-BODY", QoderSigPath(rawURL)) == sig {
		t.Fatal("signature must depend on body")
	}
	if QoderSign(payload, cosyKey, cosyDate, body, "/algo"+QoderSigPath(rawURL)) == sig {
		t.Fatal("signature must depend on path (no /algo prefix)")
	}
}

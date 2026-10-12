package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

// TestQoderE2ELive 真实活体端到端：PAT → jobToken → 多轮 chat → 解析 SSE。
//
// 单元测试（httptest + 夹具）无法覆盖的部分由它守门：真实凭证的
// jobToken 滚动、真实网关对 temp_key 字符集的校验、多轮请求的会话复用。
//
// **默认跳过**——需要真实网络与真实 PAT，不适合进 CI。开启方式：
//
//	L3API_QODER_LIVE=1 L3API_QODER_PAT=pt-xxx go test -run TestQoderE2ELive ./internal/upstream/
//
// PAT 一律从环境变量取，测试代码里不留任何凭证。
func TestQoderE2ELive(t *testing.T) {
	pat := os.Getenv("L3API_QODER_PAT")
	if os.Getenv("L3API_QODER_LIVE") != "1" || pat == "" {
		t.Skip("活体测试默认跳过：需设置 L3API_QODER_LIVE=1 与 L3API_QODER_PAT")
	}
	c := New()

	// 1. PAT → jobToken（真实兑换）
	req, _ := http.NewRequest("POST", "https://openapi.qoder.com.cn/api/v1/jobToken/exchange",
		strings.NewReader(`{"personal_token":"`+pat+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	var jt struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jt); err != nil {
		t.Fatalf("decode jobToken: %v", err)
	}
	resp.Body.Close()
	if jt.Token == "" {
		t.Fatal("no jobToken in response")
	}
	t.Logf("jobToken: %s… (len %d)", jt.Token[:10], len(jt.Token))

	// 2. userinfo → uid
	uiReq, _ := http.NewRequest("GET", "https://openapi.qoder.com.cn/api/v1/userinfo", nil)
	uiReq.Header.Set("Authorization", "Bearer "+jt.Token)
	uiResp, err := c.HTTP.Do(uiReq)
	if err != nil {
		t.Fatalf("userinfo: %v", err)
	}
	var uinfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(uiResp.Body).Decode(&uinfo); err != nil {
		t.Fatalf("decode userinfo: %v", err)
	}
	uiResp.Body.Close()
	if uinfo.ID == "" {
		t.Fatal("no uid in userinfo")
	}
	t.Logf("uid: %s | name: %s", uinfo.ID, uinfo.Name)

	// 3. 构造 auth（模拟池子里的凭证）
	a := &auth.Auth{
		UID:          uinfo.ID,
		Nickname:     uinfo.Name,
		AccessToken:  jt.Token,
		RefreshToken: jt.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}

	// 4. 两轮 chat，验证 jobToken 复用与 SSE 解包
	for round, text := range []string{"say OK only", "and now say YES only"} {
		body := []byte(`{"messages":[{"role":"user","content":"` + text + `"}]}`)
		rc, status, _, err := c.QoderChatStream(context.Background(), a, body, "e2e-live", "qfmodel")
		if err != nil {
			t.Fatalf("round %d chat: %v", round, err)
		}
		if status != http.StatusOK {
			rc.Close()
			t.Fatalf("round %d status = %d, want 200", round, status)
		}

		out, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("round %d read: %v", round, err)
		}

		// 必须没有 101 —— 这是本次攻破的核心回归点
		if strings.Contains(string(out), `"code":"101"`) || strings.Contains(string(out), `"code":101`) {
			t.Fatalf("round %d STILL code 101 Signature invalid:\n%s", round, out)
		}

		// 必须能解出 OpenAI 兼容帧（证明 SSE 信封解包器工作）
		var gotContent, gotReasoning strings.Builder
		var frames int
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue // 非 chat.completion.chunk 帧，跳过
			}
			frames++
			for _, ch := range chunk.Choices {
				gotContent.WriteString(ch.Delta.Content)
				gotReasoning.WriteString(ch.Delta.ReasoningContent)
			}
		}
		if frames == 0 {
			t.Fatalf("round %d: no OpenAI chunk frames decoded from %d bytes:\n%s",
				round, len(out), truncate(string(out), 500))
		}
		t.Logf("round %d: %d frames | content=%q | reasoning=%d chars",
			round, frames, gotContent.String(), gotReasoning.Len())
	}

	t.Log("PASS: 端到端两轮对话全通，无 code 101")
}

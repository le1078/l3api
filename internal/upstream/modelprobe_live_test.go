// modelprobe_live_test.go 三品牌官方模型目录 live 探测（人工触发，非常规测试）。
//
// 用法：L3API_LIVE_PROBE=1 go test ./internal/upstream -run TestLiveProbeModelLists -v
// 读取 auths/ 目录真实账号，分别打：
//   - codebuddy（CN）：FetchModels（/console/enterprises/personal/models）
//   - qoder：GET /algo/api/v2/model/list?Encode=1（Bearer COSY 签名，同 chat 链路）
//   - trae：POST {TraeChatHost}/api/ide/v1/get_detail_param {"function":"chat_v3"}
//
// 输出原始 JSON 到 stdout，供与静态目录（qoder_models.go/trae_models.go）比对。
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

func probeAuths(t *testing.T, brand string) []*auth.Auth {
	t.Helper()
	all, err := auth.LoadDir("../../auths")
	if err != nil {
		t.Fatalf("load auths: %v", err)
	}
	var out []*auth.Auth
	for _, a := range all {
		if a.Brand() == brand {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no auth for brand %q", brand)
	}
	return out
}

func TestLiveProbeModelLists(t *testing.T) {
	if os.Getenv("L3API_LIVE_PROBE") != "1" {
		t.Skip("set L3API_LIVE_PROBE=1 to run live probe")
	}
	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ── codebuddy CN ──
	for _, a := range probeAuths(t, "codebuddy") {
		infos, err := c.FetchModels(a)
		if err != nil {
			fmt.Printf("CODEBUDDY %s ERROR: %v\n", a.UID, err)
			continue
		}
		for _, mi := range infos {
			fmt.Printf("CODEBUDDY %s MODEL id=%s name=%s vl=%v reasoning=%v maxIn=%d maxOut=%d tags=%v credits=%s\n",
				a.UID, mi.ID, mi.Name, mi.SupportsImages, mi.SupportsReasoning, mi.ContextWindow, mi.MaxTokens, mi.Tags, mi.Credits)
		}
		break // 一个号即可代表目录
	}

	// ── qoder model/list ──
	for _, a := range probeAuths(t, "qoder") {
		if a.NeedsRefresh(qoderRefreshSkew) && a.RefreshToken != "" {
			sess, err := c.qoderRefreshJobToken(ctx, a.RefreshToken)
			if err != nil {
				fmt.Printf("QODER %s refresh ERROR: %v\n", a.UID, err)
				continue
			}
			a.Lock()
			a.AccessToken = sess.JobToken
			a.RefreshToken = sess.RefreshToken
			a.ExpiresAt = sess.ExpiresAt.Unix()
			a.Unlock()
			_ = a.SaveAtomic()
		}
		listURL := "https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1"
		identity := qoderCosyIdentity{UID: a.UID, SecurityOauthToken: a.AccessToken}
		bearer, cosyKey, cosyDate, err := QoderBuildBearer(identity, listURL, "", qoderUUID())
		if err != nil {
			t.Fatalf("qoder bearer: %v", err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
		qoderSetChatHeaders(req, bearer, cosyKey, cosyDate, a.UID, "", a.UID)
		req.Header.Del("Accept")
		req.Header.Del("X-Model-Key")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			fmt.Printf("QODER %s ERROR: %v\n", a.UID, err)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		fmt.Printf("QODER %s STATUS %d RAWLEN %d\nQODER_RAW_BEGIN\n%s\nQODER_RAW_END\n", a.UID, resp.StatusCode, len(raw), raw)
		if dec, derr := QoderDecode(string(raw)); derr == nil {
			fmt.Printf("QODER %s DECODED:\n%s\n", a.UID, dec)
		}
		break
	}

	// ── trae get_detail_param ──
	for _, a := range probeAuths(t, "trae") {
		jwt, err := c.traeJWTFor(ctx, a)
		if err != nil {
			fmt.Printf("TRAE %s jwt ERROR: %v\n", a.UID, err)
			continue
		}
		a.Lock()
		uid := a.UID
		deviceID := a.DeviceToken
		a.Unlock()
		if deviceID == "" {
			deviceID = TraeDeviceIDFor(uid)
		}
		body := []byte(`{"function":"chat_v3"}`)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			TraeChatHost+"/api/ide/v1/get_detail_param", bytes.NewReader(body))
		h := req.Header
		h.Set("Content-Type", "application/json")
		h.Set("Authorization", "Cloud-IDE-JWT "+jwt)
		h.Set("X-Cloudide-Token", jwt)
		h.Set("x-app-id", traeAppID)
		h.Set("x-app-version", "default")
		h.Set("x-ide-version", traeIdeVersion)
		h.Set("x-ide-version-code", traeIdeVerCode)
		h.Set("x-ide-version-type", "stable")
		h.Set("x-device-id", deviceID)
		h.Set("x-machine-id", deviceID)
		h.Set("x-device-brand", "Windows")
		h.Set("x-device-cpu", "x64")
		h.Set("x-device-type", "desktop")
		h.Set("x-os-version", "10.0.26200")
		h.Set("x-uid", uid)
		h.Set("request-traffic-type", "prod")
		h.Set("User-Agent", traeUA)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			fmt.Printf("TRAE %s ERROR: %v\n", a.UID, err)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		fmt.Printf("TRAE %s STATUS %d LEN %d\n%s\n", a.UID, resp.StatusCode, len(raw), raw)
		break
	}
}

// TestLiveProbeTraeChat 对候选新模型逐一实测 chat（200 + 首个实质 output 即通过）。
func TestLiveProbeTraeChat(t *testing.T) {
	if os.Getenv("L3API_LIVE_PROBE") != "1" {
		t.Skip("set L3API_LIVE_PROBE=1 to run live probe")
	}
	models := []string{
		"Doubao-Seed-Evolving", "mimo-v2.6-flash", "mimo-v2.6-pro",
		"qwen-3.7-plus", "DeepSeek-V4-Pro-Official", "DeepSeek-V4-Flash-Official",
	}
	c := New()
	for _, a := range probeAuths(t, "trae") {
		for _, m := range models {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			body := []byte(`{"model":"` + m + `","messages":[{"role":"user","content":"ping, reply with one word"}],"stream":true}`)
			rc, status, raw, err := c.TraeChatStream(ctx, a, body, "", m)
			if err != nil {
				fmt.Printf("TRAECHAT %-26s status=%d err=%v body=%.150s\n", m, status, err, raw)
				cancel()
				continue
			}
			out, _ := io.ReadAll(io.LimitReader(rc, 8192))
			rc.Close()
			cancel()
			fmt.Printf("TRAECHAT %-26s status=%d OK out=%.120s\n", m, status, out)
		}
		break
	}
}

// TestLiveModelSyncRound 端到端验证 SyncQoderModels/SyncTraeModels：
// 覆盖文件指向临时路径，目录刚按官方更新过 → 预期 diff 全空、不落盘。
func TestLiveModelSyncRound(t *testing.T) {
	if os.Getenv("L3API_LIVE_PROBE") != "1" {
		t.Skip("set L3API_LIVE_PROBE=1 to run live probe")
	}
	resetModelSyncForTest()
	defer resetModelSyncForTest()
	SetModelSyncPath(filepath.Join(t.TempDir(), "model_sync.json"))

	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if a := probeAuths(t, "qoder")[0]; a != nil {
		d, err := c.SyncQoderModels(ctx, a)
		fmt.Printf("SYNC qoder diff=%+v err=%v\n", d, err)
		if err != nil {
			t.Errorf("qoder sync: %v", err)
		} else if !d.empty() {
			t.Errorf("qoder diff should be empty: %+v", d)
		}
	}
	if a := probeAuths(t, "trae")[0]; a != nil {
		// 第一轮：种子与官方口径的固定字段偏差会作为 Changed 上报一次
		// （仅告警不入目录，随后并入 model_sync.json 的已告警集合）。
		d, err := c.SyncTraeModels(ctx, a)
		fmt.Printf("SYNC trae round1 diff=%+v err=%v\n", d, err)
		if err != nil {
			t.Errorf("trae sync: %v", err)
		}
		// 第二轮：稳态必须全空（Added/Removed 为零；Changed 已知化去重）。
		d, err = c.SyncTraeModels(ctx, a)
		fmt.Printf("SYNC trae round2 diff=%+v err=%v\n", d, err)
		if err != nil {
			t.Errorf("trae sync round2: %v", err)
		} else if !d.empty() || len(d.Rejected) > 0 {
			t.Errorf("trae round2 diff should be empty: %+v", d)
		}
	}
}

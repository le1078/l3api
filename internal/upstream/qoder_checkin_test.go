package upstream

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkinServer 模拟 Qoder 活动相关两个端点。campaignsStatus / campaignsBody 控制主响应。
// 无指纹场景下不会发起 claim，故此处只覆盖监测路径。
func checkinServer(t *testing.T, campaignsStatus int, campaignsBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sash/api/v1/me/campaigns":
			if r.Method != http.MethodGet {
				t.Errorf("campaigns method = %s, want GET", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer jt-test" {
				t.Errorf("campaigns Authorization = %q, want Bearer jt-test", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(campaignsStatus)
			_, _ = w.Write([]byte(campaignsBody))
		case "/sash/api/v1/me/campaigns/client_launch_26/limited-number":
			// 辅助端点：best-effort，恒返回已分配（不影响主流程断言）。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"hasNumber":true,"number":62206260,"createdAt":"2026-09-21T17:09:58Z"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
}

// TestQoderDailyCheckinClaimable 无指纹 + 有可领活动 → ErrCheckinInfo 且 Msg 含领取深链
// （仅监测模式的降级行为：不能代领就如实给链接，不伪报已领）。
func TestQoderDailyCheckinClaimable(t *testing.T) {
	body := `{"uid":"u-test","showCampaign":true,"claimable":true,` +
		`"campaignUrl":"https://qoder.com.cn/campaign/daily100","campaigns":[]}`
	srv := checkinServer(t, 200, body)
	defer srv.Close()

	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	a := newQoderTestAuth()

	err := c.QoderDailyCheckin(a)
	if err == nil {
		t.Fatal("expect ErrCheckinInfo, got nil")
	}
	if !IsCheckinInfo(err) {
		t.Fatalf("IsCheckinInfo = false, want true (err=%v)", err)
	}
	if !strings.Contains(err.Error(), "https://qoder.com.cn/campaign/daily100") {
		t.Errorf("Msg missing claim URL: %v", err)
	}
}

// TestQoderDailyCheckinNone 无可用活动 → 幂等静默（nil）。
func TestQoderDailyCheckinNone(t *testing.T) {
	body := `{"uid":"u-test","showCampaign":false,"claimable":false,"campaignUrl":"","campaigns":[]}`
	srv := checkinServer(t, 200, body)
	defer srv.Close()

	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	a := newQoderTestAuth()

	if err := c.QoderDailyCheckin(a); err != nil {
		t.Fatalf("expect nil, got %v", err)
	}
}

// TestQoderDailyCheckinUnauth 401 → 真实错误（非 IsCheckinInfo / 非 IsAlreadyCheckin）。
func TestQoderDailyCheckinUnauth(t *testing.T) {
	srv := checkinServer(t, 401, `{"code":"105","message":"Login expired"}`)
	defer srv.Close()

	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	a := newQoderTestAuth()

	err := c.QoderDailyCheckin(a)
	if err == nil {
		t.Fatal("expect error")
	}
	if IsCheckinInfo(err) {
		t.Errorf("401 should not be IsCheckinInfo: %v", err)
	}
	if IsAlreadyCheckin(err) {
		// 401 文案不应命中"已签到"标记。
		t.Errorf("401 should not be IsAlreadyCheckin: %v", err)
	}
}

// TestDailyCheckinDispatchQoder DailyCheckin 按品牌分发到 QoderDailyCheckin。
func TestDailyCheckinDispatchQoder(t *testing.T) {
	body := `{"showCampaign":false,"claimable":false,"campaignUrl":"","campaigns":[]}`
	srv := checkinServer(t, 200, body)
	defer srv.Close()

	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}
	a := newQoderTestAuth() // brand = qoder

	// 不应走 codebuddy billingMeterJSON（无 /billing 调用、无错误）。
	if err := c.DailyCheckin(a); err != nil {
		t.Fatalf("qoder DailyCheckin should not error on empty campaign: %v", err)
	}
}

// writeFingerprintFile 写一份按 uid 索引的指纹文件，并把环境变量指向它。
func writeFingerprintFile(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "qoder_fingerprints.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write fingerprint file: %v", err)
	}
	t.Setenv(qoderFingerprintEnv, p)
}

// claimServer 模拟「带指纹」的完整领取链路：campaigns GET + claim POST + 限量号。
func claimServer(t *testing.T, campaignsBody, claimBody string,
	gotClaimPath, gotMachineToken *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/sash/api/v1/me/campaigns":
			if r.Method != http.MethodGet {
				t.Errorf("campaigns method = %s, want GET", r.Method)
			}
			*gotMachineToken = r.Header.Get("Cosy-MachineToken")
			_, _ = w.Write([]byte(campaignsBody))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			if r.Method != http.MethodPost {
				t.Errorf("claim method = %s, want POST", r.Method)
			}
			*gotClaimPath = r.URL.Path
			_, _ = w.Write([]byte(claimBody))
		case strings.HasSuffix(r.URL.Path, "/limited-number"):
			_, _ = w.Write([]byte(`{"hasNumber":true,"number":1}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
}

// TestQoderDailyCheckinClaimWithFingerprint 有指纹 + 有可领活动 → 真实 POST claim，
// 指纹头随请求发出，成功返回信息性结果（不触发冷却）。
func TestQoderDailyCheckinClaimWithFingerprint(t *testing.T) {
	writeFingerprintFile(t, `{"u-test":{"machineToken":"mt-1","machineType":"windows_x64",`+
		`"machineCode":"mc-1","machineOS":"win32","machineId":"mid-1","cosyVersion":"0.4.1"}}`)

	camps := `{"uid":"u-test","campaigns":[{"campaignId":"act-20260923-252",` +
		`"campaignKey":"act-20260923-252","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE"}]}`
	var claimPath, machineToken string
	srv := claimServer(t, camps, `{"status":"CLAIMED","replayed":false}`, &claimPath, &machineToken)
	defer srv.Close()

	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}

	err := c.QoderDailyCheckin(newQoderTestAuth())
	if err == nil {
		t.Fatal("expect info result, got nil")
	}
	if !IsCheckinInfo(err) {
		t.Fatalf("want IsCheckinInfo, got %v", err)
	}
	if claimPath != "/sash/api/v1/me/campaigns/act-20260923-252/claim" {
		t.Errorf("claim path = %q", claimPath)
	}
	if machineToken != "mt-1" {
		t.Errorf("Cosy-MachineToken = %q, want mt-1 (指纹未随请求发出)", machineToken)
	}
	if !strings.Contains(err.Error(), "领取成功") {
		t.Errorf("Msg should report success: %v", err)
	}
}

// TestQoderDailyCheckinClaimReplayed 幂等重放：已领过的活动再 claim → 仍算成功，
// 但文案标明「已领取」，调度器据此打 info 而非 error。
func TestQoderDailyCheckinClaimReplayed(t *testing.T) {
	writeFingerprintFile(t, `{"u-test":{"machineToken":"mt-1","machineCode":"mc-1"}}`)

	camps := `{"campaigns":[{"campaignId":"act-1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE"}]}`
	var claimPath, machineToken string
	srv := claimServer(t, camps, `{"status":"CLAIMED","replayed":true}`, &claimPath, &machineToken)
	defer srv.Close()

	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}

	err := c.QoderDailyCheckin(newQoderTestAuth())
	if !IsCheckinInfo(err) {
		t.Fatalf("want IsCheckinInfo, got %v", err)
	}
	if !strings.Contains(err.Error(), "已领取") {
		t.Errorf("Msg should mark already claimed: %v", err)
	}
}

// TestQoderDailyCheckinAllClaimed 活动存在但已全部 CLAIMED → 不发起 claim，静默返回。
func TestQoderDailyCheckinAllClaimed(t *testing.T) {
	writeFingerprintFile(t, `{"u-test":{"machineToken":"mt-1","machineCode":"mc-1"}}`)

	camps := `{"campaigns":[{"campaignId":"act-1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED"}]}`
	var claimPath, machineToken string
	srv := claimServer(t, camps, `{}`, &claimPath, &machineToken)
	defer srv.Close()

	c := New()
	c.HTTP = &http.Client{Transport: rewriteHost(srv.URL)}

	if err := c.QoderDailyCheckin(newQoderTestAuth()); err != nil {
		t.Fatalf("all-CLAIMED should be silent nil, got %v", err)
	}
	if claimPath != "" {
		t.Errorf("should not claim when all CLAIMED, got %q", claimPath)
	}
}

// TestQoderSafeCampaignID 活动 ID 白名单校验（防拼进 URL 时的路径穿越）。
func TestQoderSafeCampaignID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"act-20260923-252", true},
		{"abc_XYZ_019", true},
		{"", false},
		{"../../evil", false},
		{"act/1", false},
		{"act 1", false},
	}
	for _, c := range cases {
		if got := qoderSafeCampaignID(c.in); got != c.want {
			t.Errorf("qoderSafeCampaignID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

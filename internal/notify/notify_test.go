// notify_test.go 投递器行为：订阅过滤、去重、各渠道 payload、非阻塞投递。
package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectServer 假渠道端点：记录收到的请求体，恒 200。
func collectServer(t *testing.T, mu *sync.Mutex, got *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*got = append(*got, string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
}

// waitUntil 轮询等待条件成立（worker 异步投递）。
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}

func TestDisabledOrNoChannelsNoop(t *testing.T) {
	n := New(Config{Enabled: false, Channels: []Channel{{Type: "generic", URL: "http://127.0.0.1:1/x"}}})
	defer n.Stop()
	n.Post(EventPoolExhausted, "", "t", "b") // 未启用：不入队不发送
	if len(n.queue) != 0 {
		t.Fatalf("disabled notifier must not enqueue: %d", len(n.queue))
	}
	n2 := New(Config{Enabled: true}) // 无渠道
	defer n2.Stop()
	n2.Post(EventPoolExhausted, "", "t", "b")
	if len(n2.queue) != 0 {
		t.Fatalf("no-channel notifier must not enqueue: %d", len(n2.queue))
	}
}

func TestEventSubscriptionFilter(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := collectServer(t, &mu, &got)
	defer srv.Close()
	n := New(Config{
		Enabled:  true,
		Events:   []string{EventPoolExhausted}, // 只订阅池耗尽
		Channels: []Channel{{Type: "generic", URL: srv.URL}},
	})
	defer n.Stop()
	n.Post(EventBreakerOpen, "u1", "t", "b") // 未订阅 → 丢弃
	n.Post(EventPoolExhausted, "", "t", "b") // 订阅 → 发送
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 })
}

func TestDedupWithinCooldown(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := collectServer(t, &mu, &got)
	defer srv.Close()
	n := New(Config{
		Enabled:  true,
		Channels: []Channel{{Type: "generic", URL: srv.URL}},
		Cooldown: "10m",
	})
	defer n.Stop()
	n.Post(EventPoolExhausted, "", "t1", "b1")
	n.Post(EventPoolExhausted, "", "t2", "b2")     // 同 subject 全局：去重
	n.Post(EventAccountDisabled, "u1", "t3", "b3") // 不同事件：放行
	n.Post(EventPoolExhausted, "u2", "t4", "b4")   // 同事件不同 subject：放行
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 3 })
	mu.Lock()
	defer mu.Unlock()
	for _, b := range got {
		if strings.Contains(b, "t2") {
			t.Errorf("dedup failed, second global post delivered: %s", b)
		}
	}
}

func TestGenericChannelPayload(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := collectServer(t, &mu, &got)
	defer srv.Close()
	n := New(Config{
		Enabled:  true,
		Channels: []Channel{{Type: "generic", URL: srv.URL}},
	})
	defer n.Stop()
	n.Post(EventBalanceLow, "u1", "标题", "正文")
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 })
	mu.Lock()
	defer mu.Unlock()
	var p map[string]any
	if err := json.Unmarshal([]byte(got[0]), &p); err != nil {
		t.Fatalf("payload not json: %s", got[0])
	}
	if p["event"] != EventBalanceLow || p["subject"] != "u1" || p["title"] != "标题" || p["body"] != "正文" {
		t.Errorf("unexpected payload: %v", p)
	}
}

func TestWecomAndTelegramPayload(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := collectServer(t, &mu, &got)
	defer srv.Close()
	n := New(Config{
		Enabled: true,
		Channels: []Channel{
			{Type: "wecom", URL: srv.URL},
			{Type: "telegram", URL: srv.URL, Secret: "123456"},
		},
	})
	defer n.Stop()
	n.Post(EventAccountDisabled, "u1", "禁用", "原因")
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 })
	mu.Lock()
	defer mu.Unlock()
	var wecom, tg map[string]any
	_ = json.Unmarshal([]byte(got[0]), &wecom)
	_ = json.Unmarshal([]byte(got[1]), &tg)
	if wecom["msgtype"] != "text" {
		t.Errorf("wecom payload: %v", wecom)
	}
	if tg["chat_id"] != "123456" {
		t.Errorf("telegram payload: %v", tg)
	}
}

func TestRetryOnFailure(t *testing.T) {
	attempts := 0
	var mu sync.Mutex
	failFirst := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		fail := failFirst
		failFirst = false // 第二次成功
		mu.Unlock()
		if fail {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	n := New(Config{
		Enabled:  true,
		Channels: []Channel{{Type: "generic", URL: srv.URL}},
	})
	defer n.Stop()
	n.Post(EventPoolExhausted, "", "t", "b")
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return attempts >= 2 })
}

func TestValidateRejectsUnknownEvent(t *testing.T) {
	if err := Validate(Config{Events: []string{"nope"}}); err == nil {
		t.Fatal("unknown event must be rejected")
	}
	if err := Validate(Config{Cooldown: "bogus"}); err == nil {
		t.Fatal("bogus cooldown must be rejected")
	}
	if err := Validate(Config{Enabled: true}); err == nil {
		t.Fatal("enabled without channels must be rejected")
	}
	if err := Validate(Config{}); err != nil {
		t.Fatalf("disabled empty config must pass: %v", err)
	}
}

func TestStoreHotSwap(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := collectServer(t, &mu, &got)
	defer srv.Close()
	n := New(Config{Enabled: true, Channels: []Channel{{Type: "generic", URL: srv.URL}}})
	defer n.Stop()
	// 热关：Post 后不再入队。
	_ = n.Store(Config{Enabled: false, Channels: []Channel{{Type: "generic", URL: srv.URL}}})
	n.Post(EventPoolExhausted, "", "t", "b")
	if len(n.queue) != 0 {
		t.Fatalf("after disable, post must not enqueue: %d", len(n.queue))
	}
}

func TestBuildMail(t *testing.T) {
	msg := notification{event: EventSessionDead, subject: "u1", title: "账号 Session 已过期", body: "账号 abc 过期"}
	m := buildMail("bot@qq.com", []string{"a@x.com", "b@y.com"}, msg)
	for _, want := range []string{"From: bot@qq.com", "To: a@x.com, b@y.com", "MIME-Version: 1.0", "text/plain"} {
		if !strings.Contains(m, want) {
			t.Fatalf("mail missing %q:\n%s", want, m)
		}
	}
	// 中文主题必须 RFC2047 编码（不含裸 UTF-8 主题行）
	if strings.Contains(m, "Subject: [l3api]") {
		t.Fatalf("subject not encoded: %s", m)
	}
	if !strings.Contains(m, "Subject: =?UTF-8?") {
		t.Fatalf("subject not B-encoded: %s", m)
	}
}

func TestStoreSmtpCleaning(t *testing.T) {
	n := New(Config{})
	defer n.Stop()
	// 缺 host / 缺收件人 / 缺 from 链 / 非法端口 / 非法 tls 模式 → 全部丢弃
	err := n.Store(Config{Enabled: true, Channels: []Channel{
		{Type: "smtp", SmtpTo: []string{"a@x.com"}},
		{Type: "smtp", SmtpHost: "smtp.qq.com"},
		{Type: "smtp", SmtpHost: "smtp.qq.com", SmtpTo: []string{"a@x.com"}},
		{Type: "smtp", SmtpHost: "smtp.qq.com", SmtpTo: []string{"a@x.com"}, SmtpUsername: "u@qq.com", SmtpPort: -1},
		{Type: "smtp", SmtpHost: "smtp.qq.com", SmtpTo: []string{"a@x.com"}, SmtpUsername: "u@qq.com", SmtpTLS: "tls"},
	}})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if len(n.cfg.Load().channels) != 0 {
		t.Fatalf("invalid smtp channels must be dropped, got %d", len(n.cfg.Load().channels))
	}
	// 合法渠道：收件人空项剔除后保留
	err = n.Store(Config{Enabled: true, Channels: []Channel{
		{Type: "smtp", SmtpHost: "smtp.qq.com", SmtpTo: []string{" a@x.com ", ""}, SmtpUsername: "u@qq.com"},
	}})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	chs := n.cfg.Load().channels
	if len(chs) != 1 || len(chs[0].SmtpTo) != 1 || chs[0].SmtpTo[0] != "a@x.com" {
		t.Fatalf("smtp to cleaning wrong: %+v", chs)
	}
}

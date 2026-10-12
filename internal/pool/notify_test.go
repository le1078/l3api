// notify_test.go pool 侧告警挂钩行为：禁用/熔断/积分耗尽事件外推与过滤。
package pool

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/notify"
)

// recorderPoster 收集 Post 调用（事件, subject）。
type recorderPoster struct {
	events []string
}

func (r *recorderPoster) Post(event, subject, title, body string) {
	r.events = append(r.events, event+"\x00"+subject)
}

func newNotifyPool(t *testing.T) (*Pool, *recorderPoster) {
	t.Helper()
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	rec := &recorderPoster{}
	p.SetNotifier(rec)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	return p, rec
}

func TestNotifyDisableAutoVsManual(t *testing.T) {
	p, rec := newNotifyPool(t)
	p.Disable("u1", "12153 session dead")
	if len(rec.events) != 1 || !strings.HasPrefix(rec.events[0], notify.EventAccountDisabled) {
		t.Fatalf("auto disable must notify: %v", rec.events)
	}
	p.ReviveDisabled("u1")
	rec.events = nil
	// 手动禁用（panel 路径）不告警。
	p.Disable("u1", "manual disable (panel)")
	if len(rec.events) != 0 {
		t.Fatalf("manual disable must not notify: %v", rec.events)
	}
	// 重复禁用（已禁用 → 禁用）不重复告警。
	p.ReviveDisabled("u1")
	p.Disable("u1", "12153 session dead")
	p.Disable("u1", "12153 session dead")
	if n := len(rec.events); n != 1 {
		t.Fatalf("first-migration only: got %d events", n)
	}
}

func TestNotifyCreditExhausted(t *testing.T) {
	p, rec := newNotifyPool(t)
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	if len(rec.events) != 1 || !strings.HasPrefix(rec.events[0], notify.EventCreditExhausted) {
		t.Fatalf("credit exhausted must notify: %v", rec.events)
	}
}

func TestNotifyNilPosterNoop(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetNotifier(nil)
	// nil poster：全部路径静默，不 panic。
	p.Disable("u1", "12153 session dead")
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	_ = time.Now
}

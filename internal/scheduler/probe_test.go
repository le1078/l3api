package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/pool"
	"github.com/linguo2625469/l3api/internal/upstream"
)

// capturePoster 记录 notify 投递（实现 notify.Poster，非阻塞语义由测试自身保证）。
type capturePoster struct {
	mu     sync.Mutex
	events []string
}

func (c *capturePoster) Post(event, subject, title, body string) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

// TestNextWakeProbeScheduled 定时体检接入小时制主循环：配 9 点 → 9 点含 taskProbe；
// 禁用后不再出现。
func TestNextWakeProbeScheduled(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{21}, ProbeHours: []int{9},
		TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, BlackcatDisabled: true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, scheduleTZ))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, scheduleTZ); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskProbe) {
		t.Errorf("kinds=%v want probe", kinds)
	}
	s.Reconfigure(nil, nil, nil, nil, nil, []int{9}, true, true, true, true, true, true)
	at, kinds = s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, scheduleTZ))
	if hasKind(kinds, taskProbe) {
		t.Errorf("kinds=%v want no probe after disable", kinds)
	}
}

// TestRunProbeNow 一轮定时体检：成功+失败混合 → probe_last.json 落盘、
// 失败聚合发一条 probe_failed 告警。
func TestRunProbeNow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "bad") {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "good1", Nickname: "好号", AccessToken: "at-good", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "bad1", Nickname: "坏号", AccessToken: "bad-token", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	poster := &capturePoster{}
	statePath := t.TempDir() + "/probe_last.json"
	s := New(Config{
		Pool: p, Upstream: up, Notifier: poster,
		ProbeHours: []int{9}, ProbeStatePath: statePath,
		TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, BlackcatDisabled: true, CheckinDisabled: true,
	})

	s.RunProbeNow(context.Background())

	st, err := LoadProbeState(statePath)
	if err != nil || st == nil {
		t.Fatalf("load probe state: st=%v err=%v", st, err)
	}
	if st.Total != 2 || st.OK != 1 {
		t.Errorf("state total=%d ok=%d want 2/1", st.Total, st.OK)
	}
	if st.RanAt == "" || len(st.Results) != 2 {
		t.Errorf("state ran_at=%q results=%d", st.RanAt, len(st.Results))
	}
	poster.mu.Lock()
	defer poster.mu.Unlock()
	if len(poster.events) != 1 || poster.events[0] != "probe_failed" {
		t.Errorf("notify events=%v want [probe_failed]", poster.events)
	}
}

// TestLoadProbeStateMissing 文件缺失/损坏 → nil, nil（面板显示「尚未执行」）。
func TestLoadProbeStateMissing(t *testing.T) {
	if st, err := LoadProbeState(t.TempDir() + "/nope.json"); st != nil || err != nil {
		t.Errorf("missing: st=%v err=%v want nil,nil", st, err)
	}
	bad := t.TempDir() + "/bad.json"
	if err := os.WriteFile(bad, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := LoadProbeState(bad); st != nil || err != nil {
		t.Errorf("corrupt: st=%v err=%v want nil,nil", st, err)
	}
	if st, err := LoadProbeState(""); st != nil || err != nil {
		t.Errorf("empty path: st=%v err=%v want nil,nil", st, err)
	}
}

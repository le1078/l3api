package upstream

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestMonitorBodyIdleCutoff 静默超过 idle（配小值）→ Read 返回错误（context canceled）。
func TestMonitorBodyIdleCutoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 模拟真实 resp.Body：阻塞直到请求 context 被 cancel 才返回 error。
	body := monitorBody(&ctxBoundReader{ctx: ctx}, 50*time.Millisecond, 0, cancel)
	defer body.Close()
	if _, err := body.Read(make([]byte, 16)); err == nil {
		t.Fatal("expect read error after idle cutoff")
	}
}

// TestMonitorBodyRenewsOnActivity 持续活跃（周期吐数据，总时长 > idle）→ 流不被掐。
func TestMonitorBodyRenewsOnActivity(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 每 30ms 吐 1 字节；idle=80ms。总读 10 字节耗时 300ms > idle：
	// 若空闲续命失效（绝对 deadline 语义）早在 80ms 就被掐断。
	body := monitorBody(nopCloserBody{Reader: periodicReader{period: 30 * time.Millisecond}}, 80*time.Millisecond, 0, cancel)
	defer body.Close()
	buf := make([]byte, 1)
	for i := 0; i < 10; i++ {
		if _, err := io.ReadFull(body, buf); err != nil {
			t.Fatalf("read %d: %v (active stream must not be cut)", i, err)
		}
	}
}

// TestMonitorBodyDisabledWhenIdleZero idle<=0 直接返回原底流。
func TestMonitorBodyDisabledWhenIdleZero(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := nopCloserBody{Reader: &oneByteReader{}}
	out := monitorBody(rc, 0, 0, cancel)
	if _, ok := out.(nopCloserBody); !ok {
		t.Fatalf("idle<=0 should return underlying body verbatim, got %T", out)
	}
	buf := make([]byte, 1)
	if _, err := out.Read(buf); err != nil || buf[0] != 'a' {
		t.Fatalf("read=%q err=%v", buf, err)
	}
}

// TestMonitorBodyCloseStopsGoroutine Close 停掉 goroutine，无泄漏（等效监控循环 + 短周期）。
func TestMonitorBodyCloseStopsGoroutine(t *testing.T) {
	m := &idleMonitoringBody{
		rc:       &ctxBoundReader{ctx: context.Background()},
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   func() {},
	}
	stopped := make(chan struct{})
	go func() {
		tk := time.NewTicker(idleTick(40 * time.Millisecond))
		defer tk.Stop()
		for {
			select {
			case <-m.stopCh:
				close(stopped)
				return
			case <-tk.C:
			}
		}
	}()
	m.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("goroutine not stopped after Close")
	}
}

// TestMonitorBodyTotalCutoff 流总时长硬顶（2026-10-20）：上游持续活跃
// （每 20ms 吐 1 字节，idle 续命有效）但 total=100ms → 到点 cancel，
// 底流 Read 感知 ctx 取消后返回错误（模拟真实 resp.Body 行为）。
// 场景：keepalive 拖流——空闲监控永不触发，靠 total 兜底释放在途名额。
func TestMonitorBodyTotalCutoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := monitorBody(&activeUntilCancel{ctx: ctx, period: 20 * time.Millisecond},
		200*time.Millisecond, // idle 远大于活跃周期 → 续命有效
		100*time.Millisecond, // total 硬顶
		cancel)
	defer body.Close()
	buf := make([]byte, 1)
	start := time.Now()
	var lastErr error
	for i := 0; i < 200; i++ {
		if _, err := body.Read(buf); err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Fatal("total timeout never fired (active stream must still be cut)")
	}
	if d := time.Since(start); d < 80*time.Millisecond || d > 300*time.Millisecond {
		t.Fatalf("cut at %v, want ~100ms (total)", d)
	}
}

// TestMonitorBodyTotalZeroDisables 只配 idle（total=0）→ 长流不被 total 掐
// （保旧行为：idle 续命有效的活跃流可无限持续）。
func TestMonitorBodyTotalZeroDisables(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := monitorBody(nopCloserBody{Reader: periodicReader{period: 10 * time.Millisecond}},
		100*time.Millisecond, 0, cancel)
	defer body.Close()
	buf := make([]byte, 1)
	// 活跃流总时长 600ms > idle：若 total 误启用（默认值泄漏）会在 100ms 掐掉。
	for i := 0; i < 60; i++ {
		if _, err := io.ReadFull(body, buf); err != nil {
			t.Fatalf("read %d: %v (total=0 must not cut active stream)", i, err)
		}
	}
}

// TestMonitorBodyDisabledWhenBothZero idle<=0 且 total<=0 → 原样返回底流。
func TestMonitorBodyDisabledWhenBothZero(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := nopCloserBody{Reader: &oneByteReader{}}
	out := monitorBody(rc, 0, 0, cancel)
	if _, ok := out.(nopCloserBody); !ok {
		t.Fatalf("idle<=0 && total<=0 should return underlying body verbatim, got %T", out)
	}
}

// —— helpers ——

// ctxBoundReader 模拟 net/http resp.Body：Read 阻塞在 ctx.Done() 上，ctx cancel 后返回错误。
type ctxBoundReader struct{ ctx context.Context }

func (r *ctxBoundReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *ctxBoundReader) Close() error { return nil }

type periodicReader struct{ period time.Duration }

func (r periodicReader) Read(p []byte) (int, error) {
	time.Sleep(r.period)
	p[0] = 'x'
	return 1, nil
}

// activeUntilCancel 活跃吐数据但监听 ctx：cancel 后 Read 返回 ctx.Err()
// （模拟 net/http resp.Body 在请求 ctx 取消时的真实行为——periodicReader
// 永不出错，测不出 cancel 生效）。
type activeUntilCancel struct {
	ctx    context.Context
	period time.Duration
}

func (r *activeUntilCancel) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-time.After(r.period):
	}
	p[0] = 'x'
	return 1, nil
}

func (r *activeUntilCancel) Close() error { return nil }

type oneByteReader struct{ done bool }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	p[0] = 'a'
	return 1, nil
}

type nopCloserBody struct{ io.Reader }

func (nopCloserBody) Close() error { return nil }

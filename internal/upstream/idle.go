// idle.go 聊天 SSE 流中空闲监控：活跃吐数据续命不掐，静默超过阈值才断流（释放租约）。
//
// 2026-10-20 追加流总时长硬顶：空闲监控只看「有没有字节到达」，上游每 N 分钟
// 吐 1 字节 keepalive 就能永不断流，无限期占用账号在途名额（max_in_flight=8，
// 几条这样的流就能把池子打满、正常请求全 503）。total>0 时从开流起计时，
// 到点 cancel——与空闲监控共用同一 cancel 与后台 goroutine。
package upstream

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleMonitoringBody 包在聊天 SSE body 外层：
// 每次读到底层数据（n>0）就刷新 lastRead；后台 goroutine 周期检查，
// 静默超过 idle 就 cancel 请求 context，中断阻塞中的 Read。
type idleMonitoringBody struct {
	rc       io.ReadCloser
	mu       sync.Mutex
	lastRead time.Time
	stopOnce sync.Once
	stopCh   chan struct{}
	cancel   context.CancelFunc
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	return n, err
}

// Close 停掉后台 goroutine、取消请求 context、关闭底流，保证无泄漏。
func (b *idleMonitoringBody) Close() error {
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.cancel()
	return b.rc.Close()
}

func (b *idleMonitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastRead)
}

// monitorBody 若 idle<=0 且 total<=0 直接返回原底流（禁监控）；
// 否则包上流中监控（idle>0 管静默、total>0 管总时长，二者至少其一启用）。
// 计时从返回 body 之后开始——首字节阶段由 Transport.ResponseHeaderTimeout
// 管，这里不抢跑。
//
// total<=0 表示不设总时长硬顶（长回答合法时长可达数分钟，默认由 idle 兜底）；
// 需要硬顶的上游路径可显式传值（见 MaxStreamTotalDuration）。
func monitorBody(rc io.ReadCloser, idle, total time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 && total <= 0 {
		return rc
	}
	b := &idleMonitoringBody{
		rc:       rc,
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   cancel,
	}
	// 后台 goroutine 两件事都做：空闲 tick 检查 + 总时长到点 cancel。
	// 总时长用 absolute deadline 而非「tick 累加」——避免 tick 漂移与
	// 「刚好在 tick 间隙超时」的边界争议。
	//
	// timer/ticker 在 goroutine 内创建并 defer Stop（2026-10-20 踩坑：放
	// monitorBody 里 defer 会在函数返回时就把它们停掉，goroutine 的 select
	// 永远等不到事件——idle 监控静默失效、测试挂死）。
	go func() {
		var deadline <-chan time.Time
		var tick <-chan time.Time
		if total > 0 {
			t := time.NewTimer(total)
			defer t.Stop()
			deadline = t.C
		}
		if idle > 0 {
			tk := time.NewTicker(idleTick(idle))
			defer tk.Stop()
			tick = tk.C
		}
		for {
			select {
			case <-b.stopCh:
				return
			case <-deadline:
				// 流总时长硬顶（2026-10-20）：上游 keepalive 拖流时空闲监控
				// 永不触发，靠这里强制收流，释放账号在途名额。
				cancel()
				return
			case <-tick:
				if b.idleFor() > idle {
					cancel()
					return
				}
			}
		}
	}()
	return b
}

// idleTick 返回监控周期：idle/4，钳在 [10ms, 1s]。小 idle 也能快速发现，大小值避免空转。
func idleTick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}

// refresh_singleflight_test.go 刷新单飞的并发语义（2026-10-20）。
package upstream

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRefreshAuthOnceConcurrent 同 key 并发调用：fn 只跑一次（leader），
// 其余全部等它完成后返回——qoder 四条刷新路径合并后面临的真实场景
//（同账号 8 并发同时进过期窗口）。
//
// 同步方式：8 个 goroutine 先用 WaitGroup barrier 齐步，再同时调
// RefreshAuthOnce——不用「轮询等 leader 出现」的猜时序写法（-race 下调度
// 慢，轮询会在 follower 尚未排队时就放行 leader，测出 2 次调用的假失败）。
func TestRefreshAuthOnceConcurrent(t *testing.T) {
	// 清理单飞组状态（测试间隔离）。
	flightGroup.mu.Lock()
	flightGroup.busy = make(map[string]chan struct{})
	flightGroup.mu.Unlock()

	var calls atomic.Int64
	release := make(chan struct{})
	const n = 8
	var start, done sync.WaitGroup
	start.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			start.Done()   // 报「已就位」
			start.Wait()   // 等所有 goroutine 就位后齐步
			RefreshAuthOnce("uid-1", func() {
				calls.Add(1)
				<-release // 按住 leader，确保 follower 都排队进来
			})
		}()
	}
	// 等齐步后稍等片刻，让 7 个 follower 都排到等待点上（它们要抢
	// flightGroup.mu 才看得到 busy 条目）。给足 200ms——纯本地锁竞争，
	// 远小于测试超时；这期间 leader 被 release 前不会返回。
	// 注意：不能靠 calls==1 判定「已排队」，follower 不调用 fn。
	time.Sleep(200 * time.Millisecond)
	if c := calls.Load(); c != 1 {
		t.Fatalf("leader calls = %d, want 1（并发应被单飞合并）", c)
	}
	close(release)
	done.Wait()
	if c := calls.Load(); c != 1 {
		t.Fatalf("after all done: calls = %d, want 1", c)
	}
	// 组应已清空。
	if c := refreshFlightCount(); c != 0 {
		t.Fatalf("flight group not drained: %d", c)
	}
}

// TestRefreshAuthOnceSequential 串行调用：每次都真跑（单飞不跨调用缓存）。
func TestRefreshAuthOnceSequential(t *testing.T) {
	flightGroup.mu.Lock()
	flightGroup.busy = make(map[string]chan struct{})
	flightGroup.mu.Unlock()

	var calls atomic.Int64
	for i := 0; i < 3; i++ {
		RefreshAuthOnce("uid-seq", func() { calls.Add(1) })
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("sequential calls = %d, want 3（单飞只合并并发，不缓存）", n)
	}
}

// TestRefreshAuthOnceEmptyKeyDirect key 空（无 uid）退化为直跑——不排队不合并。
func TestRefreshAuthOnceEmptyKeyDirect(t *testing.T) {
	var calls atomic.Int64
	done := make(chan struct{}, 2)
	go func() { RefreshAuthOnce("", func() { calls.Add(1); time.Sleep(50 * time.Millisecond) }); done <- struct{}{} }()
	go func() { RefreshAuthOnce("", func() { calls.Add(1); time.Sleep(50 * time.Millisecond) }); done <- struct{}{} }()
	<-done
	<-done
	if n := calls.Load(); n != 2 {
		t.Fatalf("empty key calls = %d, want 2（应各自直跑）", n)
	}
}

// TestRefreshAuthOncePanicDoesNotDeadlock leader panic 后：follower 必须被
// 放行、组必须清理（否则同 key 后续刷新永久卡死）。
func TestRefreshAuthOncePanicDoesNotDeadlock(t *testing.T) {
	flightGroup.mu.Lock()
	flightGroup.busy = make(map[string]chan struct{})
	flightGroup.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer func() {
			_ = recover() // 吃掉 leader 的 panic（Do 不传播 panic 给 follower）
			close(done)
		}()
		RefreshAuthOnce("uid-panic", func() { panic("boom") })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("leader panic 后 follower/清理逻辑卡死")
	}
	if n := refreshFlightCount(); n != 0 {
		t.Fatalf("panic 后 flight group 未清理: %d", n)
	}
	// 组已清理：同 key 可再次单飞（不卡死）。
	RefreshAuthOnce("uid-panic", func() {})
}

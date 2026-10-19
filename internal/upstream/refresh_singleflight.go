// refresh_singleflight.go 账号级 token 刷新单飞（2026-10-20）。
//
// 背景：qoder/trae 的刷新路径「NeedsRefresh 通过 → 直接刷新」没有互斥。
// 同一账号 8 个并发请求（max_in_flight=8）同时进过期窗口 → 8 次
// POST /api/v1/jobToken/refresh：上游是滚动刷新（旧的失效），后写覆盖先写，
// auth 内存字段被并发写；风控视角是同 uid 短时多次刷 token（异常行为）。
// codebuddy 路径有 NeedsRefresh + RefreshToken 两段式锁（client.go），
// qoder/trae 侧是缺口——本文件补齐。
//
// 用法：把「读 NeedsRefresh → 刷新 → 写回」整段包进 RefreshAuthOnce，
// 同 uid 并发调用只有 leader 真打上游，其余等待并复用其结果。
package upstream

import "sync"

// refreshFlight 单飞组：key（uid）→ 在途刷新。
type refreshFlight struct {
	mu   sync.Mutex
	busy map[string]chan struct{}
}

// flightGroup 进程级单飞组（qoder/trae 刷新共用；不同 uid 互不阻塞）。
var flightGroup = &refreshFlight{busy: make(map[string]chan struct{})}

// Do 执行 fn，同 key 并发调用只有第一个真跑 fn，其余阻塞等它完成后
// 直接返回（不重跑）。用于 token 刷新：leader 刷新成功写回 auth 后，
// follower 看到的就是新 token——无需各自再刷一次。
//
// 与 golang.org/x/sync/singleflight 的差异：零依赖（本仓库刻意不引外部
// 模块），且语义更窄（无结果返回、无 panic 传播）——刷新场景只需要
// 「别重复打上游」。
func (g *refreshFlight) Do(key string, fn func()) {
	g.mu.Lock()
	if ch, ok := g.busy[key]; ok {
		// follower：等 leader 完成
		g.mu.Unlock()
		<-ch
		return
	}
	ch := make(chan struct{})
	g.busy[key] = ch
	g.mu.Unlock()

	// leader：跑真刷新。panic 也必须在 defer 里放行 follower 并清理，
	// 否则同 key 后续刷新永久卡死。
	func() {
		defer func() {
			g.mu.Lock()
			delete(g.busy, key)
			g.mu.Unlock()
			close(ch)
		}()
		fn()
	}()
}

// RefreshAuthOnce 账号级刷新单飞：NeedsRefresh 判定与刷新动作整体包进
// 单飞组——并发请求只有 leader 打上游，follower 等 leader 写回后直接看到
// 新 token（NeedsRefresh 判定放在 fn 内、刷新成功后即失效，follower 醒来
// 时 auth 已是新 token，无需再判）。
//
// key 空（无 uid 的测试/内部调用）时退化为直跑——单飞组无法归因。
func RefreshAuthOnce(key string, fn func()) {
	if key == "" {
		fn()
		return
	}
	flightGroup.Do(key, fn)
}

// refreshFlightCount 当前在途刷新数（测试与 /status 观测用）。
func refreshFlightCount() int {
	flightGroup.mu.Lock()
	defer flightGroup.mu.Unlock()
	return len(flightGroup.busy)
}

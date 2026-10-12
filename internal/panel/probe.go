// probe.go 面板「健康检验」HTTP 层：对每个账号发一次最小真实 chat 调用，验证
// 账号 → 品牌上游链路是否健康（凭证有效、端点可达、模型可用）。
//
// 探测核心在 internal/upstream/healthprobe.go（ProbeOne/ProbeModelFor/ProbeResult），
// 与 scheduler 定时体检共用同一实现；本文件只做 HTTP 收编与并发编排。
// 设计约束（同探测核心）：
//   - 直连 upstream.Client，**绕过 pool 的一切记账**；
//   - 全部并发（上限 4），单账号 45s 超时；
//   - 结果只读返回，不写任何状态。
package panel

import (
	"net/http"
	"sort"
	"sync"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/scheduler"
	"github.com/linguo2625469/l3api/internal/upstream"
)

// probeAccounts POST /panel/api/probe：?uid= 单账号，缺省全量。并发探测（上限 4）。
func (p *Panel) probeAccounts(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream not available")
		return
	}
	uid := r.URL.Query().Get("uid")
	var targets []*auth.Auth
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if uid != "" && a.UID != uid {
			continue
		}
		targets = append(targets, a)
	}
	if uid != "" && len(targets) == 0 {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	results := make([]upstream.ProbeResult, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, a := range targets {
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = upstream.ProbeOne(r.Context(), p.cfg.Upstream, a)
		}(i, a)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].UID < results[j].UID })
	okCount := 0
	for _, r2 := range results {
		if r2.OK {
			okCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results, "total": len(results), "ok": okCount,
	})
}

// probeLast GET /panel/api/probe/last：最近一次定时体检结果（scheduler 落盘）。
// 尚未执行/文件损坏 → ran_at=null（前端显示「尚未执行」）。
func (p *Panel) probeLast(w http.ResponseWriter, r *http.Request) {
	st, err := scheduler.LoadProbeState(p.cfg.ProbeLastFile)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read probe state: "+err.Error())
		return
	}
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ran_at": nil, "total": 0, "ok": 0, "results": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

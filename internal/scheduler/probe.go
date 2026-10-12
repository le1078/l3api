// probe.go 定时体检：按 schedule.probe_hours 到点对全账号发一次最小真实 chat
// 探测（与面板「检验全部账号」同一实现，见 internal/upstream/healthprobe.go，
// 直连上游、绕过 pool 记账）。结果：
//   - 逐账号 + 汇总写运行日志；
//   - 一轮内有失败 → 发一条聚合告警（notify probe_failed 事件，subject 固定
//     "probe"，去重窗口内多轮合并）；
//   - 整轮结果落盘 ProbeStatePath（data/probe_last.json），供面板「上次定时
//     体检」展示；路径为空则跳过持久化。
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/notify"
	"github.com/linguo2625469/l3api/internal/upstream"
)

// ProbeStateFile 最近一次定时体检结果（面板 GET /panel/api/probe/last 原样透出）。
type ProbeStateFile struct {
	RanAt   string                 `json:"ran_at"` // RFC3339（CST）
	Total   int                    `json:"total"`
	OK      int                    `json:"ok"`
	Results []upstream.ProbeResult `json:"results"`
}

// LoadProbeState 读最近一次定时体检结果；文件不存在/损坏返回 nil, nil（面板据此
// 显示「尚未执行」——损坏不阻断服务，下一轮体检会覆盖重写）。
func LoadProbeState(path string) (*ProbeStateFile, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st ProbeStateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, nil
	}
	return &st, nil
}

// saveProbeState 原子落盘（tmp+rename，与 modelsync 同一模式）。
func saveProbeState(path string, st *ProbeStateFile) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RunProbeNow 立即执行一轮全账号定时体检。tryLockTask("probe") 防到点时刻
// 与其他排程任务的并发触发重复执行（面板手动检验走 HTTP handler 不持此锁，
// 但探测绕过池记账，重复一次无副作用，仅多打一次上游）。
// 口径与手动一致：遍历池内全部账号（含 disabled——体检的目的正是发现坏号），
// 并发上限 4，单账号 45s 超时（在 upstream.ProbeOne 内部）。
func (s *Scheduler) RunProbeNow(ctx context.Context) {
	unlock, ok := tryLockTask("probe")
	if !ok {
		log.Printf("scheduler: probe 正在执行中，本次触发跳过（手动/排程重复）")
		return
	}
	defer unlock()
	if s.cfg.Upstream == nil || s.cfg.Pool == nil {
		log.Printf("scheduler: probe 缺少依赖（upstream/pool 未注入），跳过本轮")
		return
	}
	start := time.Now()
	log.Printf("scheduler: 定时体检开始")

	var targets []*auth.Auth
	for _, st := range s.cfg.Pool.List() {
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		targets = append(targets, a)
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
			results[i] = upstream.ProbeOne(ctx, s.cfg.Upstream, a)
		}(i, a)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].UID < results[j].UID })

	okCount := 0
	var failed []string
	for _, r := range results {
		if r.OK {
			okCount++
			continue
		}
		label := r.UID
		if len(label) > 8 {
			label = label[:8]
		}
		if r.Nickname != "" {
			label += "/" + r.Nickname
		}
		label += "/" + r.Brand
		failed = append(failed, fmt.Sprintf("%s: %s", label, r.Err))
	}
	log.Printf("scheduler: 定时体检完成 total=%d ok=%d failed=%d 耗时 %s",
		len(results), okCount, len(failed), time.Since(start).Round(time.Second))
	for _, f := range failed {
		log.Printf("scheduler: 体检失败 %s", f)
	}

	// 一轮一告警（subject 固定 "probe"，去重窗口内多轮失败合并，防坏号刷屏）。
	if s.cfg.Notifier != nil && len(failed) > 0 {
		s.cfg.Notifier.Post(notify.EventProbeFailed, "probe",
			"l3api 定时体检存在失败",
			fmt.Sprintf("本轮体检 %d/%d 个账号失败：\n%s", len(failed), len(results), strings.Join(failed, "\n")))
	}

	st := &ProbeStateFile{
		RanAt:   time.Now().In(scheduleTZ).Format(time.RFC3339),
		Total:   len(results),
		OK:      okCount,
		Results: results,
	}
	if err := saveProbeState(s.cfg.ProbeStatePath, st); err != nil {
		log.Printf("scheduler: probe_last 落盘失败: %v", err)
	}
}

// 持久化：本地 state.json 落盘/加载、Redis 快照镜像（StoreSnapshotter）、
// 后台 flusher、择新恢复（RestoreFromSnapshot）。
package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

var flushInterval = 5 * time.Second

// persistLogEvery 连续落盘失败每 N 次打一条提醒（flusher 5s 一把 ≈ 1 分钟一次），
// 避免磁盘持续满/权限丢失时日志刷屏。
const persistLogEvery = 12

// snapshot 池状态快照（Redis 镜像用）。与本地 state.json 同源（stateFile），
// 额外带 savedAt 时间戳供"择新恢复"（比较本地与 Redis 快照的新旧）。
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// Pool 账号池。
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// defaultIdle* 闲置补偿默认参数（claude-api selectWeightedRandom 参考口径）。
func (p *Pool) RestoreFromSnapshot() {
	store := p.storeRef() // 持 RLock 读（2026-10-20：原裸读 p.store 与 SetStore 竞态）
	if store == nil || p.stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(p.stateFp)
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("pool: 恢复来源=本地 state.json（无 Redis 快照）")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// 快照无 savedAt：无法比较新旧，本地优先。
		log.Printf("pool: 恢复来源=本地 state.json（Redis 快照无 saved_at）")
		return
	}
	// 本地文件不存在（全新部署/挂载丢失，localErr != nil）时必须采用快照——
	// 此前该分支被漏掉：本地缺失会走到末尾「本地较新」的错误分支，合法
	// Redis 快照被静默忽略，全部账号冷却/熔断/禁用状态失忆。
	if localErr != nil || !localInfo.ModTime().After(snap.SavedAt) {
		// 采用快照 = 全量覆盖本地内存态（applySnapshotLocked 直接重建 byUID）。
		// 先备份本地 state.json 为 .bak（2026-10-20）：多机时钟偏移会让
		// 「较旧本地」被「较新但内容更旧」的 Redis 快照覆盖（SavedAt 由写侧
		// time.Now() 打，跨机不可比），没有副本时这次覆盖不可逆。
		if localErr == nil {
			backupStateBeforeRestore(p.stateFp)
		}
		p.mu.Lock()
		p.applySnapshotLocked(snap)
		p.mu.Unlock()
		p.dirty.Store(true)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	log.Printf("pool: 恢复来源=本地 state.json（较新于 Redis 快照 %s）", snap.SavedAt.Format(time.RFC3339))
	_ = localErr
}

// backupStateBeforeRestore 采用 Redis 快照前把本地 state.json 复制为
// <path>.bak（同名覆盖：只保留最近一次快照恢复前的副本，不滚历史）。
// 失败仅打日志不阻断——恢复本身优先（.bak 只是保险）。
func backupStateBeforeRestore(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return // 读不到就没有备份的意义（上层已判 localErr == nil，这里是双保险）
	}
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		log.Printf("pool: 快照恢复前备份 %s.bak 失败: %v（继续采用快照）", path, err)
	}
}

// Acquire 为账号占一个在途名额；false 表示该账号已达上限（或不存在）。
// 必须在成功 Pick 后调用；调用方负责 defer Release。
func (p *Pool) startFlusher() {
	stopCh := make(chan struct{})
	// 在启动 goroutine 前同步写入（避免与测试对 flushInterval 的恢复写竞争）；
	// stopCh 同步登记，Close 才能可靠停止（New 与 startFlusher 之间无并发窗口）。
	p.stopCh = stopCh
	interval := flushInterval
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.mu.Lock()
				if p.dirty.Swap(false) {
					p.saveLocked()
				}
				p.mu.Unlock()
			case <-stopCh:
				return
			}
		}
	}()
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
func (p *Pool) Flush() {
	p.mu.Lock()
	if p.dirty.Swap(false) {
		p.saveLocked()
	}
	p.mu.Unlock()
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		// 解析失败不再静默：备份损坏文件（.corrupt）并明确告警——此前静默 return
		// 会让"半截文件覆盖好文件"的落盘竞态直接演变成整池状态失忆（2026-10-13 修复）。
		bak := p.stateFp + ".corrupt"
		_ = os.Rename(p.stateFp, bak)
		log.Printf("ERROR: state.json 解析失败（%v）——已备份为 %s，本次从空状态启动（账号状态将重新学习）", err, bak)
		return
	}
	p.applyAccountsLocked(sf.Accounts)
}

// applyAccountsLocked 用持久化账号状态覆盖/插入 byUID（placeholder 凭证，Add 时换全）。
// 本地 load() 与 Redis 快照恢复共用；调用方必须已持有 p.mu。
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	now := time.Now()
	for uid, s := range accounts {
		// err_total 优先；旧文件的 err_count（连续错误）作一次性迁移源映射进来（二者取较大者，
		// 尽最大可能保留历史观测信号——旧语义下 err_count 也真实发生过错误，不应丢）。
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		e := &entry{
			a:                &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:          s.Credits,
			creditsTotal:     s.CreditsTotal,
			creditsExpiring:  s.CreditsExpiring,
			disabled:         s.Disabled,
			paused:           s.Paused,
			reason:           s.Reason,
			until:            s.Until,
			coolKind:         s.CoolKind,
			successCount:     s.SuccessCount,
			errTotal:         errTotal,
			lastErr:          s.LastErr,
			lastSuccess:      s.LastSuccess,
			tokenUsage:       s.TokenUsage,
			softStreak:       s.SoftStreak,
			sessionDeadFails: s.SessionDeadFails,
			sessionDead:      s.SessionDead,
			consecutiveFails: s.ConsecutiveFails,
		}
		// 熔断器持久化恢复：breakerUntil 未过期才恢复（过期不复活），retryCount 仅在
		// 熔断仍有效时保留（否则归零，不保留无用退避指数）。
		if s.BreakerUntil != nil && now.Before(*s.BreakerUntil) {
			e.breakerUntil = *s.BreakerUntil
			e.retryCount = s.RetryCount
		}
		// 连败降权：未过期才恢复（过期/零值不写不复活）。
		if s.DegradeUntil != nil && now.Before(*s.DegradeUntil) {
			e.degradeUntil = *s.DegradeUntil
		}
		// 模型级独立冷却（6004 重置墙钟 / 11102 负缓存）：惰性过滤已过期条目。
		if len(s.ModelCooldowns) > 0 {
			for m, mc := range s.ModelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue
				}
				if e.modelCooldowns == nil {
					e.modelCooldowns = map[string]modelCooldown{}
				}
				e.modelCooldowns[m] = modelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason}
			}
		}
		// 成本账本：惰性过滤过期（modelCostTTL 外不恢复）+ 剔除结构破损条目
		// （负 per1k / 零 LastSeen——上游异常或旧文件手改产生的脏数据）。
		if len(s.ModelCosts) > 0 {
			for m, mc := range s.ModelCosts {
				if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL || mc.CostPer1k < 0 {
					continue
				}
				if e.modelCost == nil {
					e.modelCost = map[string]modelCostEntry{}
				}
				e.modelCost[m] = modelCostEntry{CostPer1k: mc.CostPer1k, LastSeen: mc.LastSeen, Samples: mc.Samples}
			}
		}
		// 免费额度尽台账：惰性过滤已过期条目（复位点已过 → 配额已重置，不复活）。
		if len(s.FreeExhausted) > 0 {
			for m, until := range s.FreeExhausted {
				if until.IsZero() || !now.Before(until) {
					continue
				}
				if e.freeExhaust == nil {
					e.freeExhaust = map[string]time.Time{}
				}
				e.freeExhaust[m] = until
			}
		}
		p.byUID[uid] = e
	}
}

// applySnapshotLocked 用 Redis 快照覆盖内存状态（已在择新判定后采用）。调用方必须已持有 p.mu。
func (p *Pool) applySnapshotLocked(s snapshot) {
	p.byUID = map[string]*entry{}
	p.applyAccountsLocked(s.Accounts)
}
// saveLocked 仅做锁内快照（调用方持 p.mu）；实际 marshal/写盘/rename 在锁外
// 由 saveStateFile 完成——此前全量在写锁内做，磁盘慢（Defender 扫描/网络盘）
// 时每 5s 阻塞所有 Pick/Status 热路径（2026-09-29 修复）。
func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := p.stateOverviewLocked()
	fp := p.stateFp
	// 锁外 I/O 后需回写失败计数（persistFails 在 p.mu 下维护），故起 goroutine
	// 太重、同步又不应持锁——调用方（flusher/Flush）已改为先快照后放锁再写。
	p.mu.Unlock()
	defer p.mu.Lock() // 恢复调用方持锁契约
	// saveMu：写盘段串行。注意必须在本 goroutine 已放开 p.mu 之后获取、且在重新
	// 拿回 p.mu 之前释放（defer 顺序：defer p.mu.Lock() 先注册先执行——这里手动
	// 成对，避免死锁）。
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.saveStateFile(fp, sf)
}

// saveStateFile 无锁写盘：marshal + tmp+rename + Redis 镜像 + 失败计数回写。
func (p *Pool) saveStateFile(fp string, sf stateFile) {
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		p.mu.Lock()
		p.notePersistFail(err)
		p.mu.Unlock()
		return
	}
	if dir := filepath.Dir(fp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		p.mu.Lock()
		p.notePersistFail(err)
		p.mu.Unlock()
		return
	}
	if err := os.Rename(tmp, fp); err != nil {
		p.mu.Lock()
		p.notePersistFail(err)
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	if p.persistFails > 0 {
		// 从连续失败中恢复：打一条恢复日志，避免"错误打完却无人知道已恢复"。
		log.Printf("pool: state.json 落盘恢复（此前连续失败 %d 次）", p.persistFails)
		p.persistFails = 0
	}
	p.mu.Unlock()
	// 同步镜像一份快照到 Redis（fire-and-forget），与本地 state.json 并存作恢复备份。
	// store 经 storeRef 持 RLock 读（2026-10-20：原裸读与 SetStore 竞态）。
	// 注意此处已在锁外（saveLocked 末尾），不能再取 p.mu——RLock 不冲突。
	if store := p.storeRef(); store != nil {
		snapRaw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
		if err == nil {
			store.SaveState(snapRaw)
		}
	}
}

// PersistFails 返回当前连续落盘失败次数（0 = 健康）。供 /status 与 /metrics
// 暴露（2026-10-20）：此前连续失败只打节流日志，磁盘满/权限丢失时面板显示
// 一切正常——运维看不出 state.json 已经写不进去（重启即丢全部冷却/禁用状态）。
func (p *Pool) PersistFails() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.persistFails
}

// notePersistFail 记录一次本地 state.json 落盘失败，并按节流规则决定是否打日志：
// 首败（状态成功→失败）打完整错误、每 persistLogEvery 次连续失败打一条提醒、
// 其余连续失败静默（flusher 5s 一把，磁盘持续满时不刷屏）。
// 恢复成功的日志由 saveLocked 在成功路径统一打。与 redisstore 三处异步写的
// "失败仅打日志、不向上抛"范式对齐，但落盘失败对运维是盲区，故多一层节流（notification）。
func (p *Pool) notePersistFail(err error) {
	if p.persistFails == 0 {
		log.Printf("pool: state.json 落盘失败: %v", err)
	} else if p.persistFails%persistLogEvery == 0 {
		log.Printf("pool: state.json 连续落盘失败 %d 次: %v", p.persistFails, err)
	}
	p.persistFails++
}

// stateOverviewLocked 收集当前内存状态为 stateFile（供落盘 + 快照镜像复用）。调用方必须已持 p.mu。
func (p *Pool) stateOverviewLocked() stateFile {
	now := time.Now()
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		s := stateAccount{
			Credits:          e.credits,
			CreditsTotal:     e.creditsTotal,
			Disabled:         e.disabled,
			Paused:           e.paused,
			Reason:           e.reason,
			Until:            e.until,
			CoolKind:         e.coolKind,
			SuccessCount:     e.successCount,
			ErrTotal:         e.errTotal,
			LastSuccess:      e.lastSuccess,
			LastErr:          e.lastErr,
			TokenUsage:       e.tokenUsage,
			SoftStreak:       e.softStreak,
			SessionDeadFails: e.sessionDeadFails,
			SessionDead:      e.sessionDead,
			ConsecutiveFails: e.consecutiveFails,
			CreditsExpiring:  e.creditsExpiring,
		}
		// 熔断截止：仅未过期才落盘（指针 nil 才能被 omitempty 真省略）。
		if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
			u := e.breakerUntil
			s.BreakerUntil = &u
			s.RetryCount = e.retryCount
		}
		// 连败降权截止：仅未过期才落盘。
		if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
			u := e.degradeUntil
			s.DegradeUntil = &u
		}
		// 模型级独立冷却：惰性过滤已过期条目（Hits 不落盘，重启后 11102 退避从基数重学）。
		if len(e.modelCooldowns) > 0 {
			for m, mc := range e.modelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue
				}
				if s.ModelCooldowns == nil {
					s.ModelCooldowns = map[string]stateModelCooldown{}
				}
				s.ModelCooldowns[m] = stateModelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason}
			}
		}
		// 成本账本：惰性过滤过期观测（modelCostTTL 外不写——陈旧价格不复活）。
		if len(e.modelCost) > 0 {
			for m, mc := range e.modelCost {
				if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
					continue
				}
				if s.ModelCosts == nil {
					s.ModelCosts = map[string]stateModelCost{}
				}
				s.ModelCosts[m] = stateModelCost{CostPer1k: mc.CostPer1k, LastSeen: mc.LastSeen, Samples: mc.Samples}
			}
		}
		// 免费额度尽台账：惰性过滤已过期条目（复位点已过 → 配额已重置，不落盘）。
		if len(e.freeExhaust) > 0 {
			for m, until := range e.freeExhaust {
				if until.IsZero() || !now.Before(until) {
					continue
				}
				if s.FreeExhausted == nil {
					s.FreeExhausted = map[string]time.Time{}
				}
				s.FreeExhausted[m] = until
			}
		}
		sf.Accounts[uid] = s
	}
	return sf
}

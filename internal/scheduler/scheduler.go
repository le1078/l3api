// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/docsheet"
	"github.com/linguo2625469/l3api/internal/notify"
	"github.com/linguo2625469/l3api/internal/pool"
	"github.com/linguo2625469/l3api/internal/provider"
	"github.com/linguo2625469/l3api/internal/upstream"
)

// supports 品牌能力门控：运营任务仅对支持该能力的品牌账号执行。codebuddy 全能力
// 恒 true（现有行为零变化）；qoder/trae 等纯网关品牌按能力声明跳过（不发上游调用，
// 避免 404 噪音与风控暴露）。
func supports(a *auth.Auth, pick func(provider.Capabilities) bool) bool {
	return pick(provider.For(provider.Brand(a.Brand())).Capabilities())
}

// filterBrand 品牌过滤：未指定 brand（len==0）时放行全部账号；指定则只放行
// brand 匹配的账号（支持同时传多个 brand）。用于面板按品牌分区的批量操作——
// 空参数语义与改造前完全一致（所有调用方未传 brand 即全量执行）。
func filterBrand(st pool.Status, brand ...string) bool {
	if len(brand) == 0 {
		return true
	}
	for _, b := range brand {
		if b != "" && b == st.Brand {
			return true
		}
	}
	return false
}

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此优先消耗，见
	// entry.creditsExpiring）。<=0 时禁用分桶（全部归长期，行为与引入前一致）。
	// 默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool

	// ModelSyncDisabled 显式关闭模型目录同步排程（schedule.model_sync_enabled=false）。
	ModelSyncDisabled bool
	// ModelSyncTime 模型目录同步的每日时点（"HH:MM"，CST；默认 "03:30"，
	// 避开整点签到高峰）。非法值回落默认。
	ModelSyncTime string

	// ProbeHours 定时体检排程（schedule.probe_hours）。与其他任务族不同：
	// 空切片 = 关闭（零值 Config 不新增任何排程，老调用方零影响）；
	// config 层的缺省 [9] 由 main 显式传入，不经本层回落。
	ProbeHours []int
	// ProbeDisabled 显式关闭定时体检排程（schedule.probe_enabled=false）。
	ProbeDisabled bool
	// ProbeStatePath 最近一次体检结果落盘路径（面板「上次定时体检」读取）；
	// 空 = 不持久化（结果只进日志/告警）。
	ProbeStatePath string

	// DocsSheet 腾讯文档共享表格推送（目录变化后整块重写）。nil = 不推送。
	DocsSheet *docsheet.Client

	// Notifier 事件告警（v1.11）：签到失败汇总 + 余额低于阈值外推。nil = 不告警。
	Notifier notify.Poster
	// BalanceLowCredits 余额告警阈值（credits 原始单位；<=0 = 关闭）。
	// 与 notify.Config.BalanceLowCredits 同值（main 装配时注入同源）。
	BalanceLowCredits int64
}

// notifyCheckinFailed 签到失败汇总（一轮一告警，subject 固定 "checkin"——
// 去重窗口内多轮失败合并展示，避免坏号多时刷屏）。
func (s *Scheduler) notifyCheckinFailed(failed []string) {
	if s.cfg.Notifier == nil || len(failed) == 0 {
		return
	}
	s.cfg.Notifier.Post(notify.EventCheckinFailed, "checkin",
		"l3api 签到任务存在失败",
		fmt.Sprintf("本轮签到 %d 个账号失败：\n%s", len(failed), strings.Join(failed, "\n")))
}

// markSessionDeadIf 错误为 ErrSessionDead 时给账号打失效观测标志；过期告警由
// Pool.MarkSessionDead 在徽章跳变时统一外推（与 chat/keepalive 路径同口径）。
// via 描述来源路径（"余额刷新"/"签到"），供日志定位。
func (s *Scheduler) markSessionDeadIf(uid string, a *auth.Auth, err error, via string) {
	var ue *upstream.Error
	if !errors.As(err, &ue) || ue.Kind != upstream.ErrSessionDead {
		return
	}
	if s.cfg.Pool.MarkSessionDead(uid) {
		log.Printf("%s %s: session 失效（401），面板已标记，需重新登录", via, uid)
	}
}

// notifyBalanceLow 余额低于阈值告警（subject=uid，去重窗口防每 5 分钟刷新刷屏）。
func (s *Scheduler) notifyBalanceLow(uid string, brand, nickname string, remain int64) {
	s.schedMu.Lock()
	threshold := s.cfg.BalanceLowCredits
	s.schedMu.Unlock()
	if s.cfg.Notifier == nil || threshold <= 0 || remain >= threshold {
		return
	}
	label := uid
	if len(label) > 8 {
		label = label[:8]
	}
	if nickname != "" {
		label += "/" + nickname
	}
	if brand != "" {
		label += "/" + brand
	}
	s.cfg.Notifier.Post(notify.EventBalanceLow, uid,
		"l3api 账号余额偏低",
		fmt.Sprintf("账号 %s 剩余积分 %d，低于阈值 %d（该号将很快被上游限流/冷却）", label, remain, threshold))
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance 是「排程已变，立即重算」通知：Run 与余额刷新循环各自消费，
	// 分别用独立 channel（同 channel 被两个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64

	// modelSync 每日目录同步的运行期状态：fired=当天已执行的日期（CST，
	// 防 30s tick 重复触发）；rearmModelSync 热改通知（SetModelSync 调用后
	// 下一轮 tick 立即按新参数判定）。
	modelSyncFired string
	rearmModelSync chan struct{}
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	// 注意：ProbeHours 刻意不在此回落默认——空 = 关闭（见 Config 字段注释）。
	if cfg.ModelSyncTime == "" {
		cfg.ModelSyncTime = defaultModelSyncTime
	}
	return &Scheduler{
		cfg:            cfg,
		adoptTried:     make(map[string]string),
		rearmSchedule:  make(chan struct{}, 1),
		rearmBalance:   make(chan struct{}, 1),
		rearmModelSync: make(chan struct{}, 1),
	}
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours, probeHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled, probeDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(probeHours) > 0 {
		s.cfg.ProbeHours = probeHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.ProbeDisabled = probeDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
// scheduleTZ 排程时区固定 UTC+8（与上游签到/活动口径一致）：
// 不再跟随进程本地时区——容器忘设 TZ 时按 UTC 算会整体错位 8 小时（2026-09-29 修复）。
var scheduleTZ = time.FixedZone("CST", 8*3600)

func nextFire(now time.Time, hours []int) time.Time {
	now = now.In(scheduleTZ)
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, scheduleTZ)
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskProbe
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours, probeHours := s.cfg.TravelHours, s.cfg.ActivityHours, s.cfg.ProbeHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	travelOff, activityOff, probeOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled, s.cfg.ProbeDisabled
	s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	if !probeOff && len(probeHours) > 0 {
		slots = append(slots, slot{nextFire(now, probeHours), taskProbe})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 四类任务全部禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.rearmSchedule:
			timer.Stop() // 排程已变：重算下一次唤醒
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 多号 × 间隔 ≈ 数分钟睡眠）不再阻塞同槽其他任务族；返回前等全部
			// 任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠的风险与
			// 串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// taskLocks 任务族互斥锁（TryLock 语义）：定时排程与面板手动触发共用同一把锁，
// 同一类任务同一时刻只跑一遍——否则到点瞬间叠加手动触发会重复执行
// （2026-09-29 实测：trae 账号重复签到被上游 429）。
var taskLocks sync.Map // name -> chan struct{}（cap=1）

// tryLockTask 尝试占有某类任务的执行权；已被占用返回 ok=false（调用方应跳过）。
func tryLockTask(name string) (unlock func(), ok bool) {
	ch, _ := taskLocks.LoadOrStore(name, make(chan struct{}, 1))
	c := ch.(chan struct{})
	select {
	case c <- struct{}{}:
		return func() { <-c }, true
	default:
		return nil, false
	}
}

// recoverTask 后台 goroutine 的 panic 兜底：记日志后吞掉（单个任务失败不扩散）。
// net/http 只 recover handler 本 goroutine；spawned goroutine 的 panic 会直接
// 终止整个进程——生产网关上签到任务 panic = 聊天服务整体宕机（2026-09-28 修复）。
func recoverTask(name string) {
	if r := recover(); r != nil {
		log.Printf("scheduler: task %s panic recovered: %v", name, r)
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// ctx 取消时由各任务内部的可取消等待快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			// panic 兜底：net/http 只 recover handler goroutine，后台任务 panic
			// 是进程级致命——签到/保活链路解析代码量大，不允许带病打垮网关。
			defer recoverTask(fmt.Sprintf("batch-%d", int(k)))
			switch k {
			case taskCheckin:
				s.RunCheckinNow()
			case taskTravel:
				s.RunTravelNow()
			case taskActivity:
				s.runActivity(ctx)
			case taskKeepalive:
				s.RunKeepaliveNow()
			case taskBlackcat:
				s.RunBlackcatNow()
			case taskProbe:
				s.RunProbeNow(ctx)
			}
		}(k)
	}
	wg.Wait()
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
func (s *Scheduler) RunCheckinNow(brand ...string) {
	unlock, ok := tryLockTask("checkin")
	if !ok {
		log.Printf("scheduler: checkin 正在执行中，本次触发跳过（手动/排程重复）")
		return
	}
	defer unlock()
	var checkinFailed []string
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if !filterBrand(st, brand...) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		// D4 门控：realm=global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			continue
		}
		if !supports(a, func(c provider.Capabilities) bool { return c.Checkin }) {
			continue
		}
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
			if upstream.IsAlreadyCheckin(err) {
				log.Printf("checkin %s: 今天已签到（幂等）", st.UID)
			} else if upstream.IsCheckinInfo(err) {
				// qoder 权益监测的信息性结果（有可领活动待桌面端领取 / 今日无活动）：
				// 打 info 而非 error，不触发账号冷却。
				log.Printf("checkin %s: %s", st.UID, err.Error())
			} else {
				log.Printf("checkin %s: %v", st.UID, err)
				s.markSessionDeadIf(st.UID, a, err, "签到")
				checkinFailed = append(checkinFailed, fmt.Sprintf("- %s (%s): %v", st.UID, a.Brand(), err))
			}
			// 其余业务错误也继续走余额查询
		} else {
			// 成功也留一行（品牌可见性）：trae 经 checkin_info 已有明细行，
			// 这里只补「静默成功」的品牌（codebuddy/qoder），让定时签到
			// 对全品牌的覆盖在日志里可核对。
			if a.Brand() != "trae" {
				log.Printf("checkin %s: %s 签到完成", st.UID, a.Brand())
			}
		}
		// 分桶查余额：快过期窗口内的积分单独标记，pool 优先消耗。
		// ExpiringSoonWindow<=0 时退化为纯总量（与引入前一致）。
		// ⚠️ 按 Balance 能力门控（不是 Checkin）：Checkin 现有 codebuddy/qoder/trae 三家，
		// 但 trae 的余额未接通，不门控会打到 codebuddy 接口吃 401。
		// qoder 的余额由 UserResourceDetailed 内部分派到 QoderQuota，不受影响。
		if !supports(a, func(c provider.Capabilities) bool { return c.Balance }) {
			continue
		}
		remain, total, expiring, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
		if err != nil {
			log.Printf("user-resource %s: %v", st.UID, err)
			s.markSessionDeadIf(st.UID, a, err, "签到余额查询")
			continue
		}
		// 签到成功是「跨了一个计费周期」的强证据 → 强解冻（含 429 软冷却与模型级冷却）。
		s.cfg.Pool.ReviveAfterCheckin(st.UID, remain, total)
		if expiring > 0 {
			s.cfg.Pool.SetCreditsDetailed(st.UID, remain, total, expiring)
		}
		s.notifyBalanceLow(st.UID, a.Brand(), a.Nickname, remain)
	}
	s.notifyCheckinFailed(checkinFailed)
	s.RunStreakBonusNow()
	s.RunSchoolNow() // 开学季活动（活动期 9/13-9/24，结束自动跳过）
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// 一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
// RunActivityNow 是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走
// runActivity（ctx 取消时立即放弃剩余账号，不等限速睡满）。
func (s *Scheduler) RunActivityNow(brand ...string) {
	unlock, ok := tryLockTask("activity")
	if !ok {
		log.Printf("scheduler: activity 正在执行中，本次触发跳过（手动/排程重复）")
		return
	}
	defer unlock()
	s.runActivity(context.Background(), brand...)
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context, brand ...string) {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if !filterBrand(st, brand...) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无任务中心/活跃体系，不发起任何上游调用
		}
		// 能力门控（2026-09-29 修复缺失）：活跃上报是 codebuddy 独有体系，
		// 此前缺这道闸，qoder/trae 账号会被打到 codebuddy 端点吃 401。
		if !supports(a, func(c provider.Capabilities) bool { return c.Activity }) {
			continue
		}
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		cid := fmt.Sprintf("l3api-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", a.UID, err)
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", a.UID, err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", a.UID)
		return true
	}
	log.Printf("activity %s: streak days=%d", a.UID, days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow(brand ...string) {
	unlock, ok := tryLockTask("keepalive")
	if !ok {
		log.Printf("scheduler: keepalive 正在执行中，本次触发跳过（手动/排程重复）")
		return
	}
	defer unlock()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if !filterBrand(st, brand...) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用", st.UID, pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
	}
}

// RunBalanceRefreshNow 并发对所有非禁用账号查询余额并更新池内 credits。
// 解冻语义（2026-10-13 修正）：**只清信用类硬冷却**（CoolHard 余额耗尽），
// 429 软冷却与模型级冷却（6004/11102）保留——余额查询成功不代表限流恢复，
// 每 5 分钟清一次限流退避会让指数退避形同虚设。不做签到、不刷新 token。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
func (s *Scheduler) RunBalanceRefreshNow(brand ...string) {
	unlock, ok := tryLockTask("balance")
	if !ok {
		log.Printf("scheduler: balance 正在执行中，本次触发跳过（手动/排程重复）")
		return
	}
	defer unlock()
	// 并发上限（2026-10-20）：此前每个账号起一个 goroutine 同时打上游余额
	// 接口——46 账号即 46 并发，每 5 分钟一轮。高峰时同一出口 IP 短时 46 个
	// 请求，可能自触发 429/风控（把「查余额」变成「把账号打进冷却」）。
	// runActivity 早有 activityAccountDelay 的先例；这里用 worker 池限并发，
	// 46 账号分 4 路跑完（每路 ~12 个，余额接口 <1s，一轮仍 <15s）。
	const balanceWorkers = 4
	accounts := make([]*auth.Auth, 0, 16)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if !filterBrand(st, brand...) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// Balance 能力门控：未接通余额查询的品牌（trae）直接跳过，不对 codebuddy
		// 计费端点发出必然 404/鉴权失败的请求（浪费配额且产生误导性错误日志）。
		if !supports(a, func(c provider.Capabilities) bool { return c.Balance }) {
			continue
		}
		accounts = append(accounts, a)
	}
	var wg sync.WaitGroup
	work := make(chan *auth.Auth)
	for w := 0; w < balanceWorkers && w < len(accounts); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverTask("balance:worker")
			for a := range work {
				uid := a.UID
				func() {
					defer recoverTask("balance:" + uid)
					remain, total, expiring, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
					if err != nil {
						log.Printf("balance %s: %v", uid, err)
						s.markSessionDeadIf(uid, a, err, "余额刷新")
						return
					}
					// 只解信用冷却（CoolHard）；429/模型级冷却保留（见方法注释）。
					s.cfg.Pool.ReenableIfCredits(uid, remain, total)
					if expiring > 0 {
						s.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring)
					}
					s.notifyBalanceLow(uid, a.Brand(), a.Nickname, remain)
				}()
			}
		}()
	}
	for _, a := range accounts {
		work <- a
	}
	close(work)
	wg.Wait()
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				func() {
					defer recoverTask("balance_refresh_tick")
					s.RunBalanceRefreshNow()
				}()
			}
		}
	}()
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}

// ────────────────────────────────────────────────────────────────────────────
// 模型目录每日同步（v1.12 model-sync）
// ────────────────────────────────────────────────────────────────────────────

// defaultModelSyncTime 默认每日同步时点（CST）。选 03:30：避开整点签到高峰
// （整点任务族在 Run 主循环），凌晨低峰对上游风控暴露最小。
const defaultModelSyncTime = "03:30"

// modelSyncBrands 参与自动同步的品牌（codebuddy 是动态目录不在此列）。
var modelSyncBrands = []string{"qoder", "trae"}

// modelSyncFireToday 计算「今天（CST）的同步时点」；tod 非法回落默认。
func modelSyncFireToday(now time.Time, tod string) time.Time {
	hh, mm := 3, 30
	if t, err := time.Parse("15:04", tod); err == nil {
		hh, mm = t.Hour(), t.Minute()
	}
	n := now.In(scheduleTZ)
	return time.Date(n.Year(), n.Month(), n.Day(), hh, mm, 0, 0, scheduleTZ)
}

// StartModelSync 启动每日模型目录同步循环（30s tick，到点（CST）且当天未跑
// 则触发）。循环恒启动（main 无条件调用）：disabled 时挂起在 rearmModelSync
// 上等待——面板热开启需要有人消费唤醒信号。
// 运行期 SetModelSync 热改开关/时点，下一轮 tick 生效（重新启用时立即唤醒判定）。
func (s *Scheduler) StartModelSync(ctx context.Context) {
	go func() {
		for {
			s.schedMu.Lock()
			disabled := s.cfg.ModelSyncDisabled
			tod := s.cfg.ModelSyncTime
			s.schedMu.Unlock()
			if disabled {
				select {
				case <-ctx.Done():
					return
				case <-s.rearmModelSync:
					continue
				}
			}
			now := time.Now()
			today := now.In(scheduleTZ).Format("2006-01-02")
			if s.modelSyncFired != today && !now.Before(modelSyncFireToday(now, tod)) {
				s.modelSyncFired = today
				func() {
					defer recoverTask("model_sync")
					s.RunModelSyncNow(ctx)
				}()
			}
			select {
			case <-ctx.Done():
				return
			case <-s.rearmModelSync:
			case <-time.After(30 * time.Second):
			}
		}
	}()
	if s.cfg.ModelSyncDisabled {
		log.Printf("scheduler: 模型目录每日同步已禁用（schedule.model_sync_enabled=false）")
	} else {
		log.Printf("scheduler: 模型目录每日同步已启动（时点 %s CST，品牌 %v）", s.cfg.ModelSyncTime, modelSyncBrands)
	}
}

// SetBalanceLowCredits 热改余额告警阈值（面板保存配置时调用）。
func (s *Scheduler) SetBalanceLowCredits(v int64) {
	s.schedMu.Lock()
	s.cfg.BalanceLowCredits = v
	s.schedMu.Unlock()
}

// SetDocsSheet 热改腾讯文档推送配置（面板保存配置时调用）；nil = 关闭推送。
func (s *Scheduler) SetDocsSheet(c *docsheet.Client) {
	s.schedMu.Lock()
	s.cfg.DocsSheet = c
	s.schedMu.Unlock()
}

// SetModelSync 热改模型同步开关/时点（面板保存配置时调用）。重新启用或时点
// 改到今天更晚时刻时，下一轮 tick（30s 内）即按新参数判定。
func (s *Scheduler) SetModelSync(disabled bool, tod string) {
	s.schedMu.Lock()
	s.cfg.ModelSyncDisabled = disabled
	if tod != "" {
		s.cfg.ModelSyncTime = tod
	}
	s.schedMu.Unlock()
	poke(s.rearmModelSync)
}

// RunModelSyncNow 立即执行一轮模型目录同步（定时与面板手动触发共用，
// tryLockTask 互斥）。流程：每品牌 PeekHealthy 挑一个健康只读号 → 官方端点
// 探测 → diff →（trae 新增实测门）→ 有变化才落盘；变化/实测未过/探测失败
// 汇总为一条告警（一轮一单，防刷屏）。单品牌失败不拖累另一品牌。
func (s *Scheduler) RunModelSyncNow(ctx context.Context) {
	unlock, ok := tryLockTask("modelsync")
	if !ok {
		log.Printf("scheduler: 模型目录同步已在执行中，跳过本轮")
		return
	}
	defer unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	up := s.cfg.Upstream
	if up == nil || s.cfg.Pool == nil {
		return
	}

	var lines []string // 告警正文（变化 + 失败汇总）
	applied := false     // 本轮是否有目录变化实际生效（决定要不要推表格）
	for _, brand := range modelSyncBrands {
		a := s.cfg.Pool.PeekHealthy(brand)
		if a == nil {
			log.Printf("scheduler: 模型目录同步跳过 %s（池内无健康账号）", brand)
			continue
		}
		bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		var (
			d   upstream.ModelSyncDiff
			err error
		)
		switch brand {
		case "qoder":
			d, err = up.SyncQoderModels(bctx, a)
		case "trae":
			d, err = up.SyncTraeModels(bctx, a)
		}
		cancel()
		if err != nil {
			log.Printf("scheduler: 模型目录同步 %s 探测失败: %v", brand, err)
			lines = append(lines, fmt.Sprintf("%s：探测失败：%v", brand, err))
			continue
		}
		var parts []string
		if len(d.Added) > 0 {
			parts = append(parts, "新增 "+strings.Join(d.Added, "、"))
		}
		if len(d.Removed) > 0 {
			parts = append(parts, "下架 "+strings.Join(d.Removed, "、"))
		}
		if len(d.Changed) > 0 {
			parts = append(parts, "字段变更 "+strings.Join(d.Changed, "、"))
		}
		if len(d.Rejected) > 0 {
			parts = append(parts, "新模型实测未过（未收录）"+strings.Join(d.Rejected, "、"))
		}
		if len(parts) == 0 {
			log.Printf("scheduler: 模型目录同步 %s 无变化", brand)
			continue
		}
		log.Printf("scheduler: 模型目录同步 %s：%s", brand, strings.Join(parts, "；"))
		lines = append(lines, brand+"："+strings.Join(parts, "；"))
		if len(d.Added) > 0 || len(d.Removed) > 0 || (brand == "qoder" && len(d.Changed) > 0) {
			applied = true
		}
	}
	if len(lines) > 0 && s.cfg.Notifier != nil {
		s.cfg.Notifier.Post(notify.EventModelSync, "modelsync",
			"l3api 模型目录同步", strings.Join(lines, "\n"))
	}
	// 生效目录有实际变化才推表格（trae 的 Changed 仅告警不入目录，不触发推送；
	// qoder Changed 已整体替换生效，算实际变化）。
	if applied {
		s.pushModelSheet(ctx)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// 模型目录同步结果推送腾讯文档（v1.12 docsheet）
// ────────────────────────────────────────────────────────────────────────────

// modelSheetRows 组装全量目录快照行（列语义见 docsheet.Header）：workbuddy
// 国内/国际动态目录 + qoder/trae 静态目录。
// codebuddy 是动态目录：用健康号 FetchModels 现拉；拉取失败返回 error ——
// 调用方中止本次推送（表格保持旧的完整数据，好过写一份缺品牌的）。
func (s *Scheduler) modelSheetRows() ([][]string, error) {
	boolCN := func(b bool) string {
		if b {
			return "是"
		}
		return "否"
	}
	src := "自动同步 " + time.Now().In(scheduleTZ).Format("2006-01-02")
	rows := [][]string{}
	// workbuddy（codebuddy CN 动态目录）。realm 必须传 "cn"：空串在
	// realmMatch 里是全品牌放行，池内 qoder/trae 号 uid 更小时会错选，
	// FetchModels 拿非 codebuddy token 必失败 → 整轮推送中止。
	if a := s.cfg.Pool.PeekHealthy("cn"); a != nil {
		infos, err := s.cfg.Upstream.FetchModels(a)
		if err != nil {
			return nil, fmt.Errorf("codebuddy 动态目录拉取失败: %w", err)
		}
		for _, mi := range infos {
			rows = append(rows, []string{"workbuddy（国内版）", mi.ID, mi.Name,
				boolCN(mi.SupportsImages), boolCN(mi.SupportsReasoning),
				fmt.Sprint(mi.ContextWindow), src + creditNote(mi)})
		}
	} else {
		return nil, fmt.Errorf("codebuddy 池内无健康账号")
	}
	// workbuddy 国际版（global realm）：有 global 账号才探测（无则跳过该段，
	// 不阻断整表推送——global 是可选域）。
	if a := s.cfg.Pool.PeekHealthy("global"); a != nil {
		if ginfos := s.cfg.Upstream.FetchGlobalModelInfos(a); len(ginfos) > 0 {
			for _, mi := range ginfos {
				rows = append(rows, []string{"workbuddy（国际版）", mi.ID, mi.Name,
					boolCN(mi.SupportsImages), boolCN(mi.SupportsReasoning),
					fmt.Sprint(mi.ContextWindow), src + creditNote(mi)})
			}
		}
	}
	for _, m := range upstream.QoderChatModels() {
		rows = append(rows, []string{"qoder", m.ID, m.DisplayName,
			boolCN(m.IsVL), boolCN(m.IsReasoning), fmt.Sprint(m.MaxInputTokens), src})
	}
	for _, m := range upstream.TraeChatModels() {
		rows = append(rows, []string{"trae", m.ID, m.Name,
			boolCN(m.IsVL), boolCN(m.IsReasoning), fmt.Sprint(m.MaxTokens), src})
	}
	return rows, nil
}

// creditNote 倍率/徽章/活动备注（腾讯文档「来源/口径」列后缀）。
//
// 时段此前硬编码 23:00–8:00（实测上游真实口径是 23:00–7:50），且只能识别
// 「夜间免费」一种徽章。改为按结构化促销（/v3/config modelPromotions）输出，
// 无结构化数据时回落徽章兜底窗口——口径集中在 upstream.EffectivePromotion。
//
// 输出约定（保持「 · 」前缀与既有阅读习惯）：
//   - 免费类且当前生效 → " · 免费" / " · 免费（23:00–07:50）"
//   - 折扣类 → " · 夜间折扣 0.50x（23:00–07:50，当前生效）" 或 "（23:00–07:50 生效）"
//   - 日期区间型 → " · 独家限免（10-12 至 10-25）"
//   - 无活动 → 原倍率
func creditNote(mi upstream.ModelInfo) string {
	cr := strings.TrimSpace(mi.Credits)
	now := time.Now().In(scheduleTZ)
	p := mi.EffectivePromotion()
	switch {
	case cr == "x0.00" || cr == "x0" || cr == "0":
		return " · 免费"
	case p != nil:
		label := p.Label
		if label == "" {
			label = mi.Badge
		}
		win := p.WindowText()
		// 免费判定：折后 0x 或因子为 0（上游 discountedCredits="0x" / factor=0）。
		isFree := p.Factor == 0 && (p.Credit == "0x" || p.Credit == "x0.00" || p.Credit == "0")
		switch {
		case isFree && win != "":
			if p.Active(now) {
				return " · 免费（" + win + "）"
			}
			return " · " + label + "（" + win + "）"
		case isFree:
			return " · 免费"
		case win != "":
			// 折扣类：带折后倍率，并说明当前是否生效。
			state := "，当前生效"
			if !p.Active(now) {
				state = " 生效"
			}
			if p.Credit != "" {
				return " · " + label + " " + p.Credit + "（" + win + state + "）"
			}
			return " · " + label + "（" + win + state + "）"
		default:
			// 无时段（错峰使用这类，真含义在原文）：只标徽章，原文不进表格（太长）。
			return " · " + label
		}
	case mi.Badge != "":
		return " · " + mi.Badge
	case cr != "":
		return " · 倍率 " + cr
	}
	return ""
}

// pushModelSheet 目录变化后推送腾讯文档共享表格（cfg.DocsSheet nil/未启用 = 跳过）。
// 失败记日志 + 追加一条告警（subject 独立，去重窗口内多轮合并）。推送失败不影响
// 目录本身已生效的事实（model_sync.json 已落盘）。
func (s *Scheduler) pushModelSheet(ctx context.Context) {
	s.schedMu.Lock()
	ds := s.cfg.DocsSheet // 与 SetDocsSheet 热更互斥（指针读写需同步）
	s.schedMu.Unlock()
	if ds == nil {
		return
	}
	rows, err := s.modelSheetRows()
	if err != nil {
		log.Printf("scheduler: 模型表格推送数据组装失败: %v", err)
		if s.cfg.Notifier != nil {
			s.cfg.Notifier.Post(notify.EventModelSync, "modelsync-sheet",
				"l3api 模型表格推送失败", "数据组装失败："+err.Error())
		}
		return
	}
	// 大表写入（~650 单元格）实测可达数十秒（2026-10-12：首次写入 >30s 超时，
	// 重试 8s 完成，docs 后端波动大）——60s 会截断写入，放宽到 3 分钟，
	// 与 main.docsSheetClient 的 HTTP 超时配套。
	pctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := ds.SyncModels(pctx, rows); err != nil {
		log.Printf("scheduler: 模型表格推送失败: %v", err)
		if s.cfg.Notifier != nil {
			s.cfg.Notifier.Post(notify.EventModelSync, "modelsync-sheet",
				"l3api 模型表格推送失败", err.Error())
		}
		return
	}
	log.Printf("scheduler: 模型目录已推送腾讯文档（%d 行）", len(rows))
}

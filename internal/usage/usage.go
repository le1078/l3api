// Package usage 记录并聚合逐请求 token 用量，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，实测单桶约 90 字节。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// bucket 一个 (时间片, realm, uid, model) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope string `json:"s"` // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm string `json:"r"`
	UID   string `json:"u"`
	Model string `json:"m"`
	// Key 多租户 API Key 名（v1.11）；空 = 无归因（老数据/未启用多 key）。
	// 只追加不参与既有维度：老 usage.json 无 k 字段反序列化为空串，零迁移。
	Key   string  `json:"k,omitempty"`
	Req   int64   `json:"q"`  // 请求数（含失败）
	Err   int64   `json:"e"`  // 失败数
	PT    int64   `json:"p"`  // prompt tokens
	CT    int64   `json:"c"`  // completion tokens
	TT    int64   `json:"t"`  // total tokens（上游给什么用什么的合计）
	LatMs int64   `json:"l"`  // 延迟累计（ms）
	LatN  int64   `json:"ln"` // 延迟样本数
	TPS   float64 `json:"v"`  // 吐字速率累计
	TPSN  int64   `json:"vn"` // 速率样本数
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|realm|uid|model
	dirty   bool
	started time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	// bgStarted 标记后台 goroutine 已启动（Stop 据此避免未 Start 时裸等 done）。
	bgStarted atomic.Bool
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	r.bgStarted.Store(true)
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				r.mu.Lock()
				n := len(r.buckets)
				r.mu.Unlock()
				if n > maxBuckets {
					r.Rollup(time.Now())
				}
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘。未 Start 时直接返回（done 只有
// Start 的 goroutine 退出才 close，裸等会永久阻塞）。
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	if !r.bgStarted.Load() {
		return
	}
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	LatencyMs        int64
	HasLatency       bool
	TokensPerSecond  float64
	HasTPS           bool
}

// Add 记录一次请求尝试（无 key 归因的旧入口，等价 AddKey(k="")）。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
	r.AddKey(now, realm, uid, model, "", d, ok)
}

// AddKey 同 Add，另带多租户 key 名归因（v1.11）。key 为空串时与旧桶键完全同形
// （零迁移、零回归）；非空时追加为独立桶，供面板按 key 拆分用量。
func (r *Recorder) AddKey(now time.Time, realm, uid, model, key string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	// 桶键以 \x00 分隔：段内含 NUL 会撞段（model 来自客户端请求体）。
	// 清洗而非拒绝——用量记账永不阻断请求。
	model = strings.ReplaceAll(model, "\x00", "")
	uid = strings.ReplaceAll(uid, "\x00", "")
	key = strings.ReplaceAll(key, "\x00", "")
	scope := "h:" + now.Format(hourLayout)
	// 分隔用 \x00 而非 "|"：model 来自客户端请求体，含 "|" 时会与其他桶键碰撞串桶
	// （2026-09-29 修复；与本文件 modelAcctAgg 既有口径一致）。
	bucketKey := scope + "\x00" + realm + "\x00" + uid + "\x00" + model
	if key != "" {
		// key 段仅非空时追加：老桶键保持逐字节同形（load/rollup 零回归）。
		bucketKey += "\x00" + key
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[bucketKey]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model, Key: key}
		r.buckets[bucketKey] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	r.dirty = true
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct {
		from, to string
		dayScope string // 折叠后的日粒度 scope（"d:YYYY-MM-DD"）
	}
	var moves []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		dayScope := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{
			from:     k,
			to:       bucketMapKey(dayScope, b.Realm, b.UID, b.Model, b.Key),
			dayScope: dayScope,
		})
	}
	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = m.dayScope
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
			dst.TPS += src.TPS
			dst.TPSN += src.TPSN
		}
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

// bucketMapKey 桶在内存 map 里的组合键。统一 \x00 分隔（model 来自客户端请求体，
// 含 "|" 时会与 "|" 拼接的其他桶键碰撞串桶——2026-09-29 在 Add 引入 \x00，本次把
// load/Rollup 一并对齐：此前三处混用两种分隔符，重启后同一逻辑桶会裂成两个键，
// 下轮 flush→load 再静默丢一份）；key 段仅非空时追加，老桶键保持逐字节同形。
func bucketMapKey(scope, realm, uid, model, key string) string {
	k := scope + "\x00" + realm + "\x00" + uid + "\x00" + model
	if key != "" {
		k += "\x00" + key
	}
	return k
}

// load 从盘恢复桶。
func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[bucketMapKey(b.Scope, b.Realm, b.UID, b.Model, b.Key)] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: 1, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] 原子替换失败: %v", err)
	}
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	PromptTokens  int64   `json:"prompt_tokens"`
	CompletionTok int64   `json:"completion_tokens"`
	TotalTokens   int64   `json:"total_tokens"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	AvgTPS        float64 `json:"avg_tokens_per_second"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg
	latSum     int64
	latSamples int64
	tpsSum     float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	return a
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Key   string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Brand string `json:"brand,omitempty"` // 账号/模型行放品牌（来自面板传入的 uid→brand 映射）
	Extra string `json:"extra,omitempty"` // 账号行放昵称
	Agg
}

// Point 时序上的一个点。
type Point struct {
	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Agg
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals         Agg        `json:"totals"`
	ByRealm        []KeyedAgg `json:"by_realm"`
	ByAccount      []KeyedAgg `json:"by_account"`
	ByModel        []KeyedAgg `json:"by_model"`
	ByModelAccount []KeyedAgg `json:"by_model_account"` // 按 (账号×模型) 拆分，每条记录带品牌+昵称
	ByApiKey       []KeyedAgg `json:"by_api_key"`       // 按 API Key 名拆分（v1.11 多租户归因；空 = 无 key 桶）
	Series         []Point    `json:"series"`

	// ModelSeries / KeySeries 是**逐维度**的时序（v1.15.4）：
	//   map[模型名][]Point / map[key 名][]Point，时间片与 Series 同构（窗口内小时点 +
	//   固定日点），供面板「按模型/按 key 分色多线」图使用。命名维度先按总量取前
	//   seriesTopN 个，其余归并为一个「其他」系列，保证系列数与图例可控。
	// 与 Series 一样只回**用户在看的窗口**：窗口外小时桶折叠成的旧日点不出现。
	ModelSeries map[string][]Point `json:"model_series,omitempty"`
	KeySeries   map[string][]Point `json:"key_series,omitempty"`
	// BrandSeries 按品牌拆分（v1.15.11）。品牌由桶的 realm 派生：品牌账号的 realm
	// 就是品牌名（qoder/trae），codebuddy 的 cn/global 归一为 codebuddy——无额外
	// 持久化字段，老 usage.json 直接可用，零迁移。
	BrandSeries map[string][]Point `json:"brand_series,omitempty"`
	Buckets     int                `json:"buckets"`
	FileBytes   int64              `json:"file_bytes"`
	Since       string             `json:"since,omitempty"`
	Generated   string             `json:"generated"`

	// 时序窗口（面板按所选「近 N 小时/天」画 x 轴用）。WindowFrom/WindowTo 是本地
	// 小时格式（与 Point.T 的 hour 形态同形，"2006-01-02T15"），左闭右闭。
	// 前端据此把 x 轴锚定在真实窗口上——否则只能按「首个数据点」起画，当所选窗口
	// 起点早于最早数据点时，x 轴会被压缩到数据范围，看起来「和选的窗口不一致」。
	WindowFrom  string `json:"window_from,omitempty"`
	WindowTo    string `json:"window_to,omitempty"`
	WindowHours int    `json:"window_hours,omitempty"`
}

// Snapshot 聚合面板一次拉取的用量视图。hours 同时控制时序窗口（小时点个数，
// 其余折叠为日点）与汇总口径：Totals 与 By* 各表只统计落在所选窗口内的桶
// （日粒度桶按所在日纳入），切换「近 3 天 / 近 7 天」时卡片数字随窗口变化。
// nicks 是 uid→昵称映射，仅用于展示。
// TodayTotals 今日（本地时区）请求数与 token 合计，面板顶部「今日请求/Token」卡用。
// 只扫小时桶：今天的桶必然还是 h: 粒度（日粒度折叠只发生在更老的历史上）。
// TT 是上游给的合计口径，缺失时（老桶）退 PT+CT。
func (r *Recorder) TodayTotals() (requests, tokens int64) {
	if r == nil {
		return 0, 0
	}
	prefix := "h:" + time.Now().Format(dayLayout)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, prefix) {
			continue
		}
		requests += b.Req
		if b.TT > 0 {
			tokens += b.TT
		} else {
			tokens += b.PT + b.CT
		}
	}
	return requests, tokens
}

func (r *Recorder) Snapshot(hours int, nicks, brands map[string]string) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	if hours <= 0 || hours > 24*60 {
		hours = 72
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	modelAgg := map[string]*aggAcc{}
	modelAcctAgg := map[string]*aggAcc{} // 按 (uid\x00model) 拆分，供 by_model_account 维度
	keyAgg := map[string]*aggAcc{}       // 按 API Key 名拆分（v1.11）
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}
	// 逐维度时序（v1.15.4）：与 hourSeries/daySeries 同一时间分片，另按 model / key
	// 再拆一层。keyHorizon 只保留窗口内分片——窗口外桶折叠出的旧日点与 Series 同口径
	// 剔除；dayHorizon 收全部日分片（可能含 90 天前的固定日点）。
	modelHour := map[string]map[string]*aggAcc{}
	modelDay := map[string]map[string]*aggAcc{}
	keyHour := map[string]map[string]*aggAcc{}
	keyDay := map[string]map[string]*aggAcc{}
	brandHour := map[string]map[string]*aggAcc{}
	brandDay := map[string]map[string]*aggAcc{}

	nowHour := time.Now().Truncate(time.Hour)
	hourFrom := nowHour.Add(-time.Duration(hours-1) * time.Hour)
	// 日粒度桶只有整天边界，窗口口径按「所在日不早于窗口起点所在日」纳入
	// （会多算窗口首日 0 点到 hourFrom 这段，日粒度无法更细，可接受）。
	dayFrom := hourFrom.Format(dayLayout)

	for i := range bs {
		b := &bs[i]

		scope := strings.TrimPrefix(b.Scope, "h:")
		isHour := strings.HasPrefix(b.Scope, "h:")
		// 逐维度时序：小时桶超窗并入其所在日（dayScope），保证「其他」与命名维度
		// 在日粒度上口径一致（与小时序列的折叠规则同构）。
		dayScope := scope
		inWin := false
		if isHour {
			ts, err := time.ParseInLocation(hourLayout, scope, time.Local)
			if err != nil {
				continue
			}
			inWin = !ts.Before(hourFrom)
			dayScope = ts.Format(dayLayout)
		}
		// inWindow 决定这个桶是否计入卡片/表格（Totals/By*）：小时桶按窗口，
		// 日桶按所在日。窗口外的历史只进时序的折叠日点，不进汇总数字。
		inWindow := inWin || (!isHour && strings.TrimPrefix(b.Scope, "d:") >= dayFrom)
		if inWindow {
			total.add(b)

			if realmAgg[b.Realm] == nil {
				realmAgg[b.Realm] = &aggAcc{}
			}
			realmAgg[b.Realm].add(b)

			if acctAgg[b.UID] == nil {
				acctAgg[b.UID] = &aggAcc{}
			}
			acctAgg[b.UID].add(b)
			// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；
			// keyed() 的 Realm 字段默认是空的（它按 key 分组，不知道 realm）。
			if acctRealm[b.UID] == "" {
				acctRealm[b.UID] = b.Realm
			}

			if modelAgg[b.Model] == nil {
				modelAgg[b.Model] = &aggAcc{}
			}
			modelAgg[b.Model].add(b)

			mk := b.UID + "\x00" + b.Model
			if modelAcctAgg[mk] == nil {
				modelAcctAgg[mk] = &aggAcc{}
			}
			modelAcctAgg[mk].add(b)

			if b.Key != "" {
				if keyAgg[b.Key] == nil {
					keyAgg[b.Key] = &aggAcc{}
				}
				keyAgg[b.Key].add(b)
			}
		}

		// dimAdd 把一个桶累加进 (维度值, 时间片) 的序列格。
		dimAdd := func(m map[string]map[string]*aggAcc, name, slot string) {
			inner := m[name]
			if inner == nil {
				inner = map[string]*aggAcc{}
				m[name] = inner
			}
			if inner[slot] == nil {
				inner[slot] = &aggAcc{}
			}
			inner[slot].add(b)
		}
		if isHour {
			if inWin {
				if hourSeries[scope] == nil {
					hourSeries[scope] = &aggAcc{}
				}
				hourSeries[scope].add(b)
			} else {
				// 超出小时窗口的细粒度数据并入其所在日，避免时序出现空洞。
				if daySeries[dayScope] == nil {
					daySeries[dayScope] = &aggAcc{}
				}
				daySeries[dayScope].add(b)
			}
		} else {
			if daySeries[scope] == nil {
				daySeries[scope] = &aggAcc{}
			}
			daySeries[scope].add(b)
		}
		// 逐模型/逐 key 时序只收「面板画得出的时间片」：窗口内小时点 + 固定日点；
		// 窗口外小时桶折叠出的旧日点与 Series 同口径剔除（否则折线左端挂一个
		// 90 天前的孤立点）。
		brand := usageBrand(b.Realm)
		if inWin {
			dimAdd(modelHour, b.Model, scope)
			dimAdd(brandHour, brand, scope)
			if b.Key != "" {
				dimAdd(keyHour, b.Key, scope)
			}
		}
		if !isHour || !inWin {
			dimAdd(modelDay, b.Model, dayScope)
			dimAdd(brandDay, brand, dayScope)
			if b.Key != "" {
				dimAdd(keyDay, b.Key, dayScope)
			}
		}
	}

	snap := Snapshot{
		Totals:  total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) { return k, "" }),
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		ByModel:     keyed(modelAgg, func(k string) (string, string) { return k, "" }),
		ByApiKey:    keyed(keyAgg, func(k string) (string, string) { return k, "" }),
		Buckets:     len(bs),
		Generated:   time.Now().Format(time.RFC3339),
		WindowFrom:  hourFrom.Format(hourLayout),
		WindowTo:    nowHour.Format(hourLayout),
		WindowHours: hours,
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
		snap.ByAccount[i].Brand = brands[snap.ByAccount[i].Key]
	}

	// by_model_account：按 (账号×模型) 拆分，每条记录带品牌+昵称，解决
	// 「同名模型跨品牌/账号混在一起，看不出归属」的问题（如 glm-5.2 在 codebuddy
	// 与 qoder 都存在）。Key 最终取模型名展示，brand/extra 取自传入映射。
	bma := make([]KeyedAgg, 0, len(modelAcctAgg))
	for k, acc := range modelAcctAgg {
		uid, model := splitKey(k, "\x00")
		bma = append(bma, KeyedAgg{
			Key:   model,
			Realm: acctRealm[uid],
			Brand: brands[uid],
			Extra: nicks[uid],
			Agg:   acc.finish(),
		})
	}
	sort.Slice(bma, func(i, j int) bool {
		if bma[i].Brand != bma[j].Brand {
			return bma[i].Brand < bma[j].Brand
		}
		if bma[i].Key != bma[j].Key {
			return bma[i].Key < bma[j].Key
		}
		return (bma[i].Extra) < (bma[j].Extra)
	})
	snap.ByModelAccount = bma

	// 日点（升序）+ 小时点（升序）拼成一条连续时序。
	dayKeys := make([]string, 0, len(daySeries))
	for k := range daySeries {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "day", Agg: daySeries[k].finish()})
	}
	hourKeys := make([]string, 0, len(hourSeries))
	for k := range hourSeries {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	for _, k := range hourKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "hour", Agg: hourSeries[k].finish()})
	}

	// 逐模型/逐 key 时序：分片组成与 Series 相同（日点在前、小时点在后），前端按
	// 同一时间轴铺网格。命名维度按窗口内总量取前 seriesTopN，其余并成「其他」。
	// 只含窗口内数据（窗口外小时桶折叠出的旧日点已在收集阶段剔除）。
	snap.ModelSeries = buildDimSeries(modelHour, modelDay, topDimNames(modelHour, modelDay, seriesTopN))
	snap.KeySeries = buildDimSeries(keyHour, keyDay, topDimNames(keyHour, keyDay, seriesTopN))
	// 品牌维度天然有界（品牌就 3 个），不截断、无「其他」——上界取 brandHour ∪
	// brandDay 的并集大小（只看 brandHour 会在"窗口内数据全在日桶"时算少，冒出
	// 一个本不该存在的「其他」）。
	snap.BrandSeries = buildDimSeries(brandHour, brandDay, topDimNames(brandHour, brandDay, len(brandHour)+len(brandDay)))

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// 最早的分片即数据起点。
	if len(snap.Series) > 0 {
		snap.Since = snap.Series[0].T
	}
	return snap
}

// usageBrand 由桶的 realm 归一品牌名（**与面板 displayBrand 展示键同口径**）：
//   - 品牌账号（qoder/trae）：realm 即品牌名；
//   - codebuddy 国内版（realm=cn）→ "codebuddy"；国际版（realm=global）→
//     "codebuddy-global"——两版是**独立品牌**（不同域名/不同账号体系/不同目录），
//     面板各视图一律分区展示，品牌时序也按同口径拆开（2026-10-13 用户纠正：
//     此前把 cn/global 合并成一条 codebuddy 线，与面板其余视图不一致）；
//   - "coze" 是已移除品牌的历史桶：归并进 codebuddy（不再单独成线）；
//   - 空 realm 视为 cn（老数据/无域信息）。
func usageBrand(realm string) string {
	switch realm {
	case "qoder", "trae":
		return realm
	case "global":
		return "codebuddy-global"
	case "cn", "", "coze":
		return "codebuddy"
	default:
		return realm
	}
}

// splitKey 按 sep 把组合键拆成两段（如 "uid\x00model" → "uid", "model"）。
func splitKey(s, sep string) (string, string) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):]
	}
	return s, ""
}

// seriesTopN 逐维度时序里单独成线的维度值上限（模型 / key 各自独立计数）。
// 其余归并为一个「其他」系列——图例与绘图开销有界，且小量模型不会把图压糊。
const seriesTopN = 10

// otherSeriesName 是归并桶的系列名，前端用它做图例末尾标注。
const otherSeriesName = "其他"

// dimTotal 一个维度值在窗口内的 total tokens 合计（跨全部可见时间片）。
func dimTotal(hour, day map[string]map[string]*aggAcc, name string) int64 {
	var tt int64
	for _, m := range []map[string]map[string]*aggAcc{hour, day} {
		for _, acc := range m[name] {
			tt += acc.TotalTokens
		}
	}
	return tt
}

// topDimNames 取窗口内总量前 n 的维度值（不足则全取）。并列时按名字升序，
// 保证输出稳定。
func topDimNames(hour, day map[string]map[string]*aggAcc, n int) map[string]bool {
	seen := map[string]bool{}
	names := make([]string, 0, len(hour)+len(day))
	for _, m := range []map[string]map[string]*aggAcc{hour, day} {
		for name := range m {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Slice(names, func(i, j int) bool {
		ti, tj := dimTotal(hour, day, names[i]), dimTotal(hour, day, names[j])
		if ti != tj {
			return ti > tj
		}
		return names[i] < names[j]
	})
	if len(names) > n {
		names = names[:n]
	}
	top := make(map[string]bool, len(names))
	for _, name := range names {
		top[name] = true
	}
	return top
}

// buildDimSeries 把逐维度累加格拍平成「维度名 → 时间片序列」。
//
//   - hour 里的时间片键是窗口内小时（"2006-01-02T15"），day 里是日（"2006-01-02"），
//     两类键同轴不重名，直接合并即可；
//   - top 内的维度单独成线，各保留自己的时间片；缺格由前端铺 null 断线，这里不为
//     缺格造 0 点；
//   - 其余维度归并为一个「其他」系列：小时格按原时间片、日格按日格**逐格累加**
//     （只回合计的话其他系列画不出来）。
//
// 返回按名字升序的切片，前端直接用（顺序稳定，刷新时不抖）。
func buildDimSeries(hour, day map[string]map[string]*aggAcc, top map[string]bool) map[string][]Point {
	merged := map[string]map[string]*aggAcc{}
	put := func(name, slot string, acc *aggAcc) {
		if merged[name] == nil {
			merged[name] = map[string]*aggAcc{}
		}
		if merged[name][slot] == nil {
			merged[name][slot] = &aggAcc{}
		}
		dst := merged[name][slot]
		dst.Requests += acc.Requests
		dst.Errors += acc.Errors
		dst.PromptTokens += acc.PromptTokens
		dst.CompletionTok += acc.CompletionTok
		dst.TotalTokens += acc.TotalTokens
		dst.latSum += acc.latSum
		dst.latSamples += acc.latSamples
		dst.tpsSum += acc.tpsSum
		dst.tpsSamples += acc.tpsSamples
	}
	for _, m := range []map[string]map[string]*aggAcc{hour, day} {
		for name, slots := range m {
			for slot, acc := range slots {
				if top[name] {
					put(name, slot, acc)
				} else {
					put(otherSeriesName, slot, acc)
				}
			}
		}
	}

	out := map[string][]Point{}
	for name, slots := range merged {
		dayKeys := make([]string, 0, len(slots))
		for slot := range slots {
			if len(slot) == len(dayLayout) {
				dayKeys = append(dayKeys, slot)
			}
		}
		sort.Strings(dayKeys)
		hourKeys := make([]string, 0, len(slots))
		for slot := range slots {
			if len(slot) == len(hourLayout) {
				hourKeys = append(hourKeys, slot)
			}
		}
		sort.Strings(hourKeys)
		pts := make([]Point, 0, len(slots))
		for _, k := range dayKeys {
			pts = append(pts, Point{T: k, Scope: "day", Agg: slots[k].finish()})
		}
		for _, k := range hourKeys {
			pts = append(pts, Point{T: k, Scope: "hour", Agg: slots[k].finish()})
		}
		out[name] = pts
	}
	return out
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}

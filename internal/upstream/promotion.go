// 模型营销/折扣活动（modelPromotions）解析与时段判定。
//
// 背景：徽章（Badge，企业端点 tags 里 badge:<文字>:#<颜色>）只有「夜间免费」这类
// 文字，**没有时段**——此前面板与腾讯文档都硬编码「23:00–8:00」，且实测上游真实
// 口径是 23:00–7:50（glm-5.2 夜间折扣），硬编码值是错的。
//
// /v3/config 的 data.modelPromotions[] 才是权威源，单条形态（2026-10-12 实测）：
//
//	{
//	  "id": "glm-52-night-discount-202607", "kind": "discount",
//	  "enabled": true, "priority": 100, "modelIds": ["glm-5.2"],
//	  "badge":    {"label": "夜间折扣", "color": "#1E90FF"},
//	  "discount": {"discountedCredits": "0.50x", "factor": 0.5},
//	  "hover":    {"textZh": "每晚 23:00—次日 8:00 积分限时立减，错峰用更省"},
//	  "schedule": {"daily": [{"start":"23:00","end":"7:50"}], "timezone": "Asia/Shanghai"}
//	}
//
// schedule 三形态（本包都要能表达，展示层各自决定怎么渲染）：
//   - 日窗：daily[].{start,end}，可跨午夜（23:00→7:50）；
//   - 日期区间：validFrom/validUntil（RFC3339，如 2026-10-12~2026-10-25）；
//   - 全天 + 原文说明：daily 为 00:00–23:59，真正含义在 hover.textZh
//     （如「周一至周五 09:00–12:00、14:00–18:00 属高峰原价」——结构化表达不了，
//     故 WindowText 对全天窗口返回空串，交给原文兜底）。
//
// 注意：同一模型可能有多条促销（glm-5.2 实测两条：prio 100 带 discount 的夜间窗、
// prio 50 白天只挂徽章无折扣），故必须按「带 discount 优先、其次 priority」挑选，
// 否则白天会把「只挂徽章」那条当成折扣生效。
package upstream

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// promoTZ 促销时段时区。上游 schedule.timezone 写 "Asia/Shanghai"；容器可能没有
// /usr/share/zoneinfo（LoadLocation 会失败），故固定回落 UTC+8 —— 与 scheduler 的
// scheduleTZ 同口径，不引入 tzdata 依赖（不 import time/tzdata 膨胀二进制）。
var promoTZ = time.FixedZone("CST", 8*3600)

// ModelPromotion 单个模型的生效营销/折扣活动（展示用，不参与选号/计费）。
//
// Active 不落字段：Start/End 是字符串，生效与否必须**每次请求现算**——
// /v1/models 有 1h 缓存，把 bool 缓存进去会导致到了 23:00 仍报「未生效」最多 1 小时。
type ModelPromotion struct {
	ID    string // 促销 id（如 glm-52-night-discount-202607）
	Label string // 徽章文字（如 夜间折扣 / 限时免费 / 错峰使用）
	Kind  string // 活动类型（实测恒为 "discount"）

	// 日窗：Start/End 形如 "23:00"/"7:50"（上游原样，可能跨午夜）。
	// 日期区间型促销这两个字段为空串。
	Start string
	End   string

	// 日期区间：ValidFrom/ValidUntil 为 RFC3339 原文（如 2026-10-12T00:00:00+08:00）。
	// 日窗型促销这两个字段为空串。展示层只需日期部分，故另存人读短串。
	ValidFrom  string
	ValidUntil string
	FromShort  string // "10-12"（取自 ValidFrom，展示用）
	UntilShort string // "10-25"（取自 ValidUntil，展示用）

	Factor float64 // 折扣因子（0 = 免费，0.5 = 5折）；无 discount 时为 0 且 Credit 为空
	Credit string  // 折后倍率原文（"0x" / "0.50x"）；空 = 该条未声明折扣（只挂徽章）
	Text   string  // hover.textZh 上游原文说明（结构化表达不了的细节在此）
}

// WindowText 人读时段串；无有效时段（全天窗口 00:00–23:59）返回空串——
// 「错峰使用」这类真含义只在 Text 原文里，编一个「全天」反而误导。
//
// 日窗 → "23:00–07:50"（补零对齐）；日期区间 → "10-12 至 10-25"；两者都有时
// 以日窗为准（日窗是用户每天要看的，日期区间进 Text 兜底）。
func (p ModelPromotion) WindowText() string {
	if p == (ModelPromotion{}) {
		return ""
	}
	if w := p.dailyWindowText(); w != "" {
		return w
	}
	return p.dateRangeText()
}

// dailyWindowText 日窗串（"23:00–07:50"）；全天（00:00–23:59）或无日窗 → 空串。
func (p ModelPromotion) dailyWindowText() string {
	s, e := normalizeClock(p.Start), normalizeClock(p.End)
	if s == "" || e == "" {
		return ""
	}
	if s == "00:00" && e == "23:59" {
		return "" // 全天窗口：无信息量，交原文兜底
	}
	return s + "–" + e
}

// dateRangeText 日期区间串（"10-12 至 10-25"）；缺任一 → 空串。
func (p ModelPromotion) dateRangeText() string {
	if p.FromShort == "" || p.UntilShort == "" {
		return ""
	}
	return p.FromShort + " 至 " + p.UntilShort
}

// Active 报告该促销在 now 时刻是否生效（日窗与日期区间是「与」关系，都要满足）。
//
// 日窗按跨午夜处理：start<=end（如 09:00–18:00）为同一天内区间；
// start>end（如 23:00–7:50）表示跨午夜，命中条件为 now>=start 或 now<end。
// 无日窗（日期区间型）则只看日期区间；两者都无 → 恒 true（无限制条件）。
func (p ModelPromotion) Active(now time.Time) bool {
	if p == (ModelPromotion{}) {
		return false
	}
	return p.inDailyWindow(now) && p.inDateRange(now)
}

// inDailyWindow 日窗判定。无 Start/End（日期区间型）→ 恒真（不受日内时段限制）。
func (p ModelPromotion) inDailyWindow(now time.Time) bool {
	sm, sok := parseClock(p.Start)
	em, eok := parseClock(p.End)
	if !sok || !eok {
		return true
	}
	m := now.In(promoTZ).Hour()*60 + now.In(promoTZ).Minute()
	if sm <= em {
		return m >= sm && m < em
	}
	// 跨午夜：23:00–7:50 → 23:00 之后 或 7:50 之前。
	return m >= sm || m < em
}

// IsFree 报告该促销是否为「免费」形态（折后 0x）。
// 与 scheduler creditNote 的免费判定同口径：Factor==0 且 Credit ∈ {0x, x0.00, 0}。
// 无 discount 的条目（Credit 空）不算免费——那类只是白天挂徽章。
func (p ModelPromotion) IsFree() bool {
	if p.Factor != 0 {
		return false
	}
	switch p.Credit {
	case "0x", "x0.00", "0":
		return true
	}
	return false
}

// NextReset 返回该促销的免费额度复位点（free_guard 台账的到期时刻）：
//   - 有日窗（如夜间免费 23:00–8:00）→ 当前/下一个窗口的 End 时刻。
//     额度是窗口级配额：窗口内耗尽 → 本窗结束后下个窗口重新有额度。
//   - 无日窗（全天型，如限时免费「每日赠送免费额度」）→ 次日 00:00（promoTZ）。
//     全天型配额按自然日重置，文案明确「每日赠送」。
//
// 返回值总在 now 之后（调用方据此计算 until，零值/过去值无意义）。
func (p ModelPromotion) NextReset(now time.Time) time.Time {
	t := now.In(promoTZ)
	if sm, sok := parseClock(p.Start); sok {
		if em, eok := parseClock(p.End); eok {
			// 日窗：找 now 所在窗口的 End。
			// 构造「今天 Start」「今天 End」两个候选墙钟，按跨午夜语义推。
			dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, promoTZ)
			startToday := dayStart.Add(time.Duration(sm) * time.Minute)
			endToday := dayStart.Add(time.Duration(em) * time.Minute)
			if sm <= em {
				// 同日窗（09:00–18:00）：now 在窗内 → End=今天 end；在窗前 → 今天 end 也未来；
				// 在窗后 → 明天 end。
				if !endToday.After(t) {
					return endToday.Add(24 * time.Hour)
				}
				return endToday
			}
			// 跨午夜窗（23:00–07:50）：now>=start → End=明天 end；now<end → End=今天 end。
			if !t.Before(startToday) {
				return endToday.Add(24 * time.Hour)
			}
			// now < end（凌晨段在窗内）：End=今天 end（必定未来，因 now<end）。
			if endToday.After(t) {
				return endToday
			}
			// now 处于「end 之后、start 之前」的空档（如 12:00）：本窗未开，
			// 额度复位点无意义——返回今天 start（下一个窗开始时刻）。
			return startToday
		}
	}
	// 全天型：次日 00:00（promoTZ）。
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, promoTZ)
	return dayStart.Add(24 * time.Hour)
}

// inDateRange 日期区间判定（半开区间 [from, until)）。无区间 → 恒真。
func (p ModelPromotion) inDateRange(now time.Time) bool {
	from, fromOK := parsePromoTime(p.ValidFrom)
	until, untilOK := parsePromoTime(p.ValidUntil)
	if !fromOK && !untilOK {
		return true
	}
	t := now.In(promoTZ)
	if fromOK && t.Before(from) {
		return false
	}
	if untilOK && !t.Before(until) {
		return false
	}
	return true
}

// modelPromotionEntry /v3/config data.modelPromotions[] 的单条解析形态。
// 只解析展示所需字段；displayMode/color/action 等按「不透出」原则忽略。
type modelPromotionEntry struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		Credit string  `json:"discountedCredits"`
		Factor float64 `json:"factor"`
	} `json:"discount"`
	Hover struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule struct {
		Timezone   string `json:"timezone"`
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
		Daily      []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"daily"`
	} `json:"schedule"`
}

// toPromotion 把解析条目转成 ModelPromotion（单一映射事实来源）。
// 日窗取 daily[0]（实测单元素；多元素时取第一个——上游未见多窗口场景）。
func (e modelPromotionEntry) toPromotion() ModelPromotion {
	p := ModelPromotion{
		ID:    e.ID,
		Label: e.Badge.Label,
		Kind:  e.Kind,
		Text:  e.Hover.TextZh,
	}
	if len(e.Schedule.Daily) > 0 {
		p.Start = e.Schedule.Daily[0].Start
		p.End = e.Schedule.Daily[0].End
	}
	p.ValidFrom = e.Schedule.ValidFrom
	p.ValidUntil = e.Schedule.ValidUntil
	p.FromShort = shortDate(e.Schedule.ValidFrom)
	p.UntilShort = shortDate(e.Schedule.ValidUntil)
	if e.Discount != nil {
		p.Credit = e.Discount.Credit
		p.Factor = e.Discount.Factor
	}
	return p
}

// promotionFor 为模型 id 挑选当前应展示的促销。
//
// 挑选口径（2026-10-12 实测 glm-5.2 有两条促销，必须区分）：
//  1. 候选 = enabled && modelID ∈ modelIds && 日期区间含 now；
//  2. 候选中**带 discount 的优先**（不带 discount 的那条只是白天挂徽章、无实际折扣），
//     其次 priority 高者——否则白天会误判成折扣生效；
//  3. 无候选 → nil。
//
// 返回指针指向条目副本（调用方只读，不与解析数组共享）。
func promotionFor(modelID string, entries []modelPromotionEntry, now time.Time) *ModelPromotion {
	type cand struct {
		e       modelPromotionEntry
		hasDisc bool
		promo   ModelPromotion
	}
	var cands []cand
	for _, e := range entries {
		if !e.Enabled || !containsModelID(e.ModelIDs, modelID) {
			continue
		}
		p := e.toPromotion()
		if !p.inDateRange(now) {
			continue // 已过期/未开始（如 space-bunny 限时折扣 10-02~10-08）
		}
		cands = append(cands, cand{e: e, hasDisc: e.Discount != nil, promo: p})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		// 带 discount 优先（否则白天会选中「只挂徽章」那条，把 5 折窗口信息丢掉）；
		// 同带/同不带时当前生效者优先；再同则 priority 高者。
		if cands[i].hasDisc != cands[j].hasDisc {
			return cands[i].hasDisc
		}
		if ai, aj := cands[i].promo.Active(now), cands[j].promo.Active(now); ai != aj {
			return ai
		}
		return cands[i].e.Priority > cands[j].e.Priority
	})
	best := cands[0].promo
	return &best
}

// applyPromotions 按 modelIds 把促销挂到模型目录上（原地修改 map 内的副本后写回）。
// 无促销条目（global 域实测 0 条）→ 零改动、零分配。
func applyPromotions(out map[string]ModelInfo, entries []modelPromotionEntry, now time.Time) {
	if len(entries) == 0 || len(out) == 0 {
		return
	}
	for id, mi := range out {
		p := promotionFor(id, entries, now)
		if p == nil {
			continue
		}
		mi.Promotion = p
		out[id] = mi
	}
}

// containsModelID 报告 modelID 是否在名单内（精确匹配，大小写敏感）。
func containsModelID(ids []string, modelID string) bool {
	for _, id := range ids {
		if id == modelID {
			return true
		}
	}
	return false
}

// parseClock 解析 "23:00"/"7:50"/"23:59" 为当日分钟数。非法 → (0, false)。
func parseClock(s string) (int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return 0, false
	}
	h, err := strconv.Atoi(strings.TrimSpace(s[:i]))
	if err != nil {
		return 0, false
	}
	m, err := strconv.Atoi(strings.TrimSpace(s[i+1:]))
	if err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// normalizeClock 把 "7:50" 规整为 "07:50"（展示对齐）；非法 → 空串。
func normalizeClock(s string) string {
	h, m := 0, 0
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return ""
	}
	if v, err := strconv.Atoi(strings.TrimSpace(s[:i])); err == nil {
		h = v
	} else {
		return ""
	}
	if v, err := strconv.Atoi(strings.TrimSpace(s[i+1:])); err == nil {
		m = v
	} else {
		return ""
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return ""
	}
	return itoa2(h) + ":" + itoa2(m)
}

// shortDate 从 RFC3339 取 "10-12" 短日期；解析失败 → 空串。
func shortDate(s string) string {
	t, ok := parsePromoTime(s)
	if !ok {
		return ""
	}
	return itoa2(int(t.Month())) + "-" + itoa2(t.Day())
}

// parsePromoTime 解析促销的 RFC3339 时间戳；空串/非法 → (zero, false)。
func parsePromoTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// itoa2 两位补零（月/日/时/分展示对齐）。
func itoa2(n int) string {
	if n < 0 || n > 99 {
		return "00"
	}
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// BadgeFallbackPromotion 无结构化促销数据时的徽章兜底窗口。
//
// 为什么需要：促销按 modelIds 匹配，而企业端点的徽章是按**模型 tag** 打的，
// 两者口径不同源——实测 hy4-preview 的企业端点 tag 是 badge:夜间免费，但 v3
// 促销的 modelIds 是 "hy4-preview-f"（另一个 id），纯 modelId 匹配必然漏掉它。
// 没有兜底的话，hy4-preview 会从此前「夜间免费」退化成「无时段信息」。
//
// 只在 ModelInfo.Promotion == nil 时生效：上游一旦下发了结构化 schedule，
// 立即以结构化为准（兜底值是人工观测口径，权威度低）。
//
// Credit/Factor/Text 留空：兜底不编造**折扣力度**（夜间折扣的具体折率未知）。
// 例外：「夜间免费」字面即免费，故声明 Credit="0x"——否则展示层会把它当成
// 「有窗口但不免费」的折扣，反而显示原价（2026-10-12 修）。
func BadgeFallbackPromotion(badge string) *ModelPromotion {
	switch {
	case strings.Contains(badge, "夜间免费"):
		return &ModelPromotion{Label: badge, Start: "23:00", End: "8:00", Credit: "0x"}
	case strings.Contains(badge, "夜间折扣"):
		return &ModelPromotion{Label: badge, Start: "23:00", End: "7:50"}
	case strings.Contains(badge, "限时免费"):
		// 限时免费是日期区间型（上游未给日窗），兜底 = 全时段免费，无窗口文案。
		return &ModelPromotion{Label: badge, Factor: 0, Credit: "0x"}
	}
	return nil
}

// EffectivePromotion 取模型当前应展示的促销：结构化优先，无则回落徽章兜底。
// 两者皆无 → nil（展示层按无活动处理）。
//
// 这是三个出口（/v1/models、面板、腾讯文档）共用的唯一取值口径，避免各写一份
// 「先结构化后兜底」的判断而漂移。
func (mi ModelInfo) EffectivePromotion() *ModelPromotion {
	if mi.Promotion != nil {
		return mi.Promotion
	}
	return BadgeFallbackPromotion(mi.Badge)
}

// PromotionJSON 促销的 API/面板透出形态（三个出口共用，杜绝各写一份字段集而漂移）。
//
// 字段按「空值省略」：空串/0 值不写进 map（与 /v1/models 既有口径一致，不编造）。
// Active 每次调用现算（传 now），绝不缓存进 map——/v1/models 有 1h 缓存，
// 缓存 bool 会让时段切换后最多 1 小时仍报旧值。
func PromotionJSON(p *ModelPromotion, now time.Time) map[string]any {
	if p == nil {
		return nil
	}
	m := map[string]any{}
	if p.ID != "" {
		m["id"] = p.ID
	}
	if p.Label != "" {
		m["label"] = p.Label
	}
	if p.Kind != "" {
		m["kind"] = p.Kind
	}
	if p.Start != "" {
		m["start"] = p.Start
	}
	if p.End != "" {
		m["end"] = p.End
	}
	if w := p.WindowText(); w != "" {
		m["window"] = w // 人读时段（"23:00–07:50" / "10-12 至 10-25"）
	}
	if p.FromShort != "" {
		m["valid_from"] = p.FromShort
	}
	if p.UntilShort != "" {
		m["valid_until"] = p.UntilShort
	}
	if p.Credit != "" {
		m["credit"] = p.Credit
		m["factor"] = p.Factor // 有 Credit 才带 Factor（无 discount 的条目不编造）
	}
	if p.Text != "" {
		m["text"] = p.Text
	}
	m["active"] = p.Active(now)
	return m
}

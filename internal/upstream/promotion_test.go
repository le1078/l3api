package upstream

import (
	"encoding/json"
	"testing"
	"time"
)

// promoNow 构造 CST 时刻（测试基准，避免依赖本机时区）。
func promoNow(h, m int) time.Time {
	return time.Date(2026, 10, 12, h, m, 0, 0, promoTZ) // 2026-10-12 是周一
}

// TestParseModelPromotions 覆盖上游实测的三种 schedule 形态：
//   - 日窗（可跨午夜）：glm-5.2 夜间折扣 23:00–7:50
//   - 日期区间：step-5-preview 独家限免 10-12~10-25
//   - 全天 + 原文说明：deepseek-v4-flash 错峰使用 00:00–23:59
func TestParseModelPromotions(t *testing.T) {
	raw := `{"code":0,"data":{
      "models":[{"id":"glm-5.2"},{"id":"step-5-preview"},{"id":"deepseek-v4-flash"},{"id":"plain"}],
      "modelPromotions":[
        {"id":"glm-52-night-discount-202607","kind":"discount","enabled":true,"priority":100,
         "modelIds":["glm-5.2"],
         "badge":{"label":"夜间折扣","color":"#1E90FF"},
         "discount":{"discountedCredits":"0.50x","displayMode":"strikethrough","factor":0.5},
         "hover":{"textZh":"每晚 23:00—次日 8:00 积分限时立减，错峰用更省"},
         "schedule":{"daily":[{"start":"23:00","end":"7:50"}],"timezone":"Asia/Shanghai"}},
        {"id":"step-5-preview-fire-trial-202610","kind":"discount","enabled":true,"priority":100,
         "modelIds":["step-5-preview"],
         "badge":{"label":"独家限免","color":"#FF0000"},
         "discount":{"discountedCredits":"0x","displayMode":"replace","factor":0},
         "hover":{"textZh":"10月12日~10月25日，独家官方合作享限时免费体验。"},
         "schedule":{"timezone":"Asia/Shanghai",
           "validFrom":"2026-10-12T00:00:00+08:00","validUntil":"2026-10-25T00:00:00+08:00"}},
        {"id":"deepseek-v4-flash-peak-202608","kind":"discount","enabled":true,"priority":100,
         "modelIds":["deepseek-v4-flash","deepseek-v4-flash-ioa"],
         "badge":{"label":"错峰使用","color":"#1E90FF"},
         "hover":{"textZh":"周一至周五 09:00–12:00、14:00–18:00 属高峰原价，非高峰期积分5折"},
         "schedule":{"daily":[{"start":"00:00","end":"23:59"}],"timezone":"Asia/Shanghai"}}
      ]}}`
	var env struct {
		Data struct {
			Models          []dynModelEntry       `json:"models"`
			ModelPromotions []modelPromotionEntry `json:"modelPromotions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.ModelPromotions) != 3 {
		t.Fatalf("promos=%d want 3", len(env.Data.ModelPromotions))
	}
	now := promoNow(15, 0) // 白天 15:00
	out := map[string]ModelInfo{}
	for _, m := range env.Data.Models {
		out[m.ID] = m.modelInfo()
	}
	applyPromotions(out, env.Data.ModelPromotions, now)

	// 日窗型：窗口准确（23:00–07:50，不是硬编码的 8:00），白天不生效。
	g := out["glm-5.2"]
	if g.Promotion == nil {
		t.Fatal("glm-5.2: no promotion attached")
	}
	if got, want := g.Promotion.WindowText(), "23:00–07:50"; got != want {
		t.Errorf("glm-5.2 window=%q want %q", got, want)
	}
	if got, want := g.Promotion.Credit, "0.50x"; got != want {
		t.Errorf("glm-5.2 credit=%q want %q", got, want)
	}
	if g.Promotion.Factor != 0.5 {
		t.Errorf("glm-5.2 factor=%v want 0.5", g.Promotion.Factor)
	}
	if g.Promotion.Active(now) {
		t.Error("glm-5.2: 15:00 should NOT be active (night window 23:00–07:50)")
	}
	if !g.Promotion.Active(promoNow(23, 30)) {
		t.Error("glm-5.2: 23:30 should be active")
	}
	if !g.Promotion.Active(promoNow(3, 0)) {
		t.Error("glm-5.2: 03:00 should be active (crosses midnight)")
	}
	if g.Promotion.Active(promoNow(7, 50)) {
		t.Error("glm-5.2: 07:50 should NOT be active (end is exclusive)")
	}

	// 日期区间型：窗口为日期串，区间内生效，区间外不生效。
	s := out["step-5-preview"]
	if s.Promotion == nil {
		t.Fatal("step-5-preview: no promotion attached")
	}
	if got, want := s.Promotion.WindowText(), "10-12 至 10-25"; got != want {
		t.Errorf("step-5-preview window=%q want %q", got, want)
	}
	if !s.Promotion.Active(promoNow(15, 0)) {
		t.Error("step-5-preview: 10-12 should be active")
	}
	if s.Promotion.Active(time.Date(2026, 10, 26, 10, 0, 0, 0, promoTZ)) {
		t.Error("step-5-preview: 10-26 should NOT be active (validUntil 10-25)")
	}
	if s.Promotion.Active(time.Date(2026, 10, 11, 10, 0, 0, 0, promoTZ)) {
		t.Error("step-5-preview: 10-11 should NOT be active (validFrom 10-12)")
	}
	if !promoFree(s.Promotion) {
		t.Error("step-5-preview: credit 0x should be free")
	}

	// 全天窗口型：WindowText 必须为空（避免显示无意义的「全天」），原文保留。
	d := out["deepseek-v4-flash"]
	if d.Promotion == nil {
		t.Fatal("deepseek-v4-flash: no promotion attached")
	}
	if got := d.Promotion.WindowText(); got != "" {
		t.Errorf("deepseek-v4-flash window=%q want empty (all-day has no info)", got)
	}
	if d.Promotion.Text == "" {
		t.Error("deepseek-v4-flash: Text (hover.textZh) should be kept for tooltip")
	}

	// 未命中任何促销的模型：Promotion 保持 nil。
	if out["plain"].Promotion != nil {
		t.Error("plain: should have no promotion")
	}
}

// promoFree 测试辅助：与展示层同口径（免费判定）。
func promoFree(p *ModelPromotion) bool {
	if p == nil {
		return false
	}
	return p.Factor == 0 && (p.Credit == "0x" || p.Credit == "x0.00" || p.Credit == "0")
}

// TestPromotionForPrefersDiscount glm-5.2 实测有两条促销：prio 100 带 discount 的
// 夜间窗、prio 50 白天只挂徽章无折扣。必须选带 discount 的那条，否则白天会把
// 「只挂徽章」当成生效折扣——这是本功能的核心正确性约束。
func TestPromotionForPrefersDiscount(t *testing.T) {
	entries := []modelPromotionEntry{
		{ID: "night", Enabled: true, Priority: 100},
		{ID: "daytime-badge", Enabled: true, Priority: 50},
	}
	entries[0].ModelIDs = []string{"glm-5.2"}
	entries[0].Badge.Label = "夜间折扣"
	entries[0].Discount = &struct {
		Credit string  `json:"discountedCredits"`
		Factor float64 `json:"factor"`
	}{Credit: "0.50x", Factor: 0.5}
	entries[0].Schedule.Daily = append(entries[0].Schedule.Daily, struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}{Start: "23:00", End: "7:50"})
	entries[1].ModelIDs = []string{"glm-5.2"}
	entries[1].Badge.Label = "夜间折扣"
	entries[1].Schedule.Daily = append(entries[1].Schedule.Daily, struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}{Start: "7:50", End: "23:00"})

	// 白天：两条都不带「生效」时，仍应选带 discount 的（保留真实的折扣窗口信息）。
	p := promotionFor("glm-5.2", entries, promoNow(15, 0))
	if p == nil {
		t.Fatal("no promotion selected")
	}
	if p.ID != "night" {
		t.Errorf("daytime: selected %q want %q (discount entry must win)", p.ID, "night")
	}
	if p.Credit != "0.50x" {
		t.Errorf("selected credit=%q want 0.50x", p.Credit)
	}
	// 夜间：带 discount 且生效，同样选中它。
	if p = promotionFor("glm-5.2", entries, promoNow(23, 30)); p == nil || p.ID != "night" {
		t.Errorf("night: selected %v want night", p)
	}
	// 不匹配的模型 → nil。
	if p = promotionFor("other", entries, promoNow(15, 0)); p != nil {
		t.Errorf("other model: got %v want nil", p)
	}
}

// TestPromotionForSkipsDisabledAndExpired enabled=false 与已过期条目必须被排除。
func TestPromotionForSkipsDisabledAndExpired(t *testing.T) {
	var disabled modelPromotionEntry
	disabled.ID = "disabled"
	disabled.Enabled = false
	disabled.ModelIDs = []string{"a"}
	disabled.Badge.Label = "限时免费"

	var expired modelPromotionEntry
	expired.ID = "expired" // space-bunny 实测：10-02~10-08，早已过期
	expired.Enabled = true
	expired.Priority = 100
	expired.ModelIDs = []string{"b"}
	expired.Badge.Label = "限时折扣"
	expired.Schedule.ValidFrom = "2026-10-02T00:00:00+08:00"
	expired.Schedule.ValidUntil = "2026-10-08T00:00:00+08:00"

	now := promoNow(15, 0)
	if p := promotionFor("a", []modelPromotionEntry{disabled}, now); p != nil {
		t.Errorf("disabled entry must be skipped; got %v", p)
	}
	if p := promotionFor("b", []modelPromotionEntry{expired}, now); p != nil {
		t.Errorf("expired entry must be skipped; got %v", p)
	}
}

// TestPromotionActiveWindow 日窗边界与跨午夜判定。
func TestPromotionActiveWindow(t *testing.T) {
	cases := []struct {
		name  string
		start string
		end   string
		hour  int
		min   int
		want  bool
	}{
		{"night-start", "23:00", "7:50", 23, 0, true},
		{"night-before-end", "23:00", "7:50", 7, 49, true},
		{"night-at-end-exclusive", "23:00", "7:50", 7, 50, false},
		{"night-mid", "23:00", "7:50", 3, 0, true},
		{"night-just-before", "23:00", "7:50", 22, 59, false},
		{"day-window-inside", "09:00", "18:00", 12, 0, true},
		{"day-window-before", "09:00", "18:00", 8, 59, false},
		{"day-window-after", "09:00", "18:00", 18, 0, false},
		{"all-day", "00:00", "23:59", 15, 0, true},
		// 日期区间型（无日窗）不受日内时段限制；Label 非空以区别于零值促销。
		{"no-window", "", "", 15, 0, true},
		{"malformed", "xx", "yy", 15, 0, true},
	}
	for _, c := range cases {
		// Label 恒非空：零值 ModelPromotion 表示「无活动」（Active 恒 false），
		// 与本测试要覆盖的「有活动但无日窗」是两种语义。
		p := ModelPromotion{Label: "x", Start: c.start, End: c.end}
		if got := p.Active(promoNow(c.hour, c.min)); got != c.want {
			t.Errorf("%s: Active(%02d:%02d)=%v want %v", c.name, c.hour, c.min, got, c.want)
		}
	}
}

// TestBadgeFallbackPromotion 徽章兜底只在无结构化促销时生效
// （hy4-preview 的企业端点 tag 是 夜间免费，但 v3 促销 modelIds 是 hy4-preview-f，
// 纯 modelId 匹配必然漏掉它——没有兜底会退化成「无时段信息」）。
func TestBadgeFallbackPromotion(t *testing.T) {
	// 企业端点给的徽章、无结构化促销 → 兜底窗口生效。
	mi := ModelInfo{ID: "hy4-preview", Badge: "夜间免费"}
	p := mi.EffectivePromotion()
	if p == nil {
		t.Fatal("hy4-preview: fallback must kick in (v3 modelIds mismatch)")
	}
	if got, want := p.WindowText(), "23:00–08:00"; got != want {
		t.Errorf("hy4-preview window=%q want %q", got, want)
	}
	// 「夜间免费」字面即免费：兜底必须声明免费，否则展示层会当成「有窗口但不免费」
	// 的折扣、反而显示原价（2026-10-12 修）。
	if !promoFree(p) {
		t.Errorf("hy4-preview: 夜间免费 fallback must be marked free; credit=%q factor=%v", p.Credit, p.Factor)
	}
	// 「夜间折扣」折率未知：不得编造免费（否则白天会显示成免费）。
	nd := BadgeFallbackPromotion("夜间折扣")
	if nd == nil || promoFree(nd) {
		t.Errorf("夜间折扣 fallback must NOT be marked free (discount rate unknown); got %v", nd)
	}
	if !p.Active(promoNow(23, 30)) || p.Active(promoNow(15, 0)) {
		t.Errorf("hy4-preview: 23:30 should be active, 15:00 should not")
	}
	// 结构化促销优先于兜底（上游一旦下发即以结构化为准）。
	structured := &ModelPromotion{Label: "夜间折扣", Start: "23:00", End: "7:50", Credit: "0.50x", Factor: 0.5}
	mi2 := ModelInfo{ID: "glm-5.2", Badge: "夜间折扣", Promotion: structured}
	if got := mi2.EffectivePromotion(); got != structured {
		t.Errorf("structured promotion must win over badge fallback; got %v", got)
	}
	// 无徽章无促销 → nil。
	if got := (ModelInfo{ID: "plain"}).EffectivePromotion(); got != nil {
		t.Errorf("no badge/no promo: got %v want nil", got)
	}
	// 未知徽章 → nil（不编造）。
	if got := (ModelInfo{ID: "x", Badge: "订阅优先"}).EffectivePromotion(); got != nil {
		t.Errorf("unknown badge: got %v want nil", got)
	}
}

// TestPromotionJSON 三个出口共用的透出形态：空值省略 + active 现算。
func TestPromotionJSON(t *testing.T) {
	p := &ModelPromotion{
		ID: "glm-52-night-discount-202607", Label: "夜间折扣", Kind: "discount",
		Start: "23:00", End: "7:50", Credit: "0.50x", Factor: 0.5,
		Text: "每晚 23:00—次日 8:00 积分限时立减",
	}
	// active 随 now 变化（不能缓存进 map）。
	if got := PromotionJSON(p, promoNow(23, 30))["active"]; got != true {
		t.Errorf("23:30 active=%v want true", got)
	}
	if got := PromotionJSON(p, promoNow(15, 0))["active"]; got != false {
		t.Errorf("15:00 active=%v want false", got)
	}
	m := PromotionJSON(p, promoNow(23, 30))
	if got, want := m["window"], "23:00–07:50"; got != want {
		t.Errorf("window=%v want %v", got, want)
	}
	if got, want := m["credit"], "0.50x"; got != want {
		t.Errorf("credit=%v want %v", got, want)
	}
	if _, ok := m["valid_from"]; ok {
		t.Error("valid_from must be omitted when empty (no fabrication)")
	}
	// nil → nil（调用方整字段省略）。
	if got := PromotionJSON(nil, promoNow(15, 0)); got != nil {
		t.Errorf("nil promo: got %v want nil", got)
	}
	// 无 discount 的条目：不编造 factor。
	bare := &ModelPromotion{Label: "错峰使用", Start: "00:00", End: "23:59"}
	bm := PromotionJSON(bare, promoNow(15, 0))
	if _, ok := bm["factor"]; ok {
		t.Error("factor must be omitted when no discount declared")
	}
	if _, ok := bm["window"]; ok {
		t.Error("all-day window must be omitted")
	}
}

// TestApplyPromotionsNoAlloc 无促销条目（global 域实测 0 条）→ 零改动。
func TestApplyPromotionsNoAlloc(t *testing.T) {
	out := map[string]ModelInfo{"a": {ID: "a", Badge: "夜间免费"}}
	applyPromotions(out, nil, promoNow(15, 0))
	if out["a"].Promotion != nil {
		t.Error("no promo entries: Promotion must stay nil")
	}
	if len(out) != 1 {
		t.Errorf("len=%d want 1", len(out))
	}
}

// TestIsFree 免费判定口径（free_guard 用）：Factor==0 且 Credit ∈ {0x, x0.00, "0"}。
// 无 discount（Credit 空）的徽章条目不算免费；Factor 非 0（折扣）不算免费。
func TestIsFree(t *testing.T) {
	cases := []struct {
		name   string
		p      ModelPromotion
		want   bool
	}{
		{"free-0x", ModelPromotion{Label: "限时免费", Factor: 0, Credit: "0x"}, true},
		{"free-x0.00", ModelPromotion{Label: "限时免费", Factor: 0, Credit: "x0.00"}, true},
		{"free-0", ModelPromotion{Label: "限时免费", Factor: 0, Credit: "0"}, true},
		{"badge-only", ModelPromotion{Label: "错峰使用"}, false},
		{"discount-half", ModelPromotion{Label: "夜间折扣", Factor: 0.5, Credit: "0.50x"}, false},
	}
	for _, c := range cases {
		if got := c.p.IsFree(); got != c.want {
			t.Errorf("%s: IsFree=%v want %v", c.name, got, c.want)
		}
	}
}

// TestNextReset 复位点语义（free_guard 台账到期时刻）：
//   - 日窗（夜间免费 23:00–7:50，跨午夜）：窗内 → 本窗 End；
//     窗后（空档）→ 下个窗的 Start（复位点无意义，用下窗开始兜底）。
//   - 全天型（限时免费「每日赠送」）：次日 00:00 CST。
// 返回值必须总在 now 之后（台账 until 语义）。
func TestNextReset(t *testing.T) {
	night := ModelPromotion{Label: "夜间免费", Start: "23:00", End: "7:50"}
	cases := []struct {
		name  string
		p     ModelPromotion
		h, m  int
		wantH int // 期望复位点（次日用 +24 表示）
		wantM int
	}{
		{"night-in-window-evening", night, 23, 30, 7 + 24, 50},  // 23:30 → 次日 07:50
		{"night-in-window-dawn", night, 3, 0, 7, 50},            // 03:00 → 当天 07:50
		{"night-gap-midday", night, 12, 0, 23, 0},               // 12:00 空档 → 今天 23:00（下窗开始）
		{"all-day", ModelPromotion{Label: "限时免费"}, 15, 0, 0 + 24, 0}, // 15:00 → 次日 00:00
	}
	for _, c := range cases {
		got := c.p.NextReset(promoNow(c.h, c.m))
		wantH, wantM := c.wantH, c.wantM
		// 次日时刻用 24+ 表示：换算回 [0,24) 并校验日序。
		gh := got.In(promoTZ).Hour() + 0
		if wantH >= 24 {
			wantH -= 24
			// 次日：got 的日应比基准日（10-12）多一天
			if got.In(promoTZ).Day() != 13 {
				t.Errorf("%s: NextReset day=%d want 13 (got %v)", c.name, got.In(promoTZ).Day(), got)
			}
		} else if got.In(promoTZ).Day() != 12 {
			t.Errorf("%s: NextReset day=%d want 12 (got %v)", c.name, got.In(promoTZ).Day(), got)
		}
		if gh != wantH || got.In(promoTZ).Minute() != wantM {
			t.Errorf("%s: NextReset=%02d:%02d want %02d:%02d", c.name, gh, got.In(promoTZ).Minute(), wantH, wantM)
		}
		if !got.After(promoNow(c.h, c.m)) {
			t.Errorf("%s: NextReset 必须在 now 之后（got %v）", c.name, got)
		}
	}
}

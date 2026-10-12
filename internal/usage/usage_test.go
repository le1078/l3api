package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil, nil)
	if after.Totals.Requests != 3 || after.Totals.PromptTokens != 15 {
		t.Fatalf("折叠后 totals = %d/%d, want 3/15", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 2 || after.Series[0].Scope != "day" || after.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", after.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(24, nil, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// Snapshot 把小时窗口外的细粒度并入日点，时序不出现空洞。
func TestSnapshotStitching(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外 → 日点
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内 → 小时点
	s := r.Snapshot(24, nil, nil)
	if len(s.Series) != 2 || s.Series[0].Scope != "day" || s.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v", s.Series)
	}
	if s.Series[0].PromptTokens != 5 || s.Series[1].PromptTokens != 3 {
		t.Fatalf("series tokens = %d/%d, want 5/3", s.Series[0].PromptTokens, s.Series[1].PromptTokens)
	}
}

// 逐维度时序（model_series / key_series）：同小时不同模型/key 拆成不同系列，
// 缺格不造 0 点；窗口外小时桶折叠出的旧日点与 Series 同口径（前端按窗口过滤）。
func TestDimSeries(t *testing.T) {
	r := New("")
	now := time.Now()
	r.AddKey(now, "cn", "u1", "m1", "k1", Delta{TotalTokens: 100, HasTotal: true}, true)
	r.AddKey(now, "cn", "u1", "m2", "k1", Delta{TotalTokens: 40, HasTotal: true}, true)
	r.AddKey(now, "cn", "u2", "m1", "", Delta{TotalTokens: 7, HasTotal: true}, true)                      // 无 key 桶不进 key_series
	r.AddKey(now.Add(-48*time.Hour), "cn", "u1", "m1", "k1", Delta{TotalTokens: 9, HasTotal: true}, true) // 窗口外 → 只进 Series 日点

	s := r.Snapshot(24, nil, nil)
	ms := s.ModelSeries
	// m1：当前小时点 107 + 48h 前折叠出的日点（与 Series 同口径：日点允许存在，
	// 但必须落在窗口起点之前——画在窗口内的只有小时点）。
	if len(ms["m1"]) != 2 {
		t.Fatalf("model m1 series = %+v, want 日点+小时点", ms["m1"])
	}
	winFrom, err := time.ParseInLocation(hourLayout, s.WindowFrom, time.Local)
	if err != nil {
		t.Fatalf("window_from 解析失败: %v", err)
	}
	var hourPt, dayPt *Point
	for i := range ms["m1"] {
		switch ms["m1"][i].Scope {
		case "hour":
			hourPt = &ms["m1"][i]
		case "day":
			dayPt = &ms["m1"][i]
		}
	}
	if hourPt == nil || hourPt.TotalTokens != 107 {
		t.Fatalf("m1 小时点 = %+v, want 107", hourPt)
	}
	if dayPt == nil || dayPt.TotalTokens != 9 {
		t.Fatalf("m1 日点 = %+v, want 9", dayPt)
	}
	if ds, err := time.ParseInLocation(dayLayout, dayPt.T, time.Local); err != nil || !ds.Before(winFrom) {
		t.Fatalf("m1 日点 %q 应早于窗口起点 %q", dayPt.T, s.WindowFrom)
	}
	if len(ms["m2"]) != 1 || ms["m2"][0].TotalTokens != 40 {
		t.Fatalf("model m2 series = %+v, want 40", ms["m2"])
	}
	if _, ok := ms["其他"]; ok {
		t.Fatalf("模型数未超上限时不应有「其他」: %+v", ms)
	}
	ks := s.KeySeries
	// k1 与 m1 同形：旧日点 + 当前小时点；末个（最新）时间片是小时点 140。
	if len(ks["k1"]) != 2 || ks["k1"][1].Scope != "hour" || ks["k1"][1].TotalTokens != 140 {
		t.Fatalf("key k1 series = %+v, want 末个时间片 140", ks["k1"])
	}
	if _, ok := ks[""]; ok {
		t.Fatalf("无 key 桶不应出现在 key_series: %+v", ks)
	}
	// Series 仍含窗口外旧日点（5+... 历史桶折叠口径不变），逐维度序列不含。
	if len(s.Series) != 2 || s.Series[0].Scope != "day" || s.Series[0].TotalTokens != 9 {
		t.Fatalf("series = %+v, want 旧日点 9 + 当前小时点", s.Series)
	}
}

// 品牌时序：由 realm 派生（品牌账号 realm 即品牌；codebuddy 按 cn/global 拆
// 国内版/国际版两个独立品牌），天然有界、无「其他」。
func TestBrandSeries(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{TotalTokens: 100, HasTotal: true}, true)
	r.Add(now, "global", "u2", "kimi-k3", Delta{TotalTokens: 50, HasTotal: true}, true)
	r.Add(now, "qoder", "u3", "auto", Delta{TotalTokens: 30, HasTotal: true}, true)
	r.Add(now, "trae", "u4", "glm-5.3", Delta{TotalTokens: 20, HasTotal: true}, true)

	s := r.Snapshot(24, nil, nil)
	bs := s.BrandSeries
	// codebuddy 国内/国际版是独立品牌（两个系列），qoder/trae 各自一条 → 共 4 条。
	if len(bs) != 4 {
		t.Fatalf("brand_series = %d 个品牌, want 4（codebuddy/codebuddy-global/qoder/trae）: %+v", len(bs), bs)
	}
	if len(bs["codebuddy"]) != 1 || bs["codebuddy"][0].TotalTokens != 100 {
		t.Fatalf("codebuddy（国内版）= %+v, want 100", bs["codebuddy"])
	}
	if len(bs["codebuddy-global"]) != 1 || bs["codebuddy-global"][0].TotalTokens != 50 {
		t.Fatalf("codebuddy-global（国际版）= %+v, want 50", bs["codebuddy-global"])
	}
	if len(bs["qoder"]) != 1 || bs["qoder"][0].TotalTokens != 30 {
		t.Fatalf("qoder = %+v, want 30", bs["qoder"])
	}
	if len(bs["trae"]) != 1 || bs["trae"][0].TotalTokens != 20 {
		t.Fatalf("trae = %+v, want 20", bs["trae"])
	}
	if _, ok := bs["其他"]; ok {
		t.Fatalf("品牌维度不应有「其他」: %+v", bs)
	}
}

// 维度数超上限时，前 N 个单独成线，其余归并到「其他」（逐格累加，不是只回合计）。
func TestDimSeriesOtherBucket(t *testing.T) {
	r := New("")
	now := time.Now()
	for i := 0; i < seriesTopN+3; i++ {
		name := fmt.Sprintf("m%02d", i)
		r.Add(now, "cn", "u", name, Delta{TotalTokens: int64(i + 1), HasTotal: true}, true)
	}
	s := r.Snapshot(24, nil, nil)
	ms := s.ModelSeries
	if len(ms) != seriesTopN+1 {
		t.Fatalf("model_series 系列数 = %d, want %d+1(其他)", len(ms), seriesTopN)
	}
	// 被归并的是 m00/m01/m02（总量最小）→ 其他 = 1+2+3 = 6
	other, ok := ms["其他"]
	if !ok || len(other) != 1 || other[0].TotalTokens != 6 {
		t.Fatalf("其他系列 = %+v, want 合计 6", other)
	}
	// 最大的 m%02d（seriesTopN+2）必须是独立系列
	if len(ms[fmt.Sprintf("m%02d", seriesTopN+2)]) != 1 {
		t.Fatalf("总量最大的模型未单独成线: %v", ms)
	}
}

func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// Snapshot 回带所选窗口（window_from/window_to/window_hours），供前端把 x 轴锚定
// 在「用户选的窗口」上，而不是数据范围——否则窗口起点早于最早数据点时，x 轴会
// 被压缩到数据范围，看起来「和选的窗口对不上」。左闭右闭，跨度 = hours 小时。
func TestSnapshotWindow(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	for _, hours := range []int{24, 72, 720} {
		s := r.Snapshot(hours, nil, nil)
		if s.WindowHours != hours {
			t.Fatalf("window_hours = %d, want %d", s.WindowHours, hours)
		}
		from, err1 := time.ParseInLocation(hourLayout, s.WindowFrom, time.Local)
		to, err2 := time.ParseInLocation(hourLayout, s.WindowTo, time.Local)
		if err1 != nil || err2 != nil {
			t.Fatalf("window 解析失败: from=%q(%v) to=%q(%v)", s.WindowFrom, err1, s.WindowTo, err2)
		}
		if got := int(to.Sub(from).Hours()); got != hours-1 {
			t.Fatalf("hours=%d 窗口跨度 = %d 小时, want %d（左闭右闭）", hours, got, hours-1)
		}
		if to.Before(time.Now().Add(-2 * time.Hour)) {
			t.Fatalf("window_to=%q 明显早于现在", s.WindowTo)
		}
	}
}

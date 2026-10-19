// metrics.go /metrics 端点与运行时指标（v1.11，方案 A：Prometheus 同机抓取）。
//
// 指标面（刻意小而稳，label 全部来自内部枚举或裁剪后的 model 名，防卡 explosions）：
//
//	l3api_chat_requests_total{model,realm,status,mode}   对外交付的请求数（含错误路径）
//	l3api_chat_attempts_total{realm,outcome}             上游尝试数（含换号重试，成功/失败）
//	l3api_chat_request_duration_seconds{mode}            请求全时长直方图
//	l3api_pool_accounts{brand,state}                     池内账号数（state=healthy/cooling/disabled）
//	l3api_pool_in_flight{brand}                          在途请求数（每账号观测求和）
//	l3api_sticky_sessions                                粘性会话绑定数
//	l3api_uptime_seconds                                 进程运行时长
//	l3api_build_info{version}                            版本标识（恒 1）
//
// 鉴权口径：/metrics 不鉴权（Prometheus 抓取免配 token）。安全边界靠部署形态：
// 生产 compose 监听 127.0.0.1:7863，Prometheus 经 compose 内网直连容器端口，
// 公网反代必须拦截 /metrics（DEPLOY.md 已给 Nginx/Caddy 样例）。改 listen 到
// 非回环时务必先配好反代拦截或打开 metrics 鉴权前移。
package server

import (
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"
	"time"

	"github.com/linguo2625469/l3api/internal/metrics"
)

// metricsSet handler 持有的指标集。
type metricsSet struct {
	started  time.Time
	requests *metrics.CounterVec
	attempts *metrics.CounterVec
	duration *metrics.HistogramVec
	// upstreamErrs 上游错误分类计数（2026-10-20 补）：{realm,kind,status}——
	// attempts 的 outcome 只有 success/fail，429/5xx/超时/内容拦截全混在
	// fail 里，看不出失败构成。kind 用 upstream.ErrKind 的稳定字符串。
	upstreamErrs *metrics.CounterVec
	// pickFails 选号失败计数（2026-10-20 补）：{realm,reason}——503
	// no_healthy_account 此前只有一条通知（未配 notify 即全静默），看不到
	// 候选账号各自因何被排除。reason 是枚举（cooling/breaker/degrade/
	// in_flight_full/free_exhaust/model_cooldown/credit_floor/none）。
	pickFails *metrics.CounterVec
}

func newMetricsSet() *metricsSet {
	return &metricsSet{
		started:      time.Now(),
		requests:     metrics.NewCounterVec("l3api_chat_requests_total", "Chat requests delivered by protocol endpoint."),
		attempts:     metrics.NewCounterVec("l3api_chat_attempts_total", "Upstream account attempts (including rotation retries)."),
		duration:     metrics.NewHistogramVec("l3api_chat_request_duration_seconds", "End-to-end request duration.", nil),
		upstreamErrs: metrics.NewCounterVec("l3api_chat_upstream_errors_total", "Upstream error responses by classified kind."),
		pickFails:    metrics.NewCounterVec("l3api_chat_pick_failures_total", "Account pick failures by exclusion reason."),
	}
}

// clipLabel model 名来自客户端请求体（不可信输入）。两道处理：
//  1. ToValidUTF8 + 控制字符清洗：按字节截断会把多字节字符劈成非法 UTF-8
//     （Prometheus 整个抓取可能失败）；	/ 等控制字符也一并清掉。
//  2. 限长 64 字节（截后再验证一次 UTF-8）。
// 注意这是「单 label 值」的防护——基数上限（不同 model 名的个数）不在这里，
// 由 server 侧的 model 目录命中判定天然收敛（未命中目录的自定义名会共享有限
// 的取值空间；恶意客户端造不出无限 series，目录本身有界）。
func clipLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if len(s) > 64 {
		s = s[:64]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// statusLabel HTTP 状态码 → label（0 = panic 前未写状态，标 "panic_pending"）。
func statusLabel(status int) string {
	if status == 0 {
		return "unset"
	}
	return strconv.Itoa(status)
}

// realmLabelForMetrics realm 归一到有限 label 空间（防基数爆炸）：
// cn/global/qoder/trae 原样，空 → "cn"（裸名缺省域），其余归一 "other"。
func realmLabelForMetrics(realm string) string {
	switch realm {
	case "", "cn", "global", "qoder", "trae":
		if realm == "" {
			return "cn"
		}
		return realm
	}
	return "other"
}

// metricsHandler GET /metrics：池仪表按 scrape 实时计算（Pool.List 一次遍历），
// 计数器/直方图从 metricsSet 读。
func (h *Handler) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if h.m == nil {
		http.NotFound(w, r)
		return
	}
	m := h.m

	// 池仪表：brand×state 计数 + 在途合计。List() 拿锁快照，scrape 频率下开销可忽略。
	acct := map[string]int{}       // brand\x00state → count
	inFlight := map[string]int64{} // brand → sum
	totalInFlight := int64(0)
	for _, st := range h.cfg.Pool.List() {
		brand := st.Brand
		if brand == "" {
			brand = "codebuddy"
		}
		state := "healthy"
		switch {
		case st.Disabled:
			state = "disabled"
		case st.Cooling:
			state = "cooling"
		}
		acct[brand+"\x00"+state]++
		inFlight[brand] += int64(st.InFlight)
		totalInFlight += int64(st.InFlight)
	}
	// ExtraHeader 逐行拼池仪表（gauge 序列的 label 组装在 metrics.Render 外完成）。
	var extra []string
	extra = append(extra, "# HELP l3api_pool_accounts Pool accounts by brand and state.",
		"# TYPE l3api_pool_accounts gauge")
	for k, v := range acct {
		parts := splitNul(k)
		extra = append(extra, "l3api_pool_accounts{brand="+quote(parts[0])+",state="+quote(parts[1])+"} "+strconv.Itoa(v))
	}
	extra = append(extra, "# HELP l3api_pool_in_flight In-flight requests by brand.",
		"# TYPE l3api_pool_in_flight gauge")
	for k, v := range inFlight {
		extra = append(extra, "l3api_pool_in_flight{brand="+quote(k)+"} "+strconv.FormatInt(v, 10))
	}
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	extra = append(extra, "# HELP l3api_sticky_sessions Sticky session bindings.",
		"# TYPE l3api_sticky_sessions gauge",
		"l3api_sticky_sessions "+strconv.Itoa(sticky),
		"# HELP l3api_uptime_seconds Process uptime in seconds.",
		"# TYPE l3api_uptime_seconds gauge",
		"l3api_uptime_seconds "+strconv.FormatInt(int64(time.Since(m.started).Seconds()), 10),
		"# HELP l3api_build_info Build information.",
		"# TYPE l3api_build_info gauge",
		"l3api_build_info{version="+quote(h.cfg.Version)+"} 1",
		// Go 运行时基础量（内存/goroutine）：供容量观测，不追求 prometheus 客户端全量。
		"# HELP l3api_goroutines Current goroutine count.",
		"# TYPE l3api_goroutines gauge",
		"l3api_goroutines "+strconv.Itoa(runtimeNumGoroutine()),
	)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte((&metrics.WriteTo{
		Counters:    []*metrics.CounterVec{m.requests, m.attempts, m.upstreamErrs, m.pickFails},
		Histograms:  []*metrics.HistogramVec{m.duration},
		ExtraHeader: extra,
	}).Render()))
}

// quote Prometheus label 值引号包裹 + 转义。
func quote(s string) string {
	return "\"" + escapeLabelValue(s) + "\""
}

func splitNul(s string) []string {
	out := []string{"", ""}
	i := 0
	for _, c := range s {
		if c == '\x00' {
			i++
			if i > 1 {
				break
			}
			continue
		}
		out[i] += string(c)
	}
	return out
}

// escapeLabelValue 与 metrics.escapeLabel 同规则（此处引号拼接前调用）。
func escapeLabelValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func runtimeNumGoroutine() int { return runtime.NumGoroutine() }

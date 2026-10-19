// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"

	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/l3api/internal/apikeys"
	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/httpauth"
	"github.com/linguo2625469/l3api/internal/livecfg"
	"github.com/linguo2625469/l3api/internal/notify"
	"github.com/linguo2625469/l3api/internal/pool"
	"github.com/linguo2625469/l3api/internal/prompt"
	"github.com/linguo2625469/l3api/internal/provider"
	"github.com/linguo2625469/l3api/internal/session"
	"github.com/linguo2625469/l3api/internal/upstream"
	"github.com/linguo2625469/l3api/internal/usage"
)

// clientDisconnected 报告「本次失败是客户端主动断开造成的，不是账号故障」。
//
// 判据（任一成立即真）：
//   - 请求 ctx 已取消/超时（客户端断开连接 → r.Context() Done）；
//   - 错误本身是 context.Canceled / EPIPE（写客户端失败）。
//
// 用途：流式写失败与非流式写失败路径据此跳过 NoteFailures/NoteSuccess——
// 客户端断开不是账号问题（Codex/中断生成的客户端批量取消时，旧实现会把
// 健康号连续打出池，池子静默缩水）。上游 EOF/解析失败仍照常罚号。
func clientDisconnected(r *http.Request, err error) bool {
	if r != nil && r.Context().Err() != nil {
		return true
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	// 写客户端失败的典型错误（broken pipe / connection reset）。
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "client disconnected")
}

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
	MaxBodyBytes int64
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 软冷却基数，默认 60s（仅在上游未给重试时间时使用；连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// MetricsEnabled /metrics 端点开关（config metrics.enabled，缺省 true）。
	MetricsEnabled bool
	// Version 版本标识（/metrics l3api_build_info 用）。
	Version string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder

	// Notifier 事件告警（可选；nil = 不告警）。当前仅挂「池耗尽」：轮转全部
	// 用尽且无上游错误信封（纯调度性 503）时外推——上游错误由 pool 侧事件
	// （禁用/熔断/积分耗尽）覆盖，这里不重复。
	Notifier notify.Poster
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:       h.cfg.APIKey,
		SoftCooldown: h.cfg.SoftCooldown,
	}
}

// defaultSoftCooldown 上游未给任何重试时间时的软冷却基数（1 分钟）。
// 上游一旦明说（body 重置墙钟 / Retry-After 头 / body retry_after 字段），
// 一律以上游为准，本值只作最后兜底。
const defaultSoftCooldown = 60 * time.Second

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return defaultSoftCooldown
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（60s 起 + 指数升级），一次偶发 404 会被逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
// 多品牌改造后统一以 l3api 对外标识（模块名同此）。旧值 workbuddy2api 是
// 单品牌时期的遗留；/healthz 与 X-Service 头会透出该值，若有外部监控按名
// 匹配需同步调整。
const ServiceName = "l3api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP wafIPGate
	// maxBodyBytes 请求体上限的运行期值（cfg.MaxBodyBytes 的原子镜像）。
	// 面板在线改 server.max_body_mb 时经 SetMaxBodyBytes 热生效，无需重启
	// （issue #17：改了配置却静默不生效，用户仍被 8MB 413 拦截）。
	// <=0 = 不限（2026-10-18 起，与 config 同口径）。
	maxBodyBytes atomic.Int64
	// m 运行时指标集（cfg.MetricsEnabled=false 时 nil，/metrics 返回 404）。
	m *metricsSet
}

// SetMaxBodyBytes 热更新请求体上限（面板保存配置路径调用）。
// n<=0 = 不限（与 config.Server.MaxBodyMB 同口径：0/缺省即不设限，2026-10-18 改）。
// 旧口径把 <=0 兜底成 8MB，导致面板填 0 想表达"不限"却被当成 8MB 继续 413。
func (h *Handler) SetMaxBodyBytes(n int64) {
	h.maxBodyBytes.Store(n)
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = defaultSoftCooldown // 软限流基数（上游给时间时以上游为准）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	// MaxBodyBytes <=0 保持原样（=不限，readChatBody 据此跳过 413 检查）。
	// 旧口径在此兜底成 8MB，与 config 的"0 = 不限"冲突（2026-10-18 改）。
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.maxBodyBytes.Store(cfg.MaxBodyBytes)
	if cfg.MetricsEnabled {
		h.m = newMetricsSet()
	}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// Anthropic Messages 兼容层（v1.11）：Claude Code / Cline 等原生客户端直连。
	h.mux.HandleFunc("POST /v1/messages", h.withAuthAs(h.anthropicMessages, writeAnthropicErr))
	// OpenAI Responses API（v1.11）：Codex / 新版 SDK 直连。
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responsesAPI))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// /metrics 运行时指标（v1.11）：不鉴权（Prometheus 免配 token），安全边界靠
	// 回环监听 + 反代拦截（见 metrics.go 头注释与 DEPLOY.md）。
	if h.m != nil {
		h.mux.HandleFunc("GET /metrics", h.metricsHandler)
	}
	// /livez 进程级恒活探针：不查池健康（无账号/全池冷却时也恒 200），
	// 专供 Docker HEALTHCHECK / k8s livenessProbe 判「进程活着」；
	// /healthz 保持池健康语义（无号可服务时 503），留给负载均衡读流量。
	h.mux.HandleFunc("GET /livez", h.livez)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// panic-recover 兜底：任何 handler/panel 内意外 panic 时记完整栈 + 回
	// OpenAI 风格 JSON 500，而不是让 Go http server 默认 recover 打回裸 500
	// （日志格式不统一、客户端拿到的不是规范错误结构）。
	// 注意：SSE 流式响应已开流（WriteHeader(200) 已发）时 panic 无法再改状态码，
	// 此分支仅记日志——上游 sse.go 另有局部 recover 兜底流内写失败。
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("PANIC: %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
			// 已 WriteHeader 的响应再写状态码无效（Go 会打 superfluous WriteHeader 日志），
			// 但 JSON body 仍可尝试补写——多数 panic 发生在写响应之前，能正常回 500。
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}
	}()
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return h.withAuthAs(next, writeOpenAIError)
}

// withAuthAs 同 withAuth，但 401 错误形态由调用方注入（Anthropic 兼容层用
// authentication_error 形态，其余端点用 OpenAI 形态）。
func (h *Handler) withAuthAs(next http.HandlerFunc, writeErr func(w http.ResponseWriter, status int, code, msg string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entry, ok := h.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r.WithContext(apikeys.WithEntry(r.Context(), entry)))
	}
}

// authenticate 请求鉴权：多 key 注册表优先（livecfg.Keys），未启用多 key 时
// 回退单 api_key 口径。注册表启用但 token 不匹配任何条目（含禁用条目）→ false；
// 注册表为空（Len()==0，面板清光了 key）= 鉴权关闭，与启动语义一致——回落
// 单 key 口径继续判定（api_key 也空 → 放行）。
func (h *Handler) authenticate(r *http.Request) (apikeys.Entry, bool) {
	live := h.loadLive()
	if reg := live.Keys; reg != nil {
		if e, ok := reg.Verify(httpauth.BearerToken(r)); ok {
			return e, true
		}
		if reg.Len() > 0 {
			return apikeys.Entry{}, false
		}
	}
	if !httpauth.VerifyBearer(r, live.APIKey) {
		return apikeys.Entry{}, false
	}
	return apikeys.Entry{Name: "default"}, true
}

// enforceModelAllowlist 只查模型白名单（无状态，重复调用安全）。
// responses/messages 的入口 handler 用它做早拒（403 形态早于 doChat），
// RPM 一律留给 doChat 内的 enforceKeyPolicy 计一次。
func (h *Handler) enforceModelAllowlist(w http.ResponseWriter, r *http.Request, models ...string) bool {
	entry, ok := apikeys.FromContext(r.Context())
	if !ok {
		return true
	}
	if !entry.AllowsModel(models...) {
		writeOpenAIError(w, http.StatusForbidden, "model_not_allowed",
			fmt.Sprintf("model %q is not allowed for api key %q", firstNonEmpty(models...), entry.Name))
		return false
	}
	return true
}

// enforceKeyPolicy 多租户策略（chat 路径与 doChat 内部用）：模型白名单 +
// RPM 限流。entry 的零值（单 key 回退分支）Models/RPM 均为零 → 天然全放行，
// 无需特判；未挂 entry（裸用/测试直调）也放行。
// 返回 false 时已写出 403/429 响应，调用方直接 return。
func (h *Handler) enforceKeyPolicy(w http.ResponseWriter, r *http.Request, models ...string) bool {
	if !h.enforceModelAllowlist(w, r, models...) {
		return false
	}
	entry, ok := apikeys.FromContext(r.Context())
	if !ok {
		return true
	}
	if reg := h.loadLive().Keys; reg != nil {
		if allow, retryAfter := reg.Allow(entry.Name, entry.RPM); !allow {
			if retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
			}
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
				fmt.Sprintf("api key %q exceeded %d requests/minute limit", entry.Name, entry.RPM))
			return false
		}
	}
	return true
}

// firstNonEmpty 返回第一个非空串（全空返回空串）。
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// livez 进程级恒活探针：进程能响应即 200，不做任何池/上游检查。
// 与 /healthz 分工：livez 答「活着没」（Docker HEALTHCHECK），healthz 答「能不能接客」（LB）。
func (h *Handler) livez(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": ServiceName})
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
		// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// countsMapFrom 把 CountsDetailed 五元组打包成 /status 的域分组建模。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 1h），失败/无号返回空列表（无静态兜底——
// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型）。
// 多 key 白名单生效时按 key 过滤（id 原文或剥前缀裸名命中白名单才列出）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	list := h.modelList()
	if entry, ok := apikeys.FromContext(r.Context()); ok && len(entry.Models) > 0 {
		filtered := make([]map[string]any, 0, len(list))
		for _, e := range list {
			id, _ := e["id"].(string)
			if entry.AllowsModel(id, bareModelOf(id)) {
				filtered = append(filtered, e)
			}
		}
		list = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   list,
	})
}

// bareModelOf 剥掉模型 id 的 "xxx:" 路由前缀（无前缀原样返回）。
func bareModelOf(id string) string {
	if i := strings.Index(id, ":"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	// 徽章与营销/折扣活动（badge 此前完全没透出——既有缺口；promotion 带准确时段）。
	// 结构化促销优先，无则回落徽章兜底窗口（见 EffectivePromotion 注释）。
	// active 每次请求现算：/v1/models 有 1h 缓存，缓存 bool 会让时段切换后仍报旧值。
	if mi.Badge != "" {
		entry["badge"] = mi.Badge
	}
	if pm := upstream.PromotionJSON(mi.EffectivePromotion(), time.Now()); pm != nil {
		entry["promotion"] = pm
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
		if mi.CanDisableThinking {
			entry["can_disable_thinking"] = true
		}
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// acctMatchesRealm 判定账号是否属于请求选号域（与 pool 的 realmMatch 同口径）：
// 品牌域按 Brand()，cn/global 按 Realm() 且排除非 codebuddy 品牌。
// realm=="" 恒真。修复（2026-09-28）：此前统一按 Realm()==realm 判定，
// 品牌账号 Realm() 恒为 cn → qoder/trae 粘性号永远被判"realm 不符"而解绑。
func acctMatchesRealm(a *auth.Auth, realm string) bool {
	if realm == "" {
		return true
	}
	if IsBrandRealm(realm) {
		return a.Brand() == realm
	}
	return a.Brand() == "codebuddy" && a.Realm() == realm
}

// modelList 模型列表：CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel
// 对称）；global.enabled=true 时追加 global: 前缀的国际版名单。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	// 品牌静态目录（v1.11 注册表化）：qoder/trae 等静态目录品牌由 upstream 侧
	// 注册（provider.RegisterChatModels，见 brand_registry.go），这里按注册表
	// 统一输出——前缀 "<brand>:" 是网关路由协议（resolveModel 剥离后分发给
	// 品牌桥）。仅当池内有该品牌账号时列出（无号则不提供路由目标，避免 503 幻觉）。
	for _, brand := range provider.ChatBrands() {
		if !h.cfg.Pool.HasBrand(string(brand)) {
			continue
		}
		for _, mi := range provider.ChatModels(brand) {
			entry := map[string]any{
				"id":           string(brand) + ":" + mi.ID,
				"object":       "model",
				"created":      1753600000,
				"owned_by":     string(brand),
				"brand":        string(brand),
				"name":         mi.DisplayName,
				"format":       "openai",
				"is_vl":        mi.IsVL,
				"is_reasoning": mi.IsReasoning,
			}
			if mi.ContextLength > 0 {
				entry["context_length"] = mi.ContextLength
			}
			out = append(out, entry)
		}
	}
	for _, mi := range h.fetchDynamicModels() {
		entry := map[string]any{
			"id":       "cn:" + mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens 四级查找（upstream.model_catalog）：
		// 上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态种子表 →
		// model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，拉到后
		// 写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
		// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
		// 截断、白白丢上下文）。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （客户端可发现档位，不再盲传）。无档位 → 省略字段。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 纯动态探测结果（fetchGlobalModels，失败/无号 → 空）。
	if h.cfg.GlobalEnabled {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			entry := map[string]any{
				"id":       "global:" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens 四级查找（与 CN 动态分支同口径）。
			var remoteCtx, remoteOut int64
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				remoteCtx, remoteOut = mi.ContextWindow, mi.MaxTokens
			}
			entry["context_length"] = upstream.ContextWindowListingV4(id, remoteCtx, h.cfg.Upstream.HTTP)
			if mo, ok := upstream.MaxOutputTokensListingV4(id, remoteOut, h.cfg.Upstream.HTTP); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 缓存/失败回落封在 upstream.FetchGlobalModels（内部 1h + 5min 负缓存）。
// 本方法只负责"何时探测"：池中无 global 账号 → 空名单 + nil 账号（零上游调用）。
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PeekHealthy("global") // 无副作用探测（不写 lastUsed）
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// fetchDynamicModels 从池中任一健康 CN 账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接返回 nil（纯动态，无静态表兜底），
// 避免反复打上游。只从 CN realm 账号拉取（global 走独立探测）。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.PeekHealthy("cn") // 无副作用探测（不写 lastUsed）
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError：NoteError 喂的是 chat
		// 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道健康的账号；
		// models 拉取失败 ≠ 账号 chat 不可用。
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（hint 判定用：错误路径加一次 FetchModels 网络调用既拖慢
// 错误响应、又污染上游调用语义）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

// activeFreePromo 返回 bareModel 在指定 realm 当前生效的免费促销（free_guard
// 守护判定用）；无生效免费促销 → nil（守护不生效，照常计费）。
//
// 数据源（仅守护 key 会走到这里，允许冷缓存时同步拉一次目录——fetchDynamicModels
// /FetchGlobalModelInfos 内部各有 1h 缓存 + 5min 负缓存，命中后零网络调用）：
//   - cn（裸名）：CN 模型目录（fetchDynamicModels）；
//   - global：global 模型目录（PeekHealthy("global") + FetchGlobalModelInfos，
//     与 /v1/models 同一条缓存链；无 global 号 → nil）；
//   - 品牌域（qoder/trae）：无促销概念，恒 nil。
//
// 判定口径 = EffectivePromotion（结构化促销优先，徽章兜底）且当前生效
// （Active）且免费形态（IsFree，折后 0x）。目录拉取失败 → nil：
// 保守不守护（宁放过不误拦——促销数据缺失时按正常计费走）。
func (h *Handler) activeFreePromo(realm, bareModel string) *upstream.ModelPromotion {
	if bareModel == "" {
		return nil
	}
	var infos []upstream.ModelInfo
	switch realm {
	case "cn":
		infos = h.fetchDynamicModels()
	case "global":
		acct := h.cfg.Pool.PeekHealthy("global") // 无副作用探测（不写 lastUsed）
		if acct == nil {
			return nil
		}
		infos = h.cfg.Upstream.FetchGlobalModelInfos(acct)
	default:
		return nil // 品牌域无促销（qoder/trae 目录不含 modelPromotions）
	}
	for _, mi := range infos {
		if mi.ID != bareModel {
			continue
		}
		p := mi.EffectivePromotion()
		if p != nil && p.IsFree() && p.Active(time.Now()) {
			return p
		}
		return nil // 命中模型但无生效免费促销 → 不守护
	}
	return nil // 目录里没这个模型（未知模型）→ 不守护
}


func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// OpenAI 原生端点：读体 + 单次解析事实，随后交 doChat 轮转核心（OpenAI sink 恒等输出）。
	body, ok := h.readChatBody(w, r, writeOpenAIError)
	if !ok {
		return
	}
	// bodyFacts 一次解析产出 stream/model/工具名单/日志 model（2026-10-20 合并：
	// 原先这里 peek 一次、doChat 内再解析 6 次）。
	facts := bodyFacts(body)
	// "@paid" 后缀（free_guard 单次强制付费放行阀）在入口剥：doChat/白名单/粘性/
	// 记账全按剥后名处理。sink 展示名用剥后名（客户端看到的响应 model 无后缀）。
	cleanModel, forcePaid := splitPaidSuffix(facts.Model)
	if forcePaid {
		// 出站 body 同步改写：否则 doChat 里 bareModel==reqModel 跳过重写，
		// 上游收到带后缀的模型名 → 11102。
		body = rewriteModel(body, cleanModel)
	}
	h.doChat(w, r, body, facts.Stream, cleanModel, forcePaid, newOpenAISink(), facts)
}

// doChat 轮转核心（v1.11 从 chatCompletions 抽取）：选号/粘性/换号/错误分类/
// 记账与协议无关；三个协议输出点经 sink 分发（OpenAI=恒等，Anthropic/Responses=
// internal/protocol 翻译）。stream/reqModel 由调用方从各协议请求解析。
// facts 是调用方单次解析出的请求体事实（bodyFacts）；2026-10-20 起 doChat 不再
// 自行解析 body（原先 6 次 unmarshal 全合并到调用方一次）。facts 零值时按
// 「空 model/无名单/无粘性」继续——测试与内部调用方可传零值。
// forcePaid = 请求模型名带 "@paid" 后缀（调用方已剥离）：本请求绕过免费额度守护
// （free_guard），照常计费——守护 key 的单次付费放行阀。
func (h *Handler) doChat(w http.ResponseWriter, r *http.Request, body []byte, stream bool, reqModel string, forcePaid bool, sink *chatSink, facts ChatFacts) {
	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)

	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本
	// （见 upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的
	// tools」与「会话历史里出现过的工具名」两个来源——实测 tools 声明会在
	// 中转环节丢失，只认前者会让修复层在最需要它的场景下失效。
	// 名单由调用方单次解析产出（facts.DeclaredTools，合并前此处独自 unmarshal）。
	declaredTools := facts.DeclaredTools

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	// "@paid" 后缀已在各协议入口剥离（chatCompletions/anthropicMessages/responsesAPI
	// 的 splitPaidSuffix），doChat 收到的 reqModel 恒为剥后名，forcePaid 走参数。
	realm, bareModel := resolveModel(reqModel)

	// 多租户策略：模型白名单 + RPM 限流（key 维度，withAuth 已解析条目）。
	// 同时比对请求原文与剥前缀裸名（白名单两种写法都接受）。
	if !h.enforceKeyPolicy(w, r, reqModel, bareModel) {
		return
	}
	// key 名随请求归因到用量（withAuth 零值条目 name="default"，口径统一）。
	apiKeyName := ""
	if e, ok := apikeys.FromContext(r.Context()); ok {
		apiKeyName = e.Name
	}

	// 免费额度守护（free_guard）：key 开了守护、未带 @paid 后缀、且该模型当前
	// 处于生效的免费促销窗口 → 本请求走守护选号（排除免费额度已尽的账号）。
	// 窗口外/非免费促销/目录拉取失败 → guardPromo=nil，守护不生效（照常计费）。
	// 注意顺序：先判 key 开关再取促销——守护 key 的请求允许冷缓存时同步拉一次
	// 目录（fetchDynamicModels 内部 1h 缓存/5min 负缓存，命中后零网络）；
	// 非守护 key 恒不触发，热路径零开销。
	freeGuard := false
	var guardPromo *upstream.ModelPromotion
	if e, ok := apikeys.FromContext(r.Context()); ok && e.FreeGuard && !forcePaid {
		guardPromo = h.activeFreePromo(realm, bareModel)
		freeGuard = guardPromo != nil
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	// model 用调用方解析出的 facts.LogModel（合并前此处独自 parseModelFromBody）。
	st := newChatStat(time.Now(), facts.LogModel, stream)
	defer st.done()
	// 指标（v1.11）：请求计数 + 全时长直方图（realm 解析后才能打 label，此处 defer）。
	reqStart := time.Now()
	defer func() {
		if h.m != nil {
			mode := "sync"
			if stream {
				mode = "stream"
			}
			h.m.requests.Inc("model", clipLabel(bareModel), "realm", realm,
				"status", statusLabel(st.status), "mode", mode)
			h.m.duration.Observe(time.Since(reqStart).Seconds(), "mode", mode)
		}
	}()

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// ExtractKey 与粘性开关解耦（issue #35 侧）：关闭粘性时会话头族的聚合主键仍按
	// 会话级（RequestIDForKey(sessKey)），不悄悄退化成轮级——提取本身与粘性无关。
	// sessKey 由调用方单次解析产出（facts.SessionKey，合并前此处独自 ExtractKey）。
	sessKey := facts.SessionKey
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		// 按模型解析：绑定号在**当前模型**被 6004 限额时视为不可用 → 重新分配，
		// 而不是钉在限额号上反复失败（"限额后换不动号"的正解）。
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, reqModel); ok {
			stickyUID = uid
		}
	}

	// 轮级兜底聚合键：无会话键的客户端（OpenAI 兼容协议——dsh / Codex / Cherry
	// Studio 等请求体里既无 conversationId 也无 metadata）sessKey 恒空，会话头族的
	// 聚合主键只能逐请求新生成，agent 多轮在上游用量明细里仍是一条请求一条记录。
	// 这里按 body 里最后一条 user 消息派生轮级键（同轮内所有上游调用同键）。
	// 必须在下方 prompt.Rewrite 之前取——改写会动 messages 内容，之后取会让键漂移。
	turnKey := ""
	if sessKey == "" {
		turnKey = session.TurnKey(body)
	}

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。facts.HasImage 由调用方解析产出
	// （合并前此处独自 hasImagePart）。
	reqHasImage := facts.HasImage

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	// usageRealm 归账域：品牌账号按品牌名归账（其 Realm 恒归一为 cn，直用会把
	// qoder/trae 用量混进 cn 视图）；codebuddy 账号按 Realm（cn/global）。
	// 只在池状态里查一次——recordAttempt 与 recordCredit 共用同一口径，否则同一
	// 请求的 token 与积分会落进不同桶（按品牌聚合时对不上）。
	usageRealm := func(uid string) string {
		attemptRealm := "cn"
		if a, ok := h.cfg.Pool.Status(uid); ok {
			if a.Brand != "" && a.Brand != "codebuddy" {
				attemptRealm = a.Brand
			} else if a.Realm != "" {
				attemptRealm = a.Realm
			}
		}
		return attemptRealm
	}
	// recordCredit 记录本次请求的积分消耗（上游末帧 usage.credit）。独立于
	// recordAttempt：credit 要等流结束/聚合完成才拿得到，而请求数在尝试结束时
	// 就已记账。归因字段与 recordAttempt 逐字段相同（见 usageRealm）。
	recordCredit := func(uid string, credit float64) {
		if h.cfg.Usage == nil || credit <= 0 {
			return
		}
		h.cfg.Usage.AddCredit(time.Now(), usageRealm(uid), uid, reqModel, apiKeyName, credit)
	}

	recordAttempt := func(uid string, delta pool.TokenUsageDelta, started time.Time) {
		delta.Model = reqModel
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs
		if delta.HasCompletionTokens && delta.CompletionTokens >= 0 && latencyMs > 0 {
			delta.HasTokensPerSecond = true
			delta.TokensPerSecond = float64(delta.CompletionTokens) * 1000 / float64(latencyMs)
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)

		// 归账域只查一次池状态（此前指标与用量各查一次 Status——每次上游尝试
		// 两次进池锁 + 两次全量 Status 构造，2026-10-13 合并为一次）。
		attemptRealm := usageRealm(uid)

		// 尝试计数（v1.11）：成功/失败都算一次上游尝试（含换号重试），运维据此
		// 看到重试放大。
		if h.m != nil {
			outcome := "fail"
			if delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens {
				outcome = "success"
			}
			h.m.attempts.Inc("realm", attemptRealm, "outcome", outcome)
		}

		// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
		// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := attemptRealm
			h.cfg.Usage.AddKey(time.Now(), realm, uid, delta.Model, apiKeyName, usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
			}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
		}
	}

	// 系统提示词改写 + outbound model 前缀剥离，一次 JSON 往返完成（2026-10-20
	// 优化：原先 prompt.Rewrite 与 rewriteModel 各自全量解编一遍，700KB body
	// 白付一次往返；两者总是相邻执行，合并零语义变化）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough 非降级期：透传客户端原始 system（不改写）。
	// model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，上游不认前缀
	// （global 账号也请求裸模型名）。裸名时 bareModel==reqModel，本步零操作。
	degradedApplied := false
	switch {
	case h.cfg.PromptMode == "custom" && h.cfg.PromptText != "":
		body = prompt.RewriteAndModel(body, h.cfg.PromptText, bareModel)
	case h.cfg.PromptMode == "passthrough" && h.degrade.Active():
		body = prompt.RewriteAndModel(body, prompt.Degraded, bareModel)
		degradedApplied = true
	default:
		if bareModel != reqModel {
			body = rewriteModel(body, bareModel)
		}
	}

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先，否则按
	//     粘性 key 进程内稳定生成；粘性 key 也空时走轮级兜底（TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级随机——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	chatMeta := upstream.ChatMeta{ConversationID: facts.ConversationID}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if sessKey != "" {
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键客户端：轮级兜底——同轮内 tool call 多轮 / 换号重试 / 降级重发
		// 共享同键，用户发下一条消息自动换键。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")
	chatMeta.Model = bareModel // Qoder 分支据此透传模型 key（qfmodel/kmodel_latest/...）

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			// 免费额度守护：粘性号在此模型免费额度已尽 → 视同不可用，解绑重选
			//（同 6004 豁免口径：钉在额度尽的号上反复扣费正是守护要拦的）。
			if acct != nil && freeGuard && h.cfg.Pool.FreeExhausted(stickyUID, bareModel) {
				acct = nil
			}
			if acct == nil || !acctMatchesRealm(acct, realm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额/免费额度尽）
				// 或域/品牌不符 → 解绑，本次回落普通轮换。
				unbindSticky()
				acct = nil
			}
		}
		if acct == nil {
			// 模型感知 + 品牌感知选号：品牌域（qoder/trae）走品牌过滤（只选
			// 本品牌账号），裸模型/cn/global 走 codebuddy 池（品牌账号被谓词隔离）。
			// 免费额度守护：守护生效时排除免费额度已尽的账号（freeExhaust 台账）。
			if freeGuard {
				acct = h.cfg.Pool.PickExcludingForBrandGuarded(tried, bareModel, realm)
			} else {
				acct = h.cfg.Pool.PickExcludingForBrand(tried, bareModel, realm)
			}
		}
		if acct == nil {
			// 守护全尽：选不出号且有账号被台账排除 → 402 明确报错（附 @paid 提示），
			// 而不是落到末端 503「稍后再试」（额度尽不是暂时性抖动，重试无意义）。
			if freeGuard && h.cfg.Pool.FreeExhaustedCount(realm, bareModel) > 0 {
				st.status = http.StatusPaymentRequired
				sink.WriteError(w, http.StatusPaymentRequired, "free_quota_exhausted",
					fmt.Sprintf("模型 %s 的免费额度已用尽（免费时段内所有账号实测开始扣积分）。"+
						"等配额重置后再试，或模型名加 @paid 后缀（如 %s@paid）本次强制付费调用。",
						bareModel, bareModel), "")
				return
			}
			st.status = http.StatusServiceUnavailable
			// 选号失败归因（2026-10-20）：把「候选账号各自因何被排除」记进
			// 指标与一行日志——503 no_healthy_account 此前只有一条 notify
			//（未配即全静默），池子缩水时完全看不出是冷却/熔断/额度尽还是域不匹配。
			if h.m != nil && h.m.pickFails != nil {
				reason := h.cfg.Pool.PickFailureReason(tried, bareModel, realm, freeGuard)
				h.m.pickFails.Inc("realm", realmLabelForMetrics(realm), "reason", reason)
				if chatLogEnabled {
					log.Printf("chat pick failed: realm=%s model=%s reason=%s", realm, bareModel, reason)
				}
			}
			break
		}
		st.uid = acct.UID
		// 品牌展示名（含域）随账号一起定下来：日志行要在模型前面打出「谁打的」；
		// 换号重试时跟着本轮的 acct 走，不留上一轮的品牌。
		st.brand = brandLabel(acct.Brand(), acct.Realm())
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			// 本地调度竞争（名额被并发抢走）不sleep退避（2026-10-20）：
			// 没打上游、没有可等待的对方——白睡 500ms/1s 只是拖长本请求的
			// 失败时延。仅最后一轮前不睡（rotateBackoff 既有语义），中间轮
			// 也跳过：换下一个号是纯本地操作，微秒级完成。
			if r.Context().Err() != nil {
				break // 客户端已断连：换号重试无意义
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			// 客户端已断开：刷新与换号都无意义（省一次上游 refresh 调用）。
			if r.Context().Err() != nil {
				st.status = 499
				return
			}
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					// 阈值化（连续 N 次才禁用）：12153/401 会被网络抖动与 refresh
					// 竞态临时触发，一次失败就永久杀号是历史 P0-1 误杀的根因
					// （2026-10-13 与 keepalive/chat 路径统一口径）。
					if h.cfg.Pool.NoteSessionDead(acct.UID) {
						log.Printf("chat refresh uid=%s: session dead 连续 %d 次，已禁用", uidFull(acct.UID), pool.SessionDeadThreshold())
					}
				} else {
					// refresh 端点失败 ≠ chat 通道故障：只给短软冷却，不喂 chat
					// 熔断器（NoteError 3 次熔断 30min——auth 端点独立故障会把
					// 完全健康的 chat 账号熔断，2026-09-28 修复）。
					h.cfg.Pool.CooldownSoftRate(acct.UID, time.Minute, time.Time{}, "refresh failed")
				}
				fail(acct.UID)
				if !rotateBackoff(i, h.cfg.MaxRotate, r.Context()) {
					break // ctx 取消：终止轮转（refresh 失败换号退避）
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
			}
		}

		// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析）。传输层错误（非 *Error）走抖动换号分支；防御分支
		// （terr 为 nil 但 status>=400，如 ErrNone 兜底）回落本地 Classify，双保险。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 客户端主动断开（Ctrl-C / 中断生成 / 下游超时取消）：不是账号故障，
			// 不罚号不喂连败（2026-10-20 修复：此前一律 NoteFailures，批量取消
			// 请求的客户端会把健康号连续打出池）。只记尝试（用量视图可见）。
			if clientDisconnected(r, terr) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
				st.status = 499 // nginx 口径：客户端关闭请求
				return
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, h.cfg.MaxRotate, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避）
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报（passthrough 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				h.degrade.Trigger()
				// 降级重试：改写 system 提示词（同一次往返里把 model 剥回裸名，
				// 防止带前缀名出站——prompt.Rewrite 不动 model 字段）。
				body = prompt.RewriteAndModel(body, prompt.Degraded, bareModel)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样），
				// 不再改写成网关固定文案——客户端必须看到真实错误才能排查。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				sink.WriteError(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作，fail 只释放租约。
			// message 装上游 body 原文（含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				sink.WriteError(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义）：
			// 该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，IP 被拦而非账号）
			// 则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍打同一出口 IP，
			// 加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, h.cfg.MaxRotate, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避）
			}
			continue
		}
		// 成功记账推迟到流/聚合完成之后：开流只代表 HTTP 200，流内仍可能出现
		// error 帧（6004 限流/审核拦截/余额耗尽）。过早 NoteSuccess 会把这类失败
		// 记为成功并清掉熔断/连败/负缓存（2026-09-28 修复）。
		commitSuccess := func() {
			h.cfg.Pool.NoteSuccess(acct.UID)
			// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
			// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
			h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
			// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
			// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
			if sessKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(sessKey, acct.UID)
			}
		}
		if stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			// panic 兜底：StreamHint 写循环 panic 时（由 http 层 recover 接住）
			// rc 也必须关闭，否则上游连接 + monitorBody goroutine 泄漏。
			rcClosed := false
			defer func() {
				if !rcClosed {
					rc.Close()
				}
			}()
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			// DSML 标记修复（dsml.go）：上游在没有 tools 的请求里把工具调用吐成
			// 原生标记文本，这里在透传前还原成 delta.tool_calls。allowUnknown=true
			// = 名单为空时启用弱判定（tools 声明在链路上丢失是实测最常见的泄漏成因）。
			serr := sink.WriteStream(w, rc, stats, func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}, upstream.NewMarkupRepair(declaredTools, true))
			rc.Close()
			rcClosed = true
			// 流内 error 帧（已透传给客户端）：撤销乐观记账，按帧内容分类罚号。
			var sfe *upstream.StreamFrameError
			if errors.As(serr, &sfe) {
				recordAttempt(acct.UID, stats.Usage(), attemptStarted)
				kind := upstream.FrameKind(sfe.Payload)
				if kind != upstream.ErrNone {
					h.applyErrorPolicy(acct.UID, kind, sfe.Payload, bareModel,
						&upstream.Error{Kind: kind, Status: 400, Msg: sfe.Payload})
				} else {
					h.cfg.Pool.NoteFailures(acct.UID)
				}
				return
			}
			if serr != nil {
				// 客户端主动断开（写失败）：中性账——不记成功也不罚号
				// （2026-10-20：批量取消的客户端此前会把健康号打出池）。
				if clientDisconnected(r, serr) {
					recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
					st.status = 499
					return
				}
				// 空流/写失败等本地异常：不记成功，喂连败兜底（单次不罚）。
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
				h.cfg.Pool.NoteFailures(acct.UID)
				return
			}
			commitSuccess()
			recordAttempt(acct.UID, stats.Usage(), attemptStarted)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			// 积分消耗时序（v1.16.9）：只要上游下发了 credit 就记（与是否有
			// total_tokens 无关——单价算不了也要让「今日消耗」对得上）。
			if credit, ok := stats.Credit(); ok {
				recordCredit(acct.UID, credit)
				if total, tok := stats.TotalTokens(); tok && total > 0 {
					h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
					h.noteFreeExhaust(acct.UID, bareModel, credit, guardPromo)
				}
			}
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err == nil {
			// DSML 标记修复（非流式）：正文里的原生标记还原成 message.tool_calls。
			// 只在「没有结构化 tool_calls」且 finish_reason=stop 时生效（dsml.go）。
			if mr := upstream.RepairAggregatedResponse(resp, declaredTools, true); mr.Converted() > 0 {
				log.Printf("INFO: [server] non-stream model=%s: repaired %d native tool call(s) from text", bareModel, mr.Converted())
			}
		}
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
			// 客户端主动断开（聚合读取消）：中性账——不罚号（2026-10-20，
			// 与流式路径同口径）。
			if clientDisconnected(r, err) {
				st.status = 499
				return
			}
			// 流内 error 帧：与流式路径同口径做 FrameKind 分类冷却（6004 模型级
			// 冷却、硬信用冷却等）——此前一律 NoteFailures + 502，错误账号/模型
			// 不被冷却，下个请求继续撞同一坑。
			var sfe *upstream.StreamFrameError
			if errors.As(err, &sfe) {
				kind := upstream.FrameKind(sfe.Payload)
				if kind != upstream.ErrNone {
					h.applyErrorPolicy(acct.UID, kind, sfe.Payload, bareModel,
						&upstream.Error{Kind: kind, Status: 400, Msg: sfe.Payload})
				} else {
					h.cfg.Pool.NoteFailures(acct.UID)
				}
				sink.WriteError(w, http.StatusBadGateway, "upstream_in_stream_error", sfe.Payload, "")
				st.status = http.StatusBadGateway
				return
			}
			h.cfg.Pool.NoteFailures(acct.UID) // 聚合失败（解析/传输）：不记成功
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			sink.WriteError(w, http.StatusBadGateway, "upstream_parse", err.Error(), "")
			st.status = http.StatusBadGateway
			return
		}
		commitSuccess()
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), attemptStarted)
		sink.WriteFinal(w, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if credit, total, ok := usageCreditTotal(resp); ok {
			recordCredit(acct.UID, credit)
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
			h.noteFreeExhaust(acct.UID, bareModel, credit, guardPromo)
		} else if c, ok2 := responseCredit(resp); ok2 {
			// 有 credit 但没有 total_tokens：单价算不了，积分消耗仍要记。
			recordCredit(acct.UID, c)
		}
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	// 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	// requestId 原样保留）。HTTP 状态码按 OpenAI 兼容口径映射类别：ErrSoftRate → 429
	// （限流语义、客户端应等待重试），其余保持 503。本地调度类错误（无可用账号/
	// 传输层抖动/非上游返回的 lastErr）→ 保留自有文案 no_healthy_account（本地错误
	// 没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// 池耗尽告警（v1.11）：lastErr 为空 = 纯调度性耗尽（无号/全冷却/在途占满，
	// 上游没被请求过），这种状态最容易被忽视——池子静默缩水。上游错误路径
	// （lastErr 非 nil）由 pool 侧账号级事件覆盖，不在此重复。
	if lastErr == nil && h.cfg.Notifier != nil {
		total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
		h.cfg.Notifier.Post(notify.EventPoolExhausted, "",
			"l3api 池子耗尽",
			fmt.Sprintf("请求轮转后无可用账号（total=%d healthy=%d），已返回 503——请检查账号冷却/禁用状态", total, healthy))
	}
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定；本地调度类
	// 错误（无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：网关出口 IP 被 WAF 拦截、
				// 轮转已止损、窗口过后自动解除。客户端提前重试无意义（换号不换 IP）；
				// 有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
		}
	}
	sink.WriteError(w, status, code, msg, hint)
	st.status = status
}

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// noteFreeExhaust 免费额度尽检测（free_guard 的台账写入口，两条扣费路径共用）：
// 模型处于生效的免费促销窗口（promo 非 nil）而本次实测扣费（credit>0）→
// 该 (账号, 模型) 免费配额已尽，记入台账（复位点 = promo.NextReset）。
//
// 检测**不分 key**（守护 key 与普通 key 的观测都记账）——额度是账号维度的事实，
// 守护与否只是选号策略，扣费事实一旦测得应全局共享，否则守护 key 蹭普通 key
// 的号时会重复付学费探测。promo 为 nil（无生效免费促销）→ 直接返回。
func (h *Handler) noteFreeExhaust(uid, bareModel string, credit float64, promo *upstream.ModelPromotion) {
	if promo == nil || credit <= 0 || uid == "" || bareModel == "" {
		return
	}
	h.cfg.Pool.MarkFreeExhausted(uid, bareModel, promo.NextReset(time.Now()))
}

// responseCredit 从聚合响应里只取 usage.credit（不看 total_tokens）。
// 用于「有扣费但没 token 统计」的响应：单价算不了，积分消耗仍要记账。
func responseCredit(resp map[string]any) (float64, bool) {
	u, _ := resp["usage"].(map[string]any)
	if u == nil {
		return 0, false
	}
	c, ok := u["credit"].(float64)
	if !ok || c <= 0 {
		return 0, false
	}
	return c, true
}

// usageCreditTotal 从聚合响应取 usage.credit 与 total_tokens（成本台账非流式入口）。
// 任一字段缺失/非法 → ok=false（不记录）。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0, false
	}
	c, _ := usage["credit"].(float64)
	t, _ := usage["total_tokens"].(float64)
	if t <= 0 {
		return 0, 0, false
	}
	return c, int(t), true
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：第 i 次轮转失败
// （continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，±25% 抖动），
// ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转（客户端已走，
// 换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；正常单号请求
// （首次成功）不经过本函数，零开销。
func rotateBackoff(i, maxRotate int, ctx context.Context) bool {
	// 已是最后一轮：退避没有意义（后面没有下一次尝试了），纯浪费时间预算
	// ——失败请求（429/5xx/传输错误）此前会白等 1.5~2.5s 才返回（2026-10-13 修复）。
	if i >= maxRotate-1 {
		return ctx.Err() == nil
	}
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 九条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避。冷却时长优先采信
//     Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却：**不 Disable**——WAF 403 是 IP/指纹维频控信号，
//     罚过即走、到期自愈。时长优先 Retry-After 头；缺失按 wafCooldownBase(60s)
//     起 · softStreak 指数、封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避。
//     基数经 jitterDur 抖动（防多账号同相位冷却到期再聚团）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号；passthrough 首遇触发降级重试，最终仍拦则回 400。
//   - ErrBadParams → 不罚账号（同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrPromptTooLong → 11115：请求的问题不是账号的问题。零动作（不冷却/不熔断/
//     不 NoteError、不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池。
//
// body 仅在 ErrSoftRate/ErrAccountFault 分支用于解析重置时间/分野；model 为请求
// 携带的模型名。uerr 是 ChatStreamContext 返回的分类信封（可携带 RetryAfter）；
// 零值/防御路径下为 nil，冷却时长回落既有计算。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	// 上游错误分类计数（2026-10-20）：在唯一入口按 Kind 记账——attempts 的
	// outcome 只有 success/fail，429/5xx/超时/内容拦截全混在 fail 里看不出
	// 失败构成；这个计数器让 Grafana 能按 kind 分维看。
	if h.m != nil && h.m.upstreamErrs != nil && kind != upstream.ErrNone {
		status := 0
		if uerr != nil {
			status = uerr.Status
		}
		h.m.upstreamErrs.Inc("kind", kind.String(), "status", statusLabel(status))
	}
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 冷却时长按下述优先级取「上游明说的恢复时刻」，越靠前越权威：
		//   1. body「将在 … 重置」文案 → 精确到该墙钟。无论业务 code 是 6004 还是
		//      11140 rate-limiting 等形态。
		//      - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型豁免。
		//      - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免。
		//   2. 响应头 Retry-After / retry-after-ms / x-ratelimit-reset。
		//   3. body 里的 retry_after 字段：CodeBuddy 排队类 429（code 6020/6022）
		//      会带 data.retry_after=40 —— 上游明说 40 秒后可再来。此前只认头/文案，
		//      这 40s 的等待被放大成 soft_rate 基数。
		//   4. 以上都没有 → 有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		//      soft_rate_max；已在冷却中的兜底探测不翻倍）。
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if upstream.IsModelRateLimit(body) {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
			return
		}
		// body 无重置文案但带 Retry-After 头 → 冷却到该时刻（不做指数堆加）。
		// 头优先于 body 字段：HTTP 标准头语义 > 业务自定义字段。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			return
		}
		// body 里的 retry_after（秒/毫秒）字段 → 同样冷却到该时刻。
		// 走 resetAt 分支：不推进 softStreak、不做指数堆加（上游说了多久就多久，
		// 仍受 soft_rate_max 封顶）。6004 仍保持模型级语义（只锁该模型）。
		if d, ok := upstream.ParseBodyRetryAfter(body); ok {
			if upstream.IsModelRateLimit(body) {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), time.Now().Add(d), model, "6004 model rate limit (body retry_after)")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Now().Add(d), "429 rate limit (body retry_after)")
			return
		}
		// 上游什么都没给 → 账号级有界退避，基数取 h.softCooldown()（热改优先），
		// 默认 60s：一分钟足够让频控/排队窗口滑过去，又不至于像 600s 那样把
		// 几十秒就能恢复的号白白闲置。
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Time{}, "429 rate limit")
	case upstream.ErrWafBlock:
		// WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族：基数
		// wafCooldownBase（60s，抖动后落 [45s,75s]）、softStreak 指数升级、封顶
		// soft_rate_max、冷却中兜底探测不翻倍——全部继承既有语义。
		// Retry-After 头优先（WAF 拦截页可能带该头）。不 Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		// 阈值化：一次 12153 只记数+亮徽章，连续 sessionDeadThreshold 次才禁用
		// （与 keepalive 路径同口径；见 pool.NoteSessionDead 注释的误杀史）。
		if h.cfg.Pool.NoteSessionDead(uid) {
			log.Printf("WARN: [server] uid=%s 连续 session dead 达阈值，已禁用", uidFull(uid))
		}
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：硬禁用（Disable）。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			h.cfg.Pool.Disable(uid, "account banned by upstream (11140 request illegal), re-login required")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked 待遇），
		// chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），选号侧 healthyForModel 对该账号自动避开该模型。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
		// ErrClient（未知 4xx）喂连败计数（issue #114）：连续 N 次该形态失败 →
		// 账号临时出池（NoteFailures 达阈降权），单次/偶发不罚（不误伤）。ErrNone
		// 到这里属防御路径（status>=400 但分类成功），语义不明不喂。
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// wafCooldownBase WAF 403 软冷却基数（建议 60s 起；抖动 ±25% 后落 [45s,75s]，
// 实际进入 CooldownSoftRate 后再按 softStreak 指数、封顶 soft_rate_max）。
// 与 SoftCooldown 分流的原因：WAF 403 是 IP/指纹维频控，信号比 429「账号级限流」轻
// （账号本身健康），但比 404 重（带粘性会连环）；60s 级的快速避让已足够让频控窗口
// 滑过。抖动复用 backoff.go jitterDur（单一来源）。
const wafCooldownBase = 60 * time.Second

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url part（OpenAI 兼容形态
// messages[].content[] {type:"image_url"}）。畸形/其他形态一律 false（hint 侧
// 宁缺勿滥：判不出带图就不给「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 1h 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}

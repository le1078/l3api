// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 鉴权复用网关 api_key（Bearer），与 /v1/* 同一口径；api_key 为空 = 不鉴权
//     （仅本机/私网使用）。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/l3api/internal/apikeys"
	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/httpauth"
	"github.com/linguo2625469/l3api/internal/livecfg"
	"github.com/linguo2625469/l3api/internal/pool"
	"github.com/linguo2625469/l3api/internal/provider"
	"github.com/linguo2625469/l3api/internal/scheduler"
	"github.com/linguo2625469/l3api/internal/upstream"
	"github.com/linguo2625469/l3api/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	RedisMode string               // "upstash" / "noop"，仅观测透出
	Version   string               // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder

	// Keys 多租户 key 注册表（nil = keys 接口返回 501）。与 livecfg 快照内
	// 指针同源（main 装配保证），热更经 Registry.Store 原地生效。
	Keys *apikeys.Registry
	// SaveKeys 整体替换 api_keys 数组：校验（config normalize）→ 落盘 →
	// Registry.Store 热生效。nil 时 keys 写接口返回 501。
	SaveKeys func(entries []apikeys.Entry) error

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string

	// ProbeLastFile 定时体检最近结果文件（scheduler 落盘；空或不存在 =
	// probe/last 返回 ran_at=null，面板显示「尚未执行」）。只读。
	ProbeLastFile string
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		logins:  map[string]loginSession{},
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/theme.js", p.themeScript)
	p.mux.HandleFunc("GET /panel/echarts.min.js", p.echartsScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/accounts/import", p.withAuth(p.accountImport))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.withAuth(p.accountPause))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/note", p.withAuth(p.accountNote))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/school/status", p.withAuth(p.schoolStatus))
	p.mux.HandleFunc("POST /panel/api/school/run_all", p.withAuth(p.schoolRunAll))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/probe", p.withAuth(p.probeAccounts))
	p.mux.HandleFunc("GET /panel/api/probe/last", p.withAuth(p.probeLast))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
	// API Keys 管理（v1.11 多租户）。
	p.mux.HandleFunc("GET /panel/api/keys", p.withAuth(p.keysList))
	p.mux.HandleFunc("POST /panel/api/keys/upsert", p.withAuth(p.keysUpsert))
	p.mux.HandleFunc("POST /panel/api/keys/remove", p.withAuth(p.keysRemove))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 与 /v1 同源的 Bearer 鉴权（2026-10-13 统一）：
//  1. 多 key 注册表优先（Live 快照里的 Keys）——注册表非空时，token 必须命中
//     某条目才放行（与 server.authenticate 完全同口径）；
//  2. 注册表空时回落单 api_key（httpauth 常量时间比较）；
//  3. **裸奔保护**：注册表空 且 api_key 为空时不再无脑放行——仅回环请求放行
//     （本机部署/开发场景不受影响），非回环请求一律 401。此前面板只看单 key，
//     "只配了 api_keys[] 没配 api_key" 的部署会把全部管理接口暴露给公网
//     （2026-10-13 修复；与 main 启动 WARN 是同一类问题的收口）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := httpauth.BearerToken(r)
		if p.cfg.Keys != nil {
			if _, ok := p.cfg.Keys.Verify(token); ok {
				next(w, r)
				return
			}
			if p.cfg.Keys.Len() > 0 {
				writeErr(w, http.StatusUnauthorized, "invalid_api_key")
				return
			}
		}
		if k := p.apiKey(); k != "" {
			if !httpauth.VerifyBearer(r, k) {
				writeErr(w, http.StatusUnauthorized, "invalid_api_key")
				return
			}
			next(w, r)
			return
		}
		if !isLoopbackRequest(r) {
			writeErr(w, http.StatusUnauthorized,
				"面板未配置任何密钥（api_key/api_keys 均空），非本机访问已拒绝——请在配置页设置密钥")
			return
		}
		next(w, r)
	}
}

// isLoopbackRequest 报告 TCP 对端是否为回环地址。
//
// 只看 RemoteAddr，**不看 X-Forwarded-For**：本项目的典型部署是 nginx 反代
// （后端看到的 RemoteAddr 恒为 nginx 的 127.0.0.1），反代场景的防护由 nginx
// 层承担；XFF 是客户端可伪造的头，拿它做"本机放行"判定等于把口子交给伪造者。
// 裸绑公网（无反代）时 RemoteAddr 才是真实客户端地址，此时非回环请求会被
// 上面的「无密钥拒绝」分支挡住——这正是要防的场景。
func isLoopbackRequest(r *http.Request) bool {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost" || host == "[::1]"
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "pool not available")
		return
	}
	total, healthy, cooling, disabled, inFlightFull := p.cfg.Pool.CountsDetailed()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	// 在途请求：瞬时负载信号（池打满与否先看这里）。
	inFlight := 0
	for _, st := range p.cfg.Pool.List() {
		inFlight += st.InFlight
	}
	// 今日产出（usage 可能未启用，零值即可）。今日积分消耗与这两个数同一次
	// 扫描取回（v1.16.9）：顶部「积分剩余 / 总额」卡要同时展示今日消耗。
	var todayReq, todayTok int64
	var todayCred float64
	if p.cfg.Usage != nil {
		todayReq, todayTok, todayCred = p.cfg.Usage.TodaySummary()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"redis_mode":      p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"in_flight":       inFlight,
		"today_requests":  todayReq,
		"today_tokens":    todayTok,
		// today_credits 今日积分消耗（今日小时桶里 usage.credit 之和，v1.16.9）。
		// 保留两位小数：上游 credit 是小数，浮点累加后的尾数（12.340000000002）
		// 直接透出会让前端格式化很别扭。
		"today_credits": math.Round(todayCred*100) / 100,
		"accounts":      p.accountsView(),
	})
}

// accountsView 池状态快照 + 每账号备注（Auth.Note）。备注存 auth 文件（顶层 note 键），
// 不进 pool 状态结构；此处序列化后附上，供前端展示与编辑。
func (p *Panel) accountsView() []map[string]any {
	sts := p.cfg.Pool.List()
	out := make([]map[string]any, 0, len(sts))
	for _, st := range sts {
		b, err := json.Marshal(st)
		if err != nil {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil {
			m["note"] = a.Note
		}
		out = append(out, m)
	}
	return out
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 无可用账号 503（先添加账号）；上游失败 502。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	// 优先用 codebuddy 账号拉动态模型目录（FetchModels 走 codebuddy /v3 + 企业端点；
	// qoder 模型走下方静态目录，不依赖某个具体账号在线）。无 codebuddy 账号时退回 Pick()。
	// 无副作用探测（不写 lastUsed）：面板浏览不应扰动选号权重。
	// CN 目录用 CN 域 codebuddy 账号；国际版目录用 global 域账号（v1.12 拆分：
	// 此前只挑第一个 codebuddy 账号，global 目录会被错标成 cn: 前缀）。
	var acctCN, acctGlobal *auth.Auth
	for _, st := range p.cfg.Pool.List() {
		if st.Brand != string(provider.CodeBuddy) {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if st.Realm == "global" {
			if acctGlobal == nil {
				acctGlobal = a
			}
		} else if acctCN == nil {
			acctCN = a
		}
	}
	if acctCN == nil && acctGlobal == nil {
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
		return
	}
	var infos []upstream.ModelInfo
	if acctCN != nil {
		var err error
		infos, err = p.cfg.Upstream.FetchModels(acctCN)
		if err != nil {
			// 拉取动态模型目录失败不致命：qoder 静态目录不依赖账号在线，仍应展示。
			log.Printf("[panel] models: fetch codebuddy CN models failed (qoder static list still served): %v", err)
			infos = nil
		}
	}
	out := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		entry := map[string]any{
			"id": mi.ID,
			// call_id 客户端真实可调用的模型名（网关路由协议，resolveModel 剥前缀）：
			// codebuddy 动态目录走 cn 域 → "cn:"+id。前端展示以此为准。
			"call_id":              "cn:" + mi.ID,
			"name":                 mi.Name,
			"brand":                string(provider.CodeBuddy), // CN 段：本循环只装 CN 目录（global 走下方 codebuddy-global 段）
			"badge":                mi.Badge, // 营销徽章（限时免费/夜间免费/夜间折扣，上游 tags 实时值）
			"default_effort":       mi.DefaultEffort,
			"supported_efforts":    mi.Efforts,
			"can_disable_thinking": mi.CanDisableThinking,
			"supports_reasoning":   mi.SupportsReasoning,
			"supports_images":      mi.SupportsImages,
			"credits":              mi.Credits,
			"description":          mi.Description,
			"tags":                 mi.Tags,
			"vendor":               mi.Vendor,
			"is_default":           mi.IsDefault,
			"supports_tool_call":   mi.SupportsToolCall,
			"only_reasoning":       mi.OnlyReasoning,
			"reasoning_effort":     mi.ReasoningEffort,
			"reasoning_summary":    mi.ReasoningSummary,
		}
		if mi.MaxAllowedSize > 0 {
			entry["max_allowed_size"] = mi.MaxAllowedSize
		}
		// 与 /v1/models 同口径：context_length / max_output_tokens 走四级查找链
		// （上游动态值 → 静态知识表 → model.json → models.dev → 1M 兜底），
		// effort 档位走 EffortListing（远端权威 ∪ CN 静态兜底表）——面板展示的
		// 数值即客户端实际拿到的数值，两侧不再漂移。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, p.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, p.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
			if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
				entry["supported_efforts"] = efforts
				if def != "" {
					entry["default_effort"] = def
				}
			}
			// 营销/折扣活动（带准确时段）：结构化促销优先，无则回落徽章兜底窗口。
			// active 现算——面板每次点「重新获取」都重新拉目录，不缓存 bool。
			if pm := upstream.PromotionJSON(mi.EffectivePromotion(), time.Now()); pm != nil {
				entry["promotion"] = pm
			}
			out = append(out, entry)
		}
	// 追加国际版（global realm）目录：品牌标 codebuddy-global（与 CN 区分），
	// call_id 带 global: 前缀（与 /v1/models 的路由协议一致）。
	if acctGlobal != nil {
		ginfos := p.cfg.Upstream.FetchGlobalModelInfos(acctGlobal)
		for _, mi := range ginfos {
			entry := map[string]any{
				"id":                   mi.ID,
				"call_id":              "global:" + mi.ID,
				"name":                 mi.Name,
				"brand":                "codebuddy-global",
				"vendor":               "WorkBuddy 国际版",
				"badge":                mi.Badge,
				"default_effort":       mi.DefaultEffort,
				"supported_efforts":    mi.Efforts,
				"can_disable_thinking": mi.CanDisableThinking,
				"supports_reasoning":   mi.SupportsReasoning,
				"supports_images":      mi.SupportsImages,
				"credits":              mi.Credits,
				"description":          mi.Description,
				"tags":                 mi.Tags,
				"is_default":           mi.IsDefault,
				"supports_tool_call":   mi.SupportsToolCall,
			}
				if mi.ContextWindow > 0 {
					entry["context_length"] = mi.ContextWindow
				}
				if mi.MaxTokens > 0 {
					entry["max_output_tokens"] = mi.MaxTokens
				}
				// 国际版促销（实测 global 域 modelPromotions 为 0 条，故通常无此字段；
				// 上游若日后下发则自动生效，与 CN 段同口径）。
				if pm := upstream.PromotionJSON(mi.EffectivePromotion(), time.Now()); pm != nil {
					entry["promotion"] = pm
				}
				out = append(out, entry)
			}
		}
	// 追加 Qoder 静态模型目录（仅作能力展示，不依赖账号在线；与 /v1/models 同源）。
	for _, qm := range upstream.QoderChatModels() {
		entry := map[string]any{
			"id":                 qm.ID,
			"call_id":            "qoder:" + qm.ID,
			"name":               qm.DisplayName,
			"brand":              string(provider.Qoder),
			"vendor":             "Qoder (阿里)",
			"supports_reasoning": qm.IsReasoning,
			"supports_images":    qm.IsVL,
			"is_default":         qm.ID == "auto",
		}
		if qm.MaxInputTokens > 0 {
			entry["context_length"] = qm.MaxInputTokens
		}
		out = append(out, entry)
	}
	// 追加 Trae 静态模型目录（不走模型路由，仅作能力展示）。
	for _, tm := range upstream.TraeChatModels() {
		out = append(out, map[string]any{
			"id": tm.ID, "call_id": "trae:" + tm.ID, "name": tm.Name, "brand": string(provider.Trae),
			"vendor": "Trae (字节)", "is_default": false,
			"supports_reasoning": tm.IsReasoning, "supports_images": tm.IsVL,
			"context_length": tm.MaxTokens,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountPause 暂停/恢复选号（与禁用正交：保号任务照跑）。body {"paused": bool}。
func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body")
		return
	}
	p.cfg.Pool.SetPaused(uid, body.Paused)
	log.Printf("panel: pause uid=%s paused=%v", uid, body.Paused)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": body.Paused})
}

// markSessionDead 手动操作路径拿到 ErrSessionDead（401 session 过期）时给账号打
// 失效观测标志——面板据此亮「Session 过期」徽章，不再只有日志里一行 401。
func markSessionDead(pl *pool.Pool, uid string, err error) {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		pl.MarkSessionDead(uid)
	}
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
		markSessionDead(p.cfg.Pool, uid, err)
	}
	resp := map[string]any{"ok": true}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	// 余额查询按 Balance 能力门控：trae 未接通（积分余额端点未确认），跳过而非吃 401。
	if provider.For(provider.Brand(a.Brand())).Capabilities().Balance {
		remain, total, err := p.cfg.Upstream.UserResource(a)
		if err != nil {
			resp["balance_error"] = err.Error()
			markSessionDead(p.cfg.Pool, uid, err)
		} else {
			// 签到后查余额：走强解冻（签到跨了计费周期，429/模型级冷却一并清）。
			p.cfg.Pool.ReviveAfterCheckin(uid, remain, total)
			resp["credits"] = remain
			resp["credits_total"] = total
			log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：UserResource → SetCredits（不触碰冷却状态）。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// 品牌门控：trae 余额未接通，给明确提示而非 401 堆栈。
	if !provider.For(provider.Brand(a.Brand())).Capabilities().Balance {
		writeErr(w, http.StatusNotImplemented, "该品牌暂未接通余额查询（trae 积分余额端点未确认）")
		return
	}
	remain, total, err := p.cfg.Upstream.UserResource(a)
	if err != nil {
		markSessionDead(p.cfg.Pool, uid, err)
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCredits(uid, remain, total)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountNote 编辑账号备注（Auth.Note，落盘 auth 文件顶层 note 键）。
// 空串 = 清除备注。仅运维标注用途，不影响路由与调度。上限 100 字（rune 截断）。
func (p *Panel) accountNote(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	note := strings.TrimSpace(req.Note)
	if rs := []rune(note); len(rs) > 100 {
		note = string(rs[:100])
	}
	a.Lock()
	a.Note = note
	a.Unlock()
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save note: "+err.Error())
		return
	}
	log.Printf("panel: note uid=%s set (%d runes)", uid, len([]rune(note)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	brands := brandsParam(r)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panel: manual task RunCheckinNow panic recovered: %v", r)
			}
		}()
		p.cfg.Scheduler.RunCheckinNow(brands...)
	}()
	log.Printf("panel: 手动签到已触发（含猫猫旅行）brand=%v", brands)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "brand": brands})
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	brands := brandsParam(r)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panel: manual task RunTravelNow panic recovered: %v", r)
			}
		}()
		p.cfg.Scheduler.RunTravelNow(brands...)
	}()
	log.Printf("panel: 手动旅行巡检已触发 brand=%v", brands)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "brand": brands})
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	brands := brandsParam(r)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panel: manual task RunActivityNow panic recovered: %v", r)
			}
		}()
		p.cfg.Scheduler.RunActivityNow(brands...)
	}()
	log.Printf("panel: 手动全量活跃上报已触发 brand=%v", brands)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "brand": brands})
}

// keepaliveAll 手动触发全量 token 保活（异步执行）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	brands := brandsParam(r)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panel: manual task RunKeepaliveNow panic recovered: %v", r)
			}
		}()
		p.cfg.Scheduler.RunKeepaliveNow(brands...)
	}()
	log.Printf("panel: 手动全量保活已触发 brand=%v", brands)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "brand": brands})
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	brands := brandsParam(r)
	p.cfg.Scheduler.RunBalanceRefreshNow(brands...)
	log.Printf("panel: 手动全量余额刷新完成 brand=%v", brands)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。hours 查询参数控制小时粒度时序窗口（默认 72，
// 上限 1440=60 天）；更早的数据自动折叠为日点，因此长期趋势不会丢。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	hours := 72
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	if hours > 1440 {
		hours = 1440
	}
	// 昵称/品牌仅用于展示，取自池快照（不含任何凭证）。
	nicks := map[string]string{}
	brands := map[string]string{}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
		brands[s.UID] = string(s.Brand)
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.Snapshot(hours, nicks, brands))
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Brand    string                   `json:"brand,omitempty"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm, Brand: s.Brand}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			// 按品牌分派：codebuddy 走 billingMeterJSON 多包明细；qoder/trae 走各自余额接口。
			switch a.Brand() {
			case "qoder":
				r, t, err := p.cfg.Upstream.QoderQuota(r.Context(), a)
				if err != nil {
					it.Error = err.Error()
					out[i] = it
					return
				}
				it.Remain, it.Size = r, t
				it.Packages = []upstream.CreditPackage{{Name: "Qoder 总池", Remain: r, Size: t}}
			case "trae":
				r, t, err := p.cfg.Upstream.TraeQuota(r.Context(), a)
				if err != nil {
					it.Error = err.Error()
					out[i] = it
					return
				}
				it.Remain, it.Size = r, t
				it.Packages = []upstream.CreditPackage{{Name: "Trae 总池", Remain: r, Size: t}}
			default: // codebuddy
				packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
				if err != nil {
					it.Error = err.Error()
					out[i] = it
					return
				}
				it.Packages = packs
				it.Remain = remain
				it.Size = size
			}
			out[i] = it
		}(i, s)
	}
	wg.Wait()

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// brandsParam 从 query 解析 ?brand= 参数（可多次出现 → 多品牌并行过滤）。
// 空列表表示「全部品牌」，与改造前全量语义一致（面板不传 brand 即全量）。
func brandsParam(r *http.Request) []string {
	return r.URL.Query()["brand"]
}

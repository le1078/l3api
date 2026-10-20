// main.go l3api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/linguo2625469/l3api/internal/docsheet"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	// tzdata：内嵌 IANA 时区库（+450KB）。促销时段判定（upstream.promoLoc）
	// 要按上游 schedule.timezone 解析 Asia/Shanghai 等时区——镜像可能没有
	// /usr/share/zoneinfo（LoadLocation 失败 → 回落 UTC+8 固定，上游一旦发
	// 非 +8 时区窗口判定就偏）。内嵌后恒可解析，与 promoTZ 兜底互补。
	_ "time/tzdata"

	"github.com/linguo2625469/l3api/internal/apikeys"
	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/livecfg"
	"github.com/linguo2625469/l3api/internal/notify"
	"github.com/linguo2625469/l3api/internal/panel"
	"github.com/linguo2625469/l3api/internal/pool"
	"github.com/linguo2625469/l3api/internal/redisstore"
	"github.com/linguo2625469/l3api/internal/scheduler"
	"github.com/linguo2625469/l3api/internal/server"
	"github.com/linguo2625469/l3api/internal/session"
	"github.com/linguo2625469/l3api/internal/upstream"
	"github.com/linguo2625469/l3api/internal/usage"
)

// appVersion 网关版本（fork 版：面板 + 任务体系），透出到 /panel/api/overview。
const appVersion = "1.17.3-panel"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
// docsSheetClient 按配置构建腾讯文档推送客户端；未启用返回 nil（scheduler 侧
// nil = 不推送）。启动装配与 saveConfig 热更共用。
func docsSheetClient(c *Config) *docsheet.Client {
	if !c.DocsSheet.Enabled {
		return nil
	}
	return &docsheet.Client{
		Token: c.DocsSheet.Token, FileID: c.DocsSheet.FileID, SheetID: c.DocsSheet.SheetID,
		// 大表写入耗时长（SyncModels 两笔 MCP 调用，~650 单元格实测可达数十秒），
		// docsheet 默认 30s client 超时会截断写入（2026-10-12 实测踩中），放宽到 3 分钟。
		HTTP: &http.Client{Timeout: 3 * time.Minute},
	}
}

func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 三因子加权调优（从 config 注入，
	// 非正值回退默认）。
	// 事件告警（v1.11）：账号禁用/熔断/积分耗尽（pool 内挂钩）+ 签到失败/余额偏低
	// （scheduler）+ 池耗尽（handler）。notify.Cooldown 去重防风暴；未启用时 Post 全空操作。
	notifier := notify.New(notify.Config{
		Enabled:           cfg.Notify.Enabled,
		Events:            cfg.Notify.Events,
		Channels:          cfg.Notify.Channels,
		Cooldown:          cfg.Notify.Cooldown,
		BalanceLowCredits: cfg.Notify.BalanceLowCredits,
	})
	defer notifier.Stop()
	p.SetNotifier(notifier)

	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCreditFloor(cfg.Pool.CreditFloor)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	// 聊天 SSE 流总时长硬顶（2026-10-20）：防上游 keepalive 拖流无限期占用
	// 在途名额。缺省 1800s；配 0 = 不设（显式禁用）。
	up.StreamTotalTimeout = time.Duration(cfg.Upstream.StreamTotalTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))
	// model_sync.json 品牌静态目录覆盖文件（qoder/trae 每日自动同步落盘，
	// QoderChatModels/TraeChatModels 读取时整体替换种子；modelsync.go）。
	upstream.SetModelSyncPath(stateSibling(cfg.StateFile, "model_sync.json"))

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
		Notifier:           notifier,
		BalanceLowCredits:  cfg.Notify.BalanceLowCredits,
		ModelSyncDisabled:  !cfg.Schedule.ModelSyncEnabled,
		ModelSyncTime:      cfg.Schedule.ModelSyncTime,
		ProbeHours:         cfg.Schedule.ProbeHours,
		ProbeDisabled:      !cfg.Schedule.ProbeEnabled,
		ProbeStatePath:     stateSibling(cfg.StateFile, "probe_last.json"),
		DocsSheet:          docsSheetClient(cfg),
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	switch {
	case !cfg.Schedule.ProbeEnabled:
		log.Printf("定时体检已禁用（schedule.probe_enabled=false）")
	default:
		log.Printf("定时体检已启用：%v 点（全账号最小真实探测，结果落盘 probe_last.json）", cfg.Schedule.ProbeHours)
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/多 key 注册表/soft_rate/脱敏开关），面板保存配置时在线替换。
	// Keys 注册表指针跨热更保持同一实例（面板 Keys 字段同源）：热改走 Registry.Store
	// 原地换快照，而不是给 livecfg 换新指针——否则面板持有的注册表会与 handler 漂移。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		Keys:                 cfg.KeyRegistry,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// chatHandler 前置声明：panel 的 SaveConfig 闭包要拿到 handler 以热应用
	// server.max_body_mb，而 handler 的 Config.Panel 又依赖 pn——装配循环用
	// 变量前置 + saveConfig 内 nil 保护解开（SaveConfig 只在请求期被调，彼时
	// handler 必已就位）。
	var chatHandler *server.Handler
	pn := panel.New(panel.Config{
		Pool:        p,
		Usage:       rec,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		Version:     appVersion,
		Live:        live,
		// 多租户 key 管理（v1.11）：注册表与 livecfg 快照同指针（上同）。
		Keys: cfg.KeyRegistry,
		SaveKeys: func(entries []apikeys.Entry) error {
			return saveKeys(entries, *cfgPath, cfg.KeyRegistry)
		},
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile: stateSibling(cfg.StateFile, "output_probes.json"),
		// 定时体检最近结果（scheduler 落盘，面板「上次定时体检」读取）。
		ProbeLastFile: stateSibling(cfg.StateFile, "probe_last.json"),
		ConfigPath:    *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch, chatHandler, notifier)
		},
	})
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		Notifier:     notifier,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled:  cfg.Global.Enabled,
		// MB → 字节；0（缺省/不限）左移后仍是 0，handler 侧据此跳过 413 检查。
		MaxBodyBytes:   int64(cfg.Server.MaxBodyMB) << 20,
		MetricsEnabled: cfg.Metrics.Enabled,
		Version:        appVersion,
	})
	chatHandler = h

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	sch.StartModelSync(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// max_body_mb 缺省/0 = 不限（2026-10-18）后，多图/长上下文会话体可达
		// 数十 MB，慢链路上传轻松超过 60s → 请求在读体阶段被掐且错误不可读
		// （客户端只看到连接重置）。2026-10-20 放宽到 300s：仍防无限慢速
		// 上传（恶意/故障客户端），但不再掐合法大请求。
		// 若配置了具体上限（MB），按上限动态收紧：8MB 内 60s 足够，
		// 上限越大给越多时间（每 MB 5s，仍封顶 300s）。
		ReadTimeout: readTimeoutFor(cfg.Server.MaxBodyMB),
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("l3api listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	// 公网裸奔硬告警：空 api_key（不鉴权）+ 监听非回环地址 = 所有接口（含面板
	// 写接口：加账号/改配置/执行任务）对网络完全开放。本地预览可忽略；云部署
	// 看到本告警必须立即收敛 listen 到 127.0.0.1 或配置强 api_key。
	if cfg.APIKey == "" && !isLoopbackListen(cfg.Listen) {
		log.Printf("⚠️⚠️⚠️  WARN: api_key 为空且监听地址 %q 非回环——网关当前对网络无鉴权开放！"+
			" 云部署请设强 api_key 或将 listen 收窄为 127.0.0.1:7863 并走反代。", cfg.Listen)
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// (":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回)。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// readTimeoutFor 按请求体上限推导 http.Server.ReadTimeout（覆盖含 body 的
// 整个请求读取）。
//
//   - maxMB <= 0（不限，2026-10-18 起的缺省口径）：300s。多图/长上下文会话
//     每轮以 base64 重发历史图片，数十 MB 体在慢链路上传可超过 60s——
//     旧固定 60s 会把这些合法请求掐在读体阶段（客户端只看到连接重置，
//     排障时完全看不出是超时）。300s 仍防无限慢速上传。
//   - maxMB > 0（配了具体上限）：每 MB 5s，最少 60s，封顶 300s。
//     8MB → 60s（原行为），64MB → 300s。
func readTimeoutFor(maxMB int) time.Duration {
	const (
		floor = 60 * time.Second
		cap   = 300 * time.Second
		perMB = 5 * time.Second
	)
	if maxMB <= 0 {
		return cap // 不限：按最宽松档
	}
	d := time.Duration(maxMB) * perMB
	if d < floor {
		d = floor
	}
	if d > cap {
		d = cap
	}
	return d
}

// isLoopbackListen 判定 listen 地址是否只绑定回环（127.0.0.1 / ::1 / localhost）。
// ""/":7863"/"0.0.0.0:7863"/"[::]:7863" 等非回环形态（含全网卡绑定）返回 false。
// 仅用于启动时的「空 api_key 公网裸奔」告警判定，不参与任何访问控制决策。
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// 无法解析的形态（如裸 ":7863" 其实可解析；走到这里是异常输入）按非回环处理，
		// 宁可误告警不漏告警。
		return false
	}
	if host == "" {
		return false // ":7863" = 绑所有网卡
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval
//   - server.max_body_mb → handler.SetMaxBodyBytes（issue #17：面板改完即时生效，不再"静默不生效还重启也不提示"）
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler, srv *server.Handler, notifier *notify.Notifier) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	if err := writeConfigAtomic(path, out); err != nil {
		return nil, err
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	// key 注册表保持指针身份：面板与 livecfg 快照持有的是同一 Registry 实例，
	// 这里把新配置解析出的条目 Store 进既有实例（原地热更），而不是换指针。
	keyReg := live.Load().Keys
	if keyReg != nil {
		if newCfg.KeyRegistry != nil {
			keyReg.Store(newCfg.KeyRegistry.Entries())
		} else {
			keyReg.Store(nil) // 新配置全空 = 鉴权关闭（空注册表，authenticate 回落放行口径）
		}
	}
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		Keys:                 keyReg,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
	})
	up.SanitizeFingerprints = newCfg.Features.SanitizeBlacklistFingerprints
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetCreditFloor(newCfg.Pool.CreditFloor)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.ProbeHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.ProbeEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetModelSync(!newCfg.Schedule.ModelSyncEnabled, newCfg.Schedule.ModelSyncTime)
	sch.SetBalanceLowCredits(newCfg.Notify.BalanceLowCredits)
	sch.SetDocsSheet(docsSheetClient(newCfg))
	if notifier != nil {
		if err := notifier.Store(notify.Config{
			Enabled:           newCfg.Notify.Enabled,
			Events:            newCfg.Notify.Events,
			Channels:          newCfg.Notify.Channels,
			Cooldown:          newCfg.Notify.Cooldown,
			BalanceLowCredits: newCfg.Notify.BalanceLowCredits,
		}); err != nil {
			return nil, err
		}
	}
	// srv 为 nil 仅出现在装配未完成的窗口（SaveConfig 只在请求期被调，理论不可达），
	// 跳过热应用即可——下次重启仍会从落盘的 config.json 读到新值。
	if srv != nil {
		// 0 直接透传（=不限）；热生效，与进程启动时同口径。
		srv.SetMaxBodyBytes(int64(newCfg.Server.MaxBodyMB) << 20)
	}

	return restartRequiredFields(newCfg), nil
}

// osRename 测试注入点（writeConfigAtomic 的 rename 步骤）。
var osRename = os.Rename

// writeConfigAtomic 原子写配置：tmp + rename 优先。rename 撞上 Docker 单文件
// bind mount 时（目标是挂载点，内核拒绝被替换，报 EBUSY "device or resource
// busy"——已在容器实测）回退为原地截断写。原地写失去崩溃原子性（写一半断电
// 可能损坏 config.json），但配置文件 KB 级、写窗极短，且是单文件挂载形态下的
// 唯一可行解；要完整原子性请把 compose 挂载改为目录（见 DEPLOY.md）。
func writeConfigAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := osRename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		// 原地截断写：config.json 是系统命门，截断+重写之间掉电/崩溃会留半截
		// JSON，进程下次启动直接起不来。收窄窗口：先 fsync 旧文件（保证截断前
		// 内容已落盘，文件系统恢复时更可能拿到完整旧版本），写完再 fsync。
		if f, oerr := os.OpenFile(path, os.O_WRONLY, 0o600); oerr == nil {
			_ = f.Sync()
			_ = f.Close()
		}
		if werr := writeFileSync(path, data); werr != nil {
			return fmt.Errorf("replace config: rename 失败(%v)且原地写失败: %w", err, werr)
		}
		// 写完立刻回读解析一遍：半截写/截断损坏当场暴露，错误冒泡给面板，
		// 而不是等下次重启才发现配置已坏。
		if back, rerr := os.ReadFile(path); rerr != nil {
			return fmt.Errorf("replace config: 原地写后回读失败: %w", rerr)
		} else {
			var v any
			if jerr := json.Unmarshal(back, &v); jerr != nil {
				return fmt.Errorf("replace config: 原地写后回读校验失败（config.json 可能已损坏，请检查）: %w", jerr)
			}
		}
	}
	return nil
}

// writeFileSync 写文件并在关闭前 fsync（writeConfigAtomic 的 EBUSY 原地写回退路径用）。
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// saveKeys 面板 API Keys 保存路径（panel/keys.go 注入调用）：
// 整体替换 config.json 的 api_keys 数组 → 同一套 normalize 校验 → 原子落盘 →
// Registry.Store 热生效。config 其余键保持原样（只动 api_keys 一个键）。
//
// 注册表条目以 normalize 输出为准（含 default 合成条目），livecfg 快照内的
// Registry 指针保持身份不变——与 saveConfig 的热更口径一致。
func saveKeys(entries []apikeys.Entry, path string, reg *apikeys.Registry) error {
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read current config: %w", err)
	}
	var cur map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		return fmt.Errorf("parse current config: %w", err)
	}
	// entries 序列化进 map：与面板配置页同一条链路（map 合并 → ParseConfig 校验），
	// 保证 api_keys 的校验规则（空 key/重复/上限）只有一份实现。
	raw, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal keys: %w", err)
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return fmt.Errorf("reparse keys: %w", err)
	}
	cur["api_keys"] = arr

	newCfg, err := ParseConfig(mergedJSON(cur))
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := writeConfigAtomic(path, out); err != nil {
		return err
	}

	if reg != nil {
		if newCfg.KeyRegistry != nil {
			reg.Store(newCfg.KeyRegistry.Entries())
		} else {
			reg.Store(nil) // api_keys+api_key 全清空 = 关闭鉴权（与启动语义一致）
		}
	}
	return nil
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段名。
// 恒返回完整清单中的"与当前进程装配期依赖相关"的项——面板据此提示用户。
func restartRequiredFields(c *Config) []string {
	var out []string
	// 这些字段在进程内被监听地址/HTTP client/目录句柄等装配期对象捕获。
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		out = append(out, "upstash")
	}
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval")
	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if k == "channels" {
			// notify.channels：数组整体覆盖时，渠道密钥（secret/smtp_password）
			// 面板回显已脱敏为空串，按索引从旧配置继承，防「读-改-写」丢密码。
			v = mergeChannels(cur[k], v)
		}
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergeChannels 渠道数组按索引继承未提交（缺键或空串）的密钥字段。
// 渠道顺序由面板行顺序决定，索引对齐即可靠；新增渠道在旧数组无对应项则跳过。
func mergeChannels(curV, inV any) any {
	inArr, ok := inV.([]any)
	if !ok {
		return inV
	}
	curArr, _ := curV.([]any)
	for i, e := range inArr {
		em, ok := e.(map[string]any)
		if !ok || i >= len(curArr) {
			continue
		}
		om, ok := curArr[i].(map[string]any)
		if !ok {
			continue
		}
		for _, f := range []string{"secret", "smtp_password"} {
			if v, has := em[f]; !has || v == "" {
				if old, ok := om[f].(string); ok && old != "" {
					em[f] = old
				}
			}
		}
	}
	return inArr
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

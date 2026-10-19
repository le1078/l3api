// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/l3api/internal/apikeys"
	"github.com/linguo2625469/l3api/internal/notify"
	"github.com/linguo2625469/l3api/internal/prompt"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	// APIKeys 多租户 key 集合（v1.11）：每把 key 独立名称/白名单/限流。
	// 与 api_key 的关系：api_key 非空时自动合成 name="default" 的条目并入
	// 注册表（api_keys 为空时注册表只有这一条，老客户端零回归）；两者重复时
	// api_keys 优先（同名/同 key 只保留 api_keys 声明）。
	APIKeys []apikeys.Entry `json:"api_keys,omitempty"`

	Server struct {
		// MaxBodyMB 聊天请求体大小上限（单位 MB）。
		// 请求体超过该值直接返回 413 request_body_too_large，不再静默截断后喂给上游
		// （issue #41：截断的 JSON 让上游 unmarshal 报 unexpected EOF，网关却罚号）。
		//
		// 缺省/0 = 不限（2026-10-18 改口径）：多图会话与长上下文客户端每轮把历史
		// 图片以 base64 重发，实测轻松越过原先的 8MB 默认（Anthropic /v1/messages
		// 端点踩了一串 413）；网关本就该把"多大算过大"交给上游判定，本地硬限只是
		// 把可用性问题前置成 413。要恢复限制显式填正整数（如 50）；上界 1024，
		// 超出 normalize 报错。负值仍非法（防 "-1" 被当成不限的误配）。
		MaxBodyMB int `json:"max_body_mb"`
	} `json:"server"`

	Metrics struct {
		// Enabled /metrics 端点开关（缺省 true）。端点不鉴权：安全边界是回环
		// 监听 + 反代拦截 /metrics（部署文档有样例）。false = 完全不注册路由。
		Enabled bool `json:"enabled"`
	} `json:"metrics"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		// SoftRate 软冷却基数（默认 "60s"）。仅在**上游没有给出任何重试时间**时
		// 使用：上游明说「将在 … 重置」/ 响应头 Retry-After / body retry_after 字段
		// 时一律以上游为准。无上游时间时的连续触发按 softStreak 指数退避。
		SoftRate string `json:"soft_rate"`
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		TravelHours    []int `json:"travel_hours"`    // [9,21]
		ActivityHours  []int `json:"activity_hours"`  // [10]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		BlackcatHours  []int `json:"blackcat_hours"`  // [23] 夜猫子窗口（23:00–08:00 计数）
		// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/BlackcatEnabled 显式禁用开关（缺省 true）。
		//
		// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
		//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
		//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
		//     则对老配置零影响，向后完全兼容。
		//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
		//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
		// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
		BlackcatEnabled  bool `json:"blackcat_enabled"`  // 缺省 true；false = 关夜猫子

		// 余额后台周期刷新：两次签到时点之间 credits 也能保持新鲜（面板/状态观测用）。
		// 解冻语义同签到（余额 > 0 的冷却账号自动解冻），但不做签到不刷 token。
		BalanceRefreshEnabled bool `json:"balance_refresh_enabled"` // 缺省 true；false = 关闭
		BalanceRefreshMinutes int  `json:"balance_refresh_minutes"` // 缺省 5；<=0 回落 5

		// 品牌模型目录每日自动同步（qoder/trae 静态目录；codebuddy 动态目录不在此列）：
		// 到点探测官方目录 → diff →（trae 新增实测门）→ 落盘 model_sync.json 覆盖生效，
		// 变化汇总走 notify 告警。
		ModelSyncEnabled bool   `json:"model_sync_enabled"` // 缺省 true；false = 关闭
		ModelSyncTime    string `json:"model_sync_time"`    // 缺省 "03:30"（CST 每日时点）

		// 定时体检：到点对全账号发一次最小真实 chat 探测（同面板「检验全部账号」
		// 口径，直连上游、绕过池记账），结果落盘 data/probe_last.json 供面板展示，
		// 失败汇总走 notify 告警。
		ProbeEnabled bool  `json:"probe_enabled"` // 缺省 true；false = 关闭
		ProbeHours   []int `json:"probe_hours"`   // 缺省 [9]
	} `json:"schedule"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// StreamTotalTimeoutSeconds 聊天 SSE 流总时长硬顶（2026-10-20）。
		// 缺省（未设置）→ 1800（30min）；显式 0 = 禁用硬顶；负数归一为禁用。
		// 区分「未设置」与「显式 0」靠 streamTotalRaw（指针，json 忽略）：
		// 缺省时指针为 nil，显式 0 时非 nil 且值为 0——否则「禁用」会被
		// 缺省补齐逻辑补成 1800。
		// 与 idle 的分工：idle 管静默多久，本项管总共多久——上游每 N 分钟吐
		// 1 字节 keepalive 时 idle 永不触发，流会无限期占用账号在途名额
		// （几条这样的流就把池子打满、正常请求全 503）。到点强制断流。
		StreamTotalTimeoutSeconds int  `json:"stream_total_timeout_seconds"`
		streamTotalRaw            *int `json:"-"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// 默认值已对齐官方 WorkBuddy 桌面形态（三段式），用户仍可配完全自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与归属头
		// X-IDE-Version）。空 = 内置默认（对齐官方 5.5.4 分发包）。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（官方内置 CLI 2.137.1）。
		CliVersion string `json:"cli_version"`
		// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
		// 空 = 旧行为 X-Product="SaaS" 不设 X-IDE-*；配 "WorkBuddy" 则四头跟随。
		ClientName string `json:"client_name"`
		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底；空 = 不注入。
		// 每号 auth 文件的 device_token 键优先于本项。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile device token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
		DeviceTokenFile string `json:"device_token_file"`
		// PassthroughIP 是否透传客户端 IP 给上游（默认 false，反代安全边界）。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer。
		Mode string `json:"mode"` // "passthrough" / "custom"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal  int    `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 风控紧域压低并发），0 = 回落默认 2
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114）：ErrClient/传输层这类「不罚号」失败连续计数，达阈
		// 临时出池。与冷却/熔断并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长，默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权封顶，默认 "2h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// CreditFloor 积分保底（默认 0 = 关闭）：余额低于此线的账号对已实测收费
		// 模型不再参与选号（防收费模型打穿余额连累免费模型；免费/未知模型照常）。
		CreditFloor int64 `json:"credit_floor"`
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到/余额刷新时，到期时间在
		// 此窗口内的积分被标记为"快过期"，选号优先消耗。空/0 = 禁用分桶。
		ExpiringSoon string `json:"expiring_soon"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// DocsSheet 腾讯文档共享表格推送（模型目录同步变化后整块重写模型表）。
	// 通道为腾讯文档 MCP 端点（docs.qq.com/openapi/mcp，Bearer token）。
	DocsSheet struct {
		Enabled bool   `json:"enabled"`  // 缺省 false
		Token   string `json:"token"`    // MCP Bearer token（敏感，只进 config 不外发）
		FileID  string `json:"file_id"`  // 分享链接 /sheet/ 后的串
		SheetID string `json:"sheet_id"` // 子表 tab id（?tab= 后的串）
	} `json:"docs_sheet"`

	// Notify 事件告警（v1.11）：账号禁用/熔断/积分耗尽/签到失败/余额偏低/池耗尽
	// 外推到 Telegram/Bark/企业微信/钉钉/通用 webhook。字段校验在 notify.Validate。
	Notify struct {
		Enabled           bool             `json:"enabled"`
		Events            []string         `json:"events,omitempty"`
		Channels          []notify.Channel `json:"channels,omitempty"`
		Cooldown          string           `json:"cooldown,omitempty"`
		BalanceLowCredits int64            `json:"balance_low_credits,omitempty"`
	} `json:"notify"`

	// 解析后
	SoftRateDur            time.Duration `json:"-"`
	SoftRateMaxDur         time.Duration `json:"-"`
	BreakerCooldownDur     time.Duration `json:"-"`
	BreakerCooldownMaxD    time.Duration `json:"-"`
	DegradeCooldownDur     time.Duration `json:"-"`
	DegradeCooldownMaxD    time.Duration `json:"-"`
	SessionTTL             time.Duration `json:"-"`
	SessionGCInterval      time.Duration `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"` // 0 = 不启动（enabled=false）
	ExpiringSoonDur        time.Duration `json:"-"`

	// KeyRegistry 解析后的多租户 key 注册表（含 default 合成条目），main 直接注入 livecfg。
	KeyRegistry *apikeys.Registry `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	// 60s：上游未给重试时间时的兜底；上游明说时以解析出的时间为准。
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.SoftRateMax = "2h"
	// 请求体上限缺省 0 = 不限（2026-10-18 改口径，见 Server.MaxBodyMB 注释：
	// 8MB 默认把多图/长上下文会话前置成 413，而"多大算过大"本该由上游判定）。
	c.Server.MaxBodyMB = 0
	c.Metrics.Enabled = true
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.BlackcatHours = []int{23}
	// 开关「缺省 true」靠这几行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Schedule.ModelSyncEnabled = true
	c.Schedule.ModelSyncTime = "03:30"
	c.Schedule.ProbeEnabled = true
	c.Schedule.ProbeHours = []int{9}
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：透传客户端原始 system（对齐上游；custom 由用户显式选择）
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403 修复
	// P1-1）；0/负数 normalize 回落默认（与 max_in_flight 的 0=不限语义不同，分档键
	// 的 0 没有合理语义，回退分档默认最稳）。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		// 目录检查：Docker bind mount 在宿主机文件缺失时会静默创建同名目录，
		// 直接 ReadFile 会报 "Incorrect function" 之类晦涩错误，这里给出可操作提示。
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件——"+
				"Docker 部署时若宿主机缺少 config.json，bind mount 会创建同名目录。"+
				"请先 `cp config.example.json config.json` 或删除该目录（程序会自动生成配置）", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if _, err := ParseConfigInto(raw, c); err != nil {
			return nil, err
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfigInto 把 JSON 覆盖到 c 上并 normalize（不做 env、不读文件）。
// 面板保存配置走这条路径：与 Load 完全同一套解析/校验逻辑，避免两处漂移。
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// stream_total_timeout_seconds 的「未设置 vs 显式 0」需要原始 JSON 判定
	// （2026-10-20）：普通 int 字段两者都是 0，会把「显式禁用硬顶」补成 1800。
	var rawUp struct {
		Upstream struct {
			StreamTotalTimeoutSeconds *int `json:"stream_total_timeout_seconds"`
		} `json:"upstream"`
	}
	if json.Unmarshal(raw, &rawUp) == nil {
		c.Upstream.streamTotalRaw = rawUp.Upstream.StreamTotalTimeoutSeconds
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfig 基于默认值解析一段配置 JSON（等价于 Load 的文件分支，但不读环境变量）。
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，双击即开免手工复制样例）。
// 值取自 Default()（含超时/熔断/签到排程等推荐值），api_key 用 crypto/rand 随机生成：
// 安全默认优于示例占位符（listen 绑定 0.0.0.0，空 key 会把网关裸暴露给局域网）。
// 返回生成的 key 供启动日志透出。已存在时经 O_EXCL 原子拒绝，绝不改写用户配置。
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	_ = c.normalize() // Default() 全合法，normalize 仅补齐 header/idle 超时的展示值
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL 原子拒绝覆盖：即使调用方漏判"不存在"，也绝不悄悄改写用户已有配置。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_MAX_BODY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.MaxBodyMB = n
		}
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_STREAM_TOTAL_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.StreamTotalTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
}

func (c *Config) normalize() error {
	var err error
	// max_body_mb：0（含缺省）= 不限；正值 = 上限（MB）；负值非法。
	// 旧口径 0/负数一律报错，导致"想不限"只能填一个拍脑袋的大数（2026-10-18 改）。
	if c.Server.MaxBodyMB < 0 {
		return fmt.Errorf("server.max_body_mb: %d 非法（0 或留空 = 不限；要限制请填正整数，单位 MB，上限 1024）", c.Server.MaxBodyMB)
	}
	// 上界：int64(mb)<<20 在极端值下溢出 int64；1GB 上限对代理场景已绰绰有余。
	if c.Server.MaxBodyMB > 1024 {
		return fmt.Errorf("server.max_body_mb: %d 超出上限 1024（单位 MB）", c.Server.MaxBodyMB)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	// 快过期窗口：空 = 禁用（ExpiringSoonDur 0）；非空必须可解析（拼写错误 fail fast）。
	if c.Pool.ExpiringSoon != "" {
		if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
			return fmt.Errorf("pool.expiring_soon: %w", err)
		}
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省独立回落 60s（2026-10-20 修正：原回落 timeout_seconds=120s，
	// 比 transport.go 精心选的 60s 慢一倍——MaxRotate=3 的最坏轮转从 3min
	// 变 6min，且 transport.go:36 注释明确反对放大首字节超时）。
	// idle/stream_total 缺省走内置大值。任务书约定：0 一律视为"未设置"走默认，
	// 真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = 60
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	// 流总时长硬顶（2026-10-20）：缺省 1800s（30min）。合法长回答远小于此
	// （keepalive 拖流才是本项要防的）。三分支口径见字段注释：未设置→1800、
	// 负数→0（禁用）、显式值（含 0=禁用）原样。
	switch {
	case c.Upstream.streamTotalRaw == nil:
		c.Upstream.StreamTotalTimeoutSeconds = 1800
	case *c.Upstream.streamTotalRaw < 0:
		c.Upstream.StreamTotalTimeoutSeconds = 0
	default:
		c.Upstream.StreamTotalTimeoutSeconds = *c.Upstream.streamTotalRaw
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 空数组与 null 反序列化后覆盖掉 Default() 的排程值（键缺席才保留），在此补齐。
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	if len(c.Schedule.BlackcatHours) == 0 {
		c.Schedule.BlackcatHours = []int{23}
	}
	if len(c.Schedule.ProbeHours) == 0 {
		c.Schedule.ProbeHours = []int{9}
	}
	// 余额后台刷新：启用时 minutes<=0 回落默认 5；关闭时 interval 保持 0（不启动）。
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	// 模型目录同步时点：空回落默认；非法格式报错（防静默不触发）。
	if c.Schedule.ModelSyncTime == "" {
		c.Schedule.ModelSyncTime = "03:30"
	} else if _, err := time.Parse("15:04", c.Schedule.ModelSyncTime); err != nil {
		return fmt.Errorf("schedule.model_sync_time %q 不是合法 HH:MM", c.Schedule.ModelSyncTime)
	}
	// 腾讯文档推送：启用时三要素必填（fail fast，避免运行期每轮推送失败刷告警）。
	if c.DocsSheet.Enabled {
		if c.DocsSheet.Token == "" || c.DocsSheet.FileID == "" || c.DocsSheet.SheetID == "" {
			return fmt.Errorf("docs_sheet.enabled=true 时 token/file_id/sheet_id 必填")
		}
	}
	if err := c.normalizeAPIKeys(); err != nil {
		return err
	}
	if err := notify.Validate(notify.Config{
		Enabled:           c.Notify.Enabled,
		Events:            c.Notify.Events,
		Channels:          c.Notify.Channels,
		Cooldown:          c.Notify.Cooldown,
		BalanceLowCredits: c.Notify.BalanceLowCredits,
	}); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// maxAPIKeys 单实例 key 条目上限：防御手滑粘贴超大数组把内存/比对开销撑爆。
const maxAPIKeys = 200

// normalizeAPIKeys 归一 api_keys 数组并构建注册表（config.KeyRegistry）。
//
// 规则：
//   - name 缺省 "key-N"（N 从 1 计）；name/key 首尾空白裁剪；
//   - key 为空的条目直接报错（fail fast——静默丢弃会让人以为配上了）；
//   - RPM < 0 报错；models 逐项裁剪、空项剔除；
//   - key/name 重复报错（重复 key 会让鉴权结果取决于条目顺序）；
//   - legacy api_key 非空且未在 api_keys 中出现 → 合成 name="default" 条目并入
//     （老客户端零回归；api_keys 已包含同 key 时以 api_keys 声明为准，不重复）；
//   - api_key 与 api_keys 全空 → 鉴权关闭（沿用"空 = 不鉴权"语义），注册表置 nil。
func (c *Config) normalizeAPIKeys() error {
	if len(c.APIKeys) > maxAPIKeys {
		return fmt.Errorf("api_keys: %d 条超出上限 %d", len(c.APIKeys), maxAPIKeys)
	}
	entries := make([]apikeys.Entry, 0, len(c.APIKeys)+1)
	seenKey, seenName := map[string]bool{}, map[string]bool{}
	for i := range c.APIKeys {
		e := c.APIKeys[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Key = strings.TrimSpace(e.Key)
		if e.Name == "" {
			e.Name = fmt.Sprintf("key-%d", i+1)
		}
		if e.Key == "" {
			return fmt.Errorf("api_keys[%d] (%s): key 不能为空", i, e.Name)
		}
		if e.RPM < 0 {
			return fmt.Errorf("api_keys[%s]: rpm 不能为负数", e.Name)
		}
		models := make([]string, 0, len(e.Models))
		for _, m := range e.Models {
			if m = strings.TrimSpace(m); m != "" {
				models = append(models, m)
			}
		}
		e.Models = models
		if seenKey[e.Key] {
			return fmt.Errorf("api_keys[%s]: key 与其他条目重复", e.Name)
		}
		if seenName[e.Name] {
			return fmt.Errorf("api_keys[%s]: name 与其他条目重复", e.Name)
		}
		seenKey[e.Key], seenName[e.Name] = true, true
		entries = append(entries, e)
	}
	// legacy 单 key 合成 default 条目（api_keys 已含同 key 时不重复注入）。
	if c.APIKey != "" && !seenKey[c.APIKey] {
		entries = append(entries, apikeys.Entry{Name: "default", Key: c.APIKey})
	}
	if len(entries) == 0 {
		// 全空 = 不鉴权（legacy 语义）。注册表 nil，handler 回退"空 key 放行"。
		c.KeyRegistry = nil
		return nil
	}
	c.KeyRegistry = apikeys.NewRegistry(entries)
	return nil
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom 模式）。
//
// mode 非法（非 custom/passthrough）启动报错，避免静默回落到某一分支；
// custom 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（passthrough / custom）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// validateScheduleHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", c.Schedule.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", c.Schedule.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.blackcat_hours", "blackcat_enabled", c.Schedule.BlackcatHours); err != nil {
		return err
	}
	return checkHourRange("schedule.probe_hours", "probe_enabled", c.Schedule.ProbeHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}

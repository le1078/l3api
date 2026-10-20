// modelsync.go 品牌静态模型目录的定时自动同步（v1.12 model-sync）。
//
// 背景：qoder/trae 模型目录历史上是静态硬编码（qoder_models.go/trae_models.go
// 注释的「季度级变化、静态表+人工校正」），人工校正依赖记得去探——本文件把
// 这条链路自动化：官方端点探测 → 与生效目录 diff →（trae 新增实测门）→
// 落盘覆盖文件 → QoderChatModels/TraeChatModels 改为「种子 + 覆盖」读取。
//
// 生效路径设计（为什么不直接改注册表/provider.RegisterChatModels）：
// 目录的两个消费方里，面板「模型与档位」页直读 upstream.QoderChatModels()/
// TraeChatModels()（panel.go），不经 provider 注册表；改这两个函数的读取源
// 才能一处改动同时覆盖 /v1/models 与面板。覆盖语义是**整体替换**（非逐条
// 合并）：官方下架的模型也随之消失，目录永远等于最近一次成功探测。
//
// codebuddy 不在本文件范围：CN/global 目录已是动态探测（FetchModels/
// fetchGlobalModelsOnce，1h 缓存），静态表概念不存在。
//
// 落盘文件 model_sync.json 与 state.json 同目录（stateSibling 接线），
// 仿 model.json 的「种子 embed + 运行目录覆盖 + 损坏降级 WARN 不崩溃」模式
// （model_catalog.go）。写盘用 tmp+rename 原子写。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

// ModelSyncFile model_sync.json 的落盘结构。品牌段为 nil = 该品牌从未同步成功
// （读取侧回落种子）。同步单品牌时读出旧文件、只替换对应段、原子写回。
type ModelSyncFile struct {
	Qoder    []QoderModel `json:"qoder,omitempty"`
	Trae     []TraeModel  `json:"trae,omitempty"`
	SyncedAt string       `json:"synced_at"` // RFC3339 UTC，最近一次任意品牌同步成功
	// TraeChangedKnown 已告警过的 Trae 字段变更 ID（Changed 是「仅告警」口径，
	// 不入目录；不记住已告警集合的话，人工校准字段与官方口径的固定偏差会
	// 让每轮同步都重复告警同一批模型）。
	TraeChangedKnown []string `json:"trae_changed_known,omitempty"`
}

// modelSyncState 覆盖文件的进程内缓存：path 由 SetModelSyncPath 接线；
// loaded=已尝试加载（无论成败，失败回落种子且每 5min 允许重试——文件可能
// 由定时任务在进程运行期首次写出）。
var modelSyncState = struct {
	sync.Mutex
	path     string
	file     ModelSyncFile
	loaded   bool
	lastFail time.Time
}{}

// SetModelSyncPath 接线 model_sync.json 路径（cmd/server 启动时调用）。
// 仅设置路径；加载在首次读目录时懒做（首次调用前已缓存的种子不受影响——
// 本函数若在使用后调用，清空缓存强制下次读取重载）。
func SetModelSyncPath(path string) {
	modelSyncState.Lock()
	modelSyncState.path = path
	modelSyncState.loaded = false // 路径变更 → 下次读取重新加载
	modelSyncState.Unlock()
	// init 期注册进 provider 注册表的是种子（path 未接线）；接线后立刻按
	// 磁盘覆盖重注册，否则重启后 /v1/models 永远停在种子目录。
	RegisterBrandCatalogs()
}

// modelSyncFailCooldown 覆盖文件加载失败的重试间隔（防每次读目录都重打磁盘）。
const modelSyncFailCooldown = 5 * time.Minute

// loadModelSyncLocked 懒加载覆盖文件（持锁调用）。文件不存在 = 未同步过，
// 正常回落种子（不算失败、不进负缓存）；存在但解析失败 → WARN + 负缓存。
func loadModelSyncLocked() {
	if modelSyncState.loaded {
		return
	}
	if !modelSyncState.lastFail.IsZero() && time.Since(modelSyncState.lastFail) < modelSyncFailCooldown {
		return
	}
	modelSyncState.loaded = true
	if modelSyncState.path == "" {
		return
	}
	raw, err := os.ReadFile(modelSyncState.path)
	if err != nil {
		if !os.IsNotExist(err) {
			// 失败路径复位 loaded：lastFail 冷却期过后允许重试（文件可能随后
			// 被修好/首次写出）。不复位则一次损坏 = 本进程永久回落种子。
			modelSyncState.loaded = false
			modelSyncState.lastFail = time.Now()
			log.Printf("WARN: [modelsync] 读取覆盖文件失败（回落种子）: path=%s err=%v", modelSyncState.path, err)
		}
		return
	}
	var f ModelSyncFile
	if err := json.Unmarshal(raw, &f); err != nil {
		modelSyncState.loaded = false
		modelSyncState.lastFail = time.Now()
		log.Printf("WARN: [modelsync] 覆盖文件损坏（回落种子）: path=%s err=%v", modelSyncState.path, err)
		return
	}
	modelSyncState.file = f
}

// saveModelSync 读出旧文件、替换指定品牌段、原子写回并刷新进程内缓存。
// 目录不可写/序列化失败 → 静默 WARN（内存缓存仍已刷新，本次进程生效）。
func saveModelSync(brand string, qoder []QoderModel, trae []TraeModel) {
	modelSyncState.Lock()
	defer modelSyncState.Unlock()
	f := modelSyncState.file // 以已加载的为基底；未加载过尝试读一次
	if !modelSyncState.loaded && modelSyncState.path != "" {
		if raw, err := os.ReadFile(modelSyncState.path); err == nil {
			_ = json.Unmarshal(raw, &f)
		}
	}
	switch brand {
	case "qoder":
		f.Qoder = qoder
	case "trae":
		f.Trae = trae
	}
	f.SyncedAt = time.Now().UTC().Format(time.RFC3339)
	modelSyncState.file = f
	modelSyncState.loaded = true
	modelSyncState.lastFail = time.Time{}
	if modelSyncState.path == "" {
		return
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(modelSyncState.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := modelSyncState.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: [modelsync] 落盘失败（内存缓存仍生效）: path=%s err=%v", modelSyncState.path, err)
		return
	}
	if err := os.Rename(tmp, modelSyncState.path); err != nil {
		log.Printf("WARN: [modelsync] 落盘改名失败: path=%s err=%v", modelSyncState.path, err)
	}
}

// modelSyncTraeChangedKnown 已告警过的 Trae 字段变更集合（持锁读，触发懒加载）。
func modelSyncTraeChangedKnown() map[string]bool {
	modelSyncState.Lock()
	defer modelSyncState.Unlock()
	loadModelSyncLocked()
	out := make(map[string]bool, len(modelSyncState.file.TraeChangedKnown))
	for _, id := range modelSyncState.file.TraeChangedKnown {
		out[id] = true
	}
	return out
}

// modelSyncMarkTraeChanged 把本轮观测到的字段变更并入已告警集合（内存态；
// 随后 saveModelSync 落盘时一并持久化）。
func modelSyncMarkTraeChanged(ids []string) {
	modelSyncState.Lock()
	defer modelSyncState.Unlock()
	seen := make(map[string]bool, len(modelSyncState.file.TraeChangedKnown)+len(ids))
	for _, id := range modelSyncState.file.TraeChangedKnown {
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			modelSyncState.file.TraeChangedKnown = append(modelSyncState.file.TraeChangedKnown, id)
		}
	}
}

// resetModelSyncForTest 测试钩子：清空覆盖缓存与路径。
func resetModelSyncForTest() {
	modelSyncState.Lock()
	defer modelSyncState.Unlock()
	modelSyncState.path = ""
	modelSyncState.file = ModelSyncFile{}
	modelSyncState.loaded = false
	modelSyncState.lastFail = time.Time{}
}

// ────────────────────────────────────────────────────────────────────────────
// 探测
// ────────────────────────────────────────────────────────────────────────────

// qoderModelListURL Qoder 官方模型目录端点（与 chat 同源 Bearer COSY 签名）。
// 包级变量：测试可指向 httptest mock。
var qoderModelListURL = "https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1"

// ProbeQoderModels 探测 Qoder 官方 chat 组模型目录（GET model/list）。
// jobToken 临期先滚动刷新（与 QoderChatStream 同链路，共用同一单飞组）。
// 返回上游原始顺序。
func (c *Client) ProbeQoderModels(ctx context.Context, a *auth.Auth) ([]QoderModel, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 单飞刷新（2026-10-20）：与 chat/checkin/RefreshIfNeeded 共用，防同账号
	// 并发路径重复打 refresh 端点互相覆盖。
	if a.NeedsRefresh(qoderRefreshSkew) {
		if a.RefreshToken == "" {
			return nil, &Error{Kind: ErrSessionDead, Status: 401, Msg: "qoder refresh_token missing"}
		}
		_ = c.qoderRefreshSingleflight(a)
		if a.NeedsRefresh(qoderRefreshSkew) {
			return nil, &Error{Kind: ErrSessionDead, Status: 401, Msg: "qoder token refresh failed"}
		}
	}
	if a.UID == "" || a.AccessToken == "" {
		return nil, &Error{Kind: ErrSessionDead, Status: 401, Msg: "qoder auth incomplete (uid/jobToken missing)"}
	}

	identity := qoderCosyIdentity{UID: a.UID, SecurityOauthToken: a.AccessToken}
	bearer, cosyKey, cosyDate, err := QoderBuildBearer(identity, qoderModelListURL, "", qoderUUID())
	if err != nil {
		return nil, fmt.Errorf("qoder modelsync: build bearer: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qoderModelListURL, nil)
	if err != nil {
		return nil, err
	}
	qoderSetChatHeaders(req, bearer, cosyKey, cosyDate, a.UID, "", a.UID)
	req.Header.Del("Accept") // 非 SSE 端点
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("qoder modelsync: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Kind: qoderClassifyHTTP(resp.StatusCode, string(raw)),
			Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return parseQoderModelList(raw)
}

// parseQoderModelList 解析 model/list 响应的 chat 组（明文 JSON；
// 响应形态见 qoder_models.go 文件头注释）。空 chat 组视为解析失败
// （宁可本轮不同步，也不把空目录落盘清掉现网目录）。
func parseQoderModelList(raw []byte) ([]QoderModel, error) {
	var env struct {
		Chat []struct {
			Key            string   `json:"key"`
			DisplayName    string   `json:"display_name"`
			IsVL           bool     `json:"is_vl"`
			IsReasoning    bool     `json:"is_reasoning"`
			MaxInputTokens int64    `json:"max_input_tokens"`
			Enable         bool     `json:"enable"`
			PriceFactor    *float64 `json:"price_factor"`
			ThinkingConfig *struct {
				Disabled *json.RawMessage `json:"disabled"`
				Enabled  *struct {
					IsDefault bool `json:"is_default"`
					Efforts   map[string]struct {
						IsDefault bool `json:"is_default"`
					} `json:"efforts"`
				} `json:"enabled"`
			} `json:"thinking_config"`
		} `json:"chat"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("qoder modelsync: parse model/list: %w", err)
	}
	out := make([]QoderModel, 0, len(env.Chat))
	for _, e := range env.Chat {
		if e.Key == "" || !e.Enable {
			continue
		}
		m := QoderModel{
			ID: e.Key, DisplayName: e.DisplayName,
			IsVL: e.IsVL, IsReasoning: e.IsReasoning, MaxInputTokens: e.MaxInputTokens,
			Credits:         qoderCreditsText(e.PriceFactor),
			MaxOutputTokens: brandMaxOutputGuess(e.MaxInputTokens),
		}
		// 固定档默认值：is_reasoning 而官方未下发档位/默认档时记 "on"
		// （面板显示「固定档 · 默认 on」而不是「默认 ?」）。thinking_config
		// 有值时仍以其为权威（enabled.is_default 或 efforts 的 is_default）。
		if m.IsReasoning {
			m.DefaultEffort = "on"
		}
		if tc := e.ThinkingConfig; tc != nil {
			m.CanDisableThinking = tc.Disabled != nil
			if en := tc.Enabled; en != nil && en.IsDefault && m.DefaultEffort == "" {
				m.DefaultEffort = "on"
			}
			if en := tc.Enabled; en != nil && len(en.Efforts) == 0 && en.IsDefault {
				// 固定档（enabled.is_default 且无 efforts 列表）：默认档记 "on"。
				m.DefaultEffort = "on"
			}
			if en := tc.Enabled; en != nil && len(en.Efforts) > 0 {
				// 稳定输出：按固定顺序（low/medium/high/xhigh/max）排列，
				// 避免 map 迭代顺序导致 diff 每轮误报 Changed。
				for _, name := range []string{"low", "medium", "high", "xhigh", "max"} {
					if ef, ok := en.Efforts[name]; ok {
						m.Efforts = append(m.Efforts, name)
						if ef.IsDefault {
							m.DefaultEffort = name
						}
					}
				}
				// 兜底：上游出现枚举外档位时按字典序补上，不丢信息。
				for _, name := range sortedEffortKeys(en.Efforts) {
					if !containsEffort(m.Efforts, name) {
						m.Efforts = append(m.Efforts, name)
					}
				}
			}
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("qoder modelsync: model/list chat 组为空")
	}
	return out, nil
}

// qoderCreditsText price_factor → 面板积分倍率原文（与 WorkBuddy 的 "x0.05" 同形）。
// is_free 的模型 price_factor=0 → "x0.00"（面板按免费渲染）。nil → 空（不编造）。
func qoderCreditsText(pf *float64) string {
	if pf == nil {
		return ""
	}
	return fmt.Sprintf("x%.2f", *pf)
}

// brandMaxOutputGuess 品牌目录无官方 max_output 时的输出上限估算。
// 口径：WorkBuddy 动态目录实测「上下文:输出 ≈ 4:1」（1M→256K、200K→48K），
// 与面板提示「多数模型实际被限制在 40K 左右」一致。四舍五入到 1K 粒度。
// 估算值在面板上以「≈」标注（max_output_estimated=true），不与官方值混淆。
func brandMaxOutputGuess(context int64) int64 {
	if context <= 0 {
		return 0
	}
	return ((context / 4) + 500) / 1000 * 1000
}

// qoderModelEqual 条目相等（Efforts 是 slice，不能直接 !=）。
func qoderModelEqual(a, b QoderModel) bool {
	if a.ID != b.ID || a.DisplayName != b.DisplayName || a.IsVL != b.IsVL ||
		a.IsReasoning != b.IsReasoning || a.MaxInputTokens != b.MaxInputTokens ||
		a.Credits != b.Credits || a.MaxOutputTokens != b.MaxOutputTokens ||
		a.DefaultEffort != b.DefaultEffort || a.CanDisableThinking != b.CanDisableThinking {
		return false
	}
	if len(a.Efforts) != len(b.Efforts) {
		return false
	}
	for i := range a.Efforts {
		if a.Efforts[i] != b.Efforts[i] {
			return false
		}
	}
	return true
}

// traeModelEqual 条目相等（Efforts 是 slice，不能直接 !=）。
func traeModelEqual(a, b TraeModel) bool {
	if a.ID != b.ID || a.Name != b.Name || a.IsVL != b.IsVL ||
		a.IsReasoning != b.IsReasoning || a.MaxTokens != b.MaxTokens ||
		a.Credits != b.Credits || a.MaxOutputTokens != b.MaxOutputTokens ||
		a.DefaultEffort != b.DefaultEffort || a.CanDisableThinking != b.CanDisableThinking {
		return false
	}
	if len(a.Efforts) != len(b.Efforts) {
		return false
	}
	for i := range a.Efforts {
		if a.Efforts[i] != b.Efforts[i] {
			return false
		}
	}
	return true
}

// sortedEffortKeys 兜底排序（仅用于枚举外档位补全；稳定序防 diff 抖动）。
func sortedEffortKeys(m map[string]struct {
	IsDefault bool `json:"is_default"`
}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ProbeTraeModels 探测 Trae 用户可见预设模型目录（parseTraeDetailParam 的
// Visible 集；面板/手动探测用）。同步主链路走 probeTraeDetailParamRaw +
// parseTraeDetailParam（需要 AllIDs 做下架判定）。
func (c *Client) ProbeTraeModels(ctx context.Context, a *auth.Auth) ([]TraeModel, error) {
	raw, err := c.probeTraeDetailParamRaw(ctx, a)
	if err != nil {
		return nil, err
	}
	res, err := parseTraeDetailParam(raw)
	if err != nil {
		return nil, err
	}
	return res.Visible, nil
}

// probeTraeDetailParamRaw 拉取 get_detail_param 原始响应（POST {"function":
// "chat_v3"}，Cloud-IDE-JWT + 桌面端指纹头，与 trae_chat.go 同口径）。
func (c *Client) probeTraeDetailParamRaw(ctx context.Context, a *auth.Auth) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	jwt, err := c.traeJWTFor(ctx, a)
	if err != nil {
		return nil, err
	}
	a.Lock()
	uid := a.UID
	deviceID := a.DeviceToken
	a.Unlock()
	if deviceID == "" {
		deviceID = TraeDeviceIDFor(uid)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		TraeChatHost+"/api/ide/v1/get_detail_param",
		strings.NewReader(`{"function":"chat_v3"}`))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Cloud-IDE-JWT "+jwt)
	h.Set("X-Cloudide-Token", jwt)
	h.Set("x-app-id", traeAppID)
	h.Set("x-app-version", "default")
	h.Set("x-ide-version", traeIdeVersion)
	h.Set("x-ide-version-code", traeIdeVerCode)
	h.Set("x-ide-version-type", "stable")
	h.Set("x-device-id", deviceID)
	h.Set("x-machine-id", deviceID)
	h.Set("x-device-brand", "Windows")
	h.Set("x-device-cpu", "x64")
	h.Set("x-device-type", "desktop")
	h.Set("x-os-version", "10.0.26200")
	h.Set("x-uid", uid)
	h.Set("request-traffic-type", "prod")
	h.Set("User-Agent", traeUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("trae modelsync: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Kind: traeClassifyHTTP(resp.StatusCode, string(raw)),
			Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// traeProbeResult get_detail_param 的解析结果。两个口径分离的原因（2026-10-09
// 与种子逐条校准发现）：官方响应里 is_invisible_to_user=true 的条目（glm-5.1/
// kimi-k2/Doubao_1_6 等隐藏或内部路由模型）——对用户不可选、不该自动**新增**；
// 但种子里人工收录过其中仍可用的（DeepSeek-V4-Pro 等实测 200），仅因「变隐藏」
// 就自动下架会误杀可用模型。因此：
//   - 新增：只看 Visible（inv != true）；
//   - 下架：只看 AllIDs（彻底从官方响应消失才下架）。
type traeProbeResult struct {
	Visible []TraeModel     // 用户可见预设（自动新增候选）
	AllIDs  map[string]bool // 全部官方预设 ID（含隐藏；下架判定基准）
}

// parseTraeDetailParam 解析 get_detail_param 响应（可见性口径见
// traeProbeResult）。零结果视为解析失败（不落盘空目录）。
//
// 过滤口径：config_source==1（官方预设，排除 custom_model_* 用户自备槽位）
// && config_switch=true && 有展示名（内部槽位 summary/fast_apply/
// title_generation 等无 display_name，天然被剔除）&& 非 *_advisor/*-for-auto
// 内部路由槽位。
//
// 字段映射（2026-10-09 与静态种子逐条校准）：
//   - IsReasoning = display_config.model_capability=="reasoning_model" 或
//     带 reasoning_effort_config（chat_model 但可调推理档的混合模型，如
//     kimi-k3/Seed-Evolving 实测流内带 reasoning_content）；
//   - IsVL = display_config.multimodal；
//   - MaxTokens = context_window_tokens.dev（客户端实际生效窗）。
func parseTraeDetailParam(raw []byte) (*traeProbeResult, error) {
	var env struct {
		List []struct {
			ConfigName   string `json:"config_name"`
			ConfigSource int    `json:"config_source"`
			ConfigSwitch bool   `json:"config_switch"`
			Invisible    *bool  `json:"is_invisible_to_user"` // 缺省/null = 可见
			ContextWin   struct {
				Dev int `json:"dev"`
			} `json:"context_window_tokens"`
			Display struct {
				Name       string `json:"display_name"`
				Capability string `json:"model_capability"`
				Multimodal bool   `json:"multimodal"`
			} `json:"display_config"`
			ReasoningEffort json.RawMessage `json:"reasoning_effort_config"`
			Contact         json.RawMessage `json:"display_contact_config"`
			Details         []struct {
				MaxTokens int64 `json:"max_tokens"`
			} `json:"model_detail_list"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("trae modelsync: parse get_detail_param: %w", err)
	}
	res := &traeProbeResult{AllIDs: map[string]bool{}}
	seen := map[string]bool{}
	for _, e := range env.List {
		if e.ConfigSource != 1 || !e.ConfigSwitch || e.ConfigName == "" {
			continue
		}
		if strings.HasPrefix(e.ConfigName, "custom_model") {
			continue
		}
		// 内部路由槽位剔除：*_advisor（双模型顾问链路）、*-for-auto（Auto 后端）
		// ——它们带 display_name 且 ping 可通，但不是用户可选模型。
		if strings.Contains(e.ConfigName, "_advisor") || strings.Contains(e.ConfigName, "-for-auto") {
			continue
		}
		if e.Display.Name == "" {
			continue // 内部槽位（summary/fast_apply/title_generation/...）
		}
		if seen[e.ConfigName] {
			continue
		}
		seen[e.ConfigName] = true
		res.AllIDs[e.ConfigName] = true
		if e.Invisible != nil && *e.Invisible {
			continue // 隐藏模型：计入 AllIDs（不下架），不进 Visible（不新增）
		}
		reasoning := e.Display.Capability == "reasoning_model" ||
			(len(e.ReasoningEffort) > 0 && string(e.ReasoningEffort) != "null")
		tm := TraeModel{
			ID: e.ConfigName, Name: e.Display.Name,
			IsReasoning: reasoning, IsVL: e.Display.Multimodal, MaxTokens: e.ContextWin.Dev,
			Credits:         traeCreditsText(e.Contact),
			MaxOutputTokens: traeMaxOutput(e.Details, int64(e.ContextWin.Dev)),
		}
		if ec := parseTraeEffort(e.ReasoningEffort); ec != nil {
			tm.Efforts = ec.efforts
			tm.DefaultEffort = ec.defaultEffort
			tm.CanDisableThinking = ec.canDisable
		} else if reasoning {
			// 推理模型但无可枚举档位 = 固定档：默认档记 "on"（支持思考、档位不可调）。
			tm.DefaultEffort = "on"
		}
		res.Visible = append(res.Visible, tm)
	}
	if len(res.AllIDs) == 0 {
		return nil, fmt.Errorf("trae modelsync: get_detail_param 无预设模型")
	}
	return res, nil
}

// traeEffortCap reasoning_effort_config 的归一结果。
type traeEffortCap struct {
	efforts       []string
	defaultEffort string
	canDisable    bool
}

// parseTraeEffort 解析 reasoning_effort_config（JSON 形态：
// {"support_thinking":true,"default_level":"high","options":["light","high"]}）。
// 非对象/无 options → nil（固定档或不可调，面板按 supports_reasoning 显示「固定档」）。
func parseTraeEffort(raw json.RawMessage) *traeEffortCap {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var ec struct {
		SupportThinking bool     `json:"support_thinking"`
		DefaultLevel    string   `json:"default_level"`
		Options         []string `json:"options"`
		CanDisable      bool     `json:"can_disable"`
	}
	if err := json.Unmarshal(raw, &ec); err != nil {
		return nil
	}
	if len(ec.Options) == 0 {
		return nil
	}
	// options 保持上游顺序（Trae 口径 light→high→extra_high 本身即稳定序）。
	return &traeEffortCap{
		efforts:       append([]string(nil), ec.Options...),
		defaultEffort: ec.DefaultLevel,
		canDisable:    ec.CanDisable,
	}
}

// traeCreditsText display_contact_config（对象或字符串化 JSON）→ 积分倍率原文。
// 取 consumption_rate.data.rate（实测 0.06~1.83）；缺失回退活动折后
// consumption_rate；仍无 → 空串（不编造）。
func traeCreditsText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		var s string
		if err2 := json.Unmarshal(raw, &s); err2 != nil {
			return ""
		}
		if err3 := json.Unmarshal([]byte(s), &obj); err3 != nil {
			return ""
		}
	}
	rate, _ := nestedFloat(obj, "consumption_rate", "data", "rate")
	if rate <= 0 {
		rate, _ = nestedFloat(obj, "activity_discount", "data", "current", "consumption_rate")
	}
	if rate <= 0 {
		return ""
	}
	return fmt.Sprintf("x%.2f", rate)
}

// traeMaxOutput model_detail_list[0].max_tokens；缺失时回落窗口估算（≈context/4）。
func traeMaxOutput(details []struct {
	MaxTokens int64 `json:"max_tokens"`
}, context int64) int64 {
	if len(details) > 0 && details[0].MaxTokens > 0 {
		return details[0].MaxTokens
	}
	return brandMaxOutputGuess(context)
}

// nestedFloat 按路径取嵌套 float64（JSON 数字解成 float64）。
func nestedFloat(obj map[string]any, path ...string) (float64, bool) {
	var cur any = obj
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		cur, ok = m[k]
		if !ok {
			return 0, false
		}
	}
	f, ok := cur.(float64)
	return f, ok
}

// VerifyTraeModel Trae 新模型实测门：发一条极短 chat，首个实质事件为 output
// 即通过（TraeChatStream 预读语义：流内 error 帧在预读窗口到达 → 返回 *Error）。
// 上游下架/不可用（4023/4001）→ 返回 error，调用方剔除并告警。
func (c *Client) VerifyTraeModel(ctx context.Context, a *auth.Auth, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"ping, reply with one word"}],"stream":true}`)
	rc, _, raw, err := c.TraeChatStream(ctx, a, body, "", model)
	if err != nil {
		if raw != nil {
			return fmt.Errorf("%w (body=%.150s)", err, raw)
		}
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 8192))
	rc.Close()
	return nil
}

// ────────────────────────────────────────────────────────────────────────────
// diff + 同步编排
// ────────────────────────────────────────────────────────────────────────────

// ModelSyncDiff 单品牌目录差异（ID 列表，上游原序）。
type ModelSyncDiff struct {
	Added   []string `json:"added,omitempty"`   // 官方新增（trae 侧 = 实测门通过的新模型）
	Removed []string `json:"removed,omitempty"` // 官方下架
	Changed []string `json:"changed,omitempty"` // ID 相同但能力/窗口字段变化
	// Rejected 探测到但实测门未通过的新模型（仅 trae；不进目录、进告警）。
	Rejected []string `json:"rejected,omitempty"`
}

func (d ModelSyncDiff) empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// diffQoderModels / diffTraeModels 按 ID diff（probed 为官方最新，current 为
// 当前生效目录）。Changed 判字段级相等。
func diffQoderModels(probed, current []QoderModel) ModelSyncDiff {
	var d ModelSyncDiff
	cur := map[string]QoderModel{}
	for _, m := range current {
		cur[m.ID] = m
	}
	probe := map[string]QoderModel{}
	for _, m := range probed {
		probe[m.ID] = m
		if old, ok := cur[m.ID]; !ok {
			d.Added = append(d.Added, m.ID)
		} else if !qoderModelEqual(old, m) {
			d.Changed = append(d.Changed, m.ID)
		}
	}
	for _, m := range current {
		if _, ok := probe[m.ID]; !ok {
			d.Removed = append(d.Removed, m.ID)
		}
	}
	return d
}

func diffTraeModels(probed, current []TraeModel) ModelSyncDiff {
	var d ModelSyncDiff
	cur := map[string]TraeModel{}
	for _, m := range current {
		cur[m.ID] = m
	}
	probe := map[string]TraeModel{}
	for _, m := range probed {
		probe[m.ID] = m
		if old, ok := cur[m.ID]; !ok {
			d.Added = append(d.Added, m.ID)
		} else if !traeModelEqual(old, m) {
			d.Changed = append(d.Changed, m.ID)
		}
	}
	for _, m := range current {
		if _, ok := probe[m.ID]; !ok {
			d.Removed = append(d.Removed, m.ID)
		}
	}
	return d
}

// SyncQoderModels 同步 Qoder 目录：探测 → diff（对当前生效目录）→ 有变化则
// 落盘覆盖。返回 diff（无变化时 empty）。Qoder 目录条目即官方目录原样，
// 无实测门（model/list 的 enable 标记已是官方口径；错误模型由 chat 链路的
// 换号/换模型容错兜底）。
func (c *Client) SyncQoderModels(ctx context.Context, a *auth.Auth) (ModelSyncDiff, error) {
	probed, err := c.ProbeQoderModels(ctx, a)
	if err != nil {
		return ModelSyncDiff{}, err
	}
	d := diffQoderModels(probed, QoderChatModels())
	if d.empty() {
		return d, nil
	}
	saveModelSync("qoder", probed, nil)
	RegisterBrandCatalogs() // /v1/models 走注册表，同步生效后必须重注册
	return d, nil
}

// SyncTraeModels 同步 Trae 目录。与 Qoder 的「整体替换」不同，Trae 是
// **增删口径 + 存量字段不动**（原因见 traeProbeResult：种子里的 VL/推理标记
// 是人工实测口径，display_config 的 multimodal/model_capability 与实测有出入
// ——如 glm-5.3 官方 mm=false 但实测支持图片；自动覆盖会回归人工校准）：
//   - 新增：Visible（用户可见）且不在当前目录 → 逐一实测门（VerifyTraeModel，
//     200 才收录，失败进 Rejected 告警）；
//   - 下架：当前目录条目彻底从官方响应消失（不在 AllIDs）→ 剔除；
//   - 字段变更：只进告警（Changed），不自动覆盖人工校准字段。
func (c *Client) SyncTraeModels(ctx context.Context, a *auth.Auth) (ModelSyncDiff, error) {
	raw, err := c.probeTraeDetailParamRaw(ctx, a)
	if err != nil {
		return ModelSyncDiff{}, err
	}
	res, err := parseTraeDetailParam(raw)
	if err != nil {
		return ModelSyncDiff{}, err
	}
	current := TraeChatModels()
	curByID := map[string]TraeModel{}
	for _, m := range current {
		curByID[m.ID] = m
	}

	var d ModelSyncDiff
	// 下架：当前条目不在官方预设 ID 集（彻底消失）。
	kept := make([]TraeModel, 0, len(current))
	for _, m := range current {
		if !res.AllIDs[m.ID] {
			d.Removed = append(d.Removed, m.ID)
			continue
		}
		kept = append(kept, m)
	}
	// 字段变更（仅告警）：可见条目与当前同 ID 但字段不同。已告警过的不再
	// 重复上报（人工校准字段与官方口径的固定偏差会形成稳定的 Changed 集，
	// 不去重每轮都刷屏）。
	var allChanged []string
	for _, m := range res.Visible {
		if old, ok := curByID[m.ID]; ok && !traeModelEqual(old, m) {
			allChanged = append(allChanged, m.ID)
		}
	}
	known := modelSyncTraeChangedKnown()
	for _, id := range allChanged {
		if !known[id] {
			d.Changed = append(d.Changed, id)
		}
	}
	// 新增：可见且不在当前目录 → 实测门。
	for _, m := range res.Visible {
		if _, ok := curByID[m.ID]; ok {
			continue
		}
		if verr := c.VerifyTraeModel(ctx, a, m.ID); verr != nil {
			log.Printf("WARN: [modelsync] trae 新模型 %s 实测未过（不收录）: %v", m.ID, verr)
			d.Rejected = append(d.Rejected, m.ID)
			continue
		}
		d.Added = append(d.Added, m.ID)
		kept = append(kept, m)
	}
	if len(kept) == 0 {
		// 目录全空只可能是探测/过滤异常（官方不可能下架所有预设模型），
		// 拒绝落盘——空切片经 omitempty 序列化后读取侧会回落种子，
		// 「整体替换」语义在空目录边界破缺，宁可本轮不同步。
		return d, fmt.Errorf("trae modelsync: 同步后目录为空，拒绝落盘（疑似探测异常）")
	}
	if d.empty() {
		return d, nil
	}
	modelSyncMarkTraeChanged(allChanged) // 并入已告警集合，随落盘持久化
	saveModelSync("trae", nil, kept)
	RegisterBrandCatalogs() // /v1/models 走注册表，同步生效后必须重注册
	return d, nil
}

// qoder_checkin.go Qoder 每日权益领取（每日 100 积分）。
//
// 2026-09-28 修正：本文件早前断言「领取是桌面端 webview 专属、服务端无代领接口」，
// 因而只做监测 + 返回深链。该结论是**观测偏差**——请求没带 Cosy-Machine* 设备指纹头，
// campaigns 接口按「无指纹」返回空数组，于是被误读成「今天没活动 / 服务端不开放」。
//
// 真实路径（Campaign 体系，协议细节见 qoder_fingerprint.go 顶部说明）：
//  1. GET  /sash/api/v1/me/campaigns
//     → 筛 actionType==CLAIM_BENEFIT 且 claimStatus==CLAIMABLE
//  2. POST /sash/api/v1/me/campaigns/{campaignId}/claim  （幂等，响应带 replayed）
//
// 指纹由本机 Qoder 客户端的 runtime-info.exe 采集、落盘到 data/qoder_fingerprints.json
// （按 uid 索引），网关原样重放即可代领——多账号即多份指纹，pool 遍历时各取各的。
//
// 降级契约（重要）：**取不到指纹时不报错、也不误报「今日无活动」**，而是退化为
// 早前的「仅监测」模式（给深链 / 静默），并打一行 warn 说明缺指纹。两条路径的差别
// 只在能否 POST claim，其余（jobToken 刷新、限量号、错误分类）完全一致。
//
// 协议依据：逆向 Qoder CN 客户端 app.asar
//   - /sash/api/v1/me/campaigns  → {showCampaign, claimable, campaignUrl, campaigns[]}
//   - client_launch_26           → GET .../client_launch_26/limited-number 取限量号状态
// 国际版（openapi.qoder.sh）同一路径，活动项改用 campaignId/claimStatus 字段，
// 故 qoderCampaignItem 按两套形态并集解析（见其注释）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/linguo2625469/l3api/internal/auth"
)

// qoderCampaignsPath Qoder 活动状态端点（openapi 域，/sash 前缀）。
const qoderCampaignsPath = "/sash/api/v1/me/campaigns"

// qoderLimitedNumberCampaign 硬编码的活动 ID（与桌面端一致：client_launch_26）。
// 这是当前桌面端唯一内置检查的活动；其余活动由服务端在 campaigns 数组里动态下发。
const qoderLimitedNumberCampaign = "client_launch_26"

// qoderCampaignsResp GET /sash/api/v1/me/campaigns 响应。
type qoderCampaignsResp struct {
	UID          string              `json:"uid"`
	ShowCampaign bool                `json:"showCampaign"`
	Claimable    bool                `json:"claimable"`
	CampaignURL  string              `json:"campaignUrl"`
	Campaigns    []qoderCampaignItem `json:"campaigns"`
}

// qoderCampaignItem 活动项。字段按**两套形态并集**解析：
//   - 国际版（openapi.qoder.sh）：campaignId / campaignKey / actionType / claimStatus / benefit
//   - CN 客户端逆向形态：id / claimUrl / claimable
//
// 两套都保留、判定统一走 isClaimable()，避免换域名或换客户端版本后整段失效。
type qoderCampaignItem struct {
	ID          string          `json:"id"`
	CampaignID  string          `json:"campaignId"`
	CampaignKey string          `json:"campaignKey"`
	Title       string          `json:"title"`
	ClaimURL    string          `json:"claimUrl"`
	Description string          `json:"description"`
	ActionType  string          `json:"actionType"`
	ClaimStatus string          `json:"claimStatus"`
	Claimable   bool            `json:"claimable"`
	Benefit     json.RawMessage `json:"benefit"`
}

// isClaimable 是否可领：优先按 claimStatus（CLAIMABLE），回落布尔 claimable。
//
// ⚠️ 活动 key 固定不按天变（如 act-20260923-252），轮次每日 10:00 刷新。
// 判「今日可否领」只看状态，**绝不要按日期匹配 campaignKey**——两边都踩过这个坑。
func (it qoderCampaignItem) isClaimable() bool {
	if s := it.ClaimStatus; s != "" {
		return eqFoldASCII(s, "CLAIMABLE")
	}
	return it.Claimable
}

// isBenefit 是否积分领取类活动。actionType 缺失时（CN 形态不带该字段）不因此排除，
// 交由 isClaimable 单独判定，避免字段缺失造成漏领。
func (it qoderCampaignItem) isBenefit() bool {
	if s := it.ActionType; s != "" {
		return eqFoldASCII(s, "CLAIM_BENEFIT")
	}
	return true
}

// id 领取用的活动 ID（两套字段名兜底）。空 = 无法代领。
func (it qoderCampaignItem) id() string {
	if it.CampaignID != "" {
		return it.CampaignID
	}
	return it.ID
}

// label 日志/提示里的可读标识。
func (it qoderCampaignItem) label() string {
	if it.CampaignKey != "" {
		return it.CampaignKey
	}
	if it.Title != "" {
		return it.Title
	}
	return it.id()
}

// qoderClaimResp POST .../claim 响应。
// 成功判据只有 status==CLAIMED；replayed=true 表示本次是幂等重放（今日已领过）。
type qoderClaimResp struct {
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// qoderLimitedNumberResp GET .../client_launch_26/limited-number 响应（限量号状态）。
type qoderLimitedNumberResp struct {
	HasNumber bool   `json:"hasNumber"`
	Number    int64  `json:"number"`
	CreatedAt string `json:"createdAt"`
}

// qoderDailyCheckinURL 构造活动状态请求 URL。
func qoderDailyCheckinURL() string {
	return qoderOpenapiBase + qoderCampaignsPath
}

// qoderLimitedNumberURL 构造限量号请求 URL。
func qoderLimitedNumberURL() string {
	return fmt.Sprintf("%s%s/%s/limited-number", qoderOpenapiBase, qoderCampaignsPath, qoderLimitedNumberCampaign)
}

// qoderClaimURL 构造领取请求 URL。
// campaignID 必须已通过 qoderSafeCampaignID 校验（防路径注入）。
func qoderClaimURL(campaignID string) string {
	return fmt.Sprintf("%s%s/%s/claim", qoderOpenapiBase, qoderCampaignsPath, campaignID)
}

// qoderSafeCampaignID 校验活动 ID 只含 [A-Za-z0-9_-]。
// 活动 ID 来自上游响应（理论上可信），但它会被拼进 URL 路径——上游被污染或
// 中间人改写时，"../" 之类会造成路径穿越打到别的端点，故在边界处收紧。
func qoderSafeCampaignID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// eqFoldASCII 限定 ASCII 的忽略大小写比较（上游状态量恒为 ASCII 大写枚举，
// 无需引入 strings.EqualFold 的 Unicode 开销与语义）。
//
// ⚠️ 两边都要折叠：只折叠一侧而拿它去比大写常量，永远不相等
// （曾因此让 CLAIMABLE/CLAIMED 全部判 false，领取链路静默失效）。
func eqFoldASCII(s, want string) bool {
	if len(s) != len(want) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c, w := s[i], want[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if w >= 'A' && w <= 'Z' {
			w += 'a' - 'A'
		}
		if c != w {
			return false
		}
	}
	return true
}

// QoderDailyCheckin Qoder 每日权益领取。
//
// 返回：
//   - nil                          → 今日无可领活动（幂等静默，等价"今天已签到"）
//   - *Error{Kind:ErrCheckinInfo}   → 领取成功 / 今日已领 / 有可领但需桌面端手点
//   - 其他 *Error                   → 真实故障（网络/鉴权/5xx），由调度器按 error 记录
//
// 注：成功与「已领」都走 ErrCheckinInfo——它是「信息性结果」而非错误，调度器
// 据此打 info 日志且不触发账号冷却，语义与 codebuddy 的"今天已签到"对齐。
func (c *Client) QoderDailyCheckin(a *auth.Auth) error {

	// jobToken 滚动刷新（AccessToken 即当前 jobToken）。
	// 单飞（2026-10-20）：与 chat/modelsync/RefreshIfNeeded 共用同一单飞组。
	if a.NeedsRefresh(qoderRefreshSkew) {
		if a.RefreshToken == "" {
			return &Error{Kind: ErrSessionDead, Status: 401, Msg: "qoder refresh_token missing"}
		}
		_ = c.qoderRefreshSingleflight(a)
		if a.NeedsRefresh(qoderRefreshSkew) {
			return &Error{Kind: ErrSessionDead, Status: 401, Msg: "qoder token refresh failed"}
		}
	}
	if a.UID == "" || a.AccessToken == "" {
		return &Error{Kind: ErrSessionDead, Status: 401, Msg: "qoder auth incomplete (uid/jobToken missing)"}
	}

	// 设备指纹：多账号各按 uid 取用。取不到 → 仅监测模式（不报错、不误报无活动）。
	fp := loadQoderFingerprint(a.UID)
	if fp == nil {
		log.Printf("qoder checkin %s: 未取到设备指纹（%s 缺失或不含该 uid 条目），"+
			"降级为仅监测模式（campaigns 大概率返回空，需本机 runtime-info.exe 采集指纹后写入）",
			a.UID, qoderFingerprintPath())
	}

	// 1) 活动状态（核心）。
	camps, err := c.qoderGetCampaigns(a, fp)
	if err != nil {
		return err
	}

	// 2) 限量号状态（辅助信息，失败不影响主流程）。
	if _, lnErr := c.qoderGetLimitedNumber(a, fp); lnErr != nil {
		// 仅 logging，不阻断：limited-number 与"可领活动"是两套独立机制。
		_ = lnErr
	}

	// 3) 有可领的积分活动 → 真实代领（需指纹 + 活动 ID 齐备）。
	for _, it := range camps.Campaigns {
		if !it.isBenefit() || !it.isClaimable() {
			continue
		}
		id := it.id()
		if fp == nil || !qoderSafeCampaignID(id) {
			break // 缺指纹或 ID 不可用 → 落到下面的深链分支
		}
		return c.qoderClaim(a, fp, id, it.label())
	}

	// 4) 兜底：顶层 claimable（CN 客户端形态，campaigns[] 可能为空）→ 给深链。
	//    优先用 campaigns[] 里的 claimUrl，回落到顶层 campaignUrl。
	if camps.Claimable {
		url := camps.CampaignURL
		title := ""
		if len(camps.Campaigns) > 0 {
			if u := camps.Campaigns[0].ClaimURL; u != "" {
				url = u
			}
			title = camps.Campaigns[0].label()
		}
		if url == "" {
			// 标记可领但无 URL：仍告知，但提示需桌面端（兜底文案）。
			return &Error{
				Kind: ErrCheckinInfo,
				Msg:  fmt.Sprintf("qoder 有可领活动（%s），但服务端未提供领取链接，请在桌面端用量面板领取", title),
			}
		}
		return &Error{
			Kind: ErrCheckinInfo,
			Msg:  fmt.Sprintf("qoder 有可领活动（%s），请在桌面端打开领取: %s", title, url),
		}
	}

	// 5) 无可领活动 → 幂等静默（每日 no-op）。
	//    注意：fp==nil 时这里的"无活动"可能是没指纹造成的假象，上方已打 warn。
	return nil
}

// qoderClaim POST /sash/api/v1/me/campaigns/{id}/claim 领取活动。
// 幂等：重复领取由上游 replayed 标记兜底，不会重复发放。
func (c *Client) qoderClaim(a *auth.Auth, fp *qoderFingerprint, campaignID, label string) error {
	// 空 body：上游不要求额外参数（req_source 等由头族表达）。
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		qoderClaimURL(campaignID), bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	qoderApplyFingerprint(req, a.AccessToken, fp)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Kind: ErrServer, Msg: "qoder claim request: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return &Error{Kind: qoderCheckinHTTPKind(resp.StatusCode), Status: resp.StatusCode,
			Msg: fmt.Sprintf("qoder claim %d: %s", resp.StatusCode, string(body))}
	}
	var out qoderClaimResp
	if err := json.Unmarshal(body, &out); err != nil {
		return &Error{Kind: ErrClient, Msg: "qoder claim parse: " + err.Error()}
	}
	if eqFoldASCII(out.Status, "CLAIMED") {
		if out.Replayed {
			return &Error{Kind: ErrCheckinInfo, Msg: "qoder 今日已领取（幂等重放）"}
		}
		return &Error{Kind: ErrCheckinInfo, Msg: fmt.Sprintf("qoder 领取成功（%s）", label)}
	}
	// 非 CLAIMED：如实上报，不谎称成功。
	return &Error{Kind: ErrClient, Msg: fmt.Sprintf("qoder claim 未返回 CLAIMED（%s）: status=%q code=%s message=%s",
		label, out.Status, out.Code, out.Message)}
}

// qoderGetCampaigns GET /sash/api/v1/me/campaigns。
func (c *Client) qoderGetCampaigns(a *auth.Auth, fp *qoderFingerprint) (*qoderCampaignsResp, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, qoderDailyCheckinURL(), nil)
	if err != nil {
		return nil, err
	}
	qoderApplyFingerprint(req, a.AccessToken, fp)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrServer, Msg: "qoder campaigns request: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Kind: qoderCheckinHTTPKind(resp.StatusCode), Status: resp.StatusCode,
			Msg: fmt.Sprintf("qoder campaigns %d: %s", resp.StatusCode, string(body))}
	}
	var out qoderCampaignsResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &Error{Kind: ErrClient, Msg: "qoder campaigns parse: " + err.Error()}
	}
	return &out, nil
}

// qoderGetLimitedNumber GET .../client_launch_26/limited-number（辅助）。
func (c *Client) qoderGetLimitedNumber(a *auth.Auth, fp *qoderFingerprint) (*qoderLimitedNumberResp, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, qoderLimitedNumberURL(), nil)
	if err != nil {
		return nil, err
	}
	qoderApplyFingerprint(req, a.AccessToken, fp)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrServer, Msg: "qoder limited-number request: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Kind: qoderCheckinHTTPKind(resp.StatusCode), Status: resp.StatusCode,
			Msg: fmt.Sprintf("qoder limited-number %d: %s", resp.StatusCode, string(body))}
	}
	var out qoderLimitedNumberResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &Error{Kind: ErrClient, Msg: "qoder limited-number parse: " + err.Error()}
	}
	return &out, nil
}

// qoderCheckinHTTPKind 把非 200 状态码映射到 ErrKind（与 QoderQuota 同口径）。
func qoderCheckinHTTPKind(status int) ErrKind {
	switch {
	case status == 401 || status == 403:
		return ErrSessionDead
	case status == 429:
		return ErrSoftRate
	case status == 404:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrClient
	}
	return ErrNone
}

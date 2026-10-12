// trae.go Trae（字节 · CN）provider：凭据兑换与身份解析。
//
// 协议来源（两条交叉印证）：
//  1. GitHub wangmingdong/workbuddy-signin 的 Trae 签到适配器（2026-09 版）；
//  2. 本机 Trae SOLO CN 客户端会话样本 data/trae_session.json
//     （含 auth.userId / auth.host=https://api.trae.cn / account.username）。
//
// 凭据模型（与 codebuddy、qoder 都不同，**别套用它们的思路**）：
//   - 长效凭据 = 浏览器 cookie（约 14 天）→ 存 Auth.RefreshToken；
//   - 短效凭据 = Cloud-IDE-JWT（约 8h）→ **每次签到前用 cookie 现换，不落盘**；
//   - 设备 id = Auth.DeviceToken（客户端真实值，服务端按设备记账）。
//
// ⚠️ 关键坑：桌面客户端 session 里的 auth.token **不能**用于签到。
// ug/checkin_credits 认的是 cookie 换发的那一份 JWT——对照项目已验证
// 「导出桌面 userInfo.token 无需/无效」。所以即便本机有 trae_session.json，
// 导入时仍要提供 cookie。
package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/linguo2625469/l3api/internal/auth"
)

const (
	traeAPIBase   = "https://api.trae.cn"
	traeTokenPath = "/cloudide/api/v3/common/GetUserToken"
	// traeEntUsagePath 积分/额度余额：usage_summary{total_amount, consumed_amount}。
	traeEntUsagePath = "/trae/api/v2/pay/ide_user_ent_usage"
	traeOrigin       = "https://work.trae.cn"
	// traeUA 浏览器同款 UA：ug 接口按 "客户端通道" 校验，非浏览器 UA 易被判异常。
	traeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	// traeReqSource 客户端通道标记（2 = 客户端），与 x-device-id 配套。
	traeReqSource = 2
)

// TraeSession 一次 cookie 兑换的产物（JWT 短效，不落盘）。
type TraeSession struct {
	JWT      string
	UID      string
	Nickname string
}

// traeTokenResp POST /cloudide/api/v3/common/GetUserToken 响应。
// 实测取值路径 Result.Token（字节系 API 通用信封）；另留平铺 token 兜底，
// 以防上游换形态时不至于整条链路静默失效。
type traeTokenResp struct {
	Result struct {
		Token string `json:"Token"`
	} `json:"Result"`
	Token   string `json:"token"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// TraeExchangeCookie cookie → Cloud-IDE-JWT（8h）。uid/昵称从 JWT payload 解析，
// 无需额外调 userinfo 端点。deviceID 仅参与校验，不进换发请求。
func (c *Client) TraeExchangeCookie(ctx context.Context, cookie, deviceID string) (*TraeSession, error) {
	if strings.TrimSpace(cookie) == "" {
		return nil, fmt.Errorf("trae: empty cookie")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		traeAPIBase+traeTokenPath, strings.NewReader(""))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", traeOrigin)
	req.Header.Set("Referer", traeOrigin+"/")
	req.Header.Set("User-Agent", traeUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrServer, Msg: "trae GetUserToken request: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		kind := ErrClient
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			kind = ErrSessionDead // cookie 过期：需要用户重导，不是重试能解决的
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("trae GetUserToken %d: %s", resp.StatusCode, string(body))}
	}
	var out traeTokenResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &Error{Kind: ErrClient, Msg: "trae GetUserToken parse: " + err.Error()}
	}
	jwt := out.Result.Token
	if jwt == "" {
		jwt = out.Token
	}
	if jwt == "" {
		// 换不出 token 的绝大多数原因是 cookie 过期，按会话失效上报（提示重导而非重试）。
		return nil, &Error{Kind: ErrSessionDead,
			Msg: "trae cookie 换发 JWT 失败（cookie 可能已过期），请重新导出 cookie"}
	}
	uid, nick := traeJWTIdentity(jwt)
	return &TraeSession{JWT: jwt, UID: uid, Nickname: nick}, nil
}

// traeUIDKeys / traeNameKeys / traeDataUIDKeys JWT payload 里身份与昵称的候选键
// （按命中优先级）。字节系声明命名不统一，且 uid 常嵌在 data 子对象里：
//
//	实测形态：{"data":{"id":"2521630176970859","type":"user",...},"exp":...,"iat":...}
//
// 故顶层与 data.id 都要找。
var (
	traeUIDKeys     = []string{"userId", "user_id", "UserId", "uid", "accountId", "account_id", "sub"}
	traeDataUIDKeys = []string{"id", "userId", "user_id", "uid"}
	traeNameKeys    = []string{"username", "user_name", "nickname", "nick_name", "name"}
)

// traeJWTIdentity 解 JWT payload 取 (uid, nickname)。
//
// **不验签**——只用来取 uid/昵称做落盘与展示，真正的身份校验由上游在请求时完成。
// 解码失败一律返回空串（由调用方决定回退策略），不阻断主流程。

// TraeUserInfo Trae 用户信息（从 /icube/api/v1/user 取昵称；失败时返回 JWT 里的 uid）。
type TraeUserInfo struct {
	UID      string
	Nickname string
	Username string
}

// GetUserInfo 用 cookie 换 JWT 后调 /icube/api/v1/user 取用户信息。
// 这是补昵称的唯一途径：JWT payload 里没有用户名字段。
func (c *Client) TraeGetUserInfo(ctx context.Context, cookie string) (*TraeUserInfo, error) {
	// 先换 JWT（8h 短效）
	sess, err := c.TraeExchangeCookie(ctx, cookie, "")
	if err != nil {
		return nil, err
	}
	// 用 JWT 调 user info 接口
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.trae.cn/icube/api/v1/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+sess.JWT)
	req.Header.Set("User-Agent", traeUA)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrServer, Msg: "trae user/info request: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		// 404 时返回 JWT 里的 uid（无昵称）
		return &TraeUserInfo{UID: sess.UID}, nil
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			UserID   string `json:"user_id"`
			UID      string `json:"uid"`
			Username string `json:"username"`
			Nickname string `json:"nickname"`
			Name     string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return &TraeUserInfo{UID: sess.UID}, nil
	}
	uid := out.Data.UserID
	if uid == "" {
		uid = out.Data.UID
	}
	if uid == "" {
		uid = sess.UID
	}
	nick := out.Data.Nickname
	if nick == "" {
		nick = out.Data.Name
	}
	if nick == "" {
		nick = out.Data.Username
	}
	return &TraeUserInfo{UID: uid, Nickname: nick, Username: out.Data.Username}, nil
}

func traeJWTIdentity(jwt string) (uid, nickname string) {
	m := traeJWTPayload(jwt)
	if m == nil {
		return "", ""
	}
	nickname = traePickStr(m, traeNameKeys...)
	uid = traePickStr(m, traeUIDKeys...)
	if uid == "" {
		if d, ok := m["data"].(map[string]any); ok {
			uid = traePickStr(d, traeDataUIDKeys...)
		}
	}
	return uid, nickname
}

// traeJWTPayload 解 JWT 第二段（payload）。
func traeJWTPayload(jwt string) map[string]any {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 少数实现会带 padding。
		seg := parts[1]
		switch len(seg) % 4 {
		case 2:
			seg += "=="
		case 3:
			seg += "="
		}
		if raw, err = base64.URLEncoding.DecodeString(seg); err != nil {
			return nil
		}
	}
	// ⚠️ 必须 UseNumber：Trae 的 userId 是 16~19 位数字，默认解析成 float64
	// 会在 2^53 附近丢精度，uid 落盘后与上游对不上。
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil
	}
	return m
}

// traePickStr 从 payload 映射里按候选键取字符串（数字按 json.Number 原样转串）。
func traePickStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case json.Number:
			s := t.String()
			// 防御：万一上游把 id 写成浮点形式，截掉小数部分再落盘。
			if i := strings.IndexByte(s, '.'); i >= 0 {
				s = s[:i]
			}
			if s != "" {
				return s
			}
		}
	}
	return ""
}

// TraeDeviceIDFor 由 uid 派生一个稳定、互不相同的 x-device-id。
//
// 实测（2026-09-28）：ug/checkin_credits **只把 x-device-id 当「按设备记账」的 key**，
// 并不校验它是否真的来自某个 Trae 客户端——用任意 16 位数字（凭空编造）走完整
// status/claim 链路，返回 code=0 签到成功。真正的门禁在 JWT（cookie 换发的那份）。
//
// 这直接解决了多账号问题：服务端按设备维度限制「每设备每日一次」（code 9095
// 「当前设备今日已经签到」），所以多账号只要**各用各的 device id** 就能各自签到，
// 不需要为每个账号准备一台真实机器，也不必去别处采集。
//
// 派生而非随机，是为了保证同一账号每次签到都用同一个 id（设备 id 漂移反而可疑）。
func TraeDeviceIDFor(uid string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte("l3api-trae-" + uid))
	s := strconv.FormatUint(h.Sum64(), 10)
	if len(s) < 16 {
		s = strings.Repeat("0", 16-len(s)) + s
	}
	if len(s) > 16 {
		s = s[:16]
	}
	return s
}

// traeEntUsageResp POST /trae/api/v2/pay/ide_user_ent_usage 响应（只取所需字段）。
type traeEntUsageResp struct {
	IsCreditsBilling bool `json:"is_credits_billing"`
	UsageSummary     struct {
		TotalAmount    float64 `json:"total_amount"`
		ConsumedAmount float64 `json:"consumed_amount"`
	} `json:"usage_summary"`
}

// TraeQuota 查询积分/额度余额。
//
// 端点：POST /trae/api/v2/pay/ide_user_ent_usage（"IDE user entitlement usage"）。
// 来源：客户端源码 out/main.js 提取的完整端点表；2026-09-28 实测返回
// usage_summary{total_amount:1450, consumed_amount:107.9}——签到送的 work_credits
// 计入 total（免费包 + 签到 200 = 1450 吻合），remain = total - consumed。
func (c *Client) TraeQuota(ctx context.Context, a *auth.Auth) (remain, total int64, err error) {
	cookie := a.RefreshToken
	deviceID := a.DeviceToken
	if cookie == "" {
		return 0, 0, &Error{Kind: ErrSessionDead, Status: 401, Msg: "trae cookie 缺失（auth.RefreshToken 为空）"}
	}
	if deviceID == "" {
		deviceID = TraeDeviceIDFor(a.UID)
	}
	sess, err := c.TraeExchangeCookie(ctx, cookie, deviceID)
	if err != nil {
		return 0, 0, err
	}
	raw, err := c.traePostJSON(ctx, sess.JWT, deviceID, traeEntUsagePath,
		map[string]any{"req_source": traeReqSource})
	if err != nil {
		return 0, 0, err
	}
	var out traeEntUsageResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, 0, &Error{Kind: ErrClient, Msg: "trae ent_usage parse: " + err.Error()}
	}
	remainF := out.UsageSummary.TotalAmount - out.UsageSummary.ConsumedAmount
	if remainF < 0 {
		remainF = 0
	}
	// float 抹 0.5 防止 1341.999 → 1341。
	return int64(remainF + 0.5), int64(out.UsageSummary.TotalAmount + 0.5), nil
}

// traePostJSON 带 JWT + 设备头的 ug 系列 POST，返回原始响应体。
//
// 头严格对齐 TRAE SOLO CN 客户端实现：**只有 x-device-id 是设备头**。
// ⚠️ 不要自作主张加 x-device-model / x-device-system / x-device-client-version
// ——客户端根本没有这三个头，加上会被服务端判为异常请求（code 9004）。
func (c *Client) traePostJSON(ctx context.Context, jwt, deviceID, path string, payload map[string]any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		traeAPIBase+path, strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", traeUA)
	req.Header.Set("Origin", traeOrigin)
	req.Header.Set("Referer", traeOrigin+"/")
	req.Header.Set("x-device-id", deviceID)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrServer, Msg: "trae " + path + " request: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		kind := ErrClient
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			kind = ErrSessionDead
		} else if resp.StatusCode >= 500 {
			kind = ErrServer
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("trae %s %d: %s", path, resp.StatusCode, string(body))}
	}
	return body, nil
}

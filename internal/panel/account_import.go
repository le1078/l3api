// account_import.go 面板「添加账号」的各品牌导入路径。
//
// 每个品牌的接入方式由上游决定，互不相同：
//
//	qoder  → PAT（控制台生成）换 jobToken（24h 滚动）落盘；PAT 本身不落盘。
//	trae   → 浏览器 cookie（长效）+ 客户端真实 device id；JWT 每次签到前现换，不落盘。
//	codebuddy → 走 OAuth 设备流（本文件不处理，见 login.go）。
//
//	POST /panel/api/accounts/import
//	  {"brand":"qoder","pat":"pt-..."}
//	  {"brand":"trae","cookie":"...","device_id":"...","uid":"可选"}
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/provider"
	"github.com/linguo2625469/l3api/internal/upstream"
)

// accountImportReq /panel/api/accounts/import 请求体。
// qoder 用 PAT；trae 用 cookie + device_id（uid 可选，仅在 JWT 解析不出 uid 时需要）。
type accountImportReq struct {
	Brand    string `json:"brand"`
	PAT      string `json:"pat"`
	Cookie   string `json:"cookie"`
	DeviceID string `json:"device_id"`
	UID      string `json:"uid"`
}

// accountImport 品牌会话导入。
func (p *Panel) accountImport(w http.ResponseWriter, r *http.Request) {
	var req accountImportReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	brand := provider.Normalize(req.Brand)
	switch brand {
	case provider.Qoder:
		p.importQoder(w, r, req.PAT)
	case provider.Trae:
		p.importTrae(w, r, req.Cookie, req.DeviceID, req.UID)
	default:
		writeErr(w, http.StatusBadRequest, "unsupported brand（当前支持 qoder / trae；codebuddy 请用 OAuth 添加）")
	}
}

// importQoder PAT → jobToken → 落盘 qoder-<uid>.json → 热加载。
func (p *Panel) importQoder(w http.ResponseWriter, r *http.Request, pat string) {
	pat = trimPAT(pat)
	if pat == "" {
		writeErr(w, http.StatusBadRequest, "missing pat")
		return
	}
	sess, err := p.cfg.Upstream.QoderExchangePAT(r.Context(), pat)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "qoder PAT 兑换失败: "+err.Error())
		return
	}
	if sess.UID == "" {
		writeErr(w, http.StatusBadGateway, "qoder 兑换成功但未取到 uid（userinfo 失败），请重试")
		return
	}
	if !validUID(sess.UID) {
		writeErr(w, http.StatusBadGateway, "qoder 返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}

	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a := &auth.Auth{
		AccessToken:  sess.JobToken, // AccessToken 滚动为当前 jobToken（chat 身份）
		RefreshToken: sess.RefreshToken,
		ExpiresAt:    sess.ExpiresAt.Unix(),
		UID:          sess.UID,
		Nickname:     sess.Nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("qoder-%s.json", sess.UID)),
	}
	// 品牌标识必须落盘（LoadDir 据此分流；文件名前缀推断只是兜底）。
	if _, err := auth.BackfillBrandFor(a, string(provider.Qoder)); err != nil {
		writeErr(w, http.StatusInternalServerError, "set brand: "+err.Error())
		return
	}
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(sess.UID)
	log.Printf("panel: Qoder 账号已导入 uid=%s nickname=%q（免重启生效）", sess.UID, sess.Nickname)
	resp := map[string]any{
		"ok":       true,
		"brand":    string(provider.Qoder),
		"uid":      sess.UID,
		"nickname": sess.Nickname,
		"expires":  sess.ExpiresAt.Format(time.RFC3339),
	}
	// 顺带拉一次余额（失败不阻断导入，仅缺展示数值）。
	if remain, total, berr := p.cfg.Upstream.QoderQuota(r.Context(), a); berr == nil {
		p.cfg.Pool.SetCredits(sess.UID, remain, total)
		resp["credits"] = remain
		resp["credits_total"] = total
	}
	writeJSON(w, http.StatusOK, resp)
}

// importTrae cookie（必填）+ device id（可选）→ 落盘 trae-<uid>.json → 热加载。
//
// uid 优先取请求体显式指定的；其次从 JWT payload 解析（Trae 的 userId 声明）。
// 两者都取不到时拒绝落盘——uid 是池索引与文件名来源，不能瞎编。
// device id 留空则由 uid 自动派生（每账号一个，规避"每设备每日一次"的 9095）。
func (p *Panel) importTrae(w http.ResponseWriter, r *http.Request, cookie, deviceID, wantUID string) {
	cookie = trimPAT(cookie)
	deviceID = strings.TrimSpace(deviceID)
	wantUID = strings.TrimSpace(wantUID)
	if cookie == "" {
		writeErr(w, http.StatusBadRequest, "missing cookie（浏览器登录 work.trae.cn 后导出的完整 cookie 串）")
		return
	}
	sess, err := p.cfg.Upstream.TraeExchangeCookie(r.Context(), cookie, deviceID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "trae cookie 兑换失败: "+err.Error())
		return
	}
	uid := wantUID
	if uid == "" {
		uid = sess.UID
	}
	if uid == "" {
		writeErr(w, http.StatusBadGateway,
			"未能确定 uid：JWT 里没有可识别的 userId 声明，请在导入表单手工填写 uid")
		return
	}
	if !validUID(uid) {
		writeErr(w, http.StatusBadRequest, "uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}
	// device_id 可选：留空则由 uid 派生一个稳定的记账 id。
	//
	// 这么做是因为实测发现 x-device-id 只作「按设备记账」的 key、不校验真伪
	// （见 upstream.TraeDeviceIDFor 的说明）。服务端按设备限制「每设备每日一次」
	// （code 9095），所以**多账号必须各用各的 device id**；按 uid 派生既保证互不相同，
	// 又保证同一账号每次签到恒定——也就不需要为每个账号去找一台真实机器。
	if deviceID == "" {
		deviceID = upstream.TraeDeviceIDFor(uid)
	}

	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a := &auth.Auth{
		// AccessToken 存一份 JWT 只为满足凭证落盘校验（SaveAtomic 拒绝空值）。
		// ⚠️ 它是 8h 短效且**签到时不读**——TraeDailyCheckin 每次用 cookie 现换。
		// 别把它当有效凭据用，也别指望 refresh 端点去续它。
		AccessToken:  sess.JWT,
		RefreshToken: cookie,   // 长效凭据（约 14 天），签到全靠它
		DeviceToken:  deviceID, // x-device-id，服务端按设备记账
		UID:          uid,
		Nickname:     sess.Nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("trae-%s.json", uid)),
		// 按 cookie 的有效期设：trae 无 refresh 端点（RefreshToken 恒 nil），
		// 设短了会让 NeedsRefresh 反复触发并可能被池判为过期。
		ExpiresAt: time.Now().AddDate(0, 0, 14).Unix(),
	}
	if _, err := auth.BackfillBrandFor(a, string(provider.Trae)); err != nil {
		writeErr(w, http.StatusInternalServerError, "set brand: "+err.Error())
		return
	}
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: Trae 账号已导入 uid=%s nickname=%q（免重启生效）", uid, sess.Nickname)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"brand":    string(provider.Trae),
		"uid":      uid,
		"nickname": sess.Nickname,
		"expires":  time.Unix(a.ExpiresAt, 0).Format(time.RFC3339),
	})
}


// trimPAT 去掉首尾空白/换行（用户从控制台复制常带换行）。
// trimPAT 去掉首尾空白/换行（用户从控制台复制常带换行）。
func trimPAT(s string) string {
	for len(s) > 0 {
		c := s[0]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			s = s[1:]
			continue
		}
		break
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

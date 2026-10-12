// trae_checkin.go Trae 每日签到领积分（CN 版 ug/checkin_credits 体系）。
//
// 三个端点：
//
//	POST /cloudide/api/v3/common/GetUserToken          cookie → JWT（8h，不落盘）
//	POST /trae/api/v2/ug/checkin_credits/status        查今日状态 {checked_in, credits}
//	POST /trae/api/v2/ug/checkin_credits/claim         领取（body {"req_source":2}）
//
// 判定铁律（对照项目踩过的坑，别重蹈）：
//  1. **只有 code==0 才算成功**，其余 code 一律如实记失败——绝不谎报"已签到"。
//     status 与 claim 各自独立判 code，别混用。
//  2. **x-device-id 必须是 Trae 客户端真实设备 id**（服务端按设备记账）；
//     伪设备 id（如 wb-xxxx 随机串）→ code 9074 直接拒。这个错误极易被误读成
//     "参与用户太多"或"限流"而跑去加退避重试，实际重试一万次也没用。
//  3. 别加客户端不存在的设备头（x-device-model 等）→ code 9004。
//
// 幂等：先查 status，已签则直接返回，不重复 claim。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/linguo2625469/l3api/internal/auth"
)

// traeCheckinPath ug 签到积分端点前缀。
const traeCheckinPath = "/trae/api/v2/ug/checkin_credits"

// traeStatusResp status 响应。code 非 0 = 接口不可用/异常，不能当"未签到"处理。
type traeStatusResp struct {
	Code         int    `json:"code"`
	Message      string `json:"message"`
	CheckedIn    bool   `json:"checked_in"`
	Credits      int64  `json:"credits"`
	ExtraCredits int64  `json:"extra_credits"`
}

// traeClaimResp claim 响应。成功判据只有 Code==0。
type traeClaimResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// TraeDailyCheckin Trae 每日签到。
//
// 返回：
//   - *Error{Kind:ErrCheckinInfo} → 签到成功 / 今日已签（信息性，不触发冷却）
//   - 其他 *Error                 → 真实故障；其中 9074/9004 会带可操作的排查指引
func (c *Client) TraeDailyCheckin(a *auth.Auth) error {
	ctx := context.Background()

	cookie := a.RefreshToken
	deviceID := a.DeviceToken
	if cookie == "" {
		return &Error{Kind: ErrSessionDead, Status: 401,
			Msg: "trae cookie 缺失（auth.RefreshToken 为空）——cookie 是长效凭据，需重新导入账号"}
	}
	if deviceID == "" {
		return &Error{Kind: ErrClient,
			Msg: "trae device id 缺失（auth.DeviceToken 为空）：必须是该账号客户端的真实 device id，伪 id 会被 code 9074 拒绝"}
	}

	// 1) cookie → JWT（每次现换，8h 有效，不落盘）。
	sess, err := c.TraeExchangeCookie(ctx, cookie, deviceID)
	if err != nil {
		return err
	}

	// 2) 查状态：已签则幂等返回，不重复 claim。
	raw, err := c.traePostJSON(ctx, sess.JWT, deviceID, traeCheckinPath+"/status",
		map[string]any{"req_source": traeReqSource})
	if err != nil {
		return err
	}
	var st traeStatusResp
	if err := json.Unmarshal(raw, &st); err != nil {
		return &Error{Kind: ErrClient, Msg: "trae status parse: " + err.Error()}
	}
	if st.Code != 0 {
		// 接口抖动（code 1001 等）如实上报，不误判为"未签到"也不报"已签到"。
		return &Error{Kind: ErrClient,
			Msg: fmt.Sprintf("trae status 异常 code=%d message=%s（多为接口瞬时抖动，无需重导凭据）",
				st.Code, st.Message)}
	}
	if st.CheckedIn {
		return &Error{Kind: ErrCheckinInfo, Msg: "trae 今日已签到（幂等跳过）"}
	}

	// 3) 领取。
	raw, err = c.traePostJSON(ctx, sess.JWT, deviceID, traeCheckinPath+"/claim",
		map[string]any{"req_source": traeReqSource})
	if err != nil {
		return err
	}
	var cr traeClaimResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return &Error{Kind: ErrClient, Msg: "trae claim parse: " + err.Error()}
	}
	switch cr.Code {
	case 0:
		// 写成"基础 + 会员 = 合计"而非"+150（会员 +50）"——后者容易被读成"只领了 150"。
		// 口径：基础 150，会员另有加成（实测会员合计 200）。
		if st.ExtraCredits > 0 {
			return &Error{Kind: ErrCheckinInfo,
				Msg: fmt.Sprintf("trae 签到成功：基础 %d + 会员 %d = 合计 %d work_credits",
					st.Credits, st.ExtraCredits, st.Credits+st.ExtraCredits)}
		}
		return &Error{Kind: ErrCheckinInfo,
			Msg: fmt.Sprintf("trae 签到成功：%d work_credits", st.Credits)}
	case 9074:
		// 服务端按「设备」记账：伪/陌生设备直接拒。重试无用，只能换真实 device id。
		return &Error{Kind: ErrClient,
			Msg: "trae claim 被拒 code=9074：服务端按设备记账，伪设备 id 或未配置会被拒；" +
				"请确认 auth.DeviceToken 是该账号客户端的真实 device id，且 cookie 仍有效"}
	case 9004:
		// 请求被判参数/通道异常：典型原因是多加了客户端不存在的设备头。
		return &Error{Kind: ErrClient,
			Msg: "trae claim 被拒 code=9004：请求头或参数被判异常（多为多了客户端不存在的 x-device-* 头）"}
	default:
		return &Error{Kind: ErrClient,
			Msg: fmt.Sprintf("trae claim 失败 code=%d message=%s", cr.Code, cr.Message)}
	}
}

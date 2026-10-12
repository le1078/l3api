package provider

// codebuddy 腾讯 CodeBuddy：全能力品牌（本项目原始品牌，任务/签到/活动体系齐全）。
type codebuddy struct{}

func (codebuddy) Brand() Brand        { return CodeBuddy }
func (codebuddy) DisplayName() string { return "CodeBuddy (WorkBuddy)" }
func (codebuddy) Capabilities() Capabilities {
	return Capabilities{
		Checkin:  true,
		Activity: true,
		Travel:   true,
		Blackcat: true,
		Tasks:    true,
		School:   true,
		Balance:  true,
	}
}

// qoder 阿里 Qoder：PAT 令牌接入。Balance 已接通（GET /api/v2/quota/usage）；
// Checkin = 每日权益领取（每日 100 积分），走 Campaign 体系：
//
//	GET  /sash/api/v1/me/campaigns            筛 CLAIM_BENEFIT + claimStatus=CLAIMABLE
//	POST /sash/api/v1/me/campaigns/{id}/claim 幂等（响应带 replayed）
//
// 2026-09-28 修正：此前注释称「领取是桌面端 webview 专属、服务端无法代领」，
// 属观测偏差——请求未带 Cosy-Machine* 设备指纹头时 campaigns 恒返回空数组，
// 被误读成"今天没活动"。真实卡点是指纹而非"没有接口"：
// 指纹由本机客户端 runtime-info.exe 生成，且**不绑 IP**，可在服务端原样重放，
// 故网关拿到指纹即可代领（见 internal/upstream/qoder_fingerprint.go）。
//
// 多账号：指纹按 uid 索引存放（data/qoder_fingerprints.json），pool 遍历签到时
// 各取各的指纹；取不到指纹的账号自动降级为「仅监测」（给领取深链，不伪报已领）。
type qoder struct{}

func (qoder) Brand() Brand        { return Qoder }
func (qoder) DisplayName() string { return "Qoder (阿里)" }
func (qoder) Capabilities() Capabilities {
	return Capabilities{
		Balance: true,
		Checkin: true, // 每日权益领取（有指纹则代领，无指纹降级为仅监测）
	}
}

// trae 字节 Trae CN：cookie + 设备 id 接入（无 PAT / 无 OAuth 设备流）。
//
// 签到与余额均已接通（凭据模型与另两个品牌都不同，详见 internal/upstream/trae.go）：
//   - Auth.RefreshToken ← 浏览器 cookie（长效，约 14 天）
//   - Auth.DeviceToken  ← x-device-id（留空时按 uid 派生，见 TraeDeviceIDFor）
//   - Auth.AccessToken  ← 落盘仅为校验；签到/查余额时 JWT 均由 cookie 现换
//
// Checkin：ug/checkin_credits 体系（status → claim，code==0 才算成功）。
// Balance：pay/ide_user_ent_usage 的 usage_summary（total - consumed = 剩余），
// 签到积分计入 total。其余 codebuddy 运营能力（旅行/夜猫子/活动等）无对等物。
type trae struct{}

func (trae) Brand() Brand        { return Trae }
func (trae) DisplayName() string { return "Trae (字节·CN)" }
func (trae) Capabilities() Capabilities {
	return Capabilities{
		Checkin: true,
		Balance: true, // pay/ide_user_ent_usage → usage_summary
	}
}

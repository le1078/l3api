package scheduler

import (
	"time"
)

// creditdelta.go 协议桥品牌（trae/qoder）的积分消耗记账（v1.17.8，余额差值法）。
//
// 背景：codebuddy（cn）上游的聊天流末帧自带 usage.credit（本次真实扣费），
// 网关逐请求记账（handler.recordCredit）即可让「今日消耗积分」对得上；而
// trae/qoder 走协议桥——上游聊天流（token_usage 事件）只有 token 数，**没有任何
// 逐请求扣费字段**，桥翻译出的 OpenAI 末帧因此也没有 usage.credit，逐请求路径对
// 这两家永远记 0（2026-10-24 实测：usage.json 里 trae/qoder 两域积分恒 0，而账号
// 余额确实在掉）。
//
// global 域（codebuddy 国际版）**不走差值法**：2026-10-24 实测其末帧确实带
// usage.credit（global:deepseek-v4.1-flash-sg 4 请求记 0.45，落在真实模型名的
// 桶里、req=4，可证是逐请求记账），机制一直正常，只是用量小所以数字小——叠加
// 差值会双记。cr=0 的 global 模型是上游不计费/免费，与 cn 域同口径。
//
// 补法：余额差值。余额刷新本来就在对全部账号逐号查余额（UserResourceDetailed，
// balance_refresh_minutes 默认 5 分钟一轮），把每号「累计消耗」读数（consumed =
// total - remain）记下来，相邻两次读数的正差值即这段时间的真实消耗。充值/签到只
// 同时抬高 total 与 remain、不改变差值，天然免疫；上游周期重置出现负差值时只重立
// 基线、不记账。
//
// 归因粒度：差值只知道「这轮消耗了多少」，不知道落在哪个模型/Key——记到合成模型
// 「(余额差值)」名下（Key 留空，按 Key 拆分的聚合会自动跳过空 Key 桶）。按品牌/
// 按账号/今日合计的口径都精确；按模型表会多一行合成模型（token 侧按模型分布不受
// 影响，token 本就来自逐请求记录）。归账域即品牌名（trae→trae、qoder→qoder，
// 与 handler.usageRealm 口径一致）。
//
// 精度：余额读数是 int64（±0.5 取整），相邻差值误差 ≤1 积分且不随轮次累积（一天
// 首尾读数之差 = 真实日消耗 ±1）。进程重启丢基线 → 重启后首轮只立基线不记账，
// 丢失量 ≤ 一轮间隔的消耗（网关停机期间没有代理流量，损失天然有限）。

// creditModelMarker 余额差值记账用的合成模型名（面板按模型表里的展示名）。
const creditModelMarker = "(余额差值)"

// observeCreditDelta 记录一次 trae/qoder 账号的「累计消耗」读数，正差值入账。
// brand 即归账域（与 handler.usageRealm 同口径：品牌账号 realm=品牌名）。
func (s *Scheduler) observeCreditDelta(uid, brand string, consumed int64) {
	if s.cfg.Usage == nil || uid == "" || brand == "" {
		return
	}
	s.creditMu.Lock()
	prev, seen := s.creditLast[uid]
	s.creditLast[uid] = float64(consumed)
	s.creditMu.Unlock()
	// 首次观测只立基线：没有上一轮读数，差值无从谈起。
	if !seen {
		return
	}
	delta := float64(consumed) - prev
	// 负差值 = 上游周期重置/读数回退：基线已更新（上方），这段「负消耗」不记账。
	if delta <= 0 {
		return
	}
	// 差值属于「上一轮到本轮」的消耗，落当前小时桶（观测时刻取现在）。
	s.cfg.Usage.AddCredit(time.Now(), brand, uid, creditModelMarker, "", delta)
}

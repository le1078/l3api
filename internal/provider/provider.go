// Package provider 上游品牌（provider）抽象：l3api 在单品牌网关（旧名 workbuddy2api）
// 之上做多品牌接入的第一层接缝。品牌行为的实质实现（token 刷新、chat 上游、
// SSE 重建、错误分类）仍逐步从 internal/upstream 收拢过来；本包先固定
// 「品牌标识 + 能力声明 + 注册表」这三个所有消费方都需要的稳定面。
package provider

import "strings"

// Brand 上游品牌标识（auth 文件 brand 键的合法值域）。
type Brand string

const (
	// CodeBuddy 腾讯 CodeBuddy / WorkBuddy（本项目原始品牌，老凭证缺省值）。
	CodeBuddy Brand = "codebuddy"
	// Qoder 阿里 Qoder（PAT 令牌接入）。
	Qoder Brand = "qoder"
	// Trae 字节 Trae CN（OAuth 接入）。
	Trae Brand = "trae"
)

// Normalize 归一化品牌串：空串回落 CodeBuddy（老凭证/缺省零回归），
// 未知值同样回落 CodeBuddy 并由调用方日志告警（防御脏数据把账号打进死胡同）。
// 注：品牌曾有过 "coze"（扣子），2026-10-13 整体移除；旧凭证由 auth 加载
// 路径拒绝加载（provider.Valid 不过），不会流到这里。
func Normalize(s string) Brand {
	switch Brand(strings.ToLower(strings.TrimSpace(s))) {
	case CodeBuddy:
		return CodeBuddy
	case Qoder:
		return Qoder
	case Trae:
		return Trae
	default:
		return CodeBuddy
	}
}

// Valid 报告 s 是否为合法品牌串（空串不算——空串由 Normalize 回落缺省）。
func Valid(s string) bool {
	switch Brand(strings.ToLower(strings.TrimSpace(s))) {
	case CodeBuddy, Qoder, Trae:
		return true
	}
	return false
}

// Capabilities 品牌能力声明：面板/调度器/任务中心据此显隐功能入口，避免对
// 不支持的品牌下发 CodeBuddy 运营接口（打过去只会 404 甚至触发风控）。
// 全 false = 纯网关品牌（仅 chat 转发 + 池管理）。
type Capabilities struct {
	// Checkin 每日签到（codebuddy 签到送积分；trae CN 有 ug/checkin_credits 同类体系）。
	Checkin bool
	// Activity 活跃上报（点亮连登/streak，codebuddy 专属）。
	Activity bool
	// Travel 猫猫旅行（codebuddy 专属运营）。
	Travel bool
	// Blackcat 夜猫子活动（codebuddy 专属）。
	Blackcat bool
	// Tasks 成长任务体系（17 项一键完成 + 任务中心，codebuddy 专属）。
	Tasks bool
	// School 开学季活动（codebuddy 专属活动期功能）。
	School bool
	// Balance 余额/积分/额度查询（三品牌都有对等物，但接口各异；未接通前 false）。
	Balance bool
}

// Provider 单个上游品牌的静态描述。品牌相关的动态行为（token 刷新、chat
// 转发等）暂由 internal/upstream 承担，后续重构逐方法收拢到本接口后面。
type Provider interface {
	Brand() Brand
	// DisplayName 面板/日志展示名（中文，带品牌归属说明）。
	DisplayName() string
	Capabilities() Capabilities
}

// For 按品牌取实现；未知/缺省回落 CodeBuddy（与 Normalize 口径一致）。
func For(b Brand) Provider {
	switch b {
	case Qoder:
		return qoder{}
	case Trae:
		return trae{}
	default:
		return codebuddy{}
	}
}

// All 返回全部已注册品牌（注册表即清单：面板「添加账号」按此生成品牌选择项）。
func All() []Provider {
	return []Provider{codebuddy{}, qoder{}, trae{}}
}

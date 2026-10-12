package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6 + 品牌扩展）：
//
//	分布式前缀： "[realm:]model"  → realm ∈ {cn, global}（codebuddy 域内分流）
//	品牌前缀：    "qoder:model"   → realm="qoder"（Qoder 品牌路由）
//	              "trae:model"    → realm="trae"（Trae 品牌路由）
//
// 取第一个 ":"，前段恰为已知前缀才剥离；否则视为裸名，
// realm=cn、bare=原串。大小写敏感（前缀必须是精确的小写枚举）。
// bare 即出站/选号/账本使用的裸模型名（品牌模型 key，如 qfmodel / glm-5.3）。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if !IsBrandRealm(prefix) && prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// IsBrandRealm 报告 realm 串是否为品牌路由域（而非 codebuddy 的 cn/global 分布域）。
// pool/handler 的选号与粘性判定共用此口径：品牌域按 Brand() 过滤，
// 分布域按 Realm() 过滤且必须排除所有非 codebuddy 品牌账号。
func IsBrandRealm(realm string) bool {
	switch realm {
	case "qoder", "trae":
		return true
	}
	return false
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// paidSuffix 强制付费后缀（free_guard 的单次放行阀）。请求模型名以此结尾时
// 剥掉后缀并置 forcePaid——守护 key 的本请求照常计费调用（绕过免费额度尽排除）。
// 形如 "hy3@paid" / "global:hy3@paid"。
const paidSuffix = "@paid"

// splitPaidSuffix 剥模型名的 "@paid" 后缀（free_guard 单次强制付费放行阀）。
// 命中后缀 → (剥后名, true)；否则原样返回（forcePaid=false）。
// 必须在白名单/resolveModel 之前调用：后缀是网关侧调度协议，上游只认裸名。
func splitPaidSuffix(model string) (clean string, forcePaid bool) {
	if strings.HasSuffix(model, paidSuffix) {
		return strings.TrimSuffix(model, paidSuffix), true
	}
	return model, false
}


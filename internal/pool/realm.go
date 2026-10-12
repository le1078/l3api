// 分池选号域：按 realm 过滤选号与可用集合。realm=="" 退化为现状。
//
// 选号域口径（与 pick.go 的 brandRealm/pickBrand 一致）：
//   - 品牌域（qoder/trae）：只收 Brand()==realm 的账号；
//   - cn/global（codebuddy 分布域）：只收 codebuddy 品牌且 Realm() 匹配的账号；
//   - realm==""：全收（现状语义）。
//
// 修复（2026-09-28）：此前按 Realm()==realm 过滤——品牌账号（qoder/trae）
// Realm() 恒归一为 cn，导致品牌粘性可用集合恒空（qoder 会话粘性静默失效）、
// 且 cn 粘性集合混入 trae 账号（绑到它们必然 401）。
package pool

import (
	"sort"
	"time"
)

// AvailableUIDsForRealm 同 AvailableUIDs，但按选号域过滤（口径见文件头）。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !realmMatch(e, realm) {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但按选号域过滤
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !realmMatch(e, realm) {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// realmMatch 选号域归属判定（与 pickBrand 同口径）：品牌域按 Brand()==realm；
// cn/global 只收 codebuddy 品牌且 Realm() 匹配；realm=="" 全收。
func realmMatch(e *entry, realm string) bool {
	if brandRealm(realm) {
		return e.a.Brand() == realm
	}
	if realm == "" {
		return true
	}
	return e.a.Brand() == "codebuddy" && e.a.Realm() == realm
}

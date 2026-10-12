// brand_realm_test.go 品牌域选号隔离（2026-09-28 泛化修复：trae 账号
// Realm() 归一为 cn，此前会被裸/cn 请求误选，打到 codebuddy 端点必 401）。
package pool

import (
	"testing"

	"github.com/linguo2625469/l3api/internal/auth"
)

// mkTraeAuth 与 brand_pick_test.go 的 mkBrandAuth 同模式（品牌字段落盘后 Brand() 生效）。
func mkTraeAuth(t *testing.T, uid, brand string) *auth.Auth {
	t.Helper()
	return mkBrandAuth(t, uid, brand)
}

// TestPickBrandIsolationTrae trae 账号不得被裸/cn/global 请求选中，
// 品牌域必须且只能选中本品牌账号。
func TestPickBrandIsolationTrae(t *testing.T) {
	p := New("")
	p.Add(mkTraeAuth(t, "cb-1", "codebuddy"))
	p.Add(mkTraeAuth(t, "tr-1", "trae"))
	p.Add(mkTraeAuth(t, "qd-1", "qoder"))

	// 裸请求/cn：只能选 codebuddy 账号（trae/qoder 被隔离）。
	for _, realm := range []string{"", "cn"} {
		for i := 0; i < 10; i++ {
			a := p.PickExcludingForBrand(nil, "", realm)
			if a == nil || a.UID != "cb-1" {
				t.Fatalf("realm=%q pick = %v, want cb-1", realm, a)
			}
		}
	}
	// 品牌域：只选本品牌。
	if a := p.PickExcludingForBrand(nil, "", "trae"); a == nil || a.UID != "tr-1" {
		t.Fatalf("trae pick = %v, want tr-1", a)
	}
	if a := p.PickExcludingForBrand(nil, "", "qoder"); a == nil || a.UID != "qd-1" {
		t.Fatalf("qoder pick = %v, want qd-1", a)
	}
	// 品牌域兜底：tried 排除唯一候选后返回 nil，不跨品牌捞。
	if a := p.PickExcludingForBrand(map[string]bool{"tr-1": true}, "", "trae"); a != nil {
		t.Fatalf("trae pick with tried = %v, want nil", a.UID)
	}
}

// TestAvailableUIDsBrandRealm 粘性可用集合的品牌口径：品牌域只含本品牌账号，
// cn 集合不混入 trae（修复前 cn 集合含 trae → 粘性绑到它必 401）。
func TestAvailableUIDsBrandRealm(t *testing.T) {
	p := New("")
	p.Add(mkTraeAuth(t, "cb-1", "codebuddy"))
	p.Add(mkTraeAuth(t, "tr-1", "trae"))

	if got := p.AvailableUIDsForModelRealm("m", "trae"); len(got) != 1 || got[0] != "tr-1" {
		t.Fatalf("trae available = %v, want [tr-1]", got)
	}
	if got := p.AvailableUIDsForModelRealm("m", "cn"); len(got) != 1 || got[0] != "cb-1" {
		t.Fatalf("cn available = %v, want [cb-1]", got)
	}
	if got := p.AvailableUIDsForRealm("trae"); len(got) != 1 || got[0] != "tr-1" {
		t.Fatalf("trae realm available = %v, want [tr-1]", got)
	}
	// 全集（realm 空）不受品牌谓词影响（现状语义）。
	if got := p.AvailableUIDsForRealm(""); len(got) != 2 {
		t.Fatalf("empty realm available = %v, want 2", got)
	}
}

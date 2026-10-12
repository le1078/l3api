package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

// mkAuth 构造一个品牌可指定的测试账号（credits 充足、健康）。
func mkBrandAuth(t *testing.T, uid, brand string) *auth.Auth {
	t.Helper()
	a := &auth.Auth{
		UID:          uid,
		AccessToken:  "tok-" + uid,
		RefreshToken: "rt-" + uid,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	if _, err := auth.BackfillBrandFor(a, brand); err != nil {
		t.Fatalf("brand: %v", err)
	}
	return a
}

// TestPickBrandIsolation Qoder 品牌与 codebuddy 池严格隔离：
//   - realm=="qoder" 只选 qoder 账号；
//   - 裸请求（realm 空/cn）不选 qoder 账号（Qoder 无 codebuddy 计费通道）。
func TestPickBrandIsolation(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(mkBrandAuth(t, "cb-1", "codebuddy"))
	p.Add(mkBrandAuth(t, "qd-1", "qoder"))

	// realm=qoder：必须选中 qoder 账号。
	a := p.PickExcludingForBrand(nil, "", "qoder")
	if a == nil || a.UID != "qd-1" {
		t.Fatalf("qoder pick = %v, want qd-1", a)
	}
	// 裸请求（realm 空）：不得选中 qoder 账号（即便它是唯一非 codebuddy 账号）。
	for i := 0; i < 20; i++ {
		a := p.PickExcludingForBrand(nil, "", "")
		if a == nil || a.UID != "cb-1" {
			t.Fatalf("bare pick = %v, want cb-1 (iteration %d)", a, i)
		}
	}
	// cn realm：同上（Qoder 账号 Realm()=cn，但品牌谓词隔离）。
	for i := 0; i < 20; i++ {
		a := p.PickExcludingForBrand(nil, "", "cn")
		if a == nil || a.UID != "cb-1" {
			t.Fatalf("cn pick = %v, want cb-1 (iteration %d)", a, i)
		}
	}
}

// TestPickBrandOnlyQoder 池内只有 qoder 账号时，裸请求返回 nil（不跨品牌兜底）。
func TestPickBrandOnlyQoder(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(mkBrandAuth(t, "qd-1", "qoder"))
	if a := p.PickExcludingForBrand(nil, "", ""); a != nil {
		t.Fatalf("bare pick with only qoder = %v, want nil", a)
	}
	if a := p.PickExcludingForBrand(nil, "", "cn"); a != nil {
		t.Fatalf("cn pick with only qoder = %v, want nil", a)
	}
	// qoder realm 正常选。
	if a := p.PickExcludingForBrand(nil, "", "qoder"); a == nil {
		t.Fatal("qoder pick should hit the only qoder account")
	}
}

// TestHasBrand HasBrand 报告品牌存在性。
func TestHasBrand(t *testing.T) {
	p := New("")
	defer p.Close()
	if p.HasBrand("qoder") {
		t.Fatal("empty pool should have no qoder")
	}
	p.Add(mkBrandAuth(t, "qd-1", "qoder"))
	if !p.HasBrand("qoder") {
		t.Fatal("pool should have qoder")
	}
	if p.HasBrand("trae") {
		t.Fatal("pool should not have trae")
	}
}

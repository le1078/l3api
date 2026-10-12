// credit_floor_test.go 积分保底（pool.credit_floor）选号语义测试。
// 语义对照上游 workbuddy2api-panel credit_floor_test.go 五用例。
package pool

import (
	"testing"

	"github.com/linguo2625469/l3api/internal/auth"
)

// floorFixture 三账号池：paid 号对模型 m 有实测收费记录，free 号实测免费，
// unknown 号无观测。floor=100，paid 号余额压到线下。
func floorFixture(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	for _, uid := range []string{"paid", "free", "unknown"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
	}
	// 成本台账：paid 号对 m 实测收费，free 号实测免费（NoteModelCost 口径：
	// credit>0 → CostPer1k>0；credit==0 → 免费）。
	p.NoteModelCost("paid", "m", 5, 1000)
	p.NoteModelCost("free", "m", 0, 1000)
	p.SetCreditFloor(100)
	// paid 号余额压到线下（SetCredits 语义：剩余积分）。
	p.SetCredits("paid", 50, 1000)
	p.SetCredits("free", 50, 1000)
	p.SetCredits("unknown", 50, 1000)
	return p
}

func TestCreditFloorBlocksPaidBelowFloor(t *testing.T) {
	p := floorFixture(t)
	// paid 号对收费模型 m 被拦：选号结果永不出现 paid。
	for i := 0; i < 30; i++ {
		a := p.PickExcludingForModel(nil, "m")
		if a == nil {
			t.Fatal("pool not empty, pick should succeed (free/unknown)")
		}
		if a.UID == "paid" {
			t.Fatal("below-floor paid account picked for paid model")
		}
	}
}

func TestCreditFloorAllowsFreeBelowFloor(t *testing.T) {
	p := floorFixture(t)
	// 免费模型（free 号实测免费，但 paid 号对 m0 无观测）：free 号应能被选中。
	// 模型 m 对 free 号免费 → free 号不受 floor 影响，30 次内至少出现一次。
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		a := p.PickExcludingForModel(nil, "m")
		if a != nil {
			seen[a.UID] = true
		}
	}
	if !seen["free"] {
		t.Fatal("free account never picked for free model")
	}
}

func TestCreditFloorDisabledByDefault(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "paid", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteModelCost("paid", "m", 5, 1000)
	p.SetCredits("paid", 0, 1000)
	// floor 未开（0）：paid 号照常可选。
	if a := p.PickExcludingForModel(nil, "m"); a == nil || a.UID != "paid" {
		t.Fatal("floor disabled should not block")
	}
}

func TestCreditFloorAllBelowReturnsNil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "b", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteModelCost("a", "m", 5, 1000)
	p.NoteModelCost("b", "m", 5, 1000)
	p.SetCredits("a", 1, 1000)
	p.SetCredits("b", 1, 1000)
	p.SetCreditFloor(100)
	if a := p.PickExcludingForModel(nil, "m"); a != nil {
		t.Fatalf("all below floor + all paid → want nil, got %s", a.UID)
	}
}

func TestCreditFloorRecoversAfterCheckin(t *testing.T) {
	// 两号同对模型 m 实测收费（同 tier 2，排除免费层硬过滤干扰）：
	// 回血前 paid 被 floor 拦，回血后恢复可选。
	p := New("")
	p.Add(&auth.Auth{UID: "paid", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "other", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteModelCost("paid", "m", 5, 1000)
	p.NoteModelCost("other", "m", 8, 1000)
	p.SetCreditFloor(100)
	p.SetCredits("paid", 50, 1000)
	p.SetCredits("other", 50, 1000)
	for i := 0; i < 30; i++ {
		if a := p.PickExcludingForModel(nil, "m"); a != nil && a.UID == "paid" {
			t.Fatal("below-floor paid account picked")
		}
	}
	// 签到回血：余额回到线上 → 恢复可选。
	p.SetCredits("paid", 500, 1000)
	p.SetCredits("other", 500, 1000)
	seen := false
	for i := 0; i < 60; i++ {
		if a := p.PickExcludingForModel(nil, "m"); a != nil && a.UID == "paid" {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatal("recovered account should be pickable again")
	}
}

func TestCreditFloorStickyBlocked(t *testing.T) {
	p := floorFixture(t)
	// 粘性路径：PickByUIDForModel 对触底+收费模型返回 nil（handler 解绑换号）。
	if a := p.PickByUIDForModel("paid", "m"); a != nil {
		t.Fatal("sticky pick should be floor-blocked")
	}
	// 免费模型不受限：free 号粘性照常。
	if a := p.PickByUIDForModel("free", "m"); a == nil {
		t.Fatal("sticky pick for free account should pass")
	}
}

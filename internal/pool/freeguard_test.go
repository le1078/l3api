// freeguard_test.go 免费额度守护（free_guard）台账与守护选号语义测试。
package pool

import (
	"os"
	"testing"
	"time"

	"github.com/linguo2625469/l3api/internal/auth"
)

// guardFixture 三账号池：exhausted 号对模型 m 已标记免费额度尽（复位点未来），
// other 号未标记。返回池与复位点。
func guardFixture(t *testing.T, until time.Time) *Pool {
	t.Helper()
	p := New("")
	for _, uid := range []string{"exhausted", "other"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
	}
	p.MarkFreeExhausted("exhausted", "m", until)
	return p
}

func TestFreeGuardPickExcludesExhausted(t *testing.T) {
	p := guardFixture(t, time.Now().Add(time.Hour))
	// 守护选号：30 次内永不出现 exhausted 号。
	for i := 0; i < 30; i++ {
		a := p.PickExcludingForBrandGuarded(nil, "m", "")
		if a == nil {
			t.Fatal("other account should be pickable")
		}
		if a.UID == "exhausted" {
			t.Fatal("free-exhausted account picked under guard")
		}
	}
}

func TestFreeGuardNormalPickUnaffected(t *testing.T) {
	p := guardFixture(t, time.Now().Add(time.Hour))
	// 非守护选号（普通 key）：exhausted 号照常可选（守护只影响 Guarded 变体）。
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		a := p.PickExcludingForBrand(nil, "m", "")
		if a != nil {
			seen[a.UID] = true
		}
	}
	if !seen["exhausted"] {
		t.Fatal("non-guarded pick should still see the exhausted account")
	}
}

func TestFreeGuardExpiry(t *testing.T) {
	// 复位点已过 → 条目过期，守护选号重新放行（配额按窗/按日重置）。
	p := guardFixture(t, time.Now().Add(-time.Minute))
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		a := p.PickExcludingForBrandGuarded(nil, "m", "")
		if a != nil {
			seen[a.UID] = true
		}
	}
	if !seen["exhausted"] {
		t.Fatal("expired free-exhaust entry should be pickable again")
	}
}

func TestFreeGuardIdempotentMark(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	until1 := time.Now().Add(time.Hour)
	until2 := time.Now().Add(2 * time.Hour)
	p.MarkFreeExhausted("u1", "m", until1)
	p.MarkFreeExhausted("u1", "m", until2) // 幂等：不覆盖已有未过期复位点
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if len(st.FreeExhaustedModels) != 1 {
		t.Fatalf("want 1 free-exhausted row, got %d", len(st.FreeExhaustedModels))
	}
	if !st.FreeExhaustedModels[0].Until.Equal(until1) {
		t.Fatalf("idempotent mark overwrote reset point: got %v want %v",
			st.FreeExhaustedModels[0].Until, until1)
	}
}

func TestFreeGuardCountAndSticky(t *testing.T) {
	p := guardFixture(t, time.Now().Add(time.Hour))
	if got := p.FreeExhaustedCount("", "m"); got != 1 {
		t.Fatalf("FreeExhaustedCount = %d, want 1", got)
	}
	if got := p.FreeExhaustedCount("", "other-model"); got != 0 {
		t.Fatalf("FreeExhaustedCount other-model = %d, want 0", got)
	}
	if !p.FreeExhausted("exhausted", "m") {
		t.Fatal("FreeExhausted exhausted/m should be true")
	}
	if p.FreeExhausted("other", "m") {
		t.Fatal("FreeExhausted other/m should be false")
	}
	// 粘性选号本身不拦（拦截在 handler 层判 FreeExhausted），此处只验证状态口径。
}

func TestFreeGuardPersistRoundTrip(t *testing.T) {
	fp := t.TempDir() + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	until := time.Now().Add(time.Hour).Truncate(time.Second) // JSON 往返精度截断
	p.MarkFreeExhausted("u1", "m", until)
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	if !p2.FreeExhausted("u1", "m") {
		t.Fatal("free-exhaust ledger lost across reload")
	}
	st, _ := p2.Status("u1")
	if len(st.FreeExhaustedModels) != 1 || !st.FreeExhaustedModels[0].Until.Equal(until) {
		t.Fatalf("reset point mismatch after reload: %+v", st.FreeExhaustedModels)
	}

	// 过期条目不复活：手写一份含过期台账的 state.json（正常落盘路径会过滤
	// 过期条目，这里模拟时钟回拨/旧文件残留场景，验证恢复侧过滤）。
	fp3 := t.TempDir() + "/state.json"
	raw := `{"accounts":{"u2":{"credits":0,"cool_kind":0,"soft_streak":0,"session_dead_fails":0,"consecutive_fails":0,"credits_expiring":0,"free_exhausted":{"m":"2000-01-01T00:00:00+08:00"}}}}`
	if err := os.WriteFile(fp3, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	p4 := New(fp3)
	p4.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999})
	if p4.FreeExhausted("u2", "m") {
		t.Fatal("expired free-exhaust entry should not be restored")
	}
}

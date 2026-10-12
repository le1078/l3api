// paused_test.go 暂停选号语义：退出选号但保号任务照常、持久化、恢复可选。
package pool

import (
	"testing"

	"github.com/linguo2625469/l3api/internal/auth"
)

func TestPausedExcludedFromPick(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "b", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetPaused("a", true)
	for i := 0; i < 20; i++ {
		if got := p.Pick(); got != nil && got.UID == "a" {
			t.Fatal("paused account picked")
		}
	}
	// 粘性/直取路径同样拦
	if p.PickByUIDForModel("a", "m") != nil {
		t.Fatal("paused account returned by PickByUIDForModel")
	}
}

func TestPausedPersistedAndRestored(t *testing.T) {
	dir := t.TempDir()
	p := New(dir + "/state.json")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetPaused("a", true)
	p.Flush()

	p2 := New(dir + "/state.json")
	st, ok := p2.Status("a")
	if !ok || !st.Paused {
		t.Fatalf("paused not persisted: %+v ok=%v", st, ok)
	}
}

func TestPausedCountsAsDisabledInCounts(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "b", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetPaused("a", true)
	total, healthy, _, disabled, _ := p.CountsDetailed()
	if total != 2 || healthy != 1 || disabled != 1 {
		t.Fatalf("counts = %d/%d/disabled=%d, want 2/1/1", total, healthy, disabled)
	}
}

func TestUnpauseRestoresPickable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetPaused("a", true)
	p.SetPaused("a", false)
	if got := p.Pick(); got == nil || got.UID != "a" {
		t.Fatal("unpaused account should be pickable")
	}
}

func TestPauseUnknownUID(t *testing.T) {
	p := New("")
	if p.SetPaused("nope", true) {
		t.Fatal("unknown uid should return false")
	}
}

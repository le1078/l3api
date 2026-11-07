package scheduler

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/l3api/internal/usage"
)

// newDeltaTestScheduler 起一个带临时 usage.json 的调度器（只测差值记账，不跑排程）。
func newDeltaTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	return New(Config{Usage: usage.New(filepath.Join(t.TempDir(), "usage.json"))})
}

// todayCredits 今日累计积分（与面板「今日消耗积分」同口径）。
func todayCredits(t *testing.T, s *Scheduler) float64 {
	t.Helper()
	_, _, cr := s.cfg.Usage.TodaySummary()
	return cr
}

// TestObserveCreditDeltaBaseline 首次观测只立基线，不记账。
func TestObserveCreditDeltaBaseline(t *testing.T) {
	s := newDeltaTestScheduler(t)
	s.observeCreditDelta("uid-tr", "trae", 100)
	if got := todayCredits(t, s); got != 0 {
		t.Fatalf("首次观测不应记账，今日积分 = %v", got)
	}
}

// TestObserveCreditDeltaAccumulates 正差值入账：第二轮差 30、第三轮差 12.5。
func TestObserveCreditDeltaAccumulates(t *testing.T) {
	s := newDeltaTestScheduler(t)
	s.observeCreditDelta("uid-tr", "trae", 100)
	s.observeCreditDelta("uid-tr", "trae", 130)
	if got := todayCredits(t, s); got != 30 {
		t.Fatalf("第二次观测差值 30，今日积分 = %v", got)
	}
	s.observeCreditDelta("uid-tr", "trae", 142)
	if got := todayCredits(t, s); got != 42 {
		t.Fatalf("累计差值 42，今日积分 = %v", got)
	}
}

// TestObserveCreditDeltaReset 负差值（上游周期重置）只重立基线不记账；
// 之后的正差值从新基线起算。
func TestObserveCreditDeltaReset(t *testing.T) {
	s := newDeltaTestScheduler(t)
	s.observeCreditDelta("uid-qd", "qoder", 500)
	s.observeCreditDelta("uid-qd", "qoder", 560)
	if got := todayCredits(t, s); got != 60 {
		t.Fatalf("重置前应记 60，今日积分 = %v", got)
	}
	s.observeCreditDelta("uid-qd", "qoder", 10) // 周期重置：读数回退
	if got := todayCredits(t, s); got != 60 {
		t.Fatalf("负差值不应记账，今日积分 = %v", got)
	}
	s.observeCreditDelta("uid-qd", "qoder", 25)
	if got := todayCredits(t, s); got != 75 {
		t.Fatalf("新基线起算应记 60+15=75，今日积分 = %v", got)
	}
}

// TestObserveCreditDeltaZeroDelta 相同读数（无消耗）不记账。
func TestObserveCreditDeltaZeroDelta(t *testing.T) {
	s := newDeltaTestScheduler(t)
	s.observeCreditDelta("uid-tr", "trae", 100)
	s.observeCreditDelta("uid-tr", "trae", 100)
	if got := todayCredits(t, s); got != 0 {
		t.Fatalf("零差值不应记账，今日积分 = %v", got)
	}
}

// TestObserveCreditDeltaMultiAccount 多账号基线互不干扰。
func TestObserveCreditDeltaMultiAccount(t *testing.T) {
	s := newDeltaTestScheduler(t)
	s.observeCreditDelta("uid-a", "trae", 100)
	s.observeCreditDelta("uid-b", "qoder", 200)
	s.observeCreditDelta("uid-a", "trae", 150)
	s.observeCreditDelta("uid-b", "qoder", 220)
	if got := todayCredits(t, s); got != 70 {
		t.Fatalf("两号合计差值 70，今日积分 = %v", got)
	}
}

// TestObserveCreditDeltaNilUsage 未装配 Usage 记录器时不 panic（余额刷新照常）。
func TestObserveCreditDeltaNilUsage(t *testing.T) {
	s := New(Config{})
	s.observeCreditDelta("uid-tr", "trae", 100)
}

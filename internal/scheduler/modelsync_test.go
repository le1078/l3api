// modelsync_test.go 模型目录每日同步的排程单测。
package scheduler

import (
	"testing"
	"time"
)

func TestModelSyncFireToday(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, scheduleTZ)
	fire := modelSyncFireToday(now, "04:05")
	if fire.Hour() != 4 || fire.Minute() != 5 || fire.Location() != scheduleTZ {
		t.Fatalf("fire = %v", fire)
	}
	// 非法时点回落默认 03:30
	fire = modelSyncFireToday(now, "bogus")
	if fire.Hour() != 3 || fire.Minute() != 30 {
		t.Fatalf("invalid tod should fall back to 03:30, got %v", fire)
	}
	// 空串回落默认
	fire = modelSyncFireToday(now, "")
	if fire.Hour() != 3 || fire.Minute() != 30 {
		t.Fatalf("empty tod should fall back to 03:30, got %v", fire)
	}
}

func TestSetModelSyncHotUpdate(t *testing.T) {
	s := New(Config{ModelSyncTime: "03:30"})
	if s.cfg.ModelSyncTime != "03:30" {
		t.Fatalf("default tod: %q", s.cfg.ModelSyncTime)
	}
	s.SetModelSync(true, "23:45")
	s.schedMu.Lock()
	dis, tod := s.cfg.ModelSyncDisabled, s.cfg.ModelSyncTime
	s.schedMu.Unlock()
	if !dis || tod != "23:45" {
		t.Fatalf("after SetModelSync: disabled=%v tod=%q", dis, tod)
	}
	// 空 tod 不覆盖现值
	s.SetModelSync(false, "")
	s.schedMu.Lock()
	tod = s.cfg.ModelSyncTime
	s.schedMu.Unlock()
	if tod != "23:45" {
		t.Fatalf("empty tod should keep current, got %q", tod)
	}
}

func TestRunModelSyncNowNilDeps(t *testing.T) {
	// 无 Pool/Upstream 注入时安全返回（不 panic）——防御性语义锁定。
	s := New(Config{})
	s.RunModelSyncNow(nil)
}

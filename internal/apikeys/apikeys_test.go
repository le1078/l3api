// apikeys_test.go 注册表判定/白名单/限流窗口的单测。
package apikeys

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestVerifyHitMissDisabled(t *testing.T) {
	reg := NewRegistry([]Entry{
		{Name: "default", Key: "sk-master"},
		{Name: "alice", Key: "sk-alice"},
		{Name: "bob", Key: "sk-bob", Disabled: true},
	})
	if e, ok := reg.Verify("sk-alice"); !ok || e.Name != "alice" {
		t.Fatalf("alice: ok=%v entry=%+v", ok, e)
	}
	if _, ok := reg.Verify("sk-wrong"); ok {
		t.Fatal("wrong key must not verify")
	}
	if _, ok := reg.Verify(""); ok {
		t.Fatal("empty token must not verify")
	}
	// 禁用条目：密钥正确也拒（401 语义，与"没这把 key"不可区分）。
	if _, ok := reg.Verify("sk-bob"); ok {
		t.Fatal("disabled key must not verify")
	}
	// 老语义：空注册表 = 不该走到这里（main 保证 default 条目）；
	// 但 Verify 必须安全返回 false 而不是 panic。
	if _, ok := NewRegistry(nil).Verify("x"); ok {
		t.Fatal("empty registry must reject")
	}
}

func TestAllowsModel(t *testing.T) {
	cases := []struct {
		e      Entry
		models []string
		want   bool
	}{
		{Entry{}, []string{"any"}, true},                      // 空白名单 = 不限
		{Entry{Models: []string{"*"}}, []string{"any"}, true}, // 显式全放行
		{Entry{Models: []string{"cn:glm-5.2"}}, []string{"cn:glm-5.2"}, true},
		{Entry{Models: []string{"cn:glm-5.2"}}, []string{"glm-5.2"}, true},    // 裸名命中
		{Entry{Models: []string{"glm-5.2"}}, []string{"cn:glm-5.2"}, true},    // 原文命中
		{Entry{Models: []string{"CN:GLM-5.2"}}, []string{"cn:glm-5.2"}, true}, // 大小写不敏感
		{Entry{Models: []string{"cn:glm-5.2"}}, []string{"cn:qwen"}, false},
		{Entry{Models: []string{"cn:glm-5.2", "qoder:x"}}, []string{"qoder:x", "glm-5.2"}, true},
	}
	for i, c := range cases {
		if got := c.e.AllowsModel(c.models...); got != c.want {
			t.Errorf("case %d: got=%v want=%v (entry=%+v models=%v)", i, got, c.want, c.e, c.models)
		}
	}
}

func TestAllowFixedWindow(t *testing.T) {
	reg := NewRegistry(nil)
	for i := 0; i < 3; i++ {
		if ok, _ := reg.Allow("a", 3); !ok {
			t.Fatalf("req %d within limit must pass", i)
		}
	}
	ok, retry := reg.Allow("a", 3)
	if ok || retry <= 0 {
		t.Fatalf("4th request must be limited with retryAfter>0 (ok=%v retry=%v)", ok, retry)
	}
	// 其他 name 独立计数。
	if ok, _ := reg.Allow("b", 3); !ok {
		t.Fatal("different name must have independent window")
	}
	// limit<=0 不限。
	if ok, _ := reg.Allow("a", 0); !ok {
		t.Fatal("limit 0 = unlimited")
	}
}

func TestStoreKeepsWindows(t *testing.T) {
	reg := NewRegistry([]Entry{{Name: "a", Key: "k1"}})
	_, _ = reg.Allow("a", 2)
	// 热更新：换条目，窗口状态按 name 保留。
	reg.Store([]Entry{{Name: "a", Key: "k2", RPM: 2}})
	if _, ok := reg.Verify("k1"); ok {
		t.Fatal("old key must be revoked after Store")
	}
	if _, ok := reg.Verify("k2"); !ok {
		t.Fatal("new key must verify after Store")
	}
	// 计数延续：Store 前已用 1 次，限额 2 → 还能 1 次。
	if ok, _ := reg.Allow("a", 2); !ok {
		t.Fatal("window state must survive Store")
	}
	if ok, _ := reg.Allow("a", 2); ok {
		t.Fatal("window limit must still apply after Store")
	}
}

func TestMask(t *testing.T) {
	if got := (Entry{Key: "sk-0123456789abcdef"}).Mask(); got != "sk-012…cdef" {
		t.Errorf("long key mask: %q", got)
	}
	if got := (Entry{Key: "short"}).Mask(); got != "*****" {
		t.Errorf("short key mask: %q", got)
	}
}

func TestConcurrentVerifyAllow(t *testing.T) {
	reg := NewRegistry([]Entry{{Name: "a", Key: "k", RPM: 1000}})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = reg.Verify("k")
			_, _ = reg.Allow("a", 1000)
			reg.Store([]Entry{{Name: "a", Key: "k", RPM: 1000}})
		}()
	}
	wg.Wait()
}

func TestContextCarry(t *testing.T) {
	e := Entry{Name: "alice"}
	ctx := WithEntry(context.Background(), e)
	got, ok := FromContext(ctx)
	if !ok || got.Name != "alice" {
		t.Fatalf("got=%+v ok=%v", got, ok)
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("bare context must not carry entry")
	}
	_ = time.Now
}

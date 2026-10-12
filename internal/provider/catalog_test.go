// catalog_test.go 品牌模型目录注册表：注册/覆盖/枚举稳定 + 严格归一化。
package provider

import "testing"

func TestRegisterChatModels(t *testing.T) {
	t.Cleanup(func() {
		catalogMu.Lock()
		delete(catalog, "test-brand-a")
		catalogMu.Unlock()
	})
	RegisterChatModels(Brand("test-brand-a"), []ModelEntry{{ID: "m1", DisplayName: "M1"}})
	if got := ChatModels("test-brand-a"); len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("got %v", got)
	}
	// 重复注册覆盖（后者为准）。
	RegisterChatModels(Brand("test-brand-a"), []ModelEntry{{ID: "m2"}, {ID: "m3"}})
	if got := ChatModels("test-brand-a"); len(got) != 2 || got[0].ID != "m2" {
		t.Fatalf("overwrite: %v", got)
	}
	// 未注册品牌返回 nil。
	if ChatModels("nonexistent") != nil {
		t.Fatal("unregistered brand must return nil")
	}
	// ChatBrands 枚举包含注册项且输出稳定（字母序）。
	brands := ChatBrands()
	sorted := true
	for i := 1; i < len(brands); i++ {
		if brands[i-1] > brands[i] {
			sorted = false
		}
	}
	if !sorted {
		t.Fatalf("brands not sorted: %v", brands)
	}
}

func TestNormalizeStrict(t *testing.T) {
	if b, ok := NormalizeStrict("Qoder"); !ok || b != Qoder {
		t.Fatalf("qoder: %v %v", b, ok)
	}
	if _, ok := NormalizeStrict("bogus"); ok {
		t.Fatal("bogus must not be ok")
	}
	if _, ok := NormalizeStrict(""); ok {
		t.Fatal("empty must not be ok")
	}
	// 与容错口径的一致性：Normalize 对合法值同结果。
	for _, s := range []string{"codebuddy", "qoder", "trae"} {
		b, ok := NormalizeStrict(s)
		if !ok || b != Normalize(s) {
			t.Fatalf("divergence for %q: strict=(%v,%v) lenient=%v", s, b, ok, Normalize(s))
		}
	}
}

package usage

import (
	"testing"
	"time"
)

func TestSnapshotCreditByKeyAndModel(t *testing.T) {
	r := New(t.TempDir() + "/usage.json")
	r.Start()
	defer r.Stop()
	now := time.Now()
	// 两个 key、两个模型、不同积分
	r.AddKey(now, "cn", "u1", "glm-5.2", "default", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, TotalTokens: 150, HasTotal: true}, true)
	r.AddCredit(now, "cn", "u1", "glm-5.2", "default", 0.37)
	r.AddKey(now, "cn", "u1", "kimi-k3", "guardtest", Delta{PromptTokens: 200, HasPromptTokens: true, CompletionTokens: 100, HasCompletion: true, TotalTokens: 300, HasTotal: true}, true)
	r.AddCredit(now, "cn", "u1", "kimi-k3", "guardtest", 1.2)
	r.AddKey(now, "qoder", "u2", "qfmodel", "default", Delta{PromptTokens: 50, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, TotalTokens: 100, HasTotal: true}, true)
	r.AddCredit(now, "qoder", "u2", "qfmodel", "default", 0.05)

	snap := r.Snapshot(24, nil, nil)
	if len(snap.ByCreditKey) != 2 {
		t.Fatalf("by_credit_key=%d want 2: %+v", len(snap.ByCreditKey), snap.ByCreditKey)
	}
	if len(snap.ByCreditModel) != 3 {
		t.Fatalf("by_credit_model=%d want 3: %+v", len(snap.ByCreditModel), snap.ByCreditModel)
	}
	byKey := map[string]float64{}
	for _, k := range snap.ByCreditKey {
		byKey[k.Key] = k.Credits
	}
	if byKey["default"] < 0.42 || byKey["guardtest"] < 1.2 {
		t.Fatalf("credits by key wrong: %v", byKey)
	}
	byModel := map[string]float64{}
	for _, k := range snap.ByCreditModel {
		byModel[k.Key] = k.Credits
	}
	if byModel["kimi-k3"] < 1.2 || byModel["qfmodel"] < 0.05 || byModel["glm-5.2"] < 0.37 {
		t.Fatalf("credits by model wrong: %v", byModel)
	}
	// token 也应在同一行里（同一 KeyedAgg）
	for _, k := range snap.ByCreditModel {
		if k.Key == "kimi-k3" && k.TotalTokens != 300 {
			t.Fatalf("kimi-k3 tokens=%d", k.TotalTokens)
		}
	}
}

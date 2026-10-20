package upstream

import (
	"encoding/json"
	"testing"
)

func TestParseQoderPromoAndContext(t *testing.T) {
	raw := []byte(`{"chat":[
		{"key":"qfmodel","display_name":"Qwen3.8-Flash","is_vl":true,"is_reasoning":true,
		 "max_input_tokens":180000,"enable":true,"price_factor":0.0,"original_price_factor":0.1,"is_free":true,
		 "context_config":{"200K":{"token_count":200000,"is_default":true},"400K":{"token_count":400000},"1M":{"token_count":1000000}},
		 "thinking_config":{"disabled":{},"enabled":{"is_default":true,"efforts":{"low":{},"medium":{"is_default":true},"xhigh":{}}}}},
		{"key":"qmodel","display_name":"Qwen3.7-Plus","is_vl":true,"is_reasoning":true,
		 "max_input_tokens":180000,"enable":true,"price_factor":0.04,
		 "context_config":{"200K":{"token_count":200000,"is_default":true}},
		 "promotion":{"active":true,"badge":{"zh":"错峰4折"},"description":{"zh":"错峰时段4折优惠（10 PM-8 AM UTC+8）"},
		 "timezone":"Asia/Shanghai","discount_factor":0.4,"before_promotion_price_factor":0.1,
		 "window_start":"22:00","window_end":"08:00"},
		 "thinking_config":{"disabled":{},"enabled":{"is_default":true}}}
	]}`)
	ms, err := parseQoderModelList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("got %d", len(ms))
	}
	f := ms[0]
	if !f.IsFree || f.Credits != "x0.00" || f.OriginalCredits != "x0.10" {
		t.Fatalf("free: %+v", f)
	}
	if f.MaxInputTokens != 1000000 || len(f.ContextTiers) != 3 {
		t.Fatalf("context: in=%d tiers=%v", f.MaxInputTokens, f.ContextTiers)
	}
	if f.MaxOutputTokens != 250000 {
		t.Fatalf("max_out=%d", f.MaxOutputTokens)
	}
	p := ms[1]
	if p.Badge != "错峰4折" || p.PromotionWindow != "22:00-08:00" || p.PromotionFactor != 0.4 {
		t.Fatalf("promo: %+v", p)
	}
	if p.OriginalCredits != "x0.10" || p.PromotionCredit != "x0.04" {
		t.Fatalf("promo credits: %+v", p)
	}
}

func TestParseTraePromoAndContext(t *testing.T) {
	list := []map[string]any{
		{
			"config_name": "deepseek-v4.1-flash", "config_source": 1, "config_switch": true,
			"context_window_tokens": map[string]any{"dev": 116000, "max": 1000000},
			"display_config": map[string]any{"display_name": "DS", "model_capability": "reasoning_model", "multimodal": true},
			"model_detail_list": []map[string]any{{"max_tokens": 16000}},
			"display_contact_config": `{"consumption_rate":{"enable":true,"data":{"rate":0.06}},
				"activity_discount":{"enable":true,"subKey":"off_peak","data":{
					"current":{"discount_type":"off_peak","before_consumption_rate":0.12,"consumption_rate":0.06,"discount":50},
					"off_peak":{"before_consumption_rate":0.12,"after_consumption_rate":0.06,"discount":50,
						"time_windows":[{"weekdays":[1,2,3,4,5],"start_minute":0,"end_minute":540}],
						"time_zone":"Asia/Shanghai"}}}}`,
			"reasoning_effort_config": map[string]any{"default_level": "high", "options": []string{"light", "high", "extra_high"}, "support_thinking": true},
		},
	}
	raw, _ := json.Marshal(map[string]any{"config_info_list": list})
	res, err := parseTraeDetailParam(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Visible) != 1 {
		t.Fatalf("got %d", len(res.Visible))
	}
	m := res.Visible[0]
	if m.MaxTokens != 1000000 || len(m.ContextTiers) != 2 {
		t.Fatalf("context: %d %v", m.MaxTokens, m.ContextTiers)
	}
	if m.MaxOutputTokens != 16000 {
		t.Fatalf("max_out=%d", m.MaxOutputTokens)
	}
	if m.Badge != "错峰 50% off" || m.OriginalCredits != "x0.12" {
		t.Fatalf("promo: %+v", m)
	}
	if m.PromotionWindow == "" || m.PromotionTZ != "Asia/Shanghai" {
		t.Fatalf("window/tz: %+v", m)
	}
}

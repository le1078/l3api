// modelsync_test.go 模型目录同步单测：解析过滤口径、diff 三清单、
// 覆盖文件落盘/读取/损坏降级、Qoder 整体替换语义。
package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseQoderModelList(t *testing.T) {
	raw := []byte(`{"chat":[
		{"key":"auto","display_name":"Auto","is_vl":true,"is_reasoning":true,"max_input_tokens":200000,"enable":true,"price_factor":0.5},
		{"key":"gmodel","display_name":"GLM-5.3","is_vl":true,"is_reasoning":true,"max_input_tokens":180000,"enable":true,"price_factor":0.2,
		 "thinking_config":{"enabled":{"is_default":true,"efforts":{"low":{},"high":{},"max":{"is_default":true}}}}},
		{"key":"off","display_name":"Off","is_vl":false,"is_reasoning":false,"max_input_tokens":1000,"enable":false}
	],"developer":[{"key":"x"}]}`)
	ms, err := parseQoderModelList(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ms) != 2 || ms[0].ID != "auto" || ms[1].ID != "gmodel" {
		t.Fatalf("got %+v", ms)
	}
	if ms[0].MaxInputTokens != 200000 || !ms[0].IsReasoning {
		t.Fatalf("fields: %+v", ms[0])
	}
	// 五列能力字段（2026-10-21）：price_factor→credits、thinking_config→efforts、
	// max_output 按窗口 1/4 估算。
	if ms[1].Credits != "x0.20" {
		t.Fatalf("gmodel credits=%q want x0.20", ms[1].Credits)
	}
	if ms[1].MaxOutputTokens != 45000 {
		t.Fatalf("gmodel max_output=%d want 45000", ms[1].MaxOutputTokens)
	}
	if ms[1].DefaultEffort != "max" || len(ms[1].Efforts) != 3 {
		t.Fatalf("gmodel efforts=%v def=%q", ms[1].Efforts, ms[1].DefaultEffort)
	}
	if got := ms[0].MaxOutputTokens; got != 50000 {
		t.Fatalf("auto max_output=%d want 50000", got)
	}
	// 空 chat 组 = 解析失败（不落盘空目录）
	if _, err := parseQoderModelList([]byte(`{"chat":[]}`)); err == nil {
		t.Fatal("empty chat group should error")
	}
	if _, err := parseQoderModelList([]byte(`not json`)); err == nil {
		t.Fatal("bad json should error")
	}
}

func TestParseTraeDetailParam(t *testing.T) {
	mk := func(name string, src int, sw bool, dispName, cap string, mm bool, dev int, effort bool) map[string]any {
		m := map[string]any{
			"config_name":           name,
			"config_source":         src,
			"config_switch":         sw,
			"context_window_tokens": map[string]any{"dev": dev},
			"display_config":        map[string]any{"display_name": dispName, "model_capability": cap, "multimodal": mm},
		}
		if effort {
			m["reasoning_effort_config"] = map[string]any{"default": "high"}
		}
		return m
	}
	hidden := mk("glm-5.1", 1, true, "GLM-5.1", "reasoning_model", false, 116000, true)
	hidden["is_invisible_to_user"] = true // 隐藏：进 AllIDs 不进 Visible
	list := []map[string]any{
		mk("glm-5.3", 1, true, "GLM-5.3", "reasoning_model", false, 116000, true),
		mk("kimi-k3", 1, true, "Kimi-K3", "chat_model", true, 200000, true),           // chat_model + effort → 推理
		mk("seed-x", 1, true, "Seed-X", "chat_model", true, 256000, false),            // chat_model 无 effort → 非推理
		mk("custom_model_1M", 1, true, "Custom", "chat_model", false, 1000000, false), // 自备槽位剔除
		mk("summary", 1, true, "", "", false, 256000, false),                          // 无展示名剔除
		mk("off-model", 1, false, "Off", "chat_model", false, 1000, false),            // 开关关剔除
		mk("third-party", 3, true, "TP", "chat_model", false, 1000, false),            // source!=1 剔除
		mk("glm-5.2_advisor_doubao", 1, true, "GLM-5.2", "reasoning_model", false, 116000, true), // 内部路由槽位剔除
		mk("glm-5.3", 1, true, "GLM-5.3", "reasoning_model", false, 116000, true),     // 重复去重
		hidden,
	}
	raw, _ := json.Marshal(map[string]any{"config_info_list": list})
	res, err := parseTraeDetailParam(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ms := res.Visible
	if len(ms) != 3 {
		t.Fatalf("want 3 visible, got %+v", ms)
	}
	if !ms[0].IsReasoning || ms[0].IsVL || ms[0].MaxTokens != 116000 {
		t.Fatalf("glm-5.3 fields: %+v", ms[0])
	}
	if !ms[1].IsReasoning || !ms[1].IsVL { // kimi-k3：chat_model 但带 effort → 推理
		t.Fatalf("kimi-k3 fields: %+v", ms[1])
	}
	if ms[2].IsReasoning { // seed-x：chat_model 无 effort → 非推理
		t.Fatalf("seed-x fields: %+v", ms[2])
	}
	// 隐藏模型进 AllIDs（下架判定基准）但不进 Visible（不自动新增）
	if !res.AllIDs["glm-5.1"] {
		t.Fatal("hidden model should be in AllIDs")
	}
	for _, m := range ms {
		if m.ID == "glm-5.1" {
			t.Fatal("hidden model should not be visible")
		}
	}
	if _, err := parseTraeDetailParam([]byte(`{"config_info_list":[]}`)); err == nil {
		t.Fatal("empty list should error")
	}
}

func TestModelSyncDiff(t *testing.T) {
	cur := []TraeModel{
		{ID: "a", Name: "A", IsReasoning: true, IsVL: true, MaxTokens: 100},
		{ID: "b", Name: "B", MaxTokens: 100},
		{ID: "c", Name: "C", MaxTokens: 100},
	}
	probed := []TraeModel{
		{ID: "a", Name: "A", IsReasoning: true, IsVL: true, MaxTokens: 100}, // 不变
		{ID: "b", Name: "B", MaxTokens: 200},                                // 字段变
		{ID: "d", Name: "D", MaxTokens: 100},                                // 新增
	}
	d := diffTraeModels(probed, cur)
	if len(d.Added) != 1 || d.Added[0] != "d" {
		t.Fatalf("added: %+v", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0] != "c" {
		t.Fatalf("removed: %+v", d.Removed)
	}
	if len(d.Changed) != 1 || d.Changed[0] != "b" {
		t.Fatalf("changed: %+v", d.Changed)
	}
	if diffTraeModels(cur, cur).empty() != true {
		t.Fatal("identical should be empty diff")
	}
	// qoder 版同构冒烟
	qd := diffQoderModels([]QoderModel{{ID: "x"}}, nil)
	if len(qd.Added) != 1 || qd.Added[0] != "x" {
		t.Fatalf("qoder added: %+v", qd.Added)
	}
}

func TestModelSyncOverrideLoad(t *testing.T) {
	resetModelSyncForTest()
	defer resetModelSyncForTest()

	dir := t.TempDir()
	path := filepath.Join(dir, "model_sync.json")
	SetModelSyncPath(path)

	// 无文件 → 种子
	if got := QoderChatModels(); len(got) != len(qoderSeedModels()) {
		t.Fatalf("no file should fall back to seed, got %d", len(got))
	}

	// 落盘覆盖 → QoderChatModels 整体替换
	override := []QoderModel{{ID: "new1", DisplayName: "N1"}, {ID: "new2", DisplayName: "N2"}}
	saveModelSync("qoder", override, nil)
	got := QoderChatModels()
	if len(got) != 2 || got[0].ID != "new1" {
		t.Fatalf("override not applied: %+v", got)
	}
	// trae 段未被 qoder 覆盖影响（仍种子）
	if got := TraeChatModels(); len(got) != len(traeSeedModels()) {
		t.Fatalf("trae should stay seed, got %d", len(got))
	}
	// 文件确实写盘且可解析
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var f ModelSyncFile
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Qoder) != 2 || f.SyncedAt == "" {
		t.Fatalf("file content: %v %+v", err, f)
	}

	// 进程重启语义：清内存缓存重读文件
	modelSyncState.Lock()
	modelSyncState.file = ModelSyncFile{}
	modelSyncState.loaded = false
	modelSyncState.Unlock()
	if got := QoderChatModels(); len(got) != 2 {
		t.Fatalf("reload from file failed: %+v", got)
	}

	// 损坏文件 → 回落种子不崩溃
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetModelSyncForTest()
	SetModelSyncPath(path)
	if got := QoderChatModels(); len(got) != len(qoderSeedModels()) {
		t.Fatalf("corrupt should fall back to seed, got %d", len(got))
	}
}

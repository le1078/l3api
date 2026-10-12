package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/l3api/internal/auth"
)

// globalModelsTestAuth 构造 global 域测试账号（Realm 由 domain 后缀 .workbuddy.ai 推断）。
func globalModelsTestAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"}
}

func modelInfoByID(t *testing.T, c *Client, a *auth.Auth) map[string]ModelInfo {
	t.Helper()
	infos := c.FetchGlobalModelInfos(a)
	names := c.FetchGlobalModels(a)
	if len(names) == 0 {
		t.Fatalf("global catalog empty (infos=%d)", len(infos))
	}
	if len(names) != len(infos) {
		t.Fatalf("names=%d infos=%d mismatch", len(names), len(infos))
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	return byID
}

// TestGlobalProbeThreeFaceMerge 三面包并集：/v3/config 应用面（App UA，主）+
// IDE 面（补缺）+ 企业端点（补缺）。断言：
//   - 应用面/IDE 面 UA 均按预期发出；
//   - 应用面独有模型进目录（deepseek-v4.1-flash / gpt-6-astra），credits 与应用面口径一致；
//   - IDE 面独有模型经补缺进目录（auto-chat / o4-mini）；
//   - 企业端点独有模型进目录（gpt-5.3-codex / hy4-preview）；
//   - 同 id 冲突时 v3 应用面字段权威（credits/窗口）；
//   - 图片/视频生成模型被 nonChatModel 过滤。
func TestGlobalProbeThreeFaceMerge(t *testing.T) {
	const appUA = "WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1"
	var v3UAs []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			ua := r.Header.Get("User-Agent")
			v3UAs = append(v3UAs, ua)
			if ua == codeBuddyIDEUA {
				return jsonResp(200, `{"code":0,"data":{"models":[
					{"id":"auto-chat","maxInputTokens":168000,"maxOutputTokens":32000},
					{"id":"o4-mini","maxInputTokens":104000,"maxOutputTokens":24000},
					{"id":"deepseek-v4.1-flash","maxInputTokens":1000000,"maxOutputTokens":65536,"credits":"x0.99"}
				]}}`), nil
			}
			if ua != appUA {
				t.Errorf("v3/config app-face UA=%q want %q", ua, appUA)
			}
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.00","supportsReasoning":true,"onlyReasoning":true},
				{"id":"gpt-6-astra","name":"GPT-6-Astra","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x6.67 credits"},
				{"id":"hy3","maxInputTokens":192000,"maxOutputTokens":64000,"credits":"x0.00"},
				{"id":"gpt-image-2.5-sunburst","tags":["text-to-image","image-to-image"]},
				{"id":"seedance-2.5","tags":["text-to-video","image-to-video"]}
			]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.11"},
				{"id":"gpt-5.3-codex","maxInputTokens":272000,"maxOutputTokens":72000,"credits":"x1.25 credits"},
				{"id":"hy4-preview","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.29"}
			]}}`), nil
		}
		t.Errorf("unexpected path %s", r.URL.Path)
		return jsonResp(404, `{}`), nil
	})
	c.GlobalEnabled = true
	a := globalModelsTestAuth()

	byID := modelInfoByID(t, c, a)

	sawApp, sawIDE := false, false
	for _, ua := range v3UAs {
		if ua == appUA {
			sawApp = true
		}
		if ua == codeBuddyIDEUA {
			sawIDE = true
		}
	}
	if !sawApp || !sawIDE {
		t.Fatalf("v3/config faces: app=%v ide=%v (UAs=%v)", sawApp, sawIDE, v3UAs)
	}

	ds, ok := byID["deepseek-v4.1-flash"]
	if !ok {
		t.Fatal("deepseek-v4.1-flash missing (app-face model must be in union)")
	}
	if ds.Credits != "x0.00" {
		t.Errorf("deepseek credits=%q want x0.00 (app face wins over IDE/enterprise)", ds.Credits)
	}
	if ds.MaxTokens != 128000 {
		t.Errorf("deepseek MaxTokens=%d want 128000 (app face)", ds.MaxTokens)
	}
	if astra, ok := byID["gpt-6-astra"]; !ok {
		t.Error("gpt-6-astra missing (app-face model)")
	} else if astra.Credits != "x6.67" {
		t.Errorf("gpt-6-astra credits=%q want x6.67 (normalized)", astra.Credits)
	}
	if _, ok := byID["gpt-5.3-codex"]; !ok {
		t.Error("gpt-5.3-codex missing (enterprise-only supplement)")
	}
	if _, ok := byID["auto-chat"]; !ok {
		t.Error("auto-chat missing (IDE-face supplement)")
	}
	if _, ok := byID["o4-mini"]; !ok {
		t.Error("o4-mini missing (IDE-face supplement)")
	}
	if _, ok := byID["hy3"]; !ok {
		t.Error("hy3 missing (app face)")
	}
	if _, ok := byID["hy4-preview"]; !ok {
		t.Error("hy4-preview missing (enterprise supplement)")
	}
	if _, ok := byID["seedance-2.5"]; ok {
		t.Error("seedance-2.5 must be filtered (text-to-video)")
	}
	if _, ok := byID["gpt-image-2.5-sunburst"]; ok {
		t.Error("gpt-image-2.5-sunburst must be filtered (text-to-image)")
	}
}

// TestGlobalProbeAppFaceFailsDegradesToIDE 应用面失败 → 降级为 IDE 面 + 企业端点，
// 不落负缓存（names 非空），credits 以 IDE 口径为准。
func TestGlobalProbeAppFaceFailsDegradesToIDE(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			if r.Header.Get("User-Agent") == codeBuddyIDEUA {
				return jsonResp(200, `{"code":0,"data":{"models":[
					{"id":"deepseek-v4.1-flash","maxInputTokens":1000000,"maxOutputTokens":65536,"credits":"x0.99"},
					{"id":"auto-chat","maxInputTokens":168000,"maxOutputTokens":32000}
				]}}`), nil
			}
			return jsonResp(500, `{"code":-1,"msg":"boom"}`), nil
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"gpt-5.3-codex","maxInputTokens":272000,"maxOutputTokens":72000,"credits":"x1.25"}
			]}}`), nil
		}
		return jsonResp(404, `{}`), nil
	})
	c.GlobalEnabled = true
	a := globalModelsTestAuth()

	byID := modelInfoByID(t, c, a)
	ds, ok := byID["deepseek-v4.1-flash"]
	if !ok {
		t.Fatal("deepseek-v4.1-flash missing (IDE-face degrade)")
	}
	if ds.Credits != "x0.99" || ds.MaxTokens != 65536 {
		t.Errorf("deepseek degrade: credits=%q maxTokens=%d want x0.99/65536 (IDE face)", ds.Credits, ds.MaxTokens)
	}
	if _, ok := byID["auto-chat"]; !ok {
		t.Error("auto-chat missing (IDE face)")
	}
	if _, ok := byID["gpt-5.3-codex"]; !ok {
		t.Error("gpt-5.3-codex missing (enterprise supplement)")
	}
}

// TestGlobalProbeBothV3FacesFailDegradesToEnterprise 双面包全失败 → 仅企业端点结果，
// 不返回错误（等价既有「v3 失败降级」语义）。
func TestGlobalProbeBothV3FacesFailDegradesToEnterprise(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(500, `{"code":-1,"msg":"boom"}`), nil
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.11"},
				{"id":"gpt-5.3-codex","maxInputTokens":272000,"maxOutputTokens":72000,"credits":"x1.25"}
			]}}`), nil
		}
		return jsonResp(404, `{}`), nil
	})
	c.GlobalEnabled = true
	a := globalModelsTestAuth()

	byID := modelInfoByID(t, c, a)
	if ds := byID["deepseek-v4.1-flash"]; ds.Credits != "x0.11" {
		t.Errorf("enterprise-only degrade: credits=%q want x0.11", ds.Credits)
	}
	if _, ok := byID["auto-chat"]; ok {
		t.Error("auto-chat must be absent (both v3 faces down)")
	}
	if _, ok := byID["gpt-5.3-codex"]; !ok {
		t.Error("gpt-5.3-codex missing (enterprise)")
	}
}

// TestNormalizeCredits 倍率原文归一：剥 " credit(s)" 后缀、去空白，其余原样。
func TestNormalizeCredits(t *testing.T) {
	cases := map[string]string{
		"x0.59 credits": "x0.59",
		"x0.34 credit":  "x0.34",
		"x0.00":         "x0.00",
		" x3.31 ":       "x3.31",
		"":              "",
		"x1.25":         "x1.25",
	}
	for in, want := range cases {
		if got := normalizeCredits(in); got != want {
			t.Errorf("normalizeCredits(%q)=%q want %q", in, got, want)
		}
	}
}

// TestNonChatModelVideoImageTags 图片/视频生成 tags 与非对话 id 前缀均被过滤。
func TestNonChatModelVideoImageTags(t *testing.T) {
	cases := []struct {
		id     string
		maxOut int64
		tags   []string
		want   bool
	}{
		{"seedance-2.5", 0, []string{"text-to-video", "image-to-video"}, true},
		{"gpt-image-2.5-sunburst", 0, []string{"text-to-image", "image-to-image"}, true},
		{"glm-5.3", 48000, nil, false},
		{"nes-1.2", 8192, nil, true},
		{"completion-1.0", 256, nil, true},
	}
	for _, cst := range cases {
		if got := nonChatModel(cst.id, cst.maxOut, cst.tags); got != cst.want {
			t.Errorf("nonChatModel(%q,%d,%v)=%v want %v", cst.id, cst.maxOut, cst.tags, got, cst.want)
		}
	}
}

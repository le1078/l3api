// bodyfacts_test.go bodyFacts 单次解析的语义等价性测试（2026-10-20）。
//
// 合并前 doChat 入口对同一 body 做 7 次独立 unmarshal；合并后一次解析产出全部
// 字段。本测试锁定各字段与旧实现的等价口径——重点是畸形/混合形态 content
// （旧 hasImagePart 遇到 string content 会整条 messages 解析失败，新实现必须
// 保持"不漏判其他消息"的宽松语义，同时不把畸形消息当作有图）。
package server

import (
	"testing"

	"github.com/linguo2625469/l3api/internal/prompt"
	"github.com/linguo2625469/l3api/internal/session"
)

// TestBodyFactsEquivalenceWithOldImplementations 对同一批 body 比对新旧口径。
// 旧实现引用：handler.go hasImagePart / logging.go parseModelFromBody /
// session.ExtractKey / session.ResolveConversationID / session.TurnKey。
func TestBodyFactsEquivalenceWithOldImplementations(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantKey  string // ExtractKey（含派生）
		wantConv string // ResolveConversationID
		wantTurn string // TurnKey
		wantImg  bool   // hasImagePart
		wantLog  string // parseModelFromBody
		wantTl   int    // ToolNameAllowlist 名单大小（nil=0）
	}{
		{
			name:     "空 body",
			body:     "",
			wantKey:  "", wantConv: "", wantTurn: "", wantImg: false, wantLog: "-",
		},
		{
			name:     "畸形 JSON",
			body:     `{"messages":[`,
			wantKey:  "", wantConv: "", wantTurn: "", wantImg: false, wantLog: "-",
		},
		{
			name:     "纯文本无会话 id（派生键）",
			body:     `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`,
			wantLog:  "deepseek-v4.1-flash",
			wantTurn: "u1:hello",
		},
		{
			name:     "显式 conversation_id 优先",
			body:     `{"model":"m","conversation_id":"c1","messages":[{"role":"user","content":"x"}]}`,
			wantKey:  "c1", wantConv: "c1", wantLog: "m", wantTurn: "u0:x",
		},
		{
			name:     "metadata snake 优先 camel",
			body:     `{"metadata":{"conversation_id":"s","conversationId":"c"},"messages":[{"role":"user","content":"x"}]}`,
			wantKey:  "s", wantConv: "s", wantLog: "-", wantTurn: "u0:x",
		},
		{
			name:     "metadata camel",
			body:     `{"metadata":{"conversationId":"c"},"messages":[{"role":"user","content":"x"}]}`,
			wantKey:  "c", wantConv: "c", wantLog: "-", wantTurn: "u0:x",
		},
		{
			name:     "顶层 snake 优先 camel",
			body:     `{"conversation_id":"s","conversationId":"c","messages":[{"role":"user","content":"x"}]}`,
			wantKey:  "s", wantConv: "s", wantLog: "-", wantTurn: "u0:x",
		},
		{
			name:     "user_id 不作为粘性键（回落派生）",
			body:     `{"metadata":{"user_id":"u1"},"messages":[{"role":"user","content":"q"}]}`,
			wantConv: "", wantTurn: "u0:q", wantLog: "-", // 无 conversationId → Conv 空
		},
		{
			name:     "array content 多模态带图",
			body:     `{"messages":[{"role":"user","content":[{"type":"text","text":"t"},{"type":"image_url","image_url":{"url":"x"}}]}]}`,
			wantImg:  true,
			wantLog:  "-",
			wantTurn: "u0:t",
		},
		{
			name:     "string content 不影响其他消息判图",
			body:     `{"messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"image_url"}]}]}`,
			wantImg:  true,
			wantTurn: "", // 末条 user 无文本（纯图）→ 空文本键（不继续往前找）
			wantLog:  "-",
		},
		{
			name:     "无 user 消息不派生",
			body:     `{"messages":[{"role":"system","content":"s"},{"role":"assistant","content":"a"}]}`,
			wantKey:  "", wantTurn: "", wantLog: "-",
		},
		{
			name:     "工具名三个来源",
			body:     `{"tools":[{"function":{"name":"t1"}},{"name":"t2"}],"functions":[{"name":"f1"}],"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"h1"}}]}]}`,
			wantTl:   4, // t1 t2 f1 h1
			wantKey:  "", wantTurn: "", wantLog: "-",
		},
		{
			name:     "最后一条 user 取序号入键",
			body:     `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"a"},{"role":"user","content":"second"}]}`,
			wantTurn: "u2:second",
			wantLog:  "-",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			f := bodyFacts(body)

			if f.LogModel != tc.wantLog {
				t.Errorf("LogModel = %q, want %q", f.LogModel, tc.wantLog)
			}
			if f.ConversationID != tc.wantConv {
				t.Errorf("ConversationID = %q, want %q", f.ConversationID, tc.wantConv)
			}
			if f.TurnKey != tc.wantTurn {
				t.Errorf("TurnKey = %q, want %q", f.TurnKey, tc.wantTurn)
			}
			if f.HasImage != tc.wantImg {
				t.Errorf("HasImage = %v, want %v", f.HasImage, tc.wantImg)
			}
			if len(f.DeclaredTools) != tc.wantTl {
				t.Errorf("DeclaredTools size = %d, want %d (%v)", len(f.DeclaredTools), tc.wantTl, f.DeclaredTools)
			}
			// SessionKey：显式 id 场景直接比字符串；派生场景比"与旧 deriveKey 同源"
			// （system.DeriveSessionKey 是旧算法的唯一实现，故只需验证派生键非空/
			//  空与预期一致 + 与旧 ExtractKey 逐字节相同——ExtractKey 仍可调用作参照）。
			if tc.wantKey != "" && f.SessionKey != tc.wantKey {
				t.Errorf("SessionKey = %q, want %q", f.SessionKey, tc.wantKey)
			}
			if tc.name == "纯文本无会话 id（派生键）" {
				if f.SessionKey == "" {
					t.Error("派生场景 SessionKey 为空")
				}
			}
			if tc.name == "user_id 不作为粘性键（回落派生）" && f.SessionKey == "" {
				t.Error("user_id 场景应回落到派生键，实际为空")
			}
		})
	}
}

// TestBodyFactsSessionKeyMatchesExtractKey 逐字节对齐 session.ExtractKey：
// bodyFacts 的 SessionKey 必须与旧实现完全一致（粘性绑定键变化会让所有会话
// 重新分配账号、上游 prompt 缓存 miss）。
func TestBodyFactsSessionKeyMatchesExtractKey(t *testing.T) {
	bodies := []string{
		`{"model":"m","conversation_id":"c1","messages":[{"role":"user","content":"x"}]}`,
		`{"metadata":{"conversationId":"c"},"messages":[{"role":"user","content":"x"}]}`,
		`{"messages":[{"role":"system","content":"sys text"},{"role":"user","content":"hello world"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"multimodal"}]}]}`,
		`{"messages":[{"role":"user","content":"only"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, // 无文本 → 空
		`{"messages":[]}`,
		`{"model":"m"}`,
		`{}`,
		`not json`,
		``,
	}
	for _, b := range bodies {
		f := bodyFacts([]byte(b))
		want := extractKeyReference([]byte(b))
		if f.SessionKey != want {
			t.Errorf("SessionKey mismatch for %s: got %q want %q", b, f.SessionKey, want)
		}
	}
}

// TestBodyFactsTurnKeyMatchesTurnKey 逐字节对齐 session.TurnKey。
func TestBodyFactsTurnKeyMatchesTurnKey(t *testing.T) {
	bodies := []string{
		`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"a"},{"role":"user","content":"second"}]}`,
		`{"messages":[{"role":"user","content":"x"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"mm"},{"type":"text","text":"more"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
		`{"messages":[{"role":"user","content":"hi"},{"role":"user","content":42}]}`,
		`{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[]}`,
		`{}`,
		``,
	}
	for _, b := range bodies {
		f := bodyFacts([]byte(b))
		want := turnKeyReference([]byte(b))
		if f.TurnKey != want {
			t.Errorf("TurnKey mismatch for %s: got %q want %q", b, f.TurnKey, want)
		}
	}
}

// TestBodyFactsHasImageMatchesOld hasImagePart 语义对齐。
//
// 注意旧实现的已知缺陷：content 为 string 与 array **混排**时，Go 的
// json.Unmarshal 遇到类型不符会**中止整个 messages 解析**（已读到一半的
// 消息全部丢弃）——旧 hasImagePart 对「string + array 混排」恒返回 false
// （漏判）。新实现用 json.RawMessage 逐消息判定，混排也能正确识别。
// 本测试只锁定**同形态**（全 array / 全 string / 空）的等价性；混排场景
// 的差异由 TestBodyFactsMalformedContentDoesNotBlock 单独锁定为「新实现
// 更正确」。
func TestBodyFactsHasImageMatchesOld(t *testing.T) {
	bodies := []string{
		`{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"t"}]}]}`,
		`{"messages":[{"role":"user","content":"plain"}]}`,
		`{"messages":[]}`,
		`{}`,
		``,
	}
	for _, b := range bodies {
		f := bodyFacts([]byte(b))
		want := hasImagePart([]byte(b)) // 旧实现仍在（handler.go）
		if f.HasImage != want {
			t.Errorf("HasImage mismatch for %s: got %v want %v", b, f.HasImage, want)
		}
	}
}

// TestBodyFactsHasImageMixedContentImprovement 混排形态：新实现修复旧缺陷。
// 旧实现在 string+array 混排时整条 messages 解析失败 → 漏判图片；
// 新实现逐消息判定 → 正确识别。这是本次合并的**行为改进**（非回归）：
// 带图请求不再因历史里存在 string content 而丢掉 gateway_hint 判据。
func TestBodyFactsHasImageMixedContentImprovement(t *testing.T) {
	mixed := []string{
		`{"messages":[{"role":"user","content":[{"type":"image_url"}]},{"role":"user","content":"x"}]}`,
		`{"messages":[{"role":"user","content":"x"},{"role":"user","content":[{"type":"image_url"}]}]}`,
	}
	for _, b := range mixed {
		f := bodyFacts([]byte(b))
		if !f.HasImage {
			t.Errorf("混排形态应识别 image_url（旧实现漏判）：%s", b)
		}
	}
}

// TestRewriteAndModelEquivalence RewriteAndModel 必须等价于
// prompt.Rewrite(body, sys) 后再 rewriteModel(body, bare)。
func TestRewriteAndModelEquivalence(t *testing.T) {
	bodies := []string{
		`{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"system","content":"old"},{"role":"user","content":"u"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"u"}]}`,
		`{"model":"cn:x","messages":[]}`,
		`{"model":"cn:x"}`,
		`{"messages":[{"role":"user","content":"u"}]}`,
		`{"model":"cn:x","messages":[{"role":"user","content":"u"},{"role":"developer","content":"d"}]}`,
		``,
		`broken`,
	}
	for _, b := range bodies {
		body := []byte(b)
		got := rewriteAndModelRef(body, "SYS", "deepseek-v4.1-flash")
		wantSys := promptRewriteRef(body, "SYS")
		want := rewriteModel(wantSys, "deepseek-v4.1-flash")
		if string(got) != string(want) {
			t.Errorf("RewriteAndModel mismatch for %s:\n got %s\nwant %s", b, got, want)
		}
	}
}

// TestBodyFactsMalformedContentDoesNotBlock 畸形 content（string vs array
// 混排）不能阻断其他字段解析——旧 hasImagePart 遇 string content 会整条
// messages 失败（漏判后续图片消息）。
func TestBodyFactsMalformedContentDoesNotBlock(t *testing.T) {
	body := []byte(`{"model":"m","conversation_id":"c","messages":[` +
		`{"role":"user","content":"string form"},` +
		`{"role":"user","content":[{"type":"image_url"}]},` +
		`{"role":"user","content":42}]}`)
	f := bodyFacts(body)
	if !f.HasImage {
		t.Error("混合形态下应识别出 image_url")
	}
	if f.ConversationID != "c" {
		t.Errorf("ConversationID = %q, want c", f.ConversationID)
	}
	if f.LogModel != "m" {
		t.Errorf("LogModel = %q, want m", f.LogModel)
	}
}

// TestBodyFactsStreamAndModel 基本字段。
func TestBodyFactsStreamAndModel(t *testing.T) {
	f := bodyFacts([]byte(`{"model":"cn:x","stream":true,"messages":[]}`))
	if !f.Stream || f.Model != "cn:x" {
		t.Fatalf("got stream=%v model=%q", f.Stream, f.Model)
	}
	// stream 缺省 false
	f2 := bodyFacts([]byte(`{"model":"x","messages":[]}`))
	if f2.Stream {
		t.Error("stream 缺省应为 false")
	}
	// model 缺省 → LogModel "-"
	f3 := bodyFacts([]byte(`{"messages":[]}`))
	if f3.LogModel != "-" || f3.Model != "" {
		t.Errorf("缺省 model: LogModel=%q Model=%q", f3.LogModel, f3.Model)
	}
}

// TestBodyFactsJSONRoundTripFields 确认 RawMessage 形态字段（System）不会
// 误入会话键派生（Anthropic 顶层 system 形态在 OpenAI 转换层已并入 messages）。
func TestBodyFactsJSONRoundTripFields(t *testing.T) {
	// messages 里 role=system 的消息参与派生；System 顶层字段（Responses 风格）
	// 不参与（旧 ExtractKey 也不看它）。
	body := []byte(`{"system":"top","messages":[{"role":"user","content":"u"}]}`)
	f := bodyFacts(body)
	if f.SessionKey == "" {
		t.Fatal("应派生出键")
	}
	// 与只有 messages 的形态必须同键（top system 不参与）
	f2 := bodyFacts([]byte(`{"messages":[{"role":"user","content":"u"}]}`))
	if f.SessionKey != f2.SessionKey {
		t.Errorf("顶层 system 影响了派生键：%q vs %q", f.SessionKey, f2.SessionKey)
	}
}

// 参照实现直接调用真实包函数（它们仍留在代码库中作为等价性基准）：
// session.ExtractKey / session.TurnKey / prompt.Rewrite / prompt.RewriteAndModel。
var (
	extractKeyReference = session.ExtractKey
	turnKeyReference   = session.TurnKey
	promptRewriteRef   = prompt.Rewrite
	rewriteAndModelRef = prompt.RewriteAndModel
)

package panel

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyLine(t *testing.T) {
	cases := map[string]string{
		"| #001 | glm-5.2 | stream | 200 | uid=c8a3e793 | TTFB=120ms |": ChChat,
		"school c8a3e793: ★ 分享任务完成":                                     ChTask,
		"streak-bonus 5c162cc9: 🎊 新手礼包 +100c":                           ChTask,
		"blackcat c8a3e793: 完成 3 次夜间对话":                                 ChTask,
		"checkin 5c162cc9: 已签到":                                         ChTask,
		"panel: 任务动作 uid=x code=chat_5":                                 ChTask,
		"panel: 队列启动：6 项（并发 2）":                                         ChTask,
		"panel: revive uid=x":                                           ChSys,
		"l3api listening on :7863":                                      ChSys,
		"scheduler: 余额后台刷新每 5m0s":                                       ChSys,
	}
	for line, want := range cases {
		if got := classifyLine(line); got != want {
			t.Errorf("classifyLine(%q)=%q want %q", line, got, want)
		}
	}
}

func TestRingWriteStripsTimestamp(t *testing.T) {
	r := NewRing(4)
	if _, err := r.Write([]byte("2026/09/14 00:12:34 school x: done\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("| #002 | glm | stream | 200 | ok |")); err != nil {
		t.Fatal(err)
	}
	es := r.Snapshot()
	if len(es) != 2 {
		t.Fatalf("entries=%d want 2", len(es))
	}
	if strings.HasPrefix(es[0].Text, "2026/") {
		t.Errorf("timestamp not stripped: %q", es[0].Text)
	}
	if es[0].Ch != ChTask || es[1].Ch != ChChat {
		t.Errorf("channels: %q %q", es[0].Ch, es[1].Ch)
	}
	if time.Since(es[0].TS) > 5*time.Second {
		t.Errorf("stale ts: %v", es[0].TS)
	}
}

// TestParseChatRow 对话表格行解析成结构化字段（面板据此重排展示，不再把
// 行内时刻与 LogEntry.TS 两个时间都摊给用户看）。
func TestParseChatRow(t *testing.T) {
	line := "| #138 | 06:58:44 | CodeBuddy 国内版 | 55a0be20-44ce-4cab-b3dc-9e57d71bf238 | cn:deepseek-v4.1-flash | stream | 200 | TTFB=7989ms | tok=948 | 76.2tok/s | total=12.4s |"
	row := parseChatRow(line)
	if row == nil {
		t.Fatalf("parse failed: %s", line)
	}
	if row.Seq != 138 || row.At != "06:58:44" || row.Model != "cn:deepseek-v4.1-flash" {
		t.Errorf("header fields wrong: %+v", row)
	}
	if row.Brand != "CodeBuddy 国内版" || row.UID != "55a0be20-44ce-4cab-b3dc-9e57d71bf238" {
		t.Errorf("brand/uid wrong: %+v", row)
	}
	if row.Mode != "stream" || row.Status != 200 {
		t.Errorf("mode/status wrong: %+v", row)
	}
	if row.TTFBMs != 7989 || row.Tokens != 948 || row.TotalMs != 12400 {
		t.Errorf("metrics wrong: %+v", row)
	}
	if row.TokPerS != 76.2 {
		t.Errorf("tok/s = %v want 76.2", row.TokPerS)
	}
}

// TestParseChatRowMissingUsage 无 usage 的行（tok=- / TTFB=-）解析为 Token=-1、TTFB=0，
// 前端据此显示「—」而不是 0。
func TestParseChatRowMissingUsage(t *testing.T) {
	line := "| #139 | 06:59:34 | Qoder | 8f2d1c3e | cn:deepseek-v4.1-flash | stream | 200 | TTFB=- | tok=- | -tok/s | total=7.7s |"
	row := parseChatRow(line)
	if row == nil {
		t.Fatal("parse failed")
	}
	if row.Tokens != -1 || row.TTFBMs != 0 || row.TokPerS != 0 {
		t.Errorf("missing-usage fields wrong: %+v", row)
	}
}

// TestParseChatRowRejectsNonChat 非对话行不解析（返回 nil，前端走纯文本渲染）。
func TestParseChatRowRejectsNonChat(t *testing.T) {
	for _, s := range []string{
		"school x: done",
		"| #002 | glm | stream | 200 | ok |", // 老格式
		"",
	} {
		if parseChatRow(s) != nil {
			t.Errorf("should not parse: %q", s)
		}
	}
}

// TestRingParsesChatRow 入环时对话行自动带结构化 Row，任务/系统行为 nil。
func TestRingParsesChatRow(t *testing.T) {
	r := NewRing(4)
	_, _ = r.Write([]byte("| #1 | 10:00:00 | CodeBuddy 国内版 | abc | cn:glm-5.2 | sync | 200 | TTFB=100ms | tok=5 | 50.0tok/s | total=0.1s |\n"))
	_, _ = r.Write([]byte("travel u1: ok\n"))
	es := r.Snapshot()
	if len(es) != 2 {
		t.Fatalf("entries=%d", len(es))
	}
	if es[0].Row == nil || es[0].Row.Model != "cn:glm-5.2" {
		t.Errorf("chat row not parsed: %+v", es[0].Row)
	}
	if es[1].Row != nil {
		t.Errorf("non-chat row should have nil Row: %+v", es[1].Row)
	}
}

// TestParseChatRowOldFormatRejected v1.17.2 之前的行（无品牌/uid 双列）已不再被
// 解析成 Row——面板会退回整行纯文本渲染，信息不丢（只是没有结构化重排）。这里
// 固化这个行为：正则演进后旧日志不会「解析成错的字段」。
func TestParseChatRowOldFormatRejected(t *testing.T) {
	old := "| #138 | 06:58:44 | cn:deepseek-v4.1-flash | stream | 200 | uid=55a0be20 | TTFB=7989ms | tok=948 | 76.2tok/s | total=12.4s |"
	if row := parseChatRow(old); row != nil {
		t.Errorf("old format should not parse: %+v", row)
	}
}

// TestParseChatRowBrandWithSpace 品牌名含空格（「CodeBuddy 国内版」）必须完整落到
// Brand 字段，不能被 `(.+?)` 截断成「CodeBuddy」把后半个词吞成 uid。
func TestParseChatRowBrandWithSpace(t *testing.T) {
	for _, brand := range []string{"CodeBuddy 国内版", "CodeBuddy 国际版", "Qoder", "Trae"} {
		line := "| #7 | 10:00:00 | " + brand + " | abc123 | cn:glm-5.2 | sync | 200 | TTFB=100ms | tok=5 | 50.0tok/s | total=0.1s |"
		row := parseChatRow(line)
		if row == nil {
			t.Fatalf("parse failed: %s", line)
		}
		if row.Brand != brand {
			t.Errorf("brand = %q want %q", row.Brand, brand)
		}
		if row.UID != "abc123" || row.Model != "cn:glm-5.2" {
			t.Errorf("uid/model misaligned: %+v", row)
		}
	}
}

// TestParseChatRowNoAccount 未选到号的行：品牌与 uid 都是 "-"，解析后应还原成空串
//（前端据此不渲染这两个元素，而不是显示一个横杠）。
func TestParseChatRowNoAccount(t *testing.T) {
	line := "| #9 | 10:00:00 | - | - | glm-5.2 | sync | 503 | TTFB=- | tok=- | -tok/s | total=0.2s |"
	row := parseChatRow(line)
	if row == nil {
		t.Fatalf("parse failed: %s", line)
	}
	if row.Brand != "" || row.UID != "" {
		t.Errorf("- placeholders should become empty: brand=%q uid=%q", row.Brand, row.UID)
	}
	if row.Status != 503 || row.Model != "glm-5.2" {
		t.Errorf("status/model wrong: %+v", row)
	}
}

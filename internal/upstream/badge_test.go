package upstream

import "testing"

func TestBadgeOf(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"craft", "badge:夜间免费:#FF0000"}, "夜间免费"},
		{[]string{"badge:限时免费:#FF0000"}, "限时免费"},
		{[]string{"badge:夜间折扣:#1E90FF"}, "夜间折扣"},
		{[]string{"craft"}, ""},
		{nil, ""},
		{[]string{"badge:无颜色"}, "无颜色"},
	}
	for _, c := range cases {
		if got := badgeOf(c.tags); got != c.want {
			t.Errorf("badgeOf(%v) = %q, want %q", c.tags, got, c.want)
		}
	}
}

func TestMergeModelInfosBadgeBackfill(t *testing.T) {
	// 主路（v3/config）无 badge，副路（企业端点）有 → 合并后保留副路 badge/tags。
	primary := []ModelInfo{{ID: "hy3", Credits: "x0.00"}}
	secondary := []ModelInfo{{ID: "hy3", Badge: "限时免费", Tags: []string{"craft", "badge:限时免费:#FF0000"}}}
	out := mergeModelInfos(primary, secondary)
	if len(out) != 1 || out[0].Badge != "限时免费" || len(out[0].Tags) != 2 {
		t.Fatalf("badge backfill failed: %+v", out)
	}
	// 主路已有 badge 不被副路覆盖
	primary2 := []ModelInfo{{ID: "a", Badge: "夜间免费"}}
	secondary2 := []ModelInfo{{ID: "a", Badge: "限时免费"}}
	out2 := mergeModelInfos(primary2, secondary2)
	if out2[0].Badge != "夜间免费" {
		t.Fatalf("primary badge should win: %+v", out2[0])
	}
}

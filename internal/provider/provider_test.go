package provider

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]Brand{
		"":          CodeBuddy, // 空 = 缺省（老凭证零回归）
		"codebuddy": CodeBuddy,
		" Qoder ":   Qoder, // 容忍空白与大小写
		"TRAE":      Trae,
		"unknown-x": CodeBuddy, // 未知回落缺省
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"codebuddy", "qoder", "trae", "CodeBuddy", " QODER "} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "bogus"} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}

func TestCapabilities(t *testing.T) {
	// codebuddy 全能力；qoder = 余额 + 每日权益领取（Campaign claim，需设备指纹）；
	// trae = 每日签到（ug/checkin_credits：cookie 现换 JWT + 真实设备 id）。
	// 三者 Checkin 的语义各不相同，见各 brand 的注释与 upstream 实现。
	if c := For(CodeBuddy).Capabilities(); !c.Tasks || !c.Checkin || !c.Balance {
		t.Errorf("codebuddy caps missing: %+v", c)
	}
	if c := For(Qoder).Capabilities(); c != (Capabilities{Checkin: true, Balance: true}) {
		t.Errorf("qoder caps = %+v, want Checkin+Balance", c)
	}
	// trae 签到 + 余额均已接通（余额 = pay/ide_user_ent_usage 的 usage_summary）。
	if c := For(Trae).Capabilities(); c != (Capabilities{Checkin: true, Balance: true}) {
		t.Errorf("trae caps = %+v, want Checkin+Balance", c)
	}
	if All() == nil || len(All()) != 3 {
		t.Fatalf("All() should list 3 brands")
	}
}

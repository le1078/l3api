package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBrand(t *testing.T) {
	// 嵌套形/扁平形均读顶层 brand 键。
	n, err := Parse([]byte(`{"auth":{"accessToken":"at"},"account":{"uid":"u1"},"brand":"qoder"}`))
	if err != nil || n.Brand() != "qoder" {
		t.Fatalf("nested brand: %q %v", n.Brand(), err)
	}
	f, err := Parse([]byte(`{"accessToken":"at","uid":"u2","brand":"trae"}`))
	if err != nil || f.Brand() != "trae" {
		t.Fatalf("flat brand: %q %v", f.Brand(), err)
	}
	// 空/缺省回落 codebuddy（老凭证零回归）。
	d, err := Parse([]byte(`{"accessToken":"at","uid":"u3"}`))
	if err != nil || d.Brand() != "codebuddy" {
		t.Fatalf("default brand: %q %v", d.Brand(), err)
	}
}

func TestSaveAtomicBrandOmit(t *testing.T) {
	// codebuddy 老凭证：brand 键不落盘（文件保持零 diff）。
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-x.json")
	a := &Auth{AccessToken: "at", UID: "u1", FilePath: p}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if stringContains(string(raw), `"brand"`) {
		t.Fatalf("codebuddy file should omit brand: %s", raw)
	}
	// qoder 凭证：brand 键必须落盘。
	q := &Auth{AccessToken: "at2", UID: "u2", FilePath: p, brand: "qoder"}
	if err := q.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(p)
	if !stringContains(string(raw2), `"brand": "qoder"`) {
		t.Fatalf("qoder file missing brand: %s", raw2)
	}
}

func TestLoadDirBrandInference(t *testing.T) {
	// 文件名前缀推断：qoder*/trae* 补写 brand 键；workbuddy* 不写（= codebuddy）。
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("workbuddy-a.json", `{"accessToken":"at","uid":"u-wb"}`)
	write("qoder-b.json", `{"accessToken":"at","uid":"u-qd"}`)
	write("trae-c.json", `{"accessToken":"at","uid":"u-tr"}`)
	auths, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, a := range auths {
		got[a.UID] = a.Brand()
	}
	want := map[string]string{
		"u-wb": "codebuddy",
		"u-qd": "qoder",
		"u-tr": "trae",
	}
	for uid, b := range want {
		if got[uid] != b {
			t.Errorf("uid %s brand = %q, want %q", uid, got[uid], b)
		}
	}
	// qoder/trae 推断结果已 backfill 落盘；重载后仍按落盘键判定（文件改名不漂移）。
	auths2, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range auths2 {
		if a.UID == "u-qd" && a.Brand() != "qoder" {
			t.Errorf("backfilled brand lost: %q", a.Brand())
		}
	}
}

func TestBackfillBrandFor(t *testing.T) {
	a := &Auth{AccessToken: "at", UID: "u1"}
	if _, err := BackfillBrandFor(a, "bogus"); err == nil {
		t.Fatal("want error for bogus brand")
	}
	changed, err := BackfillBrandFor(a, "Trae")
	if err != nil || !changed || a.Brand() != "trae" {
		t.Fatalf("backfill: changed=%v brand=%q err=%v", changed, a.Brand(), err)
	}
	changed, _ = BackfillBrandFor(a, "trae")
	if changed {
		t.Fatal("same brand should be idempotent")
	}
}

func stringContains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

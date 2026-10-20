package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/pool"
	"github.com/linguo2625469/l3api/internal/upstream"
)

func TestBrandModelCapsInModelsList(t *testing.T) {
	up := upstream.New()
	pl := pool.New("")
	// /v1/models 的品牌段只在池内有该品牌账号时列出——塞两个最小 auth 才能进分支。
	aq := &auth.Auth{UID: "q1"}
	at := &auth.Auth{UID: "t1"}
	_, _ = auth.BackfillBrandFor(aq, "qoder")
	_, _ = auth.BackfillBrandFor(at, "trae")
	pl.Add(aq)
	pl.Add(at)
	h := NewHandler(Config{Pool: pl, Upstream: up, APIKey: ""})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := make([]byte, 1<<20)
	n, _ := resp.Body.Read(raw)
	raw = raw[:n]

	var body struct {
		Data []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	list := body.Data
	if len(list) == 0 {
		list = body.Models
	}
	byID := map[string]map[string]any{}
	for _, m := range list {
		id, _ := m["id"].(string)
		byID[id] = m
	}
	q, ok := byID["qoder:dmodel"]
	if !ok {
		t.Fatalf("missing qoder:dmodel, have %d models", len(list))
	}
	if q["credits"] != "x0.50" {
		t.Errorf("qoder:dmodel credits=%v", q["credits"])
	}
	if q["max_output_tokens"] == nil {
		t.Errorf("qoder:dmodel missing max_output_tokens: %+v", q)
	}
	tr, ok := byID["trae:glm-5.3"]
	if !ok {
		t.Fatalf("missing trae:glm-5.3")
	}
	if tr["credits"] != "x0.78" {
		t.Errorf("trae:glm-5.3 credits=%v", tr["credits"])
	}
	if tr["max_output_tokens"] == nil {
		t.Errorf("trae:glm-5.3 missing max_output_tokens: %+v", tr)
	}
}

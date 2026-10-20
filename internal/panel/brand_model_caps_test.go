package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/l3api/internal/auth"
	"github.com/linguo2625469/l3api/internal/pool"
	"github.com/linguo2625469/l3api/internal/upstream"
)

func TestPanelModelsBrandCaps(t *testing.T) {
	pl := pool.New("")
	aq := &auth.Auth{UID: "q1"}
	at := &auth.Auth{UID: "t1"}
	_, _ = auth.BackfillBrandFor(aq, "qoder")
	_, _ = auth.BackfillBrandFor(at, "trae")
	pl.Add(aq)
	pl.Add(at)

	p := New(Config{Pool: pl, Upstream: upstream.New(), APIKey: ""})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/panel/api/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	byCall := map[string]map[string]any{}
	for _, m := range body.Models {
		id, _ := m["call_id"].(string)
		byCall[id] = m
	}
	q := byCall["qoder:dmodel"]
	if q == nil {
		t.Fatalf("missing qoder:dmodel in %d models", len(body.Models))
	}
	if q["credits"] != "x0.50" {
		t.Errorf("credits=%v", q["credits"])
	}
	if q["default_effort"] != "max" {
		t.Errorf("default_effort=%v", q["default_effort"])
	}
	eff, _ := q["supported_efforts"].([]any)
	if len(eff) != 2 {
		t.Errorf("supported_efforts=%v", q["supported_efforts"])
	}
	if q["max_output_tokens"] == nil || q["context_length"] == nil {
		t.Errorf("missing sizes: %+v", q)
	}
	tr := byCall["trae:glm-5.3"]
	if tr == nil {
		t.Fatalf("missing trae:glm-5.3")
	}
	if tr["credits"] != "x0.78" {
		t.Errorf("trae credits=%v", tr["credits"])
	}
	if tr["default_effort"] != "high" {
		t.Errorf("trae default_effort=%v", tr["default_effort"])
	}
	t.Logf("qoder:dmodel credits=%v def=%v eff=%v out=%v", q["credits"], q["default_effort"], q["supported_efforts"], q["max_output_tokens"])
}

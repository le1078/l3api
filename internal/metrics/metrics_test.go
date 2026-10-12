// metrics_test.go 指标渲染的正确性：计数器/直方图累计语义、label 转义、排序稳定。
package metrics

import (
	"strings"
	"testing"
)

func TestCounterVecRender(t *testing.T) {
	c := NewCounterVec("test_total", "test counter")
	c.Inc("model", "glm", "status", "200")
	c.Add(4, "model", "glm", "status", "200")
	c.Inc("model", "qwen", "status", "503")
	out := (&WriteTo{Counters: []*CounterVec{c}}).Render()
	if !strings.Contains(out, "# TYPE test_total counter") {
		t.Fatalf("missing TYPE header:\n%s", out)
	}
	if !strings.Contains(out, `test_total{model="glm",status="200"} 5`) {
		t.Fatalf("missing glm 5 sample:\n%s", out)
	}
	if !strings.Contains(out, `test_total{model="qwen",status="503"} 1`) {
		t.Fatalf("missing qwen sample:\n%s", out)
	}
}

func TestHistogramCumulative(t *testing.T) {
	h := NewHistogramVec("test_seconds", "test hist", []float64{0.1, 1})
	h.Observe(0.05, "mode", "stream")
	h.Observe(0.5, "mode", "stream")
	h.Observe(5, "mode", "stream")
	out := (&WriteTo{Histograms: []*HistogramVec{h}}).Render()
	// le=0.1 → 1 个；le=1 → 2 个；le=+Inf → 3 个；sum≈5.55；count=3。
	for _, want := range []string{
		`test_seconds_bucket{mode="stream",le="0.1"} 1`,
		`test_seconds_bucket{mode="stream",le="1"} 2`,
		`test_seconds_bucket{mode="stream",le="+Inf"} 3`,
		`test_seconds_count{mode="stream"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	c := NewCounterVec("esc_total", "esc")
	c.Inc("model", `a"b\c`)
	out := (&WriteTo{Counters: []*CounterVec{c}}).Render()
	if !strings.Contains(out, `model="a\"b\\c"`) {
		t.Fatalf("escaping failed:\n%s", out)
	}
}

func TestGaugeFloatRender(t *testing.T) {
	g := NewGaugeVec("test_gauge", "g")
	g.SetFloat(1.5, "k", "v")
	g.Set(3, "k", "v2")
	out := (&WriteTo{Gauges: []*GaugeVec{g}}).Render()
	if !strings.Contains(out, `test_gauge{k="v"} 1.5`) {
		t.Fatalf("float gauge:\n%s", out)
	}
	if !strings.Contains(out, `test_gauge{k="v2"} 3`) {
		t.Fatalf("int gauge:\n%s", out)
	}
}

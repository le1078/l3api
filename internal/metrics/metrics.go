// Package metrics 极简 Prometheus 文本格式指标库（v1.11，零第三方依赖）。
//
// 为什么不用 prometheus/client_golang：项目依赖面刻意极小（go.mod 仅 redis），
// 本包用到的原语（Counter/Gauge/Histogram + label 维度 + text exposition）
// 手写总量约 300 行，换来零供应链面与零构建网络依赖。
//
// 并发口径：每个指标序列一个 atomic，Scrape 快照遍历输出——与 client_golang
// 的原子快照语义一致；Scrape 期间的新增写入可能落在快照前后（可接受，指标
// 是统计观测不是账本，账本语义在 usage.Recorder）。
//
// 输出格式：Prometheus text exposition（https://prometheus.io/docs/instrumenting/exposition_formats/），
// HELP/TYPE 头 + 样本行，label 值做转义。
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// CounterVec 带标签的单调递增计数器。
type CounterVec struct {
	name, help string
	mu         sync.Mutex
	series     map[string]*counterChild // label 值串 → 子序列
}

type counterChild struct {
	labelValues []string
	value       atomic.Int64
}

// NewCounterVec 构建计数器族。
func NewCounterVec(name, help string) *CounterVec {
	return &CounterVec{name: name, help: help, series: map[string]*counterChild{}}
}

// Inc 计数 +1。
func (c *CounterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add 计数 +n（n<=0 忽略——Counter 语义只增）。
func (c *CounterVec) Add(n int64, labelValues ...string) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	ch, ok := c.series[seriesKey(labelValues)]
	if !ok {
		ch = &counterChild{labelValues: append([]string(nil), labelValues...)}
		c.series[seriesKey(labelValues)] = ch
	}
	c.mu.Unlock()
	ch.value.Add(n)
}

// GaugeVec 带标签的可增减仪表。
type GaugeVec struct {
	name, help string
	mu         sync.Mutex
	series     map[string]*gaugeChild
}

type gaugeChild struct {
	labelValues []string
	value       atomic.Int64 // 定点数 ×1000（避免 float 原子）
}

// NewGaugeVec 构建仪表族。
func NewGaugeVec(name, help string) *GaugeVec {
	return &GaugeVec{name: name, help: help, series: map[string]*gaugeChild{}}
}

// Set 设置当前值（整数）。
func (g *GaugeVec) Set(v int64, labelValues ...string) {
	g.setRaw(v*1000, labelValues...)
}

// SetFloat 设置当前值（浮点，如平均延迟）。
func (g *GaugeVec) SetFloat(v float64, labelValues ...string) {
	g.setRaw(int64(v*1000), labelValues...)
}

func (g *GaugeVec) setRaw(raw int64, labelValues ...string) {
	g.mu.Lock()
	ch, ok := g.series[seriesKey(labelValues)]
	if !ok {
		ch = &gaugeChild{labelValues: append([]string(nil), labelValues...)}
		g.series[seriesKey(labelValues)] = ch
	}
	g.mu.Unlock()
	ch.value.Store(raw)
}

// HistogramVec 带标签的直方图（固定桶，Cumulative 计数）。
type HistogramVec struct {
	name, help string
	buckets    []float64 // 上界（升序，末位必含 +Inf 语义——省略 Inf 桶，输出时补）
	mu         sync.Mutex
	series     map[string]*histChild
}

type histChild struct {
	labelValues []string
	counts      []atomic.Int64 // 与 buckets 等长（各上界累计计数）
	sumMicros   atomic.Int64   // 观测总和（μs 定点，float 累计精度考虑）
	count       atomic.Int64   // 总观测数
}

// DefaultBuckets 请求延迟的默认桶（秒）。
var DefaultBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// NewHistogramVec 构建直方图族（buckets 空 → DefaultBuckets）。
func NewHistogramVec(name, help string, buckets []float64) *HistogramVec {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	} else {
		b := append([]float64(nil), buckets...)
		sort.Float64s(b)
		buckets = b
	}
	return &HistogramVec{name: name, help: help, buckets: buckets, series: map[string]*histChild{}}
}

// Observe 记录一次观测（秒）。
func (h *HistogramVec) Observe(v float64, labelValues ...string) {
	h.mu.Lock()
	ch, ok := h.series[seriesKey(labelValues)]
	if !ok {
		ch = &histChild{labelValues: append([]string(nil), labelValues...), counts: make([]atomic.Int64, len(h.buckets))}
		h.series[seriesKey(labelValues)] = ch
	}
	h.mu.Unlock()
	for i, ub := range h.buckets {
		if v <= ub {
			ch.counts[i].Add(1)
		}
	}
	ch.count.Add(1)
	ch.sumMicros.Add(int64(v * 1e6))
}

// seriesKey label 值组合的 map 键（\x00 连接——label 值来自内部白名单枚举，
// 不来自用户输入，无碰撞风险；model 标签例外，见 server 侧裁剪注释）。
func seriesKey(labelValues []string) string {
	return strings.Join(labelValues, "\x00")
}

// WriteTo 把整个指标集写成 Prometheus text exposition。
// 各 Vec 独立成族，族内按 label 值排序输出（diff 稳定）。
type WriteTo struct {
	Counters    []*CounterVec
	Gauges      []*GaugeVec
	Histograms  []*HistogramVec
	ExtraHeader []string // 追加的原始行（如逐行拼好的 gauge 序列）
}

// Render 输出文本格式。
func (w *WriteTo) Render() string {
	var b strings.Builder
	for _, c := range w.Counters {
		c.mu.Lock()
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
		keys := make([]string, 0, len(c.series))
		for k := range c.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ch := c.series[k]
			fmt.Fprintf(&b, "%s%s %d\n", c.name, renderLabels(ch.labelValues), ch.value.Load())
		}
		c.mu.Unlock()
	}
	for _, g := range w.Gauges {
		g.mu.Lock()
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		keys := make([]string, 0, len(g.series))
		for k := range g.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ch := g.series[k]
			fmt.Fprintf(&b, "%s%s %s\n", g.name, renderLabels(ch.labelValues), formatFixed(ch.value.Load()))
		}
		g.mu.Unlock()
	}
	for _, h := range w.Histograms {
		h.mu.Lock()
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
		keys := make([]string, 0, len(h.series))
		for k := range h.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ch := h.series[k]
			// 各桶累计计数（bucket 上界为 +Inf 前的全部桶）。
			cum := int64(0)
			for i, ub := range h.buckets {
				cum = ch.counts[i].Load()
				fmt.Fprintf(&b, "%s_bucket%s %d\n", h.name,
					renderLabelsWithLe(ch.labelValues, formatFloat(ub)), cum)
			}
			// +Inf 桶 = 总计数。
			fmt.Fprintf(&b, "%s_bucket%s %d\n", h.name,
				renderLabelsWithLe(ch.labelValues, "+Inf"), ch.count.Load())
			fmt.Fprintf(&b, "%s_sum%s %s\n", h.name, renderLabels(ch.labelValues), formatFixed(ch.sumMicros.Load()))
			fmt.Fprintf(&b, "%s_count%s %d\n", h.name, renderLabels(ch.labelValues), ch.count.Load())
		}
		h.mu.Unlock()
	}
	for _, line := range w.ExtraHeader {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// renderLabels label 串（无标签返回空串）：{k="v",...}
func renderLabels(labelValues []string) string {
	if len(labelValues) == 0 {
		return ""
	}
	return renderLabelsWithLe(labelValues, "")
}

// renderLabelsWithLe 带 le 标签的直方图样本 label 串。
// %q 的转义规则（\" \\ \n）与 Prometheus 文本格式对齐，不再叠加手工转义。
func renderLabelsWithLe(labelValues []string, le string) string {
	// 约定：labelValues = [k1, v1, k2, v2, ...]（见 server 侧组装）。
	var parts []string
	for i := 0; i+1 < len(labelValues); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", labelValues[i], labelValues[i+1]))
	}
	if le != "" {
		parts = append(parts, fmt.Sprintf("le=%q", le))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabel label 值转义（\ " \n）。
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// formatFixed 千分定点 → 字符串（保留最多 3 位小数，去尾零）。
func formatFixed(raw int64) string {
	v := float64(raw) / 1000
	s := fmt.Sprintf("%.3f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// formatFloat 桶上界输出（Go 的 %g 与 Prometheus 习惯对齐）。
func formatFloat(f float64) string {
	return fmt.Sprintf("%g", f)
}

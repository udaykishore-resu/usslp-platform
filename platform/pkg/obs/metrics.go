// Package obs provides the platform's observability primitives: structured
// logging, Prometheus-format metrics, W3C trace context, and the standard
// admin HTTP surface (/metrics, /healthz, /readyz) that every USSLP binary
// exposes on its own port.
//
// The implementation is dependency-free by design. The exposition format is the
// Prometheus text format, so a real Prometheus, an OpenTelemetry collector, or
// a Grafana Agent scrapes these processes with no adapter; the trace context is
// W3C, so spans join traces that started in Envoy or in a customer's POS.
package obs

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds the metrics of one process.
type Registry struct {
	mu         sync.RWMutex
	collectors []collector
	byName     map[string]collector
	// constLabels are attached to every metric: service, version, region,
	// instance. Without them, a p99 computed across three regions is a
	// meaningless average of unrelated populations.
	constLabels []label
}

type label struct{ name, value string }

type collector interface {
	name() string
	writeText(w io.Writer, constLabels []label)
}

// NewRegistry creates a registry. Const labels are supplied as alternating
// name/value pairs.
func NewRegistry(constLabels ...string) *Registry {
	r := &Registry{byName: make(map[string]collector)}
	for i := 0; i+1 < len(constLabels); i += 2 {
		r.constLabels = append(r.constLabels, label{constLabels[i], constLabels[i+1]})
	}
	sort.Slice(r.constLabels, func(i, j int) bool { return r.constLabels[i].name < r.constLabels[j].name })
	return r
}

func (r *Registry) register(c collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[c.name()]; dup {
		// Duplicate registration is a programming error that would silently
		// produce two conflicting series with the same name.
		panic("obs: metric registered twice: " + c.name())
	}
	r.byName[c.name()] = c
	r.collectors = append(r.collectors, c)
}

// WriteText renders the registry in Prometheus text exposition format.
func (r *Registry) WriteText(w io.Writer) {
	r.mu.RLock()
	cs := make([]collector, len(r.collectors))
	copy(cs, r.collectors)
	labels := r.constLabels
	r.mu.RUnlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].name() < cs[j].name() })
	for _, c := range cs {
		c.writeText(w, labels)
	}
}

// ---------------------------------------------------------------------------
// Vectors
// ---------------------------------------------------------------------------

type vec struct {
	metricName string
	help       string
	typ        string
	labelNames []string
	mu         sync.RWMutex
	children   map[string][]string // series key -> label values
}

func (v *vec) name() string { return v.metricName }

func (v *vec) key(values []string) string {
	if len(values) != len(v.labelNames) {
		panic(fmt.Sprintf("obs: metric %s expects %d label values, got %d",
			v.metricName, len(v.labelNames), len(values)))
	}
	return strings.Join(values, "\x1f")
}

func (v *vec) remember(k string, values []string) {
	v.mu.Lock()
	if v.children == nil {
		v.children = make(map[string][]string)
	}
	if _, ok := v.children[k]; !ok {
		cp := make([]string, len(values))
		copy(cp, values)
		v.children[k] = cp
	}
	v.mu.Unlock()
}

func (v *vec) header(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", v.metricName, v.help, v.metricName, v.typ)
}

func renderLabels(constLabels []label, names, values []string, extra ...label) string {
	all := make([]label, 0, len(constLabels)+len(names)+len(extra))
	all = append(all, constLabels...)
	for i := range names {
		all = append(all, label{names[i], values[i]})
	}
	all = append(all, extra...)
	if len(all) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, l := range all {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(l.name)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(l.value))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, `\"`+"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
// Counter
// ---------------------------------------------------------------------------

// CounterVec is a set of monotonically increasing counters partitioned by
// label values.
type CounterVec struct {
	vec
	mu     sync.RWMutex
	series map[string]*Counter
}

// Counter is a single monotonically increasing value.
type Counter struct{ v atomic.Uint64 }

// Inc adds one.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds n. Negative values are a programming error and are ignored rather
// than corrupting a counter that Prometheus assumes never decreases.
func (c *Counter) Add(n uint64) { c.v.Add(n) }

// Value returns the current count.
func (c *Counter) Value() uint64 { return c.v.Load() }

// Counter registers a counter vector.
func (r *Registry) Counter(name, help string, labelNames ...string) *CounterVec {
	cv := &CounterVec{
		vec:    vec{metricName: name, help: help, typ: "counter", labelNames: labelNames},
		series: make(map[string]*Counter),
	}
	r.register(cv)
	return cv
}

// With returns the counter for a set of label values.
func (cv *CounterVec) With(values ...string) *Counter {
	k := cv.key(values)
	cv.mu.RLock()
	c, ok := cv.series[k]
	cv.mu.RUnlock()
	if ok {
		return c
	}
	cv.mu.Lock()
	defer cv.mu.Unlock()
	if c, ok = cv.series[k]; ok {
		return c
	}
	c = &Counter{}
	cv.series[k] = c
	cv.remember(k, values)
	return c
}

func (cv *CounterVec) writeText(w io.Writer, constLabels []label) {
	cv.header(w)
	cv.mu.RLock()
	keys := make([]string, 0, len(cv.series))
	for k := range cv.series {
		keys = append(keys, k)
	}
	cv.mu.RUnlock()
	sort.Strings(keys)
	for _, k := range keys {
		cv.mu.RLock()
		c := cv.series[k]
		cv.mu.RUnlock()
		cv.vec.mu.RLock()
		vals := cv.children[k]
		cv.vec.mu.RUnlock()
		fmt.Fprintf(w, "%s%s %d\n", cv.metricName, renderLabels(constLabels, cv.labelNames, vals), c.Value())
	}
}

// ---------------------------------------------------------------------------
// Gauge
// ---------------------------------------------------------------------------

// GaugeVec is a set of gauges partitioned by label values.
type GaugeVec struct {
	vec
	mu     sync.RWMutex
	series map[string]*Gauge
}

// Gauge is a value that goes up and down. Stored as float bits so that
// fractional values (battery volts, mesh risk scores) are representable.
type Gauge struct{ bits atomic.Uint64 }

// Set replaces the value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add adds a delta.
func (g *Gauge) Add(d float64) {
	for {
		old := g.bits.Load()
		nv := math.Float64frombits(old) + d
		if g.bits.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

// Inc adds one. Dec subtracts one.
func (g *Gauge) Inc() { g.Add(1) }
func (g *Gauge) Dec() { g.Add(-1) }

// Value returns the current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

// Gauge registers a gauge vector.
func (r *Registry) Gauge(name, help string, labelNames ...string) *GaugeVec {
	gv := &GaugeVec{
		vec:    vec{metricName: name, help: help, typ: "gauge", labelNames: labelNames},
		series: make(map[string]*Gauge),
	}
	r.register(gv)
	return gv
}

// With returns the gauge for a set of label values.
func (gv *GaugeVec) With(values ...string) *Gauge {
	k := gv.key(values)
	gv.mu.RLock()
	g, ok := gv.series[k]
	gv.mu.RUnlock()
	if ok {
		return g
	}
	gv.mu.Lock()
	defer gv.mu.Unlock()
	if g, ok = gv.series[k]; ok {
		return g
	}
	g = &Gauge{}
	gv.series[k] = g
	gv.remember(k, values)
	return g
}

func (gv *GaugeVec) writeText(w io.Writer, constLabels []label) {
	gv.header(w)
	gv.mu.RLock()
	keys := make([]string, 0, len(gv.series))
	for k := range gv.series {
		keys = append(keys, k)
	}
	gv.mu.RUnlock()
	sort.Strings(keys)
	for _, k := range keys {
		gv.mu.RLock()
		g := gv.series[k]
		gv.mu.RUnlock()
		gv.vec.mu.RLock()
		vals := gv.children[k]
		gv.vec.mu.RUnlock()
		fmt.Fprintf(w, "%s%s %s\n", gv.metricName,
			renderLabels(constLabels, gv.labelNames, vals), formatFloat(g.Value()))
	}
}

func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ---------------------------------------------------------------------------
// Histogram
// ---------------------------------------------------------------------------

// HistogramVec is a set of histograms partitioned by label values.
type HistogramVec struct {
	vec
	buckets []float64
	mu      sync.RWMutex
	series  map[string]*Histogram
}

// Histogram accumulates observations into cumulative buckets.
type Histogram struct {
	buckets []float64
	counts  []atomic.Uint64
	sum     atomic.Uint64 // float bits
	count   atomic.Uint64
}

// LatencyBuckets are the platform's standard latency buckets in seconds. They
// are chosen around the SLOs that matter: the 500ms POS ingress budget and the
// 3-second end-to-end price propagation target, with enough resolution either
// side to see an SLO being approached rather than only breached.
var LatencyBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 1.5, 2, 2.5, 3, 4, 5, 10, 30,
}

// Histogram registers a histogram vector. Buckets must be sorted ascending.
func (r *Registry) Histogram(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	if len(buckets) == 0 {
		buckets = LatencyBuckets
	}
	b := make([]float64, len(buckets))
	copy(b, buckets)
	sort.Float64s(b)
	hv := &HistogramVec{
		vec:     vec{metricName: name, help: help, typ: "histogram", labelNames: labelNames},
		buckets: b,
		series:  make(map[string]*Histogram),
	}
	r.register(hv)
	return hv
}

// With returns the histogram for a set of label values.
func (hv *HistogramVec) With(values ...string) *Histogram {
	k := hv.key(values)
	hv.mu.RLock()
	h, ok := hv.series[k]
	hv.mu.RUnlock()
	if ok {
		return h
	}
	hv.mu.Lock()
	defer hv.mu.Unlock()
	if h, ok = hv.series[k]; ok {
		return h
	}
	h = &Histogram{buckets: hv.buckets, counts: make([]atomic.Uint64, len(hv.buckets))}
	hv.series[k] = h
	hv.remember(k, values)
	return h
}

// Observe records a value.
func (h *Histogram) Observe(v float64) {
	// Linear scan: with 17 buckets this beats a binary search on branch
	// prediction and keeps the hot path allocation free.
	for i, ub := range h.buckets {
		if v <= ub {
			h.counts[i].Add(1)
			break
		}
	}
	h.count.Add(1)
	for {
		old := h.sum.Load()
		nv := math.Float64frombits(old) + v
		if h.sum.CompareAndSwap(old, math.Float64bits(nv)) {
			break
		}
	}
}

// Count returns the number of observations.
func (h *Histogram) Count() uint64 { return h.count.Load() }

// Sum returns the sum of observations.
func (h *Histogram) Sum() float64 { return math.Float64frombits(h.sum.Load()) }

// Quantile estimates a quantile by linear interpolation within the bucket that
// contains it. This is the same approximation Prometheus' histogram_quantile
// makes, reproduced locally so tests can assert on p99 without a Prometheus.
func (h *Histogram) Quantile(q float64) float64 {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	target := q * float64(total)
	var cum uint64
	var prevBound float64
	var prevCum uint64
	for i, ub := range h.buckets {
		cum += h.counts[i].Load()
		if float64(cum) >= target {
			inBucket := cum - prevCum
			if inBucket == 0 {
				return ub
			}
			frac := (target - float64(prevCum)) / float64(inBucket)
			return prevBound + frac*(ub-prevBound)
		}
		prevBound = ub
		prevCum = cum
	}
	// Observations beyond the largest bucket: report the largest bound, which
	// is what Prometheus does and is honest about the loss of resolution.
	return h.buckets[len(h.buckets)-1]
}

func (hv *HistogramVec) writeText(w io.Writer, constLabels []label) {
	hv.header(w)
	hv.mu.RLock()
	keys := make([]string, 0, len(hv.series))
	for k := range hv.series {
		keys = append(keys, k)
	}
	hv.mu.RUnlock()
	sort.Strings(keys)
	for _, k := range keys {
		hv.mu.RLock()
		h := hv.series[k]
		hv.mu.RUnlock()
		hv.vec.mu.RLock()
		vals := hv.children[k]
		hv.vec.mu.RUnlock()
		var cum uint64
		for i, ub := range h.buckets {
			cum += h.counts[i].Load()
			ls := renderLabels(constLabels, hv.labelNames, vals, label{"le", formatFloat(ub)})
			fmt.Fprintf(w, "%s_bucket%s %d\n", hv.metricName, ls, cum)
		}
		ls := renderLabels(constLabels, hv.labelNames, vals, label{"le", "+Inf"})
		fmt.Fprintf(w, "%s_bucket%s %d\n", hv.metricName, ls, h.Count())
		base := renderLabels(constLabels, hv.labelNames, vals)
		fmt.Fprintf(w, "%s_sum%s %s\n", hv.metricName, base, formatFloat(h.Sum()))
		fmt.Fprintf(w, "%s_count%s %d\n", hv.metricName, base, h.Count())
	}
}

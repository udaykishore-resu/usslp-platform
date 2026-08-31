package obs

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ---------------------------------------------------------------------------
// W3C trace context
//
// The single most valuable diagnostic in USSLP is one trace that spans the POS
// webhook, the gateway, the event log, the Label Service, the MQTT hop, the SGU
// queue, the SEC, the Zigbee mesh and the E-Ink refresh — because that is the
// only way to answer "which of the nine hops ate the three-second budget".
//
// Trace context is therefore propagated through HTTP headers, event-bus headers
// and MQTT payload metadata using the W3C traceparent format, so that the
// context survives crossing from HTTP to Kafka to MQTT to a Zigbee frame.
// ---------------------------------------------------------------------------

// SpanContext identifies a span within a trace.
type SpanContext struct {
	TraceID string // 32 hex chars
	SpanID  string // 16 hex chars
	Sampled bool
}

// Valid reports whether the context is well formed.
func (sc SpanContext) Valid() bool {
	if len(sc.TraceID) != 32 || len(sc.SpanID) != 16 {
		return false
	}
	if _, err := hex.DecodeString(sc.TraceID); err != nil {
		return false
	}
	if _, err := hex.DecodeString(sc.SpanID); err != nil {
		return false
	}
	return sc.TraceID != strings.Repeat("0", 32) && sc.SpanID != strings.Repeat("0", 16)
}

// TraceParent renders the context as a W3C traceparent header value.
func (sc SpanContext) TraceParent() string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + sc.TraceID + "-" + sc.SpanID + "-" + flags
}

// ParseTraceParent parses a W3C traceparent header. An unparseable header
// yields an invalid context rather than an error: a malformed header from an
// upstream system must start a new trace, never drop the request.
func ParseTraceParent(v string) SpanContext {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) != 4 || parts[0] != "00" {
		return SpanContext{}
	}
	sc := SpanContext{TraceID: parts[1], SpanID: parts[2], Sampled: parts[3] == "01" || parts[3] == "03"}
	if !sc.Valid() {
		return SpanContext{}
	}
	return sc
}

// NewRootContext starts a fresh trace.
func NewRootContext(sampled bool) SpanContext {
	return SpanContext{TraceID: canon.NewTraceID(), SpanID: canon.NewSpanID(), Sampled: sampled}
}

// ---------------------------------------------------------------------------
// Spans
// ---------------------------------------------------------------------------

// Span is one timed operation.
type Span struct {
	Name       string
	Ctx        SpanContext
	ParentID   string
	Service    string
	StartTime  time.Time
	EndTime    time.Time
	Attributes map[string]string
	Events     []SpanEvent
	Status     string // "OK" | "ERROR"
	Error      string

	tracer   *Tracer
	mu       sync.Mutex
	finished bool
}

// SpanEvent is a timestamped annotation within a span — the E-Ink refresh
// starting, a mesh retry, a cache miss.
type SpanEvent struct {
	Name string
	At   time.Time
	Attr map[string]string
}

// Duration returns the elapsed time; for an unfinished span, time so far.
func (s *Span) Duration() time.Duration {
	if s.EndTime.IsZero() {
		return time.Since(s.StartTime)
	}
	return s.EndTime.Sub(s.StartTime)
}

// SetAttr adds a key/value attribute.
func (s *Span) SetAttr(k, v string) *Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Attributes == nil {
		s.Attributes = map[string]string{}
	}
	s.Attributes[k] = v
	return s
}

// SetAttrInt adds an integer attribute.
func (s *Span) SetAttrInt(k string, v int64) *Span { return s.SetAttr(k, fmt.Sprint(v)) }

// AddEvent annotates the span.
func (s *Span) AddEvent(name string, kv ...string) *Span {
	attr := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		attr[kv[i]] = kv[i+1]
	}
	s.mu.Lock()
	s.Events = append(s.Events, SpanEvent{Name: name, At: time.Now(), Attr: attr})
	s.mu.Unlock()
	return s
}

// Fail marks the span as errored.
func (s *Span) Fail(err error) *Span {
	if err == nil {
		return s
	}
	s.mu.Lock()
	s.Status = "ERROR"
	s.Error = err.Error()
	s.mu.Unlock()
	return s
}

// End finishes the span and hands it to the exporter. It is safe to call
// twice; the second call is a no-op, which makes `defer span.End()` alongside
// an early explicit End harmless.
func (s *Span) End() {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.EndTime = time.Now()
	if s.Status == "" {
		s.Status = "OK"
	}
	s.mu.Unlock()
	if s.tracer != nil {
		s.tracer.export(s)
	}
}

// Exporter receives finished spans.
type Exporter interface{ Export(*Span) }

// Tracer creates spans for one service.
type Tracer struct {
	service   string
	exporters []Exporter
	// SampleRate is the fraction of root traces recorded, expressed as 1 in N.
	// Price updates are always sampled regardless (see StartAlwaysSampled):
	// at 52,000 updates per second, head sampling at 1% is right for volume but
	// wrong for the one update a regulator asks about.
	sampleOneIn uint64
	counter     uint64
	mu          sync.Mutex
}

// NewTracer builds a tracer. sampleOneIn <= 1 records everything.
func NewTracer(service string, sampleOneIn uint64, exporters ...Exporter) *Tracer {
	if sampleOneIn == 0 {
		sampleOneIn = 1
	}
	return &Tracer{service: service, sampleOneIn: sampleOneIn, exporters: exporters}
}

type spanKey struct{}

// Start begins a span, continuing the trace in ctx if there is one.
func (t *Tracer) Start(ctx context.Context, name string) (context.Context, *Span) {
	return t.start(ctx, name, false)
}

// StartAlwaysSampled begins a span that is recorded regardless of sample rate.
// Used on the price path, provisioning and OTA, where every trace is evidence.
func (t *Tracer) StartAlwaysSampled(ctx context.Context, name string) (context.Context, *Span) {
	return t.start(ctx, name, true)
}

func (t *Tracer) start(ctx context.Context, name string, force bool) (context.Context, *Span) {
	parent := SpanContextFrom(ctx)
	sc := SpanContext{SpanID: canon.NewSpanID()}
	var parentID string
	if parent.Valid() {
		sc.TraceID = parent.TraceID
		sc.Sampled = parent.Sampled || force
		parentID = parent.SpanID
	} else {
		sc.TraceID = canon.NewTraceID()
		sc.Sampled = force || t.shouldSample()
	}
	s := &Span{
		Name:      name,
		Ctx:       sc,
		ParentID:  parentID,
		Service:   t.service,
		StartTime: time.Now(),
		tracer:    t,
	}
	return context.WithValue(ctx, spanKey{}, s), s
}

func (t *Tracer) shouldSample() bool {
	if t.sampleOneIn <= 1 {
		return true
	}
	t.mu.Lock()
	t.counter++
	c := t.counter
	t.mu.Unlock()
	return c%t.sampleOneIn == 0
}

func (t *Tracer) export(s *Span) {
	if !s.Ctx.Sampled {
		return
	}
	for _, e := range t.exporters {
		e.Export(s)
	}
}

// SpanFrom returns the active span in ctx, or nil.
func SpanFrom(ctx context.Context) *Span {
	if s, ok := ctx.Value(spanKey{}).(*Span); ok {
		return s
	}
	return nil
}

// SpanContextFrom returns the trace context carried in ctx.
func SpanContextFrom(ctx context.Context) SpanContext {
	if s := SpanFrom(ctx); s != nil {
		return s.Ctx
	}
	if sc, ok := ctx.Value(remoteKey{}).(SpanContext); ok {
		return sc
	}
	return SpanContext{}
}

type remoteKey struct{}

// WithRemoteContext attaches an inbound trace context to ctx so that the next
// Start continues the caller's trace.
func WithRemoteContext(ctx context.Context, sc SpanContext) context.Context {
	if !sc.Valid() {
		return ctx
	}
	return context.WithValue(ctx, remoteKey{}, sc)
}

// ---------------------------------------------------------------------------
// Exporters
// ---------------------------------------------------------------------------

// LogExporter writes finished spans to the structured log, where the OTel
// collector's `filelog` receiver turns them back into spans and forwards them
// to the trace backend (deploy/observability/otel/otel-collector.yaml). In
// development they are readable as they stand, which is worth more than a UI
// when debugging a mesh hop.
//
// # Why spans are not ordinary log lines
//
// This exporter used to emit at Debug on the service's own logger. That made
// the span log invisible in production for a reason nobody looking at the
// tracing code would see: config.LoadService defaults LOG_LEVEL to "info" when
// USSLP_ENVIRONMENT is "prod", so the collector's bridge received nothing at
// all and no trace backend had any data. Raising the level to "info" instead
// would have been worse — it makes span volume a function of how verbose an
// operator wants their *application* logs, in both directions: silenced by
// LOG_LEVEL=warn during an incident, and flooded to 52,000 lines a second on
// the price path by LOG_LEVEL=debug during an investigation.
//
// Spans are telemetry with their own volume story, so they get their own
// controls. NewRuntime builds a separate logger for this exporter whose
// severity is SpanLogLevel, independent of LogLevel; nothing an operator does
// to the application log level changes whether traces reach the backend.
//
// # Why there is a second sampler
//
// Level alone is not enough, because Tracer.StartAlwaysSampled deliberately
// bypasses head sampling on the price path, provisioning and OTA — where every
// trace is evidence. At 52,000 price updates a second those are all the spans.
// OneIn thins what is *written to the log bridge* without touching what is
// *recorded*: the tracer still samples as configured, spans still reach
// MemoryExporter and any future OTLP exporter at the head-sampled rate, and
// only this stopgap's output is bounded.
//
// The thinning is keyed on the trace ID rather than counted per span, so a
// trace is either wholly present in the log or wholly absent. Half a trace is
// worse than none: the backend renders it as a broken tree and the reader
// draws conclusions from missing spans.
//
// When an OTLP exporter lands, both knobs and this exporter go with the bridge.
type LogExporter struct {
	// Log is the span logger. Nil disables the exporter entirely.
	Log *Logger
	// Level is the severity spans are emitted at. The zero value is
	// slog.LevelInfo, because a span log that the level gate suppresses is the
	// defect this type exists to have fixed.
	Level slog.Level
	// OneIn writes one trace in N. Zero or 1 writes every sampled trace.
	OneIn uint64
}

// Export implements Exporter.
func (e LogExporter) Export(s *Span) {
	if e.Log == nil || !e.writes(s.Ctx.TraceID) {
		return
	}
	fields := []any{
		"span", s.Name,
		"trace_id", s.Ctx.TraceID,
		"span_id", s.Ctx.SpanID,
		"duration_ms", s.Duration().Milliseconds(),
		"status", s.Status,
	}
	if s.ParentID != "" {
		fields = append(fields, "parent_span_id", s.ParentID)
	}
	if s.Error != "" {
		fields = append(fields, "error", s.Error)
	}
	for k, v := range s.Attributes {
		fields = append(fields, "attr."+k, v)
	}
	e.Log.Log(context.Background(), e.Level, "span", fields...)
}

// writes reports whether this trace is in the export sample.
//
// FNV-1a over the trace ID: deterministic, so every span of one trace decides
// the same way in every process it touches, and uniform enough over 32 hex
// characters that "one trace in N" means what it says.
func (e LogExporter) writes(traceID string) bool {
	if e.OneIn <= 1 {
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(traceID))
	return h.Sum64()%e.OneIn == 0
}

// MemoryExporter retains spans in memory. Tests assert on the shape of a trace
// with it: that a price update produced exactly the nine expected spans, in
// order, inside the latency budget.
type MemoryExporter struct {
	mu    sync.Mutex
	spans []*Span
	limit int
}

// NewMemoryExporter creates an exporter retaining at most limit spans.
func NewMemoryExporter(limit int) *MemoryExporter {
	if limit <= 0 {
		limit = 10000
	}
	return &MemoryExporter{limit: limit}
}

// Export implements Exporter.
func (m *MemoryExporter) Export(s *Span) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.spans) >= m.limit {
		copy(m.spans, m.spans[1:])
		m.spans = m.spans[:len(m.spans)-1]
	}
	m.spans = append(m.spans, s)
}

// Spans returns a snapshot.
func (m *MemoryExporter) Spans() []*Span {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Span, len(m.spans))
	copy(out, m.spans)
	return out
}

// Trace returns the spans belonging to one trace, in start order.
func (m *MemoryExporter) Trace(traceID string) []*Span {
	var out []*Span
	for _, s := range m.Spans() {
		if s.Ctx.TraceID == traceID {
			out = append(out, s)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartTime.Before(out[j-1].StartTime); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Reset discards retained spans.
func (m *MemoryExporter) Reset() {
	m.mu.Lock()
	m.spans = nil
	m.mu.Unlock()
}

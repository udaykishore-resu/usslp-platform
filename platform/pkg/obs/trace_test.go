package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// spanLines runs fn against a LogExporter writing into a buffer at the given
// application log level, and returns the decoded span records it produced.
func spanLines(t *testing.T, appLevel string, exp LogExporter, fn func(e LogExporter)) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	exp.Log = NewLogger(LogConfig{Service: "test", Level: appLevel, Output: &buf})
	fn(exp)

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("span log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func finished(tr *Tracer, name string) *Span {
	_, s := tr.StartAlwaysSampled(context.Background(), name)
	s.SetAttr("hop", "sec-to-label")
	s.End()
	return s
}

// The regression this exporter exists to have fixed: spans were emitted at
// Debug on the service logger, so a production service at LOG_LEVEL=info wrote
// none of them and the collector's filelog bridge received nothing.
func TestSpansAreVisibleAtTheProductionLogLevel(t *testing.T) {
	tr := NewTracer("test", 1)
	got := spanLines(t, "info", LogExporter{}, func(e LogExporter) {
		e.Export(finished(tr, "label.price.apply"))
	})
	if len(got) != 1 {
		t.Fatalf("wrote %d span lines at the production log level, want 1", len(got))
	}
	if got[0]["msg"] != "span" {
		t.Errorf("msg = %v, want \"span\" — the collector's filter keys on it", got[0]["msg"])
	}
	if got[0]["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", got[0]["level"])
	}
	if got[0]["attr.hop"] != "sec-to-label" {
		t.Errorf("attributes did not survive: %v", got[0])
	}
}

// An explicit level is honoured, so an operator who wants spans out of the
// default log stream can push them to debug without editing code.
func TestSpanLogLevelIsIndependentOfTheApplicationLevel(t *testing.T) {
	tr := NewTracer("test", 1)
	got := spanLines(t, "info", LogExporter{Level: slog.LevelDebug}, func(e LogExporter) {
		e.Export(finished(tr, "label.price.apply"))
	})
	if len(got) != 0 {
		t.Fatalf("a debug-level span reached an info-level writer: %v", got)
	}
}

// Head sampling still decides what is recorded: an unsampled span must never
// reach an exporter at all.
func TestUnsampledSpansAreNotExported(t *testing.T) {
	mem := NewMemoryExporter(16)
	// One trace in a very large N: the first Start is not the Nth, so it is
	// not sampled, and StartAlwaysSampled is not used.
	tr := NewTracer("test", 1<<20, mem)
	_, s := tr.Start(context.Background(), "label.price.apply")
	s.End()
	if n := len(mem.Spans()); n != 0 {
		t.Fatalf("exported %d unsampled spans, want 0", n)
	}
}

// OneIn thins whole traces, never individual spans: half a trace renders in a
// backend as a broken tree and is worse than none.
func TestSpanLogThinningKeepsWholeTraces(t *testing.T) {
	tr := NewTracer("test", 1)
	exp := LogExporter{OneIn: 8}

	// Find a trace the sampler writes, and one it does not, then check every
	// span of each is treated the same way.
	var kept, dropped string
	for i := 0; i < 512 && (kept == "" || dropped == ""); i++ {
		_, root := tr.StartAlwaysSampled(context.Background(), "root")
		id := root.Ctx.TraceID
		root.End()
		if exp.writes(id) && kept == "" {
			kept = id
		} else if !exp.writes(id) && dropped == "" {
			dropped = id
		}
	}
	if kept == "" || dropped == "" {
		t.Fatalf("could not find both a written and a thinned trace in 512 tries")
	}
	for _, id := range []string{kept, dropped} {
		first := exp.writes(id)
		for i := 0; i < 32; i++ {
			if exp.writes(id) != first {
				t.Fatalf("trace %s: the export decision is not stable across its spans", id)
			}
		}
	}
}

// The default must bound the always-sampled price path rather than let it set
// the volume: at 52,000 updates a second, 1-in-1 is a log bill.
func TestSpanLogThinningRateIsRoughlyOneInN(t *testing.T) {
	tr := NewTracer("test", 1)
	const oneIn, traces = 10, 4000
	exp := LogExporter{OneIn: oneIn}

	written := 0
	for i := 0; i < traces; i++ {
		_, s := tr.StartAlwaysSampled(context.Background(), "root")
		id := s.Ctx.TraceID
		s.End()
		if exp.writes(id) {
			written++
		}
	}
	want := traces / oneIn
	// Generous bounds: this asserts the order of magnitude, not the hash's
	// distribution. A rate that had silently become 1-in-1 or 0-in-N fails.
	if written < want/2 || written > want*2 {
		t.Errorf("wrote %d of %d traces at OneIn=%d, want roughly %d", written, traces, oneIn, want)
	}
}

// Zero and one both mean "write everything", so a caller that leaves the field
// alone on a low-volume service is not silently sampled.
func TestSpanLogThinningOffByDefaultOnTheExporter(t *testing.T) {
	tr := NewTracer("test", 1)
	for _, oneIn := range []uint64{0, 1} {
		exp := LogExporter{OneIn: oneIn}
		for i := 0; i < 64; i++ {
			_, s := tr.StartAlwaysSampled(context.Background(), "root")
			id := s.Ctx.TraceID
			s.End()
			if !exp.writes(id) {
				t.Fatalf("OneIn=%d thinned a trace", oneIn)
			}
		}
	}
}

func TestSpanExportersHonourTheEnvironment(t *testing.T) {
	t.Setenv("USSLP_SPAN_LOG_LEVEL", "")
	t.Setenv("USSLP_SPAN_LOG_ONE_IN", "")

	exps := spanExporters(RuntimeConfig{Service: "test"})
	if len(exps) != 1 {
		t.Fatalf("default configuration produced %d exporters, want 1", len(exps))
	}
	le, ok := exps[0].(LogExporter)
	if !ok {
		t.Fatalf("exporter is %T, want LogExporter", exps[0])
	}
	if le.Level != slog.LevelInfo {
		t.Errorf("default span level is %v, want INFO — anything higher is suppressed in production", le.Level)
	}
	if le.OneIn != spanLogOneInDefault {
		t.Errorf("default thinning is %d, want %d", le.OneIn, spanLogOneInDefault)
	}

	t.Setenv("USSLP_SPAN_LOG_ONE_IN", "5")
	exps = spanExporters(RuntimeConfig{Service: "test"})
	if le := exps[0].(LogExporter); le.OneIn != 5 {
		t.Errorf("USSLP_SPAN_LOG_ONE_IN=5 gave OneIn=%d", le.OneIn)
	}

	// An explicit config field beats the environment: the environment is the
	// fallback that spares nine main packages from threading these through.
	exps = spanExporters(RuntimeConfig{Service: "test", SpanLogOneIn: 7, SpanLogLevel: "debug"})
	le = exps[0].(LogExporter)
	if le.OneIn != 7 || le.Level != slog.LevelDebug {
		t.Errorf("explicit configuration was overridden: OneIn=%d level=%v", le.OneIn, le.Level)
	}

	for _, off := range []string{"off", "none", "OFF"} {
		if exps := spanExporters(RuntimeConfig{Service: "test", SpanLogLevel: off}); exps != nil {
			t.Errorf("SpanLogLevel=%q still produced %d exporters", off, len(exps))
		}
	}
}

func TestParseLevelFallsBackToInfo(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn,
		"error": slog.LevelError,
		"info":  slog.LevelInfo, "": slog.LevelInfo,
		// A typo must not silence a service.
		"quiet": slog.LevelInfo, " inf o": slog.LevelInfo,
	} {
		if got := ParseLevel(name); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", name, got, want)
		}
	}
}

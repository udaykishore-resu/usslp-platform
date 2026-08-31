package obs

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Runtime is the standard bootstrap every USSLP binary shares: a logger, a
// metrics registry, a tracer, a health registry and an admin server, all
// labelled with the same service identity.
//
// Having one of these means a new service is genuinely a few dozen lines, and —
// more importantly — that every service reports the same metric names, so a
// single dashboard and a single set of alert rules cover the whole fleet.
type Runtime struct {
	Service  string
	Version  string
	Region   string
	Log      *Logger
	Metrics  *Registry
	Tracer   *Tracer
	Health   *Health
	Admin    *AdminServer
	Standard *StandardMetrics

	startedAt time.Time
	stopFns   []func(context.Context) error
}

// RuntimeConfig configures the bootstrap.
type RuntimeConfig struct {
	Service     string
	Version     string
	Region      string
	LogLevel    string
	LogFormat   string
	AdminAddr   string
	EnablePprof bool
	// TraceSampleOneIn records one root trace in N. The price path overrides
	// this and is always sampled.
	TraceSampleOneIn uint64

	// SpanLogLevel is the severity finished spans are written at, independent
	// of LogLevel. "off" disables the span log; "" takes USSLP_SPAN_LOG_LEVEL,
	// then defaults to "info".
	//
	// It is separate from LogLevel because spans reach the trace backend
	// through the collector's filelog bridge, and tying them to the
	// application log level meant production (LOG_LEVEL=info) received no
	// traces at all. See the LogExporter comment for the whole argument.
	SpanLogLevel string

	// SpanLogOneIn writes one trace in N to the span log, on top of the
	// tracer's head sampling. Zero takes USSLP_SPAN_LOG_ONE_IN, then defaults
	// to spanLogOneInDefault; 1 writes every sampled trace.
	//
	// It exists because StartAlwaysSampled bypasses head sampling on the price
	// path, which at 52,000 updates a second is every span. What is *recorded*
	// is unaffected — only what this stopgap writes to the log.
	SpanLogOneIn uint64
}

// Defaults for the span log, applied when neither RuntimeConfig nor the
// environment sets them.
//
// They live here rather than in pkg/config because every binary already calls
// NewRuntime and none of them pass these through; putting the fallback here is
// what makes the fix reach all of them without touching nine main packages.
// obs.BuildVersion already reads USSLP_VERSION the same way.
const (
	spanLogLevelDefault = "info"
	// One trace in a hundred, matching the production TRACE_SAMPLE_ONE_IN in
	// deploy/helm/usslp/templates/configmap.yaml. Ordinary traces are already
	// head-sampled at that rate, so this bounds the force-sampled ones to the
	// same order of magnitude rather than letting the price path set the
	// volume on its own.
	spanLogOneInDefault = 100
)

// NewRuntime assembles the observability stack and starts the admin server.
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if cfg.Version == "" {
		cfg.Version = BuildVersion()
	}
	log := NewLogger(LogConfig{
		Service: cfg.Service, Version: cfg.Version, Region: cfg.Region,
		Level: cfg.LogLevel, Format: cfg.LogFormat,
	})
	host, _ := os.Hostname()
	reg := NewRegistry(
		"service", cfg.Service,
		"version", cfg.Version,
		"region", firstNonEmpty(cfg.Region, "local"),
		"instance", firstNonEmpty(host, "unknown"),
	)
	health := NewHealth()
	admin, err := NewAdminServer(AdminConfig{
		Addr: cfg.AdminAddr, Registry: reg, Health: health, Log: log,
		EnablePprof: cfg.EnablePprof,
		BuildInfo: map[string]string{
			"service": cfg.Service, "version": cfg.Version,
			"go": runtime.Version(), "region": cfg.Region,
		},
	})
	if err != nil {
		return nil, err
	}
	rt := &Runtime{
		Service: cfg.Service, Version: cfg.Version, Region: cfg.Region,
		Log: log, Metrics: reg, Health: health, Admin: admin,
		startedAt: time.Now(),
	}
	rt.Tracer = NewTracer(cfg.Service, cfg.TraceSampleOneIn, spanExporters(cfg)...)
	rt.Standard = NewStandardMetrics(reg)
	admin.Start()
	rt.startRuntimeCollector()
	log.Info("service starting", "admin_addr", admin.Addr(), "pid", os.Getpid())
	return rt, nil
}

// spanExporters builds the tracer's exporter list.
//
// The span logger is deliberately a *different* *Logger from the service's,
// carrying the same identity, format and output but its own severity, so that
// the application log level and the span log level move independently. Writing
// to the same stream matters: the collector's filelog receiver reads the pod's
// log file, so a span on a private writer would need a volume, a sidecar and a
// deployment change to reach the same place.
func spanExporters(cfg RuntimeConfig) []Exporter {
	level := firstNonEmpty(cfg.SpanLogLevel, os.Getenv("USSLP_SPAN_LOG_LEVEL"), spanLogLevelDefault)
	if strings.EqualFold(level, "off") || strings.EqualFold(level, "none") {
		return nil
	}
	oneIn := cfg.SpanLogOneIn
	if oneIn == 0 {
		oneIn = spanLogOneInDefault
		if v, err := strconv.ParseUint(os.Getenv("USSLP_SPAN_LOG_ONE_IN"), 10, 64); err == nil && v > 0 {
			oneIn = v
		}
	}
	return []Exporter{LogExporter{
		Log: NewLogger(LogConfig{
			Service: cfg.Service, Version: cfg.Version, Region: cfg.Region,
			Level: level, Format: cfg.LogFormat,
		}),
		Level: ParseLevel(level),
		OneIn: oneIn,
	}}
}

// OnShutdown registers a cleanup function, run in reverse order of
// registration so that dependencies close after their dependents.
func (r *Runtime) OnShutdown(fn func(context.Context) error) {
	r.stopFns = append(r.stopFns, fn)
}

// Ready marks start-up complete and the process eligible for traffic.
func (r *Runtime) Ready() {
	r.Health.SetReady(true)
	r.Log.Info("service ready", "startup_ms", time.Since(r.startedAt).Milliseconds())
}

// WaitForSignal blocks until SIGINT or SIGTERM, then runs shutdown hooks with
// the given grace period.
//
// The order matters: stop accepting new work, drain in-flight work, then close
// connections. Kubernetes sends SIGTERM and waits terminationGracePeriodSeconds
// before SIGKILL, so the grace period here must be shorter than the one in the
// Deployment or draining is pointless.
func (r *Runtime) WaitForSignal(grace time.Duration) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	sig := <-ch
	r.Log.Info("shutdown signal received", "signal", sig.String())
	r.Shutdown(grace)
}

// Shutdown runs the registered hooks.
func (r *Runtime) Shutdown(grace time.Duration) {
	// Fail readiness first so the load balancer stops sending work while the
	// in-flight requests drain.
	r.Health.SetReady(false)
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	for i := len(r.stopFns) - 1; i >= 0; i-- {
		if err := r.stopFns[i](ctx); err != nil {
			r.Log.Error("shutdown hook failed", "error", err)
		}
	}
	if err := r.Admin.Shutdown(ctx); err != nil {
		r.Log.Error("admin shutdown failed", "error", err)
	}
	r.Log.Info("shutdown complete", "uptime_s", int64(time.Since(r.startedAt).Seconds()))
}

func (r *Runtime) startRuntimeCollector() {
	goroutines := r.Metrics.Gauge("usslp_go_goroutines", "Number of goroutines")
	heap := r.Metrics.Gauge("usslp_go_heap_bytes", "Heap bytes in use")
	gcPause := r.Metrics.Gauge("usslp_go_gc_pause_p99_seconds", "Recent GC pause, 99th percentile")
	uptime := r.Metrics.Gauge("usslp_process_uptime_seconds", "Process uptime")
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			goroutines.With().Set(float64(runtime.NumGoroutine()))
			heap.With().Set(float64(ms.HeapAlloc))
			gcPause.With().Set(float64(ms.PauseNs[(ms.NumGC+255)%256]) / 1e9)
			uptime.With().Set(time.Since(r.startedAt).Seconds())
		}
	}()
}

// BuildVersion reports the version stamped into the binary by the linker, or
// the VCS revision Go embeds automatically.
func BuildVersion() string {
	if v := os.Getenv("USSLP_VERSION"); v != "" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return s.Value[:7]
			}
		}
	}
	return "dev"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Standard metrics
// ---------------------------------------------------------------------------

// StandardMetrics are the series every service reports under the same names.
// A dashboard built on these works for a service that did not exist when the
// dashboard was written.
type StandardMetrics struct {
	// RequestsTotal counts inbound requests by transport, operation and outcome.
	RequestsTotal *CounterVec
	// RequestDuration is the inbound request latency histogram.
	RequestDuration *HistogramVec
	// EventsPublished and EventsConsumed track event-bus throughput.
	EventsPublished *CounterVec
	EventsConsumed  *CounterVec
	// EventHandlerDuration is per-event processing time; the difference between
	// this and consumer lag distinguishes "we are slow" from "we are behind".
	EventHandlerDuration *HistogramVec
	// EventRetries and EventsDeadLettered are the two numbers that tell you a
	// deployment introduced a poison message.
	EventRetries       *CounterVec
	EventsDeadLettered *CounterVec
	// ConsumerLag is exported per topic and partition for the autoscaler.
	ConsumerLag *GaugeVec
}

// NewStandardMetrics registers the standard series.
func NewStandardMetrics(r *Registry) *StandardMetrics {
	return &StandardMetrics{
		RequestsTotal: r.Counter("usslp_requests_total",
			"Inbound requests", "transport", "operation", "outcome"),
		RequestDuration: r.Histogram("usslp_request_duration_seconds",
			"Inbound request latency", LatencyBuckets, "transport", "operation"),
		EventsPublished: r.Counter("usslp_events_published_total",
			"Events published to the stream", "topic", "event_type"),
		EventsConsumed: r.Counter("usslp_events_consumed_total",
			"Events consumed from the stream", "topic", "group", "outcome"),
		EventHandlerDuration: r.Histogram("usslp_event_handler_duration_seconds",
			"Event handler latency", LatencyBuckets, "topic", "group"),
		EventRetries: r.Counter("usslp_event_retries_total",
			"Event handler retries", "topic", "group"),
		EventsDeadLettered: r.Counter("usslp_events_dead_lettered_total",
			"Events routed to the dead-letter stream", "topic", "group", "reason"),
		ConsumerLag: r.Gauge("usslp_consumer_lag_records",
			"Un-consumed records", "topic", "group", "partition"),
	}
}

// ObserveRequest records one inbound request.
func (m *StandardMetrics) ObserveRequest(transport, operation string, err error, d time.Duration) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	m.RequestsTotal.With(transport, operation, outcome).Inc()
	m.RequestDuration.With(transport, operation).Observe(d.Seconds())
}

// SetLag publishes consumer lag for one partition.
func (m *StandardMetrics) SetLag(topic, group string, partition int, lag int64) {
	m.ConsumerLag.With(topic, group, strconv.Itoa(partition)).Set(float64(lag))
}

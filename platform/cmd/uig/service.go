package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/adapters/clover"
	"github.com/usslp/usslp/platform/internal/uig/adapters/filedrop"
	"github.com/usslp/usslp/platform/internal/uig/adapters/generic"
	"github.com/usslp/usslp/platform/internal/uig/adapters/lightspeed"
	"github.com/usslp/usslp/platform/internal/uig/adapters/ncr"
	"github.com/usslp/usslp/platform/internal/uig/adapters/oracle"
	"github.com/usslp/usslp/platform/internal/uig/adapters/sap"
	"github.com/usslp/usslp/platform/internal/uig/adapters/shopify"
	"github.com/usslp/usslp/platform/internal/uig/adapters/square"
	"github.com/usslp/usslp/platform/internal/uig/deliveries"
	"github.com/usslp/usslp/platform/internal/uig/gateway"
	"github.com/usslp/usslp/platform/internal/uig/pipeline"
	"github.com/usslp/usslp/platform/internal/uig/reliability"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/config"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// ServiceName is the identity every metric, log line and event source carries.
const ServiceName = "uig"

// Config is the gateway's configuration.
//
// Everything here is resolvable from a file as well as an environment variable
// (contract §8), because the same binary runs in Kubernetes with a projected
// secret and on a Store Gateway Unit with a mounted one.
type Config struct {
	Service config.ServiceConfig
	// Addr is the ingest listener. Business traffic and the admin surface are
	// on separate ports so the gateway can shed customer load while remaining
	// scrapeable and debuggable — which is exactly when you need it to be.
	Addr string
	// EventLogDir backs the embedded event stream when no external broker is
	// configured. A single-store deployment runs entirely on this.
	EventLogDir string
	// StateDir backs the idempotency guard and the delivery store.
	StateDir string
	// BindingsFile is a JSON array of bindings loaded at start-up.
	BindingsFile string
	// OperatorToken authenticates the operator endpoints.
	OperatorToken string
	// DedupeWindow is the idempotency window; contract §6 fixes it at 24 hours
	// and it is configurable only so a test or a migration can shorten it.
	DedupeWindow time.Duration
	// DeliveryRetention is how long quarantined bodies are kept.
	DeliveryRetention time.Duration
	// MaxBodyBytes bounds an inbound delivery.
	MaxBodyBytes int64
	// ShutdownGrace bounds the drain.
	ShutdownGrace time.Duration
	// ReadTimeout and WriteTimeout bound a slow client.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// DefaultRateLimit applies to bindings that set none.
	DefaultRateLimit float64
	DefaultBurst     int
	// BreakerThreshold and BreakerCooldown tune outbound circuit breakers.
	BreakerThreshold int
	BreakerCooldown  time.Duration
}

// LoadConfig reads the gateway's configuration, reporting every problem at once
// rather than one per restart.
func LoadConfig() (Config, error) {
	l := config.New("USSLP")
	cfg := Config{
		Service:           config.LoadService(l, ServiceName),
		Addr:              l.String("UIG_ADDR", ":8080"),
		EventLogDir:       l.String("UIG_EVENTLOG_DIR", ""),
		StateDir:          l.String("UIG_STATE_DIR", ""),
		BindingsFile:      l.String("UIG_BINDINGS_FILE", ""),
		OperatorToken:     l.String("UIG_OPERATOR_TOKEN", ""),
		DedupeWindow:      l.Duration("UIG_DEDUPE_WINDOW", idem.DefaultWindow),
		DeliveryRetention: l.Duration("UIG_DELIVERY_RETENTION", deliveries.DefaultRetention),
		MaxBodyBytes:      int64(l.Int("UIG_MAX_BODY_BYTES", int(gateway.DefaultMaxBodyBytes))),
		ShutdownGrace:     l.Duration("UIG_SHUTDOWN_GRACE", 20*time.Second),
		ReadTimeout:       l.Duration("UIG_READ_TIMEOUT", gateway.DefaultReadTimeout),
		WriteTimeout:      l.Duration("UIG_WRITE_TIMEOUT", 20*time.Second),
		DefaultRateLimit:  float64(l.Int("UIG_DEFAULT_RATE_PER_SECOND", int(reliability.DefaultRate))),
		DefaultBurst:      l.Int("UIG_DEFAULT_BURST", reliability.DefaultBurst),
		BreakerThreshold:  l.Int("UIG_BREAKER_THRESHOLD", 5),
		BreakerCooldown:   l.Duration("UIG_BREAKER_COOLDOWN", 15*time.Second),
	}
	if cfg.EventLogDir == "" {
		cfg.EventLogDir = cfg.Service.DataDir + "/eventlog"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = cfg.Service.DataDir + "/uig-state"
	}
	return cfg, l.Err()
}

// Service is the assembled gateway: everything the process owns, in the order
// it must be torn down.
type Service struct {
	cfg      Config
	rt       *obs.Runtime
	kv       *kvstore.Store
	bus      eventbus.Bus
	pipe     *pipeline.Pipeline
	gw       *gateway.Gateway
	srv      *http.Server
	ln       net.Listener
	watchers []*filedrop.Watcher

	wg       sync.WaitGroup
	cancel   context.CancelFunc
	drained  chan struct{}
	stopOnce sync.Once
}

// NewService assembles the gateway.
//
// The order matters and is the reverse of the shutdown order: storage, then the
// event stream, then the pipeline that writes to both, then the HTTP surface
// that feeds the pipeline, then the pollers that also feed it. Nothing accepts
// work until everything it depends on exists.
func NewService(cfg Config) (*Service, error) {
	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service:     ServiceName,
		Version:     cfg.Service.Version,
		Region:      cfg.Service.Region,
		LogLevel:    cfg.Service.LogLevel,
		LogFormat:   cfg.Service.LogFormat,
		AdminAddr:   cfg.Service.AdminAddr,
		EnablePprof: cfg.Service.EnablePprof,
		// The price path is always sampled: a 3-second end-to-end budget cannot
		// be debugged from a one-in-a-thousand trace.
		TraceSampleOneIn: uint64(cfg.Service.TraceSample),
	})
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, rt: rt, drained: make(chan struct{})}

	s.kv, err = kvstore.OpenWith(kvstore.Options{
		Dir: cfg.StateDir,
		// SyncEvery rather than SyncAlways: the durable record of a price
		// change is the event stream, not this store, and paying an fsync per
		// idempotency claim inside a 50ms budget would buy nothing. What is
		// held here — guard entries and quarantined bodies — is recoverable
		// from the stream and from the retailer respectively.
		Sync:            kvstore.SyncEvery,
		Registry:        rt.Metrics,
		MetricNamespace: "uig_state",
	})
	if err != nil {
		rt.Shutdown(time.Second)
		return nil, fmt.Errorf("uig: opening state store: %w", err)
	}

	bus, err := eventlog.Open(cfg.EventLogDir,
		eventlog.WithMetrics(rt.Metrics),
		eventlog.WithLogger(rt.Log),
	)
	if err != nil {
		s.kv.Close()
		rt.Shutdown(time.Second)
		return nil, fmt.Errorf("uig: opening event log: %w", err)
	}
	s.bus = bus
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if err := bus.EnsureStreams(ctx, canon.StreamPriceUpdates, canon.StreamPOSIngress, canon.StreamDLQ); err != nil {
		s.closeStorage()
		rt.Shutdown(time.Second)
		cancel()
		return nil, fmt.Errorf("uig: ensuring streams: %w", err)
	}

	guardBackend, err := idem.NewKVBackend(s.kv, "uig/idem/")
	if err != nil {
		s.closeStorage()
		rt.Shutdown(time.Second)
		cancel()
		return nil, err
	}
	guard, err := idem.New(guardBackend, idem.WithWindow(cfg.DedupeWindow))
	if err != nil {
		s.closeStorage()
		rt.Shutdown(time.Second)
		cancel()
		return nil, err
	}
	store, err := deliveries.New(s.kv, deliveries.Options{
		Prefix:    "uig/deliveries/",
		Retention: cfg.DeliveryRetention,
	})
	if err != nil {
		s.closeStorage()
		rt.Shutdown(time.Second)
		cancel()
		return nil, err
	}

	breakers := reliability.NewBreakerSet(reliability.BreakerConfig{
		FailureThreshold: cfg.BreakerThreshold,
		Cooldown:         cfg.BreakerCooldown,
	})
	registry := adapter.NewRegistry()
	if err := RegisterAdapters(registry, breakers); err != nil {
		s.closeStorage()
		rt.Shutdown(time.Second)
		cancel()
		return nil, err
	}
	bindings := adapter.NewBindingStore(registry)
	if cfg.BindingsFile != "" {
		raw, err := os.ReadFile(cfg.BindingsFile)
		if err != nil {
			s.closeStorage()
			rt.Shutdown(time.Second)
			cancel()
			return nil, fmt.Errorf("uig: reading bindings file: %w", err)
		}
		if err := bindings.LoadJSON(raw); err != nil {
			s.closeStorage()
			rt.Shutdown(time.Second)
			cancel()
			return nil, fmt.Errorf("uig: loading bindings: %w", err)
		}
	}

	metrics := pipeline.NewMetrics(rt.Metrics)
	metrics.BindingsConfigured.With().Set(float64(bindings.Count()))

	s.pipe, err = pipeline.New(pipeline.Config{
		Registry:   registry,
		Bindings:   bindings,
		Guard:      guard,
		Bus:        bus,
		Deliveries: store,
		Limiter:    reliability.NewLimiter(),
		Breakers:   breakers,
		Metrics:    metrics,
		Log:        rt.Log,
		Tracer:     rt.Tracer,
		Region:     canon.Region(cfg.Service.Region),
	})
	if err != nil {
		s.closeStorage()
		rt.Shutdown(time.Second)
		cancel()
		return nil, err
	}

	s.gw, err = gateway.New(gateway.Config{
		Pipeline:      s.pipe,
		OperatorToken: cfg.OperatorToken,
		MaxBodyBytes:  cfg.MaxBodyBytes,
		Log:           rt.Log,
	})
	if err != nil {
		s.closeStorage()
		rt.Shutdown(time.Second)
		cancel()
		return nil, err
	}

	s.srv = &http.Server{
		Handler:           s.gw.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		// A generous idle timeout keeps a POS's keep-alive connections open
		// between bursts; re-handshaking TLS on every webhook would spend more
		// of the latency budget than the ingest itself.
		IdleTimeout: 120 * time.Second,
		ErrorLog:    nil,
	}
	s.ln, err = net.Listen("tcp", cfg.Addr)
	if err != nil {
		s.closeStorage()
		rt.Shutdown(time.Second)
		cancel()
		return nil, fmt.Errorf("uig: binding %s: %w", cfg.Addr, err)
	}

	if err := s.startWatchers(ctx, bindings, rt.Log); err != nil {
		s.ln.Close()
		s.closeStorage()
		rt.Shutdown(time.Second)
		cancel()
		return nil, err
	}

	s.registerHealth(bindings, metrics, breakers)
	return s, nil
}

// RegisterAdapters installs every adapter this build speaks.
//
// It is exported so that a test, or a future single-tenant edge build, can
// assemble the same registry without duplicating the list — a registry that
// drifts from the binary's actual capabilities is how a binding silently stops
// resolving after a refactor.
func RegisterAdapters(reg *adapter.Registry, breakers *reliability.BreakerSet) error {
	for _, a := range []adapter.Adapter{
		shopify.New(),
		square.New(),
		ncr.New(),
		sap.New(),
		oracle.New(),
		lightspeed.New(),
		// Clover is the one adapter that calls back out to its POS, so it is
		// handed the shared breaker set: its circuits then appear on the same
		// health endpoint as everything else.
		clover.New(nil, breakers),
		filedrop.New(),
		generic.New(),
	} {
		if err := reg.Register(a); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) startWatchers(ctx context.Context, bindings *adapter.BindingStore, log *obs.Logger) error {
	for _, b := range allBindings(bindings) {
		if b.Adapter != filedrop.Name {
			continue
		}
		opts, ok := b.CompiledOptions().(*filedrop.Options)
		if !ok || opts == nil {
			continue
		}
		wc, ok := opts.WatchConfigFor(b.TenantID, b.ID, log)
		if !ok {
			continue
		}
		wtr, err := filedrop.NewWatcher(wc, s.pipe)
		if err != nil {
			return fmt.Errorf("uig: watcher for binding %s/%s: %w", b.TenantID, b.ID, err)
		}
		s.watchers = append(s.watchers, wtr)
		s.wg.Add(1)
		go func(wtr *filedrop.Watcher) {
			defer s.wg.Done()
			if err := wtr.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("uig: file watcher stopped", "error", err)
			}
		}(wtr)
	}
	return nil
}

// allBindings collects every tenant's bindings. The binding store is indexed by
// tenant because that is what the request path needs; start-up is the one place
// that wants them all.
func allBindings(bindings *adapter.BindingStore) []*adapter.Binding {
	var out []*adapter.Binding
	for _, t := range bindingTenants(bindings) {
		out = append(out, bindings.List(t)...)
	}
	return out
}

func (s *Service) registerHealth(bindings *adapter.BindingStore, metrics *pipeline.Metrics, breakers *reliability.BreakerSet) {
	// Dependency checks are registered on readiness only (contract §7): a
	// storage blip must remove this pod from the load balancer, never restart
	// it, or a five-second wobble becomes a cluster-wide restart storm.
	s.rt.Health.Register("state-store", func(ctx context.Context) error {
		if _, err := s.kv.Has([]byte("uig/healthz")); err != nil {
			return err
		}
		return nil
	})
	s.rt.Health.Register("event-stream", func(ctx context.Context) error {
		// EnsureStreams is idempotent and cheap; calling it is a genuine
		// round trip through the bus rather than a flag lookup, which is the
		// difference between a readiness check and a lie.
		return s.bus.EnsureStreams(ctx, canon.StreamPriceUpdates, canon.StreamPOSIngress)
	})
	s.rt.Health.Register("bindings", func(context.Context) error {
		n := bindings.Count()
		metrics.BindingsConfigured.With().Set(float64(n))
		metrics.ObserveBreakers(breakers)
		if n == 0 {
			// A gateway with no bindings cannot serve anyone, and the
			// overwhelmingly likely cause is a configuration mount that did not
			// arrive. Failing readiness keeps it out of the load balancer
			// instead of letting it 404 a retailer's whole price book.
			return errors.New("no bindings are configured")
		}
		return nil
	})
}

// Addr reports the ingest listener's address, which is what a test needs when
// it asked for port zero.
func (s *Service) Addr() string {
	if s.ln == nil {
		return s.cfg.Addr
	}
	return s.ln.Addr().String()
}

// Runtime exposes the observability stack.
func (s *Service) Runtime() *obs.Runtime { return s.rt }

// Pipeline exposes the ingest pipeline.
func (s *Service) Pipeline() *pipeline.Pipeline { return s.pipe }

// Serve starts accepting traffic and marks the service ready.
func (s *Service) Serve() error {
	s.rt.Log.Info("uig: ingest listening",
		"addr", s.Addr(), "watchers", len(s.watchers),
		"max_body_bytes", s.cfg.MaxBodyBytes)
	s.rt.Ready()
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown drains in the order that loses the least work.
//
//  1. Fail readiness, so the load balancer stops sending new deliveries.
//  2. Stop the file watchers, so no new file is picked up mid-drain.
//  3. Close the HTTP server, letting in-flight requests finish.
//  4. Drain the pipeline's bookkeeping queue, so deliveries the platform
//     already acknowledged are still filed for support.
//  5. Close the event stream and the state store last, because everything
//     above may still be writing to them.
func (s *Service) Shutdown(grace time.Duration) {
	s.stopOnce.Do(func() {
		s.rt.Health.SetReady(false)
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()

		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()

		if err := s.srv.Shutdown(ctx); err != nil {
			s.rt.Log.Error("uig: http shutdown", "error", err)
		}
		if err := s.pipe.Close(); err != nil {
			s.rt.Log.Error("uig: pipeline drain", "error", err)
		}
		s.closeStorage()
		s.rt.Shutdown(grace)
		close(s.drained)
	})
	<-s.drained
}

func (s *Service) closeStorage() {
	if s.bus != nil {
		if err := s.bus.Close(); err != nil && !errors.Is(err, eventbus.ErrClosed) {
			s.rt.Log.Error("uig: closing event log", "error", err)
		}
	}
	if s.kv != nil {
		if err := s.kv.Close(); err != nil && !errors.Is(err, kvstore.ErrClosed) {
			s.rt.Log.Error("uig: closing state store", "error", err)
		}
	}
}

// bindingTenants is a small helper that keeps the tenant enumeration in one
// place; the binding store deliberately does not expose its index, because
// nothing on the request path should ever iterate tenants.
func bindingTenants(bindings *adapter.BindingStore) []canon.TenantID {
	seen := map[canon.TenantID]bool{}
	var out []canon.TenantID
	for _, t := range bindings.Tenants() {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// describeConfig renders the effective configuration for the start-up log,
// with nothing secret in it.
func describeConfig(cfg Config) []any {
	return []any{
		"addr", cfg.Addr,
		"admin_addr", cfg.Service.AdminAddr,
		"eventlog_dir", cfg.EventLogDir,
		"state_dir", cfg.StateDir,
		"bindings_file", cfg.BindingsFile,
		"dedupe_window", cfg.DedupeWindow.String(),
		"delivery_retention", cfg.DeliveryRetention.String(),
		"operator_api", strings.TrimSpace(cfg.OperatorToken) != "",
		"latency_budget", pipeline.LatencyBudget.String(),
	}
}

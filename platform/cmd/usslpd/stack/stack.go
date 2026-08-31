package stack

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// Stack is the assembled platform.
//
// It is safe to use from several goroutines once Start has returned. Before
// that it belongs to whoever called New.
type Stack struct {
	cfg Config

	// bootRT is the runtime that owns the things no single service owns: the
	// shared event log's metrics, the start-up log, and the control surface.
	bootRT *obs.Runtime

	log        *eventlog.Log
	hierarchy  *pki.Hierarchy
	authority  *pki.PriceAuthority
	keyRing    *pki.KeyRing
	cloud      *mqtt.Broker
	cloudAddr  string
	cloudURL   string
	cloudSvcs  *cloudServices
	stores     []*Store
	control    *http.Server
	controlLn  net.Listener
	bootDur    time.Duration
	startedAt  time.Time
	tempDir    bool
	closers    []closer
	closeOnce  sync.Once
	stopped    chan struct{}
	bgCtx      context.Context
	background context.CancelFunc

	// fwPub/fwPriv are the firmware signing pair. See firmwareKeys.
	fwPub  ed25519.PublicKey
	fwPriv ed25519.PrivateKey

	book       *priceBook
	deliveries *deliveryMonitor
	clientOnce sync.Once
	client     *http.Client
}

// backgroundCtx is the context every long-running goroutine in the process
// takes. It outlives Start's own deadline — a start-up timeout must not cancel
// the consumers it just started — and is cancelled by Stop.
func (s *Stack) backgroundCtx() context.Context {
	if s.bgCtx == nil {
		return context.Background()
	}
	return s.bgCtx
}

// closer is one teardown step, named so a failure says what would not shut
// down rather than only that something did not.
type closer struct {
	name string
	fn   func(context.Context) error
}

// New validates the configuration and prepares the data directory. It binds no
// port and starts no goroutine; call Start.
func New(cfg Config) (*Stack, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("usslpd: creating the data directory %s: %w", cfg.DataDir, err)
	}
	return &Stack{
		cfg: cfg, tempDir: cfg.Ephemeral,
		stopped: make(chan struct{}), book: newPriceBook(),
		deliveries: newDeliveryMonitor(),
	}, nil
}

// Config returns the effective configuration, defaults filled in.
func (s *Stack) Config() Config { return s.cfg }

// Start brings the whole platform up in dependency order and blocks until it is
// ready to take a price change.
//
// The order is the design:
//
//  1. the certificate hierarchy and the price authority, because a Label
//     Service with no signing key and a controller with no key ring cannot move
//     a price between them and there is no point starting either;
//  2. the shared event log, provisioned with the whole catalogue, because every
//     cloud service either produces to it or consumes from it;
//  3. the cloud MQTT broker, before any service that dials it;
//  4. the cloud services, Device Registry first — the Label Service builds its
//     entire fan-out directory from the registry's `device-events` stream from
//     offset zero, so a registry that publishes before the Label Service is
//     subscribed is still correct, but the reverse ordering would leave the
//     projection racing its own input;
//  5. per store: the gateway (its own broker first, then its cloud bridge),
//     then zero-touch provisioning of every device through the real
//     certificate path, then the planogram;
//  6. a readiness gate on the Label Service's directory, so nothing starts a
//     label fleet the cloud cannot yet address;
//  7. the controllers, which subscribe to their zones and immediately receive
//     whatever retained prices already exist;
//  8. the label fleet's radio: the paced simulation clock and the active
//     windows.
//
// Reversing any adjacent pair produces a specific, diagnosable failure, which
// is why each step waits rather than being fired off in parallel.
func (s *Stack) Start(ctx context.Context) error {
	started := time.Now()
	s.startedAt = started

	ctx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
	defer cancel()
	bg, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	s.bgCtx, s.background = bg, stopBG

	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service: "usslpd", Region: s.cfg.Region,
		LogLevel: s.cfg.LogLevel, LogFormat: s.cfg.LogFormat,
		AdminAddr: s.addr(offsetPort(s.cfg.Ports.Control, 1000)),
	})
	if err != nil {
		return fmt.Errorf("usslpd: observability: %w", err)
	}
	s.bootRT = rt
	s.push("usslpd runtime", func(context.Context) error { rt.Shutdown(5 * time.Second); return nil })

	if err := s.startPKI(); err != nil {
		return s.abort(err)
	}
	if err := s.startEventLog(); err != nil {
		return s.abort(err)
	}
	if err := s.startCloudBroker(); err != nil {
		return s.abort(err)
	}
	if err := s.startCloudServices(bg); err != nil {
		return s.abort(err)
	}
	if err := s.startStores(bg); err != nil {
		return s.abort(err)
	}
	// The shelves get their opening prices before the banner is printed, so a
	// runtime that reports itself ready has demonstrably delivered a price to
	// every label rather than merely bound every port.
	if err := s.seedOpeningPrices(ctx); err != nil {
		return s.abort(err)
	}
	if err := s.startControl(); err != nil {
		return s.abort(err)
	}

	s.bootDur = time.Since(started)
	rt.Ready()
	rt.Log.Info("usslpd ready",
		"boot_ms", s.bootDur.Milliseconds(),
		"tenants", len(s.cfg.Tenants), "stores", len(s.stores),
		"labels", s.LabelCount())
	return nil
}

// abort tears down whatever came up before a start-up step failed, so a failed
// Start leaves no listener bound and no goroutine running.
func (s *Stack) abort(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = s.Stop(ctx)
	return cause
}

// push registers a teardown step. They run in reverse order.
func (s *Stack) push(name string, fn func(context.Context) error) {
	s.closers = append(s.closers, closer{name: name, fn: fn})
}

// Stop shuts everything down in reverse dependency order and is idempotent.
//
// The order matters as much as it does on the way up, and for the same reason
// stated in the individual services: the label fleet stops before its
// controllers, the controllers before the gateway that serves them, the
// gateway before the broker the cloud publishes to, and the event log last of
// all, because every consumer above it may still be committing an offset.
func (s *Stack) Stop(ctx context.Context) error {
	var firstErr error
	s.closeOnce.Do(func() {
		if s.background != nil {
			s.background()
		}
		for i := len(s.closers) - 1; i >= 0; i-- {
			c := s.closers[i]
			if err := c.fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
				if s.bootRT != nil {
					s.bootRT.Log.Error("shutdown step failed", "step", c.name, "error", err)
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("usslpd: shutting down %s: %w", c.name, err)
				}
			}
		}
		if s.tempDir && s.cfg.DataDir != "" {
			_ = os.RemoveAll(s.cfg.DataDir)
		}
		close(s.stopped)
	})
	return firstErr
}

// Done is closed once Stop has finished.
func (s *Stack) Done() <-chan struct{} { return s.stopped }

// BootDuration is how long Start took.
func (s *Stack) BootDuration() time.Duration { return s.bootDur }

// ---------------------------------------------------------------------------
// Start-up steps
// ---------------------------------------------------------------------------

// startPKI creates or reloads the certificate hierarchy and the tenant price
// authority.
//
// The hierarchy is created with pki.TestProfile rather than the production one.
// The difference is the key algorithm — P-256 throughout instead of RSA-4096
// for the root and the intermediates — and it is the difference between a
// hierarchy that bootstraps in about ten milliseconds and one that spends the
// better part of a minute generating RSA moduli. The certificate *structure* is
// identical: the same six authorities, the same path-length constraints, the
// same key usages, the same verification. A production key ceremony runs
// pki.Bootstrap with the production profile once, on an air-gapped machine; a
// laptop that regenerates a whole hierarchy on every `make run` should not.
func (s *Stack) startPKI() error {
	dir := filepath.Join(s.cfg.DataDir, "pki")
	log := s.bootRT.Log

	h, err := pki.Load(dir, pki.LoadOptions{Logger: log})
	switch {
	case err == nil:
		s.hierarchy = h
	default:
		profile := pki.TestProfile()
		h, err = pki.Bootstrap(pki.BootstrapConfig{Profile: &profile, Logger: log})
		if err != nil {
			return fmt.Errorf("usslpd: bootstrapping the certificate hierarchy: %w", err)
		}
		if err := h.Save(dir); err != nil {
			return fmt.Errorf("usslpd: saving the certificate hierarchy to %s: %w", dir, err)
		}
		s.hierarchy = h
	}

	authDir := filepath.Join(dir, "price-authority")
	auth, err := pki.LoadPriceAuthority(authDir, pki.PriceAuthorityConfig{Logger: log})
	if err != nil {
		auth, err = pki.NewPriceAuthority(pki.PriceAuthorityConfig{Logger: log})
		if err != nil {
			return fmt.Errorf("usslpd: creating the price authority: %w", err)
		}
		if err := auth.Save(authDir); err != nil {
			return fmt.Errorf("usslpd: saving the price authority to %s: %w", authDir, err)
		}
	}
	s.authority = auth

	// The key ring is derived from the authority rather than configured
	// alongside it. That is the whole point: the controllers verify against
	// exactly the keys the Label Service signs with, so the attestation path is
	// load-bearing and a mismatch is impossible to introduce by configuration.
	ring, err := auth.KeyRing()
	if err != nil {
		return fmt.Errorf("usslpd: publishing the price-authority key ring: %w", err)
	}
	s.keyRing = ring
	log.Info("price authority ready", "kid", auth.KeyID(), "ring_keys", ring.Len())
	return nil
}

// startEventLog opens the one log every service shares.
//
// Sharing it is the reason this binary exists. pkg/eventlog holds consumer
// group state in memory, so the multi-process dev profile gives each service
// its own directory and the UIG's price-updates records never reach the Label
// Service's consumer (deploy/README.md §2). One *Log value handed to every
// constructor makes the cross-service stream real.
func (s *Stack) startEventLog() error {
	dir := filepath.Join(s.cfg.DataDir, "eventlog")
	// SyncInterval rather than SyncAlways. The production default is
	// SyncAlways because USSLP acknowledges a price change to a POS before the
	// label moves, and losing an acknowledged record is a compliance incident.
	// In this deployment shape the whole platform is one process on one
	// machine: a crash takes the log, the services and the store with it, and
	// there is no surviving component whose belief the log could contradict.
	// Twenty milliseconds bounds the loss to less than one hop of the latency
	// budget while keeping a 1,000-update benchmark from becoming an fsync
	// benchmark.
	lg, err := eventlog.Open(dir,
		eventlog.WithMetrics(s.bootRT.Metrics),
		eventlog.WithLogger(s.bootRT.Log),
		eventlog.WithSync(eventlog.SyncInterval(20*time.Millisecond)),
		// 8 MiB segments rather than 64: retention reclaims space in useful
		// increments for a directory that may be a temporary one.
		eventlog.WithSegmentBytes(8<<20),
	)
	if err != nil {
		return fmt.Errorf("usslpd: opening the shared event log at %s: %w", dir, err)
	}
	s.log = lg
	s.push("event log", func(context.Context) error { return lg.Close() })

	streams := devStreams(s.cfg.DevPartitions)
	if err := lg.EnsureStreams(context.Background(), streams...); err != nil {
		return fmt.Errorf("usslpd: provisioning streams: %w", err)
	}
	s.bootRT.Log.Info("shared event log open",
		"dir", dir, "streams", len(streams), "partitions_per_stream", s.cfg.DevPartitions)
	return nil
}

// startCloudBroker binds the cloud MQTT broker.
func (s *Stack) startCloudBroker() error {
	b := mqtt.NewBroker(mqtt.Options{
		Addr:   s.addr(s.cfg.Ports.CloudMQTT),
		Logger: s.bootRT.Log,
	})
	addr, err := b.Start()
	if err != nil {
		return fmt.Errorf("usslpd: starting the cloud MQTT broker: %w", err)
	}
	s.cloud = b
	s.cloudAddr = addr.String()
	s.cloudURL = "tcp://" + s.cloudAddr
	s.push("cloud broker", func(ctx context.Context) error { return b.Shutdown(ctx) })
	s.bootRT.Log.Info("cloud broker listening", "addr", s.cloudAddr)
	return nil
}

// ---------------------------------------------------------------------------
// Introspection
// ---------------------------------------------------------------------------

// Stores returns the running stores, ordered by identifier.
func (s *Stack) Stores() []*Store {
	out := append([]*Store(nil), s.stores...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Store returns one store by identifier.
func (s *Stack) Store(id canon.StoreID) (*Store, bool) {
	for _, st := range s.stores {
		if st.ID == id {
			return st, true
		}
	}
	return nil, false
}

// LabelCount is the total number of simulated labels across every store.
func (s *Stack) LabelCount() int {
	n := 0
	for _, st := range s.stores {
		n += st.LabelCount()
	}
	return n
}

// KeyRing is the published price-authority key ring the controllers verify
// against. Tests use it to prove that a tampered price fails verification for
// the reason they think it does.
func (s *Stack) KeyRing() *pki.KeyRing { return s.keyRing }

// PriceAuthority is the signing key the Label Service attests with.
func (s *Stack) PriceAuthority() *pki.PriceAuthority { return s.authority }

// Hierarchy is the certificate hierarchy devices are enrolled against.
func (s *Stack) Hierarchy() *pki.Hierarchy { return s.hierarchy }

// EventLog is the shared stream. It is exposed so a test can subscribe to
// `audit-log` or `label-delivery` and assert on what the platform recorded,
// rather than inferring it from a side effect.
func (s *Stack) EventLog() *eventlog.Log { return s.log }

// CloudBrokerURL is the address the store gateways bridge to.
func (s *Stack) CloudBrokerURL() string { return s.cloudURL }

// addr renders a listen address for a port, with 0 meaning "any".
//
// Everything binds to the loopback interface. usslpd has no authentication in
// front of its control surface and its API Gateway is bootstrapped with a
// printed owner key; binding it to 0.0.0.0 would put that on the network.
func (s *Stack) addr(port int) string {
	if port <= 0 {
		return "127.0.0.1:0"
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// offsetPort shifts a configured port, preserving "let the operating system
// choose". Zero plus an offset is a port number, which is a different and much
// worse thing than zero.
func offsetPort(base, by int) int {
	if base <= 0 {
		return 0
	}
	return base + by
}

// listen binds a TCP listener, translating the "address already in use" case
// into a message that names the service rather than only the port.
func (s *Stack) listen(name string, port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", s.addr(port))
	if err != nil {
		return nil, fmt.Errorf("usslpd: binding %s on %s: %w", name, s.addr(port), err)
	}
	return ln, nil
}

// serve runs an HTTP server on a listener and registers its shutdown.
func (s *Stack) serve(name string, ln net.Listener, h http.Handler, writeTimeout time.Duration) *http.Server {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.bootRT.Log.Error("http server stopped", "service", name, "error", err)
		}
	}()
	s.push(name+" http", func(ctx context.Context) error { return srv.Shutdown(ctx) })
	return srv
}

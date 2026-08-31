// Command device-registry runs the USSLP Device Registry: the fleet's source of
// truth for identity, placement, health and planogram assignment.
//
// # What this binary is responsible for
//
// It answers "which devices exist, where are they, and may we talk to them" for
// every other service in the platform, and it is the only component allowed to
// enrol a device. It publishes the `device-events` stream the Label Service
// builds its fan-out directory from, and it forwards the `label-telemetry`
// stream the analytics tier consumes.
//
// # Configuration
//
// Every value is read through config.Loader with the USSLP_ prefix and is also
// resolvable from NAME_FILE=/run/secrets/x, because a Store Gateway Unit takes
// its credentials from a mounted secret on a device that is not running
// Kubernetes (interface contract §8).
//
// Two settings deserve attention. USSLP_REGISTRY_PKI_DIR must point at a
// hierarchy produced by the platform's key ceremony; without it the registry
// cannot authenticate a device certificate and refuses to start, which is the
// correct behaviour — a registry that enrolled devices without verifying them
// would be worse than no registry. And USSLP_REGISTRY_ENABLE_SEED grants this
// process the ability to mint device identities for the development seeding
// endpoint; it defaults to off and must stay off anywhere real.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/adapters"
	"github.com/usslp/usslp/platform/internal/registry/adapters/httpapi"
	"github.com/usslp/usslp/platform/internal/registry/app"
	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/internal/registry/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/config"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/pki"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "device-registry: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.New("USSLP")

	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service:          "device-registry",
		Region:           cfg.String("REGION", "local"),
		LogLevel:         cfg.String("LOG_LEVEL", "info"),
		LogFormat:        cfg.String("LOG_FORMAT", "json"),
		AdminAddr:        cfg.String("REGISTRY_ADMIN_ADDR", "127.0.0.1:9101"),
		EnablePprof:      cfg.Bool("ENABLE_PPROF", false),
		TraceSampleOneIn: uint64(cfg.Int("TRACE_SAMPLE_ONE_IN", 100)),
	})
	if err != nil {
		return fmt.Errorf("observability runtime: %w", err)
	}
	log := rt.Log

	// The durable stores. SyncEvery rather than SyncAlways: the registry's
	// write path is provisioning and planogram uploads, not the price hot path,
	// and losing the last 200 ms of device events on a hard kill costs a
	// re-provisioning that the device will retry on its own anyway. The publish
	// cursor means nothing is lost silently — the events are replayed.
	dataDir := cfg.String("REGISTRY_DATA_DIR", "./data/device-registry")
	kv, err := kvstore.OpenWith(kvstore.Options{
		Dir:             dataDir,
		Sync:            kvstore.SyncEvery,
		Registry:        rt.Metrics,
		MetricNamespace: "registry_kv",
	})
	if err != nil {
		return fmt.Errorf("open registry store at %s: %w", dataDir, err)
	}
	rt.OnShutdown(func(context.Context) error { return kv.Close() })

	store, err := eventstore.New(kv)
	if err != nil {
		return fmt.Errorf("open event store: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return store.Close() })

	// The event bus. pkg/eventlog is an embedded, file-backed implementation of
	// the eventbus port; a production deployment swaps it for Kafka behind the
	// same interface without touching anything above.
	bus, err := eventlog.Open(cfg.String("EVENTLOG_DIR", "./data/eventlog"),
		eventlog.WithLogger(log), eventlog.WithMetrics(rt.Metrics))
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return bus.Close() })
	if err := bus.EnsureStreams(context.Background(), canon.AllStreams()...); err != nil {
		return fmt.Errorf("ensure streams: %w", err)
	}

	// The certificate hierarchy. Provisioning is certificate-based, so a
	// registry with no hierarchy has nothing to authenticate against and must
	// not start.
	pkiDir := cfg.String("REGISTRY_PKI_DIR", "./data/pki")
	hierarchy, err := pki.Load(pkiDir, pki.LoadOptions{Logger: log})
	if err != nil {
		return fmt.Errorf("load certificate hierarchy from %s "+
			"(run the key ceremony first; the registry cannot authenticate devices without it): %w",
			pkiDir, err)
	}
	if hierarchy.HasRootKey() {
		// Loading the root key into a network-facing service is a
		// misconfiguration serious enough to refuse: the root signs
		// intermediates, and nothing this process does needs it.
		return errors.New("the loaded hierarchy carries the offline root key; " +
			"the Device Registry must never hold it")
	}

	// The MQTT link to the stores. It is optional so that the registry's HTTP
	// surface is not hostage to the messaging tier coming up first.
	var messenger ports.DeviceMessenger = adapters.NopMessenger{}
	if brokerURL := cfg.String("MQTT_URL", ""); brokerURL != "" {
		client, err := mqtt.Dial(context.Background(), msgbus.Config{
			BrokerURL:      brokerURL,
			ClientID:       cfg.String("MQTT_CLIENT_ID", "device-registry"),
			Username:       cfg.String("MQTT_USERNAME", ""),
			Password:       cfg.String("MQTT_PASSWORD", ""),
			CleanSession:   false,
			KeepAlive:      cfg.Duration("MQTT_KEEPALIVE", 30*time.Second),
			ConnectTimeout: cfg.Duration("MQTT_CONNECT_TIMEOUT", 10*time.Second),
		}, mqtt.WithClientLogger(log), mqtt.WithClientRegistry(rt.Metrics))
		if err != nil {
			return fmt.Errorf("connect to broker %s: %w", brokerURL, err)
		}
		rt.OnShutdown(func(context.Context) error { return client.Close() })
		messenger = adapters.NewMessenger(client)
		// Readiness only, never liveness: a broker blip must remove the pod
		// from the load balancer, not restart it (interface contract §7).
		rt.Health.Register("mqtt", func(context.Context) error {
			if !client.Connected() {
				return errors.New("broker link is down")
			}
			return nil
		})
	}

	svcCfg := app.Config{
		Store:     store,
		Events:    adapters.NewBusPublisher(bus),
		Messenger: messenger,
		Auth:      adapters.NewHierarchyAuthenticator(hierarchy),
		Clock:     ports.SystemClock{},
		Region:    canon.Region(rt.Region),
		Log:       log,
		Metrics:   rt.Metrics,
		Health: domain.HealthPolicy{
			BeaconInterval:      cfg.Duration("REGISTRY_BEACON_INTERVAL", 30*time.Second),
			MissedBeacons:       cfg.Int("REGISTRY_MISSED_BEACONS", 3),
			BatteryCriticalPct:  cfg.Int("REGISTRY_BATTERY_CRITICAL_PCT", 10),
			BatteryCriticalMV:   cfg.Int("REGISTRY_BATTERY_CRITICAL_MV", 2400),
			BatteryEndOfLifePct: cfg.Int("REGISTRY_BATTERY_EOL_PCT", 5),
		},
	}
	if cfg.Bool("REGISTRY_ENABLE_SEED", false) {
		log.Warn("development seeding is enabled; this process can mint device identities")
		svcCfg.Issuer = adapters.NewHierarchyIssuer(hierarchy)
	}

	svc, err := app.Open(context.Background(), svcCfg)
	if err != nil {
		return fmt.Errorf("open registry service: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return svc.Close() })

	if err := svc.SubscribeDeviceTraffic(context.Background()); err != nil {
		return fmt.Errorf("subscribe to device traffic: %w", err)
	}

	// The health sweep is what turns silence into an offline event. It is a
	// sweep on a timer rather than a timer per device because fifty million
	// timers is not a design.
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	rt.OnShutdown(func(context.Context) error { stopSweep(); return nil })
	go healthSweep(sweepCtx, svc, log, cfg.Duration("REGISTRY_SWEEP_INTERVAL", 15*time.Second))

	api := httpapi.New(svc, log, rt.Standard)
	addr := cfg.String("REGISTRY_HTTP_ADDR", "127.0.0.1:8081")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler: api.Handler(),
		// A generous read timeout: the largest legitimate request is a
		// hypermarket's whole planogram, which is megabytes over a link that
		// may be a store's broadband.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}
	rt.OnShutdown(func(ctx context.Context) error { return srv.Shutdown(ctx) })

	go func() {
		log.Info("device registry listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server stopped", "error", err)
		}
	}()

	rt.Ready()
	rt.WaitForSignal(cfg.Duration("SHUTDOWN_GRACE", 20*time.Second))
	return nil
}

// healthSweep derives device state from the evidence on a fixed interval.
func healthSweep(ctx context.Context, svc *app.Service, log *obs.Logger, every time.Duration) {
	if every <= 0 {
		every = 15 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := svc.SweepHealth(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("health sweep failed", "error", err)
				continue
			}
			if n > 0 {
				log.Info("health sweep applied lifecycle transitions", "transitions", n)
			}
		}
	}
}

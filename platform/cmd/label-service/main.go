// Command label-service runs the USSLP Label Service.
//
// The Label Service is the component that turns an accepted price change into
// per-label, cryptographically attested display updates and proves they landed.
// It consumes `price-updates`, resolves the affected labels from its own
// directory read model, signs each price with the tenant's price-authority key,
// publishes the update to the owning Shelf Edge Controller over MQTT, and
// closes the loop when the acknowledgement comes back — inside a three-second
// end-to-end budget of which it owns 120 milliseconds.
//
// Configuration is twelve-factor with the USSLP_ prefix, and every value is
// also resolvable from a file (NAME_FILE=/run/secrets/x) because the same
// binary runs on a Store Gateway Unit that has no Kubernetes and no External
// Secrets Operator.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/label"
	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
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

// serviceName is the identity every metric, log line and envelope carries.
const serviceName = "label-service"

// shutdownGrace bounds the drain.
//
// Twenty seconds is chosen against the work that has to finish, not against a
// round number: a store-wide fan-out in flight when SIGTERM arrives has up to
// 40,000 labels left to publish, and stopping halfway leaves some shelves
// showing the new promotion and some the old price. It must be shorter than the
// Deployment's terminationGracePeriodSeconds or the drain is pointless.
const shutdownGrace = 20 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "label-service: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	loader := config.New("USSLP")
	svcCfg := config.LoadService(loader, serviceName)
	httpAddr := loader.String("LABEL_HTTP_ADDR", ":8080")
	brokerURL := loader.String("MQTT_BROKER_URL", "tcp://127.0.0.1:1883")
	brokerUser := loader.String("MQTT_USERNAME", "")
	brokerPass := loader.String("MQTT_PASSWORD", "")
	pkiDir := loader.String("PRICE_AUTHORITY_DIR", "")
	defaultCurrency := loader.String("DEFAULT_CURRENCY", "USD")
	guardFactor := loader.Int("PRICE_GUARDRAIL_FACTOR", int(domain.DefaultGuardrailFactor))
	effectiveGrace := loader.Duration("PRICE_EFFECTIVE_GRACE", domain.DefaultEffectiveGrace)
	fullRefreshEvery := loader.Int("EINK_FULL_REFRESH_EVERY", domain.DefaultFullRefreshEvery)
	batchWorkers := loader.Int("BATCH_WORKERS", 0)
	tenantRate := loader.Int("TENANT_RATE_PER_SECOND", int(app.DefaultTenantRate))
	tenantBurst := loader.Int("TENANT_BURST", int(app.DefaultTenantBurst))
	schedulerTick := loader.Duration("SCHEDULER_TICK", app.DefaultScheduleTick)
	idemWindow := loader.Duration("IDEMPOTENCY_WINDOW", 24*time.Hour)
	snapshotEvery := loader.Int("SNAPSHOT_EVERY", 0)
	if err := loader.Err(); err != nil {
		return err
	}

	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service: serviceName, Version: svcCfg.Version, Region: svcCfg.Region,
		LogLevel: svcCfg.LogLevel, LogFormat: svcCfg.LogFormat,
		AdminAddr: svcCfg.AdminAddr, EnablePprof: svcCfg.EnablePprof,
		// The price path is always sampled regardless of this rate; it applies
		// to everything else the process does.
		TraceSampleOneIn: uint64(svcCfg.TraceSample),
	})
	if err != nil {
		return err
	}

	dataDir := svcCfg.DataDir
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("creating data directory %s: %w", dataDir, err)
	}

	// The event store and the read models share one key/value store, and
	// therefore one write-ahead log. That is what lets a projection commit its
	// rows and its checkpoint in a single fsync instead of two.
	kv, err := kvstore.OpenWith(kvstore.Options{
		Dir: filepath.Join(dataDir, "state"), Sync: kvstore.SyncAlways,
		Registry: rt.Metrics, MetricNamespace: "label_kvstore",
	})
	if err != nil {
		return fmt.Errorf("opening the state store: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return kv.Close() })

	store, err := eventstore.New(kv)
	if err != nil {
		return fmt.Errorf("opening the event store: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return store.Close() })

	bus, err := eventlog.Open(filepath.Join(dataDir, "eventlog"),
		eventlog.WithMetrics(rt.Metrics), eventlog.WithLogger(rt.Log))
	if err != nil {
		return fmt.Errorf("opening the event log: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return bus.Close() })

	authority, err := loadPriceAuthority(pkiDir, rt.Log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	broker, err := mqtt.Dial(ctx, msgbus.Config{
		BrokerURL: brokerURL, ClientID: serviceName + "-" + canon.NewSpanID(),
		Username: brokerUser, Password: brokerPass,
		// CleanSession false so the broker holds this service's subscriptions
		// and un-acknowledged QoS 1 messages across a reconnect: a delivery
		// acknowledgement that arrives while the link is flapping must not be
		// lost, or the label it belongs to stays pending forever.
		CleanSession: false,
	}, mqtt.WithClientRegistry(rt.Metrics), mqtt.WithClientLogger(rt.Log))
	if err != nil {
		return fmt.Errorf("connecting to the MQTT broker at %s: %w", brokerURL, err)
	}
	rt.OnShutdown(func(context.Context) error { return broker.Close() })

	policies := domain.NewPolicySet()
	policies.Default = domain.Policy{
		GuardrailFactor:  float64(guardFactor),
		EffectiveGrace:   effectiveGrace,
		FullRefreshEvery: fullRefreshEvery,
	}.WithDefaults()

	svc, err := label.New(label.Config{
		Store: store, ReadModels: kv, Bus: bus, Broker: broker,
		Attestor: authority, Policies: policies,
		Currency: app.FixedCurrency(defaultCurrency),
		Registry: rt.Metrics, Log: rt.Log, Tracer: rt.Tracer, Standard: rt.Standard,
		Batch:             app.BatchConfig{Workers: batchWorkers},
		RateLimit:         app.TenantLimiterConfig{RatePerSecond: float64(tenantRate), Burst: float64(tenantBurst)},
		Scheduler:         app.ScheduledPriceRunnerConfig{Tick: schedulerTick},
		IdempotencyWindow: idemWindow,
		SnapshotEvery:     int64(snapshotEvery),
	})
	if err != nil {
		return err
	}
	if err := svc.EnsureStreams(ctx); err != nil {
		return fmt.Errorf("provisioning streams: %w", err)
	}
	for name, check := range svc.ReadinessChecks() {
		rt.Health.Register(name, check)
	}

	var wg sync.WaitGroup
	spawn := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
				rt.Log.Error("background task stopped", "task", name, "error", err)
			}
		}()
	}
	if err := svc.Start(ctx, spawn); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// A store-wide batch legitimately takes tens of seconds, so the write
		// timeout is generous; the read header timeout above is what actually
		// protects against a slow-loris.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  90 * time.Second,
	}
	go func() {
		rt.Log.Info("label service listening", "addr", httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			rt.Log.Error("http server stopped", "error", err)
			cancel()
		}
	}()

	rt.OnShutdown(func(shutdownCtx context.Context) error {
		// Stop accepting new work first, then drain what is in flight, then
		// stop the consumers. Reversing any two of those either drops a
		// half-finished fan-out or keeps taking work while trying to stop.
		if err := srv.Shutdown(shutdownCtx); err != nil {
			rt.Log.Error("http shutdown failed", "error", err)
		}
		if err := svc.Shutdown(shutdownCtx); err != nil {
			rt.Log.Error("service drain failed", "error", err)
		}
		cancel()
		wg.Wait()
		return nil
	})

	rt.Ready()
	rt.WaitForSignal(shutdownGrace)
	return nil
}

// loadPriceAuthority opens the tenant price-authority keys, or generates an
// ephemeral set when no directory is configured.
//
// The ephemeral path is for `make dev` and for tests, and it is loud about
// itself: a service signing prices with a key nobody has published cannot have
// its attestations verified by any Shelf Edge Controller in the field, which is
// a development convenience and a production outage.
func loadPriceAuthority(dir string, log *obs.Logger) (*pki.PriceAuthority, error) {
	if dir == "" {
		log.Warn("no price authority directory configured; generating an ephemeral signing key. " +
			"Attestations signed with it will not verify against any published key ring")
		return pki.NewPriceAuthority(pki.PriceAuthorityConfig{Logger: log})
	}
	authority, err := pki.LoadPriceAuthority(dir, pki.PriceAuthorityConfig{Logger: log})
	if err != nil {
		return nil, fmt.Errorf("loading the price authority from %s: %w", dir, err)
	}
	log.Info("price authority loaded", "dir", dir, "kid", authority.KeyID())
	return authority, nil
}

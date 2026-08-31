// Command pricing-service runs the USSLP AI Pricing Engine.
//
// The pricing engine is what makes USSLP an AI-native shelf platform rather
// than a display network: pricing decisions are made inside the platform, at
// three tiers of latency, instead of being computed elsewhere and pushed to
// dumb screens. Tier 1 — the rules engine — is pure and deterministic and also
// runs inside the Store Gateway Unit, so a store that has lost its WAN reaches
// the same decision the cloud would have. Tier 2 adds a per-store demand model
// and an expected-margin optimiser inside a 15-millisecond budget. Tier 3 runs
// asynchronously across stores and corrects for the volume that substitute SKUs
// take from each other.
//
// Configuration is twelve-factor with the USSLP_ prefix, and every value is
// also resolvable from a file, because the same binary runs on a gateway with
// no Kubernetes and no secrets operator.
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

	"github.com/usslp/usslp/platform/internal/pricing"
	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/internal/pricing/ports"
	"github.com/usslp/usslp/platform/pkg/config"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// serviceName is the identity every metric, log line and envelope carries.
const serviceName = "pricing-service"

// shutdownGrace bounds the drain.
//
// Fifteen seconds is sized against the longest thing in flight: a Tier-3
// cross-store pass over a category, which is a few seconds, plus the telemetry
// consumer's in-flight batch. It must be shorter than the Deployment's
// terminationGracePeriodSeconds or the drain never completes.
const shutdownGrace = 15 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

func run() error {
	loader := config.New("USSLP")
	svcCfg := config.LoadService(loader, serviceName)
	httpAddr := loader.String("PRICING_HTTP_ADDR", ":8085")
	featureRetention := loader.Duration("PRICING_FEATURE_RETENTION", 730*24*time.Hour)
	minObs := loader.Int("PRICING_ELASTICITY_MIN_OBSERVATIONS", 12)
	minPrices := loader.Int("PRICING_ELASTICITY_MIN_PRICES", 3)
	maxCIWidth := loader.String("PRICING_ELASTICITY_MAX_CI_WIDTH", "2.0")
	contamination := loader.String("PRICING_ANOMALY_CONTAMINATION", "0.005")
	anomalyRing := loader.Int("PRICING_ANOMALY_RING", 2048)
	maxQuantDelta := loader.String("PRICING_MAX_QUANTISATION_DELTA_PCT", "10")
	defaultMarginBps := loader.Int("PRICING_DEFAULT_MIN_MARGIN_BPS", 0)
	defaultCurrency := loader.String("DEFAULT_CURRENCY", "USD")
	if err := loader.Err(); err != nil {
		return err
	}

	ciWidth, err := parseFloat(maxCIWidth, "PRICING_ELASTICITY_MAX_CI_WIDTH")
	if err != nil {
		return err
	}
	contam, err := parseFloat(contamination, "PRICING_ANOMALY_CONTAMINATION")
	if err != nil {
		return err
	}
	quantDelta, err := parseFloat(maxQuantDelta, "PRICING_MAX_QUANTISATION_DELTA_PCT")
	if err != nil {
		return err
	}

	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service: serviceName, Version: svcCfg.Version, Region: svcCfg.Region,
		LogLevel: svcCfg.LogLevel, LogFormat: svcCfg.LogFormat,
		AdminAddr: svcCfg.AdminAddr, EnablePprof: svcCfg.EnablePprof,
		TraceSampleOneIn: uint64(svcCfg.TraceSample),
	})
	if err != nil {
		return err
	}

	dataDir := svcCfg.DataDir
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("creating data directory %s: %w", dataDir, err)
	}

	// The feature store and the model registry share one key/value store, and
	// therefore one write-ahead log: a training run that registers a model and
	// records the features it was built from commits both in one fsync.
	kv, err := kvstore.OpenWith(kvstore.Options{
		Dir: filepath.Join(dataDir, "pricing-state"), Sync: kvstore.SyncAlways,
		Registry: rt.Metrics, MetricNamespace: "pricing_kvstore",
	})
	if err != nil {
		return fmt.Errorf("opening the state store: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return kv.Close() })

	bus, err := eventlog.Open(filepath.Join(dataDir, "eventlog"),
		eventlog.WithMetrics(rt.Metrics), eventlog.WithLogger(rt.Log))
	if err != nil {
		return fmt.Errorf("opening the event log: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return bus.Close() })

	// The default constraint source is the static table with a tenant-wide
	// default. A production deployment replaces it with the merchandising
	// integration; the platform ships this one because a store must be able to
	// price safely before that integration exists, and an empty rule table with
	// a configured margin floor is the safe starting position.
	constraints := &ports.StaticConstraints{
		UseDefault: true,
		ByKey:      map[string]domain.Constraints{},
		Default: domain.Constraints{
			Currency:     defaultCurrency,
			MinMarginBps: int32(defaultMarginBps),
		},
	}

	svc, err := pricing.New(pricing.Config{
		State: kv, Bus: bus, ConstraintSource: constraints,
		Registry: rt.Metrics, Log: rt.Log, Tracer: rt.Tracer, Standard: rt.Standard,
		FeatureRetention: featureRetention,
		ElasticityPolicy: ml.ElasticityPolicy{
			MinObservations: minObs, MinDistinctPrices: minPrices,
			MaxCIWidth: ciWidth, RequireNegative: true, ConfidenceLevel: 0.95,
		},
		AnomalyContamination:    contam,
		AnomalyRingSize:         anomalyRing,
		MaxQuantisationDeltaPct: quantDelta,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
		// A training run and a Tier-3 pass are both legitimately slow; the read
		// header timeout above is what protects against a slow-loris.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  90 * time.Second,
	}
	go func() {
		rt.Log.Info("pricing service listening", "addr", httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			rt.Log.Error("http server stopped", "error", err)
			cancel()
		}
	}()

	rt.OnShutdown(func(shutdownCtx context.Context) error {
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

// parseFloat reads a float-valued setting.
//
// config.Loader has no Float accessor, and adding one would mean editing a
// package this service does not own. Parsing here keeps the change local and
// keeps the error message pointing at the variable the operator actually set.
func parseFloat(raw, name string) (float64, error) {
	var v float64
	if _, err := fmt.Sscanf(raw, "%g", &v); err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a number: %w", name, raw, err)
	}
	return v, nil
}

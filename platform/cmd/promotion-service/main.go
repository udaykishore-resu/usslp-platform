// Command promotion-service runs the USSLP Promotion Service.
//
// It owns the promotion rule DSL, the lifecycle that takes a promotion from
// draft to expired, the precedence policy that decides which of two overlapping
// offers a shelf shows, and the fan-out that tells the estate a promotion has
// started. It does not draw labels: it publishes `promotion.activated` and
// `promotion.expired` onto `promotion-events`, and the Label Service turns
// those into shelf updates.
//
// The activation sweep is the part that has to be right. A promotion whose
// window is "starts Monday" starts at local Monday in every store, so the sweep
// resolves each store's wall-clock window in its own zone and activates
// accordingly — never at UTC midnight, which would put a national promotion
// live on Sunday afternoon in the west of the estate.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/promotion"
	"github.com/usslp/usslp/platform/internal/promotion/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/config"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// serviceName is the identity every metric, log line and envelope carries.
const serviceName = "promotion-service"

// shutdownGrace bounds the drain.
//
// Ten seconds is sized against the longest thing in flight: a whole-catalogue
// simulation or a conflict scan, both of which are seconds rather than minutes.
// It must be shorter than the Deployment's terminationGracePeriodSeconds.
const shutdownGrace = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

func run() error {
	loader := config.New("USSLP")
	svcCfg := config.LoadService(loader, serviceName)
	httpAddr := loader.String("PROMOTION_HTTP_ADDR", ":8086")
	sweepInterval := loader.Duration("PROMOTION_SWEEP_INTERVAL", promotion.DefaultSweepInterval)
	zoneFile := loader.String("PROMOTION_STORE_ZONES_FILE", "")
	defaultZone := loader.String("PROMOTION_DEFAULT_ZONE", "UTC")
	tenants := loader.StringSlice("PROMOTION_TENANTS", nil)
	if err := loader.Err(); err != nil {
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

	kv, err := kvstore.OpenWith(kvstore.Options{
		Dir: filepath.Join(dataDir, "promotion-state"), Sync: kvstore.SyncAlways,
		Registry: rt.Metrics, MetricNamespace: "promotion_kvstore",
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

	directory, err := loadDirectory(zoneFile, defaultZone, rt.Log)
	if err != nil {
		return err
	}

	svc, err := promotion.New(promotion.Config{
		State: kv, Bus: bus, Directory: directory,
		Registry: rt.Metrics, Log: rt.Log, Tracer: rt.Tracer, Standard: rt.Standard,
		SweepInterval: sweepInterval,
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

	tenantIDs := make([]canon.TenantID, 0, len(tenants))
	for _, t := range tenants {
		tenantIDs = append(tenantIDs, canon.TenantID(t))
	}
	if len(tenantIDs) == 0 {
		rt.Log.Warn("no tenants configured for the activation sweep; scheduled promotions will not " +
			"activate on their own. Set USSLP_PROMOTION_TENANTS")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := svc.RunSweeper(ctx, func() []canon.TenantID { return tenantIDs }); err != nil &&
			!errors.Is(err, context.Canceled) {
			rt.Log.Error("activation sweeper stopped", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// A whole-catalogue simulation legitimately takes tens of seconds; the
		// read header timeout above is what protects against a slow-loris.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  90 * time.Second,
	}
	go func() {
		rt.Log.Info("promotion service listening", "addr", httpAddr)
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

// loadDirectory reads the store-to-time-zone map.
//
// It is a file rather than a service call because the sweep runs every minute
// and needs every store's zone every time; a store's zone changes when a store
// is built, which is not a rate that justifies a network dependency on the one
// path that must not fail. A deployment with a real store master replaces this
// adapter.
func loadDirectory(path, defaultZone string, log *obs.Logger) (*ports.StaticDirectory, error) {
	d := &ports.StaticDirectory{
		ZoneOf: map[canon.StoreID]string{}, ClusterOf: map[canon.StoreID]string{},
		DefaultZone: defaultZone,
	}
	if path == "" {
		log.Warn("no store zone file configured; every store resolves to the default zone, "+
			"so a wall-clock promotion will activate at the same instant everywhere",
			"default_zone", defaultZone)
		return d, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the store zone file %s: %w", path, err)
	}
	var doc struct {
		Stores map[string]struct {
			Zone    string `json:"zone"`
			Cluster string `json:"cluster,omitempty"`
		} `json:"stores"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parsing the store zone file %s: %w", path, err)
	}
	for id, entry := range doc.Stores {
		// Validating each zone at start-up beats discovering an unknown IANA
		// name during a sweep, where the failure is one store silently not
		// activating.
		if _, err := time.LoadLocation(entry.Zone); err != nil {
			return nil, fmt.Errorf("store %s has unknown time zone %q: %w", id, entry.Zone, err)
		}
		d.ZoneOf[canon.StoreID(id)] = entry.Zone
		if entry.Cluster != "" {
			d.ClusterOf[canon.StoreID(id)] = entry.Cluster
		}
	}
	log.Info("store directory loaded", "stores", len(d.ZoneOf), "file", path)
	return d, nil
}

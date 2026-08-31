// Command analytics-service runs the USSLP Retail Analytics Service.
//
// It consumes four event streams into a column-oriented time-series store,
// answers the platform's retail intelligence questions from it, computes the
// service-level objectives the platform is sold against, and ages data through
// hot, warm and cold tiers so a store's disk does not fill.
//
// The store is the platform's own: column-major blocks with per-column
// compression and a per-block min/max index, so a query reads only the columns
// it names and only the blocks its predicates cannot exclude. A tenant running
// a real warehouse points the ingest at that instead; this exists so that a
// tenant without one still gets the reports.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/analytics"
	"github.com/usslp/usslp/platform/internal/analytics/domain"
	"github.com/usslp/usslp/platform/pkg/config"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// serviceName is the identity every metric and log line carries.
const serviceName = "analytics-service"

// shutdownGrace bounds the drain.
//
// Twenty seconds is sized against the longest thing in flight: a full-scan
// report over a month of one tenant's deliveries, plus the final flush that
// seals whatever is buffered. Cutting the flush short loses at most one
// partial block, which is the accepted trade for not fsyncing per row; cutting
// the report short returns a 502 to a dashboard, which is merely annoying.
const shutdownGrace = 20 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

func run() error {
	loader := config.New("USSLP")
	svcCfg := config.LoadService(loader, serviceName)
	httpAddr := loader.String("ANALYTICS_HTTP_ADDR", ":8087")
	flushInterval := loader.Duration("ANALYTICS_FLUSH_INTERVAL", analytics.DefaultFlushInterval)
	retentionInterval := loader.Duration("ANALYTICS_RETENTION_INTERVAL", analytics.DefaultRetentionInterval)
	blockRows := loader.Int("ANALYTICS_BLOCK_ROWS", 0)
	blocksPerSegment := loader.Int("ANALYTICS_BLOCKS_PER_SEGMENT", 0)
	// Retention is overridable per tier for the whole store, because the common
	// operational need is "we are short of disk, halve everything" rather than
	// "change the telemetry policy specifically". A per-table override belongs
	// in a configuration file, and this service reads its policies from code
	// until a tenant needs that.
	hotDays := loader.Int("ANALYTICS_HOT_DAYS", 0)
	warmDays := loader.Int("ANALYTICS_WARM_DAYS", 0)
	coldDays := loader.Int("ANALYTICS_COLD_DAYS", 0)
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

	bus, err := eventlog.Open(dataDir+"/eventlog",
		eventlog.WithMetrics(rt.Metrics), eventlog.WithLogger(rt.Log))
	if err != nil {
		return fmt.Errorf("opening the event log: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return bus.Close() })

	retention := domain.DefaultRetention()
	if hotDays > 0 || warmDays > 0 || coldDays > 0 {
		retention = overrideRetention(retention, hotDays, warmDays, coldDays)
		rt.Log.Info("retention overridden from configuration",
			"hot_days", hotDays, "warm_days", warmDays, "cold_days", coldDays)
	}

	svc, err := analytics.New(analytics.Config{
		DataDir: dataDir, Bus: bus, Retention: retention,
		BlockRows: blockRows, BlocksPerSegment: blocksPerSegment,
		FlushInterval: flushInterval, RetentionInterval: retentionInterval,
		Registry: rt.Metrics, Log: rt.Log, Tracer: rt.Tracer, Standard: rt.Standard,
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
		// A full-scan report over a month legitimately takes tens of seconds;
		// the read header timeout above is what protects against a slow-loris.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  90 * time.Second,
	}
	go func() {
		rt.Log.Info("analytics service listening", "addr", httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			rt.Log.Error("http server stopped", "error", err)
			cancel()
		}
	}()

	rt.OnShutdown(func(shutdownCtx context.Context) error {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			rt.Log.Error("http shutdown failed", "error", err)
		}
		// Cancel before draining, so the flusher's context-done branch runs and
		// seals whatever is buffered. Doing it the other way round would leave
		// the last few seconds of ingest in memory.
		cancel()
		wg.Wait()
		if err := svc.Shutdown(shutdownCtx); err != nil {
			rt.Log.Error("service drain failed", "error", err)
		}
		return nil
	})

	rt.Ready()
	rt.WaitForSignal(shutdownGrace)
	return nil
}

// overrideRetention applies a blanket policy override.
//
// A zero for any tier leaves that tier's per-table default alone, so an
// operator can shorten the hot window without inadvertently rewriting the
// seven-year compliance retention on the price table.
func overrideRetention(base []domain.RetentionPolicy, hotDays, warmDays, coldDays int) []domain.RetentionPolicy {
	const day = 24 * time.Hour
	out := make([]domain.RetentionPolicy, 0, len(base))
	for _, p := range base {
		if hotDays > 0 {
			p.Hot = time.Duration(hotDays) * day
		}
		if warmDays > 0 {
			p.Warm = time.Duration(warmDays) * day
		}
		if coldDays > 0 {
			p.Cold = time.Duration(coldDays) * day
		}
		out = append(out, p)
	}
	return out
}

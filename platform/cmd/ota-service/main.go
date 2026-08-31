// Command ota-service runs the USSLP over-the-air firmware pipeline.
//
// # What this binary is responsible for
//
// It accepts signed firmware artifacts, prepares binary deltas, and drives
// staged rollouts across a fleet of devices that cannot be recovered remotely.
// A label that does not boot is a person walking an aisle with a screwdriver,
// so the safety properties are the product: nothing unsigned can be rolled out,
// cohorts start at one percent, four independent health signals are watched,
// and a rollout halts itself without waiting for a human.
//
// # Configuration
//
// Values are read through config.Loader with the USSLP_ prefix and are also
// resolvable from NAME_FILE=/run/secrets/x (interface contract §8).
//
// USSLP_OTA_SIGNING_KEYS is the one that matters. It is a comma-separated list
// of id=base64-public-key pairs naming the Ed25519 keys the platform trusts to
// sign firmware. With none configured the service accepts no artifact at all,
// which is the correct failure mode for a misconfigured deployment: an OTA
// pipeline that cannot verify a signature must not roll anything out.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/ota/adapters"
	"github.com/usslp/usslp/platform/internal/ota/adapters/httpapi"
	"github.com/usslp/usslp/platform/internal/ota/app"
	"github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/internal/ota/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/config"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ota-service: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.New("USSLP")

	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service:          "ota-service",
		Region:           cfg.String("REGION", "local"),
		LogLevel:         cfg.String("LOG_LEVEL", "info"),
		LogFormat:        cfg.String("LOG_FORMAT", "json"),
		AdminAddr:        cfg.String("OTA_ADMIN_ADDR", "127.0.0.1:9102"),
		EnablePprof:      cfg.Bool("ENABLE_PPROF", false),
		TraceSampleOneIn: uint64(cfg.Int("TRACE_SAMPLE_ONE_IN", 100)),
	})
	if err != nil {
		return fmt.Errorf("observability runtime: %w", err)
	}
	log := rt.Log

	dataDir := cfg.String("OTA_DATA_DIR", "./data/ota-service")
	kv, err := kvstore.OpenWith(kvstore.Options{
		Dir:             dataDir,
		Sync:            kvstore.SyncEvery,
		Registry:        rt.Metrics,
		MetricNamespace: "ota_kv",
	})
	if err != nil {
		return fmt.Errorf("open ota store at %s: %w", dataDir, err)
	}
	rt.OnShutdown(func(context.Context) error { return kv.Close() })

	store, err := eventstore.New(kv)
	if err != nil {
		return fmt.Errorf("open event store: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return store.Close() })

	artifacts, err := adapters.NewFileArtifactStore(cfg.String("OTA_ARTIFACT_DIR", "./data/firmware"))
	if err != nil {
		return fmt.Errorf("open artifact store: %w", err)
	}

	ring, err := parseKeyRing(cfg.String("OTA_SIGNING_KEYS", ""))
	if err != nil {
		return fmt.Errorf("parse firmware signing keys: %w", err)
	}
	if len(ring) == 0 {
		log.Error("no firmware signing keys are configured; " +
			"every artifact upload will be refused until USSLP_OTA_SIGNING_KEYS is set")
	}

	bus, err := eventlog.Open(cfg.String("EVENTLOG_DIR", "./data/eventlog"),
		eventlog.WithLogger(log), eventlog.WithMetrics(rt.Metrics))
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return bus.Close() })
	if err := bus.EnsureStreams(context.Background(), canon.AllStreams()...); err != nil {
		return fmt.Errorf("ensure streams: %w", err)
	}

	var messenger ports.DeviceMessenger = adapters.NopMessenger{}
	if brokerURL := cfg.String("MQTT_URL", ""); brokerURL != "" {
		client, err := mqtt.Dial(context.Background(), msgbus.Config{
			BrokerURL:      brokerURL,
			ClientID:       cfg.String("MQTT_CLIENT_ID", "ota-service"),
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
		rt.Health.Register("mqtt", func(context.Context) error {
			if !client.Connected() {
				return errors.New("broker link is down")
			}
			return nil
		})
	}

	// The fleet directory. In a split deployment this is an HTTP client for the
	// Device Registry; a rollout with no directory has no targets and simply
	// dispatches nothing, which is a safe state rather than an error.
	var fleet ports.FleetDirectory = adapters.NewStaticDirectory()

	ctrl, err := app.Open(context.Background(), app.Config{
		Store:               store,
		Artifacts:           artifacts,
		Keys:                ring,
		Fleet:               fleet,
		Events:              adapters.NewBusPublisher(bus),
		Messenger:           messenger,
		Clock:               ports.SystemClock{},
		Region:              canon.Region(rt.Region),
		Log:                 log,
		Metrics:             rt.Metrics,
		MaxConcurrentPerSEC: cfg.Int("OTA_MAX_CONCURRENT_PER_SEC", domain.DefaultMaxConcurrentPerSEC),
	})
	if err != nil {
		return fmt.Errorf("open rollout controller: %w", err)
	}
	rt.OnShutdown(func(context.Context) error { return ctrl.Close() })

	if err := ctrl.SubscribeResults(context.Background()); err != nil {
		return fmt.Errorf("subscribe to firmware results: %w", err)
	}

	// The control loop. Its interval is the granularity of every gate: a cohort
	// cannot advance, and a rollback cannot fire, sooner than the next pass. A
	// minute is fine — the gates are measured in tens of minutes — and keeps the
	// loop's own cost invisible next to a rollout that runs for days.
	loopCtx, stopLoop := context.WithCancel(context.Background())
	rt.OnShutdown(func(context.Context) error { stopLoop(); return nil })
	go controlLoop(loopCtx, ctrl, log, cfg.Duration("OTA_TICK_INTERVAL", time.Minute))

	api := httpapi.New(ctrl, log, rt.Standard)
	addr := cfg.String("OTA_HTTP_ADDR", "127.0.0.1:8082")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Firmware uploads are large and may arrive over a slow link.
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 2 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}
	rt.OnShutdown(func(ctx context.Context) error { return srv.Shutdown(ctx) })

	go func() {
		log.Info("ota service listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server stopped", "error", err)
		}
	}()

	rt.Ready()
	rt.WaitForSignal(cfg.Duration("SHUTDOWN_GRACE", 20*time.Second))
	return nil
}

// controlLoop advances every active rollout on a fixed interval.
func controlLoop(ctx context.Context, ctrl *app.Controller, log *obs.Logger, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			results, err := ctrl.Tick(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("rollout control loop failed", "error", err)
				continue
			}
			for _, r := range results {
				if r.Dispatched > 0 || r.MarkedSilent > 0 || r.Verdict != domain.VerdictWait {
					log.Info("rollout advanced",
						"job_id", r.JobID, "state", string(r.State), "wave", r.Wave,
						"dispatched", r.Dispatched, "marked_silent", r.MarkedSilent,
						"verdict", string(r.Verdict), "reason", r.Reason)
				}
			}
		}
	}
}

// parseKeyRing decodes "id=base64,id2=base64" into the trusted signing keys.
//
// The identifier is carried alongside the key so that a rotation is auditable
// and a compromised key's artifacts can be found. It is never used to *select*
// a key at verification time — see domain.KeyRing.Verify for why.
func parseKeyRing(spec string) (domain.KeyRing, error) {
	ring := domain.KeyRing{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, encoded, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("signing key %q is not id=base64", entry)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("signing key %q: %w", id, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("signing key %q is %d bytes, Ed25519 public keys are %d",
				id, len(raw), ed25519.PublicKeySize)
		}
		ring[strings.TrimSpace(id)] = ed25519.PublicKey(raw)
	}
	return ring, nil
}

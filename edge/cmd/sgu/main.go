// Command sgu runs a USSLP Store Gateway Unit: the store's MQTT broker, its
// bridge to the cloud, its offline brain and its diagnostics surface.
//
// It is designed to be started by a systemd unit on an industrial PC in a back
// office, with its configuration in the environment and its secrets in mounted
// files. The one behaviour worth knowing before reading the code: this process
// starts serving its store before it tries to reach the cloud, and it stays
// ready whether or not it ever succeeds. A gateway that reported itself unready
// during a WAN outage would be a gateway that took the store's labels down with
// the WAN, which is the exact failure the whole tier exists to prevent.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/usslp/usslp/edge/sgu"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/config"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/pki"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sgu: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	l := config.New("USSLP")
	svc := config.LoadService(l, "sgu")

	sguID := canon.SGUID(l.String("SGU_ID", "sgu-local"))
	storeID := canon.StoreID(l.Required("STORE_ID"))
	tenant := canon.TenantID(l.Required("TENANT_ID"))
	scope := canon.TopicScope{Tenant: tenant, Region: canon.Region(svc.Region), Store: storeID}

	brokerAddr := l.String("BROKER_ADDR", "0.0.0.0:1883")
	gatewayAdmin := l.String("GATEWAY_ADDR", "127.0.0.1:8080")
	cloudURL := l.String("CLOUD_BROKER_URL", "")
	dataDir := l.String("DATA_DIR", svc.DataDir)
	keyRingFile := l.String("KEYRING_FILE", "")
	localAuthorityDir := l.String("LOCAL_AUTHORITY_DIR", "")

	probeInterval := l.Duration("PROBE_INTERVAL", 5*time.Second)
	probeTimeout := l.Duration("PROBE_TIMEOUT", 3*time.Second)
	failThreshold := l.Int("WAN_FAIL_THRESHOLD", 3)
	failFor := l.Duration("WAN_FAIL_FOR", 12*time.Second)
	recoverThreshold := l.Int("WAN_RECOVER_THRESHOLD", 4)
	recoverFor := l.Duration("WAN_RECOVER_FOR", 15*time.Second)

	queueMax := l.Int("QUEUE_MAX_ENTRIES", 50000)
	queueBytes := l.Int("QUEUE_MAX_MB", 256)
	shutdownGrace := l.Duration("SHUTDOWN_GRACE", 20*time.Second)

	if err := l.Err(); err != nil {
		return err
	}

	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service: svc.Service, Version: svc.Version, Region: svc.Region,
		LogLevel: svc.LogLevel, LogFormat: svc.LogFormat,
		AdminAddr: svc.AdminAddr, EnablePprof: svc.EnablePprof,
		TraceSampleOneIn: uint64(svc.TraceSample),
	})
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	log := rt.Log.WithTenant(string(tenant), string(storeID))

	// The durable store is opened with SyncAlways. Losing an acknowledged price
	// change to a power cut is a weights-and-measures problem, and a store
	// gateway is a device that loses power: it sits on the same circuit as the
	// back-office kettle.
	store, err := kvstore.OpenWith(kvstore.Options{
		Dir: dataDir, Sync: kvstore.SyncAlways, Registry: rt.Metrics, MetricNamespace: "sgu_store",
	})
	if err != nil {
		return fmt.Errorf("opening the durable store in %q: %w", dataDir, err)
	}
	rt.OnShutdown(func(context.Context) error { return store.Close() })

	var ring *pki.KeyRing
	if keyRingFile != "" {
		raw, err := os.ReadFile(keyRingFile)
		if err != nil {
			return fmt.Errorf("reading the price-authority key ring from %q: %w", keyRingFile, err)
		}
		if ring, err = pki.ParseKeyRing(raw); err != nil {
			return fmt.Errorf("parsing the key ring: %w", err)
		}
		log.Info("loaded the price-authority key ring", "keys", ring.Len(), "active", ring.ActiveKeyID())
	}

	// The delegated, store-scoped authority. Without it this gateway can serve
	// prices but cannot author one, so a local point-of-sale change during an
	// outage is recorded and reported and never reaches a shelf. That is the
	// safe default and the message says so, because the alternative failure —
	// discovering it during an outage — is expensive.
	var localAuthority *pki.PriceAuthority
	if localAuthorityDir != "" {
		localAuthority, err = pki.LoadPriceAuthority(localAuthorityDir, pki.PriceAuthorityConfig{Logger: rt.Log})
		if err != nil {
			return fmt.Errorf("loading the delegated price authority from %q: %w", localAuthorityDir, err)
		}
		log.Info("loaded the delegated store price authority", "kid", localAuthority.KeyID())
	} else {
		log.Warn("no delegated price authority: local point-of-sale price changes will be " +
			"recorded and reported but will not reach a shelf, because a label refuses any price it cannot verify")
	}

	var cloud sgu.CloudFactory
	if cloudURL != "" {
		cloud = func(ctx context.Context) (msgbus.Client, error) {
			return mqtt.Dial(ctx, msgbus.Config{
				BrokerURL: cloudURL, ClientID: "sgu-" + string(sguID),
				CleanSession: false, KeepAlive: 30 * time.Second,
				ConnectTimeout: 10 * time.Second, AckTimeout: 10 * time.Second,
			}, mqtt.WithClientLogger(rt.Log))
		}
	} else {
		log.Warn("no cloud broker configured: this store will run permanently autonomous")
	}

	gw, err := sgu.New(sgu.Config{
		SGUID: sguID, StoreID: storeID, Scope: scope,
		BrokerAddr:     brokerAddr,
		Cloud:          cloud,
		Store:          store,
		KeyRing:        ring,
		LocalAuthority: localAuthority,
		Queue:          sgu.QueueConfig{MaxEntries: queueMax, MaxBytes: int64(queueBytes) << 20},
		Detector: sgu.DetectorConfig{
			Interval: probeInterval, Timeout: probeTimeout,
			FailThreshold: failThreshold, FailFor: failFor,
			RecoverThreshold: recoverThreshold, RecoverFor: recoverFor,
		},
		AdminAddr: gatewayAdmin,
		Log:       rt.Log,
		Registry:  rt.Metrics,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		return err
	}
	rt.OnShutdown(func(ctx context.Context) error { return gw.Stop(ctx) })

	// Readiness is about serving this store, and deliberately not about the
	// cloud. INTERFACE-CONTRACTS section 7 puts dependency checks on readiness
	// so that a dependency blip removes a pod from a load balancer rather than
	// restarting it; here the reasoning goes one step further. There is no load
	// balancer in front of a store gateway and nowhere else for its controllers
	// to go, so an unreachable cloud must not make this process look unhealthy.
	// The cloud's reachability is reported as a metric and on the diagnostics
	// page, where it is information rather than a verdict.
	rt.Health.Register("store-broker", func(context.Context) error {
		if gw.BrokerAddr() == "" {
			return errors.New("the store's MQTT broker is not listening")
		}
		return nil
	})
	rt.Health.Register("durable-store", func(context.Context) error {
		if s := store.Stats(); s.Sequence == 0 && s.Keys == 0 && s.WALBytes < 0 {
			return errors.New("the durable store is not writable")
		}
		return nil
	})
	rt.Health.Register("upstream-buffer", func(context.Context) error {
		s := gw.Queue().Stats()
		if s.MaxEntries > 0 && s.Depth >= s.MaxEntries {
			// Full is not unready — the store is still pricing correctly — but it
			// is the one queue state that loses data, and it belongs where an
			// operator will see it.
			return fmt.Errorf("the upstream buffer is full (%d messages): the cloud's record of this outage will have gaps", s.Depth)
		}
		return nil
	})
	rt.Ready()

	log.Info("store gateway ready",
		"sgu", sguID, "store", storeID, "broker", gw.BrokerAddr(),
		"diagnostics", gw.AdminAddr(), "admin", rt.Admin.Addr(),
		"mode", string(gw.Mode()), "labels", gw.Replica().LabelCount())

	rt.WaitForSignal(shutdownGrace)
	return nil
}

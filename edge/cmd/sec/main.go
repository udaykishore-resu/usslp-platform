// Command sec runs a USSLP Shelf Edge Controller: the Tier 2 device that
// verifies, renders and delivers prices to the labels on one shelf section.
//
// The radio it drives is edge/mesh, the platform's 802.15.4 model, rather than a
// host-controller interface to a real transceiver. That is a deliberate and
// visible limitation rather than a stub: the controller talks to its radio
// through sec.Transport, the model implements that interface honestly — one
// shared 250 kbps channel, real airtime, real retries, real duty cycling — and
// substituting a driver for a real coordinator is a matter of implementing the
// same seven methods. Everything above the interface, which is all of the
// attestation, sequencing, rendering, waveform and healing logic, is what would
// ship.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/usslp/usslp/edge/labelsim"
	"github.com/usslp/usslp/edge/mesh"
	"github.com/usslp/usslp/edge/sec"
	"github.com/usslp/usslp/edge/sim"
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
		fmt.Fprintf(os.Stderr, "sec: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	l := config.New("USSLP")
	svc := config.LoadService(l, "sec")

	secID := canon.SECID(l.Required("SEC_ID"))
	storeID := canon.StoreID(l.Required("STORE_ID"))
	tenant := canon.TenantID(l.Required("TENANT_ID"))
	gatewayURL := l.Required("GATEWAY_BROKER_URL")
	keyRingFile := l.Required("KEYRING_FILE")

	scope := canon.TopicScope{Tenant: tenant, Region: canon.Region(svc.Region), Store: storeID}
	dataDir := l.String("DATA_DIR", svc.DataDir)
	labelCount := l.Int("ZONE_LABELS", 240)
	aisleM := l.Int("ZONE_LENGTH_M", 24)
	seed := l.Int("ZONE_SEED", 1)
	healing := l.String("MESH_HEALING", "predictive")
	attestation := l.String("ATTESTATION", "end-to-end")
	sampleInterval := l.Duration("MESH_SAMPLE_INTERVAL", 30*time.Second)
	telemetryInterval := l.Duration("TELEMETRY_INTERVAL", time.Minute)
	heartbeatInterval := l.Duration("HEARTBEAT_INTERVAL", 10*time.Second)
	activeWindow := l.Duration("ACTIVE_WINDOW", 5*time.Minute)
	speed := l.Int("SIM_SPEED", 1)
	shutdownGrace := l.Duration("SHUTDOWN_GRACE", 15*time.Second)

	if err := l.Err(); err != nil {
		return err
	}
	mode, err := parseHealing(healing)
	if err != nil {
		return err
	}
	delivery, labelAttestation, err := parseAttestation(attestation)
	if err != nil {
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
	log := rt.Log.WithTenant(string(tenant), string(storeID)).With("sec", secID)

	raw, err := os.ReadFile(keyRingFile)
	if err != nil {
		return fmt.Errorf("reading the price-authority key ring from %q: %w", keyRingFile, err)
	}
	ring, err := pki.ParseKeyRing(raw)
	if err != nil {
		return fmt.Errorf("parsing the key ring: %w", err)
	}
	log.Info("loaded the price-authority key ring", "keys", ring.Len(), "active", ring.ActiveKeyID())

	// SyncEvery rather than SyncAlways: the controller's cache is a performance
	// optimisation over state the gateway also holds and republishes as retained
	// messages, so losing the last fraction of a second of it to a power cut
	// costs a re-sync, not a price.
	store, err := kvstore.OpenWith(kvstore.Options{
		Dir: dataDir, Sync: kvstore.SyncEvery, Registry: rt.Metrics, MetricNamespace: "sec_store",
	})
	if err != nil {
		return fmt.Errorf("opening the label cache in %q: %w", dataDir, err)
	}
	rt.OnShutdown(func(context.Context) error { return store.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The zone's radio and the labels on it.
	eng := sim.New(time.Now().UTC(), uint64(seed))
	// The zone's labels hold the same ring this controller does and verify for
	// themselves before driving a pixel. Both checks run; they are independent,
	// and the label's is the one that survives this controller being rooted.
	zone, err := labelsim.NewZone(eng, labelsim.ZoneSpec{
		StoreID: storeID, SECID: secID, Labels: labelCount,
		AisleLengthM: float64(aisleM), TelemetryInterval: telemetryInterval,
		KeyRing: ring, Attestation: labelAttestation, Mesh: mesh.Config{},
	})
	if err != nil {
		return fmt.Errorf("building the zone: %w", err)
	}
	formed := make(chan time.Duration, 1)
	zone.Form(func(d time.Duration) { formed <- d })
	// Formation is fast-forwarded rather than waited out: the mesh's own timing
	// model is what the tests measure, and a controller starting up should not
	// spend a real minute reproducing it.
	eng.RunUntil(eng.Elapsed() + 5*time.Minute)
	var formTime time.Duration
	select {
	case formTime = <-formed:
	default:
		return errors.New("the zone's mesh did not form")
	}
	go func() { _ = eng.Run(ctx, float64(speed)) }()
	log.Info("zone mesh formed", "labels", labelCount, "joined", zone.Net.Stats().Joined,
		"relays", len(zone.Relays()), "formation", formTime.String())

	// The last will is what lets the gateway learn about this controller's death
	// in under thirty seconds without polling every controller in the store.
	bus, err := mqtt.Dial(ctx, msgbus.Config{
		BrokerURL: gatewayURL, ClientID: "sec-" + string(secID),
		CleanSession: false, KeepAlive: 30 * time.Second,
		ConnectTimeout: 10 * time.Second, AckTimeout: 10 * time.Second,
		Will: sec.WillFor(scope, secID),
	}, mqtt.WithClientLogger(rt.Log))
	if err != nil {
		return fmt.Errorf("connecting to the store gateway at %s: %w", gatewayURL, err)
	}
	rt.OnShutdown(func(context.Context) error { return bus.Close() })

	coord := sec.NewCoordinator(zone.Net, sec.SimScheduler(eng), sec.CoordinatorConfig{
		SECID: secID, StoreID: storeID, Healing: mode,
		SampleInterval: sampleInterval, LabelReportInterval: telemetryInterval,
		Log: rt.Log, Registry: rt.Metrics,
	})
	specs := make([]sec.LabelSpec, 0, labelCount)
	for _, lb := range zone.Labels() {
		specs = append(specs, sec.LabelSpec{ID: lb.ID(), Node: lb.NodeID(), Tier: lb.Tier()})
	}
	ctl, err := sec.New(sec.Config{
		SECID: secID, StoreID: storeID, Scope: scope, Bus: bus, Store: store,
		KeyRing: ring, Coordinator: coord, Sched: sec.SimScheduler(eng), Labels: specs,
		TelemetryInterval: telemetryInterval, HeartbeatInterval: heartbeatInterval,
		Attestation: delivery, Log: rt.Log, Registry: rt.Metrics,
	})
	if err != nil {
		return err
	}
	if err := ctl.Start(ctx); err != nil {
		return err
	}
	rt.OnShutdown(func(ctx context.Context) error { ctl.Stop(ctx); return nil })

	// A price load reaches the glass inside the platform's SEC-to-label budget
	// (INTERFACE-CONTRACTS §4) only while the zone is in its active window;
	// outside it a label is on a thirty-second listen interval and is, on
	// average, fifteen seconds from being reachable at all. Holding the window
	// open is the controller's job.
	go holdActiveWindow(ctx, zone, activeWindow)

	rt.Health.Register("gateway-link", func(context.Context) error {
		if !bus.Connected() {
			return errors.New("not connected to the store gateway's broker")
		}
		return nil
	})
	rt.Health.Register("zone-mesh", func(context.Context) error {
		s := zone.Net.Stats()
		if s.Joined < s.Nodes/2 {
			return fmt.Errorf("only %d of %d nodes have joined the zone mesh", s.Joined, s.Nodes)
		}
		return nil
	})
	rt.Ready()

	log.Info("shelf edge controller ready",
		"labels", labelCount, "gateway", gatewayURL, "healing", mode.String(),
		"attestation", delivery.String(), "admin", rt.Admin.Addr())
	if delivery == sec.AttestControllerOnly {
		log.Warn("running in controller-only attestation: the labels in this zone will display " +
			"any price this controller accepts, so a compromise of this process is a compromise " +
			"of the shelf edge. Set USSLP_ATTESTATION=end-to-end once the labels can verify.")
	}

	rt.WaitForSignal(shutdownGrace)
	return nil
}

// holdActiveWindow keeps the zone's labels on their fast listen interval,
// re-broadcasting before the window would lapse.
func holdActiveWindow(ctx context.Context, zone *labelsim.Zone, window time.Duration) {
	if window <= 0 {
		return
	}
	zone.OpenActiveWindow(window)
	t := time.NewTicker(window / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			zone.OpenActiveWindow(window)
		}
	}
}

// parseAttestation resolves the attestation posture for the controller and for
// the labels it drives.
//
// They move together on purpose. A controller emitting legacy frames to labels
// that require end-to-end verification is a zone that quietly stops taking
// prices, and the two halves of that mistake are configured in the same place
// so it cannot be made by editing one of them.
func parseAttestation(s string) (sec.AttestationDelivery, labelsim.AttestationMode, error) {
	switch s {
	case "end-to-end", "":
		return sec.AttestEndToEnd, labelsim.AttestEndToEnd, nil
	case "controller-only":
		return sec.AttestControllerOnly, labelsim.AttestTrustController, nil
	}
	return sec.AttestEndToEnd, labelsim.AttestEndToEnd,
		fmt.Errorf("ATTESTATION must be end-to-end or controller-only, not %q", s)
}

func parseHealing(s string) (sec.HealingMode, error) {
	switch s {
	case "predictive", "":
		return sec.HealPredictive, nil
	case "reactive":
		return sec.HealReactive, nil
	case "off":
		return sec.HealOff, nil
	}
	return sec.HealPredictive, fmt.Errorf("MESH_HEALING must be predictive, reactive or off, not %q", s)
}

// Command labelsim runs a whole simulated store — N shelf edge controllers,
// each with M smart labels — in one process.
//
// It exists for three jobs that a fleet of real hardware cannot do cheaply:
// showing the platform working end to end without a warehouse full of labels,
// load-testing the edge tier at store scale, and answering questions about the
// hardware budget ("what does a 250 ms listen interval actually cost?") with
// arithmetic rather than opinion.
//
// The store is one goroutine driving one event queue. See edge/sim for why that
// is the design and what it buys: 40,000 labels are 40,000 structs, and a
// simulated year of battery drain costs a few thousand events rather than a few
// billion clock ticks.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
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
		fmt.Fprintf(os.Stderr, "labelsim: %v\n", err)
		os.Exit(1)
	}
}

// zone is one simulated controller and its labels.
type zone struct {
	id    canon.SECID
	z     *labelsim.Zone
	coord *sec.Coordinator
	ctl   *sec.Controller
	bus   msgbus.Client
}

// labelAttestation picks the posture a simulated label can actually honour.
func labelAttestation(ring *pki.KeyRing) labelsim.AttestationMode {
	if ring == nil {
		return labelsim.AttestTrustController
	}
	return labelsim.AttestEndToEnd
}

func run() error {
	l := config.New("USSLP")
	svc := config.LoadService(l, "labelsim")

	storeID := canon.StoreID(l.String("STORE_ID", "store-sim"))
	tenant := canon.TenantID(l.String("TENANT_ID", "demo-retail"))
	scope := canon.TopicScope{Tenant: tenant, Region: canon.Region(svc.Region), Store: storeID}

	controllers := l.Int("CONTROLLERS", 4)
	labelsPerSEC := l.Int("LABELS_PER_CONTROLLER", 250)
	aisleM := l.Int("ZONE_LENGTH_M", 24)
	colourEvery := l.Int("COLOUR_EVERY", 0)
	chillerPct := l.Int("CHILLER_PERCENT", 0)
	seed := l.Int("SEED", 1)
	speed := l.Int("SIM_SPEED", 60)
	gatewayURL := l.String("GATEWAY_BROKER_URL", "")
	keyRingFile := l.String("KEYRING_FILE", "")
	httpAddr := l.String("SIM_HTTP_ADDR", "127.0.0.1:8099")
	telemetryInterval := l.Duration("TELEMETRY_INTERVAL", 5*time.Minute)
	shutdownGrace := l.Duration("SHUTDOWN_GRACE", 10*time.Second)

	if err := l.Err(); err != nil {
		return err
	}
	if controllers <= 0 || labelsPerSEC <= 0 {
		return errors.New("CONTROLLERS and LABELS_PER_CONTROLLER must both be positive")
	}

	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service: svc.Service, Version: svc.Version, Region: svc.Region,
		LogLevel: svc.LogLevel, LogFormat: svc.LogFormat,
		AdminAddr: svc.AdminAddr, EnablePprof: svc.EnablePprof,
	})
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	log := rt.Log.WithTenant(string(tenant), string(storeID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ring *pki.KeyRing
	if keyRingFile != "" {
		raw, err := os.ReadFile(keyRingFile)
		if err != nil {
			return fmt.Errorf("reading the key ring from %q: %w", keyRingFile, err)
		}
		if ring, err = pki.ParseKeyRing(raw); err != nil {
			return fmt.Errorf("parsing the key ring: %w", err)
		}
	}

	eng := sim.New(time.Now().UTC(), uint64(seed))
	built := time.Now()
	zones := make([]*zone, 0, controllers)
	for i := 0; i < controllers; i++ {
		secID := canon.SECID(fmt.Sprintf("sec-%03d", i))
		z, err := labelsim.NewZone(eng, labelsim.ZoneSpec{
			StoreID: storeID, SECID: secID, Labels: labelsPerSEC,
			AisleLengthM: float64(aisleM), ColourEvery: colourEvery,
			ChillerFraction:   float64(chillerPct) / 100,
			TelemetryInterval: telemetryInterval,
			// Labels verify for themselves when a ring is available. Without
			// one they cannot, and a simulated store with no key material runs
			// in compatibility mode rather than refusing every price — which is
			// the same decision a real label with an empty ring cannot make,
			// and is why the status page reports which mode it is in.
			KeyRing:     ring,
			Attestation: labelAttestation(ring),
			// Each controller is a separate personal-area network on its own
			// channel, which is how a site survey plans them: zones that share a
			// channel share its 250 kbps and update each other's labels slowly.
			Mesh: mesh.Config{PANID: uint16(0x1000 + i), Channel: 11 + i%16},
		})
		if err != nil {
			return fmt.Errorf("building zone %s: %w", secID, err)
		}
		zn := &zone{id: secID, z: z}
		zones = append(zones, zn)
	}
	for _, zn := range zones {
		zn.z.Form(func(time.Duration) {})
	}
	eng.RunUntil(eng.Elapsed() + 10*time.Minute)
	joined := 0
	for _, zn := range zones {
		joined += zn.z.Net.Stats().Joined
	}
	log.Info("simulated store built",
		"controllers", controllers, "labels", controllers*labelsPerSEC, "joined", joined,
		"build_time", time.Since(built).Round(time.Millisecond).String())

	// Controllers are attached only when there is a gateway to attach them to.
	// Without one the simulator is still useful — it is a mesh and a fleet of
	// labels whose battery and radio behaviour can be measured — and saying so
	// beats failing to start.
	if gatewayURL != "" && ring != nil {
		for _, zn := range zones {
			if err := attachController(ctx, rt, zn, scope, storeID, gatewayURL, ring, eng, telemetryInterval); err != nil {
				return err
			}
			rt.OnShutdown(func(ctx context.Context) error {
				zn.ctl.Stop(ctx)
				return zn.bus.Close()
			})
		}
		log.Info("controllers connected to the store gateway", "gateway", gatewayURL)
	} else {
		log.Warn("running without controllers: set GATEWAY_BROKER_URL and KEYRING_FILE to drive real price updates")
	}

	go func() { _ = eng.Run(ctx, float64(speed)) }()

	srv, err := serveStatus(httpAddr, storeID, eng, zones)
	if err != nil {
		return err
	}
	rt.OnShutdown(func(ctx context.Context) error { return srv.Shutdown(ctx) })

	rt.Health.Register("mesh", func(context.Context) error {
		total, up := 0, 0
		for _, zn := range zones {
			s := zn.z.Net.Stats()
			total += s.Nodes
			up += s.Joined
		}
		if up < total/2 {
			return fmt.Errorf("only %d of %d nodes are on the mesh", up, total)
		}
		return nil
	})
	rt.Ready()

	proj := labelsim.DefaultPower().Project(labelsim.Tier29BWR, labelsim.DefaultWorkload())
	log.Info("simulated store running",
		"status", "http://"+httpAddr+"/store", "admin", rt.Admin.Addr(),
		"sim_speed", speed,
		"battery_projection_years", fmt.Sprintf("%.2f", proj.Years),
		"battery_average_ua", fmt.Sprintf("%.2f", proj.TotalUA))

	rt.WaitForSignal(shutdownGrace)
	return nil
}

// attachController wires a real Shelf Edge Controller to a simulated zone.
func attachController(ctx context.Context, rt *obs.Runtime, zn *zone, scope canon.TopicScope,
	storeID canon.StoreID, gatewayURL string, ring *pki.KeyRing, eng *sim.Engine, telemetry time.Duration) error {

	bus, err := mqtt.Dial(ctx, msgbus.Config{
		BrokerURL: gatewayURL, ClientID: "sec-" + string(zn.id),
		CleanSession: false, KeepAlive: 30 * time.Second,
		ConnectTimeout: 10 * time.Second, AckTimeout: 10 * time.Second,
		Will: sec.WillFor(scope, zn.id),
	})
	if err != nil {
		return fmt.Errorf("connecting %s to the gateway: %w", zn.id, err)
	}
	zn.bus = bus

	// Every controller in this process shares one metrics registry, and
	// obs.Registry refuses duplicate metric names, so only the first registers.
	// The alternative — a registry per controller — would produce a /metrics
	// page with four copies of every series and no way to tell them apart.
	var reg *obs.Registry
	if zn.id == "sec-000" {
		reg = rt.Metrics
	}
	zn.coord = sec.NewCoordinator(zn.z.Net, sec.SimScheduler(eng), sec.CoordinatorConfig{
		SECID: zn.id, StoreID: storeID, Healing: sec.HealPredictive,
		LabelReportInterval: telemetry, Log: rt.Log, Registry: reg,
	})
	store, err := kvstore.OpenWith(kvstore.Options{Sync: kvstore.SyncNever})
	if err != nil {
		return fmt.Errorf("opening the label cache for %s: %w", zn.id, err)
	}
	specs := make([]sec.LabelSpec, 0, len(zn.z.Labels()))
	for _, lb := range zn.z.Labels() {
		specs = append(specs, sec.LabelSpec{ID: lb.ID(), Node: lb.NodeID(), Tier: lb.Tier()})
	}
	ctl, err := sec.New(sec.Config{
		SECID: zn.id, StoreID: storeID, Scope: scope, Bus: bus, Store: store,
		KeyRing: ring, Coordinator: zn.coord, Sched: sec.SimScheduler(eng), Labels: specs,
		TelemetryInterval: telemetry, HeartbeatInterval: 10 * time.Second,
		Log: rt.Log, Registry: reg,
	})
	if err != nil {
		return err
	}
	if err := ctl.Start(ctx); err != nil {
		return err
	}
	zn.ctl = ctl
	// Hold the zone awake so the demo's price changes land inside the latency
	// budget rather than inside a resting duty cycle.
	zn.z.OpenActiveWindow(365 * 24 * time.Hour)
	return nil
}

// storeView is the simulator's own status surface: what the store looks like
// right now, in terms a person watching the demo cares about.
type storeView struct {
	StoreID       canon.StoreID `json:"store_id"`
	SimulatedTime time.Time     `json:"simulated_time"`
	SimulatedFor  string        `json:"simulated_for"`
	Events        uint64        `json:"events_processed"`
	Labels        int           `json:"labels"`
	Alive         int           `json:"labels_alive"`
	Zones         []zoneView    `json:"zones"`
	Battery       batteryView   `json:"battery_projection"`
}

type zoneView struct {
	SECID              canon.SECID `json:"sec_id"`
	Labels             int         `json:"labels"`
	MeshNodes          int         `json:"mesh_nodes"`
	MeshJoined         int         `json:"mesh_nodes_joined"`
	MaxDepth           int         `json:"max_hop_depth"`
	ChannelUtilisation string      `json:"channel_utilisation"`
	Refreshes          int64       `json:"panel_refreshes"`
	Partials           int64       `json:"partial_refreshes"`
	Discarded          int64       `json:"stale_updates_discarded"`
	MeanBatteryPct     int         `json:"mean_battery_percent"`
	Delivered          uint64      `json:"deliveries"`
	Failed             uint64      `json:"delivery_failures"`
}

type batteryView struct {
	Profile     string `json:"profile"`
	Attestation string `json:"attestation"`
	AverageUA   string `json:"average_current_ua"`
	Years       string `json:"projected_years"`
	BeaconShare string `json:"share_spent_listening"`
	Note        string `json:"note"`
}

func serveStatus(addr string, storeID canon.StoreID, eng *sim.Engine, zones []*zone) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/store", func(w http.ResponseWriter, r *http.Request) {
		view := storeView{
			StoreID: storeID, SimulatedTime: eng.Now(),
			SimulatedFor: eng.Elapsed().Round(time.Second).String(), Events: eng.Fired(),
		}
		for _, zn := range zones {
			ns := zn.z.Net.Stats()
			zv := zoneView{SECID: zn.id, Labels: len(zn.z.Labels()),
				MeshNodes: ns.Nodes, MeshJoined: ns.Joined}
			zv.ChannelUtilisation = fmt.Sprintf("%.2f%%", 100*zn.z.Net.ChannelUtilisation())
			for _, st := range zn.z.Net.Topology() {
				if st.Depth > zv.MaxDepth {
					zv.MaxDepth = st.Depth
				}
			}
			batt := 0
			for _, lb := range zn.z.Labels() {
				s := lb.Stats()
				zv.Refreshes += s.RefreshCount
				zv.Partials += s.PartialRefreshes
				zv.Discarded += s.Discarded
				batt += s.BatteryPct
			}
			if n := len(zn.z.Labels()); n > 0 {
				zv.MeanBatteryPct = batt / n
			}
			if zn.coord != nil {
				cs := zn.coord.Stats()
				zv.Delivered, zv.Failed = cs.Delivered, cs.Failed
			}
			view.Labels += zv.Labels
			view.Alive += zn.z.Alive()
			view.Zones = append(view.Zones, zv)
		}
		sort.Slice(view.Zones, func(i, j int) bool { return view.Zones[i].SECID < view.Zones[j].SECID })

		proj := labelsim.DefaultPower().Project(labelsim.Tier29BWR, labelsim.DefaultWorkload())
		literal := labelsim.AlwaysFastPower().Project(labelsim.Tier29BWR, labelsim.DefaultWorkload())
		mode := labelsim.AttestTrustController.String()
		if len(zones) > 0 && len(zones[0].z.Labels()) > 0 {
			mode = zones[0].z.Labels()[0].AttestationMode().String()
		}
		view.Battery = batteryView{
			Profile:     "2.9in BWR, 10 updates/day, duty-cycled",
			Attestation: mode,
			AverageUA:   fmt.Sprintf("%.2f", proj.TotalUA),
			Years:       fmt.Sprintf("%.2f", proj.Years),
			BeaconShare: fmt.Sprintf("%.0f%%", 100*proj.BeaconUA/proj.TotalUA),
			Note: fmt.Sprintf(
				"the same label listening every 250 ms with no duty cycling draws %.0f uA and lasts %.2f years",
				literal.TotalUA, literal.Years),
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(view)
	})
	mux.HandleFunc("/label", func(w http.ResponseWriter, r *http.Request) {
		id := canon.LabelID(r.URL.Query().Get("id"))
		for _, zn := range zones {
			if lb, ok := zn.z.Label(id); ok {
				w.Header().Set("Content-Type", "application/json")
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				_ = enc.Encode(map[string]any{
					"label_id": id, "sec_id": zn.id, "panel": lb.Display().Name,
					"in_active_window": lb.InActiveWindow(), "dead": lb.Dead(),
					"stats": lb.Stats(),
				})
				return
			}
		}
		http.Error(w, "no such label in this simulated store", http.StatusNotFound)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "labelsim: status surface stopped: %v\n", err)
		}
	}()
	return srv, nil
}

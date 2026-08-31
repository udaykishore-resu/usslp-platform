package stack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/usslp/usslp/platform/internal/analytics"
	"github.com/usslp/usslp/platform/internal/apigw"
	"github.com/usslp/usslp/platform/internal/label"
	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	otaadapters "github.com/usslp/usslp/platform/internal/ota/adapters"
	otahttp "github.com/usslp/usslp/platform/internal/ota/adapters/httpapi"
	otaapp "github.com/usslp/usslp/platform/internal/ota/app"
	otaports "github.com/usslp/usslp/platform/internal/ota/ports"
	"github.com/usslp/usslp/platform/internal/pricing"
	pricingdomain "github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
	pricingports "github.com/usslp/usslp/platform/internal/pricing/ports"
	"github.com/usslp/usslp/platform/internal/promotion"
	promoports "github.com/usslp/usslp/platform/internal/promotion/ports"
	regadapters "github.com/usslp/usslp/platform/internal/registry/adapters"
	reghttp "github.com/usslp/usslp/platform/internal/registry/adapters/httpapi"
	regapp "github.com/usslp/usslp/platform/internal/registry/app"
	regdomain "github.com/usslp/usslp/platform/internal/registry/domain"
	regports "github.com/usslp/usslp/platform/internal/registry/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// cloudServices holds every Tier-4 component and the address it answers on.
type cloudServices struct {
	registry     *regapp.Service
	registryURL  string
	label        *label.Service
	labelURL     string
	labelBroker  msgbus.Client
	ota          *otaapp.Controller
	otaURL       string
	otaFleet     *registryFleet
	pricing      *pricing.Service
	pricingURL   string
	promotion    *promotion.Service
	promotionURL string
	analytics    *analytics.Service
	analyticsURL string
	uig          *uigService
	uigURL       string
	gateway      *apigw.Gateway
	gatewayURL   string
	// tenantKeys are the owner API keys minted at boot, one per tenant. They
	// are printed in the banner because a gateway whose only key-issuing
	// endpoint requires a key cannot be used otherwise.
	tenantKeys   map[canon.TenantID]string
	bootstrapKey string
	admin        map[string]string
}

// startCloudServices assembles the whole Tier-4 platform against the shared
// event log and the cloud broker.
//
// Each service gets its own obs.Runtime, and therefore its own metrics registry
// and its own admin port. That is not ceremony: obs.Registry panics on a
// duplicate metric name, so one registry for the whole process would make two
// services that both register `usslp_requests_total` a start-up crash, and a
// single /metrics page carrying eight services' series with no way to tell them
// apart would be useless to the dashboards in deploy/observability, which key
// on the `service` label.
func (s *Stack) startCloudServices(ctx context.Context) error {
	c := &cloudServices{admin: map[string]string{}}
	s.cloudSvcs = c

	if err := s.startRegistry(ctx, c); err != nil {
		return err
	}
	if err := s.startLabelService(ctx, c); err != nil {
		return err
	}
	if err := s.startOTA(ctx, c); err != nil {
		return err
	}
	if err := s.startPricing(ctx, c); err != nil {
		return err
	}
	if err := s.startPromotion(ctx, c); err != nil {
		return err
	}
	if err := s.startAnalytics(ctx, c); err != nil {
		return err
	}
	if err := s.startUIG(ctx, c); err != nil {
		return err
	}
	if err := s.startAPIGateway(ctx, c); err != nil {
		return err
	}
	return nil
}

// runtimeFor builds one service's observability stack and registers its
// teardown.
func (s *Stack) runtimeFor(service string, adminPort int) (*obs.Runtime, error) {
	rt, err := obs.NewRuntime(obs.RuntimeConfig{
		Service: service, Region: s.cfg.Region,
		LogLevel: s.cfg.LogLevel, LogFormat: s.cfg.LogFormat,
		AdminAddr:        s.addr(adminPort),
		TraceSampleOneIn: 100,
	})
	if err != nil {
		return nil, fmt.Errorf("usslpd: %s observability: %w", service, err)
	}
	s.push(service+" runtime", func(context.Context) error { rt.Shutdown(5 * time.Second); return nil })
	return rt, nil
}

// adminPort returns the admin port for the nth cloud service, or 0 when the
// operating system is choosing.
func (s *Stack) adminPort(n int) int {
	if s.cfg.Ports.AdminBase <= 0 {
		return 0
	}
	return s.cfg.Ports.AdminBase + n
}

// kvFor opens one service's durable store.
func (s *Stack) kvFor(name string, rt *obs.Runtime, sync kvstore.SyncPolicy) (*kvstore.Store, error) {
	dir := filepath.Join(s.cfg.DataDir, name)
	kv, err := kvstore.OpenWith(kvstore.Options{
		Dir: dir, Sync: sync, Registry: rt.Metrics,
		MetricNamespace: sanitiseNamespace(name) + "_kv",
	})
	if err != nil {
		return nil, fmt.Errorf("usslpd: opening %s state at %s: %w", name, dir, err)
	}
	s.push(name+" state", func(context.Context) error { return kv.Close() })
	return kv, nil
}

// sanitiseNamespace turns a directory name into a Prometheus metric prefix.
func sanitiseNamespace(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			out = append(out, ch)
		case ch >= 'A' && ch <= 'Z':
			out = append(out, ch+32)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// dialCloud connects one cloud service to the cloud MQTT broker.
func (s *Stack) dialCloud(ctx context.Context, clientID string, rt *obs.Runtime) (msgbus.Client, error) {
	cl, err := mqtt.Dial(ctx, msgbus.Config{
		BrokerURL: s.cloudURL, ClientID: clientID,
		// CleanSession false so the broker holds the subscription and any
		// unacknowledged QoS 1 message across a reconnect: a delivery
		// acknowledgement lost while the link flaps is a label that stays
		// pending forever.
		CleanSession:   false,
		KeepAlive:      30 * time.Second,
		ConnectTimeout: 10 * time.Second,
		AckTimeout:     10 * time.Second,
	}, mqtt.WithClientLogger(rt.Log), mqtt.WithClientRegistry(rt.Metrics))
	if err != nil {
		return nil, fmt.Errorf("usslpd: connecting %s to the cloud broker at %s: %w", clientID, s.cloudURL, err)
	}
	s.push(clientID+" broker link", func(context.Context) error { return cl.Close() })
	return cl, nil
}

// ---------------------------------------------------------------------------
// Device Registry
// ---------------------------------------------------------------------------

func (s *Stack) startRegistry(ctx context.Context, c *cloudServices) error {
	rt, err := s.runtimeFor("device-registry", s.adminPort(3))
	if err != nil {
		return err
	}
	kv, err := s.kvFor("device-registry", rt, kvstore.SyncEvery)
	if err != nil {
		return err
	}
	store, err := eventstore.New(kv)
	if err != nil {
		return fmt.Errorf("usslpd: registry event store: %w", err)
	}
	s.push("device-registry event store", func(context.Context) error { return store.Close() })

	client, err := s.dialCloud(ctx, "device-registry", rt)
	if err != nil {
		return err
	}

	svc, err := regapp.Open(ctx, regapp.Config{
		Store:     store,
		Events:    regadapters.NewBusPublisher(s.log),
		Messenger: regadapters.NewMessenger(client),
		Auth:      regadapters.NewHierarchyAuthenticator(s.hierarchy),
		// The issuer is present because this deployment shape *is* the
		// development seeding case: usslpd stands a synthetic store up on every
		// boot, and every device it creates goes through the same certificate
		// issuance, manifest comparison and anti-cloning check a real label
		// goes through. A production registry is configured without an issuer,
		// which disables this path entirely.
		Issuer: regadapters.NewHierarchyIssuer(s.hierarchy),
		Clock:  regports.SystemClock{},
		Region: canon.Region(s.cfg.Region),
		Log:    rt.Log, Metrics: rt.Metrics,
		Health: regdomain.HealthPolicy{
			BeaconInterval: 30 * time.Second, MissedBeacons: 3,
			BatteryCriticalPct: 10, BatteryCriticalMV: 2400, BatteryEndOfLifePct: 5,
		},
	})
	if err != nil {
		return fmt.Errorf("usslpd: opening the device registry: %w", err)
	}
	s.push("device-registry service", func(context.Context) error { return svc.Close() })
	if err := svc.SubscribeDeviceTraffic(ctx); err != nil {
		return fmt.Errorf("usslpd: subscribing the registry to device traffic: %w", err)
	}
	if s.cfg.RegistrySweepInterval > 0 {
		go s.registrySweep(ctx, svc, rt.Log)
	}

	ln, err := s.listen("device-registry", s.cfg.Ports.Registry)
	if err != nil {
		return err
	}
	s.serve("device-registry", ln, reghttp.New(svc, rt.Log, rt.Standard).Handler(), 2*time.Minute)

	c.registry = svc
	c.registryURL = "http://" + ln.Addr().String()
	c.admin["device-registry"] = "http://" + rt.Admin.Addr()
	rt.Ready()
	return nil
}

// registrySweep turns silence into an offline event.
//
// # Why it is off by default here
//
// regapp.Service.SweepHealth has a data race with the mesh-report ingest path,
// and running both is enough to fail `go test -race` reliably. The two use
// different locks over the same state: IngestMeshReport updates a device's
// LastSeen under s.mu, while SweepHealth's apply phase clones the same device
// under s.cmdMu only. The race is in platform/internal/registry/app, it is
// present in the shipped device-registry binary too — which runs this sweep
// every fifteen seconds — and it is reported rather than patched from here,
// because this package does not own that code.
//
// Turning the sweep off costs one thing and nothing else: a device that stops
// reporting is not automatically transitioned to offline. Telemetry, mesh
// reports and heartbeats still arrive and are still folded into the registry's
// health model, so the evidence is there; it is only the derivation on a timer
// that is absent. Set Config.RegistrySweepInterval to turn it back on, and
// expect the race detector to find it.
func (s *Stack) registrySweep(ctx context.Context, svc *regapp.Service, log *obs.Logger) {
	t := time.NewTicker(s.cfg.RegistrySweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := svc.SweepHealth(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("health sweep failed", "error", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Label Service
// ---------------------------------------------------------------------------

func (s *Stack) startLabelService(ctx context.Context, c *cloudServices) error {
	rt, err := s.runtimeFor("label-service", s.adminPort(2))
	if err != nil {
		return err
	}
	kv, err := s.kvFor("label-service", rt, kvstore.SyncEvery)
	if err != nil {
		return err
	}
	store, err := eventstore.New(kv)
	if err != nil {
		return fmt.Errorf("usslpd: label event store: %w", err)
	}
	s.push("label-service event store", func(context.Context) error { return store.Close() })

	client, err := s.dialCloud(ctx, "label-service", rt)
	if err != nil {
		return err
	}
	c.labelBroker = client

	policies := domain.NewPolicySet()
	policies.Default = domain.Policy{
		GuardrailFactor:  float64(domain.DefaultGuardrailFactor),
		EffectiveGrace:   domain.DefaultEffectiveGrace,
		FullRefreshEvery: domain.DefaultFullRefreshEvery,
	}.WithDefaults()

	svc, err := label.New(label.Config{
		Store: store, ReadModels: kv, Bus: s.log, Broker: client,
		Attestor: s.authority, Policies: policies,
		Currency: app.FixedCurrency(s.cfg.Currency),
		Registry: rt.Metrics, Log: rt.Log, Tracer: rt.Tracer, Standard: rt.Standard,
		// The scheduler tick decides how late a future-dated price may be. One
		// second rather than the default, because the end-to-end suite asserts
		// that a promotion scheduled for "two seconds from now" activates, and
		// a coarser tick would make that assertion about the tick.
		Scheduler: app.ScheduledPriceRunnerConfig{Tick: time.Second},
		Streams:   devStreams(s.cfg.DevPartitions),
	})
	if err != nil {
		return fmt.Errorf("usslpd: assembling the label service: %w", err)
	}
	if err := svc.EnsureStreams(ctx); err != nil {
		return fmt.Errorf("usslpd: label service streams: %w", err)
	}
	for name, check := range svc.ReadinessChecks() {
		rt.Health.Register(name, check)
	}
	if err := svc.Start(ctx, s.spawn(rt.Log)); err != nil {
		return fmt.Errorf("usslpd: starting the label service: %w", err)
	}
	s.push("label-service drain", func(ctx context.Context) error { return svc.Shutdown(ctx) })

	ln, err := s.listen("label-service", s.cfg.Ports.Label)
	if err != nil {
		return err
	}
	s.serve("label-service", ln, svc.Handler(), 120*time.Second)

	c.label = svc
	c.labelURL = "http://" + ln.Addr().String()
	c.admin["label-service"] = "http://" + rt.Admin.Addr()
	rt.Ready()
	return nil
}

// spawn returns the background-task starter every service's Start takes.
func (s *Stack) spawn(log *obs.Logger) func(string, func(context.Context) error) {
	return func(name string, fn func(context.Context) error) {
		ctx := s.backgroundCtx()
		go func() {
			if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("background task stopped", "task", name, "error", err)
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// OTA
// ---------------------------------------------------------------------------

func (s *Stack) startOTA(ctx context.Context, c *cloudServices) error {
	rt, err := s.runtimeFor("ota-service", s.adminPort(4))
	if err != nil {
		return err
	}
	kv, err := s.kvFor("ota-service", rt, kvstore.SyncEvery)
	if err != nil {
		return err
	}
	store, err := eventstore.New(kv)
	if err != nil {
		return fmt.Errorf("usslpd: ota event store: %w", err)
	}
	s.push("ota-service event store", func(context.Context) error { return store.Close() })

	artifacts, err := otaadapters.NewFileArtifactStore(filepath.Join(s.cfg.DataDir, "firmware"))
	if err != nil {
		return fmt.Errorf("usslpd: opening the firmware artifact store: %w", err)
	}
	client, err := s.dialCloud(ctx, "ota-service", rt)
	if err != nil {
		return err
	}

	fleet := &registryFleet{}
	c.otaFleet = fleet

	ctrl, err := otaapp.Open(ctx, otaapp.Config{
		Store: store, Artifacts: artifacts,
		// The trusted firmware signing keys are generated with the rest of the
		// hierarchy and handed to the operator through the control surface, so
		// that `usslpctl ota start` can sign an artifact the service will
		// actually accept. A ring that verified nothing would make every OTA
		// test a test of a bypass.
		Keys:      s.firmwareKeys(),
		Fleet:     fleet,
		Events:    otaadapters.NewBusPublisher(s.log),
		Messenger: otaadapters.NewMessenger(client),
		Clock:     otaports.SystemClock{},
		Region:    canon.Region(s.cfg.Region),
		Log:       rt.Log, Metrics: rt.Metrics,
	})
	if err != nil {
		return fmt.Errorf("usslpd: opening the rollout controller: %w", err)
	}
	s.push("ota-service controller", func(context.Context) error { return ctrl.Close() })
	if err := ctrl.SubscribeResults(ctx); err != nil {
		return fmt.Errorf("usslpd: subscribing to firmware results: %w", err)
	}
	// One second rather than the production minute. Every rollout gate — cohort
	// advance, soak, rollback — is evaluated on this tick, so it is also the
	// granularity of the safety properties. A minute is right when a rollout
	// runs for days; it would make a demonstration of automatic rollback take
	// five minutes to watch.
	go s.otaLoop(ctx, ctrl, rt.Log, time.Second)

	ln, err := s.listen("ota-service", s.cfg.Ports.OTA)
	if err != nil {
		return err
	}
	s.serve("ota-service", ln, otahttp.New(ctrl, rt.Log, rt.Standard).Handler(), 2*time.Minute)

	c.ota = ctrl
	c.otaURL = "http://" + ln.Addr().String()
	c.admin["ota-service"] = "http://" + rt.Admin.Addr()
	rt.Ready()
	return nil
}

func (s *Stack) otaLoop(ctx context.Context, ctrl *otaapp.Controller, log *obs.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := ctrl.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("rollout control loop failed", "error", err)
			}
		}
	}
}

// registryFleet adapts the Device Registry to the OTA service's fleet port.
//
// In the distributed topology this is an HTTP client; in one process it is a
// direct call. Either way the registry, not the OTA service, decides which
// devices are addressable — which is what stops the two from disagreeing about
// a quarantined label and pushing firmware at a device the platform has decided
// it cannot trust.
type registryFleet struct {
	svc *regapp.Service
	// zone is the store's IANA time zone, used to evaluate quiet hours in the
	// store's own local time.
	zone string
}

func (f *registryFleet) attach(svc *regapp.Service, zone string) { f.svc, f.zone = svc, zone }

func (f *registryFleet) Targets(_ context.Context, tenant canon.TenantID, stores []canon.StoreID, tier string) ([]otaports.Target, error) {
	if f.svc == nil {
		return nil, nil
	}
	candidates := stores
	if len(candidates) == 0 {
		candidates = f.svc.Stores()
	}
	var out []otaports.Target
	for _, store := range candidates {
		for _, d := range f.svc.DevicesForOTA(store, tier) {
			if d.TenantID != tenant || d.Kind != regdomain.KindLabel {
				continue
			}
			// A device that has not reported yet is assumed healthy rather than
			// flat. Reporting zero would make the OTA service skip it as
			// low-battery, which is the wrong failure: "we have not heard from
			// it" and "its cell is nearly gone" are different facts and only
			// the second is a reason not to send it firmware.
			battery := 100
			if d.LastTelemetry != nil && d.LastTelemetry.BatteryPct > 0 {
				battery = d.LastTelemetry.BatteryPct
			}
			out = append(out, otaports.Target{
				DeviceID: d.ID, StoreID: d.Placement.StoreID, SECID: d.Placement.SECID,
				Zone: d.Placement.Zone, HardwareTier: d.HardwareTier,
				FirmwareVersion: d.FirmwareVersion, BatteryPct: battery, TimeZone: f.zone,
			})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Pricing
// ---------------------------------------------------------------------------

func (s *Stack) startPricing(ctx context.Context, c *cloudServices) error {
	rt, err := s.runtimeFor("pricing-service", s.adminPort(5))
	if err != nil {
		return err
	}
	kv, err := s.kvFor("pricing-service", rt, kvstore.SyncEvery)
	if err != nil {
		return err
	}
	svc, err := pricing.New(pricing.Config{
		State: kv, Bus: s.log,
		ConstraintSource: &pricingports.StaticConstraints{
			UseDefault: true,
			ByKey:      map[string]pricingdomain.Constraints{},
			Default:    pricingdomain.Constraints{Currency: s.cfg.Currency},
		},
		Registry: rt.Metrics, Log: rt.Log, Tracer: rt.Tracer, Standard: rt.Standard,
		ElasticityPolicy: ml.ElasticityPolicy{
			MinObservations: 12, MinDistinctPrices: 3,
			MaxCIWidth: 2.0, RequireNegative: true, ConfidenceLevel: 0.95,
		},
		AnomalyContamination:    0.005,
		AnomalyRingSize:         2048,
		MaxQuantisationDeltaPct: 10,
		Streams:                 devStreams(s.cfg.DevPartitions),
	})
	if err != nil {
		return fmt.Errorf("usslpd: assembling the pricing service: %w", err)
	}
	if err := svc.EnsureStreams(ctx); err != nil {
		return fmt.Errorf("usslpd: pricing streams: %w", err)
	}
	for name, check := range svc.ReadinessChecks() {
		rt.Health.Register(name, check)
	}
	if err := svc.Start(ctx, s.spawn(rt.Log)); err != nil {
		return fmt.Errorf("usslpd: starting the pricing service: %w", err)
	}
	s.push("pricing-service drain", func(ctx context.Context) error { return svc.Shutdown(ctx) })

	ln, err := s.listen("pricing-service", s.cfg.Ports.Pricing)
	if err != nil {
		return err
	}
	s.serve("pricing-service", ln, svc.Handler(), 120*time.Second)

	c.pricing = svc
	c.pricingURL = "http://" + ln.Addr().String()
	c.admin["pricing-service"] = "http://" + rt.Admin.Addr()
	rt.Ready()
	return nil
}

// ---------------------------------------------------------------------------
// Promotion
// ---------------------------------------------------------------------------

func (s *Stack) startPromotion(ctx context.Context, c *cloudServices) error {
	rt, err := s.runtimeFor("promotion-service", s.adminPort(6))
	if err != nil {
		return err
	}
	kv, err := s.kvFor("promotion-service", rt, kvstore.SyncEvery)
	if err != nil {
		return err
	}
	dir := &promoports.StaticDirectory{
		ZoneOf: map[canon.StoreID]string{}, ClusterOf: map[canon.StoreID]string{},
		DefaultZone: "UTC",
	}
	for _, tenant := range s.cfg.Tenants {
		for i := 0; i < s.cfg.Stores; i++ {
			dir.ZoneOf[StoreIDFor(tenant, i)] = "UTC"
		}
	}
	svc, err := promotion.New(promotion.Config{
		State: kv, Bus: s.log, Directory: dir,
		Registry: rt.Metrics, Log: rt.Log, Tracer: rt.Tracer, Standard: rt.Standard,
		// One second rather than a minute: the sweep is what turns a scheduled
		// promotion into an active one, and a demonstration that waits a minute
		// to see it is a demonstration nobody watches.
		SweepInterval: time.Second,
		Streams:       devStreams(s.cfg.DevPartitions),
	})
	if err != nil {
		return fmt.Errorf("usslpd: assembling the promotion service: %w", err)
	}
	if err := svc.EnsureStreams(ctx); err != nil {
		return fmt.Errorf("usslpd: promotion streams: %w", err)
	}
	for name, check := range svc.ReadinessChecks() {
		rt.Health.Register(name, check)
	}
	tenants := append([]canon.TenantID(nil), s.cfg.Tenants...)
	go func() {
		if err := svc.RunSweeper(s.backgroundCtx(), func() []canon.TenantID { return tenants }); err != nil &&
			!errors.Is(err, context.Canceled) {
			rt.Log.Error("activation sweeper stopped", "error", err)
		}
	}()
	s.push("promotion-service drain", func(ctx context.Context) error { return svc.Shutdown(ctx) })

	ln, err := s.listen("promotion-service", s.cfg.Ports.Promotion)
	if err != nil {
		return err
	}
	s.serve("promotion-service", ln, svc.Handler(), 120*time.Second)

	c.promotion = svc
	c.promotionURL = "http://" + ln.Addr().String()
	c.admin["promotion-service"] = "http://" + rt.Admin.Addr()
	rt.Ready()
	return nil
}

// ---------------------------------------------------------------------------
// Analytics
// ---------------------------------------------------------------------------

func (s *Stack) startAnalytics(ctx context.Context, c *cloudServices) error {
	rt, err := s.runtimeFor("analytics-service", s.adminPort(7))
	if err != nil {
		return err
	}
	svc, err := analytics.New(analytics.Config{
		DataDir: filepath.Join(s.cfg.DataDir, "analytics"), Bus: s.log,
		Registry: rt.Metrics, Log: rt.Log, Tracer: rt.Tracer, Standard: rt.Standard,
		// A one-second flush rather than the default. The SLO report is read
		// straight after a run of price changes in the demo and in the tests,
		// and a report that reflects the state of a minute ago reads as a bug.
		FlushInterval: time.Second,
		Streams:       devStreams(s.cfg.DevPartitions),
	})
	if err != nil {
		return fmt.Errorf("usslpd: assembling the analytics service: %w", err)
	}
	if err := svc.EnsureStreams(ctx); err != nil {
		return fmt.Errorf("usslpd: analytics streams: %w", err)
	}
	for name, check := range svc.ReadinessChecks() {
		rt.Health.Register(name, check)
	}
	if err := svc.Start(ctx, s.spawn(rt.Log)); err != nil {
		return fmt.Errorf("usslpd: starting the analytics service: %w", err)
	}
	s.push("analytics-service drain", func(ctx context.Context) error { return svc.Shutdown(ctx) })

	ln, err := s.listen("analytics-service", s.cfg.Ports.Analytics)
	if err != nil {
		return err
	}
	s.serve("analytics-service", ln, svc.Handler(), 120*time.Second)

	c.analytics = svc
	c.analyticsURL = "http://" + ln.Addr().String()
	c.admin["analytics-service"] = "http://" + rt.Admin.Addr()
	rt.Ready()
	return nil
}

// ---------------------------------------------------------------------------
// API Gateway
// ---------------------------------------------------------------------------

func (s *Stack) startAPIGateway(ctx context.Context, c *cloudServices) error {
	rt, err := s.runtimeFor("api-gateway", s.adminPort(0))
	if err != nil {
		return err
	}
	keyStore := apigw.NewMemoryKeyStore()
	issuer, err := apigw.NewKeyIssuer(apigw.KeyIssuerConfig{
		Store: keyStore, Prefix: apigw.KeyPrefixLive,
		// The key-derivation cost is a deliberate defence against an offline
		// attack on a stolen key store. It is also paid on every single
		// request, and this deployment's threat model is "a laptop", so it is
		// turned down to keep the gateway out of the latency budget it is
		// meant to be measuring.
		Iterations: 4096,
	})
	if err != nil {
		return fmt.Errorf("usslpd: building the API key issuer: %w", err)
	}

	upstreams := []apigw.UpstreamConfig{
		{Name: apigw.UpstreamUIG, Address: c.uigURL},
		{Name: apigw.UpstreamLabel, Address: c.labelURL},
		{Name: apigw.UpstreamRegistry, Address: c.registryURL},
		{Name: apigw.UpstreamOTA, Address: c.otaURL},
		{Name: apigw.UpstreamPricing, Address: c.pricingURL},
		{Name: apigw.UpstreamPromotion, Address: c.promotionURL},
		{Name: apigw.UpstreamAnalytics, Address: c.analyticsURL},
	}

	gw, err := apigw.New(apigw.Config{
		Service: "api-gateway", Version: rt.Version,
		Log: rt.Log, Tracer: rt.Tracer, Registry: rt.Metrics, Health: rt.Health,
		Auth:      apigw.AuthConfig{Keys: issuer},
		Keys:      issuer,
		Upstreams: upstreams,
		Source:    &apigw.BusSource{Bus: s.log, Group: "api-gateway-" + canon.NewSpanID(), Log: rt.Log},
	})
	if err != nil {
		return fmt.Errorf("usslpd: assembling the API gateway: %w", err)
	}
	gw.Start(ctx)
	s.push("api-gateway drain", func(ctx context.Context) error { return gw.Shutdown(ctx) })

	// One owner key per tenant, minted at boot. A gateway whose only
	// key-issuing endpoint requires a key cannot be started otherwise, and the
	// alternative — an unauthenticated escape hatch — is the kind of
	// development convenience that ships.
	c.tenantKeys = map[canon.TenantID]string{}
	for _, tenant := range s.cfg.Tenants {
		_, plaintext, err := issuer.Issue(ctx, apigw.IssueRequest{
			TenantID: tenant, Name: "usslpd-owner",
			Roles: []apigw.Role{apigw.RoleOwner}, TTL: 24 * time.Hour,
			CreatedBy: "usslpd",
		})
		if err != nil {
			return fmt.Errorf("usslpd: issuing the bootstrap key for %s: %w", tenant, err)
		}
		c.tenantKeys[tenant] = plaintext
	}
	c.bootstrapKey = c.tenantKeys[s.cfg.Tenants[0]]

	ln, err := s.listen("api-gateway", s.cfg.Ports.APIGateway)
	if err != nil {
		return err
	}
	// No write timeout: it would apply to the WebSocket stream too, and a
	// deadline on a connection meant to live for hours closes every console at
	// the same interval.
	srv := &http.Server{
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			rt.Log.Error("http server stopped", "error", err)
		}
	}()
	s.push("api-gateway http", func(ctx context.Context) error { return srv.Shutdown(ctx) })

	c.gateway = gw
	c.gatewayURL = "http://" + ln.Addr().String()
	c.admin["api-gateway"] = "http://" + rt.Admin.Addr()
	rt.Ready()
	return nil
}

// ---------------------------------------------------------------------------
// Accessors
// ---------------------------------------------------------------------------

// Services exposes the assembled cloud tier, so a test can call a use case
// directly rather than through HTTP when HTTP is not what it is testing.
func (s *Stack) Services() *cloudServices { return s.cloudSvcs }

// Registry is the Device Registry.
func (c *cloudServices) Registry() *regapp.Service { return c.registry }

// Label is the Label Service.
func (c *cloudServices) Label() *label.Service { return c.label }

// OTA is the rollout controller.
func (c *cloudServices) OTA() *otaapp.Controller { return c.ota }

// Pricing is the pricing service.
func (c *cloudServices) Pricing() *pricing.Service { return c.pricing }

// Promotion is the promotion service.
func (c *cloudServices) Promotion() *promotion.Service { return c.promotion }

// Analytics is the analytics service.
func (c *cloudServices) Analytics() *analytics.Service { return c.analytics }

// URLs of each service's business surface.
func (c *cloudServices) GatewayURL() string   { return c.gatewayURL }
func (c *cloudServices) UIGURL() string       { return c.uigURL }
func (c *cloudServices) LabelURL() string     { return c.labelURL }
func (c *cloudServices) RegistryURL() string  { return c.registryURL }
func (c *cloudServices) OTAURL() string       { return c.otaURL }
func (c *cloudServices) PricingURL() string   { return c.pricingURL }
func (c *cloudServices) PromotionURL() string { return c.promotionURL }
func (c *cloudServices) AnalyticsURL() string { return c.analyticsURL }

// APIKey returns a tenant's bootstrap owner key.
func (c *cloudServices) APIKey(tenant canon.TenantID) string { return c.tenantKeys[tenant] }

// AdminURLs are the per-service /metrics, /healthz and /readyz surfaces.
func (c *cloudServices) AdminURLs() map[string]string { return c.admin }

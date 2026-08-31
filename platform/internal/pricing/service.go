// Package pricing wires the AI Pricing Engine: the three decision tiers, the
// model registry, the point-in-time feature store, the anomaly detector and the
// HTTP surface that exposes them.
//
// # The three tiers, and why they are three
//
// A price decision in USSLP is made at whichever tier can afford to make it.
//
//   - Tier 1, the rules engine, is pure, deterministic and under a millisecond.
//     It runs in this service, and the same code runs inside the Store Gateway
//     Unit from a compact policy pack, so a store that has lost its WAN reaches
//     the identical decision the cloud would have reached. Every price the
//     platform ever displays passes through it.
//   - Tier 2, edge ML, adds a per-store demand model and an expected-margin
//     optimiser inside an 8-15 millisecond budget. It answers "what should this
//     price be", and its answer is always inside Tier 1's feasible set, so it
//     cannot propose something that then has to be clamped.
//   - Tier 3, cloud optimisation, runs asynchronously every fifteen minutes
//     across stores, ensembling a sequence model with the trees and correcting
//     for the volume substitute SKUs take from each other. It is the only tier
//     that can see that discounting four brands of the same thing wins nothing.
//
// # Tenancy
//
// The tenant comes from the X-USSLP-Tenant header, set by the API Gateway after
// it has authenticated the caller. As in the Label Service, that is safe only
// behind the gateway: a deployment exposing this port directly must terminate
// mTLS here and derive the tenant from the peer certificate.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/pricing/app"
	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/features"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/internal/pricing/ports"
	"github.com/usslp/usslp/platform/internal/pricing/registry"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// TenantHeader carries the authenticated tenant.
const TenantHeader = "X-USSLP-Tenant"

// Consumer group names. As elsewhere in the platform they are constants,
// because a group's identity is its committed offsets and renaming one replays
// the stream from the beginning.
const (
	// GroupTelemetry consumes label-telemetry for anomaly detection.
	GroupTelemetry = "pricing-service.telemetry"
	// GroupPrices consumes price-updates to keep the feature store current.
	GroupPrices = "pricing-service.prices"
	// GroupInventory consumes inventory-sync for stock features.
	GroupInventory = "pricing-service.inventory"
)

// Config is everything the service needs.
type Config struct {
	// State backs the feature store and the model registry.
	State *kvstore.Store
	// Bus is the event stream. Nil disables the consumers, which is what a
	// unit test and a Tier-1-only edge deployment both want.
	Bus eventbus.Bus
	// ConstraintSource resolves per-(store, SKU) Tier-1 rules.
	ConstraintSource ports.ConstraintSource
	// Registry, Log, Tracer and Standard come from obs.Runtime.
	Registry *obs.Registry
	Log      *obs.Logger
	Tracer   *obs.Tracer
	Standard *obs.StandardMetrics
	// Clock is injected so the point-in-time tests can control "now".
	Clock ports.Clock
	// FeatureRetention bounds feature history.
	FeatureRetention time.Duration
	// ElasticityPolicy is the evidence bar an elasticity estimate must clear.
	ElasticityPolicy ml.ElasticityPolicy
	// AnomalyContamination is the fraction of a fleet expected to be anomalous.
	AnomalyContamination float64
	// AnomalyRingSize bounds the in-memory flag ring served by GET /v1/anomalies.
	AnomalyRingSize int
	// MaxQuantisationDeltaPct refuses to promote a model whose int8 artefact
	// lost more than this much accuracy. Zero disables the check.
	MaxQuantisationDeltaPct float64
	// Streams overrides the catalogue EnsureStreams provisions.
	Streams []canon.Stream
}

// Service is the assembled pricing engine.
type Service struct {
	cfg      Config
	features *features.Store
	models   *registry.Registry
	log      *obs.Logger
	tracer   *obs.Tracer
	clock    ports.Clock
	metrics  *serviceMetrics

	// mu guards the mutable serving state: the loaded champion models and the
	// anomaly detector. Model reloads are rare and reads are on the hot path,
	// so this is an RWMutex rather than an atomic swap of a whole state struct;
	// the difference is unmeasurable at this write rate and the code is
	// clearer.
	mu       sync.RWMutex
	demand   map[string]*ml.GBT
	detector *app.AnomalyDetector

	consumers []eventbus.Consumer
	closeOnce sync.Once
}

// serviceMetrics are the series this service publishes.
type serviceMetrics struct {
	decisions   *obs.CounterVec
	tierLatency *obs.HistogramVec
	anomalies   *obs.CounterVec
	modelLoads  *obs.CounterVec
	elasticity  *obs.CounterVec
}

func newServiceMetrics(r *obs.Registry) *serviceMetrics {
	if r == nil {
		r = obs.NewRegistry()
	}
	return &serviceMetrics{
		decisions: r.Counter("usslp_pricing_decisions_total",
			"Tier-1 decisions by outcome", "outcome"),
		// The buckets straddle the tier budgets: 10 ms for Tier 1 and 15 ms for
		// Tier 2. A histogram whose buckets do not bracket the SLO cannot
		// answer whether the SLO was met.
		tierLatency: r.Histogram("usslp_pricing_tier_seconds",
			"Decision latency by tier",
			[]float64{0.0001, 0.0005, 0.001, 0.002, 0.005, 0.008, 0.010, 0.015, 0.025, 0.05, 0.1, 0.5},
			"tier"),
		anomalies: r.Counter("usslp_pricing_anomalies_total",
			"Telemetry anomalies flagged, by driving feature", "feature"),
		modelLoads: r.Counter("usslp_pricing_model_loads_total",
			"Champion model loads by result", "result"),
		elasticity: r.Counter("usslp_pricing_elasticity_total",
			"Elasticity estimates by usability", "usable"),
	}
}

// New assembles the service.
func New(cfg Config) (*Service, error) {
	if cfg.State == nil {
		return nil, fmt.Errorf("pricing: a state store is required")
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Clock == nil {
		cfg.Clock = ports.SystemClock{}
	}
	if cfg.AnomalyRingSize <= 0 {
		cfg.AnomalyRingSize = 2048
	}
	fs, err := features.New(features.Config{KV: cfg.State, Retention: cfg.FeatureRetention})
	if err != nil {
		return nil, err
	}
	reg, err := registry.New(cfg.State)
	if err != nil {
		return nil, err
	}
	s := &Service{
		cfg: cfg, features: fs, models: reg,
		log: cfg.Log, tracer: cfg.Tracer, clock: cfg.Clock,
		metrics: newServiceMetrics(cfg.Registry),
		demand:  map[string]*ml.GBT{},
	}
	return s, nil
}

// Features exposes the feature store, for the training jobs and for tests.
func (s *Service) Features() *features.Store { return s.features }

// Models exposes the model registry.
func (s *Service) Models() *registry.Registry { return s.models }

// SetDetector installs an anomaly detector, replacing any current one.
func (s *Service) SetDetector(d *app.AnomalyDetector) {
	s.mu.Lock()
	s.detector = d
	s.mu.Unlock()
}

// Detector returns the installed anomaly detector, or nil.
func (s *Service) Detector() *app.AnomalyDetector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.detector
}

// EnsureStreams provisions the streams this service reads and writes.
func (s *Service) EnsureStreams(ctx context.Context) error {
	if s.cfg.Bus == nil {
		return nil
	}
	streams := s.cfg.Streams
	if len(streams) == 0 {
		streams = []canon.Stream{
			canon.StreamPriceUpdates, canon.StreamTelemetry,
			canon.StreamInventory, canon.StreamPromotions, canon.StreamDLQ,
		}
	}
	return s.cfg.Bus.EnsureStreams(ctx, streams...)
}

// ReadinessChecks are registered on the admin server's readiness endpoint.
//
// Readiness only, never liveness: a model that has not loaded means this
// instance should not take traffic, and it emphatically does not mean the
// process should be restarted — a restart loses the loaded models of every
// other store it serves.
func (s *Service) ReadinessChecks() map[string]obs.Check {
	return map[string]obs.Check{
		"state_store": func(ctx context.Context) error {
			if _, err := s.cfg.State.Has([]byte("readiness-probe")); err != nil {
				return fmt.Errorf("state store unreadable: %w", err)
			}
			return nil
		},
	}
}

// evaluate is the Tier-1 hot path.
func (s *Service) evaluate(c domain.Constraints, requested int64) domain.Decision {
	start := time.Now()
	d := domain.Evaluate(c, requested)
	s.metrics.tierLatency.With("1").Observe(time.Since(start).Seconds())
	s.metrics.decisions.With(d.Outcome.String()).Inc()
	return d
}

// demandModel returns the champion demand model for a slot, loading it on first
// use.
//
// Lazily rather than at start-up: a chain has thousands of stores and this
// service is deployed as a stateless pool, so an instance loads only the models
// for the stores whose traffic it actually receives. Loading every model at
// start-up would make a rolling deploy an hour long and would put a tenant's
// whole model corpus in every pod's heap.
func (s *Service) demandModel(slot registry.Slot) (*ml.GBT, error) {
	key := slot.Tenant.String() + "\x00" + slot.Store.String()
	s.mu.RLock()
	m, ok := s.demand[key]
	s.mu.RUnlock()
	if ok {
		return m, nil
	}
	names := make([]string, domain.NumFeatures)
	copy(names, domain.FeatureNames[:])
	loaded, _, err := s.models.LoadChampionGBT(slot, names)
	if err != nil {
		s.metrics.modelLoads.With("miss").Inc()
		return nil, err
	}
	s.mu.Lock()
	s.demand[key] = loaded
	s.mu.Unlock()
	s.metrics.modelLoads.With("ok").Inc()
	return loaded, nil
}

// InvalidateModels drops the cached champions for a tenant, so the next request
// reloads. It is called after a promotion; without it a promotion would take
// effect only as instances happened to restart.
func (s *Service) InvalidateModels(tenant canon.TenantID) {
	s.mu.Lock()
	for k := range s.demand {
		if len(k) >= len(tenant) && k[:len(tenant)] == string(tenant) {
			delete(s.demand, k)
		}
	}
	s.mu.Unlock()
}

// Start launches the background consumers.
func (s *Service) Start(ctx context.Context, spawn func(name string, fn func(context.Context) error)) error {
	if s.cfg.Bus == nil {
		return nil
	}
	telemetry, err := s.cfg.Bus.Subscribe(eventbus.SubscribeOptions{
		Group: GroupTelemetry, Topics: []string{canon.StreamTelemetry.Name},
		// Telemetry is order-insensitive per label for anomaly scoring, so the
		// consumer runs several handlers per partition. This is the one
		// consumer in the pricing service where that is safe.
		Concurrency: 4,
	}.WithDefaults())
	if err != nil {
		return err
	}
	s.consumers = append(s.consumers, telemetry)
	spawn("telemetry-consumer", func(ctx context.Context) error {
		return telemetry.Run(ctx, s.handleTelemetry)
	})

	prices, err := s.cfg.Bus.Subscribe(eventbus.SubscribeOptions{
		Group: GroupPrices, Topics: []string{canon.StreamPriceUpdates.Name},
	}.WithDefaults())
	if err != nil {
		return err
	}
	s.consumers = append(s.consumers, prices)
	spawn("price-consumer", func(ctx context.Context) error {
		return prices.Run(ctx, s.handlePriceUpdate)
	})
	return nil
}

// handleTelemetry scores a telemetry batch for anomalies and records the raw
// signals as features.
func (s *Service) handleTelemetry(ctx context.Context, m eventbus.Message) error {
	var env canon.Envelope
	if err := decodeEnvelope(m.Value, &env); err != nil {
		// A malformed envelope will never become well-formed; returning nil
		// commits the offset and lets the consumer's own dead-letter path deal
		// with it rather than wedging the partition.
		s.log.Warn("undecodable telemetry envelope", "error", err, "offset", m.Offset)
		return nil
	}
	var batch []canon.Telemetry
	if err := env.Decode(&batch); err != nil {
		var single canon.Telemetry
		if err2 := env.Decode(&single); err2 != nil {
			s.log.Warn("undecodable telemetry payload", "error", err, "offset", m.Offset)
			return nil
		}
		batch = []canon.Telemetry{single}
	}
	det := s.Detector()
	if det == nil {
		return nil
	}
	for _, t := range batch {
		prev := s.previousTelemetry(env.TenantID, t.LabelID)
		feats := app.TelemetryFeaturesFrom(t, prev)
		rec := det.Evaluate(t, env.TenantID, feats)
		if rec.Score >= det.Threshold() {
			s.metrics.anomalies.With(rec.Feature).Inc()
		}
		s.rememberTelemetry(env.TenantID, t)
	}
	return nil
}

// telemetryKey is the last-report cache key. The previous report is needed for
// the discharge-rate feature and is kept in the state store rather than in
// memory so that a restarted instance does not lose every label's history.
func telemetryKey(tenant canon.TenantID, label canon.LabelID) []byte {
	return []byte("tl\x00" + string(tenant) + "\x00" + string(label))
}

func (s *Service) previousTelemetry(tenant canon.TenantID, label canon.LabelID) canon.Telemetry {
	raw, err := s.cfg.State.Get(telemetryKey(tenant, label))
	if err != nil {
		return canon.Telemetry{}
	}
	var t canon.Telemetry
	if err := decodeJSON(raw, &t); err != nil {
		return canon.Telemetry{}
	}
	return t
}

func (s *Service) rememberTelemetry(tenant canon.TenantID, t canon.Telemetry) {
	blob, err := encodeJSON(t)
	if err != nil {
		return
	}
	// Seven days: long enough that a label reporting every five minutes always
	// has a predecessor, short enough that a decommissioned label's row expires
	// without a sweep.
	_ = s.cfg.State.PutTTL(telemetryKey(tenant, t.LabelID), blob, 7*24*time.Hour)
}

// handlePriceUpdate records an accepted price as a feature observation.
//
// KnownAt is the envelope's RecordedAt — the moment USSLP took durable
// responsibility — and ValidFrom is the price's effective date. For a
// forward-dated price change those differ by days, and getting them the right
// way round is what stops a model learning from a price the store had not yet
// displayed.
func (s *Service) handlePriceUpdate(ctx context.Context, m eventbus.Message) error {
	var env canon.Envelope
	if err := decodeEnvelope(m.Value, &env); err != nil {
		s.log.Warn("undecodable price envelope", "error", err, "offset", m.Offset)
		return nil
	}
	var p canon.PriceUpdated
	if err := env.Decode(&p); err != nil {
		s.log.Warn("undecodable price payload", "error", err, "offset", m.Offset)
		return nil
	}
	knownAt := env.RecordedAt
	if knownAt.IsZero() {
		knownAt = s.clock.Now()
	}
	return s.features.Put(features.Key{
		Tenant: env.TenantID, Store: p.StoreID, SKU: p.SKU,
		Name: domain.FeatureNames[domain.FeatPrice],
	}, features.Value{
		Number: float64(p.Price.Amount), ValidFrom: p.EffectiveAt, KnownAt: knownAt,
		Source: "price-updates",
	})
}

// Shutdown stops the consumers.
func (s *Service) Shutdown(ctx context.Context) error {
	var err error
	s.closeOnce.Do(func() {
		for _, c := range s.consumers {
			if cerr := c.Close(); cerr != nil && err == nil {
				err = cerr
			}
		}
	})
	if err != nil && !errors.Is(err, eventbus.ErrClosed) {
		return err
	}
	return nil
}

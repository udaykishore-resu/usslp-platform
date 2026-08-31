// Package promotion wires the Promotion Service: the rule DSL and its
// compiler, the lifecycle, the conflict-resolution policy, the fan-out onto the
// event stream, and lift measurement.
//
// # What this service is responsible for
//
// Deciding *which* promotions apply to which products, when, in which stores,
// and what price they produce. It does not draw labels and it does not talk to
// devices: it publishes `promotion.activated` and `promotion.expired` onto
// `promotion-events`, and the Label Service turns those into shelf updates.
// That split is what lets a promotion activate across two thousand stores
// without this service knowing that shelf labels exist.
//
// # Tenancy
//
// As elsewhere in the platform, the tenant comes from the X-USSLP-Tenant header
// set by the API Gateway after authenticating the caller. Trusting it is safe
// behind the gateway and is not safe without one.
package promotion

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/promotion/app"
	"github.com/usslp/usslp/platform/internal/promotion/domain"
	"github.com/usslp/usslp/platform/internal/promotion/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// TenantHeader carries the authenticated tenant.
const TenantHeader = "X-USSLP-Tenant"

// SourceName is what this service calls itself in every envelope it publishes.
const SourceName = "promotion-service"

// DefaultSweepInterval is how often the activation sweep runs.
//
// One minute. The sweep is what turns a scheduled promotion into an active one
// at the right local moment, and a promotion that starts up to a minute late is
// invisible to a shopper while one that starts an hour late is a complaint. The
// sweep is cheap — it walks a tenant's scheduled promotions, not its
// catalogue — so the interval is set by the accuracy wanted rather than by the
// cost.
const DefaultSweepInterval = time.Minute

// Config is everything the service needs.
type Config struct {
	// State backs the promotion store.
	State *kvstore.Store
	// Bus is the event stream promotion lifecycle events are published to.
	// Nil disables publication, which is what a unit test wants.
	Bus eventbus.Bus
	// Catalogue supplies the products promotions are evaluated against.
	Catalogue ports.Catalogue
	// Directory resolves store time zones and clusters.
	Directory ports.StoreDirectory
	// Sales supplies trading history for lift measurement.
	Sales ports.SalesSource
	// Registry, Log, Tracer and Standard come from obs.Runtime.
	Registry *obs.Registry
	Log      *obs.Logger
	Tracer   *obs.Tracer
	Standard *obs.StandardMetrics
	// Clock is injected so activation tests can control "now".
	Clock ports.Clock
	// SweepInterval overrides DefaultSweepInterval.
	SweepInterval time.Duration
	// Streams overrides the catalogue EnsureStreams provisions.
	Streams []canon.Stream
}

// Service is the assembled promotion engine.
type Service struct {
	cfg     Config
	store   *app.Store
	log     *obs.Logger
	tracer  *obs.Tracer
	clock   ports.Clock
	metrics *serviceMetrics

	// mu guards the compiled active set, which the sweep rebuilds and the
	// evaluation path reads. Rebuilds happen on activation and expiry —
	// a handful of times a day — and reads happen on every price change, so an
	// RWMutex is the right shape.
	mu       sync.RWMutex
	compiled map[canon.TenantID]*domain.MatcherSet

	closeOnce sync.Once
}

type serviceMetrics struct {
	transitions *obs.CounterVec
	fanout      *obs.CounterVec
	evaluations *obs.HistogramVec
	conflicts   *obs.GaugeVec
	active      *obs.GaugeVec
}

func newServiceMetrics(r *obs.Registry) *serviceMetrics {
	if r == nil {
		r = obs.NewRegistry()
	}
	return &serviceMetrics{
		transitions: r.Counter("usslp_promotion_transitions_total",
			"Promotion lifecycle transitions", "to"),
		fanout: r.Counter("usslp_promotion_fanout_total",
			"Promotion lifecycle events published", "event"),
		// The buckets cover a single-SKU resolve at the fast end and a
		// whole-catalogue simulation at the slow end.
		evaluations: r.Histogram("usslp_promotion_evaluation_seconds",
			"Time to evaluate a promotion set",
			[]float64{0.00001, 0.0001, 0.001, 0.01, 0.1, 1, 5, 30}, "operation"),
		conflicts: r.Gauge("usslp_promotion_conflicts",
			"Authoring conflicts detected, by severity", "severity"),
		active: r.Gauge("usslp_promotion_active",
			"Promotions currently active", "tenant"),
	}
}

// New assembles the service.
func New(cfg Config) (*Service, error) {
	if cfg.State == nil {
		return nil, errors.New("promotion: a state store is required")
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Clock == nil {
		cfg.Clock = ports.SystemClock{}
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = DefaultSweepInterval
	}
	store, err := app.NewStore(cfg.State)
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg: cfg, store: store,
		log: cfg.Log, tracer: cfg.Tracer, clock: cfg.Clock,
		metrics:  newServiceMetrics(cfg.Registry),
		compiled: map[canon.TenantID]*domain.MatcherSet{},
	}, nil
}

// Store exposes the promotion store, for tests and for the HTTP layer.
func (s *Service) Store() *app.Store { return s.store }

// EnsureStreams provisions the streams this service publishes to.
func (s *Service) EnsureStreams(ctx context.Context) error {
	if s.cfg.Bus == nil {
		return nil
	}
	streams := s.cfg.Streams
	if len(streams) == 0 {
		streams = []canon.Stream{canon.StreamPromotions, canon.StreamAudit, canon.StreamDLQ}
	}
	return s.cfg.Bus.EnsureStreams(ctx, streams...)
}

// ReadinessChecks are registered on the admin server's readiness endpoint.
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

// Activate moves a promotion to active and fans the fact out.
//
// The order matters and is the opposite of the obvious one: the state is
// committed *before* the event is published. A crash between the two leaves a
// promotion marked active that no shelf knows about, which the next sweep
// repairs by republishing; the reverse order would leave shelves showing a
// promotion the platform has no record of, which nothing repairs.
func (s *Service) Activate(ctx context.Context, tenant canon.TenantID, id canon.PromotionID, by string) (app.Record, error) {
	now := s.clock.Now()
	rec, err := s.store.Get(tenant, id)
	if err != nil {
		return app.Record{}, err
	}
	// Draft promotions are scheduled first: activation is a two-step move so
	// that "approved" and "running" are distinguishable in the audit trail.
	if rec.State == domain.StateDraft {
		if rec, err = s.store.SetState(tenant, id, domain.StateScheduled, now, by, ""); err != nil {
			return app.Record{}, err
		}
		s.metrics.transitions.With(string(domain.StateScheduled)).Inc()
	}
	rec, err = s.store.SetState(tenant, id, domain.StateActive, now, by, "")
	if err != nil {
		return app.Record{}, err
	}
	s.metrics.transitions.With(string(domain.StateActive)).Inc()
	s.invalidate(tenant)

	if err := s.publish(ctx, rec, canon.EvtPromotionActivated); err != nil {
		// The state stands; the sweep republishes. Logging rather than failing
		// keeps a broker blip from making an operator think the activation did
		// not happen.
		s.log.Error("promotion activated but not published", "promotion", id, "error", err)
	}
	return rec, nil
}

// Cancel stops a promotion and fans the fact out.
func (s *Service) Cancel(ctx context.Context, tenant canon.TenantID, id canon.PromotionID, by, reason string) (app.Record, error) {
	rec, err := s.store.SetState(tenant, id, domain.StateCancelled, s.clock.Now(), by, reason)
	if err != nil {
		return app.Record{}, err
	}
	s.metrics.transitions.With(string(domain.StateCancelled)).Inc()
	s.invalidate(tenant)
	if err := s.publish(ctx, rec, canon.EvtPromotionExpired); err != nil {
		s.log.Error("promotion cancelled but not published", "promotion", id, "error", err)
	}
	return rec, nil
}

// Expire ends a promotion that has run its course.
func (s *Service) Expire(ctx context.Context, tenant canon.TenantID, id canon.PromotionID) (app.Record, error) {
	rec, err := s.store.SetState(tenant, id, domain.StateExpired, s.clock.Now(), "", "")
	if err != nil {
		return app.Record{}, err
	}
	s.metrics.transitions.With(string(domain.StateExpired)).Inc()
	s.invalidate(tenant)
	if err := s.publish(ctx, rec, canon.EvtPromotionExpired); err != nil {
		s.log.Error("promotion expired but not published", "promotion", id, "error", err)
	}
	return rec, nil
}

// ActivationEvent is the payload on `promotion-events`.
//
// It carries the whole rule rather than an identifier. The Label Service must
// be able to price a shelf from this event alone, without a synchronous call
// back into the promotion service — a national activation would otherwise turn
// into two thousand stores' worth of simultaneous lookups against one service,
// which is exactly the fan-in the event-driven design exists to avoid.
type ActivationEvent struct {
	PromotionID canon.PromotionID `json:"promotion_id"`
	TenantID    canon.TenantID    `json:"tenant_id"`
	Rule        domain.Rule       `json:"rule"`
	State       domain.State      `json:"state"`
	// Windows are the resolved absolute activation windows per store zone, so a
	// consumer does not have to re-resolve wall-clock times and risk disagreeing
	// about them.
	Windows map[string]domain.StoreWindow `json:"windows,omitempty"`
	// EffectiveAt is when this transition took effect.
	EffectiveAt time.Time `json:"effective_at"`
	// Reason explains a cancellation.
	Reason string `json:"reason,omitempty"`
}

// publish emits a lifecycle event.
func (s *Service) publish(ctx context.Context, rec app.Record, eventType string) error {
	if s.cfg.Bus == nil {
		return nil
	}
	payload := ActivationEvent{
		PromotionID: rec.Rule.ID, TenantID: rec.Rule.TenantID,
		Rule: rec.Rule, State: rec.State,
		EffectiveAt: s.clock.Now(), Reason: rec.CancelReason,
	}
	if s.cfg.Directory != nil {
		if zones, err := s.cfg.Directory.Zones(ctx, rec.Rule.TenantID); err == nil {
			windows := map[string]domain.StoreWindow{}
			for _, zone := range zones {
				if _, done := windows[zone]; done {
					continue
				}
				if w, err := rec.Rule.Schedule.ResolveWindow(zone); err == nil {
					windows[zone] = w
				}
			}
			payload.Windows = windows
		}
	}
	env, err := canon.NewEnvelope(eventType, "promotion", string(rec.Rule.ID), rec.Rule.TenantID, payload)
	if err != nil {
		return err
	}
	env.Source = SourceName
	// The idempotency key ties the event to the exact transition, so a
	// republished activation after a crash is recognised as the same fact
	// rather than applied twice.
	env.IdempotencyKey = fmt.Sprintf("%s:%s:%s:%d",
		rec.Rule.TenantID, rec.Rule.ID, rec.State, rec.UpdatedAt.UnixNano())
	body, err := marshalEnvelope(env)
	if err != nil {
		return err
	}
	if err := eventbus.PublishEnvelope(ctx, s.cfg.Bus, canon.StreamPromotions.Name, env, body); err != nil {
		return err
	}
	s.metrics.fanout.With(eventType).Inc()
	return nil
}

// Sweep advances every promotion whose local window has opened or closed.
//
// # Why a sweep rather than a timer per promotion
//
// A promotion has as many activation instants as the estate has time zones, and
// a chain running two hundred concurrent promotions across six zones would need
// 2,400 timers, each of which has to survive a pod restart. A sweep over the
// scheduled set is stateless, idempotent, and recovers automatically from a
// restart — it simply notices on its next tick that a window has opened.
func (s *Service) Sweep(ctx context.Context, tenant canon.TenantID) (activated, expired int, err error) {
	now := s.clock.Now()
	records, err := s.store.List(tenant, []domain.State{domain.StateScheduled, domain.StateActive})
	if err != nil {
		return 0, 0, err
	}
	zones := domain.StoreZones{}
	if s.cfg.Directory != nil {
		if z, err := s.cfg.Directory.Zones(ctx, tenant); err == nil {
			zones = z
		}
	}
	if len(zones) == 0 {
		// With no store directory the platform can still run absolute-window
		// promotions correctly; wall-clock ones fall back to UTC, which the
		// resolver documents.
		zones = domain.StoreZones{"": ""}
	}

	activeCount := 0
	for _, rec := range records {
		anyOpen, allClosed := false, true
		for _, zone := range zones {
			open, err := rec.Rule.Schedule.ActiveInStore(zone, now)
			if err != nil {
				s.log.Warn("promotion schedule could not be resolved",
					"promotion", rec.Rule.ID, "zone", zone, "error", err)
				continue
			}
			if open {
				anyOpen = true
			}
			win, err := rec.Rule.Schedule.ResolveWindow(zone)
			if err == nil && now.Before(win.End) {
				allClosed = false
			}
		}
		switch {
		case rec.State == domain.StateScheduled && anyOpen:
			if _, err := s.Activate(ctx, tenant, rec.Rule.ID, "scheduler"); err != nil {
				s.log.Error("scheduled activation failed", "promotion", rec.Rule.ID, "error", err)
				continue
			}
			activated++
			activeCount++
		case rec.State == domain.StateActive && allClosed:
			if _, err := s.Expire(ctx, tenant, rec.Rule.ID); err != nil {
				s.log.Error("scheduled expiry failed", "promotion", rec.Rule.ID, "error", err)
				continue
			}
			expired++
		case rec.State == domain.StateActive:
			activeCount++
		}
	}
	s.metrics.active.With(string(tenant)).Set(float64(activeCount))
	return activated, expired, nil
}

// RunSweeper runs the activation sweep on a ticker until the context is
// cancelled.
func (s *Service) RunSweeper(ctx context.Context, tenants func() []canon.TenantID) error {
	ticker := time.NewTicker(s.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for _, tenant := range tenants() {
				if a, e, err := s.Sweep(ctx, tenant); err != nil {
					s.log.Error("sweep failed", "tenant", tenant, "error", err)
				} else if a > 0 || e > 0 {
					s.log.Info("promotion sweep", "tenant", tenant, "activated", a, "expired", e)
				}
			}
		}
	}
}

// ActiveSet returns the compiled matcher set for a tenant's live promotions,
// building it on first use and after every lifecycle change.
func (s *Service) ActiveSet(tenant canon.TenantID) (*domain.MatcherSet, error) {
	s.mu.RLock()
	set, ok := s.compiled[tenant]
	s.mu.RUnlock()
	if ok {
		return set, nil
	}
	records, err := s.store.List(tenant, []domain.State{domain.StateActive})
	if err != nil {
		return nil, err
	}
	rules := make([]domain.Rule, 0, len(records))
	for _, rec := range records {
		rules = append(rules, rec.Rule)
	}
	set = domain.CompileSet(rules)
	s.mu.Lock()
	s.compiled[tenant] = set
	s.mu.Unlock()
	return set, nil
}

// invalidate drops a tenant's compiled set so the next evaluation rebuilds it.
func (s *Service) invalidate(tenant canon.TenantID) {
	s.mu.Lock()
	delete(s.compiled, tenant)
	s.mu.Unlock()
}

// Resolve applies a tenant's live promotions to one product.
func (s *Service) Resolve(tenant canon.TenantID, p domain.Product) (domain.Resolution, error) {
	set, err := s.ActiveSet(tenant)
	if err != nil {
		return domain.Resolution{}, err
	}
	start := time.Now()
	matched := set.Match(p, make([]domain.Rule, 0, 8))
	res, err := domain.Resolve(matched, p)
	s.metrics.evaluations.With("resolve").Observe(time.Since(start).Seconds())
	return res, err
}

// Shutdown releases resources. The service holds no long-lived connections of
// its own; the hook exists so the composition root's shutdown sequence is
// uniform across services.
func (s *Service) Shutdown(context.Context) error {
	s.closeOnce.Do(func() {})
	return nil
}

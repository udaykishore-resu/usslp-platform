// Package label wires the Label Service: the domain, its use cases, the
// adapters that connect them to the platform, and the HTTP surface that exposes
// them.
//
// This file is the composition root. It is the only place that knows all four
// layers exist at once, which is what keeps the dependency arrows in every
// other file pointing inwards.
//
// # Transport
//
// gRPC is the production transport; `api/label_service.proto` is its documented
// contract. The HTTP surface here is the same operations over JSON, and it is
// not a second-class debug interface: it is what the API Gateway calls, what
// the store back-office calls when the gateway is unreachable, and what a
// support engineer curls at three in the morning.
//
// # Tenancy
//
// Every request's tenant comes from the `X-USSLP-Tenant` header. In production
// that header is set by the API Gateway *after* it has authenticated the caller
// and derived the tenant from its mTLS client certificate; the service is
// deployed behind a network policy that admits traffic only from the gateway,
// so the header cannot be forged by a caller. A deployment that exposes this
// port directly must terminate mTLS here and derive the tenant from the peer
// certificate instead — trusting a client-supplied header at an open edge would
// be a cross-tenant authorisation hole.
package label

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/label/adapters"
	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// TenantHeader carries the authenticated tenant. See the package comment for
// why trusting it is safe behind the gateway and is not safe without one.
const TenantHeader = "X-USSLP-Tenant"

// Consumer group names. They are constants because a consumer group's identity
// is its committed offsets: renaming one silently replays a stream from the
// beginning, which for `price-updates` means re-publishing seven days of price
// history to every shelf in the estate.
const (
	// GroupPrice consumes the price-updates stream.
	GroupPrice = "label-service.price"
	// GroupDelivery consumes the label-delivery stream.
	GroupDelivery = "label-service.delivery"
	// GroupDevices consumes the device-events stream.
	GroupDevices = "label-service.devices"
	// GroupPromotions consumes the promotion-events stream.
	GroupPromotions = "label-service.promotions"
)

// Config is everything the service needs to be built.
type Config struct {
	// Store is the event store backing the write side.
	Store *eventstore.Store
	// ReadModels is the key/value store backing the directory, the query-side
	// read model and the schedule index.
	ReadModels *kvstore.Store
	// Bus is the event stream.
	Bus eventbus.Bus
	// Broker is the MQTT client the device tier is reached through.
	Broker msgbus.Client
	// Attestor signs authorised prices, normally a *pki.PriceAuthority.
	Attestor ports.Attestor
	// Policies resolves per-tenant guard rails. Nil means platform defaults.
	Policies *domain.PolicySet
	// Currency resolves a store's trading currency. Nil defaults to USD.
	Currency app.CurrencyResolver
	// Registry, Log and Tracer come from obs.Runtime.
	Registry *obs.Registry
	Log      *obs.Logger
	Tracer   *obs.Tracer
	// Standard are the shared per-service series.
	Standard *obs.StandardMetrics
	// Clock is the injected clock. Nil means the system clock.
	Clock ports.Clock
	// Batch tunes the fan-out pipeline.
	Batch app.BatchConfig
	// RateLimit tunes the per-tenant fan-out budget.
	RateLimit app.TenantLimiterConfig
	// Scheduler tunes the future-dated price runner.
	Scheduler app.ScheduledPriceRunnerConfig
	// IdempotencyWindow is how long a processed stream record is remembered.
	// Zero means idem.DefaultWindow.
	IdempotencyWindow time.Duration
	// SnapshotEvery is the aggregate snapshot interval.
	SnapshotEvery int64
	// ConsumerConcurrency is the in-flight handler count per partition. It stays
	// at one for the price path: ordering within a partition is the guarantee
	// that two price changes for the same product apply in the right order.
	ConsumerConcurrency int
	// Streams overrides the catalogue EnsureStreams provisions. Empty means
	// canon's.
	//
	// canon's partition counts are sized for the whole estate: 1,024 partitions
	// on price-updates so that 52,000 updates per second spread across a
	// consumer group of two hundred nodes. A Store Gateway Unit running this
	// same binary for one store has one consumer, and a partition per consumer
	// goroutine at that width costs more in scheduling than the store produces
	// in work — so a single-store deployment overrides the catalogue with
	// counts sized for it.
	Streams []canon.Stream
}

// Service is the assembled Label Service.
type Service struct {
	cfg Config

	repo      *adapters.EventStoreRepository
	directory *adapters.KVDirectory
	state     *adapters.KVStateStore
	schedules *adapters.KVScheduleStore
	device    *adapters.MQTTDevicePublisher
	streams   *adapters.BusStreamPublisher
	ack       *adapters.ACKBridge

	price     *app.UpdatePriceHandler
	delivery  *app.DeliveryConfirmationHandler
	devices   *app.DirectoryProjection
	promos    *app.PromotionHandler
	batch     *app.BatchUpdater
	scheduler *app.ScheduledPriceRunner
	view      *app.LabelStateProjection
	runner    *adapters.StateProjectionRunner

	consumers []*adapters.StreamConsumer
	metrics   *app.Metrics
	log       *obs.Logger
	tracer    *obs.Tracer
	clock     ports.Clock
}

// New assembles the service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("label: Config.Store is required")
	case cfg.ReadModels == nil:
		return nil, errors.New("label: Config.ReadModels is required")
	case cfg.Bus == nil:
		return nil, errors.New("label: Config.Bus is required")
	case cfg.Broker == nil:
		return nil, errors.New("label: Config.Broker is required")
	case cfg.Attestor == nil:
		return nil, errors.New("label: Config.Attestor is required")
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Tracer == nil {
		cfg.Tracer = obs.NewTracer(app.SourceName, 1)
	}
	if cfg.Clock == nil {
		cfg.Clock = ports.SystemClock{}
	}
	if cfg.Policies == nil {
		cfg.Policies = domain.NewPolicySet()
	}
	if cfg.ConsumerConcurrency <= 0 {
		cfg.ConsumerConcurrency = 1
	}

	s := &Service{cfg: cfg, log: cfg.Log, tracer: cfg.Tracer, clock: cfg.Clock}
	s.metrics = app.NewMetrics(cfg.Registry)

	var err error
	if s.repo, err = adapters.NewEventStoreRepository(cfg.Store, adapters.RepositoryConfig{
		SnapshotEvery: cfg.SnapshotEvery, Source: app.SourceName,
	}); err != nil {
		return nil, err
	}
	if s.directory, err = adapters.NewKVDirectory(cfg.ReadModels); err != nil {
		return nil, err
	}
	if s.state, err = adapters.NewKVStateStore(cfg.ReadModels); err != nil {
		return nil, err
	}
	if s.schedules, err = adapters.NewKVScheduleStore(cfg.ReadModels); err != nil {
		return nil, err
	}
	if s.device, err = adapters.NewMQTTDevicePublisher(cfg.Broker); err != nil {
		return nil, err
	}
	if s.streams, err = adapters.NewBusStreamPublisher(cfg.Bus, cfg.Standard); err != nil {
		return nil, err
	}

	deps := app.Deps{
		Repo: s.repo, Directory: s.directory, Attestor: cfg.Attestor,
		Device: s.device, Streams: s.streams, State: s.state, Schedules: s.schedules,
		Policies: cfg.Policies, Clock: cfg.Clock, Metrics: s.metrics,
		Log: cfg.Log, Tracer: cfg.Tracer,
	}

	guard, err := newGuard(cfg)
	if err != nil {
		return nil, err
	}
	if s.price, err = app.NewUpdatePriceHandler(deps, guard); err != nil {
		return nil, err
	}
	if s.delivery, err = app.NewDeliveryConfirmationHandler(deps); err != nil {
		return nil, err
	}
	if s.devices, err = app.NewDirectoryProjection(deps, cfg.Currency); err != nil {
		return nil, err
	}
	limiter := app.NewTenantLimiter(cfg.RateLimit)
	if s.batch, err = app.NewBatchUpdater(s.price, deps, limiter, cfg.Batch); err != nil {
		return nil, err
	}
	if s.scheduler, err = app.NewScheduledPriceRunner(s.price, deps, cfg.Scheduler); err != nil {
		return nil, err
	}
	if s.promos, err = app.NewPromotionHandler(deps, s.batch, guard); err != nil {
		return nil, err
	}
	if s.view, err = app.NewLabelStateProjection(s.state); err != nil {
		return nil, err
	}
	if s.runner, err = adapters.NewStateProjectionRunner(cfg.Store, s.state, s.view, "label-state"); err != nil {
		return nil, err
	}
	if s.ack, err = adapters.NewACKBridge(cfg.Broker, s.streams, adapters.ACKBridgeConfig{Log: cfg.Log}); err != nil {
		return nil, err
	}
	return s, nil
}

// newGuard builds the ingress de-duplication guard over the read-model store.
// It shares the store deliberately: one write-ahead log means the guard's claim
// and the read-model row it protects are one fsync, not two.
func newGuard(cfg Config) (*idem.Guard, error) {
	backend, err := idem.NewKVBackend(cfg.ReadModels, "label/idem/")
	if err != nil {
		return nil, err
	}
	opts := []idem.Option{idem.WithClock(func() time.Time { return cfg.Clock.Now() })}
	if cfg.IdempotencyWindow > 0 {
		opts = append(opts, idem.WithWindow(cfg.IdempotencyWindow))
	}
	return idem.New(backend, opts...)
}

// EnsureStreams creates the streams the service produces to and consumes from.
// Auto-creation at the bus level is deliberately unsupported, so this is where
// a fresh environment self-provisions.
func (s *Service) EnsureStreams(ctx context.Context) error {
	return s.cfg.Bus.EnsureStreams(ctx, s.streamCatalogue()...)
}

// streamCatalogue is the set of streams this service produces to and consumes
// from, with any deployment override applied.
func (s *Service) streamCatalogue() []canon.Stream {
	if len(s.cfg.Streams) > 0 {
		return s.cfg.Streams
	}
	return []canon.Stream{
		canon.StreamPriceUpdates, canon.StreamDelivery, canon.StreamDeviceEvents,
		canon.StreamPromotions, canon.StreamLabelState, canon.StreamAudit, canon.StreamDLQ,
	}
}

// Start subscribes the consumers, brings the read model up to date, starts the
// ACK bridge and the scheduled price runner, and returns.
//
// The order matters. The read model is caught up before any consumer starts, so
// a replica never serves a query from a read model it is still building; the
// ACK bridge subscribes before the price consumer, so no acknowledgement for an
// update this replica publishes can be missed.
func (s *Service) Start(ctx context.Context, run func(name string, fn func(context.Context) error)) error {
	if _, err := s.runner.CatchUp(ctx); err != nil {
		return fmt.Errorf("label: catching up the read model: %w", err)
	}
	if err := s.ack.Start(ctx); err != nil {
		return fmt.Errorf("label: subscribing to delivery acknowledgements: %w", err)
	}

	subscriptions := []struct {
		group   string
		topics  []string
		handler eventbus.Handler
		// fromBeginning starts a brand-new group at offset 0 instead of at the
		// tail.
		//
		// It is set for device-events alone. The directory is a read model
		// derived from the whole history of that stream — a replica that joined
		// at the tail would know about no label provisioned before it started,
		// and would silently decline to price them. The price and delivery
		// streams are the opposite case: a new group joining at offset 0 would
		// replay seven days of price history to every shelf in the estate.
		fromBeginning bool
	}{
		{GroupDevices, []string{canon.StreamDeviceEvents.Name}, s.devices.HandleMessage, true},
		{GroupPrice, []string{canon.StreamPriceUpdates.Name}, s.price.HandleMessage, false},
		{GroupDelivery, []string{canon.StreamDelivery.Name}, s.delivery.HandleMessage, false},
		{GroupPromotions, []string{canon.StreamPromotions.Name}, s.promos.HandleMessage, false},
	}
	for _, sub := range subscriptions {
		c, err := adapters.NewStreamConsumer(s.cfg.Bus, eventbus.SubscribeOptions{
			Group: sub.group, Topics: sub.topics,
			FromBeginning: sub.fromBeginning,
			// Concurrency stays at one for every one of these groups, and most
			// pointedly for promotions.
			//
			// `promotion-events` is keyed tenant:promo, so every transition of
			// one promotion lands on one partition in the order it happened.
			// Above a concurrency of one, ordering within a partition is no
			// longer guaranteed, and an `expired` overtaking its own
			// `activated` would revert a promotion to base prices and then
			// immediately re-apply it — leaving a whole chain discounted with
			// nothing left to switch it off. One record here is already a
			// 40,000-label fan-out with its own bounded worker pool, so the
			// parallelism that matters is inside the handler, not across it.
			Concurrency: s.cfg.ConsumerConcurrency,
		}, sub.handler, s.cfg.Standard, s.log)
		if err != nil {
			return err
		}
		s.consumers = append(s.consumers, c)
		run("consumer:"+sub.group, c.Run)
	}
	run("projection:label-state", s.runner.Run)
	run("scheduler", s.scheduler.Run)
	return nil
}

// Shutdown drains in-flight fan-out and releases the subscriptions.
//
// Draining first is the point of the whole sequence: a process that exits
// mid-fan-out leaves a store half repriced, with some shelves showing the new
// promotion and some the old price, which is worse than not having started.
func (s *Service) Shutdown(ctx context.Context) error {
	var firstErr error
	if err := s.batch.Drain(ctx); err != nil {
		firstErr = err
	}
	if err := s.ack.Stop(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	for _, c := range s.consumers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ReadinessChecks are the dependency probes the admin surface registers.
//
// They are readiness checks and never liveness ones. A broker blip must remove
// this pod from the load balancer; restarting it would turn a five-second
// dependency wobble into a cluster-wide restart storm.
func (s *Service) ReadinessChecks() map[string]obs.Check {
	return map[string]obs.Check{
		"broker": func(ctx context.Context) error {
			if !s.device.Connected() {
				return errors.New("MQTT broker link is down")
			}
			return nil
		},
		"eventstore": func(ctx context.Context) error {
			_, err := s.cfg.Store.Version(ctx, "label/readiness-probe")
			return err
		},
		"bus": func(ctx context.Context) error {
			for _, c := range s.consumers {
				if err := c.PublishLag(ctx); err != nil {
					return err
				}
			}
			return nil
		},
		"read-model": func(ctx context.Context) error {
			_, err := s.runner.Position()
			return err
		},
	}
}

// Batch exposes the fan-out pipeline, for the benchmark and for callers that
// drive it directly rather than over HTTP.
func (s *Service) Batch() *app.BatchUpdater { return s.batch }

// PriceHandler exposes the single-label price use case.
func (s *Service) PriceHandler() *app.UpdatePriceHandler { return s.price }

// DeliveryHandler exposes the delivery confirmation use case.
func (s *Service) DeliveryHandler() *app.DeliveryConfirmationHandler { return s.delivery }

// DirectoryProjection exposes the device-events projection.
func (s *Service) DirectoryProjection() *app.DirectoryProjection { return s.devices }

// StateProjection exposes the read-model runner, so an operator can rebuild it.
func (s *Service) StateProjection() *adapters.StateProjectionRunner { return s.runner }

// Scheduler exposes the future-dated price runner.
func (s *Service) Scheduler() *app.ScheduledPriceRunner { return s.scheduler }

// PromotionHandler exposes the promotion fan-out use case, for callers that
// drive a transition synchronously rather than over the stream.
func (s *Service) PromotionHandler() *app.PromotionHandler { return s.promos }

// Directory exposes the placement read model.
func (s *Service) Directory() ports.Directory { return s.directory }

// Metrics exposes the service's series.
func (s *Service) Metrics() *app.Metrics { return s.metrics }

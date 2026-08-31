// Package analytics wires the Retail Analytics Service: the columnar
// time-series store, the ingest that fills it from four event streams, the
// retail intelligence reports the platform sells, the SLO computation, and the
// retention sweeps that keep the whole thing from filling a disk.
//
// # What it is not
//
// It is not a general data warehouse and does not try to be. It holds four
// tables defined by the platform's own event contracts, answers a fixed set of
// questions about them through a structured query API, and ages data through
// three tiers. A tenant with a real warehouse points this service's ingest at
// it instead; a tenant without one gets everything the platform's reports need
// from a service that runs on the same nodes as the rest of it.
//
// # Tenancy
//
// The tenant comes from the X-USSLP-Tenant header, and — unlike the other
// services — every query this one runs has a tenant filter injected server-side
// rather than taken from the request body. A caller cannot ask for another
// tenant's rows because the filter is not theirs to set.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/analytics/app"
	"github.com/usslp/usslp/platform/internal/analytics/columnar"
	"github.com/usslp/usslp/platform/internal/analytics/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// TenantHeader carries the authenticated tenant.
const TenantHeader = "X-USSLP-Tenant"

// Consumer group names. They are constants because a group's identity is its
// committed offsets: renaming one replays the stream from the beginning, which
// for analytics means double-counting every row it has already stored.
const (
	GroupTelemetry = "analytics.telemetry"
	GroupDelivery  = "analytics.delivery"
	GroupPrices    = "analytics.prices"
	GroupPromos    = "analytics.promotions"
)

// DefaultFlushInterval is how often buffered rows are sealed into blocks and
// become visible to queries.
//
// Five seconds. A query cannot see a row until it is flushed, so this is the
// visibility lag; a dashboard refreshing every thirty seconds does not notice
// it, and flushing more often would produce partial blocks that compress worse
// and prune worse. The trade is stated here because it is the one place the
// store's design is visible to a user.
const DefaultFlushInterval = 5 * time.Second

// DefaultRetentionInterval is how often the tiering and deletion sweep runs.
// Hourly: retention windows are measured in days, and a sweep that runs on the
// hour costs one directory listing per table.
const DefaultRetentionInterval = time.Hour

// Config is everything the service needs.
type Config struct {
	// DataDir is where the column store's files live.
	DataDir string
	// Bus is the event stream. Nil disables the consumers.
	Bus eventbus.Bus
	// Retention overrides the default policies.
	Retention []domain.RetentionPolicy
	// SLOs overrides the default objectives.
	SLOs []domain.SLOTarget
	// BlockRows and BlocksPerSegment tune the store.
	BlockRows        int
	BlocksPerSegment int
	// FlushInterval and RetentionInterval tune the background loops.
	FlushInterval     time.Duration
	RetentionInterval time.Duration
	// Registry, Log, Tracer and Standard come from obs.Runtime.
	Registry *obs.Registry
	Log      *obs.Logger
	Tracer   *obs.Tracer
	Standard *obs.StandardMetrics
	// Clock is injected so the retention tests can control "now".
	Clock func() time.Time
	// Streams overrides the catalogue EnsureStreams provisions.
	Streams []canon.Stream
}

// Service is the assembled analytics engine.
type Service struct {
	cfg     Config
	tables  app.Tables
	ingest  *app.Ingest
	log     *obs.Logger
	tracer  *obs.Tracer
	now     func() time.Time
	metrics *serviceMetrics

	consumers []eventbus.Consumer
	closeOnce sync.Once
}

type serviceMetrics struct {
	ingested   *obs.CounterVec
	dropped    *obs.CounterVec
	queries    *obs.HistogramVec
	blocks     *obs.CounterVec
	retention  *obs.CounterVec
	compressed *obs.GaugeVec
}

func newServiceMetrics(r *obs.Registry) *serviceMetrics {
	if r == nil {
		r = obs.NewRegistry()
	}
	return &serviceMetrics{
		ingested: r.Counter("usslp_analytics_rows_ingested_total", "Rows written, by table", "table"),
		dropped:  r.Counter("usslp_analytics_rows_dropped_total", "Records skipped, by reason", "reason"),
		// The buckets span an indexed lookup at the fast end and a full-scan
		// report at the slow end, which is the range this service's queries
		// actually occupy.
		queries: r.Histogram("usslp_analytics_query_seconds", "Query latency",
			[]float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 30}, "operation"),
		blocks: r.Counter("usslp_analytics_blocks_total", "Blocks scanned and skipped", "outcome"),
		retention: r.Counter("usslp_analytics_retention_total",
			"Segments moved or dropped by the retention sweep", "action"),
		compressed: r.Gauge("usslp_analytics_compression_ratio",
			"Measured compression ratio, by table", "table"),
	}
}

// New assembles the service, opening one column store per table.
func New(cfg Config) (*Service, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("analytics: a data directory is required")
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultFlushInterval
	}
	if cfg.RetentionInterval <= 0 {
		cfg.RetentionInterval = DefaultRetentionInterval
	}
	if len(cfg.Retention) == 0 {
		cfg.Retention = domain.DefaultRetention()
	}
	for _, p := range cfg.Retention {
		if err := p.Validate(); err != nil {
			return nil, err
		}
	}
	if len(cfg.SLOs) == 0 {
		cfg.SLOs = domain.DefaultSLOs()
	}

	tables := app.Tables{}
	for _, schema := range domain.AllSchemas() {
		store, err := columnar.Open(columnar.Options{
			Dir:    filepath.Join(cfg.DataDir, "columnar"),
			Schema: schema, BlockRows: cfg.BlockRows, BlocksPerSegment: cfg.BlocksPerSegment,
		})
		if err != nil {
			return nil, fmt.Errorf("analytics: opening table %s: %w", schema.Table, err)
		}
		tables[schema.Table] = store
	}

	return &Service{
		cfg: cfg, tables: tables, ingest: app.NewIngest(tables),
		log: cfg.Log, tracer: cfg.Tracer, now: cfg.Clock,
		metrics: newServiceMetrics(cfg.Registry),
	}, nil
}

// Tables exposes the column stores, for the reports and for tests.
func (s *Service) Tables() app.Tables { return s.tables }

// Ingest exposes the ingester, so a test or a backfill job can write rows
// without going through the event bus.
func (s *Service) Ingest() *app.Ingest { return s.ingest }

// EnsureStreams provisions the streams this service consumes.
func (s *Service) EnsureStreams(ctx context.Context) error {
	if s.cfg.Bus == nil {
		return nil
	}
	streams := s.cfg.Streams
	if len(streams) == 0 {
		streams = []canon.Stream{
			canon.StreamTelemetry, canon.StreamDelivery,
			canon.StreamPriceUpdates, canon.StreamPromotions, canon.StreamDLQ,
		}
	}
	return s.cfg.Bus.EnsureStreams(ctx, streams...)
}

// ReadinessChecks are registered on the admin server's readiness endpoint.
func (s *Service) ReadinessChecks() map[string]obs.Check {
	return map[string]obs.Check{
		"columnar_store": func(ctx context.Context) error {
			for name, store := range s.tables {
				if _, err := store.Stats(); err != nil {
					return fmt.Errorf("table %s unreadable: %w", name, err)
				}
			}
			return nil
		},
	}
}

// Start launches the consumers and the background loops.
func (s *Service) Start(ctx context.Context, spawn func(name string, fn func(context.Context) error)) error {
	spawn("flusher", s.runFlusher)
	spawn("retention", s.runRetention)

	if s.cfg.Bus == nil {
		return nil
	}
	subscriptions := []struct {
		group  string
		topic  string
		name   string
		concur int
	}{
		// Analytics is order-insensitive: a row's position in the table does not
		// depend on when it arrived relative to its neighbours, and every query
		// aggregates. That is what lets these consumers run several handlers per
		// partition, which is how one instance keeps up with 167,000 telemetry
		// events a second.
		{GroupTelemetry, canon.StreamTelemetry.Name, "telemetry-consumer", 8},
		{GroupDelivery, canon.StreamDelivery.Name, "delivery-consumer", 4},
		{GroupPrices, canon.StreamPriceUpdates.Name, "price-consumer", 4},
		{GroupPromos, canon.StreamPromotions.Name, "promotion-consumer", 1},
	}
	for _, sub := range subscriptions {
		consumer, err := s.cfg.Bus.Subscribe(eventbus.SubscribeOptions{
			Group: sub.group, Topics: []string{sub.topic}, Concurrency: sub.concur,
		}.WithDefaults())
		if err != nil {
			return err
		}
		s.consumers = append(s.consumers, consumer)
		c := consumer
		spawn(sub.name, func(ctx context.Context) error { return c.Run(ctx, s.handle) })
	}
	return nil
}

// handle routes one stream record into the store.
func (s *Service) handle(ctx context.Context, m eventbus.Message) error {
	var env canon.Envelope
	if err := decodeEnvelope(m.Value, &env); err != nil {
		// A malformed envelope will never become well-formed. Committing the
		// offset and counting it beats wedging a partition that is carrying
		// 167,000 events a second.
		s.metrics.dropped.With("undecodable").Inc()
		s.log.Warn("undecodable envelope", "topic", m.Topic, "offset", m.Offset, "error", err)
		return nil
	}
	if err := s.ingest.Envelope(env); err != nil {
		if errors.Is(err, app.ErrUnroutable) {
			// An event type this service does not model. Normal during a
			// rolling upgrade, and never a reason to retry.
			s.metrics.dropped.With("unroutable").Inc()
			return nil
		}
		return err
	}
	s.metrics.ingested.With(m.Topic).Inc()
	return nil
}

// runFlusher seals buffered rows on a ticker so queries see recent data.
func (s *Service) runFlusher(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Flush on the way out, so a clean shutdown does not lose the
			// partial block. A crash does lose it, which is the accepted trade
			// for not fsyncing per row on an analytics store.
			if err := s.ingest.Flush(); err != nil {
				s.log.Error("final flush failed", "error", err)
			}
			return ctx.Err()
		case <-ticker.C:
			if err := s.ingest.Flush(); err != nil {
				s.log.Error("flush failed", "error", err)
				continue
			}
			for name, store := range s.tables {
				if st, err := store.Stats(); err == nil && st.CompressionRatio > 0 {
					s.metrics.compressed.With(name).Set(st.CompressionRatio)
				}
			}
		}
	}
}

// runRetention moves and deletes segments on a ticker.
func (s *Service) runRetention(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.RetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.SweepRetention(); err != nil {
				s.log.Error("retention sweep failed", "error", err)
			}
		}
	}
}

// RetentionOutcome is what one sweep did.
type RetentionOutcome struct {
	Table      string `json:"table"`
	MovedWarm  int    `json:"moved_to_warm"`
	MovedCold  int    `json:"moved_to_cold"`
	Dropped    int    `json:"dropped"`
	FreedBytes int64  `json:"freed_bytes"`
}

// SweepRetention applies every table's policy once.
//
// The order is deliberate: promote hot to warm, then warm to cold, then delete
// from cold. Running deletion first would delete from cold what is about to be
// replaced by data arriving from warm — harmless but wasteful — and running the
// promotions in the other order would move a segment through two tiers in one
// sweep, skipping the warm window entirely.
func (s *Service) SweepRetention() error {
	now := s.now()
	for _, policy := range s.cfg.Retention {
		store, ok := s.tables[policy.Table]
		if !ok {
			continue
		}
		out := RetentionOutcome{Table: policy.Table}
		if policy.Hot > 0 {
			n, err := store.MoveTier(columnar.TierHot, columnar.TierWarm, now.Add(-policy.Hot))
			if err != nil {
				return err
			}
			out.MovedWarm = n
		}
		if policy.Warm > 0 {
			n, err := store.MoveTier(columnar.TierWarm, columnar.TierCold, now.Add(-policy.Warm))
			if err != nil {
				return err
			}
			out.MovedCold = n
		}
		if policy.Cold > 0 {
			n, freed, err := store.DropBefore(columnar.TierCold, now.Add(-policy.Cold))
			if err != nil {
				return err
			}
			out.Dropped, out.FreedBytes = n, freed
		}
		if out.MovedWarm > 0 {
			s.metrics.retention.With("moved_warm").Add(uint64(out.MovedWarm))
		}
		if out.MovedCold > 0 {
			s.metrics.retention.With("moved_cold").Add(uint64(out.MovedCold))
		}
		if out.Dropped > 0 {
			s.metrics.retention.With("dropped").Add(uint64(out.Dropped))
			s.log.Info("retention sweep", "table", out.Table,
				"moved_warm", out.MovedWarm, "moved_cold", out.MovedCold,
				"dropped", out.Dropped, "freed_bytes", out.FreedBytes)
		}
	}
	return nil
}

// Shutdown stops the consumers and flushes.
func (s *Service) Shutdown(ctx context.Context) error {
	var err error
	s.closeOnce.Do(func() {
		for _, c := range s.consumers {
			if cerr := c.Close(); cerr != nil && err == nil {
				err = cerr
			}
		}
		if ferr := s.ingest.Flush(); ferr != nil && err == nil {
			err = ferr
		}
	})
	if err != nil && !errors.Is(err, eventbus.ErrClosed) {
		return err
	}
	return nil
}

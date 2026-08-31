package app

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Batch pipeline defaults.
const (
	// DefaultBatchQueue is the depth of the channel between the resolver and
	// the workers.
	//
	// It is deliberately small relative to a 40,000-label promotion. The queue
	// is a smoothing buffer, not a holding pen: a deep queue would let the
	// resolver race ahead, materialise every task in memory, and turn a
	// cancelled request into tens of thousands of orphaned allocations. At this
	// depth the resolver blocks as soon as the workers fall behind, which is
	// backpressure applied where it costs nothing.
	DefaultBatchQueue = 1024
	// MaxBatchWorkers caps the pool. Beyond it the workers contend on the event
	// store's append mutex more than they accomplish.
	MaxBatchWorkers = 256
	// DefaultStoreChunk is how many labels of one store's fan-out are charged
	// to the rate limiter at a time. Charging per label would be a lock
	// acquisition per label; charging the whole store at once would make a
	// 40,000-label store wait for a full bucket before the first label moves.
	DefaultStoreChunk = 256
)

// BatchItem is one (store, SKU) price change within a batch.
type BatchItem struct {
	// StoreID is the store to reprice in.
	StoreID canon.StoreID `json:"store_id"`
	// SKU is the product. Every label showing it in the store is repriced.
	SKU canon.SKU `json:"sku"`
	// LabelID, when set, narrows the change to one label instead of every
	// facing of the product. It is how a colleague fixes one mis-rendered
	// label without touching its neighbours.
	LabelID canon.LabelID `json:"label_id,omitempty"`
	// Price is the new price.
	Price canon.Money `json:"price"`
	// WasPrice, UnitPrice and UnitMeasure are the optional comparison prices.
	WasPrice    *canon.Money `json:"was_price,omitempty"`
	UnitPrice   *canon.Money `json:"unit_price,omitempty"`
	UnitMeasure string       `json:"unit_measure,omitempty"`
	// EffectiveAt is when the price takes effect; future dates schedule.
	EffectiveAt time.Time `json:"effective_at"`
	// ExpiresAt is when a promotional price lapses.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// PromotionID, Reason and Attributes drive template selection.
	PromotionID canon.PromotionID `json:"promotion_id,omitempty"`
	// PromotionPriority is the activating rule's priority, recorded on the
	// resulting event for the audit trail.
	PromotionPriority int               `json:"promotion_priority,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	Attributes        map[string]string `json:"attributes,omitempty"`
	// InitiatedBy names the operator or system behind the change.
	InitiatedBy string `json:"initiated_by,omitempty"`
	// IdempotencyKey makes the item's appends no-ops on a retried batch. A
	// caller that omits it gets at-least-once semantics with the aggregate's
	// own no-op and out-of-order rules as the only protection, which is
	// correct but re-publishes.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// BatchRequest is a bulk price change.
type BatchRequest struct {
	// TenantID owns every item. A batch never spans tenants: the rate limiter,
	// the guard-rail policy and the topic namespace are all tenant scoped.
	TenantID canon.TenantID `json:"tenant_id"`
	// Region scopes the MQTT topics for stores whose placements predate a
	// region assignment.
	Region canon.Region `json:"region,omitempty"`
	// Items are the changes.
	Items []BatchItem `json:"items"`
	// CorrelationID ties the batch's events to the request that produced it.
	CorrelationID canon.CorrelationID `json:"correlation_id,omitempty"`
	// OccurredAt is the source-clock instant every item in the batch is stamped
	// with. Zero means "now".
	//
	// It is what decides a race with a direct price change on the same label.
	// The aggregate rejects an update whose source clock predates the price on
	// the glass, so a promotion activated at 09:00 whose fan-out lands at 09:07
	// will not overwrite a till correction made at 09:05 — the more recent
	// statement of what the retailer wants charged wins, whichever arrives
	// first. Stamping the batch with wall-clock time at fan-out instead would
	// make the winner depend on scheduling.
	OccurredAt time.Time `json:"occurred_at,omitempty"`
	// Cause is the envelope that triggered the batch, when there is one. Its
	// trace context and causation are inherited by every event the batch
	// produces, so one promotion activation stays one trace across every label
	// it touches.
	Cause canon.Envelope `json:"-"`
	// InitiatedBy names the caller, for the audit trail.
	InitiatedBy string `json:"initiated_by,omitempty"`
}

// BatchReport is the per-label outcome of a batch.
type BatchReport struct {
	// Requested is the number of items submitted.
	Requested int `json:"requested"`
	// Resolved is the number of labels those items expanded to.
	Resolved int `json:"resolved"`
	// Applied, Scheduled, Rejected, Stale and Failed partition Results.
	Applied   int `json:"applied"`
	Scheduled int `json:"scheduled"`
	Rejected  int `json:"rejected"`
	Stale     int `json:"stale"`
	Failed    int `json:"failed"`
	// Results is one entry per label touched, ordered by label id so that two
	// runs of the same batch produce comparable reports.
	Results []PriceResult `json:"results"`
	// Duration is the wall time of the batch.
	Duration time.Duration `json:"-"`
	// DurationMS is Duration in milliseconds, for the JSON response.
	DurationMS int64 `json:"duration_ms"`
	// Partial is true when at least one label failed. The caller decides
	// whether to retry the whole batch — which is safe, because every applied
	// item is idempotent — or to act on the failures alone.
	Partial bool `json:"partial"`
}

// BatchConfig configures the pipeline.
type BatchConfig struct {
	// Workers is the pool size. Zero picks a value from GOMAXPROCS.
	Workers int
	// Queue is the resolver-to-worker channel depth. Zero means
	// DefaultBatchQueue.
	Queue int
	// StoreChunk is the rate-limiter charging granularity. Zero means
	// DefaultStoreChunk.
	StoreChunk int
}

// BatchUpdater is the scale-critical path: a store-wide promotion touching
// 40,000 labels at once.
//
// The shape is a two-stage pipeline. One resolver goroutine walks the request
// store by store, turning each (store, SKU) item into the set of labels that
// show it, and feeds a bounded channel. A fixed pool of workers drains the
// channel, each one running the same single-label command the stream path runs.
//
// Three properties matter more than throughput:
//
//   - Bounded memory. The resolver blocks on a small queue, so a 40,000-label
//     promotion never materialises 40,000 in-flight tasks.
//   - Per-tenant fairness. Each store's fan-out is charged to the tenant's
//     bucket in chunks, so one tenant's bulk repricing cannot occupy the pool
//     to the exclusion of another tenant's single urgent change.
//   - Honest partial failure. A label that fails is reported as that label
//     failing. The batch does not abort, because forty thousand correct price
//     changes must not be thrown away by one unreachable controller.
type BatchUpdater struct {
	handler *UpdatePriceHandler
	deps    Deps
	limiter ports.RateLimiter

	workers    int
	queue      int
	storeChunk int

	// inflight tracks running batches so that shutdown can drain them. A
	// process that exits mid-fan-out leaves a store half repriced, which is the
	// one outcome worse than not repricing it at all.
	inflight sync.WaitGroup
}

// NewBatchUpdater builds the pipeline. A nil limiter disables per-tenant
// shaping, which is correct only for a single-tenant deployment.
func NewBatchUpdater(handler *UpdatePriceHandler, deps Deps, limiter ports.RateLimiter, cfg BatchConfig) (*BatchUpdater, error) {
	deps = deps.withDefaults()
	if handler == nil {
		return nil, fmt.Errorf("%w: UpdatePriceHandler", ErrMissingDependency)
	}
	if deps.Directory == nil {
		return nil, fmt.Errorf("%w: Directory", ErrMissingDependency)
	}
	if cfg.Workers <= 0 {
		// Each task is dominated by a durable append and a broker round trip,
		// not by CPU, so the pool is oversubscribed relative to the cores.
		cfg.Workers = runtime.GOMAXPROCS(0) * 8
	}
	if cfg.Workers > MaxBatchWorkers {
		cfg.Workers = MaxBatchWorkers
	}
	if cfg.Queue <= 0 {
		cfg.Queue = DefaultBatchQueue
	}
	if cfg.StoreChunk <= 0 {
		cfg.StoreChunk = DefaultStoreChunk
	}
	return &BatchUpdater{
		handler: handler, deps: deps, limiter: limiter,
		workers: cfg.Workers, queue: cfg.Queue, storeChunk: cfg.StoreChunk,
	}, nil
}

// Workers reports the configured pool size.
func (b *BatchUpdater) Workers() int { return b.workers }

// Drain waits for in-flight batches to finish, or for ctx to expire. It is the
// graceful-shutdown hook.
func (b *BatchUpdater) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		b.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("label: batch drain timed out: %w", ctx.Err())
	}
}

type batchTask struct {
	cmd PriceCommand
}

// BatchUpdatePrices runs a bulk price change and reports every label's outcome.
//
// It returns an error only when the batch could not be run at all — a cancelled
// context, an unusable request. A label that failed is reported in the results
// with Outcome error and sets Partial, because the caller's decision after "one
// controller was unreachable" is completely different from its decision after
// "the whole batch was rejected".
func (b *BatchUpdater) BatchUpdatePrices(ctx context.Context, req BatchRequest) (BatchReport, error) {
	if req.TenantID == "" {
		return BatchReport{}, fmt.Errorf("%w: BatchRequest.TenantID is required", domain.ErrInvalidCommand)
	}
	if len(req.Items) == 0 {
		return BatchReport{}, nil
	}
	b.inflight.Add(1)
	defer b.inflight.Done()

	ctx, span := b.deps.Tracer.StartAlwaysSampled(ctx, "label.price.batch")
	defer span.End()
	span.SetAttr("tenant", string(req.TenantID)).SetAttrInt("items", int64(len(req.Items)))

	// Wall time, not the injected clock: this measurement is a metric about the
	// pipeline's own behaviour, and a test clock that does not advance would
	// report every batch as instantaneous.
	started := time.Now()
	// The whole pipeline shares one cancellable context so that a caller
	// hanging up, or a fatal resolver error, stops the workers immediately
	// rather than letting them finish forty thousand now-pointless updates.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tasks := make(chan batchTask, b.queue)
	var (
		mu       sync.Mutex
		results  []PriceResult
		resolved int
	)
	appendResult := func(r PriceResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(b.workers)
	for i := 0; i < b.workers; i++ {
		go func() {
			defer wg.Done()
			for task := range tasks {
				if ctx.Err() != nil {
					// Drain without working: closing the channel is the
					// resolver's job, and returning here would deadlock it.
					appendResult(PriceResult{
						LabelID: task.cmd.Placement.LabelID,
						Outcome: OutcomeError, Detail: ctx.Err().Error(),
					})
					continue
				}
				res, err := b.handler.Apply(ctx, task.cmd)
				if err != nil {
					appendResult(PriceResult{
						LabelID: task.cmd.Placement.LabelID,
						Outcome: OutcomeError, Detail: err.Error(),
					})
					continue
				}
				appendResult(res)
			}
		}()
	}

	resolveErr := b.resolve(ctx, req, tasks, &resolved)
	close(tasks)
	wg.Wait()

	if resolveErr != nil && !errors.Is(resolveErr, context.Canceled) {
		span.Fail(resolveErr)
		return BatchReport{}, resolveErr
	}
	if err := ctx.Err(); err != nil && resolveErr != nil {
		return BatchReport{}, err
	}

	sort.Slice(results, func(i, j int) bool { return results[i].LabelID < results[j].LabelID })
	report := BatchReport{
		Requested: len(req.Items),
		Resolved:  resolved,
		Results:   results,
		Duration:  time.Since(started),
	}
	for _, r := range results {
		switch r.Outcome {
		case OutcomeApplied, OutcomeRepublished:
			report.Applied++
		case OutcomeScheduled:
			report.Scheduled++
		case OutcomeRejected:
			report.Rejected++
		case OutcomeStale:
			report.Stale++
		default:
			report.Failed++
		}
	}
	report.Partial = report.Failed > 0
	report.DurationMS = report.Duration.Milliseconds()
	b.deps.Metrics.FanOutBatchSize.With(string(req.TenantID)).Observe(float64(resolved))
	b.deps.Metrics.FanOutDuration.With(string(req.TenantID)).Observe(report.Duration.Seconds())
	span.SetAttrInt("resolved", int64(resolved)).SetAttrInt("applied", int64(report.Applied))
	return report, nil
}

// resolve walks the request store by store, expands each item into per-label
// commands, and feeds the worker queue.
//
// Grouping by store is not cosmetic. It keeps one store's labels adjacent in
// the queue, so the workers touching them hit the same directory pages and the
// same broker session, and it makes the rate-limiter charge granular per store
// rather than per item — the difference between one lock acquisition per 256
// labels and one per label.
func (b *BatchUpdater) resolve(ctx context.Context, req BatchRequest, tasks chan<- batchTask, resolved *int) error {
	now := b.deps.Clock.Now()
	byStore := groupByStore(req.Items)
	stores := make([]canon.StoreID, 0, len(byStore))
	for s := range byStore {
		stores = append(stores, s)
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i] < stores[j] })

	for _, store := range stores {
		pending := 0
		for _, item := range byStore[store] {
			if err := ctx.Err(); err != nil {
				return err
			}
			placements, err := b.placementsFor(ctx, req, store, item)
			if err != nil {
				return err
			}
			for _, p := range placements {
				if b.limiter != nil {
					pending++
					if pending >= b.storeChunk {
						if err := b.limiter.Wait(ctx, req.TenantID, pending); err != nil {
							return err
						}
						pending = 0
					}
				}
				task := batchTask{cmd: PriceCommand{
					Placement:      p,
					Change:         changeFromItem(item, req, now),
					Cause:          req.Cause,
					IdempotencyKey: batchIdempotencyKey(item, p.LabelID),
				}}
				select {
				case tasks <- task:
					*resolved++
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if b.limiter != nil && pending > 0 {
			if err := b.limiter.Wait(ctx, req.TenantID, pending); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *BatchUpdater) placementsFor(ctx context.Context, req BatchRequest, store canon.StoreID, item BatchItem) ([]ports.Placement, error) {
	if item.LabelID != "" {
		p, err := b.deps.Directory.Lookup(ctx, item.LabelID)
		if errors.Is(err, ports.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("label: resolving %s: %w", item.LabelID, err)
		}
		return []ports.Placement{p}, nil
	}
	placements, err := b.deps.Directory.LabelsForSKU(ctx, req.TenantID, store, item.SKU)
	if err != nil {
		return nil, fmt.Errorf("label: resolving %s/%s/%s: %w", req.TenantID, store, item.SKU, err)
	}
	return placements, nil
}

func groupByStore(items []BatchItem) map[canon.StoreID][]BatchItem {
	out := make(map[canon.StoreID][]BatchItem)
	for _, it := range items {
		out[it.StoreID] = append(out[it.StoreID], it)
	}
	return out
}

func changeFromItem(item BatchItem, req BatchRequest, now time.Time) domain.PriceChange {
	initiated := item.InitiatedBy
	if initiated == "" {
		initiated = req.InitiatedBy
	}
	effective := item.EffectiveAt
	if effective.IsZero() {
		effective = now
	}
	occurred := req.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	return domain.PriceChange{
		SKU: item.SKU, Price: item.Price, WasPrice: item.WasPrice,
		UnitPrice: item.UnitPrice, UnitMeasure: item.UnitMeasure,
		EffectiveAt: effective, ExpiresAt: item.ExpiresAt,
		PromotionID: item.PromotionID, PromotionPriority: item.PromotionPriority,
		Reason: item.Reason, Attributes: item.Attributes, InitiatedBy: initiated,
		OccurredAt: occurred, Now: now,
	}
}

func batchIdempotencyKey(item BatchItem, label canon.LabelID) string {
	if item.IdempotencyKey == "" {
		return ""
	}
	return item.IdempotencyKey + "#" + string(label)
}

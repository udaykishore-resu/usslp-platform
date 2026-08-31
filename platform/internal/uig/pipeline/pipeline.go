// Package pipeline is the single path every inbound POS delivery takes,
// whatever protocol it arrived on.
//
//	Verify → dedupe → parse → schema-map → field-normalise →
//	currency/locale-normalise → validate → enrich → publish → respond
//
// Having exactly one implementation of those stages is what makes the adapter
// pattern worth anything. An adapter is a parser and a signature check; it does
// not get to decide how deduplication works, whether a store code is resolved,
// what a 4xx means, or when a caller is acknowledged. Nine adapters with nine
// slightly different opinions about idempotency would be nine different
// production incidents.
//
// Two properties of the ordering are load-bearing:
//
//   - Verification precedes everything, on the raw bytes, because every stage
//     after it spends resources on behalf of the caller.
//   - Deduplication precedes parsing, because the expensive stage is parsing
//     and the common case in retail integration is redelivery. An SAP ALE queue
//     resending a 6,000-segment IDoc costs a hash lookup here, not a parse.
//
// The acknowledgement rule is equally deliberate. The caller is answered as
// soon as the change is durable on the event stream — not once the delivery has
// been indexed, logged and filed. POS systems retry aggressively on slow
// responses, and a 202 in 50ms is worth more to a retailer than a 200 in two
// seconds; the two seconds would be spent creating duplicate work, not
// preventing it.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/deliveries"
	"github.com/usslp/usslp/platform/internal/uig/reliability"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// Status aliases keep the pipeline's vocabulary identical to the store's, so
// there is one set of outcome names across metrics, stored records and the
// operator API.
const (
	statusAccepted    = deliveries.StatusAccepted
	statusPartial     = deliveries.StatusPartial
	statusQuarantined = deliveries.StatusQuarantined
	statusRejected    = deliveries.StatusRejected
	statusIgnored     = deliveries.StatusIgnored
)

// EventTypeDeliveryReceived is the event type of the raw copy published to the
// pos-integration stream.
//
// It is defined here rather than in canon because canon is the shared kernel
// every tier compiles against and this fact is only ever produced and consumed
// inside the cloud tier: the UIG writes it, audit and analytics read it, and no
// label, controller or gateway will ever see one.
const EventTypeDeliveryReceived = "pos.delivery.received"

// aggregateTypePrice and aggregateTypeDelivery name the two aggregates the UIG
// writes to.
const (
	aggregateTypePrice    = "price"
	aggregateTypeDelivery = "pos_delivery"
)

// RawDelivery is the payload of the pos-integration copy: everything an auditor
// needs to reconstruct what a POS actually sent, without having to trust the
// canonical events derived from it.
//
// The raw body is included. That is the point of the stream — "the retailer
// says they sent 1.99" is a question that can only be settled by the bytes —
// and it is why pos-integration is retained for three days rather than the
// seven of price-updates.
type RawDelivery struct {
	DeliveryID  string         `json:"delivery_id"`
	TenantID    canon.TenantID `json:"tenant_id"`
	BindingID   string         `json:"binding_id"`
	Adapter     string         `json:"adapter"`
	POSInstance string         `json:"pos_instance,omitempty"`
	Method      string         `json:"method,omitempty"`
	Path        string         `json:"path,omitempty"`
	ContentType string         `json:"content_type,omitempty"`
	ReceivedAt  time.Time      `json:"received_at"`
	// BodySHA256 lets a consumer detect a truncated or altered copy without
	// re-hashing megabytes it did not need.
	BodySHA256 string `json:"body_sha256"`
	BodySize   int    `json:"body_size"`
	Body       []byte `json:"body,omitempty"`
	// Emitted is how many canonical changes this delivery produced, so the
	// audit stream can be reconciled against price-updates without a join.
	Emitted int `json:"emitted"`
	// Stores lists the canonical stores the delivery touched, which is what
	// analytics partitions on.
	Stores []canon.StoreID `json:"stores,omitempty"`
	Replay bool            `json:"replay,omitempty"`
}

// Result is the outcome of one delivery. It is what the caller is answered
// with, what is replayed to a duplicate, and what is stored for triage.
type Result struct {
	DeliveryID string         `json:"delivery_id"`
	TenantID   canon.TenantID `json:"tenant_id"`
	BindingID  string         `json:"binding_id"`
	Adapter    string         `json:"adapter"`
	// Status is the canonical outcome name.
	Status deliveries.Status `json:"status"`
	// HTTPStatus is what the POS is told. It is computed here, from the failure
	// classification, rather than in the HTTP layer, because whether a source
	// retries is a pipeline decision and not a transport one.
	HTTPStatus int `json:"http_status"`
	// Emitted is the number of canonical changes published.
	Emitted int `json:"emitted"`
	// Duplicate marks a delivery the guard recognised. The original response is
	// replayed with it, so a retrying producer gets the same answer it would
	// have got the first time and stops retrying.
	Duplicate bool `json:"duplicate,omitempty"`
	// Reason is the low-cardinality failure token; Detail explains it.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// RowFailures lists per-record problems in a partially usable delivery.
	RowFailures []deliveries.RowFailure `json:"row_failures,omitempty"`
	// RetryAfter is populated on a 429 so the caller does not have to guess.
	RetryAfter time.Duration `json:"-"`
	// DurationMS is the pipeline's own latency.
	DurationMS int64 `json:"duration_ms"`
	// CorrelationID ties this delivery to every event it produced and to the
	// label ACKs that eventually come back.
	CorrelationID canon.CorrelationID `json:"correlation_id,omitempty"`
	// DedupeKey is returned so support can look a delivery up in the guard
	// without reproducing the derivation by hand.
	DedupeKey string `json:"dedupe_key,omitempty"`
}

// Config assembles a pipeline.
type Config struct {
	// Registry resolves a binding's adapter name to an implementation.
	Registry *adapter.Registry
	// Bindings holds the installed integrations.
	Bindings *adapter.BindingStore
	// Guard is the 24-hour idempotency guard from contract §6.
	Guard *idem.Guard
	// Bus is the durable event stream. Publish returning nil is the moment the
	// platform has taken responsibility for a price change, and therefore the
	// moment the caller may be acknowledged.
	Bus eventbus.Publisher
	// Deliveries is the quarantine and replay store.
	Deliveries *deliveries.Store
	// Limiter enforces per-tenant, per-adapter ingress budgets.
	Limiter *reliability.Limiter
	// Breakers guard adapters that make outbound calls.
	Breakers *reliability.BreakerSet
	// Resolver maps source store codes to canonical store ids.
	Resolver adapter.StoreResolver
	// Metrics, Log and Tracer are the observability stack.
	Metrics *Metrics
	Health  *HealthTracker
	Log     *obs.Logger
	Tracer  *obs.Tracer
	// Region stamps emitted envelopes.
	Region canon.Region
	// Now injects a clock for deterministic tests.
	Now func() time.Time
	// RecordWorkers is the size of the asynchronous bookkeeping pool. Zero uses
	// a small default; the pool exists so that filing a successful delivery
	// never sits between "durable" and "acknowledged".
	RecordWorkers int
}

// Pipeline processes deliveries.
type Pipeline struct {
	cfg     Config
	now     func() time.Time
	log     *obs.Logger
	records chan *deliveries.Record
	wg      sync.WaitGroup

	// closeMu guards the records channel's lifetime. A send racing a close
	// would panic, and shutdown racing an in-flight delivery is the normal
	// case rather than an exotic one: SIGTERM arrives mid-burst.
	closeMu sync.RWMutex
	closed  bool
	// pending counts records handed to the pool and not yet written. Queue
	// length alone is not enough: a record a worker has already dequeued but
	// not yet stored is still outstanding, and a Flush that only watched the
	// queue would return while it was in flight.
	pending atomic.Int64
}

// New builds a pipeline, starting its bookkeeping workers.
func New(cfg Config) (*Pipeline, error) {
	switch {
	case cfg.Registry == nil:
		return nil, errors.New("uig/pipeline: nil adapter registry")
	case cfg.Bindings == nil:
		return nil, errors.New("uig/pipeline: nil binding store")
	case cfg.Guard == nil:
		return nil, errors.New("uig/pipeline: nil idempotency guard")
	case cfg.Bus == nil:
		return nil, errors.New("uig/pipeline: nil event bus")
	case cfg.Deliveries == nil:
		return nil, errors.New("uig/pipeline: nil delivery store")
	case cfg.Metrics == nil:
		return nil, errors.New("uig/pipeline: nil metrics")
	}
	if cfg.Limiter == nil {
		cfg.Limiter = reliability.NewLimiter()
	}
	if cfg.Breakers == nil {
		cfg.Breakers = reliability.NewBreakerSet(reliability.BreakerConfig{})
	}
	if cfg.Resolver == nil {
		cfg.Resolver = adapter.BindingResolver{}
	}
	if cfg.Health == nil {
		cfg.Health = NewHealthTracker()
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RecordWorkers <= 0 {
		cfg.RecordWorkers = 2
	}
	p := &Pipeline{
		cfg:     cfg,
		now:     cfg.Now,
		log:     cfg.Log,
		records: make(chan *deliveries.Record, 256),
	}
	for i := 0; i < cfg.RecordWorkers; i++ {
		p.wg.Add(1)
		go p.recordLoop()
	}
	return p, nil
}

// Health exposes the per-binding health tracker for the operator API.
func (p *Pipeline) Health() *HealthTracker { return p.cfg.Health }

// Breakers exposes the outbound circuit breakers.
func (p *Pipeline) Breakers() *reliability.BreakerSet { return p.cfg.Breakers }

// Bindings exposes the installed bindings.
func (p *Pipeline) Bindings() *adapter.BindingStore { return p.cfg.Bindings }

// Deliveries exposes the quarantine and replay store.
func (p *Pipeline) Deliveries() *deliveries.Store { return p.cfg.Deliveries }

// Metrics exposes the registered series.
func (p *Pipeline) Metrics() *Metrics { return p.cfg.Metrics }

func (p *Pipeline) recordLoop() {
	defer p.wg.Done()
	for rec := range p.records {
		if err := p.cfg.Deliveries.Put(rec); err != nil {
			p.log.Error("uig: filing delivery record failed",
				"delivery_id", rec.ID, "tenant_id", string(rec.TenantID), "error", err)
		}
		p.pending.Add(-1)
	}
}

// Close drains the bookkeeping pool. It is called from the service's shutdown
// hook so that a pod terminating mid-burst still files the deliveries it
// already acknowledged, rather than leaving support with a gap.
func (p *Pipeline) Close() error {
	p.closeMu.Lock()
	if !p.closed {
		p.closed = true
		close(p.records)
	}
	p.closeMu.Unlock()
	p.wg.Wait()
	return nil
}

// Flush waits for the bookkeeping pool to catch up. Tests call it before
// asserting on stored records; production does not need it, because nothing
// downstream of an acknowledged delivery reads the record synchronously.
func (p *Pipeline) Flush(ctx context.Context) error {
	p.closeMu.RLock()
	closed := p.closed
	p.closeMu.RUnlock()
	if closed {
		// Close already waited for the workers, so there is nothing left in
		// flight and polling the queue would spin on a closed channel.
		return nil
	}
	// A bounded poll on the outstanding count is adequate: the pool does local
	// writes measured in microseconds, and the count reaching zero means every
	// record handed to it has actually been stored.
	deadline := time.Now().Add(5 * time.Second)
	for p.pending.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return errors.New("uig/pipeline: timed out draining delivery records")
		}
		time.Sleep(100 * time.Microsecond)
	}
	return nil
}

// Ingest runs one delivery through the pipeline.
//
// It never returns an error: every failure mode is a classified Result, because
// the caller's job is to render an answer to a POS and every path must produce
// one. An unclassified error escaping to the HTTP layer would be answered 500,
// which is the one answer that must never be given to a message that will never
// parse.
func (p *Pipeline) Ingest(ctx context.Context, d *adapter.Delivery) *Result {
	start := p.now()
	if d.ID == "" {
		d.ID = canon.NewULID()
	}
	if d.ReceivedAt.IsZero() {
		d.ReceivedAt = start
	}
	res := &Result{
		DeliveryID: d.ID,
		TenantID:   d.TenantID,
		BindingID:  d.BindingID,
		Adapter:    "unknown",
	}
	m := p.cfg.Metrics
	m.InFlight.With().Inc()
	defer m.InFlight.With().Dec()

	// ---- binding resolution -------------------------------------------------
	b, err := p.cfg.Bindings.Get(d.TenantID, d.BindingID)
	if err != nil {
		return p.finish(ctx, d, res, start, adapter.NotFound("unknown_binding",
			"no integration is configured at this address"))
	}
	d.Binding = b
	res.Adapter = b.Adapter
	if b.Disabled {
		// Answered 404 rather than 403: a disabled binding is indistinguishable
		// from an absent one to the outside world, which is what stops a
		// decommissioned endpoint from confirming that it once existed.
		return p.finish(ctx, d, res, start, adapter.NotFound("binding_disabled",
			"no integration is configured at this address"))
	}
	a, err := p.cfg.Registry.Get(b.Adapter)
	if err != nil {
		// The binding store validates the adapter name on install, so reaching
		// here means the process is misconfigured rather than the caller.
		return p.finish(ctx, d, res, start, adapter.Unavailable("adapter_missing",
			"the configured adapter is not available in this process", err))
	}

	ctx, span := p.startSpan(ctx, "uig.ingest")
	if span != nil {
		span.SetAttr("uig.adapter", b.Adapter)
		span.SetAttr("uig.tenant", string(d.TenantID))
		span.SetAttr("uig.binding", b.ID)
		span.SetAttr("uig.delivery_id", d.ID)
		defer span.End()
	}
	res.CorrelationID = correlationID(d)

	// ---- rate limiting ------------------------------------------------------
	// The key is tenant and adapter, not tenant alone: a retailer running
	// Shopify webhooks and a nightly SAP drop has two traffic shapes, and one
	// shared bucket would let the file drop starve the webhooks.
	limitKey := string(d.TenantID) + "|" + b.Adapter + "|" + b.ID
	if ok, wait := p.cfg.Limiter.Allow(limitKey, b.RateLimit.RatePerSecond, b.RateLimit.Burst); !ok {
		m.RateLimited.With(b.Adapter, string(d.TenantID)).Inc()
		res.RetryAfter = wait
		return p.finish(ctx, d, res, start, &adapter.Error{
			Kind:   adapter.FailureRateLimited,
			Reason: "rate_limited",
			Detail: fmt.Sprintf("binding is over its ingress budget; retry in %s", wait.Round(time.Millisecond)),
		})
	}

	// ---- verify -------------------------------------------------------------
	if !d.Replay {
		if err := a.Verify(ctx, d); err != nil {
			cls := adapter.Classify(err)
			m.VerifyFailures.With(b.Adapter, cls.Reason).Inc()
			return p.finish(ctx, d, res, start, err)
		}
	}

	// ---- dedupe -------------------------------------------------------------
	key := p.dedupeKey(a, d)
	res.DedupeKey = key
	if !d.Replay {
		first, previous, err := p.cfg.Guard.Check(ctx, key)
		if err != nil {
			return p.finish(ctx, d, res, start, adapter.Unavailable("guard_unavailable",
				"the idempotency guard is unavailable", err))
		}
		if !first {
			m.DedupeHits.With().Inc()
			return p.finishDuplicate(ctx, d, res, start, previous)
		}
		// From here on, any path that does not durably publish must release the
		// key. Holding it after a failure would suppress every retry for 24
		// hours and lose the price change silently, which is the worst failure
		// mode a pricing system has.
	}

	// ---- parse, schema-map, field-normalise --------------------------------
	changes, ingestErr := a.Ingest(ctx, d)
	partial, isPartial := adapter.IsPartial(ingestErr)
	if ingestErr != nil && !isPartial {
		cls := adapter.Classify(ingestErr)
		m.ParseErrors.With(b.Adapter, cls.Reason).Inc()
		p.release(ctx, d, key)
		return p.finish(ctx, d, res, start, ingestErr)
	}
	var rowFailures []deliveries.RowFailure
	if isPartial {
		for _, f := range partial.Failures {
			m.RowFailures.With(b.Adapter, f.Reason).Inc()
			rowFailures = append(rowFailures, deliveries.RowFailure(f))
		}
	}

	// ---- currency/locale normalise, validate, enrich ------------------------
	normalised := make([]canon.PriceChangeRequested, 0, len(changes))
	for i := range changes {
		pc := changes[i]
		if err := p.normalise(ctx, d, b, &pc); err != nil {
			cls := adapter.Classify(err)
			m.RowFailures.With(b.Adapter, cls.Reason).Inc()
			rowFailures = append(rowFailures, deliveries.RowFailure{
				Index:  i,
				Ref:    string(pc.SKU),
				Reason: cls.Reason,
				Detail: cls.Detail,
			})
			continue
		}
		normalised = append(normalised, pc)
	}

	if len(normalised) == 0 {
		if len(rowFailures) > 0 {
			// Everything the delivery contained was unusable. That is a
			// quarantine, not a partial success.
			p.release(ctx, d, key)
			res.RowFailures = rowFailures
			m.ParseErrors.With(b.Adapter, "all_records_invalid").Inc()
			return p.finish(ctx, d, res, start, adapter.Invalid("all_records_invalid",
				fmt.Sprintf("all %d records in the delivery were unusable", len(rowFailures)), nil))
		}
		// Understood, deliberately empty: a webhook topic this adapter does not
		// act on, or a catalogue update that changed a description. Recording
		// the dedupe key still matters, so a redelivery is not re-parsed.
		res.Status = statusIgnored
		res.HTTPStatus = 202
		p.recordGuard(ctx, d, key, res)
		return p.finish(ctx, d, res, start, nil)
	}

	// ---- publish ------------------------------------------------------------
	msgs, stores, err := p.buildMessages(d, b, key, normalised, res.CorrelationID)
	if err != nil {
		p.release(ctx, d, key)
		return p.finish(ctx, d, res, start, adapter.Invalid("encode_failed",
			"the canonical events could not be encoded", err))
	}
	if err := p.cfg.Bus.Publish(ctx, msgs...); err != nil {
		m.PublishFailures.With(b.Adapter).Inc()
		p.release(ctx, d, key)
		return p.finish(ctx, d, res, start, adapter.Unavailable("publish_failed",
			"USSLP could not durably record the price change", err))
	}
	// The price change is now durable. Everything below this line is
	// bookkeeping, and none of it may delay the acknowledgement.
	res.Emitted = len(normalised)
	res.RowFailures = rowFailures
	res.Status = statusAccepted
	res.HTTPStatus = 202
	if len(rowFailures) > 0 {
		res.Status = statusPartial
	}
	m.ChangesEmitted.With().Add(uint64(len(normalised)))
	if span != nil {
		span.SetAttrInt("uig.changes", int64(len(normalised)))
		span.SetAttrInt("uig.stores", int64(len(stores)))
	}
	p.recordGuard(ctx, d, key, res)
	return p.finish(ctx, d, res, start, nil)
}

// normalise applies the currency, locale, defaulting, validation and store
// enrichment stages to one change.
//
// It runs per change rather than per delivery so that one unusable record in a
// 1,000-row file costs one product rather than a chain — the same isolation the
// file adapter provides at the parsing layer, applied again at the semantic
// one.
func (p *Pipeline) normalise(ctx context.Context, d *adapter.Delivery, b *adapter.Binding, pc *canon.PriceChangeRequested) error {
	if pc.SKU == "" {
		return adapter.Invalid("missing_sku", "record carries no SKU", nil)
	}
	if !canon.ValidID(string(pc.SKU)) {
		return adapter.Invalid("invalid_sku",
			fmt.Sprintf("SKU %q contains characters reserved by the topic and key namespaces", pc.SKU), nil)
	}
	if pc.Price.Currency == "" {
		pc.Price.Currency = b.Currency()
	}
	pc.Price.Currency = strings.ToUpper(strings.TrimSpace(pc.Price.Currency))
	if !pc.Price.Valid() {
		return adapter.Invalid("invalid_currency",
			fmt.Sprintf("currency %q is not an ISO 4217 alphabetic code and the binding has no default", pc.Price.Currency), nil)
	}
	if !b.CurrencyAllowed(pc.Price.Currency) {
		return adapter.Invalid("currency_not_allowed",
			fmt.Sprintf("currency %s is not in this binding's allowed set", pc.Price.Currency), nil)
	}
	for _, side := range []*canon.Money{pc.WasPrice, pc.UnitPrice} {
		if side == nil {
			continue
		}
		if side.Currency == "" {
			side.Currency = pc.Price.Currency
		}
		side.Currency = strings.ToUpper(strings.TrimSpace(side.Currency))
	}
	if pc.EffectiveAt.IsZero() {
		// A source that does not say when a price starts means "now". Using the
		// receipt time rather than the publish time keeps the value stable
		// across a replay.
		pc.EffectiveAt = d.ReceivedAt.UTC()
	}
	pc.EffectiveAt = pc.EffectiveAt.UTC()
	if pc.ExpiresAt != nil {
		exp := pc.ExpiresAt.UTC()
		pc.ExpiresAt = &exp
	}
	if pc.SourceSystem == "" {
		pc.SourceSystem = b.Adapter
	}
	if pc.InitiatedBy == "" {
		pc.InitiatedBy = b.InitiatedBy
	}
	if pc.InitiatedBy == "" {
		pc.InitiatedBy = "pos:" + b.Adapter + "/" + b.ID
	}
	// Enrichment: the adapter left the source system's store code in StoreID,
	// and this is where it becomes a USSLP store. Doing it here rather than in
	// each adapter means a retailer renumbering its estate changes one binding.
	store, err := p.cfg.Resolver.Resolve(ctx, b, string(pc.StoreID))
	if err != nil {
		return err
	}
	pc.StoreID = store
	if err := pc.Validate(); err != nil {
		return adapter.Invalid("canonical_invalid", err.Error(), err)
	}
	return nil
}

// buildMessages turns normalised changes into stream records: one canonical
// event per change on price-updates, plus one raw copy on pos-integration.
func (p *Pipeline) buildMessages(
	d *adapter.Delivery,
	b *adapter.Binding,
	dedupeKey string,
	changes []canon.PriceChangeRequested,
	corr canon.CorrelationID,
) ([]eventbus.Message, []canon.StoreID, error) {
	now := p.now().UTC()
	occurred := d.ReceivedAt.UTC()
	if !d.SourceTime.IsZero() {
		// The POS's own clock, when it sent one. OccurredAt and RecordedAt
		// differing is the normal state of affairs during a backfill or after a
		// WAN outage, and analytics depends on being able to tell them apart.
		occurred = d.SourceTime.UTC()
	}
	traceID, spanID := traceIDs(d)

	msgs := make([]eventbus.Message, 0, len(changes)+1)
	seen := make(map[canon.StoreID]bool, 4)
	stores := make([]canon.StoreID, 0, 4)

	for i := range changes {
		pc := changes[i]
		if !seen[pc.StoreID] {
			seen[pc.StoreID] = true
			stores = append(stores, pc.StoreID)
		}
		env := canon.Envelope{
			EventID:       canon.NewEventID(),
			EventType:     canon.EvtPriceChangeRequested,
			AggregateType: aggregateTypePrice,
			AggregateID:   string(pc.StoreID) + ":" + string(pc.SKU),
			TenantID:      d.TenantID,
			StoreID:       pc.StoreID,
			Region:        p.cfg.Region,
			OccurredAt:    occurred,
			RecordedAt:    now,
			TraceID:       traceID,
			SpanID:        spanID,
			CorrelationID: corr,
			Source:        "uig/" + b.Adapter,
			SchemaVersion: canon.SchemaVersion,
			// Per-event key rather than per-delivery: contract §6 makes
			// Envelope.IdempotencyKey a no-op re-append marker in the event
			// store, and a delivery carrying 400 variants must not collapse to
			// one event on replay.
			IdempotencyKey: dedupeKey + "/" + strconv.Itoa(i),
		}
		env, err := env.WithPayload(pc)
		if err != nil {
			return nil, nil, err
		}
		if err := env.Validate(); err != nil {
			return nil, nil, err
		}
		body, err := json.Marshal(env)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, eventbus.Message{
			Topic: canon.StreamPriceUpdates.Name,
			Key:   env.PartitionKey(),
			Value: body,
			Headers: map[string]string{
				eventbus.HeaderEventType:     env.EventType,
				eventbus.HeaderTenantID:      string(env.TenantID),
				eventbus.HeaderStoreID:       string(env.StoreID),
				eventbus.HeaderCorrelationID: string(env.CorrelationID),
				eventbus.HeaderSchemaVersion: strconv.Itoa(canon.SchemaVersion),
				eventbus.HeaderIdempotency:   env.IdempotencyKey,
				eventbus.HeaderTraceParent:   traceParent(traceID, spanID),
			},
			Timestamp: env.RecordedAt,
		})
	}

	sum := sha256.Sum256(d.Body)
	primary := canon.StoreID("")
	if len(stores) > 0 {
		primary = stores[0]
	}
	raw := RawDelivery{
		DeliveryID:  d.ID,
		TenantID:    d.TenantID,
		BindingID:   b.ID,
		Adapter:     b.Adapter,
		POSInstance: b.POSInstance,
		Method:      d.Method,
		Path:        d.Path,
		ContentType: d.ContentType,
		ReceivedAt:  d.ReceivedAt.UTC(),
		BodySHA256:  hex.EncodeToString(sum[:]),
		BodySize:    len(d.Body),
		Body:        d.Body,
		Emitted:     len(changes),
		Stores:      stores,
		Replay:      d.Replay,
	}
	rawEnv := canon.Envelope{
		EventID:       canon.NewEventID(),
		EventType:     EventTypeDeliveryReceived,
		AggregateType: aggregateTypeDelivery,
		// pos-integration is keyed tenant:store by contract §2. The envelope's
		// PartitionKey falls back to AggregateID for a payload with no SKU, so
		// the key is put there directly.
		AggregateID:    string(d.TenantID) + ":" + string(primary),
		TenantID:       d.TenantID,
		StoreID:        primary,
		Region:         p.cfg.Region,
		OccurredAt:     occurred,
		RecordedAt:     now,
		TraceID:        traceID,
		SpanID:         spanID,
		CorrelationID:  corr,
		Source:         "uig/" + b.Adapter,
		SchemaVersion:  canon.SchemaVersion,
		IdempotencyKey: dedupeKey,
	}
	rawEnv, err := rawEnv.WithPayload(raw)
	if err != nil {
		return nil, nil, err
	}
	if err := rawEnv.Validate(); err != nil {
		return nil, nil, err
	}
	rawBody, err := json.Marshal(rawEnv)
	if err != nil {
		return nil, nil, err
	}
	msgs = append(msgs, eventbus.Message{
		Topic: canon.StreamPOSIngress.Name,
		Key:   rawEnv.PartitionKey(),
		Value: rawBody,
		Headers: map[string]string{
			eventbus.HeaderEventType:     rawEnv.EventType,
			eventbus.HeaderTenantID:      string(rawEnv.TenantID),
			eventbus.HeaderStoreID:       string(rawEnv.StoreID),
			eventbus.HeaderCorrelationID: string(rawEnv.CorrelationID),
			eventbus.HeaderSchemaVersion: strconv.Itoa(canon.SchemaVersion),
			eventbus.HeaderIdempotency:   rawEnv.IdempotencyKey,
		},
		Timestamp: rawEnv.RecordedAt,
	})
	return msgs, stores, nil
}

// dedupeKey derives the guard key from the adapter's vendor-specific parts.
//
// The tenant, binding and adapter are always mixed in, so two retailers whose
// POS systems happen to number their messages identically — which, with
// sequential integer message ids, they routinely do — can never dedupe each
// other's traffic.
func (p *Pipeline) dedupeKey(a adapter.Adapter, d *adapter.Delivery) string {
	parts := []string{"uig", string(d.TenantID), d.BindingID, a.Name()}
	vendor := a.IdempotencyParts(d)
	usable := false
	for _, v := range vendor {
		if strings.TrimSpace(v) != "" {
			usable = true
			break
		}
	}
	if usable {
		parts = append(parts, vendor...)
	} else {
		// No vendor identity: fall back to a digest of the exact bytes. Always
		// correct, merely coarser — it dedupes byte-identical redeliveries but
		// not a source that re-sends the same facts with a fresh timestamp.
		sum := sha256.Sum256(d.Body)
		parts = append(parts, "body-sha256", hex.EncodeToString(sum[:]))
	}
	return idem.Key(parts...)
}

func (p *Pipeline) release(ctx context.Context, d *adapter.Delivery, key string) {
	if d.Replay {
		return
	}
	if err := p.cfg.Guard.Release(ctx, key); err != nil {
		p.log.Error("uig: releasing idempotency key failed",
			"delivery_id", d.ID, "tenant_id", string(d.TenantID), "error", err)
	}
}

func (p *Pipeline) recordGuard(ctx context.Context, d *adapter.Delivery, key string, res *Result) {
	if d.Replay {
		// A replay deliberately bypasses the guard; recording its result under
		// the original key would overwrite the answer the POS was given.
		return
	}
	body, err := json.Marshal(res)
	if err != nil {
		p.log.Error("uig: encoding idempotency result failed", "delivery_id", d.ID, "error", err)
		return
	}
	if err := p.cfg.Guard.Record(ctx, key, body, 0); err != nil {
		p.log.Error("uig: recording idempotency result failed",
			"delivery_id", d.ID, "tenant_id", string(d.TenantID), "error", err)
	}
}

// finishDuplicate answers a redelivery with the original response.
func (p *Pipeline) finishDuplicate(ctx context.Context, d *adapter.Delivery, res *Result, start time.Time, previous []byte) *Result {
	res.Duplicate = true
	if len(previous) > 0 {
		var prev Result
		if err := json.Unmarshal(previous, &prev); err == nil {
			// Replay the original answer verbatim apart from the identity of
			// *this* delivery, so a retrying producer sees the outcome it would
			// have seen first time and stops retrying.
			status, http, emitted := prev.Status, prev.HTTPStatus, prev.Emitted
			reason, detail, rows := prev.Reason, prev.Detail, prev.RowFailures
			corr := prev.CorrelationID
			res.Status, res.HTTPStatus, res.Emitted = status, http, emitted
			res.Reason, res.Detail, res.RowFailures = reason, detail, rows
			if corr != "" {
				res.CorrelationID = corr
			}
		}
	}
	if res.Status == "" {
		// The original delivery is still in flight on another replica. Telling
		// the producer "accepted" would be a lie and telling it "failed" would
		// cause a duplicate; 202 with no emitted changes is the honest answer.
		res.Status = statusAccepted
		res.HTTPStatus = 202
		res.Detail = "an identical delivery is already being processed"
	}
	// A duplicate never emits, whatever the original did.
	res.Emitted = 0
	return p.finish(ctx, d, res, start, nil)
}

// finish stamps the result, files the record, updates metrics and returns.
func (p *Pipeline) finish(ctx context.Context, d *adapter.Delivery, res *Result, start time.Time, err error) *Result {
	end := p.now()
	dur := end.Sub(start)
	res.DurationMS = dur.Milliseconds()
	m := p.cfg.Metrics

	if err != nil {
		cls := adapter.Classify(err)
		res.Reason, res.Detail = cls.Reason, cls.Detail
		res.HTTPStatus = cls.Kind.HTTPStatus()
		if cls.Kind.RetainsBody() {
			res.Status = statusQuarantined
			m.Quarantined.With(res.Adapter, cls.Reason).Inc()
		} else {
			res.Status = statusRejected
		}
	}
	if res.Status == "" {
		res.Status = statusAccepted
	}
	if res.HTTPStatus == 0 {
		res.HTTPStatus = 202
	}
	if res.CorrelationID == "" {
		res.CorrelationID = correlationID(d)
	}

	m.IngestTotal.With(res.Adapter, string(res.TenantID), string(res.Status)).Inc()
	m.IngestDuration.With(res.Adapter).Observe(dur.Seconds())
	if dur > LatencyBudget {
		m.BudgetExceeded.With(res.Adapter).Inc()
	}
	p.cfg.Health.Record(res, end)

	rec := &deliveries.Record{
		ID:              d.ID,
		TenantID:        d.TenantID,
		BindingID:       d.BindingID,
		Adapter:         res.Adapter,
		Status:          res.Status,
		ReceivedAt:      d.ReceivedAt.UTC(),
		CompletedAt:     end.UTC(),
		DurationMS:      res.DurationMS,
		HTTPStatus:      res.HTTPStatus,
		Reason:          res.Reason,
		Detail:          res.Detail,
		Emitted:         res.Emitted,
		RowFailures:     res.RowFailures,
		Method:          d.Method,
		URL:             d.URL,
		Path:            d.Path,
		Headers:         d.Headers,
		ContentType:     d.ContentType,
		Body:            d.Body,
		BodySize:        len(d.Body),
		ForceRetainBody: d.Binding != nil && d.Binding.RetainRaw,
	}
	if d.Replay {
		rec.ReplayOf = d.ReplayOf
		rec.ReplayCount = d.ReplayCount
	}
	// A record whose body must be retained is written before the caller is
	// answered: support cannot triage a 4xx whose payload was never stored, and
	// the operator replay endpoint has nothing to replay. A record without a
	// body is bookkeeping and is filed asynchronously, off the acknowledgement
	// path.
	if rec.Status.RetainsBody() || rec.ForceRetainBody {
		if err := p.cfg.Deliveries.Put(rec); err != nil {
			p.log.Error("uig: storing delivery failed",
				"delivery_id", d.ID, "tenant_id", string(d.TenantID), "error", err)
		}
	} else {
		p.fileAsync(rec)
	}

	lg := p.log.FromContext(ctx).WithTenant(string(d.TenantID), "")
	if res.HTTPStatus >= 400 {
		lg.Warn("uig: delivery refused",
			"delivery_id", d.ID, "binding_id", d.BindingID, "adapter", res.Adapter,
			"status", string(res.Status), "reason", res.Reason, "detail", res.Detail,
			"http_status", res.HTTPStatus, "duration_ms", res.DurationMS)
	} else {
		lg.Info("uig: delivery accepted",
			"delivery_id", d.ID, "binding_id", d.BindingID, "adapter", res.Adapter,
			"status", string(res.Status), "emitted", res.Emitted,
			"duplicate", res.Duplicate, "duration_ms", res.DurationMS)
	}
	return res
}

// fileAsync hands a record to the bookkeeping pool, falling back to an inline
// write when the queue is full. Backpressure rather than loss: a saturated
// gateway should get slower, not start forgetting what it accepted.
func (p *Pipeline) fileAsync(rec *deliveries.Record) {
	// The read lock is held across the send so that Close, which needs the
	// write lock, cannot close the channel underneath it.
	p.closeMu.RLock()
	if !p.closed {
		// The counter is raised before the send so that a worker can never
		// decrement it below the true outstanding count.
		p.pending.Add(1)
		select {
		case p.records <- rec:
			p.closeMu.RUnlock()
			return
		default:
			p.pending.Add(-1)
		}
	}
	p.closeMu.RUnlock()
	if err := p.cfg.Deliveries.Put(rec); err != nil {
		p.log.Error("uig: storing delivery failed", "delivery_id", rec.ID, "error", err)
	}
}

func (p *Pipeline) startSpan(ctx context.Context, name string) (context.Context, *obs.Span) {
	if p.cfg.Tracer == nil {
		return ctx, nil
	}
	// The price path is always sampled: a 3-second budget cannot be debugged
	// from a one-in-a-thousand sample.
	return p.cfg.Tracer.StartAlwaysSampled(ctx, name)
}

// correlationID takes the caller's correlation id when it supplied one, so a
// trace that started in the retailer's POS continues into USSLP rather than
// restarting at the boundary.
func correlationID(d *adapter.Delivery) canon.CorrelationID {
	for _, h := range []string{"X-Correlation-Id", "X-Request-Id"} {
		if v := d.Header(h); v != "" && canon.ValidID(v) {
			return canon.CorrelationID(v)
		}
	}
	if tp := d.Header("traceparent"); tp != "" {
		if sc := obs.ParseTraceParent(tp); sc.Valid() {
			return canon.CorrelationID(sc.TraceID)
		}
	}
	return canon.NewCorrelationID()
}

func traceIDs(d *adapter.Delivery) (traceID, spanID string) {
	if tp := d.Header("traceparent"); tp != "" {
		if sc := obs.ParseTraceParent(tp); sc.Valid() {
			return sc.TraceID, canon.NewSpanID()
		}
	}
	return canon.NewTraceID(), canon.NewSpanID()
}

func traceParent(traceID, spanID string) string {
	return "00-" + traceID + "-" + spanID + "-01"
}

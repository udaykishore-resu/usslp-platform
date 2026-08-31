package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// concurrencyAttempts is how many times a command re-reads and re-decides after
// losing an optimistic concurrency race.
//
// Two, not more. The contract says a genuine concurrent update should win
// rather than error, and one reload is enough for that: the losing command
// reloads, sees the winner's price, and either supersedes it or is correctly
// judged stale. A longer loop would turn a hot SKU — a store-wide promotion
// touching one label from two partitions — into unbounded work on the hot path,
// and the third attempt is far more likely to be a symptom of a duplicated
// consumer than of real contention.
const concurrencyAttempts = 2

// PriceCommand is one label's share of a price change.
type PriceCommand struct {
	// Placement is where the label sits. It supplies the MQTT topic scope, so
	// it must come from the directory rather than be reconstructed.
	Placement ports.Placement
	// Change is the domain command.
	Change domain.PriceChange
	// Cause is the envelope that caused this, when the update came from a
	// stream. Its trace context, correlation and tenancy are inherited by
	// every event and message this command produces, which is what keeps one
	// POS webhook visible as one trace across nine hops.
	Cause canon.Envelope
	// IdempotencyKey makes the aggregate append a no-op on redelivery. It must
	// be stable across deliveries of the same logical update and distinct
	// between labels, because one stream record fans out to every facing of a
	// product and they all append to different streams.
	IdempotencyKey string
}

// PriceResult reports what happened to one label.
type PriceResult struct {
	// LabelID is the label the result belongs to.
	LabelID canon.LabelID `json:"label_id"`
	// Outcome is one of the Outcome* constants.
	Outcome string `json:"outcome"`
	// Reason is the rejection reason, empty unless Outcome is rejected.
	Reason string `json:"reason,omitempty"`
	// Detail explains a rejection or an error in words.
	Detail string `json:"detail,omitempty"`
	// Sequence is the per-label sequence the update was published under.
	Sequence int64 `json:"sequence,omitempty"`
	// Version is the aggregate version after the command.
	Version int64 `json:"version,omitempty"`
	// EffectiveAt is when the price takes or took effect.
	EffectiveAt time.Time `json:"effective_at,omitempty"`
	// Attested reports whether a signature was produced and travelled with the
	// update. A false here on an applied update is a compliance incident.
	Attested bool `json:"attested,omitempty"`
}

// Applied reports whether the update reached the glass.
func (r PriceResult) Applied() bool { return r.Outcome == OutcomeApplied }

// UpdatePriceHandler is the hot path.
//
// It consumes the `price-updates` stream, resolves the affected labels from the
// local directory read model, and for each one loads the aggregate, applies the
// command, attests the price, appends the resulting events, publishes to the
// store's broker and emits the platform's downstream records. The whole of that
// owns 120 ms of the three-second budget (INTERFACE-CONTRACTS §4), which is why
// the directory is a local read model and not a call to the Device Registry.
type UpdatePriceHandler struct {
	deps Deps
	// guard de-duplicates stream deliveries at ingress. The aggregate's
	// idempotency key already makes a redelivered append a no-op, but that is
	// one round trip per label; the guard short-circuits the whole fan-out of a
	// record the service has already finished, which for a store-wide promotion
	// is 40,000 aggregate loads avoided.
	guard *idem.Guard
}

// NewUpdatePriceHandler builds the price handler. The guard may be nil, in
// which case de-duplication falls back to the aggregate's idempotency key
// alone — correct, but more expensive.
func NewUpdatePriceHandler(deps Deps, guard *idem.Guard) (*UpdatePriceHandler, error) {
	deps = deps.withDefaults()
	if err := deps.requirePricePath(); err != nil {
		return nil, err
	}
	if deps.Directory == nil {
		return nil, fmt.Errorf("%w: Directory", ErrMissingDependency)
	}
	return &UpdatePriceHandler{deps: deps, guard: guard}, nil
}

// HandleMessage adapts the handler to an event-bus subscription.
func (h *UpdatePriceHandler) HandleMessage(ctx context.Context, m eventbus.Message) error {
	var env canon.Envelope
	if err := json.Unmarshal(m.Value, &env); err != nil {
		// A record that cannot be parsed will never parse. Returning a
		// permanent-shaped error routes it to the dead-letter stream instead of
		// wedging a consumer group serving 1,024 partitions.
		return fmt.Errorf("%w: price-updates record at %s/%d/%d: %v",
			canon.ErrEnvelopeInvalid, m.Topic, m.Partition, m.Offset, err)
	}
	return h.HandleEnvelope(ctx, env)
}

// HandleEnvelope processes one `pricing.change.requested` envelope, fanning it
// out across every label showing the product in the store.
//
// A record touching several labels is all-or-nothing only in the sense that the
// caller retries it: each label's outcome is independent and durable on its own
// stream, and a redelivery is a no-op for the labels that already succeeded.
// That is the only shape that works when one record can touch forty facings and
// the thirty-ninth broker publish fails.
func (h *UpdatePriceHandler) HandleEnvelope(ctx context.Context, env canon.Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	ctx = obs.WithRemoteContext(ctx, obs.SpanContext{
		TraceID: env.TraceID, SpanID: env.SpanID, Sampled: true,
	})
	ctx, span := h.deps.Tracer.StartAlwaysSampled(ctx, "label.price.fanout")
	defer span.End()
	span.SetAttr("tenant", string(env.TenantID)).
		SetAttr("store", string(env.StoreID)).
		SetAttr("event_id", string(env.EventID))

	key := dedupeKey(env)
	if h.guard != nil && key != "" {
		firstSeen, _, err := h.guard.Check(ctx, key)
		if err != nil {
			return fmt.Errorf("label: idempotency check for %s: %w", env.EventID, err)
		}
		if !firstSeen {
			// Already processed, or in flight on another replica. Either way
			// this delivery must not publish a second time.
			span.AddEvent("deduplicated")
			h.deps.Metrics.PriceUpdates.With(OutcomeStale, "duplicate_record").Inc()
			return nil
		}
	}
	results, err := h.fanOut(ctx, env)
	if err != nil {
		if h.guard != nil && key != "" {
			// Release before returning so the producer's retry is treated as a
			// first delivery. Without this a delivery that claimed the key and
			// then failed would suppress every retry for the whole window, and
			// the price change would simply never be applied — a silent loss,
			// which is the worst failure mode a pricing system has.
			if rerr := h.guard.Release(ctx, key); rerr != nil {
				h.deps.Log.FromContext(ctx).Error("releasing idempotency key failed",
					"key", key, "error", rerr)
			}
		}
		span.Fail(err)
		return err
	}
	if h.guard != nil && key != "" {
		body, _ := json.Marshal(results)
		if rerr := h.guard.Record(ctx, key, body, 0); rerr != nil {
			h.deps.Log.FromContext(ctx).Warn("recording idempotency result failed",
				"key", key, "error", rerr)
		}
	}
	span.SetAttrInt("labels", int64(len(results)))
	return nil
}

func dedupeKey(env canon.Envelope) string {
	if env.IdempotencyKey != "" {
		return idem.Key("label-service", "price", env.IdempotencyKey)
	}
	if env.EventID != "" {
		return idem.Key("label-service", "price", string(env.EventID))
	}
	return ""
}

func (h *UpdatePriceHandler) fanOut(ctx context.Context, env canon.Envelope) ([]PriceResult, error) {
	var req canon.PriceChangeRequested
	if err := env.Decode(&req); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", canon.ErrEnvelopeInvalid, err)
	}
	store := req.StoreID
	if store == "" {
		store = env.StoreID
	}
	placements, err := h.deps.Directory.LabelsForSKU(ctx, env.TenantID, store, req.SKU)
	if err != nil {
		return nil, fmt.Errorf("label: resolving labels for %s/%s/%s: %w", env.TenantID, store, req.SKU, err)
	}
	if len(placements) == 0 {
		// Not an error. A tenant's catalogue is far larger than the set of
		// products any one store gives shelf space to, and the price of a
		// product this store does not stock is a fact with no consequence here.
		h.deps.Log.FromContext(ctx).Debug("price change touches no labels",
			"tenant", string(env.TenantID), "store", string(store), "sku", string(req.SKU))
		return nil, nil
	}

	now := h.deps.Clock.Now()
	results := make([]PriceResult, 0, len(placements))
	var firstErr error
	for _, p := range placements {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		cmd := PriceCommand{
			Placement:      p,
			Change:         ChangeFromRequest(req, env, now),
			Cause:          env,
			IdempotencyKey: labelIdempotencyKey(env, p.LabelID),
		}
		res, err := h.Apply(ctx, cmd)
		if err != nil {
			// Keep going: the other facings of this product are independent,
			// and failing the whole record would re-publish to the labels that
			// already succeeded on every retry.
			if firstErr == nil {
				firstErr = err
			}
			h.deps.Log.FromContext(ctx).Error("label price update failed",
				"label_id", string(p.LabelID), "sku", string(req.SKU), "error", err)
			results = append(results, PriceResult{
				LabelID: p.LabelID, Outcome: OutcomeError, Detail: err.Error(),
			})
			continue
		}
		results = append(results, res)
	}
	return results, firstErr
}

// ChangeFromRequest maps the canonical POS-normalised request onto the domain
// command. It is exported because the batch pipeline and the HTTP surface build
// the same command from their own inputs and must not drift from this mapping.
func ChangeFromRequest(req canon.PriceChangeRequested, env canon.Envelope, now time.Time) domain.PriceChange {
	occurred := env.OccurredAt
	if occurred.IsZero() {
		occurred = req.EffectiveAt
	}
	return domain.PriceChange{
		SKU:           req.SKU,
		Price:         req.Price,
		WasPrice:      req.WasPrice,
		UnitPrice:     req.UnitPrice,
		UnitMeasure:   req.UnitMeasure,
		EffectiveAt:   req.EffectiveAt,
		ExpiresAt:     req.ExpiresAt,
		PromotionID:   req.PromotionID,
		Reason:        req.Reason,
		Attributes:    req.Attributes,
		InitiatedBy:   req.InitiatedBy,
		OccurredAt:    occurred,
		Now:           now,
		SourceEventID: env.EventID,
	}
}

// labelIdempotencyKey derives the per-label append key. It includes the label
// because one stream record fans out to several aggregates, each of which
// applies the key to its own stream.
func labelIdempotencyKey(env canon.Envelope, label canon.LabelID) string {
	base := env.IdempotencyKey
	if base == "" {
		base = string(env.EventID)
	}
	if base == "" {
		return ""
	}
	return base + "#" + string(label)
}

// Apply runs one label's price command: load, decide, attest, append, publish.
//
// The order is not negotiable. The events are durable before anything is
// published, so a price on a shelf is always explainable from the event store;
// the attestation is computed before the append, so the signature that
// authorises a price is stored with it rather than re-derived later by a
// service that might disagree about what was signed; and the device publish
// happens before the stream publishes, because the shelf is what the SLO
// measures and the analytics pipeline is not.
func (h *UpdatePriceHandler) Apply(ctx context.Context, cmd PriceCommand) (PriceResult, error) {
	label := cmd.Placement.LabelID
	if label == "" {
		return PriceResult{}, fmt.Errorf("%w: PriceCommand.Placement.LabelID is required", domain.ErrInvalidCommand)
	}
	ctx, span := h.deps.Tracer.StartAlwaysSampled(ctx, "label.price.apply")
	defer span.End()
	span.SetAttr("label_id", string(label)).SetAttr("sku", string(cmd.Change.SKU))

	var lastErr error
	for attempt := 1; attempt <= concurrencyAttempts; attempt++ {
		res, err := h.applyOnce(ctx, cmd)
		if err == nil {
			span.SetAttr("outcome", res.Outcome).SetAttrInt("sequence", res.Sequence)
			return res, nil
		}
		if !errors.Is(err, ports.ErrConcurrency) {
			span.Fail(err)
			return res, err
		}
		lastErr = err
		span.AddEvent("concurrency_conflict", "attempt", fmt.Sprint(attempt))
	}
	// Out of attempts: report the conflict so the consumer retries with fresh
	// backoff rather than silently dropping a price change.
	span.Fail(lastErr)
	h.deps.Metrics.PriceUpdates.With(OutcomeError, "concurrency").Inc()
	return PriceResult{LabelID: label, Outcome: OutcomeError, Detail: lastErr.Error()}, lastErr
}

func (h *UpdatePriceHandler) applyOnce(ctx context.Context, cmd PriceCommand) (PriceResult, error) {
	label := cmd.Placement.LabelID
	agg, err := h.deps.Repo.Load(ctx, label)
	if err != nil {
		return PriceResult{}, fmt.Errorf("label: loading %s: %w", label, err)
	}
	if !agg.Exists() {
		// The directory knows about a label the write side has never seen. That
		// is a projection running ahead of provisioning, which resolves itself;
		// it is not a poison record.
		h.deps.Metrics.PriceUpdates.With(OutcomeRejected, domain.ReasonNotAssigned).Inc()
		return PriceResult{
			LabelID: label, Outcome: OutcomeRejected,
			Reason: domain.ReasonNotAssigned, Detail: "label is not provisioned",
		}, nil
	}

	policy := h.deps.Policies.For(agg.TenantID)
	events, decideErr := agg.ApplyPriceChange(cmd.Change, policy)

	switch {
	case errors.Is(decideErr, domain.ErrAlreadyApplied):
		return h.republish(ctx, agg, cmd)
	case errors.Is(decideErr, domain.ErrStaleUpdate), errors.Is(decideErr, domain.ErrOutOfOrder):
		reason := "stale_sequence"
		if errors.Is(decideErr, domain.ErrOutOfOrder) {
			reason = "out_of_order"
		}
		h.deps.Metrics.PriceUpdates.With(OutcomeStale, reason).Inc()
		return PriceResult{
			LabelID: label, Outcome: OutcomeStale, Reason: reason,
			Detail: decideErr.Error(), Sequence: agg.Sequence, Version: agg.Version,
		}, nil
	case errors.Is(decideErr, domain.ErrInvalidCommand):
		// A malformed command is a producer bug. It is not retryable and must
		// not be dressed up as a rejection event, which would put a fabricated
		// business reason in the compliance archive.
		return PriceResult{}, decideErr
	}

	return h.commit(ctx, agg, events, decideErr, cmd)
}

// commit persists a decision and carries out its consequences: signing,
// appending, keeping the due-index in step, publishing to the device and the
// streams, and refreshing the read model.
//
// It is shared by the stream path and by the scheduled-price runner so that an
// activation at its effective time is indistinguishable, in the event store and
// on the wire, from an immediate change — which is the property that lets one
// audit query answer "how did this price get here" for both.
func (h *UpdatePriceHandler) commit(ctx context.Context, agg *domain.Label, events []domain.Event, decideErr error, cmd PriceCommand) (PriceResult, error) {
	label := agg.ID
	meta := h.metaFor(agg, cmd)
	schedulesBefore := scheduleIDs(agg)
	outcome, err := h.persist(ctx, agg, events, meta)
	if err != nil {
		return PriceResult{}, err
	}

	if decideErr != nil { // a rejection, already recorded as an event
		reason := domain.RejectionReason(decideErr)
		h.deps.Metrics.PriceUpdates.With(OutcomeRejected, reason).Inc()
		if reason == domain.ReasonGuardrail {
			h.deps.Metrics.GuardrailRejections.With(string(agg.TenantID)).Inc()
			h.deps.Log.FromContext(ctx).Warn("price change refused by guard rail",
				"label_id", string(label), "sku", string(cmd.Change.SKU),
				"current", agg.Price.String(), "proposed", cmd.Change.Price.String())
		}
		if err := h.publishRejection(ctx, agg, outcome, cmd); err != nil {
			return PriceResult{}, err
		}
		h.writeState(ctx, agg)
		return PriceResult{
			LabelID: label, Outcome: OutcomeRejected, Reason: reason,
			Detail: decideErr.Error(), Version: agg.Version,
		}, nil
	}

	if err := h.syncSchedules(ctx, agg, schedulesBefore); err != nil {
		return PriceResult{}, err
	}

	applied, ok := lastApplied(outcome.Events)
	if !ok {
		// Nothing but a schedule was produced: the change is future dated and
		// nothing reaches the glass until the runner activates it.
		h.deps.Metrics.PriceUpdates.With(OutcomeScheduled, "").Inc()
		h.writeState(ctx, agg)
		sched, _ := lastScheduled(outcome.Events)
		return PriceResult{
			LabelID: label, Outcome: OutcomeScheduled, Version: agg.Version,
			EffectiveAt: sched.EffectiveAt,
		}, nil
	}

	env, err := h.priceEnvelope(agg, applied, cmd)
	if err != nil {
		return PriceResult{}, err
	}
	if err := h.publishUpdate(ctx, agg, cmd.Placement, env); err != nil {
		return PriceResult{}, err
	}
	h.writeState(ctx, agg)

	h.deps.Metrics.PriceUpdates.With(OutcomeApplied, "").Inc()
	h.deps.Metrics.PendingDelivery.With(string(agg.StoreID)).Inc()
	return PriceResult{
		LabelID: label, Outcome: OutcomeApplied, Sequence: applied.Sequence,
		Version: agg.Version, EffectiveAt: applied.EffectiveAt, Attested: true,
	}, nil
}

// persist appends the decided events, signing any price among them first.
func (h *UpdatePriceHandler) persist(ctx context.Context, agg *domain.Label, events []domain.Event, meta ports.AppendMeta) (ports.AppendOutcome, error) {
	if len(events) == 0 {
		return ports.AppendOutcome{Version: agg.Version}, nil
	}
	signed := make([]domain.Event, len(events))
	copy(signed, events)
	for i := range signed {
		applied, ok := signed[i].(domain.PriceApplied)
		if !ok {
			continue
		}
		att, err := h.deps.Attestor.Sign(canon.AttestationInput{
			TenantID: agg.TenantID, StoreID: applied.StoreID, LabelID: applied.LabelID,
			SKU: applied.SKU, Price: applied.Price, EffectiveAt: applied.EffectiveAt,
			Sequence: applied.Sequence, PromotionID: applied.PromotionID,
		})
		if err != nil {
			// Non-negotiable: a label never displays a price it cannot verify,
			// so an unsigned price is never persisted and never published.
			h.deps.Metrics.AttestationFailures.With("sign_failed").Inc()
			return ports.AppendOutcome{}, fmt.Errorf("label: attesting %s sequence %d: %w",
				applied.LabelID, applied.Sequence, err)
		}
		applied.Attestation = att
		signed[i] = applied
	}
	expected := agg.Version
	outcome, err := h.deps.Repo.Append(ctx, agg.ID, expected, signed, meta)
	if err != nil {
		return ports.AppendOutcome{}, err
	}
	// Reflect the decision onto the in-memory aggregate so the read-model write
	// and the returned result describe the state the store now holds.
	for _, e := range signed {
		agg.Apply(e)
		agg.Version++
	}
	return outcome, nil
}

func (h *UpdatePriceHandler) metaFor(agg *domain.Label, cmd PriceCommand) ports.AppendMeta {
	meta := ports.AppendMeta{
		TenantID:       agg.TenantID,
		StoreID:        agg.StoreID,
		Region:         agg.Region,
		Source:         SourceName,
		IdempotencyKey: cmd.IdempotencyKey,
		OccurredAt:     cmd.Change.OccurredAt,
	}
	if cmd.Cause.EventID != "" {
		meta.TraceID = cmd.Cause.TraceID
		meta.CorrelationID = cmd.Cause.CorrelationID
		meta.CausationID = cmd.Cause.EventID
		if meta.TenantID == "" {
			meta.TenantID = cmd.Cause.TenantID
		}
	}
	if meta.CorrelationID == "" {
		meta.CorrelationID = canon.NewCorrelationID()
	}
	return meta
}

// priceEnvelope builds the `label.price.updated` envelope that travels to the
// device, the compacted state stream and the audit archive. One envelope, three
// destinations: an auditor comparing the shelf against the archive must be
// comparing byte-identical payloads, not two renderings of one decision.
func (h *UpdatePriceHandler) priceEnvelope(agg *domain.Label, applied domain.PriceApplied, cmd PriceCommand) (canon.Envelope, error) {
	payload := canon.PriceUpdated{
		LabelID: applied.LabelID, SKU: applied.SKU, StoreID: applied.StoreID,
		Price: applied.Price, WasPrice: applied.WasPrice, UnitPrice: applied.UnitPrice,
		UnitMeasure: applied.UnitMeasure, EffectiveAt: applied.EffectiveAt,
		PromotionID: applied.PromotionID, Render: applied.Render,
		Attestation: applied.Attestation, Sequence: applied.Sequence,
	}
	env, err := h.childEnvelope(agg, cmd, canon.EvtPriceUpdated, payload)
	if err != nil {
		return canon.Envelope{}, err
	}
	env.Version = agg.Version
	return env, nil
}

func (h *UpdatePriceHandler) childEnvelope(agg *domain.Label, cmd PriceCommand, eventType string, payload any) (canon.Envelope, error) {
	if cmd.Cause.EventID != "" {
		env, err := cmd.Cause.Caused(eventType, domain.AggregateType, string(agg.ID), payload)
		if err != nil {
			return canon.Envelope{}, err
		}
		env.Source = SourceName
		env.StoreID = agg.StoreID
		env.Region = agg.Region
		env.IdempotencyKey = cmd.IdempotencyKey
		return env, nil
	}
	env, err := canon.NewEnvelope(eventType, domain.AggregateType, string(agg.ID), agg.TenantID, payload)
	if err != nil {
		return canon.Envelope{}, err
	}
	env.StoreID = agg.StoreID
	env.Region = agg.Region
	env.Source = SourceName
	env.IdempotencyKey = cmd.IdempotencyKey
	env.CorrelationID = canon.NewCorrelationID()
	return env, nil
}

// publishUpdate sends the update to the device and then to the streams.
func (h *UpdatePriceHandler) publishUpdate(ctx context.Context, agg *domain.Label, p ports.Placement, env canon.Envelope) error {
	target := p
	if target.SECID == "" {
		target.SECID = agg.SECID
	}
	target.TenantID = agg.TenantID
	target.StoreID = agg.StoreID
	target.Region = agg.Region

	start := h.deps.Clock.Now()
	if err := h.deps.Device.PublishPrice(ctx, target, env); err != nil {
		h.deps.Metrics.PriceUpdates.With(OutcomeError, "device_publish").Inc()
		return fmt.Errorf("label: publishing %s to %s: %w", agg.ID, target.SECID, err)
	}
	h.deps.Metrics.DevicePublishDuration.With(string(agg.StoreID)).
		Observe(h.deps.Clock.Now().Sub(start).Seconds())

	// The compacted label-state stream is what a read model rebuilds from
	// without replaying seven days of price traffic; the audit stream is the
	// statutory record. Both carry the same envelope as the device did.
	if err := h.deps.Streams.Publish(ctx, canon.StreamLabelState.Name, env); err != nil {
		return fmt.Errorf("label: publishing label-state for %s: %w", agg.ID, err)
	}
	if err := h.deps.Streams.Publish(ctx, canon.StreamAudit.Name, env); err != nil {
		return fmt.Errorf("label: publishing audit record for %s: %w", agg.ID, err)
	}
	return nil
}

func (h *UpdatePriceHandler) publishRejection(ctx context.Context, agg *domain.Label, outcome ports.AppendOutcome, cmd PriceCommand) error {
	for _, se := range outcome.Events {
		rejected, ok := deref(se.Event).(domain.PriceRejected)
		if !ok {
			continue
		}
		env, err := h.childEnvelope(agg, cmd, canon.EvtPriceRejected, rejected)
		if err != nil {
			return err
		}
		// Rejections go to the audit stream only. Putting them on the compacted
		// label-state stream would overwrite the label's last known good state
		// with a record describing a price that never reached it.
		if err := h.deps.Streams.Publish(ctx, canon.StreamAudit.Name, env); err != nil {
			return fmt.Errorf("label: publishing rejection for %s: %w", agg.ID, err)
		}
	}
	return nil
}

// republish re-sends an update the event store already holds.
//
// This is the recovery path for a crash or an MQTT failure between the durable
// append and the device publish. It re-sends the stored envelope verbatim
// rather than re-deciding, because a re-decision could legitimately produce a
// different render — a different partial-refresh verdict, say — and the label
// must not receive two different renderings under one sequence number.
func (h *UpdatePriceHandler) republish(ctx context.Context, agg *domain.Label, cmd PriceCommand) (PriceResult, error) {
	if agg.Pending == nil {
		return PriceResult{LabelID: agg.ID, Outcome: OutcomeStale, Reason: "already_applied"}, nil
	}
	history, err := h.deps.Repo.History(ctx, agg.ID, 32)
	if err != nil {
		return PriceResult{}, fmt.Errorf("label: reading history of %s: %w", agg.ID, err)
	}
	for _, se := range history {
		applied, ok := asPriceApplied(se.Event)
		if !ok || applied.Sequence != agg.Pending.Sequence {
			continue
		}
		env, err := h.priceEnvelope(agg, applied, cmd)
		if err != nil {
			return PriceResult{}, err
		}
		if err := h.publishUpdate(ctx, agg, cmd.Placement, env); err != nil {
			return PriceResult{}, err
		}
		h.deps.Metrics.PriceUpdates.With(OutcomeRepublished, "").Inc()
		return PriceResult{
			LabelID: agg.ID, Outcome: OutcomeRepublished, Sequence: applied.Sequence,
			Version: agg.Version, EffectiveAt: applied.EffectiveAt, Attested: true,
		}, nil
	}
	return PriceResult{
		LabelID: agg.ID, Outcome: OutcomeStale, Reason: "already_applied",
		Sequence: agg.Sequence, Version: agg.Version,
	}, nil
}

// scheduleIDs snapshots the aggregate's pending schedule identifiers.
func scheduleIDs(agg *domain.Label) map[string]time.Time {
	out := make(map[string]time.Time, len(agg.Scheduled))
	for _, s := range agg.Scheduled {
		out[s.ScheduleID] = s.EffectiveAt
	}
	return out
}

// syncSchedules reconciles the due-index against the aggregate.
//
// It diffs rather than reacting to individual events because three different
// things remove a schedule — an explicit cancellation, a supersession folded
// away by a newly applied price, and an activation — and a rule per event type
// is a rule that will eventually miss one and leave the runner waking for a
// change that no longer exists.
func (h *UpdatePriceHandler) syncSchedules(ctx context.Context, agg *domain.Label, before map[string]time.Time) error {
	if h.deps.Schedules == nil {
		return nil
	}
	after := scheduleIDs(agg)
	for id, at := range after {
		if _, existed := before[id]; existed {
			continue
		}
		if err := h.deps.Schedules.Add(ctx, ports.ScheduleEntry{
			ScheduleID: id, LabelID: agg.ID, TenantID: agg.TenantID,
			StoreID: agg.StoreID, EffectiveAt: at,
		}); err != nil {
			return fmt.Errorf("label: scheduling %s on %s: %w", id, agg.ID, err)
		}
	}
	for id := range before {
		if _, still := after[id]; still {
			continue
		}
		if err := h.deps.Schedules.Remove(ctx, agg.ID, id); err != nil {
			return fmt.Errorf("label: dropping schedule %s on %s: %w", id, agg.ID, err)
		}
	}
	return nil
}

// writeState refreshes the query-side row. It is a write-through rather than a
// pure projection so the HTTP API gives read-your-writes to the operator who
// just pushed a price; the projection remains the authority and rebuilds the
// same row from the same function.
func (h *UpdatePriceHandler) writeState(ctx context.Context, agg *domain.Label) {
	if h.deps.State == nil {
		return
	}
	if err := h.deps.State.Put(ctx, StateFromLabel(agg)); err != nil {
		h.deps.Log.FromContext(ctx).Warn("updating label read model failed",
			"label_id", string(agg.ID), "error", err)
	}
}

func lastApplied(events []ports.StoredEvent) (domain.PriceApplied, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if a, ok := asPriceApplied(events[i].Event); ok {
			return a, true
		}
	}
	return domain.PriceApplied{}, false
}

func lastScheduled(events []ports.StoredEvent) (domain.PriceScheduled, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if ev, ok := deref(events[i].Event).(domain.PriceScheduled); ok {
			return ev, true
		}
	}
	return domain.PriceScheduled{}, false
}

func asPriceApplied(e domain.Event) (domain.PriceApplied, bool) {
	switch v := e.(type) {
	case domain.PriceApplied:
		return v, true
	case *domain.PriceApplied:
		return *v, true
	}
	return domain.PriceApplied{}, false
}

// deref normalises the pointer forms the event decoder produces to values, so
// application-layer type switches only need one case per event.
func deref(e domain.Event) domain.Event {
	switch v := e.(type) {
	case *domain.LabelProvisioned:
		return *v
	case *domain.LabelAssigned:
		return *v
	case *domain.PriceApplied:
		return *v
	case *domain.PriceScheduled:
		return *v
	case *domain.ScheduleCancelled:
		return *v
	case *domain.PriceRejected:
		return *v
	case *domain.DeliveryConfirmed:
		return *v
	case *domain.DeliveryFailed:
		return *v
	case *domain.LabelWentOffline:
		return *v
	case *domain.LabelCameOnline:
		return *v
	case *domain.LabelRetired:
		return *v
	}
	return e
}

// Activate brings a due scheduled price change to the glass.
//
// The sequence is allocated here rather than when the change was scheduled, so
// an urgent price change made in the meantime still wins at the label:
// sequences are handed out in the order updates actually reach the glass, which
// is the order the label's discard rule enforces.
func (h *UpdatePriceHandler) Activate(ctx context.Context, entry ports.ScheduleEntry) (PriceResult, error) {
	ctx, span := h.deps.Tracer.StartAlwaysSampled(ctx, "label.price.activate")
	defer span.End()
	span.SetAttr("label_id", string(entry.LabelID)).SetAttr("schedule_id", entry.ScheduleID)

	now := h.deps.Clock.Now()
	var lastErr error
	for attempt := 1; attempt <= concurrencyAttempts; attempt++ {
		agg, err := h.deps.Repo.Load(ctx, entry.LabelID)
		if err != nil {
			return PriceResult{}, fmt.Errorf("label: loading %s: %w", entry.LabelID, err)
		}
		sched, ok := agg.Schedule(entry.ScheduleID)
		if !ok {
			// Superseded or already activated. Removing the index entry is the
			// whole of the work left to do.
			if h.deps.Schedules != nil {
				if rerr := h.deps.Schedules.Remove(ctx, entry.LabelID, entry.ScheduleID); rerr != nil {
					return PriceResult{}, rerr
				}
			}
			return PriceResult{LabelID: entry.LabelID, Outcome: OutcomeStale, Reason: "schedule_superseded"}, nil
		}
		policy := h.deps.Policies.For(agg.TenantID)
		events, decideErr := agg.ActivateSchedule(entry.ScheduleID, policy, now)
		if errors.Is(decideErr, domain.ErrStaleUpdate) || errors.Is(decideErr, domain.ErrOutOfOrder) {
			if h.deps.Schedules != nil {
				if rerr := h.deps.Schedules.Remove(ctx, entry.LabelID, entry.ScheduleID); rerr != nil {
					return PriceResult{}, rerr
				}
			}
			return PriceResult{LabelID: entry.LabelID, Outcome: OutcomeStale, Reason: "schedule_superseded"}, nil
		}
		if decideErr != nil && !errors.Is(decideErr, domain.ErrRejected) {
			return PriceResult{}, decideErr
		}
		cmd := PriceCommand{
			Placement: ports.Placement{
				LabelID: agg.ID, SECID: agg.SECID, TenantID: agg.TenantID,
				StoreID: agg.StoreID, Region: agg.Region, SKU: sched.SKU,
			},
			Change:         domain.PriceChange{SKU: sched.SKU, Price: sched.Price, Now: now, OccurredAt: now, EffectiveAt: sched.EffectiveAt},
			IdempotencyKey: "schedule:" + entry.ScheduleID,
		}
		res, err := h.commit(ctx, agg, events, decideErr, cmd)
		if err == nil {
			if h.deps.Schedules != nil {
				if rerr := h.deps.Schedules.Remove(ctx, entry.LabelID, entry.ScheduleID); rerr != nil {
					return res, rerr
				}
			}
			return res, nil
		}
		if !errors.Is(err, ports.ErrConcurrency) {
			return res, err
		}
		lastErr = err
	}
	return PriceResult{}, lastErr
}

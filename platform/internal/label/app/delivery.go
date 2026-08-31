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
	"github.com/usslp/usslp/platform/pkg/obs"
)

// Delivery outcome labels.
const (
	// DeliveryConfirmed marks an acknowledged, displayed update.
	DeliveryConfirmed = "confirmed"
	// DeliveryFailed marks an update the edge gave up on.
	DeliveryFailed = "failed"
	// DeliveryDuplicate marks an acknowledgement for a sequence already closed.
	DeliveryDuplicate = "duplicate"
	// DeliveryUnknown marks an acknowledgement for a label the write side has
	// never seen.
	DeliveryUnknown = "unknown_label"
)

// DeliveryConfirmationHandler closes the loop.
//
// It consumes the `label-delivery` stream — which the MQTT ACK subscriber
// republishes from `usslp/+/+/+/sec/+/labels/+/ack` — records the confirmation
// on the label's stream, and observes the end-to-end latency that the
// three-second SLO is written against.
//
// That latency is the only number in the platform a retailer can verify by
// looking at a shelf, which is why it is measured from the envelope's
// RecordedAt — the moment USSLP took durable responsibility for the change —
// rather than from when this service happened to publish it. Measuring from our
// own publish would exclude every queueing delay upstream of us, which is
// exactly where a breach hides.
type DeliveryConfirmationHandler struct {
	deps Deps
}

// NewDeliveryConfirmationHandler builds the handler.
func NewDeliveryConfirmationHandler(deps Deps) (*DeliveryConfirmationHandler, error) {
	deps = deps.withDefaults()
	if deps.Repo == nil {
		return nil, fmt.Errorf("%w: Repo", ErrMissingDependency)
	}
	return &DeliveryConfirmationHandler{deps: deps}, nil
}

// HandleMessage adapts the handler to an event-bus subscription.
func (h *DeliveryConfirmationHandler) HandleMessage(ctx context.Context, m eventbus.Message) error {
	var env canon.Envelope
	if err := json.Unmarshal(m.Value, &env); err != nil {
		return fmt.Errorf("%w: label-delivery record at %s/%d/%d: %v",
			canon.ErrEnvelopeInvalid, m.Topic, m.Partition, m.Offset, err)
	}
	return h.HandleEnvelope(ctx, env)
}

// HandleEnvelope processes one delivery acknowledgement or failure.
func (h *DeliveryConfirmationHandler) HandleEnvelope(ctx context.Context, env canon.Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	ctx = obs.WithRemoteContext(ctx, obs.SpanContext{
		TraceID: env.TraceID, SpanID: env.SpanID, Sampled: true,
	})
	ctx, span := h.deps.Tracer.StartAlwaysSampled(ctx, "label.delivery.confirm")
	defer span.End()

	switch env.EventType {
	case canon.EvtLabelDelivered:
		var payload canon.LabelDelivered
		if err := env.Decode(&payload); err != nil {
			return err
		}
		span.SetAttr("label_id", string(payload.LabelID)).SetAttrInt("sequence", payload.Sequence)
		return h.confirm(ctx, env, payload)
	case canon.EvtLabelDeliveryFailed:
		var payload canon.LabelDeliveryFailed
		if err := env.Decode(&payload); err != nil {
			return err
		}
		span.SetAttr("label_id", string(payload.LabelID)).SetAttrInt("sequence", payload.Sequence)
		return h.fail(ctx, env, payload)
	}
	// Another producer's event on a shared stream. Committing the offset is
	// correct; erroring would dead-letter a perfectly valid record.
	return nil
}

func (h *DeliveryConfirmationHandler) confirm(ctx context.Context, env canon.Envelope, p canon.LabelDelivered) error {
	if p.LabelID == "" || p.Sequence <= 0 {
		return fmt.Errorf("%w: delivery for %q carries sequence %d",
			canon.ErrEnvelopeInvalid, p.LabelID, p.Sequence)
	}
	agg, err := h.deps.Repo.Load(ctx, p.LabelID)
	if err != nil {
		return fmt.Errorf("label: loading %s: %w", p.LabelID, err)
	}
	if !agg.Exists() {
		// An ACK for a label this replica's write side has never seen. It is
		// not a poison record — a store can be bridged before its provisioning
		// events have been consumed — but there is nothing to record against.
		h.deps.Metrics.DeliveryConfirmations.With(string(p.StoreID), DeliveryUnknown).Inc()
		return nil
	}

	deliveredAt := p.DeliveredAt
	if deliveredAt.IsZero() {
		deliveredAt = h.deps.Clock.Now()
	}
	latency := time.Duration(p.LatencyMS) * time.Millisecond
	if latency <= 0 {
		// The edge did not measure. Fall back to the envelope's RecordedAt,
		// which is the same reference point the edge would have used, so a
		// fleet with a mix of firmware versions still reports one comparable
		// number.
		latency = deliveredAt.Sub(referenceTime(env, agg))
	}
	if latency < 0 {
		// Clock skew between a store gateway and the cloud. Reporting a
		// negative latency would poison the histogram's sum; dropping the
		// observation and keeping the confirmation is the honest trade.
		latency = 0
	}

	events, err := agg.ConfirmDelivery(domain.Confirm{
		SECID: p.SECID, Sequence: p.Sequence, DeliveredAt: deliveredAt,
		LatencyMS: latency.Milliseconds(), MeshHops: p.MeshHops,
		RefreshMS: p.RefreshMS, Partial: p.Partial,
	})
	if errors.Is(err, domain.ErrStaleUpdate) {
		// Duplicate ACK: mesh delivery is at-least-once and this is routine.
		h.deps.Metrics.DeliveryConfirmations.With(string(agg.StoreID), DeliveryDuplicate).Inc()
		return nil
	}
	if err != nil {
		return err
	}
	if err := h.append(ctx, agg, events, env); err != nil {
		return err
	}

	h.deps.Metrics.E2ELatency.With(string(agg.TenantID), string(agg.StoreID)).Observe(latency.Seconds())
	h.deps.Metrics.DeliveryConfirmations.With(string(agg.StoreID), DeliveryConfirmed).Inc()
	h.deps.Metrics.PendingDelivery.With(string(agg.StoreID)).Dec()
	h.deps.Log.FromContext(ctx).Debug("delivery confirmed",
		"label_id", string(p.LabelID), "sequence", p.Sequence,
		"latency_ms", latency.Milliseconds(), "mesh_hops", p.MeshHops)
	return nil
}

func (h *DeliveryConfirmationHandler) fail(ctx context.Context, env canon.Envelope, p canon.LabelDeliveryFailed) error {
	if p.LabelID == "" || p.Sequence <= 0 {
		return fmt.Errorf("%w: delivery failure for %q carries sequence %d",
			canon.ErrEnvelopeInvalid, p.LabelID, p.Sequence)
	}
	agg, err := h.deps.Repo.Load(ctx, p.LabelID)
	if err != nil {
		return fmt.Errorf("label: loading %s: %w", p.LabelID, err)
	}
	if !agg.Exists() {
		h.deps.Metrics.DeliveryConfirmations.With(string(p.StoreID), DeliveryUnknown).Inc()
		return nil
	}
	events, err := agg.FailDelivery(p.SECID, p.Sequence, p.Reason, p.Attempts, h.deps.Clock.Now())
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	if err := h.append(ctx, agg, events, env); err != nil {
		return err
	}
	h.deps.Metrics.DeliveryConfirmations.With(string(agg.StoreID), DeliveryFailed).Inc()
	h.deps.Metrics.PendingDelivery.With(string(agg.StoreID)).Dec()
	h.deps.Log.FromContext(ctx).Warn("label update failed at the edge",
		"label_id", string(p.LabelID), "sequence", p.Sequence,
		"reason", p.Reason, "attempts", p.Attempts)
	return nil
}

// append persists delivery events.
//
// Delivery events are appended with the aggregate's expected version and one
// reload on conflict, because a price change landing on the same label at the
// same instant is entirely normal: the confirmation of update N and the
// authorisation of update N+1 race constantly on a busy shelf.
func (h *DeliveryConfirmationHandler) append(ctx context.Context, agg *domain.Label, events []domain.Event, env canon.Envelope) error {
	meta := ports.AppendMeta{
		TenantID: agg.TenantID, StoreID: agg.StoreID, Region: agg.Region,
		TraceID: env.TraceID, CorrelationID: env.CorrelationID,
		CausationID: env.EventID, Source: SourceName,
		IdempotencyKey: scopedIdempotencyKey(env, agg.ID, "delivery"),
	}
	var lastErr error
	for attempt := 1; attempt <= concurrencyAttempts; attempt++ {
		_, err := h.deps.Repo.Append(ctx, agg.ID, agg.Version, events, meta)
		if err == nil {
			for _, e := range events {
				agg.Apply(e)
				agg.Version++
			}
			if h.deps.State != nil {
				if perr := h.deps.State.Put(ctx, StateFromLabel(agg)); perr != nil {
					h.deps.Log.FromContext(ctx).Warn("updating label read model failed",
						"label_id", string(agg.ID), "error", perr)
				}
			}
			return nil
		}
		if !errors.Is(err, ports.ErrConcurrency) {
			return err
		}
		lastErr = err
		reloaded, lerr := h.deps.Repo.Load(ctx, agg.ID)
		if lerr != nil {
			return lerr
		}
		*agg = *reloaded
	}
	return lastErr
}

// referenceTime is the instant end-to-end latency is measured from: the moment
// USSLP took durable responsibility for the change. The ACK envelope's
// RecordedAt is inherited from the price envelope through the trace, and the
// aggregate's pending update is the fallback for an ACK that arrived without
// one.
func referenceTime(env canon.Envelope, agg *domain.Label) time.Time {
	if agg.Pending != nil && !agg.Pending.PublishedAt.IsZero() {
		return agg.Pending.PublishedAt
	}
	if !env.OccurredAt.IsZero() {
		return env.OccurredAt
	}
	return env.RecordedAt
}

// scopedIdempotencyKey derives an append key from an inbound envelope.
//
// It is scoped by label and by purpose because one envelope can legitimately
// produce more than one append against one aggregate — an assignment for a
// label whose provisioning has not been consumed yet provisions it and then
// assigns it — and the event store treats a batch whose keys have all been seen
// as a no-op. Two appends sharing a key would silently drop the second, leaving
// a provisioned label with no SKU that then quietly declines every price.
func scopedIdempotencyKey(env canon.Envelope, label canon.LabelID, purpose string) string {
	base := env.IdempotencyKey
	if base == "" {
		base = string(env.EventID)
	}
	if base == "" {
		return ""
	}
	return base + "#" + string(label) + "#" + purpose
}

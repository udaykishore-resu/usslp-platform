package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/label/ports"
	promodomain "github.com/usslp/usslp/platform/internal/promotion/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// PromotionActivation is the payload of `promotion.activated` and
// `promotion.expired` on the `promotion-events` stream.
//
// It mirrors the Promotion Service's own `ActivationEvent`, declared here rather
// than imported so that the Label Service depends on the promotion *domain* —
// the shared rule vocabulary, which imports nothing but canon — and not on the
// promotion *service*, whose store, HTTP surface and scheduler are none of this
// service's business. This is the same consumer-defined-contract shape as
// LabelAssignment above, and for the same reason.
//
// The rule travels whole, by the producer's explicit design, so that a national
// activation does not become two thousand stores' worth of simultaneous lookups
// against one service.
type PromotionActivation struct {
	// PromotionID and TenantID identify the promotion.
	PromotionID canon.PromotionID `json:"promotion_id"`
	TenantID    canon.TenantID    `json:"tenant_id"`
	// Rule is the whole promotion document, including the conditions that
	// decide which shelves it touches and the parameters that price them.
	Rule promodomain.Rule `json:"rule"`
	// State is the lifecycle state this transition moved the promotion into.
	State promodomain.State `json:"state"`
	// Windows are the resolved absolute activation windows per store zone.
	Windows map[string]promodomain.StoreWindow `json:"windows,omitempty"`
	// EffectiveAt is when the transition took effect. It is the source clock
	// every resulting price change is stamped with.
	EffectiveAt time.Time `json:"effective_at"`
	// Reason explains a cancellation.
	Reason string `json:"reason,omitempty"`
}

// Promotion fan-out outcome labels for usslp_promotion_fanouts_total.
const (
	// PromotionApplied marks an activation that reached the glass.
	PromotionApplied = "applied"
	// PromotionReverted marks an expiry that restored base prices.
	PromotionReverted = "reverted"
	// PromotionSkipped marks a transition that correctly touched nothing.
	PromotionSkipped = "skipped"
	// PromotionUnresolvable marks a rule the shelf tier cannot evaluate.
	PromotionUnresolvable = "unresolvable"
	// PromotionFailed marks an infrastructure failure.
	PromotionFailed = "failed"
)

// Reasons a promotion is not fanned out. Like the domain's rejection reasons
// these are stable identifiers: they reach the audit stream and alert rules.
const (
	// ReasonNotShelfPriceable: the mechanic cannot produce a definite shelf
	// price (THRESHOLD), or the rule is restricted to loyalty segments.
	ReasonNotShelfPriceable = "not_shelf_priceable"
	// ReasonUnobservableCondition: the rule is scoped by an attribute the shelf
	// tier does not hold.
	ReasonUnobservableCondition = "unobservable_condition"
	// ReasonActivationStale: the transition is older than the tenant's
	// effective-at grace.
	ReasonActivationStale = "activation_stale"
	// ReasonNoBasePrice: an expiry found labels with no everyday price to
	// revert to.
	ReasonNoBasePrice = "no_base_price"
)

// PromotionHandler turns a promotion lifecycle event into shelf prices.
//
// # Why this consumer exists
//
// INTERFACE-CONTRACTS §2 lists `promotion-events` as consumed by the Label
// Service, and the Promotion Service's package comment says the Label Service
// turns those into shelf updates. This is the component that makes both true:
// without it a promotion activates, the fact is published, and no shelf
// changes.
//
// # What it decides and what it does not
//
// It decides *which labels a named promotion touches* and *what each should now
// cost under that one rule*, using the promotion domain's own compiled matcher
// and pricing functions rather than a second implementation of either.
//
// It deliberately does not arbitrate between overlapping promotions. Priority,
// stacking and exclusive groups are resolved by promodomain.Resolve against the
// whole active set, and only the Promotion Service holds that set. A partial
// arbiter here would be a second pricing engine that eventually disagrees with
// the first, which is the failure this split exists to prevent. The consequence
// is explicit and predictable: where two promotions overlap, the most recently
// activated one is what the shelf shows, because it takes the higher per-label
// sequence. Making the shelf honour full conflict resolution needs the
// resolved outcome on the event, not more logic here.
//
// # Everything goes through the batch path
//
// Nothing in this file publishes to a device or appends an event. The resolved
// set becomes a BatchRequest and goes through BatchUpdater, so a promotion
// fan-out gets exactly the same per-tenant rate limiting, attestation,
// sequencing, guard rails and per-label failure reporting as a store-wide
// repricing — because it *is* a store-wide repricing, arrived at differently.
type PromotionHandler struct {
	deps  Deps
	batch *BatchUpdater
	// guard de-duplicates deliveries at ingress. The Promotion Service keys its
	// events on the exact transition (tenant:promo:state:instant), so a
	// redelivered activation carries the same key and is recognised as the same
	// fact rather than fanned out twice.
	guard *idem.Guard
}

// NewPromotionHandler builds the handler. The guard may be nil, in which case
// de-duplication falls back to the per-label idempotency keys alone — correct,
// but it reloads every affected aggregate to discover it.
func NewPromotionHandler(deps Deps, batch *BatchUpdater, guard *idem.Guard) (*PromotionHandler, error) {
	deps = deps.withDefaults()
	if batch == nil {
		return nil, fmt.Errorf("%w: BatchUpdater", ErrMissingDependency)
	}
	if deps.State == nil {
		return nil, fmt.Errorf("%w: StateStore", ErrMissingDependency)
	}
	return &PromotionHandler{deps: deps, batch: batch, guard: guard}, nil
}

// PromotionReport is what one lifecycle transition did.
type PromotionReport struct {
	// PromotionID and TenantID identify the promotion.
	PromotionID canon.PromotionID `json:"promotion_id"`
	TenantID    canon.TenantID    `json:"tenant_id"`
	// Outcome is one of the Promotion* constants.
	Outcome string `json:"outcome"`
	// Reason names why a transition was skipped or refused.
	Reason string `json:"reason,omitempty"`
	// Detail explains it in words.
	Detail string `json:"detail,omitempty"`
	// Stores are the stores the fan-out touched, ordered.
	Stores []canon.StoreID `json:"stores,omitempty"`
	// Batch is the per-label report from the fan-out.
	Batch BatchReport `json:"batch"`
}

// HandleMessage adapts the handler to an event-bus subscription.
func (h *PromotionHandler) HandleMessage(ctx context.Context, m eventbus.Message) error {
	var env canon.Envelope
	if err := json.Unmarshal(m.Value, &env); err != nil {
		return fmt.Errorf("%w: promotion-events record at %s/%d/%d: %v",
			canon.ErrEnvelopeInvalid, m.Topic, m.Partition, m.Offset, err)
	}
	_, err := h.HandleEnvelope(ctx, env)
	return err
}

// HandleEnvelope processes one promotion lifecycle transition.
//
// The returned report is for callers driving the handler directly — the tests,
// and an operator endpoint that wants to see what an activation did. The stream
// path discards it and relies on the metrics and the audit record.
func (h *PromotionHandler) HandleEnvelope(ctx context.Context, env canon.Envelope) (PromotionReport, error) {
	if err := env.Validate(); err != nil {
		return PromotionReport{}, err
	}
	switch env.EventType {
	case canon.EvtPromotionActivated, canon.EvtPromotionExpired:
	default:
		// Another producer's event on a shared stream. Committing the offset is
		// correct; erroring would dead-letter a valid record.
		return PromotionReport{Outcome: PromotionSkipped, Reason: "unhandled_event_type"}, nil
	}

	ctx = obs.WithRemoteContext(ctx, obs.SpanContext{
		TraceID: env.TraceID, SpanID: env.SpanID, Sampled: true,
	})
	ctx, span := h.deps.Tracer.StartAlwaysSampled(ctx, "label.promotion.fanout")
	defer span.End()
	span.SetAttr("tenant", string(env.TenantID)).SetAttr("event_type", env.EventType)

	key := promotionDedupeKey(env)
	if h.guard != nil && key != "" {
		firstSeen, _, err := h.guard.Check(ctx, key)
		if err != nil {
			return PromotionReport{}, fmt.Errorf("label: idempotency check for %s: %w", env.EventID, err)
		}
		if !firstSeen {
			span.AddEvent("deduplicated")
			h.deps.Metrics.PromotionFanouts.With(env.EventType, PromotionSkipped).Inc()
			return PromotionReport{Outcome: PromotionSkipped, Reason: "duplicate_record"}, nil
		}
	}

	report, err := h.fanOut(ctx, env)
	if err != nil {
		if h.guard != nil && key != "" {
			// Release so the producer's retry is a first delivery. A fan-out
			// that claimed the key and then failed would otherwise suppress
			// every retry for the whole window, and the promotion would simply
			// never reach the shelves.
			if rerr := h.guard.Release(ctx, key); rerr != nil {
				h.deps.Log.FromContext(ctx).Error("releasing promotion idempotency key failed",
					"key", key, "error", rerr)
			}
		}
		h.deps.Metrics.PromotionFanouts.With(env.EventType, PromotionFailed).Inc()
		span.Fail(err)
		return report, err
	}
	if h.guard != nil && key != "" {
		body, _ := json.Marshal(report)
		if rerr := h.guard.Record(ctx, key, body, 0); rerr != nil {
			h.deps.Log.FromContext(ctx).Warn("recording promotion idempotency result failed",
				"key", key, "error", rerr)
		}
	}
	h.deps.Metrics.PromotionFanouts.With(env.EventType, report.Outcome).Inc()
	h.deps.Metrics.PromotionLabels.With(env.EventType).Observe(float64(report.Batch.Resolved))
	span.SetAttrInt("labels", int64(report.Batch.Resolved)).
		SetAttrInt("applied", int64(report.Batch.Applied)).
		SetAttr("outcome", report.Outcome)
	return report, nil
}

func promotionDedupeKey(env canon.Envelope) string {
	if env.IdempotencyKey != "" {
		return idem.Key("label-service", "promotion", env.IdempotencyKey)
	}
	if env.EventID != "" {
		return idem.Key("label-service", "promotion", string(env.EventID))
	}
	return ""
}

func (h *PromotionHandler) fanOut(ctx context.Context, env canon.Envelope) (PromotionReport, error) {
	var act PromotionActivation
	if err := env.Decode(&act); err != nil {
		return PromotionReport{}, err
	}
	if act.TenantID == "" {
		act.TenantID = env.TenantID
	}
	if act.PromotionID == "" {
		act.PromotionID = act.Rule.ID
	}
	if act.PromotionID == "" || act.TenantID == "" {
		return PromotionReport{}, fmt.Errorf("%w: promotion event names neither a promotion nor a tenant",
			canon.ErrEnvelopeInvalid)
	}
	report := PromotionReport{PromotionID: act.PromotionID, TenantID: act.TenantID}

	at := act.EffectiveAt
	if at.IsZero() {
		at = env.OccurredAt
	}
	if at.IsZero() {
		at = h.deps.Clock.Now()
	}
	now := h.deps.Clock.Now()

	// A transition older than the tenant's grace is refused once, here, rather
	// than per label. The aggregate would reject each of forty thousand labels
	// with effective_at_too_old and write forty thousand rejection events for
	// one stale record; one refusal with a named reason is the same protection
	// and a readable audit trail.
	policy := h.deps.Policies.For(act.TenantID)
	if at.Before(now.Add(-policy.EffectiveGrace)) {
		report.Outcome, report.Reason = PromotionSkipped, ReasonActivationStale
		report.Detail = fmt.Sprintf("transition at %s is more than %s in the past",
			at.UTC().Format(time.RFC3339), policy.EffectiveGrace)
		h.deps.Log.FromContext(ctx).Warn("promotion transition too old to apply",
			"promotion", string(act.PromotionID), "effective_at", at, "grace", policy.EffectiveGrace)
		return report, nil
	}

	if env.EventType == canon.EvtPromotionExpired {
		return h.revert(ctx, env, act, report, at)
	}
	return h.activate(ctx, env, act, report, at)
}

// activate reprices every label the rule matches.
func (h *PromotionHandler) activate(ctx context.Context, env canon.Envelope, act PromotionActivation, report PromotionReport, at time.Time) (PromotionReport, error) {
	rule := act.Rule
	if rule.ID == "" {
		rule.ID = act.PromotionID
	}
	if blocker := shelfBlocker(rule); blocker != "" {
		report.Outcome, report.Reason, report.Detail = PromotionUnresolvable, ReasonUnobservableCondition, blocker
		h.deps.Log.FromContext(ctx).Warn("promotion cannot be resolved on the shelf tier",
			"promotion", string(act.PromotionID), "detail", blocker)
		return report, nil
	}
	if !rule.Type.ShelfPriceable() {
		// A THRESHOLD promotion depends on the whole basket, which the shelf
		// cannot know. The label keeps showing the undiscounted price, which is
		// the only display that will match the till.
		report.Outcome, report.Reason = PromotionSkipped, ReasonNotShelfPriceable
		report.Detail = fmt.Sprintf("%s is priced at the till, not on the shelf", rule.Type)
		return report, nil
	}
	if promodomain.Compile(rule).Segmented() {
		// A segmented promotion applies to some shoppers and not others. The
		// shelf cannot know who is standing in front of it, so repricing it
		// would show a price the till will not charge most customers.
		report.Outcome, report.Reason = PromotionSkipped, ReasonNotShelfPriceable
		report.Detail = "rule is restricted to customer segments, which a shelf cannot evaluate"
		return report, nil
	}

	matcher := promodomain.Compile(rule)
	stores, err := h.candidateStores(ctx, act.TenantID, rule)
	if err != nil {
		return report, err
	}

	var items []BatchItem
	touched := map[canon.StoreID]bool{}
	for _, store := range stores {
		rows, err := h.deps.State.ListByStore(ctx, act.TenantID, store)
		if err != nil {
			return report, fmt.Errorf("label: listing %s/%s: %w", act.TenantID, store, err)
		}
		for _, row := range rows {
			item, ok := h.itemFor(matcher, rule, row, at)
			if !ok {
				continue
			}
			items = append(items, item)
			touched[store] = true
		}
	}
	report.Stores = sortedStores(touched)
	if len(items) == 0 {
		report.Outcome, report.Reason = PromotionSkipped, "no_matching_labels"
		return report, nil
	}
	batch, err := h.run(ctx, env, act, items, at)
	report.Batch = batch
	if err != nil {
		return report, err
	}
	report.Outcome = PromotionApplied
	h.deps.Log.FromContext(ctx).Info("promotion fanned out",
		"promotion", string(act.PromotionID), "stores", len(report.Stores),
		"resolved", batch.Resolved, "applied", batch.Applied,
		"rejected", batch.Rejected, "failed", batch.Failed)
	return report, nil
}

// itemFor decides whether one label is in the promotion and what it should cost.
func (h *PromotionHandler) itemFor(matcher *promodomain.Matcher, rule promodomain.Rule, row ports.LabelState, at time.Time) (BatchItem, bool) {
	if row.SKU == "" || row.State == "retired" {
		return BatchItem{}, false
	}
	base := row.BasePrice
	if base.Amount == 0 && base.Currency == "" {
		// A label that has never held an everyday price cannot be discounted
		// from one. Skipping is the safe direction: discounting from a price
		// that is itself promotional compounds two promotions.
		return BatchItem{}, false
	}
	product := promodomain.Product{
		SKU: row.SKU, StoreID: row.StoreID,
		Category: row.Category, Brand: row.Brand,
		BasePriceMinor: base.Amount, Currency: base.Currency,
	}
	if !matcher.Matches(product) {
		return BatchItem{}, false
	}
	priced, err := promodomain.Apply(rule, product)
	if err != nil {
		// A currency mismatch between rule and store, or a mechanic the pricing
		// function refuses. Skipping one label is right; the rest of the estate
		// is unaffected.
		return BatchItem{}, false
	}
	if !priced.ShelfPriced || priced.PriceMinor == base.Amount {
		// A mechanic that leaves the shelf price alone — a bundle, a multi-buy —
		// changes the badge, not the price. Republishing an identical price
		// would burn a full refresh across the store to change nothing.
		return BatchItem{}, false
	}
	was := base
	return BatchItem{
		// Addressed to the label, not to the (store, SKU) pair. The batch
		// resolver would otherwise expand each item across every facing of the
		// product, and since this loop already emits one item per facing the
		// fan-out would be squared. Matching is per label anyway: two facings
		// of one product can disagree about their recorded category or their
		// base price if one of them missed a price change.
		StoreID: row.StoreID, SKU: row.SKU, LabelID: row.LabelID,
		Price:             canon.NewMoney(priced.PriceMinor, base.Currency),
		WasPrice:          &was,
		EffectiveAt:       at,
		PromotionID:       rule.ID,
		PromotionPriority: rule.Priority,
		Reason:            promotionReason(rule),
		Attributes:        promotionAttributes(rule),
		InitiatedBy:       "promotion:" + string(rule.ID),
		IdempotencyKey:    promotionItemKey(canon.EvtPromotionActivated, rule.ID, at, row.LabelID),
	}, true
}

// revert restores the everyday price on every label showing an ending
// promotion.
//
// Reversion is driven by "which labels are showing this promotion" rather than
// by re-evaluating the rule's conditions. That is deliberate: a label whose
// category or price changed while the promotion ran would no longer match, and
// re-matching would leave it discounted forever. What went on because of a
// promotion comes off when it ends, whatever has happened since.
func (h *PromotionHandler) revert(ctx context.Context, env canon.Envelope, act PromotionActivation, report PromotionReport, at time.Time) (PromotionReport, error) {
	stores, err := h.candidateStores(ctx, act.TenantID, act.Rule)
	if err != nil {
		return report, err
	}
	var items []BatchItem
	touched := map[canon.StoreID]bool{}
	missingBase := 0
	for _, store := range stores {
		rows, err := h.deps.State.ListByStore(ctx, act.TenantID, store)
		if err != nil {
			return report, fmt.Errorf("label: listing %s/%s: %w", act.TenantID, store, err)
		}
		for _, row := range rows {
			if row.PromotionID != act.PromotionID || row.SKU == "" {
				continue
			}
			if row.BasePrice.Amount == 0 && row.BasePrice.Currency == "" {
				missingBase++
				continue
			}
			if row.BasePrice.Amount == row.Price.Amount && row.BasePrice.Currency == row.Price.Currency {
				// Already at the everyday price; only the promotion marker
				// would change, and that is not worth a refresh.
				continue
			}
			items = append(items, BatchItem{
				StoreID: row.StoreID, SKU: row.SKU, LabelID: row.LabelID,
				Price:       row.BasePrice,
				EffectiveAt: at,
				// No PromotionID: this is the everyday price returning, and the
				// absence of the marker is what lets a later expiry of the same
				// promotion find nothing left to do.
				Reason:         "promotion_ended:" + string(act.PromotionID),
				InitiatedBy:    "promotion:" + string(act.PromotionID),
				IdempotencyKey: promotionItemKey(canon.EvtPromotionExpired, act.PromotionID, at, row.LabelID),
			})
			touched[row.StoreID] = true
		}
	}
	report.Stores = sortedStores(touched)
	if missingBase > 0 {
		report.Reason, report.Detail = ReasonNoBasePrice,
			fmt.Sprintf("%d labels had no everyday price to revert to", missingBase)
		h.deps.Log.FromContext(ctx).Warn("promotion expiry left labels discounted",
			"promotion", string(act.PromotionID), "labels", missingBase)
	}
	if len(items) == 0 {
		if report.Outcome == "" {
			report.Outcome = PromotionSkipped
		}
		if report.Reason == "" {
			report.Reason = "no_matching_labels"
		}
		return report, nil
	}
	batch, err := h.run(ctx, env, act, items, at)
	report.Batch = batch
	if err != nil {
		return report, err
	}
	report.Outcome = PromotionReverted
	h.deps.Log.FromContext(ctx).Info("promotion reverted",
		"promotion", string(act.PromotionID), "stores", len(report.Stores),
		"resolved", batch.Resolved, "applied", batch.Applied)
	return report, nil
}

// run hands the resolved set to the batch pipeline.
func (h *PromotionHandler) run(ctx context.Context, env canon.Envelope, act PromotionActivation, items []BatchItem, at time.Time) (BatchReport, error) {
	return h.batch.BatchUpdatePrices(ctx, BatchRequest{
		TenantID: act.TenantID,
		Region:   env.Region,
		Items:    items,
		// The transition instant, not wall-clock time at fan-out: see
		// BatchRequest.OccurredAt for why that is what decides a race with a
		// direct price change.
		OccurredAt:    at,
		Cause:         env,
		CorrelationID: env.CorrelationID,
		InitiatedBy:   "promotion:" + string(act.PromotionID),
	})
}

// candidateStores is the set of stores a rule could touch.
//
// A rule naming stores is trusted to name them all; one naming none is national
// and is resolved against every store the tenant has labels in. Reading that
// from the local read model rather than from the Device Registry keeps the
// promotion path on the same local-data-only footing as the price path, so a
// registry outage cannot stop a promotion going live.
func (h *PromotionHandler) candidateStores(ctx context.Context, tenant canon.TenantID, rule promodomain.Rule) ([]canon.StoreID, error) {
	if len(rule.Conditions.Stores) > 0 {
		out := append([]canon.StoreID(nil), rule.Conditions.Stores...)
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out, nil
	}
	stores, err := h.deps.State.Stores(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("label: listing stores for %s: %w", tenant, err)
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i] < stores[j] })
	return stores, nil
}

// shelfBlocker names the first condition a rule carries that the shelf tier
// cannot observe, or "" when the rule is fully evaluable here.
//
// # Why refusing beats guessing
//
// The compiled matcher is a conjunction, so an unobservable attribute has only
// two possible readings and both are wrong. Passing an empty value makes the
// test fail and the promotion silently applies to nothing, which a
// merchandising team discovers from a sales report a week later. Skipping the
// test makes it apply to everything, which puts a discount on the alcohol,
// tobacco and infant-formula lines the exclusion list exists to protect — a
// regulatory incident rather than a missed opportunity.
//
// The third option is to refuse loudly, naming the attribute, so an operator
// can rescope the rule onto conditions the shelf can see or the platform can
// grow the read model to carry the one that is missing. Category and brand are
// evaluable because the price feed carries them; inventory, shelf life and
// store clusters are not, because nothing on the shelf path has ever been told
// them.
func shelfBlocker(rule promodomain.Rule) string {
	c := rule.Conditions
	switch {
	case len(c.StoreGroups) > 0:
		return "conditions.store_groups: the shelf tier does not know a store's clusters"
	case c.MinInventory > 0:
		return "conditions.min_inventory: the shelf tier does not know stock on hand"
	case c.MaxDaysToExpiry != nil:
		return "conditions.max_days_to_expiry: the shelf tier does not know shelf life"
	}
	return ""
}

func promotionReason(rule promodomain.Rule) string {
	if strings.EqualFold(rule.Funding, "supplier") {
		return "promotion_supplier:" + string(rule.ID)
	}
	return "promotion:" + string(rule.ID)
}

// promotionAttributes carries the rule's display block onto the price change,
// where DecideRender turns it into the template, badge and LED. Passing the
// authored badge through verbatim matters: "2 FOR £3" is a legal claim the
// retailer made, and the platform is not entitled to replace it with "SALE".
func promotionAttributes(rule promodomain.Rule) map[string]string {
	attrs := map[string]string{
		// Always explicit, never omitted: a rule that says "do not show the
		// original price" must be able to turn off a strike-through the
		// derivation would otherwise add, and an omitted key would read as
		// "no opinion" rather than as "no".
		"show_was": strconv.FormatBool(rule.Display.ShowOriginalPrice),
	}
	if rule.Display.Badge != "" {
		attrs["badge"] = rule.Display.Badge
	}
	if t := rule.Display.Template; t != "" {
		attrs["template"] = t
	}
	if c := rule.Display.LEDColor; c != "" {
		attrs["led_color"] = c
	}
	if a := rule.Display.Animation; a != "" {
		attrs["animation"] = a
	}
	return attrs
}

// promotionItemKey is the per-label append key.
//
// It names the transition, not the delivery: the same activation redelivered
// produces the same key on the same label, so the aggregate's append is a no-op
// and no second refresh is driven. It includes the transition instant so that a
// genuine re-activation later is a different fact, and the label id because one
// promotion event fans out to many aggregates, each applying the key to its own
// stream.
func promotionItemKey(eventType string, id canon.PromotionID, at time.Time, label canon.LabelID) string {
	return fmt.Sprintf("%s:%s:%d:%s", eventType, id, at.UTC().UnixNano(), label)
}

func sortedStores(set map[canon.StoreID]bool) []canon.StoreID {
	if len(set) == 0 {
		return nil
	}
	out := make([]canon.StoreID, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

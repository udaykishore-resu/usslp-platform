package label

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	promodomain "github.com/usslp/usslp/platform/internal/promotion/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
)

// seedPricedLabel provisions a label, assigns it a SKU, and gives it an
// everyday price carrying the merchandising attributes a category-scoped
// promotion needs. It is the state every promotion test starts from, because a
// promotion discounts from a base price and there is no base price until an
// ordinary price change has been through.
func seedPricedLabel(t *testing.T, h *harness, id canon.LabelID, sku canon.SKU, amount int64, category string) {
	t.Helper()
	h.provisionLabel(id, testSEC, sku)
	env := h.priceEnvelope(sku, amount, "seed-"+string(id))
	var req canon.PriceChangeRequested
	if err := env.Decode(&req); err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	req.Attributes = map[string]string{"category": category, "brand": "own-label"}
	seeded, err := env.WithPayload(req)
	if err != nil {
		t.Fatalf("re-encode seed: %v", err)
	}
	if err := h.svc.PriceHandler().HandleEnvelope(context.Background(), seeded); err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
}

// promotionRule builds a percentage-off rule scoped to a category.
func promotionRule(id canon.PromotionID, category string, percentOff float64) promodomain.Rule {
	return promodomain.Rule{
		ID: id, TenantID: testTenant, Name: "test promotion",
		Type: promodomain.TypePercentageOff, Priority: 100,
		Params:     promodomain.Params{PercentOff: percentOff},
		Conditions: promodomain.Conditions{Categories: []string{category}},
		Display: promodomain.Display{
			Badge: "SALE", LEDColor: "GREEN", ShowOriginalPrice: true,
		},
		Schedule: promodomain.Schedule{
			StartLocal: "2026-01-01T00:00", EndLocal: "2027-01-01T00:00",
		},
	}
}

// promotionEnvelope builds the envelope the Promotion Service publishes,
// including the idempotency key it derives from the exact transition.
func promotionEnvelope(t *testing.T, h *harness, eventType string, rule promodomain.Rule, state promodomain.State, at time.Time) canon.Envelope {
	t.Helper()
	env, err := canon.NewEnvelope(eventType, "promotion", string(rule.ID), testTenant,
		app.PromotionActivation{
			PromotionID: rule.ID, TenantID: testTenant, Rule: rule,
			State: state, EffectiveAt: at,
		})
	if err != nil {
		t.Fatalf("promotion envelope: %v", err)
	}
	env.Region = testRegion
	env.Source = "promotion-service"
	env.TraceID = canon.NewTraceID()
	env.SpanID = canon.NewSpanID()
	env.CorrelationID = canon.NewCorrelationID()
	env.IdempotencyKey = string(testTenant) + ":" + string(rule.ID) + ":" +
		string(state) + ":" + at.UTC().Format(time.RFC3339Nano)
	return env
}

// TestPromotionActivationReachesExactlyTheMatchingLabels is the gap this
// consumer closes: a promotion activates and the right shelves change.
func TestPromotionActivationReachesExactlyTheMatchingLabels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Two dairy facings, one bakery line, and a dairy line the rule excludes.
	seedPricedLabel(t, h, "lbl-milk-a", "sku-milk", 300, "dairy")
	seedPricedLabel(t, h, "lbl-milk-b", "sku-milk", 300, "dairy")
	seedPricedLabel(t, h, "lbl-bread", "sku-bread", 250, "bakery")
	seedPricedLabel(t, h, "lbl-formula", "sku-formula", 1200, "dairy")
	h.waitForMessages(canon.LeafPrice, 4, 5*time.Second)
	h.resetCaptured()

	rule := promotionRule("promo-dairy", "dairy", 20)
	// Infant formula is the reason the exclusion list exists.
	rule.Conditions.ExcludeSKUs = []canon.SKU{"sku-formula"}

	at := h.clock.Now()
	report, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, at))
	if err != nil {
		t.Fatalf("activation: %v", err)
	}
	if report.Outcome != app.PromotionApplied {
		t.Fatalf("outcome = %s (%s: %s)", report.Outcome, report.Reason, report.Detail)
	}
	if report.Batch.Resolved != 2 || report.Batch.Applied != 2 {
		t.Fatalf("resolved=%d applied=%d, want 2/2 (the two dairy facings)",
			report.Batch.Resolved, report.Batch.Applied)
	}

	msgs := h.waitForMessages(canon.LeafPrice, 2, 5*time.Second)
	if len(msgs) != 2 {
		t.Fatalf("published to %d labels, want exactly the two dairy facings", len(msgs))
	}
	seen := map[canon.LabelID]canon.PriceUpdated{}
	for _, m := range msgs {
		env, update := h.decodeUpdate(m)
		seen[update.LabelID] = update
		h.verifyAttestation(update)
		// 20% off 3.00 is 2.40, computed by the promotion domain's own pricing
		// function in integer minor units.
		if update.Price.Amount != 240 {
			t.Errorf("label %s priced at %d, want 240", update.LabelID, update.Price.Amount)
		}
		if update.PromotionID != "promo-dairy" {
			t.Errorf("label %s carries promotion %q", update.LabelID, update.PromotionID)
		}
		if update.WasPrice == nil || update.WasPrice.Amount != 300 {
			t.Errorf("label %s has no was-price for the saving claim: %v", update.LabelID, update.WasPrice)
		}
		// The authored display block wins over the platform's own derivation:
		// the merchandiser briefed a green LED and a SALE badge, and the aisle
		// walk that verifies the promotion is looking for exactly that.
		if update.Render.Template != domain.TemplatePromo || update.Render.Badge != "SALE" {
			t.Errorf("label %s render = %+v, want the authored promo display", update.LabelID, update.Render)
		}
		if update.Render.LEDColor != "GREEN" {
			t.Errorf("label %s LED = %q, want the authored GREEN", update.LabelID, update.Render.LEDColor)
		}
		if !update.Render.ShowWas {
			t.Errorf("label %s does not strike through the original price, which the rule requires", update.LabelID)
		}
		if update.Render.PartialRefresh {
			t.Errorf("a promotion changes price, badge and strike-through at once; it needs a full waveform")
		}
		// Trace context from the activation must survive to the shelf, so one
		// promotion is one trace across every label it touched.
		if env.TraceID == "" {
			t.Errorf("label %s update lost the activation's trace context", update.LabelID)
		}
	}
	if _, ok := seen["lbl-milk-a"]; !ok {
		t.Errorf("lbl-milk-a was not repriced")
	}
	if _, ok := seen["lbl-milk-b"]; !ok {
		t.Errorf("lbl-milk-b was not repriced")
	}
	if _, ok := seen["lbl-bread"]; ok {
		t.Errorf("a bakery label was caught by a dairy promotion")
	}
	if _, ok := seen["lbl-formula"]; ok {
		t.Errorf("an excluded SKU was discounted; the exclusion list is a regulatory control")
	}
}

// TestPromotionExpiryRevertsToTheEverydayPrice covers the other half: what went
// on because of a promotion comes off when it ends.
func TestPromotionExpiryRevertsToTheEverydayPrice(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedPricedLabel(t, h, "lbl-milk-a", "sku-milk", 300, "dairy")
	seedPricedLabel(t, h, "lbl-bread", "sku-bread", 250, "bakery")
	h.waitForMessages(canon.LeafPrice, 2, 5*time.Second)
	h.resetCaptured()

	rule := promotionRule("promo-dairy", "dairy", 20)
	at := h.clock.Now()
	if _, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, at)); err != nil {
		t.Fatalf("activation: %v", err)
	}
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	h.resetCaptured()

	row, err := h.svc.state.Get(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("read model: %v", err)
	}
	if row.Price.Amount != 240 || row.BasePrice.Amount != 300 {
		t.Fatalf("after activation price=%d base=%d, want 240/300", row.Price.Amount, row.BasePrice.Amount)
	}

	h.clock.Advance(time.Hour)
	expiry := h.clock.Now()
	report, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionExpired, rule, promodomain.StateExpired, expiry))
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	if report.Outcome != app.PromotionReverted {
		t.Fatalf("outcome = %s (%s: %s)", report.Outcome, report.Reason, report.Detail)
	}
	if report.Batch.Applied != 1 {
		t.Fatalf("reverted %d labels, want 1", report.Batch.Applied)
	}

	msgs := h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	if len(msgs) != 1 {
		t.Fatalf("%d publishes on expiry, want only the discounted label", len(msgs))
	}
	_, update := h.decodeUpdate(msgs[0])
	h.verifyAttestation(update)
	if update.LabelID != "lbl-milk-a" {
		t.Fatalf("reverted %s, want lbl-milk-a", update.LabelID)
	}
	if update.Price.Amount != 300 {
		t.Fatalf("reverted to %d, want the everyday 300", update.Price.Amount)
	}
	if update.PromotionID != "" {
		t.Fatalf("the reverted price still carries promotion %q", update.PromotionID)
	}
	// The sequence must advance so the label accepts the reversion at all.
	if update.Sequence <= 2 {
		t.Fatalf("reversion sequence = %d, must exceed the promotional one", update.Sequence)
	}

	// A second expiry finds nothing left to revert.
	h.resetCaptured()
	h.clock.Advance(time.Hour)
	again, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionExpired, rule, promodomain.StateExpired, h.clock.Now()))
	if err != nil {
		t.Fatalf("second expiry: %v", err)
	}
	if again.Batch.Applied != 0 {
		t.Fatalf("a second expiry repriced %d labels", again.Batch.Applied)
	}
}

// TestPromotionRedeliveryDoesNotDoubleApply covers the at-least-once guarantee:
// a redelivered activation, and a re-activation of a promotion already on the
// glass, must both leave the shelves alone.
func TestPromotionRedeliveryDoesNotDoubleApply(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedPricedLabel(t, h, "lbl-milk-a", "sku-milk", 300, "dairy")
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	h.resetCaptured()

	rule := promotionRule("promo-dairy", "dairy", 20)
	at := h.clock.Now()
	env := promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, at)

	if _, err := h.svc.PromotionHandler().HandleEnvelope(ctx, env); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)

	// The same record again, as a Kafka redelivery presents it.
	report, err := h.svc.PromotionHandler().HandleEnvelope(ctx, env)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if report.Outcome != app.PromotionSkipped {
		t.Fatalf("redelivery outcome = %s, want skipped", report.Outcome)
	}

	// A fresh envelope carrying the same transition — a producer retry after a
	// partial failure — must also be recognised.
	retry := env
	retry.EventID = canon.NewEventID()
	if _, err := h.svc.PromotionHandler().HandleEnvelope(ctx, retry); err != nil {
		t.Fatalf("producer retry: %v", err)
	}

	// And a genuine re-activation at a later instant, which is what an operator
	// re-enabling a running promotion produces. Nothing on the glass changes,
	// so nothing should be published.
	h.clock.Advance(time.Hour)
	reactivated, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, h.clock.Now()))
	if err != nil {
		t.Fatalf("re-activation: %v", err)
	}
	if reactivated.Batch.Applied != 0 {
		t.Fatalf("re-activating a running promotion repriced %d labels", reactivated.Batch.Applied)
	}

	time.Sleep(200 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 1 {
		t.Fatalf("%d publishes for one logical activation, want 1", len(msgs))
	}
	agg, err := h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Sequence != 2 {
		t.Fatalf("sequence = %d after four deliveries of one activation, want 2", agg.Sequence)
	}
}

// TestPromotionGuardrailRejectsPerLabel covers the invariant that a promotion
// is not a licence to bypass the guard rails: a fat-fingered fixed price is
// refused label by label, with the existing rejection event, and the rest of
// the estate is unaffected.
func TestPromotionGuardrailRejectsPerLabel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// A £2.49 line and a £249 line in the same promotion. A fixed price of
	// £2.49 is an ordinary markdown for the second and a hundredfold drop for
	// the first — exactly the shape of a decimal point lost in a feed.
	seedPricedLabel(t, h, "lbl-cheap", "sku-cheap", 249, "dairy")
	seedPricedLabel(t, h, "lbl-tv", "sku-tv", 24900, "dairy")
	h.waitForMessages(canon.LeafPrice, 2, 5*time.Second)
	h.resetCaptured()

	rule := promotionRule("promo-fatfinger", "dairy", 20)
	rule.Type = promodomain.TypeFixedPrice
	rule.Params = promodomain.Params{FixedPriceMinor: 199, Currency: "USD"}

	at := h.clock.Now()
	report, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, at))
	if err != nil {
		t.Fatalf("activation: %v", err)
	}
	if report.Batch.Resolved != 2 {
		t.Fatalf("resolved = %d, want 2", report.Batch.Resolved)
	}
	if report.Batch.Applied != 1 || report.Batch.Rejected != 1 {
		t.Fatalf("applied=%d rejected=%d, want 1/1", report.Batch.Applied, report.Batch.Rejected)
	}
	if report.Batch.Failed != 0 {
		t.Fatalf("a guard-rail rejection must not be reported as a failure: %+v", report.Batch)
	}
	byLabel := map[canon.LabelID]app.PriceResult{}
	for _, r := range report.Batch.Results {
		byLabel[r.LabelID] = r
	}
	if got := byLabel["lbl-tv"]; got.Reason != domain.ReasonGuardrail {
		t.Fatalf("lbl-tv outcome = %s reason = %q, want a guardrail rejection", got.Outcome, got.Reason)
	}
	if got := byLabel["lbl-cheap"]; !got.Applied() {
		t.Fatalf("lbl-cheap outcome = %s (%s); an ordinary markdown must still apply", got.Outcome, got.Detail)
	}

	// Only the safe label reached the glass, and the refused one still shows
	// its old price.
	msgs := h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	if len(msgs) != 1 {
		t.Fatalf("%d publishes, want only the label that passed the guard rail", len(msgs))
	}
	agg, err := h.svc.repo.Load(ctx, "lbl-tv")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Price.Amount != 24900 {
		t.Fatalf("the guard-railed label was repriced to %d", agg.Price.Amount)
	}
	if agg.RejectedCount != 1 {
		t.Fatalf("the rejection was not recorded on the label's stream: %d", agg.RejectedCount)
	}
	if got := h.svc.metrics.GuardrailRejections.With(string(testTenant)).Value(); got != 1 {
		t.Fatalf("guardrail metric = %d, want 1", got)
	}
}

// TestPromotionDoesNotOverwriteANewerDirectPriceChange covers the race the
// sequence rule alone does not settle.
//
// Which wins is decided by the source clock, not by arrival order: a till
// correction made after the promotion took effect is the more recent statement
// of what the retailer wants charged, and the till will charge it, so the shelf
// must show it. A promotion activated after a price change supersedes it.
func TestPromotionDoesNotOverwriteANewerDirectPriceChange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedPricedLabel(t, h, "lbl-milk-a", "sku-milk", 300, "dairy")
	seedPricedLabel(t, h, "lbl-milk-b", "sku-milk2", 300, "dairy")
	h.waitForMessages(canon.LeafPrice, 2, 5*time.Second)

	// The promotion takes effect now...
	activationAt := h.clock.Now()

	// ...but a colleague corrects one label's price a minute later, and that
	// change lands first.
	h.clock.Advance(time.Minute)
	placement, err := h.svc.Directory().Lookup(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	correction := h.clock.Now()
	res, err := h.svc.PriceHandler().Apply(ctx, app.PriceCommand{
		Placement: placement,
		Change: domain.PriceChange{
			SKU: "sku-milk", Price: canon.NewMoney(275, "USD"),
			EffectiveAt: correction, OccurredAt: correction, Now: correction,
			InitiatedBy: "colleague-14",
		},
	})
	if err != nil || !res.Applied() {
		t.Fatalf("direct correction: %+v (%v)", res, err)
	}
	h.resetCaptured()

	// Now the promotion's fan-out arrives, stamped with the moment the
	// promotion took effect.
	rule := promotionRule("promo-dairy", "dairy", 20)
	report, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, activationAt))
	if err != nil {
		t.Fatalf("activation: %v", err)
	}
	if report.Batch.Resolved != 2 {
		t.Fatalf("resolved = %d, want both dairy labels", report.Batch.Resolved)
	}
	byLabel := map[canon.LabelID]app.PriceResult{}
	for _, r := range report.Batch.Results {
		byLabel[r.LabelID] = r
	}
	// The corrected label refuses the older promotion price as out of order.
	if got := byLabel["lbl-milk-a"]; got.Outcome != app.OutcomeStale {
		t.Fatalf("lbl-milk-a outcome = %s (%s); a promotion must not overwrite a newer direct price",
			got.Outcome, got.Detail)
	}
	// The untouched label takes it normally.
	if got := byLabel["lbl-milk-b"]; !got.Applied() {
		t.Fatalf("lbl-milk-b outcome = %s (%s)", got.Outcome, got.Detail)
	}

	agg, err := h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Price.Amount != 275 {
		t.Fatalf("the corrected price was overwritten: shelf shows %d, want 275", agg.Price.Amount)
	}
	if agg.PromotionID != "" {
		t.Fatalf("the corrected label was marked promotional")
	}

	// The mirror image: a promotion activating *after* the correction wins.
	h.clock.Advance(time.Minute)
	h.resetCaptured()
	later, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, h.clock.Now()))
	if err != nil {
		t.Fatalf("later activation: %v", err)
	}
	if later.Batch.Applied != 1 {
		t.Fatalf("a promotion activated after the correction applied %d labels, want 1", later.Batch.Applied)
	}
	agg, err = h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// 20% off the *corrected* base of 2.75 is 2.20 — the promotion discounts
	// from the everyday price the correction established, not from the old one.
	if agg.Price.Amount != 220 {
		t.Fatalf("shelf shows %d, want 220 (20%% off the corrected 275)", agg.Price.Amount)
	}
}

// TestPromotionRefusesRulesTheShelfCannotEvaluate covers the safety rule:
// a condition the shelf tier cannot observe is refused by name rather than
// guessed at in either direction.
func TestPromotionRefusesRulesTheShelfCannotEvaluate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedPricedLabel(t, h, "lbl-milk-a", "sku-milk", 300, "dairy")
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	h.resetCaptured()

	tests := []struct {
		name    string
		mutate  func(*promodomain.Rule)
		outcome string
		reason  string
	}{
		{
			name:    "store clusters",
			mutate:  func(r *promodomain.Rule) { r.Conditions.StoreGroups = []string{"convenience"} },
			outcome: app.PromotionUnresolvable,
			reason:  app.ReasonUnobservableCondition,
		},
		{
			name:    "stock on hand",
			mutate:  func(r *promodomain.Rule) { r.Conditions.MinInventory = 5 },
			outcome: app.PromotionUnresolvable,
			reason:  app.ReasonUnobservableCondition,
		},
		{
			name: "shelf life",
			mutate: func(r *promodomain.Rule) {
				days := 3
				r.Conditions.MaxDaysToExpiry = &days
			},
			outcome: app.PromotionUnresolvable,
			reason:  app.ReasonUnobservableCondition,
		},
		{
			name: "a basket-dependent mechanic",
			mutate: func(r *promodomain.Rule) {
				r.Type = promodomain.TypeThreshold
				r.Params = promodomain.Params{ThresholdSpendMinor: 2000, PercentOff: 10}
			},
			outcome: app.PromotionSkipped,
			reason:  app.ReasonNotShelfPriceable,
		},
		{
			name:    "a loyalty-segmented rule",
			mutate:  func(r *promodomain.Rule) { r.Conditions.CustomerSegments = []string{"gold"} },
			outcome: app.PromotionSkipped,
			reason:  app.ReasonNotShelfPriceable,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule := promotionRule(canon.PromotionID("promo-unresolvable-"+string(rune('a'+i))), "dairy", 20)
			tc.mutate(&rule)
			h.clock.Advance(time.Minute)
			report, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
				promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, h.clock.Now()))
			if err != nil {
				t.Fatalf("activation: %v", err)
			}
			if report.Outcome != tc.outcome || report.Reason != tc.reason {
				t.Fatalf("outcome/reason = %s/%s, want %s/%s (detail: %s)",
					report.Outcome, report.Reason, tc.outcome, tc.reason, report.Detail)
			}
			if report.Detail == "" {
				t.Fatalf("a refusal must name what it could not evaluate")
			}
		})
	}
	time.Sleep(200 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 0 {
		t.Fatalf("a rule the shelf cannot evaluate reached %d labels", len(msgs))
	}
}

// TestPromotionRefusesAStaleActivation covers the replay guard at the fan-out
// level: one refusal with a named reason rather than forty thousand per-label
// rejection events.
func TestPromotionRefusesAStaleActivation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedPricedLabel(t, h, "lbl-milk-a", "sku-milk", 300, "dairy")
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	h.resetCaptured()

	rule := promotionRule("promo-ancient", "dairy", 20)
	stale := h.clock.Now().Add(-7 * 24 * time.Hour)
	report, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, stale))
	if err != nil {
		t.Fatalf("stale activation must not fail the record: %v", err)
	}
	if report.Outcome != app.PromotionSkipped || report.Reason != app.ReasonActivationStale {
		t.Fatalf("outcome/reason = %s/%s, want skipped/%s", report.Outcome, report.Reason, app.ReasonActivationStale)
	}
	time.Sleep(200 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 0 {
		t.Fatalf("a week-old activation reached %d shelves", len(msgs))
	}
	agg, err := h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.RejectedCount != 0 {
		t.Fatalf("one stale record wrote %d per-label rejection events", agg.RejectedCount)
	}
}

// TestPromotionStreamConsumerIsWired is the regression test for the gap itself:
// a record on `promotion-events`, consumed by the service's own consumer group,
// changes a shelf. No bridge, no composition-root wiring.
func TestPromotionStreamConsumerIsWired(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	seedPricedLabel(t, h, "lbl-milk-a", "sku-milk", 300, "dairy")
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)

	var wg sync.WaitGroup
	spawn := func(_ string, fn func(context.Context) error) {
		wg.Add(1)
		go func() { defer wg.Done(); _ = fn(ctx) }()
	}
	if err := h.svc.Start(ctx, spawn); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
	h.resetCaptured()

	rule := promotionRule("promo-dairy", "dairy", 20)
	at := h.clock.Now()

	// Same tail-pinning window as the other stream tests: a brand-new consumer
	// group pins its start offset the first time it owns a partition, so the
	// transition is offered until one delivery lands. The assertion afterwards
	// is the strong one — however many copies were offered, exactly one shelf
	// update was produced.
	deadline := time.Now().Add(15 * time.Second)
	for len(h.messages(canon.LeafPrice)) == 0 && time.Now().Before(deadline) {
		env := promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, at)
		publishEnvelope(t, h, canon.StreamPromotions.Name, env)
		time.Sleep(200 * time.Millisecond)
	}
	msgs := h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	if len(msgs) != 1 {
		t.Fatalf("one activation produced %d shelf updates", len(msgs))
	}
	_, update := h.decodeUpdate(msgs[0])
	h.verifyAttestation(update)
	if update.Price.Amount != 240 || update.PromotionID != "promo-dairy" {
		t.Fatalf("shelf update = %d / %q, want 240 / promo-dairy", update.Price.Amount, update.PromotionID)
	}
}

// TestPromotionEventsPartitionKeyIsTheProducersConcern documents the boundary
// the partitionKeyFor override sits on.
//
// The Label Service consumes `promotion-events` and never produces to it, so its
// publisher-side key override cannot affect the stream's partitioning: that is
// the Promotion Service's `tenant:promo` key, and it is what guarantees one
// promotion's transitions stay ordered. The test pins the fact that this
// service publishes nothing there, because the day it does, the key needs a
// case in partitionKeyFor and an activation could otherwise overtake its own
// expiry.
func TestPromotionEventsPartitionKeyIsTheProducersConcern(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedPricedLabel(t, h, "lbl-milk-a", "sku-milk", 300, "dairy")
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)

	consumer, err := h.bus.Subscribe(eventbus.SubscribeOptions{
		Group: "promotion-stream-observer", Topics: []string{canon.StreamPromotions.Name},
		FromBeginning: true,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer consumer.Close()

	produced := make(chan string, 4)
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.Run(runCtx, func(_ context.Context, m eventbus.Message) error {
			var env canon.Envelope
			if err := json.Unmarshal(m.Value, &env); err == nil && env.Source == app.SourceName {
				select {
				case produced <- env.EventType:
				default:
				}
			}
			return nil
		})
	}()

	rule := promotionRule("promo-dairy", "dairy", 20)
	if _, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, h.clock.Now())); err != nil {
		t.Fatalf("activation: %v", err)
	}
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)

	select {
	case et := <-produced:
		t.Fatalf("the Label Service published %q to promotion-events; partitionKeyFor now needs a case for it", et)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestSecondFanOutOfTheSamePromotionDrivesNoSecondRefresh answers the question
// the composition root needs answered before it deletes its promotion bridge.
//
// While both the bridge and this consumer are wired they sit in different
// consumer groups, so both see every activation and both fan it out. The
// duplicated work is real, but it cannot double-apply: the second fan-out
// computes the same price for the same SKU under the same promotion, and the
// aggregate's no-op rule declines it without burning a sequence or a refresh.
// This test stands in for the bridge by driving the batch path directly with
// the shape the bridge produces — a (store, SKU) item, its own idempotency key,
// and wall-clock time rather than the transition instant.
func TestSecondFanOutOfTheSamePromotionDrivesNoSecondRefresh(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedPricedLabel(t, h, "lbl-milk-a", "sku-milk", 300, "dairy")
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	h.resetCaptured()

	rule := promotionRule("promo-dairy", "dairy", 20)
	at := h.clock.Now()
	if _, err := h.svc.PromotionHandler().HandleEnvelope(ctx,
		promotionEnvelope(t, h, canon.EvtPromotionActivated, rule, promodomain.StateActive, at)); err != nil {
		t.Fatalf("activation: %v", err)
	}
	h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)

	agg, err := h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sequenceAfterFirst := agg.Sequence

	// The bridge's shape: same promotion, same resulting price, different
	// idempotency key, wall-clock timestamps, addressed by (store, SKU).
	h.clock.Advance(time.Second)
	report, err := h.svc.Batch().BatchUpdatePrices(ctx, app.BatchRequest{
		TenantID: testTenant, Region: testRegion,
		Items: []app.BatchItem{{
			StoreID: testStore, SKU: "sku-milk",
			Price:          canon.NewMoney(240, "USD"),
			WasPrice:       &canon.Money{Amount: 300, Currency: "USD"},
			EffectiveAt:    h.clock.Now(),
			PromotionID:    "promo-dairy",
			InitiatedBy:    "usslpd/promotion-bridge",
			IdempotencyKey: "promo:promo-dairy:store-01:sku-milk",
		}},
		OccurredAt: h.clock.Now(),
	})
	if err != nil {
		t.Fatalf("second fan-out: %v", err)
	}
	if report.Applied != 0 || report.Stale != 1 {
		t.Fatalf("second fan-out applied=%d stale=%d, want 0/1", report.Applied, report.Stale)
	}

	time.Sleep(200 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 1 {
		t.Fatalf("%d publishes for one promotion fanned out twice, want 1", len(msgs))
	}
	agg, err = h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if agg.Sequence != sequenceAfterFirst {
		t.Fatalf("the second fan-out burned a sequence: %d -> %d", sequenceAfterFirst, agg.Sequence)
	}
	if agg.Price.Amount != 240 {
		t.Fatalf("shelf shows %d, want 240", agg.Price.Amount)
	}
}

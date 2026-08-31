package label

import (
	"context"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestScheduledPriceRunnerActivatesAtTheEffectiveTime covers the whole point of
// scheduling: nobody has to be present at 9am for the 9am promotion to appear.
func TestScheduledPriceRunnerActivatesAtTheEffectiveTime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")

	// Seed a displayed price so the promotion has a was-price to strike out.
	if err := h.svc.PriceHandler().HandleEnvelope(ctx, h.priceEnvelope("sku-milk", 279, "seed")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.waitForMessages(canon.LeafPrice, 1, 3*time.Second)
	h.resetCaptured()

	placement, err := h.svc.Directory().Lookup(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	now := h.clock.Now()
	effective := now.Add(4 * time.Hour)
	res, err := h.svc.PriceHandler().Apply(ctx, app.PriceCommand{
		Placement: placement,
		Change: domain.PriceChange{
			SKU: "sku-milk", Price: canon.NewMoney(199, "USD"),
			WasPrice:    &canon.Money{Amount: 279, Currency: "USD"},
			PromotionID: "promo-morning", EffectiveAt: effective,
			OccurredAt: now, Now: now, ScheduleID: "sch-morning",
		},
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if res.Outcome != app.OutcomeScheduled {
		t.Fatalf("outcome = %s, want scheduled", res.Outcome)
	}

	// Nothing has reached the glass, and nothing is due yet.
	time.Sleep(150 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 0 {
		t.Fatalf("a future-dated change reached the glass immediately")
	}
	activated, err := h.svc.Scheduler().RunOnce(ctx, h.clock.Now())
	if err != nil {
		t.Fatalf("early sweep: %v", err)
	}
	if activated != 0 {
		t.Fatalf("a sweep four hours early activated %d changes", activated)
	}

	// The promotion's effective time arrives.
	h.clock.Advance(4*time.Hour + time.Minute)
	activated, err = h.svc.Scheduler().RunOnce(ctx, h.clock.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if activated != 1 {
		t.Fatalf("activated %d changes, want 1", activated)
	}

	msgs := h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	_, update := h.decodeUpdate(msgs[0])
	h.verifyAttestation(update)
	if update.Price.Amount != 199 {
		t.Fatalf("activated price = %d, want 199", update.Price.Amount)
	}
	// The sequence is allocated at activation, not at scheduling, so an urgent
	// change made in between would still have won at the label.
	if update.Sequence != 2 {
		t.Fatalf("sequence = %d, want 2", update.Sequence)
	}
	if update.Render.Template != domain.TemplatePromo || update.Render.Badge != "SALE" {
		t.Fatalf("render = %+v, want the promo template", update.Render)
	}

	// The due-index entry is gone, so a second sweep is a no-op rather than a
	// second refresh.
	activated, err = h.svc.Scheduler().RunOnce(ctx, h.clock.Now())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if activated != 0 {
		t.Fatalf("a second sweep re-activated %d changes", activated)
	}
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 1 {
		t.Fatalf("%d publishes after two sweeps, want 1", len(msgs))
	}
}

// TestSupersededScheduleNeverFires covers the rollback hazard: an older
// scheduled change must not fire after a newer decision has replaced it, or the
// shelf rolls back to a price nobody currently authorises.
func TestSupersededScheduleNeverFires(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")
	placement, err := h.svc.Directory().Lookup(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	now := h.clock.Now()
	effective := now.Add(2 * time.Hour)
	schedule := func(id string, amount int64) app.PriceResult {
		t.Helper()
		res, err := h.svc.PriceHandler().Apply(ctx, app.PriceCommand{
			Placement: placement,
			Change: domain.PriceChange{
				SKU: "sku-milk", Price: canon.NewMoney(amount, "USD"),
				EffectiveAt: effective, OccurredAt: h.clock.Now(), Now: h.clock.Now(),
				ScheduleID: id,
			},
		})
		if err != nil {
			t.Fatalf("schedule %s: %v", id, err)
		}
		return res
	}
	schedule("sch-a", 199)
	schedule("sch-b", 189)

	h.clock.Advance(2*time.Hour + time.Minute)
	activated, err := h.svc.Scheduler().RunOnce(ctx, h.clock.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if activated != 1 {
		t.Fatalf("activated %d changes, want only the surviving one", activated)
	}
	msgs := h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	if len(msgs) != 1 {
		t.Fatalf("%d publishes, want 1", len(msgs))
	}
	_, update := h.decodeUpdate(msgs[0])
	if update.Price.Amount != 189 {
		t.Fatalf("displayed price = %d, want the superseding 189", update.Price.Amount)
	}
}

// TestScheduleActivationSurvivesAnUrgentChangeInBetween covers the sequencing
// rule: sequences are handed out in the order updates reach the glass, which is
// the order the label's discard rule enforces.
func TestScheduleActivationSurvivesAnUrgentChangeInBetween(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")
	placement, err := h.svc.Directory().Lookup(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	now := h.clock.Now()
	if _, err := h.svc.PriceHandler().Apply(ctx, app.PriceCommand{
		Placement: placement,
		Change: domain.PriceChange{
			SKU: "sku-milk", Price: canon.NewMoney(199, "USD"),
			EffectiveAt: now.Add(3 * time.Hour), OccurredAt: now, Now: now,
			ScheduleID: "sch-evening",
		},
	}); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	// An urgent correction lands first and takes sequence 1.
	h.clock.Advance(time.Minute)
	urgent := h.clock.Now()
	res, err := h.svc.PriceHandler().Apply(ctx, app.PriceCommand{
		Placement: placement,
		Change: domain.PriceChange{
			SKU: "sku-milk", Price: canon.NewMoney(289, "USD"),
			EffectiveAt: urgent, OccurredAt: urgent, Now: urgent,
		},
	})
	if err != nil || !res.Applied() || res.Sequence != 1 {
		t.Fatalf("urgent change: %+v (%v)", res, err)
	}

	h.clock.Advance(3 * time.Hour)
	if _, err := h.svc.Scheduler().RunOnce(ctx, h.clock.Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	msgs := h.waitForMessages(canon.LeafPrice, 2, 5*time.Second)
	_, activated := h.decodeUpdate(msgs[len(msgs)-1])
	if activated.Price.Amount != 199 {
		t.Fatalf("activated price = %d, want 199", activated.Price.Amount)
	}
	if activated.Sequence != 2 {
		t.Fatalf("activated sequence = %d, want 2; the label discards anything not greater than 1",
			activated.Sequence)
	}
}

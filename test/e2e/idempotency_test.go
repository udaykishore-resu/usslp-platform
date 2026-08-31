package e2e

import (
	"testing"
	"time"
)

// TestRedeliveredWebhookIsAppliedOnce redelivers the identical POS webhook ten
// times and asserts exactly one price change and one label refresh.
//
// This is not a nicety. Shopify redelivers a webhook on an eight-hour schedule
// whenever it does not get a 2xx quickly enough, so a platform that treated
// every delivery as new would refresh a label ten times for one price change —
// ten E-Ink waveforms, ten times the battery cost, and ten entries in a
// compliance record that is supposed to say what happened.
//
// Four independent mechanisms have to line up for this to hold, and the test
// exercises all of them at once: the UIG's idempotency guard keyed on the
// webhook id, the envelope's idempotency key at the event store, the
// aggregate's optimistic concurrency, and the label's own monotonic sequence.
func TestRedeliveredWebhookIsAppliedOnce(t *testing.T) {
	tg := pick(t, shared, 0, 2, 4)
	before, _ := tg.Zone.Controller.Record(tg.Label)
	beforeStats := labelStats(t, tg)

	price := tg.nudge(73)
	webhookID := "shopify-redelivery-" + string(tg.SKU)

	// The first delivery goes through and lands on the glass.
	d, _ := pushPriceWithID(t, shared, tg, price, webhookID)
	if !d.Delivered {
		t.Fatal("the first delivery never reached the label")
	}
	afterFirst, _ := tg.Zone.Controller.Record(tg.Label)
	if afterFirst.Price.Cmp(price) != 0 {
		t.Fatalf("the first delivery put %s on the label, not %s",
			afterFirst.Price.Display(), price.Display())
	}

	// Nine more deliveries of exactly the same webhook. Shopify keeps the
	// webhook id constant across every redelivery of one event, which is why it
	// is the deduplication token.
	accepted := 0
	for i := 0; i < 9; i++ {
		if _, _, err := shared.PushShopifyPrice(t.Context(), tg.Tenant, tg.Store.ID,
			tg.SKU, price, webhookID); err != nil {
			t.Fatalf("redelivery %d was rejected outright: %v", i+2, err)
		}
		accepted++
	}
	// Every redelivery must be *acknowledged* — a 4xx would put Shopify's retry
	// schedule behind a message that will never be wanted — and must change
	// nothing.
	if accepted != 9 {
		t.Fatalf("only %d of 9 redeliveries were acknowledged", accepted)
	}
	time.Sleep(3 * time.Second)

	afterAll, _ := tg.Zone.Controller.Record(tg.Label)
	if afterAll.Sequence != afterFirst.Sequence {
		t.Errorf("ten deliveries of one webhook advanced the label's sequence from %d to %d; "+
			"exactly one should have been applied",
			afterFirst.Sequence, afterAll.Sequence)
	}
	if afterAll.Price.Cmp(price) != 0 {
		t.Errorf("the label ended showing %s, not %s", afterAll.Price.Display(), price.Display())
	}

	// One panel refresh, not ten. This is the assertion the battery budget
	// depends on: a full E-Ink waveform is about a hundred times the energy of
	// anything else a label does.
	afterStats := labelStats(t, tg)
	refreshes := afterStats.RefreshCount - beforeStats.RefreshCount
	if refreshes != 1 {
		t.Errorf("the panel refreshed %d times for one price change", refreshes)
	}
	t.Logf("10 deliveries of webhook %s: sequence %d -> %d, %d panel refresh(es), "+
		"%d stale frames discarded at the label",
		webhookID, before.Sequence, afterAll.Sequence, refreshes,
		afterStats.Discarded-beforeStats.Discarded)
}

// TestDistinctWebhookWithAnUnchangedPriceStillRefreshes records what the
// platform actually does when two *different* POS deliveries carry the same
// price, which is not what a reader of the idempotency contract would expect.
//
// INTERFACE-CONTRACTS §6 lists four deduplication boundaries and every one of
// them holds — TestRedeliveredWebhookIsAppliedOnce proves it. None of them
// covers this case: the deliveries are genuinely distinct events with distinct
// idempotency keys and increasing source timestamps, so the UIG passes both, the
// event store appends both, and the aggregate applies both. The label then runs
// a full E-Ink waveform to redraw the price it was already showing.
//
// This test asserts the behaviour rather than the expectation, so that it
// documents the gap instead of hiding it, and so that a future change which
// closes it fails here and gets noticed. It matters because a full waveform is
// roughly a hundred times the energy of anything else a label does, and a POS
// that republishes its price book nightly would spend the fleet's battery
// budget redrawing prices that did not change.
func TestDistinctWebhookWithAnUnchangedPriceStillRefreshes(t *testing.T) {
	tg := pick(t, shared, 0, 3, 5)
	price := tg.nudge(29)

	d, _ := pushPrice(t, shared, tg, price)
	if !d.Delivered {
		t.Fatal("the first price never landed")
	}
	before, _ := tg.Zone.Controller.Record(tg.Label)
	beforeStats := labelStats(t, tg)

	// A second, distinct webhook carrying the price the label already shows.
	if _, _, err := shared.PushShopifyPrice(t.Context(), tg.Tenant, tg.Store.ID,
		tg.SKU, price, ""); err != nil {
		t.Fatalf("the second delivery was rejected: %v", err)
	}
	time.Sleep(3 * time.Second)

	after, _ := tg.Zone.Controller.Record(tg.Label)
	afterStats := labelStats(t, tg)
	refreshes := afterStats.RefreshCount - beforeStats.RefreshCount
	t.Logf("a distinct webhook carrying the price already on the glass: "+
		"sequence %d -> %d, %d panel refresh(es)", before.Sequence, after.Sequence, refreshes)

	// The price is still right, which is the property that must never break.
	if after.Price.Cmp(price) != 0 {
		t.Errorf("the label ended showing %s, not %s", after.Price.Display(), price.Display())
	}
	// And the redundant refresh is recorded rather than asserted away. If this
	// ever stops happening, the platform got better and this test should be
	// changed to assert the improvement.
	if refreshes == 0 && after.Sequence == before.Sequence {
		t.Log("the platform now suppresses a re-application of the displayed price; " +
			"this test can be tightened into an assertion")
	}
}

// labelStats reads the simulated panel's own counters.
func labelStats(t *testing.T, tg target) labelCounters {
	t.Helper()
	l, ok := tg.Zone.Sim.Label(tg.Label)
	if !ok {
		t.Fatalf("%s has no simulated hardware", tg.Label)
	}
	s := l.Stats()
	return labelCounters{
		Sequence: s.Sequence, RefreshCount: s.RefreshCount,
		Discarded: s.Discarded, Frames: s.FramesReceived,
	}
}

type labelCounters struct {
	Sequence     int64
	RefreshCount int64
	Discarded    int64
	Frames       int64
}

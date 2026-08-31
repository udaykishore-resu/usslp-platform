package label

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// seedStore provisions labels facings-per-SKU across skus products and returns
// the label identifiers in the order they were created.
func seedStore(tb testing.TB, h *harness, skus, facings int) []canon.LabelID {
	tb.Helper()
	ids := make([]canon.LabelID, 0, skus*facings)
	n := 0
	for s := 0; s < skus; s++ {
		sku := canon.SKU(fmt.Sprintf("sku-%04d", s))
		// Twenty-five controllers per store, which is the design point: a
		// controller covers roughly eight metres of shelf.
		for f := 0; f < facings; f++ {
			id := labelID(n)
			sec := canon.SECID(fmt.Sprintf("sec-%02d", n%25))
			h.provisionLabel(id, sec, sku)
			ids = append(ids, id)
			n++
		}
	}
	return ids
}

func promotionItems(skus int, amount int64, at time.Time) []app.BatchItem {
	items := make([]app.BatchItem, 0, skus)
	for s := 0; s < skus; s++ {
		items = append(items, app.BatchItem{
			StoreID: testStore, SKU: canon.SKU(fmt.Sprintf("sku-%04d", s)),
			Price: canon.NewMoney(amount, "USD"), EffectiveAt: at,
			PromotionID: "promo-storewide", InitiatedBy: "merchandising",
		})
	}
	return items
}

// TestBatchFanOutToFiveThousandLabels is the scale case: a store-wide promotion
// that touches every facing at once, with a correct per-label result for each.
func TestBatchFanOutToFiveThousandLabels(t *testing.T) {
	if testing.Short() {
		t.Skip("provisioning 5,000 labels is not a short test")
	}
	h := newHarness(t)
	ctx := context.Background()

	const (
		skus    = 100
		facings = 50
		total   = skus * facings
	)
	ids := seedStore(t, h, skus, facings)
	if len(ids) != total {
		t.Fatalf("seeded %d labels, want %d", len(ids), total)
	}

	report, err := h.svc.Batch().BatchUpdatePrices(ctx, app.BatchRequest{
		TenantID: testTenant, Region: testRegion,
		Items:       promotionItems(skus, 399, time.Now().UTC()),
		InitiatedBy: "merchandising",
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if report.Requested != skus {
		t.Fatalf("requested = %d, want %d", report.Requested, skus)
	}
	if report.Resolved != total {
		t.Fatalf("resolved %d labels, want %d", report.Resolved, total)
	}
	if report.Applied != total {
		t.Fatalf("applied %d of %d labels (failed=%d rejected=%d stale=%d)",
			report.Applied, total, report.Failed, report.Rejected, report.Stale)
	}
	if report.Partial {
		t.Fatalf("batch reported partial failure with no failures")
	}
	if len(report.Results) != total {
		t.Fatalf("report holds %d results, want one per label", len(report.Results))
	}

	// Every result must name a distinct label, be attested, and carry the
	// sequence the label will enforce.
	seen := make(map[canon.LabelID]bool, total)
	for _, r := range report.Results {
		if seen[r.LabelID] {
			t.Fatalf("duplicate result for %s", r.LabelID)
		}
		seen[r.LabelID] = true
		if !r.Applied() {
			t.Fatalf("label %s outcome = %s (%s)", r.LabelID, r.Outcome, r.Detail)
		}
		if !r.Attested {
			t.Fatalf("label %s was published without an attestation", r.LabelID)
		}
		if r.Sequence != 1 {
			t.Fatalf("label %s sequence = %d, want 1", r.LabelID, r.Sequence)
		}
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("label %s was seeded but never repriced", id)
		}
	}

	msgs := h.waitForMessages(canon.LeafPrice, total, 60*time.Second)
	if len(msgs) != total {
		t.Fatalf("broker saw %d publishes, want %d", len(msgs), total)
	}
	// Spot-check the attestation on a sample rather than all 5,000: Ed25519
	// verification is ~50µs, and verifying every one would make this test about
	// the CPU rather than about the fan-out.
	for i := 0; i < len(msgs); i += 500 {
		_, update := h.decodeUpdate(msgs[i])
		h.verifyAttestation(update)
		if update.Price.Amount != 399 {
			t.Fatalf("published price = %d, want 399", update.Price.Amount)
		}
	}
	t.Logf("fan-out to %d labels in %s (%.0f labels/sec)",
		report.Resolved, report.Duration, float64(report.Resolved)/report.Duration.Seconds())
}

// TestBatchReportsPerLabelOutcomes covers the partial-failure contract: a batch
// mixing valid, guard-railed and unknown items must report each label's own
// outcome rather than one verdict for the whole call.
func TestBatchReportsPerLabelOutcomes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.provisionLabel("lbl-a", testSEC, "sku-ok")
	h.provisionLabel("lbl-b", testSEC, "sku-guard")

	// Seed a price on the guard-rail SKU so the second change has something to
	// be measured against.
	if err := h.svc.PriceHandler().HandleEnvelope(ctx, h.priceEnvelope("sku-guard", 249, "seed")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.waitForMessages(canon.LeafPrice, 1, 3*time.Second)
	h.resetCaptured()

	now := time.Now().UTC()
	report, err := h.svc.Batch().BatchUpdatePrices(ctx, app.BatchRequest{
		TenantID: testTenant,
		Items: []app.BatchItem{
			{StoreID: testStore, SKU: "sku-ok", Price: canon.NewMoney(499, "USD"), EffectiveAt: now},
			{StoreID: testStore, SKU: "sku-guard", Price: canon.NewMoney(49900, "USD"), EffectiveAt: now},
			{StoreID: testStore, SKU: "sku-not-stocked", Price: canon.NewMoney(199, "USD"), EffectiveAt: now},
			{StoreID: testStore, SKU: "sku-ok", Price: canon.NewMoney(599, "USD"),
				EffectiveAt: now.Add(6 * time.Hour)},
		},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if report.Requested != 4 {
		t.Fatalf("requested = %d, want 4", report.Requested)
	}
	// The unstocked SKU resolves to no labels at all, which is a fact with no
	// consequence rather than an error.
	if report.Resolved != 3 {
		t.Fatalf("resolved = %d, want 3 (two labels plus the scheduled change)", report.Resolved)
	}
	if report.Applied != 1 || report.Rejected != 1 || report.Scheduled != 1 {
		t.Fatalf("outcomes: applied=%d rejected=%d scheduled=%d, want 1/1/1",
			report.Applied, report.Rejected, report.Scheduled)
	}
	if report.Failed != 0 || report.Partial {
		t.Fatalf("a business rejection must not be reported as a failure: %+v", report)
	}
	byOutcome := map[string]app.PriceResult{}
	for _, r := range report.Results {
		byOutcome[r.Outcome] = r
	}
	if got := byOutcome[app.OutcomeRejected].Reason; got != domain.ReasonGuardrail {
		t.Fatalf("rejection reason = %q, want %q", got, domain.ReasonGuardrail)
	}
	// Only the accepted immediate change reaches the glass.
	time.Sleep(200 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 1 {
		t.Fatalf("%d publishes, want only the one accepted immediate change", len(msgs))
	}
}

// TestBatchHonoursContextCancellation covers the backpressure and cancellation
// contract: a caller that hangs up must stop the pipeline rather than leave it
// repricing a store nobody is waiting for.
func TestBatchHonoursContextCancellation(t *testing.T) {
	h := newHarness(t)
	seedStore(t, h, 20, 20)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := h.svc.Batch().BatchUpdatePrices(ctx, app.BatchRequest{
		TenantID: testTenant,
		Items:    promotionItems(20, 399, time.Now().UTC()),
	})
	if err == nil && report.Applied > 0 {
		t.Fatalf("a cancelled batch applied %d labels", report.Applied)
	}
}

// TestBatchDrainWaitsForInFlightWork covers graceful shutdown: a process that
// exits mid-fan-out leaves a store half repriced.
func TestBatchDrainWaitsForInFlightWork(t *testing.T) {
	h := newHarness(t)
	seedStore(t, h, 10, 10)

	done := make(chan app.BatchReport, 1)
	go func() {
		report, err := h.svc.Batch().BatchUpdatePrices(context.Background(), app.BatchRequest{
			TenantID: testTenant,
			Items:    promotionItems(10, 449, time.Now().UTC()),
		})
		if err != nil {
			t.Errorf("batch: %v", err)
		}
		done <- report
	}()

	// Wait until the fan-out is demonstrably under way before draining;
	// draining an idle pipeline would prove nothing.
	h.waitForMessages(canon.LeafPrice, 1, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.svc.Batch().Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	select {
	case report := <-done:
		if report.Applied != 100 {
			t.Fatalf("drain returned before the fan-out finished: applied %d of 100", report.Applied)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("drain returned while a batch was still running")
	}
}

// BenchmarkBatchUpdatePrices measures the fan-out pipeline against a
// pre-provisioned store.
//
// It measures the whole path a label update takes — aggregate load, decision,
// Ed25519 attestation, durable append, retained QoS 1 broker publish, two
// stream publishes, read-model write — because the number that matters is how
// many labels a replica can actually carry, not how fast a worker pool can pass
// values through a channel.
func BenchmarkBatchUpdatePrices(b *testing.B) {
	const (
		skus    = 40
		facings = 25
		total   = skus * facings
	)
	h := newHarness(b)
	ctx := context.Background()
	seedStore(b, h, skus, facings)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// A fresh price each round: an identical re-application is correctly a
		// no-op, and benchmarking no-ops would measure the wrong thing.
		report, err := h.svc.Batch().BatchUpdatePrices(ctx, app.BatchRequest{
			TenantID: testTenant, Region: testRegion,
			Items: promotionItems(skus, int64(400+i), time.Now().UTC()),
		})
		if err != nil {
			b.Fatalf("batch: %v", err)
		}
		if report.Applied != total {
			b.Fatalf("applied %d of %d labels", report.Applied, total)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(total*b.N)/b.Elapsed().Seconds(), "labels/sec")
}

// BenchmarkSingleLabelPriceUpdate measures the hot path one label at a time,
// which is the shape a POS-driven price change actually takes: the
// price-updates stream fans one record out to a handful of facings, not to a
// whole store.
func BenchmarkSingleLabelPriceUpdate(b *testing.B) {
	h := newHarness(b)
	ctx := context.Background()
	h.provisionLabel("lbl-bench", testSEC, "sku-bench")
	placement, err := h.svc.Directory().Lookup(ctx, "lbl-bench")
	if err != nil {
		b.Fatalf("lookup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		now := time.Now().UTC()
		res, err := h.svc.PriceHandler().Apply(ctx, app.PriceCommand{
			Placement: placement,
			Change: domain.PriceChange{
				SKU: "sku-bench", Price: canon.NewMoney(int64(400+i%50), "USD"),
				EffectiveAt: now, OccurredAt: now, Now: now,
			},
		})
		if err != nil {
			b.Fatalf("apply: %v", err)
		}
		if !res.Applied() {
			b.Fatalf("outcome = %s (%s)", res.Outcome, res.Detail)
		}
	}
}

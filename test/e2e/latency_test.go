package e2e

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestPriceReachesTheGlassWithinBudget is the platform's headline claim, once.
//
// A price change enters through the real Shopify webhook adapter — signed,
// HMAC-verified, deduplicated, normalised — and the test asserts four things
// about the other end: the right label updated, the price on its glass is the
// price that was sent, the attestation on that price verifies against the
// published key ring, and the whole thing fitted inside three seconds.
func TestPriceReachesTheGlassWithinBudget(t *testing.T) {
	tg := pick(t, shared, 0, 0, 0)
	before, hadBefore := tg.Zone.Controller.Record(tg.Label)

	want := tg.nudge(137)
	d, wall := pushPrice(t, shared, tg, want)

	if !d.Delivered {
		t.Fatalf("%s did not confirm the update", tg.Label)
	}
	if ok, why := shared.GlassMatches(tg.Zone, tg.Label, want); !ok {
		t.Fatalf("the price is not on the glass: %s", why)
	}
	if hadBefore && before.Price.Cmp(want) == 0 {
		t.Fatalf("the test asserted nothing: %s already showed %s", tg.Label, want.Display())
	}

	// The attestation is the load-bearing part of the claim: a price that
	// reached the glass without one would be a price no controller should have
	// displayed. Verifying it here, against the ring the controllers themselves
	// hold, proves the path is real rather than bypassed.
	verifyAttestation(t, shared, tg, want)

	budget := stack.TotalBudget
	if got := time.Duration(d.TotalMS) * time.Millisecond; got > budget {
		t.Errorf("the platform reported %s end to end, over the %s budget", got, budget)
	}
	if wall > budget+time.Second {
		// The wall-clock figure includes the test's own round trip to the UIG
		// and the scheduler's willingness to run this goroutine, so it is
		// allowed a second of slack over the contractual number. What it is
		// not allowed to be is wildly different, which is what would happen if
		// the simulated clock had drifted.
		t.Errorf("the wall clock said %s, which is too far from the reported %dms", wall, d.TotalMS)
	}

	t.Logf("POS webhook to pixels: platform %dms, wall clock %s "+
		"(SEC->label %dms, refresh %dms, %d mesh hops, partial=%v)",
		d.TotalMS, wall.Round(time.Millisecond), d.SECToLabel, d.RefreshMS, d.Hops, d.Partial)
}

// TestEndToEndLatencyAgreesWithWallClock is the check on the check.
//
// Every latency this suite reports comes from a controller stamping a
// simulated clock. If that clock drifts from the wall clock the numbers stay
// self-consistent and become meaningless, and nothing else in the suite would
// notice. So: twenty price changes, each timed twice — once by the platform,
// once by a stopwatch outside it — and the two medians must agree.
func TestEndToEndLatencyAgreesWithWallClock(t *testing.T) {
	const samples = 20
	var reported, measured []time.Duration
	for i := 0; i < samples; i++ {
		tg := pick(t, shared, 0, i%sharedControllers, 1+i/sharedControllers)
		d, wall := pushPrice(t, shared, tg, tg.nudge(int64(11+i)))
		reported = append(reported, time.Duration(d.TotalMS)*time.Millisecond)
		measured = append(measured, wall)
	}
	rp50, wp50 := percentile(reported, 50), percentile(measured, 50)
	skew := wp50 - rp50
	if skew < 0 {
		skew = -skew
	}
	t.Logf("median: platform %s, wall clock %s, difference %s", rp50, wp50, skew)
	// Half a second covers the test's own HTTP round trip and the pacing error
	// of the simulation runner. A clock that had drifted by ten minutes — which
	// is exactly what an unadjusted epoch produces after a fast-forwarded mesh
	// formation — fails this by three orders of magnitude.
	if skew > 500*time.Millisecond {
		t.Errorf("the platform's own latency measurement disagrees with the wall clock by %s; "+
			"the simulated clock has drifted and every number this suite reports is suspect", skew)
	}
}

// TestPriceLatencyPercentiles is the claim at scale: a thousand price changes
// through the whole platform, with the distribution and the per-hop breakdown
// printed against the contract's budget table.
//
// It is skipped by -short. A thousand E-Ink refreshes at 1.5 s each, across
// four controllers with eight transmissions in flight per zone, is bounded by
// the radio and the panels rather than by the software, and takes about a
// minute.
func TestPriceLatencyPercentiles(t *testing.T) {
	if testing.Short() {
		t.Skip("1,000 price changes through a simulated store; -short skips it")
	}
	const samples = 1000

	store := shared.Stores()[0]
	labels := store.Labels()
	if len(labels) == 0 {
		t.Fatal("the shared store has no labels")
	}

	// A fresh measurement window: the opening price book and the tests above
	// are already in the runtime's record, and mixing them in would be
	// reporting a different experiment.
	base := len(shared.Deliveries(store.ID))
	failedBefore := deliveryFailures(store)

	type outcome struct {
		wall time.Duration
		ok   bool
	}
	results := make(chan outcome, samples)

	// Ten price changes in flight at once.
	//
	// The number is chosen against what this store can carry, and the reasoning
	// is the whole reason it is stated here. A 100-label store on a 2-core
	// container sustains about 13 price changes a second end to end: the
	// ceiling is the edge tier — one discrete-event clock per store driving four
	// zones of radio, and a panel that spends 300 ms to 1.5 s of real time on
	// every waveform — not the cloud services, which are idle by comparison.
	//
	// Offering more than that does not measure the platform's latency; it
	// measures a queue. At sixteen in flight this same benchmark reports a p99
	// of about 3.3 s, and every millisecond of the excess is time an update
	// spent waiting for a transmission slot, which Little's law predicts
	// exactly. The three-second SLO is a statement about a price change, not
	// about a store held permanently at saturation, so the benchmark offers
	// load below the ceiling and test/load measures what happens above it.
	const inFlight = 10
	sem := make(chan struct{}, inFlight)

	// One change in flight per label. Two concurrent changes to the same label
	// are legal — the platform orders them per (store, SKU) and the second wins
	// — but a test that raced itself would compute the same "next sequence" in
	// two goroutines and one of them would wait forever for a sequence the
	// other consumed. That is the harness measuring its own race rather than
	// the platform's latency.
	var perLabel sync.Map
	started := time.Now()
	for i := 0; i < samples; i++ {
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem }()
			id := labels[i%len(labels)]
			lockAny, _ := perLabel.LoadOrStore(id, make(chan struct{}, 1))
			lock := lockAny.(chan struct{})
			lock <- struct{}{}
			defer func() { <-lock }()

			zone, _, ok := store.FindLabel(id)
			if !ok {
				results <- outcome{}
				return
			}
			sku, _ := store.SKUOf(id)
			rec, _ := zone.Controller.Record(id)
			watch := shared.WatchDelivery(id, rec.Sequence+1)
			price := canon.NewMoney(rec.Price.Amount+1+int64(i%37), rec.Price.Currency)
			sent, _, err := shared.PushShopifyPrice(t.Context(), store.Tenant, store.ID, sku, price, "")
			if err != nil {
				results <- outcome{}
				return
			}
			d, arrived := watch.Wait(60 * time.Second)
			results <- outcome{wall: time.Since(sent), ok: arrived && d.Delivered}
		}(i)
	}
	var wall []time.Duration
	delivered := 0
	for i := 0; i < samples; i++ {
		r := <-results
		if r.ok {
			delivered++
			wall = append(wall, r.wall)
		}
	}
	elapsed := time.Since(started)

	// Only the deliveries this test caused.
	all := shared.Deliveries(store.ID)
	if len(all) > base {
		all = all[base:]
	}
	report := shared.SLO(store.ID)

	t.Logf("\n%s", renderReport(samples, delivered, elapsed, wall, all))

	// Every change is accounted for: it reached a label, or the controller
	// reported that it could not. A price change that simply vanishes is the
	// one outcome a pricing platform may not have, because nobody finds out.
	failed := deliveryFailures(store) - failedBefore
	t.Logf("%d delivered, %d reported as failed by a controller, %d unaccounted for",
		delivered, failed, samples-delivered-failed)
	if delivered+failed < samples {
		t.Errorf("%d of %d price changes neither reached a label nor were reported as failed",
			samples-delivered-failed, samples)
	}
	if delivered < samples {
		t.Logf("delivery rate %.2f%%: %d change(s) were refused by the radio after retries "+
			"and reported upstream as label.update.failed",
			100*float64(delivered)/float64(samples), samples-delivered)
	}
	p99 := percentileOf(all, func(d stack.Delivery) int64 { return d.TotalMS }, 99)
	if p99 > stack.TotalBudget.Milliseconds() {
		t.Errorf("p99 end-to-end latency was %dms, over the %s budget. "+
			"This is the platform's headline claim and it is not met at this concurrency.",
			p99, stack.TotalBudget)
	}
	if report.Deliveries == 0 {
		t.Error("the SLO read model recorded no deliveries")
	}
}

// renderReport lays the measurement next to the budget table, because a
// latency number without the budget beside it is an assertion rather than
// evidence.
func renderReport(sent, delivered int, elapsed time.Duration, wall []time.Duration, ds []stack.Delivery) string {
	out := ""
	add := func(f string, a ...any) { out += fmt.Sprintf(f, a...) }

	add("  %d price changes, %d delivered, in %s (%.1f/s sustained)\n",
		sent, delivered, elapsed.Round(time.Millisecond), float64(delivered)/elapsed.Seconds())
	add("  Measured on a 2-core container; the edge tier is simulated at 1:1 wall clock.\n\n")

	add("  END TO END (envelope RecordedAt -> pixels settled, the contract's number)\n")
	add("    p50 %6dms   p95 %6dms   p99 %6dms   max %6dms   budget %dms\n\n",
		percentileOf(ds, totalMS, 50), percentileOf(ds, totalMS, 95),
		percentileOf(ds, totalMS, 99), percentileOf(ds, totalMS, 100),
		stack.TotalBudget.Milliseconds())

	add("  WALL CLOCK OUTSIDE THE PROCESS (webhook sent -> delivery observed)\n")
	add("    p50 %6dms   p95 %6dms   p99 %6dms\n\n",
		percentile(wall, 50).Milliseconds(), percentile(wall, 95).Milliseconds(),
		percentile(wall, 99).Milliseconds())

	add("  PER HOP, AGAINST INTERFACE-CONTRACTS §4\n")
	add("    %-42s %8s %8s %8s\n", "hop", "budget", "p50", "p99")
	cloudBudget := int64(0)
	for _, h := range stack.Budget[:5] {
		cloudBudget += h.BudgetMS
		add("    %-42s %7dms %8s %8s\n", "  "+h.Name, h.BudgetMS, "-", "-")
	}
	add("    %-42s %7dms %7dms %7dms   measured as a residual\n",
		"the five hops above, together", cloudBudget,
		percentileOf(ds, cloudMS, 50), percentileOf(ds, cloudMS, 99))
	add("    %-42s %7dms %7dms %7dms   measured by the controller\n",
		stack.Budget[5].Name, stack.Budget[5].BudgetMS,
		percentileOf(ds, func(d stack.Delivery) int64 { return d.SECToLabel }, 50),
		percentileOf(ds, func(d stack.Delivery) int64 { return d.SECToLabel }, 99))
	add("    %-42s %7dms %7dms %7dms   measured by the panel\n",
		stack.Budget[6].Name, stack.Budget[6].BudgetMS,
		percentileOf(ds, func(d stack.Delivery) int64 { return d.RefreshMS }, 50),
		percentileOf(ds, func(d stack.Delivery) int64 { return d.RefreshMS }, 99))
	add("    %-42s %7dms %8s %8s   not separately observable\n",
		stack.Budget[7].Name, stack.Budget[7].BudgetMS, "-", "-")
	add("    %-42s %7dms\n", "TOTAL", stack.BudgetTotalMS())
	return out
}

func totalMS(d stack.Delivery) int64 { return d.TotalMS }

// deliveryFailures is the number of updates the store's controllers gave up on.
func deliveryFailures(store *stack.Store) int {
	n := 0
	for _, z := range store.Zones {
		n += int(z.Controller.Stats().DeliveryFailed)
	}
	return n
}

// cloudMS is everything the controller did not do: ingest, the durable append,
// the Label Service, the broker publish and the store bridge. It is a residual
// rather than a measurement and the report says so.
func cloudMS(d stack.Delivery) int64 {
	v := d.TotalMS - d.SECToLabel - d.RefreshMS
	if v < 0 {
		return 0
	}
	return v
}

func percentileOf(ds []stack.Delivery, f func(stack.Delivery) int64, p int) int64 {
	vals := make([]time.Duration, 0, len(ds))
	for _, d := range ds {
		if d.Delivered {
			vals = append(vals, time.Duration(f(d)))
		}
	}
	return int64(percentile(vals, p))
}

// verifyAttestation independently verifies the update the controller acted on,
// the same way the controller did: recompute the signed tuple from the update
// as received, and check the signature against the published key ring.
//
// The update is taken from the tap on the store's own MQTT broker rather than
// from the controller's memory, so what is verified is what actually crossed
// the last hop before the shelf.
func verifyAttestation(t *testing.T, st *stack.Stack, tg target, price canon.Money) {
	t.Helper()
	env, upd, ok := priceTapFor(t, st, tg.Store).last(tg.Label)
	if !ok {
		t.Fatalf("no price update for %s was seen on %s's broker", tg.Label, tg.Store.ID)
	}
	if upd.Price.Cmp(price) != 0 {
		t.Fatalf("the last update on the wire for %s carried %s, not %s",
			tg.Label, upd.Price.Display(), price.Display())
	}
	if upd.Attestation.Algorithm != canon.AttestationAlg {
		t.Fatalf("attestation algorithm is %q, not %q", upd.Attestation.Algorithm, canon.AttestationAlg)
	}
	input := canon.AttestationInputFrom(env.TenantID, upd)
	if err := st.KeyRing().Verify(input, upd.Attestation); err != nil {
		t.Fatalf("the attestation on the price delivered to %s does not verify "+
			"against the published key ring: %v", tg.Label, err)
	}
}

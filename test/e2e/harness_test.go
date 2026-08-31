// Package e2e tests the claims USSLP is sold on, against the whole platform.
//
// Every test here boots a real runtime — platform/cmd/usslpd/stack, in
// process, with an ephemeral data directory — and asserts on behaviour rather
// than on internals. Nothing is mocked and nothing is bypassed: a price change
// enters through the Shopify webhook adapter with a real HMAC, is signed with a
// real Ed25519 price-authority key, is verified by a Shelf Edge Controller
// against a real key ring, and is measured to the moment the simulated E-Ink
// panel finished its waveform.
//
// # What the numbers mean, and what they do not
//
// The labels and their radio are edge/labelsim and edge/mesh over a
// discrete-event clock paced 1:1 against the wall clock. Airtime, CSMA backoff,
// mesh hops, duty cycling and waveform duration are modelled from the hardware
// budget; they are not measurements of silicon. A latency measured here is
// therefore an honest measurement of everything above the radio and a faithful
// model of the radio itself. It is evidence, not a field trial.
//
// The clock is the one thing worth being suspicious of, so the suite checks it:
// TestEndToEndLatencyAgreesWithWallClock compares the platform's own reported
// latency against a stopwatch held outside the process, and fails if they
// disagree by more than the pacing error.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
)

// shared is the runtime most tests use. Booting one platform for the whole
// package rather than one per test is worth about a minute of wall clock; the
// tests that mutate the fleet destructively — cutting a WAN link, killing a
// controller, killing a mesh relay — boot their own with newStack.
var shared *stack.Stack

// The shared store's shape. Four controllers is enough for the mesh, the OTA
// cohorts and the fan-out to be real; twenty-five labels each is a hundred
// labels, which is a hundred E-Ink panels each spending 1.5 s of wall clock per
// refresh, and that is what bounds how long this package takes.
const (
	sharedControllers = 4
	sharedLabels      = 25
)

func TestMain(m *testing.M) {
	st, err := stack.New(stack.Config{
		Ephemeral:           true,
		Tenants:             []canon.TenantID{"demo-retail"},
		Stores:              1,
		ControllersPerStore: sharedControllers,
		LabelsPerController: sharedLabels,
		Seed:                20260830,
		LogLevel:            "error",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: building the runtime:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := st.Start(ctx); err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "e2e: starting the runtime:", err)
		os.Exit(1)
	}
	cancel()
	shared = st

	code := m.Run()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = st.Stop(stopCtx)
	stopCancel()
	os.Exit(code)
}

// newStack boots a private runtime for a test that is going to break something.
func newStack(t *testing.T, cfg stack.Config) *stack.Stack {
	t.Helper()
	if cfg.Tenants == nil {
		cfg.Tenants = []canon.TenantID{"demo-retail"}
	}
	cfg.Ephemeral = true
	if cfg.LogLevel == "" {
		cfg.LogLevel = "error"
	}
	if cfg.Seed == 0 {
		cfg.Seed = 7
	}
	st, err := stack.New(cfg)
	if err != nil {
		t.Fatalf("building the runtime: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := st.Start(ctx); err != nil {
		t.Fatalf("starting the runtime: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := st.Stop(c); err != nil {
			t.Errorf("stopping the runtime: %v", err)
		}
	})
	return st
}

// smallStore is the configuration for a test that boots its own runtime: small
// enough to come up in a couple of seconds, large enough for the mesh, the
// fan-out and the OTA cohorts to be real.
func smallStore(controllers, labels int) stack.Config {
	return stack.Config{Stores: 1, ControllersPerStore: controllers, LabelsPerController: labels}
}

// ---------------------------------------------------------------------------
// Locating things on a shelf
// ---------------------------------------------------------------------------

// target is one label, everything about it a test needs, resolved once.
type target struct {
	Store  *stack.Store
	Zone   *stack.Zone
	Label  canon.LabelID
	SKU    canon.SKU
	Tenant canon.TenantID
}

// pick returns the nth label of the nth controller in a store.
//
// Deterministic rather than random: a test that fails should fail on the same
// label every time, because the first question is always "is it that label or
// is it the platform".
func pick(t *testing.T, st *stack.Stack, storeIdx, zoneIdx, labelIdx int) target {
	t.Helper()
	stores := st.Stores()
	if storeIdx >= len(stores) {
		t.Fatalf("the runtime has %d stores, wanted index %d", len(stores), storeIdx)
	}
	store := stores[storeIdx]
	if zoneIdx >= len(store.Zones) {
		t.Fatalf("%s has %d controllers, wanted index %d", store.ID, len(store.Zones), zoneIdx)
	}
	zone := store.Zones[zoneIdx]
	labels := zone.Labels()
	if labelIdx >= len(labels) {
		t.Fatalf("%s has %d labels, wanted index %d", zone.SECID, len(labels), labelIdx)
	}
	id := labels[labelIdx]
	sku, ok := store.SKUOf(id)
	if !ok {
		t.Fatalf("%s has no planogram assignment", id)
	}
	return target{Store: store, Zone: zone, Label: id, SKU: sku, Tenant: store.Tenant}
}

// nextSequence is the sequence a label's next update will carry.
func (tg target) nextSequence(t *testing.T) int64 {
	t.Helper()
	rec, ok := tg.Zone.Controller.Record(tg.Label)
	if !ok {
		return 1
	}
	return rec.Sequence + 1
}

// currentPrice is what the label is showing now.
func (tg target) currentPrice() canon.Money {
	rec, ok := tg.Zone.Controller.Record(tg.Label)
	if !ok {
		return usd(0)
	}
	return rec.Price
}

// nudge returns a price that differs from the current one by a few cents.
//
// It is not decoration. The Label Service refuses any change of more than five
// times the current price as a corrupt feed rather than a decision
// (domain.DefaultGuardrailFactor, and the reasoning there is sound: the failure
// it exists to catch is a decimal point lost between an ERP and a CSV). A test
// that picked prices out of the air would be silently testing the guard rail
// instead of the price path — which is exactly what happened the first time
// this suite was run.
func (tg target) nudge(delta int64) canon.Money {
	cur := tg.currentPrice()
	if cur.Amount == 0 {
		return usd(999 + delta)
	}
	if delta == 0 {
		delta = 1
	}
	next := cur.Amount + delta
	if next < 25 {
		next = 25 + delta
	}
	return canon.NewMoney(next, cur.Currency)
}

// ---------------------------------------------------------------------------
// Driving a price through the platform
// ---------------------------------------------------------------------------

// pushPrice delivers a signed Shopify webhook and waits for the label to
// confirm that the pixels changed.
//
// It returns the platform's own measurement (from the envelope's RecordedAt to
// the pixels settling, which is what INTERFACE-CONTRACTS §4 defines the SLO
// against) alongside a wall-clock measurement taken outside the process. The
// two are reported together so that a test which trusts the first can be
// checked by the second.
func pushPrice(t *testing.T, st *stack.Stack, tg target, price canon.Money) (stack.Delivery, time.Duration) {
	t.Helper()
	return pushPriceWithID(t, st, tg, price, "")
}

func pushPriceWithID(t *testing.T, st *stack.Stack, tg target, price canon.Money, webhookID string) (stack.Delivery, time.Duration) {
	t.Helper()
	// The tap on the store's broker has to be subscribed before the update
	// crosses it. Prices are published retained, so a late subscriber usually
	// still sees the last one — but "usually" is how a suite acquires a test
	// that passes on a quiet machine and fails in CI.
	priceTapFor(t, st, tg.Store)
	want := tg.nextSequence(t)

	// The interest is registered before the webhook is sent. Registering it
	// afterwards is a race that fails roughly never on a quiet machine and
	// often under -race, which is the worst possible failure rate.
	watch := st.WatchDelivery(tg.Label, want)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sent, _, err := st.PushShopifyPrice(ctx, tg.Tenant, tg.Store.ID, tg.SKU, price, webhookID)
	if err != nil {
		t.Fatalf("pushing a price for %s: %v", tg.SKU, err)
	}
	d, ok := watch.Wait(30 * time.Second)
	if !ok {
		t.Fatalf("%s never confirmed sequence %d within 30s", tg.Label, want)
	}
	// A delivery that the controller gave up on is reported here, not left for
	// a later assertion to trip over as "the glass shows the wrong price". The
	// radio abandons after three attempts, so this is a real outcome and the
	// failure should name it.
	if !d.Delivered {
		t.Fatalf("%s did not apply sequence %d: the controller gave up after %d mesh hop(s)",
			tg.Label, want, d.Hops)
	}
	return d, time.Since(sent)
}

// ---------------------------------------------------------------------------
// Reading the event streams
// ---------------------------------------------------------------------------

// collectStream reads records from a stream into a channel until the context
// ends. It is how a test asserts on what the platform *recorded*, which is a
// stronger claim than what it happened to do.
func collectStream(t *testing.T, st *stack.Stack, streamName, group string) <-chan canon.Envelope {
	t.Helper()
	out := make(chan canon.Envelope, 4096)
	consumer, err := st.EventLog().Subscribe(eventbus.SubscribeOptions{
		// From the beginning, always. A group joining at the tail races the
		// thing the test is about to do, and the failure is a test that passes
		// on a loaded machine and fails on an idle one.
		Group: group, Topics: []string{streamName}, FromBeginning: true,
	})
	if err != nil {
		t.Fatalf("subscribing to %s: %v", streamName, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = consumer.Close() })
	go func() {
		_ = consumer.Run(ctx, func(_ context.Context, m eventbus.Message) error {
			var env canon.Envelope
			if err := json.Unmarshal(m.Value, &env); err != nil {
				return nil
			}
			select {
			case out <- env:
			default:
				// A test that has stopped reading must not wedge the consumer
				// group for the rest of the package.
			}
			return nil
		})
	}()
	return out
}

// awaitEnvelope waits for an envelope matching a predicate.
func awaitEnvelope(t *testing.T, ch <-chan canon.Envelope, within time.Duration, match func(canon.Envelope) bool) (canon.Envelope, bool) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case env := <-ch:
			if match(env) {
				return env, true
			}
		case <-deadline:
			return canon.Envelope{}, false
		}
	}
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

// percentile returns the pth percentile of a set of durations, nearest-rank.
func percentile(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), ds...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	idx := (p*len(c) + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(c) {
		idx = len(c)
	}
	return c[idx-1]
}

// eventually polls a condition until it holds or the deadline passes.
//
// Polling rather than sleeping is deliberate: every boundary in this platform
// that a test has to wait on — a projection catching up, a detector changing
// mode, a rollout advancing a cohort — is eventually consistent by design, and
// a fixed sleep either flakes or wastes time.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", within, what)
}

// usd builds a price in the runtime's default trading currency.
func usd(minor int64) canon.Money { return canon.NewMoney(minor, "USD") }

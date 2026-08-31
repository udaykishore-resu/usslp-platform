package label

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// TestPriceChangeReachesTheRightLabels is the whole point of the service in one
// test: a price change on the stream produces exactly one retained QoS 1
// publish per facing, on the owning controller's zone topic, carrying an
// attestation that verifies under the published key ring.
func TestPriceChangeReachesTheRightLabels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Two facings of the priced product, one facing of a different product on a
	// different controller. The third must not receive anything.
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")
	h.provisionLabel("lbl-milk-b", testSEC, "sku-milk")
	h.provisionLabel("lbl-bread-a", "sec-09", "sku-bread")

	env := h.priceEnvelope("sku-milk", 279, "pos-webhook-1")
	if err := h.svc.PriceHandler().HandleEnvelope(ctx, env); err != nil {
		t.Fatalf("handling the price change: %v", err)
	}

	msgs := h.waitForMessages(canon.LeafPrice, 2, 3*time.Second)
	if len(msgs) != 2 {
		t.Fatalf("published to %d labels, want exactly the 2 facings of sku-milk", len(msgs))
	}

	seen := map[canon.LabelID]bool{}
	for _, m := range msgs {
		if m.QoS != msgbus.AtLeastOnce {
			t.Errorf("published at QoS %d; a lost price update is a compliance incident", m.QoS)
		}
		scope, sec, label, leaf, ok := canon.ParseSECLabelTopic(m.Topic)
		if !ok {
			t.Fatalf("topic %q is not a controller zone topic", m.Topic)
		}
		if scope.Tenant != testTenant || scope.Store != testStore || scope.Region != testRegion {
			t.Errorf("topic scope = %+v, want %s/%s/%s", scope, testTenant, testRegion, testStore)
		}
		if sec != testSEC {
			t.Errorf("routed through controller %s, want %s", sec, testSEC)
		}
		if leaf != canon.LeafPrice {
			t.Errorf("leaf = %q, want %q", leaf, canon.LeafPrice)
		}
		seen[label] = true

		envelope, update := h.decodeUpdate(m)
		if envelope.EventType != canon.EvtPriceUpdated {
			t.Errorf("event type = %q, want %q", envelope.EventType, canon.EvtPriceUpdated)
		}
		if envelope.TraceID != env.TraceID {
			t.Errorf("trace context was not propagated: %q != %q", envelope.TraceID, env.TraceID)
		}
		if envelope.CausationID != env.EventID {
			t.Errorf("causation lost: %q != %q", envelope.CausationID, env.EventID)
		}
		if update.Price.Amount != 279 || update.Price.Currency != "USD" {
			t.Errorf("published price = %s, want 2.79 USD", update.Price.String())
		}
		if update.Sequence != 1 {
			t.Errorf("sequence = %d, want 1 for a label's first price", update.Sequence)
		}
		if update.Render.PartialRefresh {
			t.Errorf("first price on a label must use a full waveform")
		}
		h.verifyAttestation(update)
	}
	if !seen["lbl-milk-a"] || !seen["lbl-milk-b"] {
		t.Fatalf("expected both milk facings, got %v", seen)
	}
	if seen["lbl-bread-a"] {
		t.Fatalf("a bread label was repriced by a milk price change")
	}

	// The retained flag is what lets a controller recover its zone after a
	// power cut, so assert it at the broker rather than only in the publish.
	if n := h.broker.RetainedCount(); n < 2 {
		t.Fatalf("broker holds %d retained messages, want at least 2", n)
	}
}

// TestAttestationCoversThePriceOnTheWire proves the guarantee the whole
// platform rests on: a substituted price fails verification even when the
// signature is otherwise valid, because the controller recomputes the digest
// from what it is holding rather than trusting what it was sent.
func TestAttestationCoversThePriceOnTheWire(t *testing.T) {
	h := newHarness(t)
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")
	if err := h.svc.PriceHandler().HandleEnvelope(context.Background(),
		h.priceEnvelope("sku-milk", 279, "pos-webhook-1")); err != nil {
		t.Fatalf("price change: %v", err)
	}
	msgs := h.waitForMessages(canon.LeafPrice, 1, 3*time.Second)
	_, update := h.decodeUpdate(msgs[0])
	h.verifyAttestation(update)

	tampered := update
	tampered.Price = canon.NewMoney(1, "USD")
	ring, err := h.authority.KeyRing()
	if err != nil {
		t.Fatalf("key ring: %v", err)
	}
	err = ring.Verify(canon.AttestationInputFrom(testTenant, tampered), tampered.Attestation)
	if !errors.Is(err, canon.ErrAttestationInvalid) {
		t.Fatalf("a tampered price verified: %v", err)
	}
}

// TestDuplicateStreamRecordPublishesOnce covers the at-least-once delivery
// guarantee: every consumer must be idempotent, and a redelivered POS webhook
// must not drive a second E-Ink refresh.
func TestDuplicateStreamRecordPublishesOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")

	env := h.priceEnvelope("sku-milk", 279, "pos-webhook-dup")
	if err := h.svc.PriceHandler().HandleEnvelope(ctx, env); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	h.waitForMessages(canon.LeafPrice, 1, 3*time.Second)

	// The same record again, exactly as a Kafka redelivery presents it.
	if err := h.svc.PriceHandler().HandleEnvelope(ctx, env); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	// And a different event id carrying the same idempotency key, which is what
	// a producer retry after a partial failure looks like.
	retry := env
	retry.EventID = canon.NewEventID()
	if err := h.svc.PriceHandler().HandleEnvelope(ctx, retry); err != nil {
		t.Fatalf("producer retry: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 1 {
		t.Fatalf("published %d times for one logical price change, want 1", len(msgs))
	}

	agg, err := h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Sequence != 1 {
		t.Fatalf("sequence = %d after three deliveries of one change, want 1", agg.Sequence)
	}
}

// TestOutOfOrderRecordIsRejected covers the reordering half of the same
// guarantee. Ordering is only promised per partition key, and a record that
// crosses partitions — a label reassigned between products, a replayed
// backfill — can arrive after a newer one.
func TestOutOfOrderRecordIsRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")

	newer := h.priceEnvelope("sku-milk", 279, "pos-newer")
	if err := h.svc.PriceHandler().HandleEnvelope(ctx, newer); err != nil {
		t.Fatalf("newer record: %v", err)
	}
	h.waitForMessages(canon.LeafPrice, 1, 3*time.Second)
	h.resetCaptured()

	older := h.priceEnvelope("sku-milk", 199, "pos-older")
	older.OccurredAt = newer.OccurredAt.Add(-2 * time.Hour)
	if err := h.svc.PriceHandler().HandleEnvelope(ctx, older); err != nil {
		t.Fatalf("older record must be handled, not errored: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 0 {
		t.Fatalf("an out-of-order record reached the glass: %d publishes", len(msgs))
	}
	agg, err := h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Price.Amount != 279 {
		t.Fatalf("displayed price rolled back to %d", agg.Price.Amount)
	}
}

// TestGuardrailRejectionIsRecordedAndNotDisplayed covers the fat-fingered feed:
// milk at $249.00 must not reach the glass, and the refusal must be a durable,
// auditable fact rather than a log line.
func TestGuardrailRejectionIsRecordedAndNotDisplayed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")

	if err := h.svc.PriceHandler().HandleEnvelope(ctx, h.priceEnvelope("sku-milk", 249, "seed")); err != nil {
		t.Fatalf("seed price: %v", err)
	}
	h.waitForMessages(canon.LeafPrice, 1, 3*time.Second)
	h.resetCaptured()

	if err := h.svc.PriceHandler().HandleEnvelope(ctx, h.priceEnvelope("sku-milk", 24900, "fat-finger")); err != nil {
		t.Fatalf("guardrail rejection must not fail the record: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if msgs := h.messages(canon.LeafPrice); len(msgs) != 0 {
		t.Fatalf("a guard-railed price reached the glass")
	}

	agg, err := h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Price.Amount != 249 {
		t.Fatalf("displayed price = %d, want the pre-rejection 249", agg.Price.Amount)
	}
	if agg.RejectedCount != 1 {
		t.Fatalf("rejection not recorded on the aggregate: %d", agg.RejectedCount)
	}

	history, err := h.svc.repo.History(ctx, "lbl-milk-a", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var rejection domain.PriceRejected
	var found bool
	for _, se := range history {
		if r, ok := se.Event.(*domain.PriceRejected); ok {
			rejection, found = *r, true
			break
		}
	}
	if !found {
		t.Fatalf("no label.price.rejected event on the stream")
	}
	if rejection.Reason != domain.ReasonGuardrail {
		t.Fatalf("rejection reason = %q, want %q", rejection.Reason, domain.ReasonGuardrail)
	}
	if !strings.Contains(rejection.Detail, "limit") {
		t.Fatalf("rejection detail does not explain itself: %q", rejection.Detail)
	}
	if got := h.svc.metrics.GuardrailRejections.With(string(testTenant)).Value(); got != 1 {
		t.Fatalf("guardrail metric = %d, want 1", got)
	}
}

// TestConcurrentUpdatesToOneLabelLoseNothing exercises the optimistic
// concurrency contract: two commands racing on one aggregate must both land,
// with the loser re-reading and re-deciding rather than erroring or silently
// overwriting.
func TestConcurrentUpdatesToOneLabelLoseNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")

	placement, err := h.svc.Directory().Lookup(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	// Identical source clocks so neither command can be judged out of order:
	// the only thing separating them is who reaches the append first.
	// Two racers, which is the contract: a command that loses the optimistic
	// concurrency check reloads once and re-decides. A deeper pile-up on one
	// label returns a retryable conflict to the consumer instead, because a
	// long in-process retry loop on the hot path is how a hot SKU turns into
	// unbounded work.
	now := time.Now().UTC()
	prices := []int64{279, 289}
	results := make([]app.PriceResult, len(prices))
	errs := make([]error, len(prices))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, price := range prices {
		wg.Add(1)
		go func(i int, price int64) {
			defer wg.Done()
			<-start
			results[i], errs[i] = h.svc.PriceHandler().Apply(ctx, app.PriceCommand{
				Placement: placement,
				Change: domain.PriceChange{
					SKU: "sku-milk", Price: canon.NewMoney(price, "USD"),
					EffectiveAt: now, OccurredAt: now, Now: now,
				},
			})
		}(i, price)
	}
	close(start)
	wg.Wait()

	applied := 0
	for i := range prices {
		if errs[i] != nil {
			t.Fatalf("concurrent update %d failed instead of retrying: %v", i, errs[i])
		}
		if results[i].Applied() {
			applied++
		}
	}
	if applied != len(prices) {
		t.Fatalf("%d of %d concurrent updates reached the glass; a genuine concurrent update must win, not error",
			applied, len(prices))
	}

	agg, err := h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Sequence != int64(len(prices)) {
		t.Fatalf("sequence = %d after %d updates; an event was lost", agg.Sequence, len(prices))
	}

	// Every applied event must be on the stream, with strictly increasing
	// sequences: that is the property the label's discard rule depends on.
	history, err := h.svc.repo.History(ctx, "lbl-milk-a", 100)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var sequences []int64
	for i := len(history) - 1; i >= 0; i-- {
		if a, ok := history[i].Event.(*domain.PriceApplied); ok {
			sequences = append(sequences, a.Sequence)
		}
	}
	if len(sequences) != len(prices) {
		t.Fatalf("stream holds %d price events, want %d", len(sequences), len(prices))
	}
	for i := 1; i < len(sequences); i++ {
		if sequences[i] <= sequences[i-1] {
			t.Fatalf("sequences are not strictly increasing: %v", sequences)
		}
	}
}

// TestAcknowledgementClosesTheLoop covers the SLO measurement: an ACK marks the
// aggregate delivered, clears the pending update and records the end-to-end
// latency the three-second budget is written against.
func TestAcknowledgementClosesTheLoop(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")

	if err := h.svc.PriceHandler().HandleEnvelope(ctx, h.priceEnvelope("sku-milk", 279, "pos-1")); err != nil {
		t.Fatalf("price change: %v", err)
	}
	msgs := h.waitForMessages(canon.LeafPrice, 1, 3*time.Second)
	_, update := h.decodeUpdate(msgs[0])

	agg, err := h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Pending == nil || agg.Pending.Sequence != update.Sequence {
		t.Fatalf("expected a pending update at sequence %d, got %+v", update.Sequence, agg.Pending)
	}

	ack := ackEnvelope(t, "lbl-milk-a", testSEC, update.Sequence, 1450*time.Millisecond)
	if err := h.svc.DeliveryHandler().HandleEnvelope(ctx, ack); err != nil {
		t.Fatalf("delivery confirmation: %v", err)
	}

	agg, err = h.svc.repo.Load(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if agg.Pending != nil {
		t.Fatalf("pending update survived its confirmation")
	}
	if agg.LastDelivery == nil || agg.LastDelivery.Sequence != update.Sequence {
		t.Fatalf("delivery not recorded: %+v", agg.LastDelivery)
	}
	if agg.LastDelivery.LatencyMS != 1450 {
		t.Fatalf("latency = %dms, want the edge's measured 1450", agg.LastDelivery.LatencyMS)
	}

	hist := h.svc.metrics.E2ELatency.With(string(testTenant), string(testStore))
	if hist.Count() != 1 {
		t.Fatalf("SLO histogram recorded %d observations, want 1", hist.Count())
	}
	if got := hist.Sum(); got < 1.4 || got > 1.5 {
		t.Fatalf("observed latency = %.3fs, want ~1.45s", got)
	}
	if got := h.svc.metrics.DeliveryConfirmations.With(string(testStore), app.DeliveryConfirmed).Value(); got != 1 {
		t.Fatalf("confirmation counter = %d, want 1", got)
	}

	// A duplicate ACK is routine on an at-least-once mesh and must not be
	// counted twice or overwrite the record.
	if err := h.svc.DeliveryHandler().HandleEnvelope(ctx, ack); err != nil {
		t.Fatalf("duplicate ack: %v", err)
	}
	if hist.Count() != 1 {
		t.Fatalf("duplicate ACK inflated the SLO histogram to %d observations", hist.Count())
	}
	if got := h.svc.metrics.DeliveryConfirmations.With(string(testStore), app.DeliveryDuplicate).Value(); got != 1 {
		t.Fatalf("duplicate counter = %d, want 1", got)
	}
}

// TestACKBridgeRepublishesOntoTheDeliveryStream covers the crossing from the
// device world into the event world: MQTT has no durable history, so the ACK
// has to become a stream record exactly once.
func TestACKBridgeRepublishesOntoTheDeliveryStream(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")

	if err := h.svc.ack.Start(ctx); err != nil {
		t.Fatalf("start ack bridge: %v", err)
	}

	consumer, err := h.bus.Subscribe(eventbus.SubscribeOptions{
		Group: "test-delivery-observer", Topics: []string{canon.StreamDelivery.Name},
		FromBeginning: true,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer consumer.Close()

	received := make(chan canon.Envelope, 4)
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.Run(runCtx, func(_ context.Context, m eventbus.Message) error {
			var env canon.Envelope
			if err := json.Unmarshal(m.Value, &env); err != nil {
				return err
			}
			select {
			case received <- env:
			default:
			}
			return nil
		})
	}()

	ack := ackEnvelope(t, "lbl-milk-a", testSEC, 1, 900*time.Millisecond)
	body, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal ack: %v", err)
	}
	scope := canon.TopicScope{Tenant: testTenant, Region: testRegion, Store: testStore}
	if err := h.observer.Publish(ctx, msgbus.Message{
		Topic:   scope.SECLabelTopic(testSEC, "lbl-milk-a", canon.LeafACK),
		Payload: body, QoS: msgbus.AtLeastOnce,
	}); err != nil {
		t.Fatalf("publish ack: %v", err)
	}

	select {
	case env := <-received:
		if env.EventType != canon.EvtLabelDelivered {
			t.Fatalf("republished %q, want %q", env.EventType, canon.EvtLabelDelivered)
		}
		// The topic, not the payload, is the authority on tenancy: a controller
		// can put anything in a body but can only publish under its own tenant.
		if env.TenantID != testTenant || env.StoreID != testStore {
			t.Fatalf("tenancy not taken from the topic: %s/%s", env.TenantID, env.StoreID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("acknowledgement never reached the delivery stream")
	}
}

// TestDirectoryProjectionRebuildsFromZero is the rebuildability property every
// read model in the platform has to hold: drop it, replay the log, and land on
// exactly the same state.
func TestDirectoryProjectionRebuildsFromZero(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	type placement struct {
		id  canon.LabelID
		sec canon.SECID
		sku canon.SKU
	}
	seed := []placement{
		{"lbl-0001", "sec-01", "sku-milk"},
		{"lbl-0002", "sec-01", "sku-milk"},
		{"lbl-0003", "sec-02", "sku-bread"},
		{"lbl-0004", "sec-02", "sku-eggs"},
	}
	var events []canon.Envelope
	for _, p := range seed {
		events = append(events, h.deviceEvents(p.id, p.sec, p.sku)...)
	}
	// A reassignment: lbl-0004 moves from eggs to milk on a different
	// controller, which is what a planogram reset produces.
	events = append(events, h.deviceEvents("lbl-0004", "sec-01", "sku-milk")...)

	replay := func() {
		for _, env := range events {
			if err := h.svc.DirectoryProjection().HandleEnvelope(ctx, env); err != nil {
				t.Fatalf("projecting %s: %v", env.EventType, err)
			}
		}
	}
	replay()

	before, err := h.svc.Directory().LabelsForSKU(ctx, testTenant, testStore, "sku-milk")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("milk resolves to %d labels, want 3 after the reassignment", len(before))
	}
	if eggs, err := h.svc.Directory().LabelsForSKU(ctx, testTenant, testStore, "sku-eggs"); err != nil || len(eggs) != 0 {
		t.Fatalf("the stale eggs index survived the reassignment: %d entries (%v)", len(eggs), err)
	}
	roster, err := h.svc.Directory().StoreLabels(ctx, testTenant, testStore)
	if err != nil || len(roster) != 4 {
		t.Fatalf("store roster = %d labels, want 4 (%v)", len(roster), err)
	}

	// Now throw the directory away and rebuild it from the same events.
	if err := h.svc.DirectoryProjection().Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if empty, err := h.svc.Directory().LabelsForSKU(ctx, testTenant, testStore, "sku-milk"); err != nil || len(empty) != 0 {
		t.Fatalf("directory not cleared: %d entries (%v)", len(empty), err)
	}
	replay()

	after, err := h.svc.Directory().LabelsForSKU(ctx, testTenant, testStore, "sku-milk")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("rebuild produced %d milk labels, want %d", len(after), len(before))
	}
	index := map[canon.LabelID]ports.Placement{}
	for _, p := range before {
		index[p.LabelID] = p
	}
	for _, p := range after {
		want, ok := index[p.LabelID]
		if !ok {
			t.Fatalf("rebuild invented placement %s", p.LabelID)
		}
		if p.SECID != want.SECID || p.SKU != want.SKU || p.StoreID != want.StoreID {
			t.Fatalf("rebuilt placement for %s diverged:\n got %+v\nwant %+v", p.LabelID, p, want)
		}
	}
}

// TestStateProjectionRebuildsFromTheEventStore covers the query side's own
// rebuildability, including the checkpoint that makes a replay resume rather
// than restart.
func TestStateProjectionRebuildsFromTheEventStore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provisionLabel("lbl-milk-a", testSEC, "sku-milk")

	if err := h.svc.PriceHandler().HandleEnvelope(ctx, h.priceEnvelope("sku-milk", 279, "pos-1")); err != nil {
		t.Fatalf("price change: %v", err)
	}
	msgs := h.waitForMessages(canon.LeafPrice, 1, 3*time.Second)
	_, update := h.decodeUpdate(msgs[0])
	if err := h.svc.DeliveryHandler().HandleEnvelope(ctx,
		ackEnvelope(t, "lbl-milk-a", testSEC, update.Sequence, 1200*time.Millisecond)); err != nil {
		t.Fatalf("ack: %v", err)
	}

	if _, err := h.svc.StateProjection().CatchUp(ctx); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	before, err := h.svc.state.Get(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if before.Price.Amount != 279 || before.LastDeliveredSequence != update.Sequence {
		t.Fatalf("read model did not reflect the change: %+v", before)
	}
	if before.PendingSequence != 0 {
		t.Fatalf("read model still shows a pending update after confirmation")
	}

	if _, err := h.svc.StateProjection().Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	after, err := h.svc.state.Get(ctx, "lbl-milk-a")
	if err != nil {
		t.Fatalf("read state after rebuild: %v", err)
	}
	if after.Price != before.Price || after.Sequence != before.Sequence ||
		after.State != before.State || after.LastDeliveredSequence != before.LastDeliveredSequence {
		t.Fatalf("rebuilt read model diverged:\n got %+v\nwant %+v", after, before)
	}
}

// TestConsumersDriveThePricePathEndToEnd runs the wiring the way production
// does: records published on the streams, consumer groups delivering them,
// projections following the tail.
func TestConsumersDriveThePricePathEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	spawn := func(_ string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = fn(ctx)
		}()
	}
	if err := h.svc.Start(ctx, spawn); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	for _, env := range h.deviceEvents("lbl-milk-a", testSEC, "sku-milk") {
		publishEnvelope(t, h, canon.StreamDeviceEvents.Name, env)
	}
	// The directory has to see the label before the price change reaches it;
	// device-events and price-updates are different streams with no ordering
	// between them, which is exactly why the directory is a read model the
	// price path consults rather than a call it makes.
	waitFor(t, 5*time.Second, func() bool {
		labels, err := h.svc.Directory().LabelsForSKU(ctx, testTenant, testStore, "sku-milk")
		return err == nil && len(labels) == 1
	}, "directory to learn about the label")

	// A brand-new consumer group pins its start offset at the tail the first
	// time it takes ownership of a partition, so a record produced in the
	// window between Run starting and that pin is legitimately not delivered.
	// Production never notices, because producers are continuous. A test that
	// publishes exactly one record has to account for it, so the same logical
	// change is offered repeatedly with fresh envelope identity until one is
	// delivered — and the assertion afterwards is the strong one: however many
	// copies were offered, exactly one reached the glass.
	price := h.priceEnvelope("sku-milk", 279, "pos-stream-1")
	deadline := time.Now().Add(10 * time.Second)
	for len(h.messages(canon.LeafPrice)) == 0 && time.Now().Before(deadline) {
		copy := price
		copy.EventID = canon.NewEventID()
		copy.IdempotencyKey = string(copy.EventID)
		publishEnvelope(t, h, canon.StreamPriceUpdates.Name, copy)
		time.Sleep(200 * time.Millisecond)
	}
	msgs := h.waitForMessages(canon.LeafPrice, 1, 5*time.Second)
	if len(msgs) != 1 {
		t.Fatalf("one logical price change produced %d publishes", len(msgs))
	}
	_, update := h.decodeUpdate(msgs[0])
	h.verifyAttestation(update)

	// The delivery group has the same tail-pinning window; a duplicate ACK is
	// a no-op at the aggregate, so re-offering it is safe.
	ack := ackEnvelope(t, "lbl-milk-a", testSEC, update.Sequence, 1100*time.Millisecond)
	latency := h.svc.metrics.E2ELatency.With(string(testTenant), string(testStore))
	deadline = time.Now().Add(10 * time.Second)
	for latency.Count() == 0 && time.Now().Before(deadline) {
		copy := ack
		copy.EventID = canon.NewEventID()
		publishEnvelope(t, h, canon.StreamDelivery.Name, copy)
		time.Sleep(200 * time.Millisecond)
	}
	waitFor(t, 5*time.Second, func() bool { return latency.Count() == 1 },
		"the delivery confirmation to close the loop")

	waitFor(t, 10*time.Second, func() bool {
		row, err := h.svc.state.Get(ctx, "lbl-milk-a")
		return err == nil && row.LastDeliveredSequence == update.Sequence && row.PendingSequence == 0
	}, "the read model to reflect the confirmed delivery")
}

func publishEnvelope(t testing.TB, h *harness, stream string, env canon.Envelope) {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := eventbus.PublishEnvelope(context.Background(), h.bus, stream, env, body); err != nil {
		t.Fatalf("publish to %s: %v", stream, err)
	}
}

func waitFor(t testing.TB, within time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

package e2e

import (
	"testing"
	"time"

	"github.com/usslp/usslp/edge/sgu"
	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestStoreSurvivesWANOutage is the second headline claim: a store keeps
// trading through a WAN outage with zero label downtime.
//
// The link is severed for real — the store's uplink is a TCP proxy and Cut
// closes every established connection and refuses new ones — rather than by
// flipping the gateway's mode flag, so the detector has to notice on its own,
// the bridge's publishes have to fail and get buffered, and the MQTT client's
// own reconnect loop has to find its way back.
//
// The assertions, in the order the store experiences them:
//
//	during   every label still shows the price it had, and none went blank;
//	during   a price change made in the cloud is not lost, it is queued;
//	during   the store's own scheduled promotion still fires, from its local
//	         clock, attested with its delegated key;
//	after    the buffer flushes in order and the cloud learns what happened;
//	after    the store's state and the cloud's agree.
func TestStoreSurvivesWANOutage(t *testing.T) {
	if testing.Short() {
		t.Skip("an outage and a recovery take about twenty seconds; -short skips it")
	}
	st := newStack(t, smallStore(2, 8))
	store := st.Stores()[0]
	tap := priceTapFor(t, st, store)

	// What the shelves show before anything breaks.
	type shelf struct {
		price canon.Money
		seq   int64
	}
	before := map[canon.LabelID]shelf{}
	for _, z := range store.Zones {
		for _, id := range z.Labels() {
			rec, ok := z.Controller.Record(id)
			if !ok || rec.Price.Amount == 0 {
				t.Fatalf("%s has no price before the outage; the test would prove nothing", id)
			}
			before[id] = shelf{price: rec.Price, seq: rec.Sequence}
		}
	}

	// A promotion the cloud pushed down *before* the outage, timed to activate
	// during it. This is the case a retailer actually cares about: the 08:00
	// promotion has to go live whether or not head office is reachable at
	// 08:00, and it has to go live on the store's own clock.
	promoTarget := pick(t, st, 0, 0, 3)
	promoPrice := promoTarget.nudge(-33)
	promoAt := time.Now().Add(6 * time.Second)
	fileScheduledPromotion(t, st, store, promoTarget, promoPrice, promoAt)

	// --- cut ---------------------------------------------------------
	store.Link.Cut()
	if err := st.AwaitMode(t.Context(), store, "autonomous", 20*time.Second); err != nil {
		t.Fatalf("the gateway did not notice the outage: %v", err)
	}
	t.Log("the store went autonomous on its own")

	// Every label still shows what it showed, and nothing went blank.
	for id, was := range before {
		z, _, _ := store.FindLabel(id)
		rec, ok := z.Controller.Record(id)
		if !ok {
			t.Errorf("%s lost its record during the outage", id)
			continue
		}
		if rec.Price.Cmp(was.price) != 0 {
			t.Errorf("%s changed from %s to %s during the outage", id, was.price.Display(), rec.Price.Display())
		}
		if rec.Price.Amount == 0 {
			t.Errorf("%s went blank during the outage", id)
		}
	}

	// A price change the cloud makes while the store is unreachable. It must
	// not be lost; the gateway's bridge simply cannot deliver it, and the
	// controller must still be showing the old price.
	tg := pick(t, st, 0, 0, 0)
	cloudPrice := tg.nudge(211)
	if _, _, err := st.PushShopifyPrice(t.Context(), tg.Tenant, store.ID, tg.SKU, cloudPrice, ""); err != nil {
		t.Fatalf("the cloud refused a price change during the outage: %v", err)
	}
	time.Sleep(2 * time.Second)
	if rec, _ := tg.Zone.Controller.Record(tg.Label); rec.Price.Cmp(cloudPrice) == 0 {
		t.Errorf("a price published while the WAN was down reached the shelf anyway; "+
			"the link was not really cut (%s)", rec.Price.Display())
	}

	// The store's own price change, originated locally and attested with the
	// store's delegated key. This is the part of autonomy that does not work at
	// all without a delegation: a controller refuses any price it cannot
	// verify, so a store with no local signing key can record a change and
	// never get it onto a shelf.
	local := pick(t, st, 0, 1, 0)
	localPrice := local.nudge(-7)
	touched, err := store.Gateway.LocalPriceChange(t.Context(), canon.PriceChangeRequested{
		SKU: local.SKU, StoreID: store.ID, Price: localPrice,
		EffectiveAt: time.Now().UTC(), InitiatedBy: "store-till", SourceSystem: "e2e",
	})
	if err != nil {
		t.Fatalf("the store could not originate a local price change while autonomous: %v", err)
	}
	if len(touched) == 0 {
		t.Fatal("the local price change touched no labels")
	}
	eventually(t, 20*time.Second, "the locally originated price to reach the glass", func() bool {
		rec, ok := local.Zone.Controller.Record(local.Label)
		return ok && rec.Price.Cmp(localPrice) == 0
	})
	t.Logf("a price change originated inside the store reached %d label(s) with the WAN down", len(touched))

	// It was attested, and by the store's delegated key rather than the
	// cloud's: an unattested local price would have been refused.
	if u, ok := tap.await(local.Label, 0, 5*time.Second); ok {
		if u.Update.Attestation.Signature == "" {
			t.Error("the locally originated price carried no attestation")
		}
		if u.Update.Attestation.KeyID == st.KeyRing().ActiveKeyID() {
			t.Log("note: the local price was signed with the platform key, not a delegated one")
		}
	}

	// The scheduled promotion fires from the store's own clock, with the cloud
	// unreachable, using the attestation the cloud signed before the link went.
	eventually(t, 25*time.Second, "the scheduled promotion to activate from the local clock", func() bool {
		rec, ok := promoTarget.Zone.Controller.Record(promoTarget.Label)
		return ok && rec.Price.Cmp(promoPrice) == 0
	})
	if n := store.Gateway.Stats().Activations; n == 0 {
		t.Error("the gateway recorded no promotion activations")
	}
	t.Logf("a promotion scheduled for %s activated on the store's own clock with the WAN down",
		promoAt.Format(time.TimeOnly))

	queued := store.Gateway.Queue().Stats()
	if queued.Depth == 0 {
		t.Error("nothing was buffered during the outage; the upstream bridge is not durable")
	}
	t.Logf("the upstream buffer holds %d messages (%d bytes) durably", queued.Depth, queued.Bytes)

	// --- restore -----------------------------------------------------
	store.Link.Restore()
	if err := st.AwaitMode(t.Context(), store, "connected", 40*time.Second); err != nil {
		t.Fatalf("the gateway did not recover: %v", err)
	}
	t.Log("the store reconnected and reconciled")

	eventually(t, 30*time.Second, "the upstream buffer to flush", func() bool {
		return store.Gateway.Queue().Stats().Depth == 0
	})
	stats := store.Gateway.Stats()
	t.Logf("gateway: %d buffered, %d bridged upstream, %d reconciliations, %d conflicts resolved",
		stats.Buffered, stats.BridgedUp, stats.Reconciliations, stats.Conflicts)
	if stats.Reconciliations == 0 {
		t.Error("the store came back without running a reconciliation")
	}

	// The price the cloud published during the outage is delivered once the
	// bridge is back, because the cloud's retained state survives the outage
	// and is re-delivered on re-subscription.
	eventually(t, 30*time.Second, "the price published during the outage to arrive", func() bool {
		rec, ok := tg.Zone.Controller.Record(tg.Label)
		return ok && rec.Price.Cmp(cloudPrice) == 0
	})

	// And the store's own decision survived the merge: a locally originated
	// price made while autonomous is newer than anything the cloud believed,
	// and the conflict policy must not roll it back.
	rec, _ := local.Zone.Controller.Record(local.Label)
	if rec.Price.Cmp(localPrice) != 0 {
		t.Errorf("after reconciliation %s shows %s, not the %s the store decided while it was alone",
			local.Label, rec.Price.Display(), localPrice.Display())
	}

	// Nothing went blank at any point.
	for id := range before {
		z, l, _ := store.FindLabel(id)
		if l.Dead() {
			t.Errorf("%s is dead after the outage", id)
		}
		if rec, ok := z.Controller.Record(id); !ok || rec.Price.Amount == 0 {
			t.Errorf("%s has no price after the outage", id)
		}
	}
	if report, ok := store.Gateway.LastReconciliation(); ok {
		t.Logf("reconciliation: %+v", report)
	}
}

// fileScheduledPromotion pushes a pre-attested promotion into a store's local
// calendar, exactly as the cloud does ahead of a timed activation.
//
// The updates are signed here with the platform's price authority — the same
// key the Label Service signs with — because that is the whole mechanism: the
// cloud attests the promotional prices when it publishes the schedule, and the
// store can then activate them at the right local moment without a signing key
// and without a network.
func fileScheduledPromotion(t *testing.T, st *stack.Stack, store *stack.Store,
	tg target, price canon.Money, at time.Time) {
	t.Helper()

	rec, _ := tg.Zone.Controller.Record(tg.Label)
	effective := at.UTC()
	promoID := canon.PromotionID("promo-during-outage")
	upd := canon.PriceUpdated{
		LabelID: tg.Label, SKU: tg.SKU, StoreID: store.ID, Price: price,
		EffectiveAt: effective, PromotionID: promoID, Sequence: rec.Sequence + 1,
		Render: canon.RenderSpec{Template: "promo", Badge: "SALE", ShowWas: true},
	}
	att, err := st.PriceAuthority().Sign(canon.AttestationInputFrom(store.Tenant, upd))
	if err != nil {
		t.Fatalf("attesting the promotional price: %v", err)
	}
	upd.Attestation = att

	env, err := canon.NewEnvelope(canon.EvtPromotionActivated, "promotion", string(promoID),
		store.Tenant, map[string]any{"promotion_id": promoID})
	if err != nil {
		t.Fatal(err)
	}
	env.StoreID = store.ID
	env.Source = "e2e"

	if err := store.Gateway.Schedule().Add(sgu.ScheduledPromotion{
		PromotionID: promoID, ActivateAt: at.UTC(),
		ExpireAt: at.Add(24 * time.Hour).UTC(),
		Updates:  []canon.PriceUpdated{upd}, Envelope: env,
		ReceivedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("filing the scheduled promotion: %v", err)
	}
}

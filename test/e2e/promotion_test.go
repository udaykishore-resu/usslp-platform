package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestStoreWidePromotionFansOut activates a promotion that touches every label
// in a store and asserts that every affected label updated, that none of the
// unaffected ones did, and reports how long the whole fan-out took.
//
// The exclusion half is the half that matters. A promotion that repriced
// everything would also pass an "every affected label updated" assertion, so
// one controller's worth of shelf is deliberately excluded and checked to have
// been left alone.
func TestStoreWidePromotionFansOut(t *testing.T) {
	if testing.Short() {
		t.Skip("a store-wide fan-out is a hundred E-Ink refreshes; -short skips it")
	}
	st := newStack(t, smallStore(3, 12))
	store := st.Stores()[0]

	// Everything on the first two controllers is in the promotion; the third
	// controller's shelf is the control group.
	var included []canon.SKU
	var excluded []canon.SKU
	for i, z := range store.Zones {
		for _, id := range z.Labels() {
			sku, _ := store.SKUOf(id)
			if i < 2 {
				included = append(included, sku)
			} else {
				excluded = append(excluded, sku)
			}
		}
	}
	if len(included) == 0 || len(excluded) == 0 {
		t.Fatal("the store did not split into an included and an excluded set")
	}

	// A snapshot of what every label is showing, taken before the activation —
	// including the panel's own refresh counter, because "how many times did
	// the glass move" is an assertion in its own right further down.
	type snapshot struct {
		price     canon.Money
		seq       int64
		refreshes int64
	}
	before := map[canon.LabelID]snapshot{}
	for _, z := range store.Zones {
		for _, id := range z.Labels() {
			rec, _ := z.Controller.Record(id)
			snap := snapshot{price: rec.Price, seq: rec.Sequence}
			if l, ok := z.Sim.Label(id); ok {
				snap.refreshes = l.Stats().RefreshCount
			}
			before[id] = snap
		}
	}

	// The tap watches the store's own broker, so the assertions below are made
	// against the bytes the controllers actually received rather than against
	// anything the cloud says it sent. It is attached before the activation
	// because a subscription made afterwards would miss the fan-out.
	tap := priceTapFor(t, st, store)

	now := time.Now().UTC()
	start, end := now.Add(-time.Hour), now.Add(24*time.Hour)
	const promoID = canon.PromotionID("promo-storewide")
	// The promotion is authored through the Promotion Service's own HTTP API
	// rather than by constructing a rule in Go. That is not squeamishness about
	// internal packages: it is the surface a merchandiser's tooling uses, so a
	// rule this test can express is a rule a customer can express.
	createPromotion(t, st, store.Tenant, map[string]any{
		"id": promoID, "tenant_id": store.Tenant,
		"name": "20 percent off two aisles", "type": "PERCENTAGE_OFF",
		"priority": 10,
		"params":   map[string]any{"percent_off": 20, "currency": "USD"},
		"conditions": map[string]any{
			"stores": []canon.StoreID{store.ID}, "include_skus": included,
		},
		"display": map[string]any{
			"badge": "SALE", "led_color": "RED", "show_original_price": true, "template": "promo",
		},
		"schedule": map[string]any{
			"absolute_start": start.Format(time.RFC3339Nano),
			"absolute_end":   end.Format(time.RFC3339Nano),
		},
		"created_by": "e2e",
	})

	// Activation goes through the Promotion Service and nothing else. The
	// Label Service's own `label-service.promotions` consumer picks the event
	// off `promotion-events`, resolves the affected labels with the promotion
	// domain's compiled matcher, and drives them through the batch fan-out.
	// Nothing in usslpd is on that path, which is what makes this test evidence
	// that the wiring exists rather than evidence that the test can call a
	// function.
	activatedAt, err := st.ActivatePromotion(t.Context(), store.Tenant, promoID, "e2e")
	if err != nil {
		t.Fatalf("activating the promotion: %v", err)
	}
	fanout, err := st.AwaitPromotion(t.Context(), store.Tenant, promoID,
		len(included), activatedAt, 60*time.Second)
	if err != nil {
		t.Fatalf("the promotion never reached the shelves: %v", err)
	}
	// The fan-out is complete when every label holds the promotion; the shelves
	// are finished when the last panel has settled.
	if err := st.AwaitQuiet(t.Context(), 90*time.Second); err != nil {
		t.Fatalf("the store never settled after the promotion: %v", err)
	}
	elapsed := time.Since(activatedAt)
	// Re-read once the panels have settled: AwaitPromotion returns when the
	// controllers hold the promotion, which is a little before the glass shows
	// it, so the confirmed count taken at that moment is always zero and always
	// meaningless.
	settled := st.PromotionFanout(store.Tenant, promoID)

	t.Logf("promotion touched %d shelf positions across %d store(s): "+
		"%d labels carry it, %d have confirmed the pixels; "+
		"reached the controllers in %dms, every panel settled in %s",
		len(included), len(fanout.Stores), settled.Labels, settled.Displayed,
		fanout.DurationMS, elapsed.Round(time.Millisecond))
	if settled.Displayed != settled.Labels {
		t.Errorf("%d of %d labels under the promotion have not confirmed the pixels changed",
			settled.Labels-settled.Displayed, settled.Labels)
	}

	if fanout.Labels == 0 {
		t.Fatal("the promotion changed no prices at all")
	}

	// Every included label moved, and moved to 80% of what it was showing.
	changed, unchanged := 0, 0
	extraRefreshes, ledRed, ledOther := 0, 0, 0
	for i, z := range store.Zones {
		for _, id := range z.Labels() {
			rec, _ := z.Controller.Record(id)
			was := before[id]
			refreshes := int64(0)
			if l, ok := z.Sim.Label(id); ok {
				refreshes = l.Stats().RefreshCount - was.refreshes
			}
			if i < 2 {
				if rec.Sequence <= was.seq {
					t.Errorf("%s is still on sequence %d: the promotion did not reach it", id, rec.Sequence)
					continue
				}
				want := was.price.PercentOff(20)
				if rec.Price.Cmp(want) != 0 {
					t.Errorf("%s shows %s; 20%% off %s is %s",
						id, rec.Price.Display(), was.price.Display(), want.Display())
				}
				if rec.PromotionID != promoID {
					t.Errorf("%s carries promotion %q, not %q", id, rec.PromotionID, promoID)
				}
				// One activation, one waveform.
				//
				// A full E-Ink refresh is roughly a hundred times the energy of
				// anything else a label does, so a fan-out that repaints twice
				// costs a fleet a measurable share of its seven-year battery
				// budget for nothing. It is also the specific symptom of two
				// components both deciding what a promotion means: one applies
				// its answer, the other applies a different one, and the label
				// obediently draws both.
				if refreshes != 1 {
					extraRefreshes++
					t.Errorf("%s ran %d waveform(s) for one activation; a promotion is one price change",
						id, refreshes)
				}
				// The authored display block, verbatim on the wire.
				//
				// DecideRender would derive a green LED for any promotional
				// price — green is what a merchandising sweep looks for — and
				// this rule authored RED. The rule wins: "a red LED is the
				// colour the aisle-walk was briefed to look for", and a
				// platform that substituted its own taste would be overriding a
				// decision a merchandiser made and put in a campaign brief.
				if tu, ok := tap.latestFor(id); ok {
					switch tu.Update.Render.LEDColor {
					case "RED":
						ledRed++
					default:
						ledOther++
						t.Errorf("%s was sent led_color %q; the rule authored RED, and the "+
							"derived default for a promotion is GREEN — the authored display "+
							"block is being dropped somewhere between the rule and the wire",
							id, tu.Update.Render.LEDColor)
					}
					if tu.Update.WasPrice == nil {
						t.Errorf("%s was sent no was-price; the rule asked for the original "+
							"price to be shown", id)
					}
					if tu.Update.Render.Badge != "SALE" {
						t.Errorf("%s was sent badge %q, not the authored SALE",
							id, tu.Update.Render.Badge)
					}
				}
				changed++
				continue
			}
			if rec.Sequence != was.seq || rec.Price.Cmp(was.price) != 0 {
				t.Errorf("%s was not in the promotion but moved from %s to %s",
					id, was.price.Display(), rec.Price.Display())
			}
			if refreshes != 0 {
				t.Errorf("%s was not in the promotion but ran %d waveform(s)", id, refreshes)
			}
			unchanged++
		}
	}
	t.Logf("%d labels repriced, %d left alone; %d ran more or fewer than one waveform; "+
		"%d were sent the authored RED led_color, %d something else",
		changed, unchanged, extraRefreshes, ledRed, ledOther)
	if changed != len(included) {
		t.Errorf("%d of %d included labels were repriced", changed, len(included))
	}
	if ledRed == 0 {
		t.Error("not one label was sent the authored led_color; nothing was asserted about it")
	}
}

// TestOverlappingPromotionsResolveLastActivationWins pins down a documented
// consequence of the shelf tier reading the rule rather than the resolution.
//
// The Promotion Service owns arbitration: priority, stacking and exclusive
// groups are settled by promodomain.Resolve against the whole active set, and
// only that service holds the whole active set. `promotion-events` carries the
// *rule*, deliberately — a national activation must not become two thousand
// stores' worth of simultaneous lookups — so the Label Service's consumer sees
// one rule at a time and has no basis on which to arbitrate. A partial arbiter
// there would be a second pricing engine that eventually disagrees with the
// first.
//
// The consequence is that where two promotions overlap, the shelf shows the one
// activated most recently, because it takes the higher per-label sequence —
// *even when the other has the higher priority*. That is a real behaviour with
// a real cost, and the reason to write it down in a test is that it is the kind
// of thing that otherwise gets discovered by a merchandiser wondering why the
// 30%-off campaign they set to priority 100 is showing 10% off. Making the
// shelf honour full conflict resolution needs Resolve's output on the event,
// not more logic in the consumer.
//
// What the shelf does *not* do is compound them: each activation prices from
// the label's stored everyday price, so the outcome is always a price one of
// the two promotions authorises. Picking the wrong one of two legitimate prices
// is a merchandising complaint; inventing a third is a pricing incident, and
// the second half of this test is what keeps the first kind from becoming the
// second.
//
// If that ever changes, this test fails — which is the point.
func TestOverlappingPromotionsResolveLastActivationWins(t *testing.T) {
	if testing.Short() {
		t.Skip("two store-wide fan-outs; -short skips it")
	}
	st := newStack(t, smallStore(1, 6))
	store := st.Stores()[0]

	var skus []canon.SKU
	for _, id := range store.Labels() {
		sku, _ := store.SKUOf(id)
		skus = append(skus, sku)
	}
	if len(skus) == 0 {
		t.Fatal("the store has no shelves")
	}

	now := time.Now().UTC()
	start, end := now.Add(-time.Hour), now.Add(24*time.Hour)
	schedule := map[string]any{
		"absolute_start": start.Format(time.RFC3339Nano),
		"absolute_end":   end.Format(time.RFC3339Nano),
	}
	// The high-priority rule is the deeper discount, so that a platform which
	// did arbitrate would visibly land somewhere else: 30% off is not 10% off,
	// and neither is a stack of the two.
	const senior = canon.PromotionID("promo-senior")
	const junior = canon.PromotionID("promo-junior")
	createPromotion(t, st, store.Tenant, map[string]any{
		"id": senior, "tenant_id": store.Tenant, "name": "30 percent off, priority 100",
		"type": "PERCENTAGE_OFF", "priority": 100,
		"params":     map[string]any{"percent_off": 30, "currency": "USD"},
		"conditions": map[string]any{"stores": []canon.StoreID{store.ID}, "include_skus": skus},
		"schedule":   schedule, "created_by": "e2e",
	})
	createPromotion(t, st, store.Tenant, map[string]any{
		"id": junior, "tenant_id": store.Tenant, "name": "10 percent off, priority 1",
		"type": "PERCENTAGE_OFF", "priority": 1,
		"params":     map[string]any{"percent_off": 10, "currency": "USD"},
		"conditions": map[string]any{"stores": []canon.StoreID{store.ID}, "include_skus": skus},
		"schedule":   schedule, "created_by": "e2e",
	})

	base := map[canon.LabelID]canon.Money{}
	for _, z := range store.Zones {
		for _, id := range z.Labels() {
			rec, _ := z.Controller.Record(id)
			base[id] = rec.Price
		}
	}

	// The senior rule first, then the junior one on top of it.
	seniorAt, err := st.ActivatePromotion(t.Context(), store.Tenant, senior, "e2e")
	if err != nil {
		t.Fatalf("activating %s: %v", senior, err)
	}
	if _, err := st.AwaitPromotion(t.Context(), store.Tenant, senior,
		len(skus), seniorAt, 60*time.Second); err != nil {
		t.Fatalf("the priority-100 promotion never reached the shelves: %v", err)
	}
	if err := st.AwaitQuiet(t.Context(), 60*time.Second); err != nil {
		t.Fatalf("the store never settled: %v", err)
	}

	juniorAt, err := st.ActivatePromotion(t.Context(), store.Tenant, junior, "e2e")
	if err != nil {
		t.Fatalf("activating %s: %v", junior, err)
	}
	last, err := st.AwaitPromotion(t.Context(), store.Tenant, junior,
		len(skus), juniorAt, 60*time.Second)
	if err != nil {
		t.Fatalf("the priority-1 promotion never reached the shelves: %v", err)
	}
	if err := st.AwaitQuiet(t.Context(), 60*time.Second); err != nil {
		t.Fatalf("the store never settled: %v", err)
	}

	// Both promotions are still active in the Promotion Service, and the one
	// the shelf is showing is not the one that service would resolve to. That
	// gap is the finding, and it is stated here so the log says it out loud.
	t.Logf("after both activations %d label(s) carry %s, the one activated last — "+
		"even though %s is still active at priority 100 and would win "+
		"promodomain.Resolve", last.Labels, junior, senior)

	for _, z := range store.Zones {
		for _, id := range z.Labels() {
			rec, _ := z.Controller.Record(id)
			if rec.PromotionID != junior {
				t.Errorf("%s carries %q; the documented behaviour is that the most "+
					"recently activated promotion wins at the shelf, which is %q",
					id, rec.PromotionID, junior)
				continue
			}
			// Ten percent off the *everyday* price, not off the price the
			// senior promotion had already set. This is the other half of the
			// behaviour and it is the reassuring half: the handler discounts
			// from the label's stored base price precisely so that two
			// promotions replace each other instead of compounding. Without it
			// the divergence above would not merely pick the wrong promotion,
			// it would invent a price neither promotion authorises — and a
			// shopper who is undercharged by a stacked discount is a refund,
			// while one who is overcharged is a regulator.
			if want := base[id].PercentOff(10); rec.Price.Cmp(want) != 0 {
				t.Errorf("%s shows %s; ten percent off the everyday price of %s is %s "+
					"(a compounded 30-then-10 would be %s)",
					id, rec.Price.Display(), base[id].Display(), want.Display(),
					base[id].PercentOff(30).PercentOff(10).Display())
			}
		}
	}
}

// createPromotion authors a promotion through the Promotion Service's HTTP API.
//
// The tenant travels in X-USSLP-Tenant, which in production is set by the API
// Gateway after it has authenticated the caller and derived the tenant from its
// credential. Calling the service directly here is calling it the way the
// gateway does.
func createPromotion(t *testing.T, st *stack.Stack, tenant canon.TenantID, rule map[string]any) {
	t.Helper()
	body, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("encoding the promotion: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		st.Services().PromotionURL()+"/v1/promotions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-USSLP-Tenant", string(tenant))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("creating the promotion: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the promotion service refused the rule: %s: %s", resp.Status, bytes.TrimSpace(got))
	}
}

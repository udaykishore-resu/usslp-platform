package stack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	labelapp "github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/uig/adapters/shopify"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// priceBook is the everyday, pre-promotion price of every product on the
// generated shelves.
//
// In a real deployment this is the retailer's merchandising system and the
// platform never guesses at it: a promotion is a discount *from* something, and
// that something is the retailer's. usslpd generates a store, so it also
// generates the price book, and records it here so the demo has a number to
// change and anything measuring a price movement has something to measure it
// from.
type priceBook struct {
	mu     sync.RWMutex
	prices map[canon.StoreID]map[canon.SKU]canon.Money
}

func newPriceBook() *priceBook {
	return &priceBook{prices: map[canon.StoreID]map[canon.SKU]canon.Money{}}
}

func (p *priceBook) set(store canon.StoreID, sku canon.SKU, m canon.Money) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prices[store] == nil {
		p.prices[store] = map[canon.SKU]canon.Money{}
	}
	p.prices[store][sku] = m
}

func (p *priceBook) get(store canon.StoreID, sku canon.SKU) (canon.Money, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	m, ok := p.prices[store][sku]
	return m, ok
}

// basePrice is the everyday price of a product in a store.
func (s *Stack) basePrice(store canon.StoreID, sku canon.SKU) (canon.Money, bool) {
	return s.book.get(store, sku)
}

// BasePrice is the exported form, for the demo and the tests.
func (s *Stack) BasePrice(store canon.StoreID, sku canon.SKU) (canon.Money, bool) {
	return s.book.get(store, sku)
}

// openingPriceFor is the deterministic everyday price of a shelf position.
//
// Prices are spread across a realistic grocery range rather than all being the
// same, because a store where every label shows £1.00 hides the two bugs that
// matter most in rendering: a price that is too wide for the panel, and a
// partial-refresh decision that is correct only when the digits do not change.
func openingPriceFor(sku canon.SKU, currency string) canon.Money {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(sku); i++ {
		h ^= uint64(sku[i])
		h *= 1099511628211
	}
	// 0.49 to 24.98, on realistic .49 and .99 endings. The range matters: the
	// Label Service refuses a change of more than five times the current price
	// as a corrupt feed (domain.DefaultGuardrailFactor), so a price book that
	// spread over three orders of magnitude would make every subsequent change
	// look like a decimal point lost in an ERP.
	minor := int64(49 + h%2450)
	if h%3 == 0 {
		minor += 50
	}
	return canon.NewMoney(minor, currency)
}

// seedOpeningPrices puts a price on every label in every store, through the
// Label Service's own batch fan-out.
//
// A store whose shelves are blank until someone runs a demo command is not a
// store; it is a fixture. Seeding through the real path also means the platform
// has been exercised end to end before the banner is printed, so a run that
// prints "ready" has actually delivered a price to every label on every shelf.
func (s *Stack) seedOpeningPrices(ctx context.Context) error {
	for _, st := range s.stores {
		items := make([]labelapp.BatchItem, 0, st.LabelCount())
		seen := map[canon.SKU]bool{}
		for _, labelID := range st.Labels() {
			sku, ok := st.skuOf[labelID]
			if !ok || seen[sku] {
				continue
			}
			seen[sku] = true
			price := openingPriceFor(sku, s.cfg.Currency)
			s.book.set(st.ID, sku, price)
			items = append(items, labelapp.BatchItem{
				StoreID: st.ID, SKU: sku, Price: price,
				EffectiveAt: time.Now().UTC(),
				Reason:      "opening price book",
				InitiatedBy: "usslpd",
				// A stable key so a restart against an existing data directory
				// re-seeds nothing: the aggregate has already seen this exact
				// decision.
				IdempotencyKey: fmt.Sprintf("opening:%s:%s:%d", st.ID, sku, price.Amount),
			})
		}
		if len(items) == 0 {
			continue
		}
		report, err := s.cloudSvcs.label.Batch().BatchUpdatePrices(ctx, labelapp.BatchRequest{
			TenantID: st.Tenant, Region: canon.Region(s.cfg.Region),
			Items: items, InitiatedBy: "usslpd/opening",
		})
		if err != nil {
			return fmt.Errorf("usslpd: seeding the opening price book for %s: %w", st.ID, err)
		}
		s.bootRT.Log.Info("opening price book applied",
			"store", string(st.ID), "skus", len(items),
			"labels", report.Resolved, "applied", report.Applied, "ms", report.Duration.Milliseconds())
	}
	// The batch returns when the last update has been published to the broker,
	// which is a long way from the last panel having finished its waveform. A
	// runtime that printed "ready" at that point would be handing out a store
	// whose shelves were still turning over, and the first latency anyone
	// measured would be the tail of the opening load rather than their own
	// price change.
	//
	// Waiting for the controllers to go quiet is not sufficient on its own, and
	// the reason is worth stating because it cost an afternoon: the work has to
	// *arrive* before there is anything to be quiet about. Between the batch
	// returning and the first update reaching a coordinator there is a broker
	// publish, a bridge hop and a subscription delivery, and during that window
	// every controller's queue is legitimately empty. A runtime that only
	// waited for quiet would sometimes declare a store open with no price on
	// any shelf — and the symptom, much later, is a caller whose first price
	// change races the opening one for the same sequence number.
	//
	// So: wait for every label to be showing something, then wait for the
	// radios to fall silent.
	// Twenty-four seconds is three attempts of eight, and eight is chosen
	// against the coordinator's 25-second acknowledgement timeout rather than
	// against how long a delivery takes: a delivery takes well under a second,
	// so a label still blank after eight is one whose update is sitting in a
	// retry the radio will not resolve for another twenty. Re-issuing over the
	// top of that is both faster and closer to what an operator does.
	if err := s.awaitShelvesPriced(ctx, 24*time.Second); err != nil {
		return err
	}
	return s.AwaitQuiet(ctx, 90*time.Second)
}

// openingRounds is how many times the opening price book will be re-issued to
// the labels that are still blank before start-up is called a failure.
const openingRounds = 3

// awaitShelvesPriced blocks until every commissioned label is showing a price,
// re-issuing the opening price to any label that is not.
//
// # Why a retry belongs here rather than in a test
//
// A single pass over a hundred labels does not reliably paint a hundred labels.
// The mesh abandons a delivery after three application-level attempts ("mesh:
// link failed after MAC retries"), which is faithful hardware behaviour rather
// than a platform defect: a radio link that is bad for a second is ordinary in
// a store full of trolleys and refrigeration compressors.
//
// The platform's own answer is the one an operator would give — send the price
// again — and the Label Service treats a fresh EffectiveAt as a new decision
// rather than a no-op, so the re-issue allocates a new sequence and gets a
// fresh set of radio attempts. Doing it here, in the composition root, is what
// lets every test downstream begin from a store whose shelves are all genuinely
// showing a price, which is the state a real store opens its doors in.
func (s *Stack) awaitShelvesPriced(ctx context.Context, within time.Duration) error {
	// openingDwell is the shortest a round may be. A re-issued price needs a
	// cloud hop, a broker publish, a bridge and a radio transmission before the
	// controller has anything new to report, and during that window every blank
	// label still carries the *previous* attempt's failure. Without a floor the
	// runtime would read that stale evidence as "abandoned again" and spend all
	// three attempts inside a millisecond.
	const openingDwell = 3 * time.Second

	deadline := time.Now().Add(within)
	roundStart := time.Now()
	roundEnd := roundStart.Add(within / openingRounds)
	reissues := 0
	for {
		missing, abandoned := s.unpricedLabels()
		if len(missing) == 0 {
			if reissues > 0 {
				s.bootRT.Log.Info("opening price book completed after a re-issue",
					"rounds", reissues+1)
			}
			return nil
		}
		example := missing[0].label
		stuck := fmt.Errorf("usslpd: %d label(s) still had no price after %d attempt(s) "+
			"(for example %s: %s); the opening price book did not reach the shelves",
			len(missing), reissues+1, example, s.describeLabel(example))
		now := time.Now()
		if now.After(deadline) {
			return stuck
		}
		// The round timer is only a backstop. Normally the controllers say so
		// themselves: every blank label has a recorded delivery failure and its
		// zone's radio is idle, so nothing more is coming and there is nothing
		// to be gained by waiting out the clock.
		if now.After(roundEnd) || (abandoned && now.Sub(roundStart) >= openingDwell) {
			if reissues >= openingRounds-1 {
				return stuck
			}
			reissues++
			s.bootRT.Log.Warn("re-issuing the opening price to labels that are still blank",
				"labels", len(missing), "attempt", reissues+1, "example", string(example),
				"why", s.describeLabel(example))
			if err := s.reissueOpeningPrices(ctx, missing, reissues); err != nil {
				return err
			}
			roundStart = time.Now()
			roundEnd = roundStart.Add(within / openingRounds)
		}
		select {
		case <-ctx.Done():
			// The parent deadline, not this one. Reporting what was still
			// missing is the difference between a diagnosable failure and
			// "context deadline exceeded".
			return fmt.Errorf("%w: %w", stuck, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// blankLabel is one label the platform has priced and the glass has not caught.
type blankLabel struct {
	store *Store
	label canon.LabelID
}

// unpricedLabels lists every commissioned label whose glass is still blank, and
// reports whether the controllers have stopped trying to paint them.
//
// # Why the panel and not the controller
//
// The obvious test is the controller's DisplayedSequence — the highest sequence
// a label acknowledged — and it is the wrong one, because it conflates two
// different things: the pixels having changed, and the controller having heard
// about it. The 802.15.4 model drops acknowledgements as readily as it drops
// updates, so a label can be correctly showing sequence 1 while its controller
// still reports 0 and retransmits. Waiting on the controller's view would make
// start-up fail over a store whose shelves are, in fact, all correct.
//
// The question this gate is asked is "are the shelves showing prices", so it
// asks the shelves. sec.Controller keeps the reconciling view — a sequence that
// went unacknowledged shows up as a delivery failure and, eventually, as a
// device the registry marks unreachable, which is the behaviour a store
// manager's exception report is built on and is deliberately left alone here.
//
// "Stopped trying" means the zone's radio has nothing queued and nothing in
// flight: no further attempt is coming, so a caller waiting for one of these to
// arrive on its own would wait forever.
func (s *Stack) unpricedLabels() (missing []blankLabel, abandoned bool) {
	abandoned = true
	for _, st := range s.stores {
		for _, z := range st.Zones {
			if z.Controller == nil {
				continue
			}
			var idle bool
			if z.Coordinator != nil {
				cs := z.Coordinator.Stats()
				idle = cs.Queued == 0 && cs.InFlight == 0
			}
			for _, id := range z.Labels() {
				l, ok := z.Sim.Label(id)
				if ok && l.Stats().Sequence >= 1 {
					continue
				}
				missing = append(missing, blankLabel{store: st, label: id})
				if !idle {
					abandoned = false
				}
			}
		}
	}
	return missing, abandoned && len(missing) > 0
}

// reissueOpeningPrices sends the opening price again, to just the labels that
// are still blank, through the same batch path the first pass used.
//
// The idempotency key carries the attempt number and the effective time is now:
// the point is deliberately to make a *new* decision the aggregate will act on,
// because the previous one is recorded as applied in the cloud and only the
// last hop failed.
func (s *Stack) reissueOpeningPrices(ctx context.Context, missing []blankLabel, attempt int) error {
	byStore := map[*Store][]labelapp.BatchItem{}
	for _, m := range missing {
		sku, ok := m.store.skuOf[m.label]
		if !ok {
			continue
		}
		price, ok := s.book.get(m.store.ID, sku)
		if !ok {
			continue
		}
		byStore[m.store] = append(byStore[m.store], labelapp.BatchItem{
			StoreID: m.store.ID, SKU: sku, Price: price,
			EffectiveAt: time.Now().UTC(),
			Reason:      "opening price book (re-issued after a failed delivery)",
			InitiatedBy: "usslpd",
			IdempotencyKey: fmt.Sprintf("opening:%s:%s:%d:retry-%d",
				m.store.ID, sku, price.Amount, attempt),
		})
	}
	for st, items := range byStore {
		if _, err := s.cloudSvcs.label.Batch().BatchUpdatePrices(ctx, labelapp.BatchRequest{
			TenantID: st.Tenant, Region: canon.Region(s.cfg.Region),
			Items: items, InitiatedBy: "usslpd/opening",
		}); err != nil {
			return fmt.Errorf("usslpd: re-issuing the opening price book for %s: %w", st.ID, err)
		}
	}
	return nil
}

// describeLabel renders what the platform knows about one label's last update,
// for an error message that has to be diagnosable from a CI log alone.
func (s *Stack) describeLabel(id canon.LabelID) string {
	for _, st := range s.stores {
		for _, z := range st.Zones {
			if z.Controller == nil {
				continue
			}
			rec, ok := z.Controller.Record(id)
			if !ok {
				continue
			}
			cs := z.Coordinator.Stats()
			last := "none"
			if d, ok := s.deliveries.last(id); ok {
				last = fmt.Sprintf("seq %d at %s", d.Sequence, d.At.Format(time.RFC3339Nano))
			}
			panel := "no such panel"
			if l, ok := z.Sim.Label(id); ok {
				ls := l.Stats()
				parent, hasParent := z.Sim.Net.ParentOf(l.NodeID())
				panel = fmt.Sprintf("panel at sequence %d, %d refreshes, %d frames in, "+
					"%d acks sent, %d acks lost, %d discarded, battery %d%%, dead=%t, "+
					"alive=%t, parent=%q(%t), active_window=%t",
					ls.Sequence, ls.RefreshCount, ls.FramesReceived, ls.AcksSent,
					ls.AcksLost, ls.Discarded, ls.BatteryPct, l.Dead(),
					z.Sim.Net.Alive(l.NodeID()), parent, hasParent, l.InActiveWindow())
			}
			return fmt.Sprintf("sequence %d sent, %d displayed, last error %q; "+
				"zone %s has %d queued and %d in flight; last delivery %s; %s",
				rec.Sequence, rec.DisplayedSequence, rec.LastError,
				z.SECID, cs.Queued, cs.InFlight, last, panel)
		}
	}
	return "the controllers have no record of it at all"
}

// AwaitQuiet blocks until every controller's transmission queue is empty: no
// update queued, none in flight, every panel settled.
//
// It is exported because the demo and the load harness both need the same
// guarantee before they start timing anything, and because "is the store
// finished?" is a question an operator asks too.
func (s *Stack) AwaitQuiet(ctx context.Context, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		queued, inflight := 0, 0
		for _, st := range s.stores {
			for _, z := range st.Zones {
				if z.Coordinator == nil {
					continue
				}
				cs := z.Coordinator.Stats()
				queued += cs.Queued
				inflight += cs.InFlight
			}
		}
		if queued == 0 && inflight == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("usslpd: the store's shelves were still turning over after %s "+
				"(%d updates queued, %d in flight)", within, queued, inflight)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// The POS ingress
// ---------------------------------------------------------------------------

// ShopifyWebhook builds a signed Shopify products/update webhook for one
// product in one store.
//
// It is here rather than in the test package because the demo, the load harness
// and the end-to-end suite all need to produce the identical bytes, and a
// second copy of the payload shape is a second thing to keep in step with the
// adapter.
type ShopifyWebhook struct {
	Request *http.Request
	Body    []byte
	// WebhookID is the value of X-Shopify-Webhook-Id, which is what Shopify
	// keeps constant across every redelivery of the same event and therefore
	// what the platform deduplicates on.
	WebhookID string
}

// BuildShopifyWebhook renders and signs a price change exactly as Shopify's
// products/update webhook would deliver it.
//
// The price is a decimal string, never a float, because that is what Shopify
// sends and because the string is the retailer's exact intent: converting it to
// minor units through the adapter's own decimal path is what keeps a price from
// drifting by a penny somewhere between the till and the shelf.
func (s *Stack) BuildShopifyWebhook(ctx context.Context, tenant canon.TenantID, store canon.StoreID,
	sku canon.SKU, price canon.Money, webhookID string) (*ShopifyWebhook, error) {

	if webhookID == "" {
		webhookID = canon.NewUUID()
	}
	// The timestamps carry sub-second precision. Shopify's own webhooks are
	// second-resolution, which is fine for a shop whose prices change a few
	// times a day and is not fine here: the Label Service aggregate discards a
	// change whose source clock is not later than the one it already applied,
	// so two changes to the same product inside one second would leave the
	// second one recorded as stale and never displayed. That rule is right —
	// it is what stops a replayed batch rolling a price backwards — and the
	// harness has to respect it rather than work around it.
	body, err := json.Marshal(map[string]any{
		"id":         strconv.FormatUint(hashOf(string(sku)), 10),
		"title":      "Product " + string(sku),
		"handle":     string(sku),
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"status":     "active",
		"variants": []map[string]any{{
			"id":         strconv.FormatUint(hashOf(string(sku))+1, 10),
			"product_id": strconv.FormatUint(hashOf(string(sku)), 10),
			"title":      "Default",
			"sku":        string(sku),
			"price":      decimalString(price),
			"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
		}},
	})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1/ingest/%s/%s", s.cloudSvcs.uigURL, tenant, ShopifyBindingID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(shopify.HeaderTopic, "products/update")
	req.Header.Set(shopify.HeaderShopDomain, ShopDomainFor(store))
	req.Header.Set(shopify.HeaderWebhookID, webhookID)
	req.Header.Set(shopify.HeaderAPIVersion, "2024-01")
	req.Header.Set(shopify.HeaderHMAC, s.cloudSvcs.uig.SignShopify(body))
	return &ShopifyWebhook{Request: req, Body: body, WebhookID: webhookID}, nil
}

// PushShopifyPrice delivers a signed webhook to the UIG and returns the moment
// the platform acknowledged it.
//
// The returned instant is the start of the SLO clock in spirit but not in
// letter: INTERFACE-CONTRACTS §4 measures from the envelope's RecordedAt, the
// moment USSLP took durable responsibility, which is a little after the request
// left here. Callers that need the contractual number read it from the
// LabelDelivered event rather than from a stopwatch, and the two are compared
// in test/e2e.
func (s *Stack) PushShopifyPrice(ctx context.Context, tenant canon.TenantID, store canon.StoreID,
	sku canon.SKU, price canon.Money, webhookID string) (sentAt time.Time, ackedAt time.Time, err error) {

	hook, err := s.BuildShopifyWebhook(ctx, tenant, store, sku, price, webhookID)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	sentAt = time.Now()
	resp, err := s.httpClient().Do(hook.Request)
	if err != nil {
		return sentAt, time.Time{}, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	ackedAt = time.Now()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return sentAt, ackedAt, fmt.Errorf("usslpd: the UIG refused the webhook: %s: %s",
			resp.Status, bytes.TrimSpace(payload))
	}
	return sentAt, ackedAt, nil
}

// httpClient is the shared client for the runtime's own calls into its
// services. Connections are pooled generously because the load harness makes
// thousands of them and a fresh TCP handshake per webhook would be measuring
// the loopback rather than the platform.
func (s *Stack) httpClient() *http.Client {
	s.clientOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxIdleConns = 512
		t.MaxIdleConnsPerHost = 512
		t.MaxConnsPerHost = 512
		s.client = &http.Client{Transport: t, Timeout: 30 * time.Second}
	})
	return s.client
}

// decimalString renders money the way a POS sends it: the bare decimal, no
// currency code. canon.Money.String appends the code because the attestation
// digest needs it; a Shopify variant price does not have one at all, which is
// the whole reason a binding carries a default currency.
func decimalString(m canon.Money) string {
	return strings.TrimSuffix(m.String(), " "+m.Currency)
}

func hashOf(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h % 9000000000
}

package stack

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/edge/sec"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Budget is the hop-by-hop latency budget from INTERFACE-CONTRACTS §4.
//
// It is transcribed here so the runtime can report measurements *against* it
// rather than in isolation: a p99 of 900 ms means nothing until it is next to
// the 3,000 ms it is allowed. The budget is normative; this table is a copy,
// and TestLatencyBudgetMatchesContract in test/e2e keeps the copy honest.
var Budget = []Hop{
	{"POS -> UIG", 50, "validate, dedupe, normalise, publish"},
	{"UIG -> stream", 30, "durable append (acks=all)"},
	{"stream -> Label Service", 120, "consume, resolve labels, price, attest"},
	{"Label Service -> broker", 100, "MQTT QoS 1 publish"},
	{"broker -> SGU -> SEC", 100, "bridge + LAN"},
	{"SEC -> label", 400, "Zigbee, up to 3 hops, attested frame + queueing"},
	{"label refresh", 2000, "E-Ink full waveform (300 ms partial)"},
	{"ACK back to cloud", 200, "confirmation"},
}

// Hop is one line of the budget.
type Hop struct {
	Name     string `json:"hop"`
	BudgetMS int64  `json:"budget_ms"`
	What     string `json:"what"`
}

// TotalBudget is the end-to-end SLO the platform is sold against.
const TotalBudget = 3 * time.Second

// BudgetTotalMS is the sum of the hop budgets, which must equal TotalBudget.
func BudgetTotalMS() int64 {
	var n int64
	for _, h := range Budget {
		n += h.BudgetMS
	}
	return n
}

// Delivery is one measured price delivery, from the controller that made it.
//
// Every field is measured rather than modelled. Total is the contract's number:
// the envelope's RecordedAt — the moment USSLP took durable responsibility —
// to the moment the pixels settled.
type Delivery struct {
	LabelID    canon.LabelID `json:"label_id"`
	StoreID    canon.StoreID `json:"store_id"`
	SECID      canon.SECID   `json:"sec_id"`
	Sequence   int64         `json:"sequence"`
	Delivered  bool          `json:"delivered"`
	TotalMS    int64         `json:"total_ms"`
	SECToLabel int64         `json:"sec_to_label_ms"`
	RefreshMS  int64         `json:"refresh_ms"`
	Hops       int           `json:"mesh_hops"`
	Partial    bool          `json:"partial_refresh"`
	// Attempts counts application-level transmissions and MACAttempts counts
	// frames actually put on the air. They are carried through because a
	// latency without them is ambiguous in exactly the case that matters: a
	// slow delivery that took one attempt is a congested channel, and a slow
	// delivery that took three is a channel that lost the first two, and the
	// remedy for those is not the same.
	Attempts    int       `json:"attempts"`
	MACAttempts int       `json:"mac_attempts"`
	At          time.Time `json:"at"`
}

// deliveryMonitor is the runtime's own SLO evidence.
//
// # Why the controller rather than the stream
//
// `label-delivery` carries LatencyMS, MeshHops and RefreshMS, which is enough
// to report the SLO but not enough to say which hop spent the budget: the
// controller-to-label time never leaves the controller. Observing
// sec.Controller.OnDelivery captures it at the only place it exists, and the
// end-to-end suite cross-checks the totals against the stream so the two cannot
// quietly diverge.
type deliveryMonitor struct {
	mu     sync.Mutex
	all    []Delivery
	byLbl  map[canon.LabelID]Delivery
	max    int
	waiter []waiter
}

type waiter struct {
	label canon.LabelID
	seq   int64
	ch    chan Delivery
}

// maxRetained bounds the ring. The load harness produces hundreds of thousands
// of deliveries and every one of them retained would be the process's largest
// allocation; 200,000 is enough for a p99 over a full benchmark run and costs
// about 15 MB.
const maxRetained = 200000

func newDeliveryMonitor() *deliveryMonitor {
	return &deliveryMonitor{byLbl: map[canon.LabelID]Delivery{}, max: maxRetained}
}

func (m *deliveryMonitor) record(store canon.StoreID, secID canon.SECID, res sec.DeliveryResult) {
	d := Delivery{
		LabelID: res.LabelID, StoreID: store, SECID: secID, Sequence: res.Sequence,
		Delivered: res.Delivered, TotalMS: res.TotalLatency.Milliseconds(),
		SECToLabel: res.SECToLabel.Milliseconds(), RefreshMS: int64(res.RefreshMS),
		Hops: res.Hops, Partial: res.Partial,
		Attempts: res.Attempts, MACAttempts: res.MACAttempts,
		At: time.Now().UTC(),
	}
	m.mu.Lock()
	if len(m.all) >= m.max {
		// Drop the oldest half rather than one at a time: a copy per record at
		// 200,000 entries is quadratic, and the percentiles do not care which
		// half of an old window they lost.
		m.all = append(m.all[:0], m.all[m.max/2:]...)
	}
	m.all = append(m.all, d)
	if d.Delivered {
		m.byLbl[d.LabelID] = d
	}
	kept := m.waiter[:0]
	var fire []waiter
	for _, w := range m.waiter {
		if w.label == d.LabelID && (w.seq == 0 || w.seq == d.Sequence) {
			fire = append(fire, w)
			continue
		}
		kept = append(kept, w)
	}
	m.waiter = kept
	m.mu.Unlock()
	for _, w := range fire {
		select {
		case w.ch <- d:
		default:
		}
	}
}

// await blocks until a delivery for a label (and optionally an exact sequence)
// is observed.
func (m *deliveryMonitor) await(label canon.LabelID, seq int64, within time.Duration) (Delivery, bool) {
	ch := make(chan Delivery, 1)
	m.mu.Lock()
	m.waiter = append(m.waiter, waiter{label: label, seq: seq, ch: ch})
	m.mu.Unlock()
	select {
	case d := <-ch:
		return d, true
	case <-time.After(within):
		return Delivery{}, false
	}
}

// last returns the most recent successful delivery for a label.
func (m *deliveryMonitor) last(label canon.LabelID) (Delivery, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.byLbl[label]
	return d, ok
}

// snapshot copies the deliveries matching a store, or all of them.
func (m *deliveryMonitor) snapshot(store canon.StoreID) []Delivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Delivery, 0, len(m.all))
	for _, d := range m.all {
		if store != "" && d.StoreID != store {
			continue
		}
		out = append(out, d)
	}
	return out
}

// SLOReport is the measured latency distribution against the budget.
type SLOReport struct {
	StoreID     canon.StoreID `json:"store_id,omitempty"`
	Deliveries  int           `json:"deliveries"`
	Failed      int           `json:"failed"`
	BudgetMS    int64         `json:"budget_ms"`
	WithinSLO   int           `json:"within_slo"`
	Attainment  string        `json:"attainment"`
	P50MS       int64         `json:"p50_ms"`
	P95MS       int64         `json:"p95_ms"`
	P99MS       int64         `json:"p99_ms"`
	MaxMS       int64         `json:"max_ms"`
	MeanMS      int64         `json:"mean_ms"`
	Measured    []MeasuredHop `json:"measured_hops"`
	PartialPct  string        `json:"partial_refresh_share"`
	MeanMeshHop float64       `json:"mean_mesh_hops"`
	// ClockDriftMS is how far the edge simulation's clock has fallen behind the
	// wall clock, in milliseconds. See Stack.ClockDrift: it is published beside
	// the latencies because it is the error bar on them.
	ClockDriftMS int64 `json:"clock_drift_ms"`
}

// MeasuredHop is one budget line with what was actually observed against it.
//
// Only three of the eight contract hops are separately observable from outside
// the process: the controller-to-label radio time, the panel's own waveform,
// and everything before them. The rest are reported as one "cloud and bridge"
// figure with the budgets they share, and the report says so rather than
// inventing a split it cannot measure.
type MeasuredHop struct {
	Name       string `json:"hop"`
	BudgetMS   int64  `json:"budget_ms"`
	P50MS      int64  `json:"p50_ms"`
	P99MS      int64  `json:"p99_ms"`
	WithinP99  bool   `json:"p99_within_budget"`
	Observable bool   `json:"separately_measured"`
	Note       string `json:"note,omitempty"`
}

// SLO computes the report for a store, or for every store when store is "".
func (s *Stack) SLO(store canon.StoreID) SLOReport {
	r := report(store, s.deliveries.snapshot(store))
	r.ClockDriftMS = s.ClockDrift(store).Milliseconds()
	return r
}

// ClockDrift is how far a store's simulated clock has fallen behind the wall
// clock — the worst of them when store is "".
//
// # Why this is published rather than hidden
//
// Every latency this platform reports is a difference between two clocks: the
// envelope's RecordedAt, stamped by a cloud service off the wall clock, and the
// moment the controller saw the pixels settle, stamped off the store's
// discrete-event clock. edge/sim paces that clock against real time by sleeping
// until each event is due, which means it can fall behind — an event handler
// that takes longer than the gap to the next event pushes everything after it
// back — and it has no mechanism to catch up. On an unloaded machine the error
// is a millisecond or two. On a two-core container running eight cloud
// services, a broker and a hundred labels it can reach a few hundred
// milliseconds over minutes, and every one of those milliseconds is subtracted
// from every latency the platform reports. A sufficiently drifted clock reports
// a *negative* end-to-end latency, which is the honest symptom of the
// underlying problem and much better than a plausible-looking small number.
//
// So the drift is measured and reported next to the latencies rather than
// corrected away. Correcting it is not available from here: advancing the
// engine from outside its paced runner would execute event handlers on two
// goroutines at once, which the edge model is not built for. What the runtime
// can do is say how big the error bar is, and test/e2e additionally times a
// sample of price changes with an outside stopwatch and fails if the two
// disagree.
func (s *Stack) ClockDrift(store canon.StoreID) time.Duration {
	now := time.Now().UTC()
	var worst time.Duration
	for _, st := range s.stores {
		if store != "" && st.ID != store {
			continue
		}
		if st.Engine == nil {
			continue
		}
		d := now.Sub(st.Engine.Now())
		if d < 0 {
			d = -d
		}
		if d > worst {
			worst = d
		}
	}
	return worst
}

func report(store canon.StoreID, ds []Delivery) SLOReport {
	r := SLOReport{StoreID: store, BudgetMS: TotalBudget.Milliseconds()}
	totals := make([]int64, 0, len(ds))
	radio := make([]int64, 0, len(ds))
	refresh := make([]int64, 0, len(ds))
	cloud := make([]int64, 0, len(ds))
	var sum, hops int64
	partial := 0
	for _, d := range ds {
		if !d.Delivered {
			r.Failed++
			continue
		}
		r.Deliveries++
		totals = append(totals, d.TotalMS)
		radio = append(radio, d.SECToLabel)
		refresh = append(refresh, d.RefreshMS)
		// Everything the controller did not do: ingest, the stream, the Label
		// Service, the broker and the bridge. It is a residual rather than a
		// measurement, and is labelled as one.
		c := d.TotalMS - d.SECToLabel - d.RefreshMS
		if c < 0 {
			c = 0
		}
		cloud = append(cloud, c)
		sum += d.TotalMS
		hops += int64(d.Hops)
		if d.Partial {
			partial++
		}
		if d.TotalMS <= TotalBudget.Milliseconds() {
			r.WithinSLO++
		}
	}
	if r.Deliveries == 0 {
		r.Attainment = "n/a"
		return r
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })
	r.P50MS, r.P95MS, r.P99MS = pct(totals, 50), pct(totals, 95), pct(totals, 99)
	r.MaxMS = totals[len(totals)-1]
	r.MeanMS = sum / int64(r.Deliveries)
	r.Attainment = fmt.Sprintf("%.3f%%", 100*float64(r.WithinSLO)/float64(r.Deliveries))
	r.PartialPct = fmt.Sprintf("%.1f%%", 100*float64(partial)/float64(r.Deliveries))
	r.MeanMeshHop = float64(hops) / float64(r.Deliveries)

	cloudBudget := Budget[0].BudgetMS + Budget[1].BudgetMS + Budget[2].BudgetMS +
		Budget[3].BudgetMS + Budget[4].BudgetMS
	r.Measured = []MeasuredHop{
		{
			Name: "POS -> ... -> SEC (cloud and bridge)", BudgetMS: cloudBudget,
			P50MS: pctSorted(cloud, 50), P99MS: pctSorted(cloud, 99), Observable: false,
			Note: "the first five contract hops, measured as a residual: total minus the " +
				"controller-to-label radio time minus the panel's own waveform",
		},
		{
			Name: "SEC -> label", BudgetMS: Budget[5].BudgetMS,
			P50MS: pctSorted(radio, 50), P99MS: pctSorted(radio, 99), Observable: true,
			Note: "measured by the controller from deciding to transmit to the frame arriving",
		},
		{
			Name: "label refresh", BudgetMS: Budget[6].BudgetMS,
			P50MS: pctSorted(refresh, 50), P99MS: pctSorted(refresh, 99), Observable: true,
			Note: "the waveform duration the panel itself reported",
		},
	}
	for i := range r.Measured {
		r.Measured[i].WithinP99 = r.Measured[i].P99MS <= r.Measured[i].BudgetMS
	}
	return r
}

func pct(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p*len(sorted) + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

func pctSorted(v []int64, p int) int64 {
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return pct(c, p)
}

// Deliveries returns the measured deliveries, for a load harness that wants to
// compute its own statistics.
func (s *Stack) Deliveries(store canon.StoreID) []Delivery { return s.deliveries.snapshot(store) }

// AwaitDelivery blocks until a label acknowledges a delivery, optionally at an
// exact sequence, and returns the measured timings.
//
// It registers its interest before it blocks, so a caller that publishes a
// price and *then* calls this has a race it will lose about once in a thousand
// tries. Use WatchDelivery for that shape.
func (s *Stack) AwaitDelivery(label canon.LabelID, seq int64, within time.Duration) (Delivery, bool) {
	return s.deliveries.await(label, seq, within)
}

// DeliveryWatch is a registered interest in one label's next delivery.
type DeliveryWatch struct {
	ch chan Delivery
}

// WatchDelivery registers interest in a delivery *before* the caller does
// anything that could cause it, and returns a handle to wait on.
//
// The two-step shape exists because the one-step one is a race. A price change
// through the whole platform takes a few hundred milliseconds, and starting a
// goroutine to watch for it after publishing usually wins — usually. On a
// loaded two-core machine with a thousand goroutines in flight it loses often
// enough to look like a fraction of a percent of lost price changes, which is
// exactly the kind of number that gets investigated as a platform fault for a
// day before someone reads the harness.
func (s *Stack) WatchDelivery(label canon.LabelID, seq int64) *DeliveryWatch {
	ch := make(chan Delivery, 1)
	s.deliveries.mu.Lock()
	s.deliveries.waiter = append(s.deliveries.waiter, waiter{label: label, seq: seq, ch: ch})
	s.deliveries.mu.Unlock()
	return &DeliveryWatch{ch: ch}
}

// Wait blocks for the delivery, reporting whether one arrived in time.
func (w *DeliveryWatch) Wait(within time.Duration) (Delivery, bool) {
	select {
	case d := <-w.ch:
		return d, true
	case <-time.After(within):
		return Delivery{}, false
	}
}

// LastDelivery is the most recent successful delivery to a label.
func (s *Stack) LastDelivery(label canon.LabelID) (Delivery, bool) {
	return s.deliveries.last(label)
}

// reset discards the measured deliveries, starting a new window.
func (m *deliveryMonitor) reset() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.all)
	m.all = nil
	return n
}

// ResetSLO starts a fresh measurement window.
//
// It exists because the population matters as much as the percentile. A store
// that has just opened has one store-wide fan-out in its record — every label
// repriced at once, which saturates the radio by construction and is the same
// experiment test/load runs deliberately — and reporting a per-change SLO
// against that population answers a question nobody asked. Resetting is not
// hiding it: the fan-out's own numbers are reported where they belong, as a
// fan-out.
func (s *Stack) ResetSLO() int { return s.deliveries.reset() }

func (s *Stack) handleSLOReset(*http.Request) (any, error) {
	n := s.ResetSLO()
	return map[string]any{
		"discarded": n,
		"note": "the measurement window is empty; latencies reported from here " +
			"describe only what happens next",
	}, nil
}

func (s *Stack) handleStoreSLO(r *http.Request) (any, error) {
	id := canon.StoreID(r.PathValue("id"))
	if id != "" && id != "all" {
		if _, ok := s.Store(id); !ok {
			return nil, fmt.Errorf("no such store: %s", id)
		}
	} else {
		id = ""
	}
	return map[string]any{"budget": Budget, "measured": s.SLO(id)}, nil
}

package sgu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// ---------------------------------------------------------------------------
// Hybrid logical clock
// ---------------------------------------------------------------------------

func TestHLCIsMonotonicUnderAClockThatGoesBackwards(t *testing.T) {
	// An NTP step correction after a store has been offline for hours moves the
	// system clock backwards. The hybrid clock must not follow it, because
	// re-issuing timestamps that have already been used would make two different
	// events indistinguishable to a merge.
	var wall time.Time
	c := NewClock("sgu-1", func() time.Time { return wall }, DefaultMaxSkew)
	wall = time.Unix(1700000000, 0)
	a := c.Now()
	wall = wall.Add(-time.Hour)
	b := c.Now()
	if !a.Before(b) {
		t.Fatalf("clock went backwards: %s then %s", a, b)
	}
	if b.WallMS != a.WallMS {
		t.Fatalf("physical component moved from %d to %d despite the system clock stepping back",
			a.WallMS, b.WallMS)
	}
	if b.Logical != a.Logical+1 {
		t.Fatalf("logical counter is %d, want %d", b.Logical, a.Logical+1)
	}
}

func TestHLCCarriesCausalityAcrossADriftedClock(t *testing.T) {
	// The property reconciliation depends on: anything the store does after
	// hearing from the cloud is ordered after it, even though the store's own
	// clock is minutes behind.
	cloudWall := time.Unix(1700000600, 0)
	storeWall := time.Unix(1700000000, 0) // ten minutes slow
	cloud := NewClock("cloud", func() time.Time { return cloudWall }, time.Hour)
	store := NewClock("sgu-1", func() time.Time { return storeWall }, time.Hour)

	remote := cloud.Now()
	local, err := store.Observe(remote)
	if err != nil {
		t.Fatalf("observing the cloud's timestamp: %v", err)
	}
	if !remote.Before(local) {
		t.Fatalf("the store's response %s does not follow the cloud's %s", local, remote)
	}
	next := store.Now()
	if !local.Before(next) {
		t.Fatal("the store's clock stopped advancing after adopting a remote timestamp")
	}
	if got := store.Skew().Last; got != 10*time.Minute {
		t.Fatalf("measured skew %v, want 10m", got)
	}
}

func TestHLCRefusesAWildlyFutureTimestamp(t *testing.T) {
	// A peer whose real-time clock battery has died and reports 2038 must not be
	// allowed to drag this store into the future, where it would win every merge
	// for the next fourteen years.
	wall := time.Unix(1700000000, 0)
	c := NewClock("sgu-1", func() time.Time { return wall }, 10*time.Minute)
	rogue := HLC{WallMS: wall.Add(48 * time.Hour).UnixMilli(), NodeID: "broken-peer"}
	got, err := c.Observe(rogue)
	if err == nil {
		t.Fatal("a timestamp two days in the future was adopted")
	}
	if got.WallMS >= rogue.WallMS {
		t.Fatalf("the local clock was dragged to %d despite refusing the timestamp", got.WallMS)
	}
	if r := c.Skew(); r.Rejected != 1 {
		t.Fatalf("rejected count is %d, want 1", r.Rejected)
	}
}

func TestHLCRoundTripsThroughItsStringForm(t *testing.T) {
	in := HLC{WallMS: 1700000000123, Logical: 7, NodeID: "sgu-0417"}
	out, err := ParseHLC(in.String())
	if err != nil {
		t.Fatalf("parsing %q: %v", in.String(), err)
	}
	if out != in {
		t.Fatalf("round trip changed %v into %v", in, out)
	}
}

// ---------------------------------------------------------------------------
// The merge
// ---------------------------------------------------------------------------

func reg(key string, kind Kind, value string, wall int64, logical uint32, node string, origin Origin) Register {
	return Register{Key: key, Kind: kind, Value: json.RawMessage(value),
		TS: HLC{WallMS: wall, Logical: logical, NodeID: node}, Origin: origin}
}

func TestMergePolicy(t *testing.T) {
	diverged := HLC{WallMS: 1000, NodeID: "sgu"}

	cases := []struct {
		name       string
		local      Register
		cloud      Register
		wantValue  string
		wantOrigin Origin
		conflict   bool
		resolution Resolution
	}{
		{
			name:  "only the store changed it during the outage",
			local: reg("price/l1", KindPricing, `199`, 2000, 0, "sgu", OriginLocal),
			// The cloud's value predates the outage. Pure last-writer-wins would
			// still let it overwrite the store's change if its clock happened to
			// be ahead, which is exactly the bug divergence tracking exists to
			// prevent.
			cloud:      reg("price/l1", KindPricing, `249`, 500, 0, "cloud", OriginCloud),
			wantValue:  `199`,
			wantOrigin: OriginLocal,
			resolution: ResolutionOnlyLocal,
		},
		{
			name:       "only the cloud changed it",
			local:      reg("price/l1", KindPricing, `249`, 500, 0, "sgu", OriginLocal),
			cloud:      reg("price/l1", KindPricing, `179`, 2000, 0, "cloud", OriginCloud),
			wantValue:  `179`,
			wantOrigin: OriginCloud,
			resolution: ResolutionOnlyCloud,
		},
		{
			name:       "both changed a price: the cloud is the system of record",
			local:      reg("price/l1", KindPricing, `199`, 9000, 0, "sgu", OriginLocal),
			cloud:      reg("price/l1", KindPricing, `179`, 2000, 0, "cloud", OriginCloud),
			wantValue:  `179`,
			wantOrigin: OriginCloud,
			conflict:   true,
			resolution: ResolutionPolicyCloudPricing,
		},
		{
			name:       "both changed a stock level: the store counted the shelf",
			local:      reg("inventory/s1", KindInventory, `12`, 2000, 0, "sgu", OriginLocal),
			cloud:      reg("inventory/s1", KindInventory, `40`, 9000, 0, "cloud", OriginCloud),
			wantValue:  `12`,
			wantOrigin: OriginLocal,
			conflict:   true,
			resolution: ResolutionPolicyLocalInventory,
		},
		{
			name:       "both changed something with no policy: the clock decides",
			local:      reg("planogram/x", KindOther, `"a"`, 9000, 0, "sgu", OriginLocal),
			cloud:      reg("planogram/x", KindOther, `"b"`, 2000, 0, "cloud", OriginCloud),
			wantValue:  `"a"`,
			wantOrigin: OriginLocal,
			conflict:   true,
			resolution: ResolutionClock,
		},
		{
			name:       "identical values are never a conflict",
			local:      reg("price/l1", KindPricing, `199`, 9000, 0, "sgu", OriginLocal),
			cloud:      reg("price/l1", KindPricing, `199`, 2000, 0, "cloud", OriginCloud),
			wantValue:  `199`,
			wantOrigin: OriginLocal,
			resolution: ResolutionIdentical,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Merge(tc.local, tc.cloud, diverged)
			if string(got.Winner.Value) != tc.wantValue {
				t.Fatalf("winner is %s, want %s (%s)", got.Winner.Value, tc.wantValue, got.Resolution)
			}
			if got.Winner.Origin != tc.wantOrigin {
				t.Fatalf("winner came from %s, want %s", got.Winner.Origin, tc.wantOrigin)
			}
			if got.Conflict != tc.conflict {
				t.Fatalf("conflict=%v, want %v (%s)", got.Conflict, tc.conflict, got.Resolution)
			}
			if got.Resolution != tc.resolution {
				t.Fatalf("resolution %q, want %q", got.Resolution, tc.resolution)
			}
			if got.Conflict && got.Loser == nil {
				t.Fatal("a conflict must record what it discarded")
			}
		})
	}
}

func TestMergeIsCommutativeInItsOutcome(t *testing.T) {
	// Both sides run the merge independently and must reach the same answer, or
	// the two ends of the link disagree about what the price is.
	diverged := HLC{WallMS: 1000}
	local := reg("planogram/x", KindOther, `"a"`, 5000, 3, "sgu", OriginLocal)
	cloud := reg("planogram/x", KindOther, `"b"`, 5000, 3, "cloud", OriginCloud)
	a := Merge(local, cloud, diverged)
	b := Merge(local, cloud, diverged)
	if string(a.Winner.Value) != string(b.Winner.Value) {
		t.Fatal("the merge is not deterministic on identical timestamps; the node-id tiebreak is not doing its job")
	}
	if a.Winner.TS.NodeID != "sgu" {
		t.Fatalf("the tiebreak picked %q; it must be the lexicographically greater node id", a.Winner.TS.NodeID)
	}
}

// ---------------------------------------------------------------------------
// The durable buffer
// ---------------------------------------------------------------------------

func testStore(t *testing.T) *kvstore.Store {
	t.Helper()
	s, err := kvstore.OpenWith(kvstore.Options{Sync: kvstore.SyncNever})
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestQueueOrderingCoalescingAndDurability(t *testing.T) {
	st := testStore(t)
	q, err := NewQueue(st, QueueConfig{})
	if err != nil {
		t.Fatalf("opening queue: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := q.Enqueue(Entry{Topic: fmt.Sprintf("ack/%d", i), Payload: []byte("x"),
			Class: ClassCritical, IdempotencyKey: fmt.Sprintf("k%d", i)}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	// Three heartbeats from the same controller must collapse to one, or a
	// twelve-hour outage buffers ten thousand copies of a message whose only
	// useful version is the last.
	for i := 0; i < 3; i++ {
		if err := q.Enqueue(Entry{Topic: "sec/1/heartbeat", Payload: []byte(fmt.Sprint(i)), Class: ClassLatest}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if got := q.Depth(); got != 6 {
		t.Fatalf("depth is %d, want 6 (five acknowledgements and one coalesced heartbeat)", got)
	}
	if got := q.Stats().Coalesced; got != 2 {
		t.Fatalf("coalesced %d, want 2", got)
	}

	head, err := q.Peek(10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	for i := 0; i < 5; i++ {
		if head[i].Topic != fmt.Sprintf("ack/%d", i) {
			t.Fatalf("position %d holds %s; the buffer must flush in the order it accepted", i, head[i].Topic)
		}
	}
	if string(head[5].Payload) != "2" {
		t.Fatalf("the coalesced heartbeat holds %q, want the newest", head[5].Payload)
	}

	// A power cut: the queue is reopened over the same store and must come back
	// with everything, in order.
	q2, err := NewQueue(st, QueueConfig{})
	if err != nil {
		t.Fatalf("reopening queue: %v", err)
	}
	if got := q2.Depth(); got != 6 {
		t.Fatalf("after a restart the queue holds %d of 6 messages", got)
	}
	head2, err := q2.Peek(10)
	if err != nil {
		t.Fatalf("peek after restart: %v", err)
	}
	for i := range head {
		if head[i].Topic != head2[i].Topic {
			t.Fatalf("restart reordered the queue at %d: %s vs %s", i, head[i].Topic, head2[i].Topic)
		}
	}
}

func TestQueueOverflowSacrificesBulkFirst(t *testing.T) {
	st := testStore(t)
	q, err := NewQueue(st, QueueConfig{MaxEntries: 10})
	if err != nil {
		t.Fatalf("opening queue: %v", err)
	}
	for i := 0; i < 6; i++ {
		_ = q.Enqueue(Entry{Topic: fmt.Sprintf("telemetry/%d", i), Payload: []byte("t"), Class: ClassBulk})
	}
	for i := 0; i < 6; i++ {
		_ = q.Enqueue(Entry{Topic: fmt.Sprintf("ack/%d", i), Payload: []byte("a"), Class: ClassCritical})
	}
	if got := q.Depth(); got != 10 {
		t.Fatalf("queue holds %d, bound is 10", got)
	}
	s := q.Stats()
	if s.DroppedBulk != 2 || s.DroppedOther != 0 {
		t.Fatalf("dropped %d bulk and %d critical; telemetry must be sacrificed before evidence",
			s.DroppedBulk, s.DroppedOther)
	}
	if s.Lossy {
		t.Fatal("dropping telemetry must not mark the reconciliation lossy")
	}
	entries, _ := q.Peek(20)
	acks := 0
	for _, e := range entries {
		if e.Class == ClassCritical {
			acks++
		}
	}
	if acks != 6 {
		t.Fatalf("%d of 6 acknowledgements survived the overflow", acks)
	}

	// Now fill it entirely with evidence: the only thing left to drop is
	// evidence, and doing so has to be announced rather than absorbed.
	for i := 0; i < 10; i++ {
		_ = q.Enqueue(Entry{Topic: fmt.Sprintf("ack/x%d", i), Payload: []byte("a"), Class: ClassCritical})
	}
	s = q.Stats()
	if s.DroppedOther == 0 {
		t.Fatal("a queue full of critical messages never dropped one, so it is not bounded")
	}
	if !s.Lossy {
		t.Fatal("dropping a critical message must latch the lossy flag; a silent hole in the compliance record is the worst outcome available")
	}
}

func TestQueueDeduplicatesWhatTheCloudAlreadyHas(t *testing.T) {
	st := testStore(t)
	q, err := NewQueue(st, QueueConfig{})
	if err != nil {
		t.Fatalf("opening queue: %v", err)
	}
	if q.AlreadySent("evt-1") {
		t.Fatal("nothing has been sent yet")
	}
	if err := q.MarkSent("evt-1"); err != nil {
		t.Fatalf("marking sent: %v", err)
	}
	if !q.AlreadySent("evt-1") {
		t.Fatal("a delivered message was not remembered; the crash-after-acknowledgement case would duplicate it")
	}
}

// ---------------------------------------------------------------------------
// WAN detection
// ---------------------------------------------------------------------------

func TestDetectorIgnoresABlipAndCatchesAnOutage(t *testing.T) {
	var wall time.Time
	var fail bool
	var mu sync.Mutex
	wall = time.Unix(1700000000, 0)

	d := NewDetector(func() bool { return true }, func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return fmt.Errorf("probe timed out")
		}
		return nil
	}, DetectorConfig{Interval: time.Second, FailThreshold: 3, FailFor: 12 * time.Second,
		RecoverThreshold: 4, RecoverFor: 15 * time.Second})
	d.SetClock(func() time.Time { mu.Lock(); defer mu.Unlock(); return wall })

	var transitions []Mode
	d.OnChange(func(m Mode, reason string) { transitions = append(transitions, m) })

	step := func(d2 time.Duration) {
		mu.Lock()
		wall = wall.Add(d2)
		mu.Unlock()
		d.Check(context.Background())
	}

	// A two-second blip: two failed probes, then recovery. Nothing should
	// happen, because flapping a store into and out of autonomy twice a minute
	// is worse than not detecting anything.
	mu.Lock()
	fail = true
	mu.Unlock()
	step(time.Second)
	step(time.Second)
	mu.Lock()
	fail = false
	mu.Unlock()
	step(time.Second)
	if d.Mode() != ModeConnected {
		t.Fatal("a two-second blip flapped the store into autonomy")
	}
	if len(transitions) != 0 {
		t.Fatalf("a blip produced %d mode transitions", len(transitions))
	}

	// A real outage: sustained failures spanning the hysteresis window.
	mu.Lock()
	fail = true
	mu.Unlock()
	for i := 0; i < 20 && d.Mode() == ModeConnected; i++ {
		step(time.Second)
	}
	if d.Mode() != ModeAutonomous {
		t.Fatal("twenty seconds of failed probes did not put the store into autonomy")
	}
	stats := d.Stats()
	if stats.Transitions != 1 {
		t.Fatalf("%d transitions, want 1", stats.Transitions)
	}

	// Recovery is deliberately slower than failure: coming back triggers a
	// reconciliation, and one interrupted halfway is the messiest state here.
	mu.Lock()
	fail = false
	mu.Unlock()
	step(time.Second)
	step(time.Second)
	if d.Mode() != ModeAutonomous {
		t.Fatal("two good probes were enough to leave autonomy; recovery must be slower than failure")
	}
	for i := 0; i < 30 && d.Mode() == ModeAutonomous; i++ {
		step(time.Second)
	}
	if d.Mode() != ModeConnected {
		t.Fatal("the store never came back after the link was restored")
	}
}

func TestDetectorTreatsADeadLinkAsAFailedProbe(t *testing.T) {
	wall := time.Unix(1700000000, 0)
	d := NewDetector(func() bool { return false }, func(context.Context) error {
		t.Fatal("the probe must not run when the transport reports the link is down")
		return nil
	}, DetectorConfig{Interval: time.Second, FailThreshold: 2, FailFor: time.Second})
	d.SetClock(func() time.Time { return wall })
	for i := 0; i < 5 && d.Mode() == ModeConnected; i++ {
		wall = wall.Add(time.Second)
		d.Check(context.Background())
	}
	if d.Mode() != ModeAutonomous {
		t.Fatal("a link the transport reports as down did not trigger autonomy")
	}
}

// ---------------------------------------------------------------------------
// Pricing guard rails
// ---------------------------------------------------------------------------

func TestRulesEngineGuardRails(t *testing.T) {
	e, err := NewRulesEngine(testStore(t))
	if err != nil {
		t.Fatalf("opening rules engine: %v", err)
	}
	if err := e.Set(ProductRules{
		SKU: "SKU-WHISKY", Currency: "GBP",
		CostMinor: 1400, MinMarginBps: 2000, // 20% of the selling price
		FloorMinor:      1500, // minimum unit pricing
		CompetitorMinor: 1850, MaxUndercutBps: 1000, MaxPremiumBps: 1500,
	}); err != nil {
		t.Fatalf("setting rules: %v", err)
	}

	price := func(minor int64) canon.Money { return canon.NewMoney(minor, "GBP") }

	if v := e.Evaluate("SKU-WHISKY", price(2000)); !v.Allowed {
		t.Fatalf("a compliant price was refused: %s", v.Error())
	}
	if v := e.Evaluate("SKU-WHISKY", price(1400)); v.Allowed {
		t.Fatal("a price below the statutory floor was allowed")
	} else if v.Violations[0].Rule != RuleRegulatoryFloor {
		t.Fatalf("first violation is %s, want the regulatory floor", v.Violations[0].Rule)
	}
	// £17.49 clears the floor but leaves only 19.95% margin on a £14.00 cost.
	if v := e.Evaluate("SKU-WHISKY", price(1749)); v.Allowed {
		t.Fatal("a price a penny below the margin floor was allowed")
	}
	if v := e.Evaluate("SKU-WHISKY", price(1750)); !v.Allowed {
		t.Fatalf("a price exactly on the margin floor was refused: %s", v.Error())
	}
	if v := e.Evaluate("SKU-WHISKY", price(2400)); v.Allowed {
		t.Fatal("a price 30% above the competitor was allowed with a 15% premium limit")
	}
	if v := e.Evaluate("SKU-UNKNOWN", price(1)); !v.Allowed || v.Evaluated {
		t.Fatal("a product with no configured rules must pass, and must say it was not evaluated")
	}
	if v := e.Evaluate("SKU-WHISKY", canon.NewMoney(2000, "EUR")); v.Allowed {
		t.Fatal("a price in the wrong currency was compared against sterling rules")
	}
}

func TestRulesEngineMeetsItsLatencyBudget(t *testing.T) {
	e, _ := NewRulesEngine(testStore(t))
	for i := 0; i < 2000; i++ {
		_ = e.Set(ProductRules{SKU: canon.SKU(fmt.Sprintf("SKU-%05d", i)), Currency: "GBP",
			CostMinor: 100, MinMarginBps: 1500, FloorMinor: 50, CeilingMinor: 100000,
			CompetitorMinor: 250, MaxUndercutBps: 800, MaxPremiumBps: 800})
	}
	const n = 100000
	start := time.Now()
	for i := 0; i < n; i++ {
		e.Evaluate(canon.SKU(fmt.Sprintf("SKU-%05d", i%2000)), canon.NewMoney(249, "GBP"))
	}
	per := time.Since(start) / n
	if s := e.Stats(); s.Slowest > EvaluationBudget {
		t.Fatalf("slowest evaluation was %v, the budget is %v", s.Slowest, EvaluationBudget)
	}
	t.Logf("Tier-1 guard rails over %d products: %v per evaluation, budget %v", 2000, per, EvaluationBudget)
	if per > time.Millisecond {
		t.Fatalf("%v per evaluation leaves no room; this has to be a map lookup and integer arithmetic", per)
	}
}

// ---------------------------------------------------------------------------
// The schedule
// ---------------------------------------------------------------------------

func TestScheduleRefusesAnUnattestedPromotion(t *testing.T) {
	s, err := NewSchedule(testStore(t))
	if err != nil {
		t.Fatalf("opening schedule: %v", err)
	}
	err = s.Add(ScheduledPromotion{
		PromotionID: "promo-1", ActivateAt: time.Now().Add(time.Hour),
		Updates: []canon.PriceUpdated{{LabelID: "l1", Price: canon.NewMoney(199, "GBP")}},
	})
	if err == nil {
		t.Fatal("a promotion with no attestations was accepted; every label would refuse it at 08:00 and nobody would know until then")
	}
	if !strings.Contains(err.Error(), "unattested") {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}
}

func TestScheduleFiresOnceAndSkipsAnExpiredWindow(t *testing.T) {
	s, err := NewSchedule(testStore(t))
	if err != nil {
		t.Fatalf("opening schedule: %v", err)
	}
	base := time.Unix(1700000000, 0).UTC()
	attested := []canon.PriceUpdated{{LabelID: "l1", Price: canon.NewMoney(199, "GBP"),
		Attestation: canon.Attestation{Signature: "sig", Algorithm: canon.AttestationAlg}}}

	if err := s.Add(ScheduledPromotion{PromotionID: "live", ActivateAt: base.Add(time.Minute),
		ExpireAt: base.Add(time.Hour), Updates: attested}); err != nil {
		t.Fatalf("adding: %v", err)
	}
	// A promotion whose whole window elapsed while the store was down: activating
	// it now would put an expired promotional price on a shelf.
	if err := s.Add(ScheduledPromotion{PromotionID: "elapsed", ActivateAt: base.Add(-2 * time.Hour),
		ExpireAt: base.Add(-time.Hour), Updates: attested}); err != nil {
		t.Fatalf("adding: %v", err)
	}

	due := s.Due(base.Add(2 * time.Minute))
	if len(due) != 1 || due[0].PromotionID != "live" {
		t.Fatalf("due list is %v, want only the live promotion", due)
	}
	if err := s.MarkActivated("live", base.Add(2*time.Minute), 250*time.Millisecond); err != nil {
		t.Fatalf("marking activated: %v", err)
	}
	if got := s.Due(base.Add(3 * time.Minute)); len(got) != 0 {
		t.Fatal("an activated promotion fired twice; a gateway restart mid-window would double-apply it")
	}
	if missed := s.Missed(base.Add(2 * time.Minute)); len(missed) != 1 || missed[0].PromotionID != "elapsed" {
		t.Fatalf("missed promotions are %v, want the elapsed one reported", missed)
	}
	for _, p := range s.All() {
		if p.PromotionID == "live" && p.ActivationSkew != 250*time.Millisecond {
			t.Fatal("the clock skew at activation was not recorded; there would be no way to tell whether the promotion started on time")
		}
	}
}

func TestReconciliationReportReadsAsEnglish(t *testing.T) {
	r := ReconciliationReport{
		StoreID: "store-0417", OutageSeconds: 3600, Flushed: 412, Deduplicated: 3,
		Dropped: 0, KeysCompared: 900, KeysChanged: 12, Conflicts: 2,
		ClockSkew: SkewReport{Last: 340 * time.Millisecond},
	}
	s := r.Summary()
	for _, want := range []string{"store-0417", "1h0m0s", "412", "2 conflicts"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary %q does not mention %q", s, want)
		}
	}
	r.Lossy = true
	if !strings.Contains(r.Summary(), "OVERFLOWED") {
		t.Fatal("a lossy reconciliation must say so in the line an operator reads first")
	}
}

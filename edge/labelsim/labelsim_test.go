package labelsim

import (
	"math"
	"testing"
	"time"

	"github.com/usslp/usslp/edge/mesh"
	"github.com/usslp/usslp/edge/sim"
)

func newZone(t *testing.T, spec ZoneSpec) (*sim.Engine, *Zone) {
	t.Helper()
	// These tests drive labels with hand-built type 1 frames, which is the
	// compatibility posture. The end-to-end path has its own tests below.
	if spec.KeyRing == nil {
		spec.Attestation = AttestTrustController
	}
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 20240501)
	z, err := NewZone(eng, spec)
	if err != nil {
		t.Fatalf("building zone: %v", err)
	}
	done := false
	z.Form(func(time.Duration) { done = true })
	eng.RunUntil(eng.Elapsed() + 5*time.Minute)
	if !done {
		t.Fatal("zone never formed")
	}
	return eng, z
}

func update(seq int64, price int64, partial bool, imageBytes int) []byte {
	u := Update{Sequence: seq, PriceMinor: price, Currency: "GBP", Image: make([]byte, imageBytes)}
	if partial {
		u.Flags |= FlagRequestPartial
	}
	// Give the image some content so its checksum is not trivially zero.
	for i := range u.Image {
		u.Image[i] = byte(i*31 + int(seq))
	}
	b, err := EncodeUpdate(u)
	if err != nil {
		panic(err)
	}
	return b
}

func TestWireRoundTrip(t *testing.T) {
	in := Update{Sequence: 42, PriceMinor: 249, Currency: "GBP", Flags: FlagRequestPartial, Template: 2,
		Image: []byte{1, 2, 3, 4, 5}}
	b, err := EncodeUpdate(in)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	out, err := DecodeUpdate(b)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Sequence != in.Sequence || out.PriceMinor != in.PriceMinor || out.Currency != in.Currency ||
		out.Flags != in.Flags || out.Template != in.Template || string(out.Image) != string(in.Image) {
		t.Fatalf("round trip changed the update: %+v vs %+v", out, in)
	}

	// A corrupted image must be rejected rather than displayed.
	b[len(b)-1] ^= 0xFF
	if _, err := DecodeUpdate(b); err == nil {
		t.Fatal("a frame with a corrupted image decoded successfully")
	}

	ack := Ack{Sequence: 42, Status: AckApplied, RefreshMS: 1500, Partial: true, BatteryMV: 3010,
		BatteryPct: 96, TemperatureCentiC: -1850}
	got, err := DecodeAck(EncodeAck(ack))
	if err != nil {
		t.Fatalf("decoding ack: %v", err)
	}
	if got != ack {
		t.Fatalf("ack round trip changed it: %+v vs %+v", got, ack)
	}

	tel := TelemetryFrame{BatteryMV: 2990, BatteryPct: 90, TemperatureCentiC: 415, ParentLQI: 180,
		ParentRSSI: -66, RefreshCount: 1200, NFCTapCount: 7, UptimeSec: 99999, Tamper: true}
	gotTel, err := DecodeTelemetry(EncodeTelemetry(tel))
	if err != nil {
		t.Fatalf("decoding telemetry: %v", err)
	}
	if gotTel != tel {
		t.Fatalf("telemetry round trip changed it: %+v vs %+v", gotTel, tel)
	}
}

func TestPanelTimingsMatchTheHardware(t *testing.T) {
	for tier, want := range map[DisplayTier]time.Duration{
		Tier29BWR:    1500 * time.Millisecond,
		Tier42:       2000 * time.Millisecond,
		Tier585Color: 15 * time.Second,
	} {
		if got := Display(tier).FullRefresh; got != want {
			t.Fatalf("%s full refresh is %v, want %v", tier, got, want)
		}
	}
	if got := Display(Tier29BWR).PartialRefresh; got != 300*time.Millisecond {
		t.Fatalf("partial refresh is %v, want 300ms", got)
	}
	if Display(Tier585Color).SupportsPartial {
		t.Fatal("the seven-colour panel has no partial waveform; claiming one would understate its cost by fifty times")
	}
}

func TestGhostingForcesAFullRefresh(t *testing.T) {
	eng, z := newZone(t, ZoneSpec{SECID: "sec-01", StoreID: "store-1", Labels: 4, AisleLengthM: 6})
	lbl := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second) // let the wake propagate

	budget := lbl.Display().MaxPartials
	for i := 1; i <= budget+1; i++ {
		z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: update(int64(i), int64(100+i), true, 300)})
		eng.RunUntil(eng.Elapsed() + 20*time.Second)
	}
	s := lbl.Stats()
	if s.PartialRefreshes != int64(budget) {
		t.Fatalf("ran %d partial refreshes, the ghosting budget is %d", s.PartialRefreshes, budget)
	}
	if s.ForcedFulls < 1 {
		t.Fatal("the ghosting budget was never enforced; residue from eight images would be readable behind the price")
	}
	if s.RefreshCount != int64(budget+1) {
		t.Fatalf("applied %d updates, sent %d", s.RefreshCount, budget+1)
	}
}

func TestSequenceRegressionIsDiscarded(t *testing.T) {
	eng, z := newZone(t, ZoneSpec{SECID: "sec-01", StoreID: "store-1", Labels: 2, AisleLengthM: 6})
	lbl := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)

	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: update(10, 599, false, 200)})
	eng.RunUntil(eng.Elapsed() + 20*time.Second)
	if got := lbl.Stats().Sequence; got != 10 {
		t.Fatalf("label is at sequence %d after the first update, want 10", got)
	}

	// A duplicate and an out-of-order replay: both must be no-ops, and neither
	// may roll the price backwards.
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: update(10, 999, false, 200)})
	eng.RunUntil(eng.Elapsed() + 20*time.Second)
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: update(7, 1, false, 200)})
	eng.RunUntil(eng.Elapsed() + 20*time.Second)

	s := lbl.Stats()
	if s.Sequence != 10 {
		t.Fatalf("sequence moved to %d; a replayed frame rolled the price backwards", s.Sequence)
	}
	if s.Discarded != 2 {
		t.Fatalf("discarded %d frames, want 2", s.Discarded)
	}
	if s.RefreshCount != 1 {
		t.Fatalf("the panel was driven %d times for one real update", s.RefreshCount)
	}
}

func TestRefreshBlocksReceiving(t *testing.T) {
	eng, z := newZone(t, ZoneSpec{SECID: "sec-01", StoreID: "store-1", Labels: 2, AisleLengthM: 6})
	lbl := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)

	// Fire an update, then a second one while the 1.5-second waveform is still
	// running. The second must cost retries: the radio is off.
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: update(1, 100, false, 200)})
	eng.RunUntil(eng.Elapsed() + 200*time.Millisecond)

	var second mesh.TxResult
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: update(2, 200, false, 200),
		Done: func(r mesh.TxResult) { second = r }})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)

	if second.Attempts < 2 {
		t.Fatalf("the update sent mid-refresh took %d attempts; a blocked receiver must cost at least one retry", second.Attempts)
	}
}

func TestBatteryProjectionMatchesSimulationOverAYear(t *testing.T) {
	// The analytic projection is what the platform quotes. This test is what
	// makes quoting it legitimate: one label, one simulated year, event by
	// event, and the two must agree.
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 4242)
	power := DefaultPower()
	// The shipping posture: every update carries its own proof and the label
	// verifies it. That is what the year has to be simulated with, or the
	// headline figure describes a configuration nobody runs.
	f := newAttestFixture(t)
	lbl := New(eng, Config{ID: "lbl-battery", Tier: Tier29BWR, Power: power, AmbientC: 20,
		KeyRing: f.ring, Attestation: AttestEndToEnd})

	const days = 365
	const updatesPerDay = 10
	// Seven partials then a full, which is exactly the panel's ghosting budget.
	seq := int64(0)
	for d := 0; d < days; d++ {
		for u := 0; u < updatesPerDay; u++ {
			seq++
			at := time.Duration(d)*24*time.Hour + time.Duration(u)*90*time.Minute
			s := seq
			eng.At(at-eng.Elapsed(), func() {
				// Receiving the update is itself what opens the active window;
				// no separate wake is needed, and adding one would charge the
				// label energy the analytic model does not claim.
				a := f.frame(t, "lbl-battery", s, 100+s, make([]byte, 300))
				a.Flags = FlagRequestPartial
				lbl.apply(a.Update, 500, &a)
			})
		}
	}
	eng.RunUntil(365 * 24 * time.Hour)

	simulated := lbl.ChargeUsedMAH()
	w := DefaultWorkload()
	w.NFCTapsPerDay = 0
	w.TelemetryPerDay = 0
	proj := power.Project(Tier29BWR, w)
	// The projection includes self-discharge, which the event model does not
	// simulate; subtract it for the comparison.
	predicted := (proj.TotalUA - proj.SelfDischargeUA) / 1000 * 24 * days

	rel := math.Abs(simulated-predicted) / predicted
	if rel > 0.05 {
		t.Fatalf("simulated %.3f mAh over a year, analytic model predicts %.3f mAh (%.1f%% apart)",
			simulated, predicted, 100*rel)
	}

	s := lbl.Stats()
	if s.AttestationFailures != 0 || s.Verifications != int64(days*updatesPerDay) {
		t.Fatalf("the simulated year performed %d verifications and %d failures for %d updates",
			s.Verifications, s.AttestationFailures, days*updatesPerDay)
	}
	t.Logf("one label, one simulated year, verifying end to end: %.2f mAh drawn (%.1f%% of a 500 mAh cell), "+
		"%d refreshes (%d partial, %d full, %d forced), %d signature verifications, "+
		"%d beacon windows of which %d fast",
		simulated, 100*simulated/500, s.RefreshCount, s.PartialRefreshes, s.FullRefreshes, s.ForcedFulls,
		s.Verifications, s.BeaconWindows, s.FastBeaconWindows)
	t.Logf("analytic projection: %.2f uA average -> %.2f years (sleep %.2f, beacon %.2f, refresh %.2f, rx %.2f, tx %.2f, self-discharge %.2f uA)",
		proj.TotalUA, proj.Years, proj.SleepUA, proj.BeaconUA, proj.RefreshUA, proj.DataRXUA, proj.TXUA, proj.SelfDischargeUA)
}

func TestBatteryProjectionAgainstThePlatformTarget(t *testing.T) {
	// The headline claim: a label doing ten updates a day lasts seven to ten
	// years. The model is not tuned to produce it; it is reported either way.
	power := DefaultPower()
	proj := power.Project(Tier29BWR, DefaultWorkload())
	t.Logf("2.9in BWR, 10 updates/day, duty-cycled: %.2f uA -> %.2f years (beacon share %.0f%%, fast fraction %.3f%%)",
		proj.TotalUA, proj.Years, 100*proj.BeaconUA/proj.TotalUA, 100*proj.FastFraction)
	if !proj.MeetsTarget {
		t.Errorf("the duty-cycled profile projects %.2f years, outside the 7-10 year commitment", proj.Years)
	}

	// The blueprint read literally — a 250 ms listen interval with no duty
	// cycling — does not come close, and the model says so rather than being
	// adjusted until it does.
	literal := AlwaysFastPower().Project(Tier29BWR, DefaultWorkload())
	t.Logf("the same label listening every 250 ms with no duty cycling: %.2f uA -> %.3f years (%.0f days)",
		literal.TotalUA, literal.Years, literal.Life.Hours()/24)
	if literal.Years > 1 {
		t.Fatalf("a 250 ms always-on listen interval projects %.2f years; that draw is 0.208 mA and cannot be", literal.Years)
	}

	sustainable := power.SustainableBeaconInterval(Tier29BWR, DefaultWorkload(), 8)
	t.Logf("to reach 8 years with no duty cycling at all, the listen interval would have to be %v", sustainable.Round(time.Second))

	// The colour panel cannot take this workload on a 500 mAh cell, and that is
	// a hardware fact the platform has to plan around rather than hide.
	colour := power.Project(Tier585Color, DefaultWorkload())
	t.Logf("5.85in seven-colour, same workload: %.1f uA -> %.2f years (a 15-second waveform at 35 mA)",
		colour.TotalUA, colour.Years)
	if colour.Years >= proj.Years {
		t.Fatal("the colour panel cannot cost less than the BWR panel; the refresh model is wrong")
	}

	chilled := DefaultWorkload()
	chilled.AmbientC = -20
	frozen := power.Project(Tier29BWR, chilled)
	t.Logf("the same label in a -20C freezer case: %.2f years (%.0f%% of rated capacity)",
		frozen.Years, 100*CapacityDerating(-20))
}

func TestDutyCyclingIsWhatCostsLatency(t *testing.T) {
	eng, z := newZone(t, ZoneSpec{SECID: "sec-01", StoreID: "store-1", Labels: 8, AisleLengthM: 8})
	lbl := z.Labels()[0]
	power := DefaultPower()

	// Asleep: the label is only reachable in a window that comes round every
	// thirty seconds.
	var cold mesh.TxResult
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: update(1, 100, false, 300),
		Done: func(r mesh.TxResult) { cold = r }})
	eng.RunUntil(eng.Elapsed() + 2*time.Minute)
	if !cold.Delivered {
		t.Fatalf("cold delivery failed: %v", cold.Err)
	}

	// In the active window: reachable inside the platform's SEC-to-label budget.
	z.OpenActiveWindow(5 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)
	var warm mesh.TxResult
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: update(2, 200, false, 300),
		Done: func(r mesh.TxResult) { warm = r }})
	eng.RunUntil(eng.Elapsed() + time.Minute)
	if !warm.Delivered {
		t.Fatalf("warm delivery failed: %v", warm.Err)
	}

	if warm.Elapsed >= cold.Elapsed {
		t.Fatalf("an update to a woken label took %v, one to a sleeping label %v; the active window buys nothing",
			warm.Elapsed, cold.Elapsed)
	}
	if warm.Elapsed > 300*time.Millisecond {
		t.Fatalf("in-window SEC-to-label delivery took %v, budget is 300ms", warm.Elapsed)
	}
	if cold.Elapsed > power.BeaconSlow+time.Second {
		t.Fatalf("a sleeping label answered in %v, which is longer than its %v listen interval", cold.Elapsed, power.BeaconSlow)
	}
	t.Logf("SEC-to-label: %v asleep (30s listen interval), %v in the active window (250ms interval)",
		cold.Elapsed.Round(time.Millisecond), warm.Elapsed.Round(time.Millisecond))
}

func TestExhaustedCellTakesTheLabelOffTheAir(t *testing.T) {
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 8)
	power := DefaultPower()
	power.CapacityMAH = 0.05 // a nearly flat cell, so exhaustion is reachable in a test
	z, err := NewZone(eng, ZoneSpec{SECID: "sec-01", StoreID: "store-1", Labels: 4,
		AisleLengthM: 6, Power: power, Attestation: AttestTrustController})
	if err != nil {
		t.Fatalf("building zone: %v", err)
	}
	z.Form(func(time.Duration) {})
	eng.RunUntil(eng.Elapsed() + 2*time.Minute)

	lbl := z.Labels()[0]
	var died bool
	lbl.OnEvent(func(e Event) {
		if e.Kind == EventBatteryDead {
			died = true
		}
	})
	z.OpenActiveWindow(30 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)
	for i := int64(1); i <= 40 && !died; i++ {
		z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: update(i, 100+i, false, 300)})
		eng.RunUntil(eng.Elapsed() + 20*time.Second)
	}
	if !died {
		t.Fatal("the label never reported an exhausted cell")
	}
	if !lbl.Dead() {
		t.Fatal("the label reports a dead cell but still thinks it is alive")
	}
	if z.Net.Alive(lbl.NodeID()) {
		t.Fatal("an exhausted label is still on the mesh")
	}
	if mv, pct := lbl.Battery(); mv > 2000 || pct > 2 {
		t.Fatalf("an exhausted cell reports %d mV / %d%%", mv, pct)
	}
}

func TestZoneSizesItsRelayBackbone(t *testing.T) {
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 77)
	z, err := NewZone(eng, ZoneSpec{SECID: "sec-01", StoreID: "store-1", Labels: 480, AisleLengthM: 40})
	if err != nil {
		t.Fatalf("building zone: %v", err)
	}
	var formed time.Duration
	ok := false
	z.Form(func(d time.Duration) { formed, ok = d, true })
	eng.RunUntil(eng.Elapsed() + 5*time.Minute)
	if !ok {
		t.Fatal("a 480-label zone never formed")
	}
	joined := z.Net.Stats().Joined
	if joined < 470 {
		t.Fatalf("only %d of 480 labels joined; the relay backbone is undersized", joined)
	}
	depth := 0
	for _, st := range z.Net.Topology() {
		if st.Depth > depth {
			depth = st.Depth
		}
	}
	t.Logf("480-label zone with %d relays formed in %v, %d joined, deepest node at hop %d",
		len(z.Relays()), formed.Round(time.Millisecond), joined, depth)
}

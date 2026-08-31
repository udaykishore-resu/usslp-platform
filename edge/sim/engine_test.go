package sim

import (
	"context"
	"sort"
	"testing"
	"time"
)

func TestEngineRunsEventsInTimeOrder(t *testing.T) {
	e := New(time.Unix(1700000000, 0).UTC(), 1)
	var order []time.Duration
	for _, d := range []time.Duration{50 * time.Millisecond, time.Millisecond, 900 * time.Microsecond, time.Second} {
		d := d
		e.At(d, func() { order = append(order, e.Elapsed()) })
	}
	e.Drain(0)

	want := []time.Duration{900 * time.Microsecond, time.Millisecond, 50 * time.Millisecond, time.Second}
	if len(order) != len(want) {
		t.Fatalf("ran %d events, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("event %d ran at %v, want %v", i, order[i], want[i])
		}
	}
	if !sort.SliceIsSorted(order, func(i, j int) bool { return order[i] < order[j] }) {
		t.Fatal("virtual time went backwards")
	}
}

func TestEngineTiesRunInInsertionOrder(t *testing.T) {
	e := New(time.Time{}, 7)
	var got []int
	for i := 0; i < 32; i++ {
		i := i
		e.At(time.Second, func() { got = append(got, i) })
	}
	e.Drain(0)
	for i, v := range got {
		if v != i {
			t.Fatalf("position %d holds event %d; equal deadlines must keep insertion order", i, v)
		}
	}
}

func TestEngineJumpsAcrossIdleTime(t *testing.T) {
	// The property the battery projection depends on: crossing a year of
	// virtual time costs one pop, not a year of ticks.
	e := New(time.Unix(0, 0).UTC(), 3)
	year := 365 * 24 * time.Hour
	fired := false
	e.At(year, func() { fired = true })

	start := time.Now()
	e.Drain(0)
	realCost := time.Since(start)

	if !fired {
		t.Fatal("the event a year out never ran")
	}
	if e.Elapsed() != year {
		t.Fatalf("virtual clock at %v, want %v", e.Elapsed(), year)
	}
	if realCost > time.Second {
		t.Fatalf("crossing a simulated year took %v of real time; the engine is stepping, not jumping", realCost)
	}
}

func TestEngineNestedSchedulingRuns(t *testing.T) {
	e := New(time.Time{}, 11)
	count := 0
	var tick func()
	tick = func() {
		count++
		if count < 100 {
			e.At(250*time.Millisecond, tick)
		}
	}
	e.At(0, tick)
	e.Drain(0)
	if count != 100 {
		t.Fatalf("ran %d nested events, want 100", count)
	}
	if got, want := e.Elapsed(), 99*250*time.Millisecond; got != want {
		t.Fatalf("clock at %v, want %v", got, want)
	}
}

func TestEngineCancel(t *testing.T) {
	e := New(time.Time{}, 5)
	ran := false
	ev := e.At(time.Second, func() { ran = true })
	e.Cancel(ev)
	e.Cancel(ev) // idempotent: a retry timer races its own acknowledgement
	e.Drain(0)
	if ran {
		t.Fatal("a cancelled event ran")
	}
	if _, ok := e.NextAt(); ok {
		t.Fatal("cancelled event still reported as next")
	}
}

func TestEngineRunUntilAdvancesPastIdle(t *testing.T) {
	e := New(time.Time{}, 2)
	e.At(time.Second, func() {})
	e.RunUntil(time.Hour)
	if got := e.Elapsed(); got != time.Hour {
		t.Fatalf("clock at %v after RunUntil(1h), want 1h; the power model integrates over elapsed time and needs it exact", got)
	}
}

func TestEnginePacedRunHonoursContext(t *testing.T) {
	e := New(time.Time{}, 9)
	e.At(time.Hour, func() {})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.Run(ctx, 1); err == nil {
		t.Fatal("paced run of an hour returned before its context expired")
	}
}

func TestEngineDeterministicUnderSeed(t *testing.T) {
	run := func() []float64 {
		e := New(time.Time{}, 42)
		var out []float64
		for i := 0; i < 64; i++ {
			e.At(time.Duration(i)*time.Millisecond, func() { out = append(out, e.Rand().Float64()) })
		}
		e.Drain(0)
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("draw %d differs between runs: %v vs %v", i, a[i], b[i])
		}
	}
}

func TestRandJitterStaysInBand(t *testing.T) {
	r := NewRand(17)
	base := int64(15 * time.Millisecond)
	for i := 0; i < 10000; i++ {
		v := r.Jitter(base, 0.2)
		if v < int64(0.8*float64(base))-1 || v > int64(1.2*float64(base))+1 {
			t.Fatalf("jittered %d outside +/-20%% of %d", v, base)
		}
	}
}

func TestEngineStopIsIdempotent(t *testing.T) {
	e := New(time.Time{}, 1)
	e.At(time.Second, func() { t.Error("event ran after Stop") })
	e.Stop()
	e.Stop()
	if ev := e.At(time.Second, func() {}); ev != nil {
		t.Fatal("scheduling on a stopped engine should return nil")
	}
	if e.Step() {
		t.Fatal("a stopped engine stepped")
	}
}

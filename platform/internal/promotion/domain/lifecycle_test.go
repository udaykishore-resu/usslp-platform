package domain

import (
	"testing"
	"time"
)

// mondaySchedule is the case the whole design exists for: "starts Monday",
// meaning local Monday in every store.
func mondaySchedule() Schedule {
	return Schedule{
		// Monday 2 March 2026 through Monday 9 March 2026, local.
		StartLocal: "2026-03-02T00:00", EndLocal: "2026-03-09T00:00",
	}
}

// TestActivationIsLocalMondayInEveryZone is the timezone-correctness test
// across three zones spanning most of the world's offsets.
func TestActivationIsLocalMondayInEveryZone(t *testing.T) {
	zones := []struct {
		name string
		// wantStartUTC is local Monday midnight expressed in UTC, worked out by
		// hand from each zone's March 2026 offset.
		wantStartUTC time.Time
	}{
		// Auckland is UTC+13 on 2 March 2026 (NZDT, daylight saving ends on the
		// 5th of April), so local midnight Monday is 11:00 UTC on Sunday.
		{"Pacific/Auckland", time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)},
		// London is UTC+0 in early March (BST starts on the 29th).
		{"Europe/London", time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)},
		// New York is UTC-5 until DST starts on the 8th, so local midnight
		// Monday is 05:00 UTC.
		{"America/New_York", time.Date(2026, 3, 2, 5, 0, 0, 0, time.UTC)},
	}

	s := mondaySchedule()
	for _, z := range zones {
		t.Run(z.name, func(t *testing.T) {
			win, err := s.ResolveWindow(z.name)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if !win.Start.Equal(z.wantStartUTC) {
				t.Errorf("start = %s, want %s", win.Start.Format(time.RFC3339), z.wantStartUTC.Format(time.RFC3339))
			}
			// One second before local Monday the promotion is not running.
			if on, err := s.ActiveInStore(z.name, z.wantStartUTC.Add(-time.Second)); err != nil || on {
				t.Errorf("active a second before local Monday (err %v)", err)
			}
			// One second after, it is.
			if on, err := s.ActiveInStore(z.name, z.wantStartUTC.Add(time.Second)); err != nil || !on {
				t.Errorf("not active a second after local Monday (err %v)", err)
			}
		})
	}

	// The three zones must genuinely start at different instants: a bug that
	// resolved everything to UTC would pass the London case alone.
	auckland, _ := s.ResolveWindow("Pacific/Auckland")
	london, _ := s.ResolveWindow("Europe/London")
	newYork, _ := s.ResolveWindow("America/New_York")
	if !auckland.Start.Before(london.Start) || !london.Start.Before(newYork.Start) {
		t.Errorf("the three zones did not start in order: %s, %s, %s",
			auckland.Start, london.Start, newYork.Start)
	}
	if d := newYork.Start.Sub(auckland.Start); d != 18*time.Hour {
		t.Errorf("Auckland and New York started %v apart, want 18h", d)
	}
}

func TestAbsoluteWindowIsTheSameInstantEverywhere(t *testing.T) {
	start := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	s := Schedule{AbsoluteStart: &start, AbsoluteEnd: &end}
	for _, zone := range []string{"Pacific/Auckland", "Europe/London", "America/New_York"} {
		win, err := s.ResolveWindow(zone)
		if err != nil {
			t.Fatalf("resolve %s: %v", zone, err)
		}
		if !win.Start.Equal(start) || !win.End.Equal(end) {
			t.Errorf("%s resolved an absolute window to %s..%s", zone, win.Start, win.End)
		}
	}
}

// TestSpringForwardGapResolvesDeterministically covers the morning a local time
// does not exist.
func TestSpringForwardGapResolvesDeterministically(t *testing.T) {
	// In London, 01:00 on 29 March 2026 jumps to 02:00: 01:30 does not exist.
	s := Schedule{StartLocal: "2026-03-29T01:30", EndLocal: "2026-03-30T00:00"}
	win, err := s.ResolveWindow("Europe/London")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The non-existent time normalises forward out of the gap, so the promotion
	// starts as soon as 01:30 has meaningfully arrived. What matters is that it
	// is deterministic and inside the morning.
	if win.Start.Before(time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)) ||
		win.Start.After(time.Date(2026, 3, 29, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("a spring-forward start resolved to %s, outside the morning", win.Start)
	}
	again, err := s.ResolveWindow("Europe/London")
	if err != nil || !again.Start.Equal(win.Start) {
		t.Errorf("resolving the same gap twice gave %s then %s", win.Start, again.Start)
	}
}

// TestAutumnAmbiguityTakesTheFirstOccurrence covers the morning a local time
// happens twice.
func TestAutumnAmbiguityTakesTheFirstOccurrence(t *testing.T) {
	// In London, 02:00 on 25 October 2026 falls back to 01:00: 01:30 happens
	// twice, at 00:30 UTC (BST) and at 01:30 UTC (GMT).
	s := Schedule{StartLocal: "2026-10-25T01:30", EndLocal: "2026-10-26T00:00"}
	win, err := s.ResolveWindow("Europe/London")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	first := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)
	if !win.Start.Equal(first) {
		t.Errorf("an ambiguous start resolved to %s, want the first occurrence %s",
			win.Start.Format(time.RFC3339), first.Format(time.RFC3339))
	}
}

func TestWeekdayAndDailyWindows(t *testing.T) {
	// A happy hour: weekdays only, 15:00 to 18:00 local.
	s := Schedule{
		StartLocal: "2026-03-02T00:00", EndLocal: "2026-03-16T00:00",
		DaysOfWeek: []int{1, 2, 3, 4, 5},
		DailyStart: "15:00", DailyEnd: "18:00",
	}
	london, err := LoadZone("Europe/London")
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	cases := []struct {
		name  string
		local time.Time
		want  bool
	}{
		{"Monday mid-afternoon", time.Date(2026, 3, 2, 16, 0, 0, 0, london), true},
		{"Monday morning", time.Date(2026, 3, 2, 9, 0, 0, 0, london), false},
		{"Monday evening", time.Date(2026, 3, 2, 19, 0, 0, 0, london), false},
		{"Saturday mid-afternoon", time.Date(2026, 3, 7, 16, 0, 0, 0, london), false},
		{"exactly at the open", time.Date(2026, 3, 3, 15, 0, 0, 0, london), true},
		{"exactly at the close", time.Date(2026, 3, 3, 18, 0, 0, 0, london), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.ActiveInStore("Europe/London", c.local.UTC())
			if err != nil {
				t.Fatalf("active: %v", err)
			}
			if got != c.want {
				t.Errorf("active = %v, want %v at %s local", got, c.want, c.local.Format(time.RFC3339))
			}
		})
	}
}

func TestDailyWindowWrappingPastMidnight(t *testing.T) {
	// An overnight promotion: 22:00 to 02:00.
	s := Schedule{
		StartLocal: "2026-03-02T00:00", EndLocal: "2026-03-16T00:00",
		DailyStart: "22:00", DailyEnd: "02:00",
	}
	london, _ := LoadZone("Europe/London")
	cases := []struct {
		local time.Time
		want  bool
	}{
		{time.Date(2026, 3, 3, 23, 0, 0, 0, london), true},
		{time.Date(2026, 3, 3, 1, 0, 0, 0, london), true},
		{time.Date(2026, 3, 3, 12, 0, 0, 0, london), false},
		{time.Date(2026, 3, 3, 21, 59, 0, 0, london), false},
	}
	for _, c := range cases {
		got, err := s.ActiveInStore("Europe/London", c.local.UTC())
		if err != nil {
			t.Fatalf("active: %v", err)
		}
		if got != c.want {
			t.Errorf("at %s local, active = %v, want %v", c.local.Format("15:04"), got, c.want)
		}
	}
}

func TestNextTransitionsDeduplicatesByZone(t *testing.T) {
	s := mondaySchedule()
	zones := StoreZones{
		"store-1": "Europe/London",
		"store-2": "Europe/London",
		"store-3": "America/New_York",
		"store-4": "Pacific/Auckland",
	}
	before := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	tps, err := s.NextTransitions(zones, before)
	if err != nil {
		t.Fatalf("transitions: %v", err)
	}
	// Three distinct zones, two transitions each: six wake-ups, not eight.
	if len(tps) != 6 {
		t.Fatalf("got %d transitions, want 6: %+v", len(tps), tps)
	}
	for i := 1; i < len(tps); i++ {
		if tps[i].At.Before(tps[i-1].At) {
			t.Errorf("transitions are not sorted at %d", i)
		}
	}
	// The first thing to happen anywhere is Auckland activating.
	if tps[0].To != StateActive || tps[0].Zone != "Pacific/Auckland" {
		t.Errorf("the first transition is %+v, want Auckland activating", tps[0])
	}
}

func TestUnknownZoneIsAnErrorNotASilentUTC(t *testing.T) {
	s := mondaySchedule()
	if _, err := s.ResolveWindow("Middle/Earth"); err == nil {
		t.Fatal("an unknown zone resolved without error")
	}
	// An empty zone is UTC, deliberately and visibly.
	win, err := s.ResolveWindow("")
	if err != nil {
		t.Fatalf("empty zone: %v", err)
	}
	if !win.Start.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("an empty zone resolved to %s, want UTC midnight", win.Start)
	}
}

func TestLifecycleStateMachine(t *testing.T) {
	legal := [][2]State{
		{StateDraft, StateScheduled},
		{StateDraft, StateCancelled},
		{StateScheduled, StateActive},
		{StateScheduled, StateCancelled},
		{StateScheduled, StateExpired},
		{StateActive, StateExpired},
		{StateActive, StateCancelled},
	}
	for _, m := range legal {
		if err := Transition(m[0], m[1]); err != nil {
			t.Errorf("%s -> %s should be legal: %v", m[0], m[1], err)
		}
	}
	illegal := [][2]State{
		{StateScheduled, StateDraft},
		{StateActive, StateDraft},
		{StateActive, StateScheduled},
		{StateExpired, StateActive},
		{StateCancelled, StateActive},
		{StateExpired, StateCancelled},
		{StateCancelled, StateExpired},
	}
	for _, m := range illegal {
		if err := Transition(m[0], m[1]); err == nil {
			t.Errorf("%s -> %s should be illegal", m[0], m[1])
		}
	}
	if !StateExpired.Terminal() || !StateCancelled.Terminal() {
		t.Error("expired and cancelled must be terminal")
	}
	if StateActive.Terminal() {
		t.Error("active must not be terminal")
	}
}

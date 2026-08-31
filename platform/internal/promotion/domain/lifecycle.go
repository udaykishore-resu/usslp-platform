package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// State is where a promotion sits in its lifecycle.
type State string

// The lifecycle states.
const (
	// StateDraft is being authored. It affects nothing.
	StateDraft State = "draft"
	// StateScheduled is approved and waiting for its window.
	StateScheduled State = "scheduled"
	// StateActive is running. Note that "active" is a property of the
	// promotion, not of every store: a promotion is active as a whole while
	// individual stores enter and leave it as their local windows open, which
	// is why ActiveInStore exists alongside this.
	StateActive State = "active"
	// StateExpired ran and finished.
	StateExpired State = "expired"
	// StateCancelled was stopped by an operator. Distinguished from expired
	// because the two mean different things in a performance report and in an
	// audit: one ran its course, the other was pulled.
	StateCancelled State = "cancelled"
)

// ErrTransition marks an illegal lifecycle move.
var ErrTransition = errors.New("promotion: illegal state transition")

// allowedTransitions is the state machine.
//
// It is a table rather than a switch so that the whole machine is visible at
// once. The absent edges are the interesting ones: nothing returns to draft
// once scheduled (an approved promotion that needs changing is cancelled and
// re-authored, so the audit trail shows two documents rather than one that
// changed under review), and nothing leaves expired or cancelled.
var allowedTransitions = map[State]map[State]bool{
	StateDraft:     {StateScheduled: true, StateCancelled: true},
	StateScheduled: {StateActive: true, StateCancelled: true, StateExpired: true},
	StateActive:    {StateExpired: true, StateCancelled: true},
	StateExpired:   {},
	StateCancelled: {},
}

// CanTransition reports whether a move is legal.
func CanTransition(from, to State) bool { return allowedTransitions[from][to] }

// Transition validates a lifecycle move.
func Transition(from, to State) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrTransition, from, to)
	}
	return nil
}

// Schedule is a promotion's activation window.
//
// # Why local time is the default
//
// "Starts Monday" means local Monday. A national promotion that activated at
// UTC midnight would go live at 4 pm Sunday in Auckland and 7 pm Sunday in New
// York — before the newspaper advertising it had been printed — and would end
// mid-morning on its last day in the west. Every retailer's answer is the same:
// the window is wall-clock time in the store's own zone. The platform therefore
// stores a *local* wall-clock window plus each store's zone, and resolves the
// two into an instant per store.
//
// A promotion that genuinely must be simultaneous everywhere — a flash sale
// timed to a broadcast — sets Absolute, and then the wall-clock fields are
// ignored.
type Schedule struct {
	// StartLocal and EndLocal are wall-clock times in the store's own zone,
	// formatted "2006-01-02T15:04:05" with no offset. The absence of an offset
	// is the point: an offset would pin them to one zone.
	StartLocal string `json:"start_local,omitempty"`
	EndLocal   string `json:"end_local,omitempty"`
	// Absolute, when set, overrides the wall-clock window with instants that
	// are the same moment everywhere.
	AbsoluteStart *time.Time `json:"absolute_start,omitempty"`
	AbsoluteEnd   *time.Time `json:"absolute_end,omitempty"`
	// DaysOfWeek restricts the promotion to particular local weekdays, 0 =
	// Sunday. Empty means every day. This is how "weekends only" is expressed
	// without authoring eight separate promotions.
	DaysOfWeek []int `json:"days_of_week,omitempty"`
	// DailyStart and DailyEnd restrict it to a local time-of-day window,
	// "15:00" to "18:00" for a happy hour. Empty means all day. A window whose
	// end is before its start wraps past midnight, which is what an overnight
	// promotion needs.
	DailyStart string `json:"daily_start,omitempty"`
	DailyEnd   string `json:"daily_end,omitempty"`
}

// localLayout is the wall-clock format. Seconds are optional on input.
const localLayout = "2006-01-02T15:04:05"
const localLayoutMinutes = "2006-01-02T15:04"

// Validate checks the schedule is usable.
func (s Schedule) Validate() error {
	absolute := s.AbsoluteStart != nil || s.AbsoluteEnd != nil
	wall := s.StartLocal != "" || s.EndLocal != ""
	if absolute && wall {
		return errors.New("a schedule has both an absolute and a wall-clock window; they cannot both apply")
	}
	if !absolute && !wall {
		return errors.New("a schedule needs either a wall-clock or an absolute window")
	}
	if absolute {
		if s.AbsoluteStart == nil || s.AbsoluteEnd == nil {
			return errors.New("an absolute window needs both a start and an end")
		}
		if !s.AbsoluteEnd.After(*s.AbsoluteStart) {
			return errors.New("absolute_end must be after absolute_start")
		}
	} else {
		start, err := parseLocal(s.StartLocal)
		if err != nil {
			return fmt.Errorf("start_local: %w", err)
		}
		end, err := parseLocal(s.EndLocal)
		if err != nil {
			return fmt.Errorf("end_local: %w", err)
		}
		if !end.After(start) {
			return errors.New("end_local must be after start_local")
		}
	}
	seen := map[int]bool{}
	for _, d := range s.DaysOfWeek {
		if d < 0 || d > 6 {
			return fmt.Errorf("days_of_week contains %d, which is not 0..6", d)
		}
		if seen[d] {
			return fmt.Errorf("days_of_week repeats %d", d)
		}
		seen[d] = true
	}
	if (s.DailyStart == "") != (s.DailyEnd == "") {
		return errors.New("daily_start and daily_end must be set together")
	}
	if s.DailyStart != "" {
		if _, err := parseClock(s.DailyStart); err != nil {
			return fmt.Errorf("daily_start: %w", err)
		}
		if _, err := parseClock(s.DailyEnd); err != nil {
			return fmt.Errorf("daily_end: %w", err)
		}
	}
	return nil
}

func parseLocal(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, errors.New("empty")
	}
	if t, err := time.Parse(localLayout, v); err == nil {
		return t, nil
	}
	t, err := time.Parse(localLayoutMinutes, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a local wall-clock time like 2026-03-02T09:00", v)
	}
	return t, nil
}

// minutesOfDay is a time of day as minutes past local midnight.
type minutesOfDay int

func parseClock(v string) (minutesOfDay, error) {
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%q is not a clock time like 15:04", v)
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("%q has an invalid hour", v)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q has an invalid minute", v)
	}
	return minutesOfDay(h*60 + m), nil
}

// StoreWindow is a promotion's window resolved into absolute instants for one
// store.
type StoreWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Zone is the IANA location the window was resolved in.
	Zone string `json:"zone"`
}

// ResolveWindow turns the schedule into absolute instants for a store's zone.
//
// # Daylight saving
//
// A wall-clock time of 01:30 on a spring-forward morning may not exist, and one
// on an autumn morning may exist twice. Both cases are resolved here rather
// than left to the standard library, whose documented behaviour for the
// ambiguous case is that the choice "is not guaranteed":
//
//   - A time in the gap normalises forward out of it, so the promotion starts
//     as soon as its wall-clock time has meaningfully arrived.
//   - An ambiguous time takes the *earliest* of its occurrences, so the
//     promotion starts once, at the first moment the shelf could legitimately
//     show it, and an expiry does not linger for an extra hour.
//
// Pinning both is what stops a national promotion drifting by an hour twice a
// year, on the two mornings nobody is watching for it.
func (s Schedule) ResolveWindow(zone string) (StoreWindow, error) {
	if s.AbsoluteStart != nil && s.AbsoluteEnd != nil {
		return StoreWindow{Start: s.AbsoluteStart.UTC(), End: s.AbsoluteEnd.UTC(), Zone: "UTC"}, nil
	}
	loc, err := LoadZone(zone)
	if err != nil {
		return StoreWindow{}, err
	}
	start, err := parseLocal(s.StartLocal)
	if err != nil {
		return StoreWindow{}, err
	}
	end, err := parseLocal(s.EndLocal)
	if err != nil {
		return StoreWindow{}, err
	}
	return StoreWindow{
		Start: resolveLocal(start, loc).UTC(),
		End:   resolveLocal(end, loc).UTC(),
		Zone:  loc.String(),
	}, nil
}

// dstSearchWindow is how far back resolveLocal looks for an earlier occurrence
// of an ambiguous wall-clock time.
//
// Three hours covers every daylight-saving shift in the IANA database, whose
// largest modern transitions are two hours (Lord Howe's is thirty minutes, and
// several historical ones are two hours). Searching at fifteen-minute
// granularity covers the sub-hour offsets that exist in Australia and Nepal.
const dstSearchWindow = 3 * time.Hour

// resolveLocal converts a wall-clock time in a location to an instant, taking
// the earliest occurrence when the wall clock is ambiguous.
func resolveLocal(wall time.Time, loc *time.Location) time.Time {
	t := time.Date(wall.Year(), wall.Month(), wall.Day(),
		wall.Hour(), wall.Minute(), wall.Second(), 0, loc)
	// Walk backwards looking for an earlier instant that renders the same wall
	// clock. On an ordinary day there is none and the loop is pure overhead
	// twelve times; on the one morning a year it exists, it is the difference
	// between starting at 01:30 BST and at 01:30 GMT.
	earliest := t
	for step := 15 * time.Minute; step <= dstSearchWindow; step += 15 * time.Minute {
		cand := t.Add(-step)
		local := cand.In(loc)
		if local.Year() == wall.Year() && local.Month() == wall.Month() && local.Day() == wall.Day() &&
			local.Hour() == wall.Hour() && local.Minute() == wall.Minute() && local.Second() == wall.Second() {
			earliest = cand
		}
	}
	return earliest
}

// LoadZone resolves an IANA location name.
//
// An empty zone is UTC rather than the process's local zone. A service whose
// promotion timing depends on the TZ environment variable of whichever node the
// pod landed on is a service that activates promotions at different times on
// different replicas.
func LoadZone(zone string) (*time.Location, error) {
	if zone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown time zone %q: %v", ErrInvalidRule, zone, err)
	}
	return loc, nil
}

// ActiveInStore reports whether the promotion is running in a store at an
// instant, applying the window, the weekday restriction and the daily window —
// all in the store's own local time.
func (s Schedule) ActiveInStore(zone string, at time.Time) (bool, error) {
	win, err := s.ResolveWindow(zone)
	if err != nil {
		return false, err
	}
	at = at.UTC()
	if at.Before(win.Start) || !at.Before(win.End) {
		return false, nil
	}
	if len(s.DaysOfWeek) == 0 && s.DailyStart == "" {
		return true, nil
	}
	loc, err := LoadZone(zone)
	if err != nil {
		return false, err
	}
	local := at.In(loc)
	if len(s.DaysOfWeek) > 0 {
		ok := false
		for _, d := range s.DaysOfWeek {
			if int(local.Weekday()) == d {
				ok = true
				break
			}
		}
		if !ok {
			return false, nil
		}
	}
	if s.DailyStart == "" {
		return true, nil
	}
	from, err := parseClock(s.DailyStart)
	if err != nil {
		return false, err
	}
	to, err := parseClock(s.DailyEnd)
	if err != nil {
		return false, err
	}
	now := minutesOfDay(local.Hour()*60 + local.Minute())
	if from <= to {
		return now >= from && now < to, nil
	}
	// A window that wraps past midnight: 22:00 to 02:00 is late evening or
	// early morning, not the twenty hours in between.
	return now >= from || now < to, nil
}

// StoreZones maps stores to IANA locations. It is the input to a
// timezone-correct activation sweep.
type StoreZones map[string]string

// NextTransitions returns the instants at which a promotion changes state in
// each store, sorted, so the scheduler can sleep until the next one rather than
// polling every store every minute.
//
// A national promotion across a chain spanning six zones has six activation
// instants and six expiry instants, not one of each. Returning them explicitly
// is what lets the scheduler wake exactly twelve times instead of scanning
// forty thousand stores a minute.
func (s Schedule) NextTransitions(zones StoreZones, after time.Time) ([]Transitionpoint, error) {
	seen := map[string]bool{}
	var out []Transitionpoint
	for store, zone := range zones {
		win, err := s.ResolveWindow(zone)
		if err != nil {
			return nil, fmt.Errorf("store %s: %w", store, err)
		}
		for _, tp := range []Transitionpoint{
			{At: win.Start, To: StateActive, Zone: win.Zone},
			{At: win.End, To: StateExpired, Zone: win.Zone},
		} {
			if !tp.At.After(after) {
				continue
			}
			// Stores sharing a zone share an instant; one wake-up serves all of
			// them.
			k := tp.Zone + "|" + tp.To.String() + "|" + tp.At.Format(time.RFC3339)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, tp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Transitionpoint is one scheduled state change.
type Transitionpoint struct {
	At   time.Time `json:"at"`
	To   State     `json:"to"`
	Zone string    `json:"zone"`
}

// String renders a state.
func (s State) String() string { return string(s) }

// Terminal reports whether a state can never change again.
func (s State) Terminal() bool { return s == StateExpired || s == StateCancelled }

package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	// tzdata is embedded so that quiet hours work identically wherever the
	// service runs.
	//
	// A quiet window is defined in a store's local time and is the difference
	// between labels refreshing at 3 a.m. and labels refreshing while a
	// customer is reading them. Resolving "Europe/London" requires a time zone
	// database, and a scratch container image has none — time.LoadLocation
	// returns an error, and the natural handler for that error is to fall back
	// to UTC, which in July would shift a London store's quiet window an hour
	// into trading. Embedding the database costs about 450 KiB of binary and
	// removes an entire class of environment-dependent wrongness from a
	// safety-critical calculation.
	_ "time/tzdata"
)

// QuietHours is a store's local window during which firmware may be delivered.
//
// # Why this exists at all
//
// An E-Ink refresh is not silent from the customer's point of view: the panel
// inverts, flashes and settles over a second and a half, and a label doing that
// while someone is reading a price looks broken. A firmware update is worse
// than a price change — it is a download, a flash and a reboot, and the label
// is blank or stale throughout. Retailers therefore contract for a window, and
// the window is theirs, not the platform's.
//
// # Why the time zone is stored and not derived
//
// The obvious implementation stores an offset. It is wrong twice a year. A
// store in London on "02:00–05:00" means 02:00 London, which is 01:00 UTC in
// winter and 02:00 UTC in summer, and on the two changeover nights the window
// is four hours long or two. Storing the IANA zone and converting at evaluation
// time is the only way to get those nights right, and those nights are exactly
// when an overnight rollout is running.
type QuietHours struct {
	// Start and End are local wall-clock times in "HH:MM" form. A window whose
	// end is before its start wraps midnight, which is what almost every real
	// window does.
	Start string `json:"start"`
	End   string `json:"end"`
	// TimeZone is an IANA location name, e.g. "Europe/London". Empty means UTC,
	// which is correct only for a test.
	TimeZone string `json:"time_zone,omitempty"`
}

// AlwaysAllowed is the window that permits delivery at any time. It is the
// right configuration for a controller or a gateway — mains powered, no display
// a customer can see — and the wrong one for a label.
var AlwaysAllowed = QuietHours{}

// InZone returns the window evaluated in a different time zone.
//
// A rollout is created once and runs across a retailer's whole estate, and
// "02:00 to 05:00" means two o'clock in each store's own morning, not two
// o'clock in head office's. The zone therefore belongs to the store rather than
// to the job, and the job's own zone is the fallback for a store that has not
// declared one. An empty zone leaves the window unchanged rather than silently
// resetting it to UTC.
func (q QuietHours) InZone(tz string) QuietHours {
	if tz == "" {
		return q
	}
	q.TimeZone = tz
	return q
}

// Configured reports whether a window was set. An unset window allows delivery
// at any time.
func (q QuietHours) Configured() bool { return q.Start != "" && q.End != "" }

// Location resolves the window's time zone.
func (q QuietHours) Location() (*time.Location, error) {
	if q.TimeZone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(q.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("%w: time zone %q: %v", ErrInvalid, q.TimeZone, err)
	}
	return loc, nil
}

// Validate rejects a window that cannot be evaluated.
func (q QuietHours) Validate() error {
	if !q.Configured() {
		if q.Start != "" || q.End != "" {
			return fmt.Errorf("%w: quiet hours need both a start and an end", ErrInvalid)
		}
		return nil
	}
	if _, err := parseWallClock(q.Start); err != nil {
		return err
	}
	if _, err := parseWallClock(q.End); err != nil {
		return err
	}
	if q.Start == q.End {
		return fmt.Errorf("%w: quiet hours start and end are both %s, which is a zero-length window",
			ErrInvalid, q.Start)
	}
	if _, err := q.Location(); err != nil {
		return err
	}
	return nil
}

// Allows reports whether firmware may be delivered at instant t.
//
// A window that wraps midnight — and 02:00–05:00 does not, but 22:00–06:00 does
// — is handled by treating the comparison as a union of two ranges rather than
// an intersection of one, which is the only formulation that is correct for
// both shapes.
func (q QuietHours) Allows(t time.Time) bool {
	if !q.Configured() {
		return true
	}
	loc, err := q.Location()
	if err != nil {
		// An unresolvable zone must not silently widen the window into trading
		// hours. Refusing to deliver is the safe direction: a delayed rollout
		// is an inconvenience, a label refreshing at midday is a complaint.
		return false
	}
	start, err := parseWallClock(q.Start)
	if err != nil {
		return false
	}
	end, err := parseWallClock(q.End)
	if err != nil {
		return false
	}
	local := t.In(loc)
	mins := local.Hour()*60 + local.Minute()
	if start < end {
		return mins >= start && mins < end
	}
	// The window wraps midnight.
	return mins >= start || mins < end
}

// NextOpen returns the next instant at or after t when the window is open.
//
// It walks forward in minutes rather than computing the boundary arithmetically
// because the arithmetic has to be right across a daylight-saving transition,
// where a local wall-clock time can fail to exist or occur twice. Stepping
// through actual instants and asking [Allows] delegates that entirely to the
// time package, which is the only component in this system that gets it right.
// The walk is bounded at two days: no window shorter than a day can be more
// than that away, and an unbounded search on a misconfigured window would spin.
func (q QuietHours) NextOpen(t time.Time) (time.Time, bool) {
	if !q.Configured() {
		return t, true
	}
	if q.Allows(t) {
		return t, true
	}
	cursor := t.Truncate(time.Minute)
	for i := 0; i < 2*24*60; i++ {
		cursor = cursor.Add(time.Minute)
		if q.Allows(cursor) {
			return cursor, true
		}
	}
	return time.Time{}, false
}

// parseWallClock parses "HH:MM" into minutes since local midnight.
func parseWallClock(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: %q is not a HH:MM wall-clock time", ErrInvalid, s)
	}
	hh, err := strconv.Atoi(parts[0])
	if err != nil || hh < 0 || hh > 23 {
		return 0, fmt.Errorf("%w: %q has an out-of-range hour", ErrInvalid, s)
	}
	mm, err := strconv.Atoi(parts[1])
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("%w: %q has an out-of-range minute", ErrInvalid, s)
	}
	return hh*60 + mm, nil
}

// String renders the window for logs and for the canon.OTAJob.QuietHours field.
func (q QuietHours) String() string {
	if !q.Configured() {
		return ""
	}
	if q.TimeZone == "" {
		return q.Start + "-" + q.End
	}
	return q.Start + "-" + q.End + "@" + q.TimeZone
}

// ParseQuietHours reads the "HH:MM-HH:MM@Zone" form back. It is the inverse of
// String and exists because canon.OTAJob carries quiet hours as a single string
// on the event stream.
func ParseQuietHours(s string) (QuietHours, error) {
	if s == "" {
		return QuietHours{}, nil
	}
	zone := ""
	if at := strings.IndexByte(s, '@'); at >= 0 {
		zone = s[at+1:]
		s = s[:at]
	}
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return QuietHours{}, fmt.Errorf("%w: quiet hours %q are not HH:MM-HH:MM", ErrInvalid, s)
	}
	q := QuietHours{Start: parts[0], End: parts[1], TimeZone: zone}
	if err := q.Validate(); err != nil {
		return QuietHours{}, err
	}
	return q, nil
}

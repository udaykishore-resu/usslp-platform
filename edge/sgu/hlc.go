// Package sgu implements the USSLP Store Gateway Unit: the Tier 3 device that
// is a store's brain and its bulkhead against the wide-area network.
//
// The platform's defining commitment is that the cloud is an optimisation, not
// a dependency. A store must keep pricing correctly — including activating
// promotions that were scheduled before the link dropped — through a complete
// WAN outage, with no label downtime, and reconcile cleanly when the link
// returns. Everything in this package exists to make that true:
//
//   - It runs the store's MQTT broker. Controllers connect to it, not to the
//     cloud, so when the cloud link drops the bridge stops and the broker does
//     not. That is the entire mechanism behind zero label downtime.
//   - It detects WAN loss with hysteresis, so a two-second blip does not flap a
//     store into and out of autonomy, and buffers every upstream message in a
//     durable, bounded queue while the link is down.
//   - It holds a full replica of the store's label state, a local promotion
//     schedule, and the Tier-1 pricing guard rails, so an autonomous store can
//     answer a controller's cold-start query, activate a scheduled promotion on
//     its own clock, and refuse a price that breaks a regulatory floor, with
//     nothing upstream of it running at all.
//   - It reconciles on reconnect with a hybrid logical clock and an explicit
//     conflict policy, rather than by hoping the two sides agree.
package sgu

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Hybrid logical clock
//
// Reconciliation needs a total order over events that happened on two sides of
// a broken link. Wall-clock timestamps cannot provide one: the whole premise of
// an autonomous store is that it has been out of contact, which is precisely
// when its NTP discipline has been unavailable longest. A store whose real-time
// clock has drifted four minutes fast would win every last-writer-wins merge
// for as long as the drift lasts, and would do so silently.
//
// A hybrid logical clock keeps the useful property of a wall clock — the
// timestamps are approximately real times, so a human can read them and an
// auditor can correlate them — while adding a logical counter that enforces
// causality regardless of drift. Two events the gateway knows the order of are
// ordered correctly whatever the RTC says, and the drift itself becomes a
// measurable quantity rather than an invisible bias.
//
// The clock is not the whole answer, and it is not claimed to be: for pricing,
// the conflict policy overrides it outright (see Merge). What it provides is a
// deterministic, explainable order for everything else, and a number an
// operator can look at when a store's merges start going the wrong way.
// ---------------------------------------------------------------------------

// ErrClockSkew reports a remote timestamp so far ahead of local physical time
// that adopting it would corrupt the local clock. It is surfaced rather than
// swallowed because it means someone's NTP is broken and a human should know.
var ErrClockSkew = errors.New("sgu: remote timestamp exceeds the acceptable clock skew")

// HLC is a hybrid logical clock timestamp.
type HLC struct {
	// WallMS is the physical component in Unix milliseconds. It only ever moves
	// forward, and only as fast as physical time or a peer's clock requires.
	WallMS int64 `json:"wall_ms"`
	// Logical is the counter that breaks ties within a millisecond and carries
	// causality when the physical component cannot move.
	Logical uint32 `json:"logical"`
	// NodeID makes the order total rather than merely partial. Two nodes can
	// produce identical (wall, logical) pairs, and a merge that cannot decide
	// between them is a merge that produces different answers on the two sides.
	NodeID string `json:"node_id"`
}

// Compare orders two timestamps: -1, 0 or +1.
func (h HLC) Compare(o HLC) int {
	switch {
	case h.WallMS != o.WallMS:
		if h.WallMS < o.WallMS {
			return -1
		}
		return 1
	case h.Logical != o.Logical:
		if h.Logical < o.Logical {
			return -1
		}
		return 1
	case h.NodeID != o.NodeID:
		if h.NodeID < o.NodeID {
			return -1
		}
		return 1
	}
	return 0
}

// Before reports whether h strictly precedes o.
func (h HLC) Before(o HLC) bool { return h.Compare(o) < 0 }

// After reports whether h strictly follows o.
func (h HLC) After(o HLC) bool { return h.Compare(o) > 0 }

// IsZero reports whether the timestamp was never set.
func (h HLC) IsZero() bool { return h.WallMS == 0 && h.Logical == 0 }

// Time returns the physical component as a time.
func (h HLC) Time() time.Time { return time.UnixMilli(h.WallMS).UTC() }

// String renders the timestamp for logs and for the reconciliation report,
// where an operator needs to see both halves.
func (h HLC) String() string {
	return fmt.Sprintf("%s+%d@%s", h.Time().Format(time.RFC3339Nano), h.Logical, h.NodeID)
}

// ParseHLC reads back the String form.
func ParseHLC(s string) (HLC, error) {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return HLC{}, fmt.Errorf("sgu: %q is not a hybrid logical clock timestamp", s)
	}
	node := s[at+1:]
	rest := s[:at]
	plus := strings.LastIndex(rest, "+")
	if plus < 0 {
		return HLC{}, fmt.Errorf("sgu: %q has no logical component", s)
	}
	logical, err := strconv.ParseUint(rest[plus+1:], 10, 32)
	if err != nil {
		return HLC{}, fmt.Errorf("sgu: logical component of %q: %w", s, err)
	}
	t, err := time.Parse(time.RFC3339Nano, rest[:plus])
	if err != nil {
		return HLC{}, fmt.Errorf("sgu: physical component of %q: %w", s, err)
	}
	return HLC{WallMS: t.UnixMilli(), Logical: uint32(logical), NodeID: node}, nil
}

// DefaultMaxSkew is how far ahead of local physical time a remote timestamp may
// be before it is refused.
//
// Ten minutes is chosen from what actually goes wrong: a store gateway whose
// NTP has been unreachable for a day drifts by seconds, not minutes, because
// its RTC is disciplined by a temperature-compensated oscillator. Minutes of
// difference means a misconfigured time zone, a dead RTC battery, or a peer
// that has been tampered with — none of which should be allowed to drag this
// store's clock along with it.
const DefaultMaxSkew = 10 * time.Minute

// Clock issues and merges hybrid logical clock timestamps.
//
// Safe for concurrent use: a gateway stamps events from its broker's dispatch
// pool, its bridge, its scheduler and its HTTP surface simultaneously.
type Clock struct {
	mu       sync.Mutex
	nodeID   string
	now      func() time.Time
	maxSkew  time.Duration
	last     HLC
	skew     time.Duration
	worst    time.Duration
	rejected uint64
	adopted  uint64
}

// NewClock creates a clock for a node. now may be nil, meaning time.Now.
func NewClock(nodeID string, now func() time.Time, maxSkew time.Duration) *Clock {
	if now == nil {
		now = time.Now
	}
	if maxSkew <= 0 {
		maxSkew = DefaultMaxSkew
	}
	return &Clock{nodeID: nodeID, now: now, maxSkew: maxSkew}
}

// Now issues a timestamp for a locally originated event.
//
// The physical component never goes backwards even if the system clock does,
// which is what stops an NTP step correction from making a store re-issue
// timestamps it has already used.
func (c *Clock) Now() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt := c.now().UnixMilli()
	if pt > c.last.WallMS {
		c.last = HLC{WallMS: pt, Logical: 0, NodeID: c.nodeID}
	} else {
		c.last.Logical++
		c.last.NodeID = c.nodeID
	}
	return c.last
}

// Observe merges a received timestamp into the local clock and returns the
// timestamp to attach to whatever the receipt causes.
//
// This is the Kulkarni update rule. Its effect is that anything this gateway
// does after receiving a message from the cloud is ordered strictly after that
// message, whatever either machine's real-time clock believes — which is the
// property reconciliation actually needs.
//
// A remote timestamp more than maxSkew ahead of local physical time is refused:
// the local clock is not advanced, ErrClockSkew is returned, and the returned
// timestamp is a purely local one. Adopting it would let one badly configured
// peer drag a whole store's clock into the future and win every subsequent
// merge on the way back.
func (c *Clock) Observe(remote HLC) (HLC, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt := c.now().UnixMilli()
	skew := time.Duration(remote.WallMS-pt) * time.Millisecond
	c.skew = skew
	if abs := absDuration(skew); abs > c.worst {
		c.worst = abs
	}

	if skew > c.maxSkew {
		c.rejected++
		local := c.tickLocked(pt)
		return local, fmt.Errorf("%w: peer %s is %v ahead of local time",
			ErrClockSkew, remote.NodeID, skew.Round(time.Second))
	}
	c.adopted++

	maxWall := pt
	if remote.WallMS > maxWall {
		maxWall = remote.WallMS
	}
	if c.last.WallMS > maxWall {
		maxWall = c.last.WallMS
	}
	switch {
	case maxWall == c.last.WallMS && maxWall == remote.WallMS:
		if remote.Logical > c.last.Logical {
			c.last.Logical = remote.Logical
		}
		c.last.Logical++
	case maxWall == c.last.WallMS:
		c.last.Logical++
	case maxWall == remote.WallMS:
		c.last.WallMS = remote.WallMS
		c.last.Logical = remote.Logical + 1
	default:
		c.last.WallMS = maxWall
		c.last.Logical = 0
	}
	c.last.NodeID = c.nodeID
	return c.last, nil
}

// tickLocked advances the clock without reference to a peer.
func (c *Clock) tickLocked(pt int64) HLC {
	if pt > c.last.WallMS {
		c.last = HLC{WallMS: pt, Logical: 0, NodeID: c.nodeID}
	} else {
		c.last.Logical++
		c.last.NodeID = c.nodeID
	}
	return c.last
}

// Last returns the most recently issued timestamp without advancing the clock.
func (c *Clock) Last() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// SkewReport is what the gateway publishes and its diagnostics page shows about
// the trustworthiness of its own clock.
type SkewReport struct {
	// Last is the most recently measured difference between a peer's physical
	// clock and this gateway's: positive means the peer is ahead.
	Last time.Duration `json:"last"`
	// Worst is the largest absolute difference seen.
	Worst time.Duration `json:"worst"`
	// Rejected counts timestamps refused for exceeding the skew limit.
	Rejected uint64 `json:"rejected"`
	// Adopted counts timestamps merged normally.
	Adopted uint64 `json:"adopted"`
	// MaxSkew is the configured limit.
	MaxSkew time.Duration `json:"max_skew"`
}

// Skew returns the clock's trust report.
//
// This is the number that matters after an outage. A store that has been
// autonomous for six hours activated its promotions on its own clock; how far
// that clock had drifted by the time the link came back is the difference
// between "the promotion started on time" and "the promotion started four
// minutes early and somebody bought forty units at the wrong price".
func (c *Clock) Skew() SkewReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return SkewReport{Last: c.skew, Worst: c.worst, Rejected: c.rejected,
		Adopted: c.adopted, MaxSkew: c.maxSkew}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

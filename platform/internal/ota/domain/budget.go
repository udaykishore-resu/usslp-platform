package domain

import (
	"sync"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// DefaultMaxConcurrentPerSEC is how many labels one Shelf Edge Controller may
// be downloading firmware to at once.
//
// # Why this number is small
//
// A controller's Zigbee network is a single shared 2.4 GHz channel carrying, in
// order of importance: price updates, delivery acknowledgements, telemetry, and
// firmware. A 250 kbit/s nominal channel yields perhaps 40 kbit/s of usable
// application throughput once mesh routing, retries and the labels' duty cycles
// are accounted for. A 300 KiB image is therefore about a minute of exclusive
// airtime per label — and the price path's slice of the three-second SLO is 300
// milliseconds for the whole Zigbee hop.
//
// Four concurrent downloads leaves the channel with headroom for the traffic
// the shelf edge is actually for. Eight would halve the rollout's duration and
// put price updates behind a firmware queue, which is the wrong trade in a
// building where a wrong price is a regulatory matter and a slow rollout is
// not.
const DefaultMaxConcurrentPerSEC = 4

// BandwidthBudget rations concurrent firmware downloads per controller.
//
// It is a semaphore per controller rather than a global rate limit because the
// contended resource is per controller: two labels on opposite sides of a store
// share nothing, and a global limit would either starve a large store or
// swamp a small one.
//
// It is safe for concurrent use.
type BandwidthBudget struct {
	max int

	mu       sync.Mutex
	inflight map[canon.SECID]map[canon.LabelID]struct{}
}

// NewBandwidthBudget returns a budget allowing max concurrent downloads per
// controller. A max of zero or less uses DefaultMaxConcurrentPerSEC.
func NewBandwidthBudget(max int) *BandwidthBudget {
	if max <= 0 {
		max = DefaultMaxConcurrentPerSEC
	}
	return &BandwidthBudget{max: max, inflight: make(map[canon.SECID]map[canon.LabelID]struct{})}
}

// Max returns the per-controller concurrency limit.
func (b *BandwidthBudget) Max() int { return b.max }

// Acquire reserves a download slot for a label on a controller.
//
// It is keyed by label as well as controller so that re-dispatching a device
// that is already downloading — which happens when a controller misses an
// acknowledgement and the controller loop retries — does not consume a second
// slot. Without that, a flapping label would monopolise a controller's whole
// budget.
func (b *BandwidthBudget) Acquire(sec canon.SECID, label canon.LabelID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.inflight[sec]
	if set == nil {
		set = make(map[canon.LabelID]struct{}, b.max)
		b.inflight[sec] = set
	}
	if _, already := set[label]; already {
		return true
	}
	if len(set) >= b.max {
		return false
	}
	set[label] = struct{}{}
	return true
}

// Release frees a label's slot. It is safe to call for a label that holds none.
func (b *BandwidthBudget) Release(sec canon.SECID, label canon.LabelID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.inflight[sec]
	if set == nil {
		return
	}
	delete(set, label)
	if len(set) == 0 {
		delete(b.inflight, sec)
	}
}

// InFlight returns how many downloads a controller is carrying.
func (b *BandwidthBudget) InFlight(sec canon.SECID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.inflight[sec])
}

// Total returns the number of downloads in flight across every controller.
func (b *BandwidthBudget) Total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, set := range b.inflight {
		n += len(set)
	}
	return n
}

// Reset frees every slot. It is called when a job reaches a terminal state.
func (b *BandwidthBudget) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inflight = make(map[canon.SECID]map[canon.LabelID]struct{})
}

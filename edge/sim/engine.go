// Package sim is the deterministic discrete-event engine underneath USSLP's
// edge simulators.
//
// Why an engine at all: the numbers the platform reports about the edge — the
// SEC-to-label latency percentiles in the SLO, the seven-to-ten-year battery
// projection, the ninety-second mesh rebuild budget — are only meaningful if
// the model that produced them advances time the way the hardware does. Wall
// clock and goroutines cannot do that: 40,000 labels cannot each own a
// goroutine, and a year of battery drain cannot be waited out.
//
// # Why a min-heap and not a timing wheel
//
// The obvious structure for a device simulator is a hierarchical timing wheel:
// O(1) insertion, O(1) expiry, and it is what an RTOS uses. It was rejected
// here for one concrete reason. A wheel advances tick by tick, so the cost of
// simulating an interval is proportional to the *interval*, not to the number
// of events in it. The battery projection has to cross a simulated year in
// which one label experiences roughly 3,650 events; a 1 ms wheel would step
// 31.5 billion times to deliver them. A binary heap keyed on the event's
// deadline lets virtual time jump straight from one event to the next, so that
// same year costs 3,650 pops.
//
// The usual counter-argument — that a heap is O(log n) where a wheel is O(1) —
// does not bite at this fleet size. The load test runs 5,000 labels in one
// process; log2(5000) is 13 comparisons, about 60 ns, against a per-event
// budget measured in microseconds. And the largest source of events in a naive
// label model, the 250 ms beacon, is not in the queue at all: see
// labelsim.Label, which resolves the next beacon boundary arithmetically and
// integrates the listening charge over elapsed time rather than scheduling
// 160,000 wake-ups per simulated second.
//
// # Determinism
//
// Events with equal deadlines run in insertion order, and every random draw in
// the edge simulators comes from Engine.Rand, which is a seeded PCG. A run is
// therefore reproducible provided scheduling itself is deterministic: that
// holds when every At call originates inside an engine callback or from a
// single driver goroutine. Engine is safe for concurrent use so that a Shelf
// Edge Controller running on its own goroutines can inject transmissions, but a
// test that wants byte-identical results across runs must drive it from one
// goroutine.
package sim

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"
)

// ErrStopped is returned by Run when the engine has been closed. It is a
// sentinel rather than nil so that a caller draining an engine in a goroutine
// can distinguish "the work finished" from "somebody shut us down".
var ErrStopped = errors.New("sim: engine stopped")

// Event is a scheduled callback. The zero value is not usable; obtain one from
// Engine.At. Holding one is only useful in order to Cancel it, which is how a
// mesh retry timer is abandoned when the acknowledgement arrives first.
type Event struct {
	at        time.Duration
	seq       uint64
	fn        func()
	index     int
	cancelled bool
}

// At reports the virtual instant the event is scheduled for, measured from the
// engine's epoch.
func (e *Event) At() time.Duration { return e.at }

// eventQueue is a min-heap ordered by deadline and then by insertion sequence.
// The sequence tiebreak is what makes two events scheduled for the same
// microsecond run in a defined order, which in turn is what makes a whole
// simulated store reproducible from a seed.
type eventQueue []*Event

func (q eventQueue) Len() int { return len(q) }

func (q eventQueue) Less(i, j int) bool {
	if q[i].at != q[j].at {
		return q[i].at < q[j].at
	}
	return q[i].seq < q[j].seq
}

func (q eventQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *eventQueue) Push(x any) {
	e := x.(*Event)
	e.index = len(*q)
	*q = append(*q, e)
}

func (q *eventQueue) Pop() any {
	old := *q
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*q = old[:n-1]
	return e
}

// Engine is a virtual clock with a queue of scheduled callbacks.
//
// Safe for concurrent use. Callbacks always run on whichever goroutine is
// stepping the engine, never on the goroutine that scheduled them, and they run
// with the engine's lock released so a callback may schedule more work.
type Engine struct {
	mu      sync.Mutex
	epoch   time.Time
	now     time.Duration
	seq     uint64
	q       eventQueue
	stopped bool
	// fired counts callbacks executed. It is the engine's own health metric:
	// a scenario that reports a plausible latency but fired ten events did not
	// really exercise anything.
	fired uint64

	// wake interrupts a paced Run that is sleeping until the event it knew
	// about when it went to sleep. Without it a runner that sees nothing due for
	// thirty seconds sleeps for thirty seconds, and an event scheduled a
	// millisecond later by another goroutine — a controller submitting a
	// transmission, which is exactly what happens in a live store — waits the
	// whole thirty. Buffered depth one because the only information the signal
	// carries is "look again".
	wake chan struct{}

	rng *Rand
}

// New creates an engine whose virtual time starts at epoch, with a random
// source seeded from seed.
//
// The epoch is a real timestamp because the events the simulators produce carry
// canon timestamps that must look like real ones: a LabelDelivered whose
// DeliveredAt is in 1970 is not a message any downstream consumer will treat
// sensibly.
func New(epoch time.Time, seed uint64) *Engine {
	if epoch.IsZero() {
		epoch = time.Unix(0, 0).UTC()
	}
	e := &Engine{epoch: epoch.UTC(), rng: NewRand(seed), wake: make(chan struct{}, 1)}
	heap.Init(&e.q)
	return e
}

// Epoch returns the real instant that virtual time zero corresponds to.
func (e *Engine) Epoch() time.Time { return e.epoch }

// Now returns the current virtual instant as a wall-clock-shaped time.
func (e *Engine) Now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epoch.Add(e.now)
}

// Elapsed returns virtual time since the epoch. Simulator internals prefer it
// over Now because durations subtract without allocating.
func (e *Engine) Elapsed() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

// Rand returns the engine's seeded random source. Every stochastic decision in
// the edge simulators — shadow fading, packet loss, CSMA backoff, join order —
// draws from here so that a seed reproduces a run exactly.
func (e *Engine) Rand() *Rand { return e.rng }

// Fired reports how many callbacks the engine has executed.
func (e *Engine) Fired() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fired
}

// Pending reports how many events are queued, cancelled ones included.
func (e *Engine) Pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.q)
}

// At schedules fn to run after d of virtual time. A non-positive d schedules it
// at the current instant, which runs before any later event but after anything
// already queued for this instant.
//
// It returns nil when the engine is stopped, so that a shutdown race in a
// simulator degrades to "the work never happens" rather than to a panic.
func (e *Engine) At(d time.Duration, fn func()) *Event {
	if fn == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil
	}
	if d < 0 {
		d = 0
	}
	ev := &Event{at: e.now + d, seq: e.seq, fn: fn}
	e.seq++
	heap.Push(&e.q, ev)
	// Only the head of the queue is worth waking a paced runner for; anything
	// later cannot change what it should do next.
	if e.q[0] == ev {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
	return ev
}

// Cancel withdraws a scheduled event. Cancelling an event that has already run,
// or one that is nil, is a no-op — both happen routinely when a mesh
// acknowledgement races its own retry timer.
func (e *Engine) Cancel(ev *Event) {
	if ev == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if ev.cancelled || ev.index < 0 {
		ev.cancelled = true
		return
	}
	ev.cancelled = true
	heap.Remove(&e.q, ev.index)
}

// NextAt reports the virtual instant of the earliest queued event and whether
// there is one. A paced runner uses it to decide how long to sleep.
func (e *Engine) NextAt() (time.Duration, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for len(e.q) > 0 && e.q[0].cancelled {
		heap.Pop(&e.q)
	}
	if len(e.q) == 0 {
		return 0, false
	}
	return e.q[0].at, true
}

// Step runs the earliest queued event, advancing virtual time to its deadline.
// It reports whether an event ran.
func (e *Engine) Step() bool {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return false
	}
	for len(e.q) > 0 && e.q[0].cancelled {
		heap.Pop(&e.q)
	}
	if len(e.q) == 0 {
		e.mu.Unlock()
		return false
	}
	ev := heap.Pop(&e.q).(*Event)
	if ev.at > e.now {
		e.now = ev.at
	}
	e.fired++
	e.mu.Unlock()
	ev.fn()
	return true
}

// RunUntil advances virtual time to the epoch plus until, executing every event
// scheduled at or before it, and reports how many ran.
//
// Virtual time ends exactly at until whether or not any event was that late, so
// a caller that runs for a simulated hour gets an hour on the clock even if the
// store was idle for the last fifty minutes. That matters for the power model,
// which integrates over elapsed time.
func (e *Engine) RunUntil(until time.Duration) int {
	n := 0
	for {
		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			return n
		}
		for len(e.q) > 0 && e.q[0].cancelled {
			heap.Pop(&e.q)
		}
		if len(e.q) == 0 || e.q[0].at > until {
			if until > e.now {
				e.now = until
			}
			e.mu.Unlock()
			return n
		}
		ev := heap.Pop(&e.q).(*Event)
		if ev.at > e.now {
			e.now = ev.at
		}
		e.fired++
		e.mu.Unlock()
		ev.fn()
		n++
	}
}

// RunFor advances virtual time by d.
func (e *Engine) RunFor(d time.Duration) int {
	e.mu.Lock()
	until := e.now + d
	e.mu.Unlock()
	return e.RunUntil(until)
}

// Drain runs queued events until none remain or maxEvents have run, whichever
// comes first, and reports how many ran. A zero or negative maxEvents means no
// limit.
//
// The limit exists because a misconfigured simulator can schedule work that
// schedules more work forever — a label that retries an undeliverable frame
// with no attempt cap, say — and a test that hangs is far less useful than one
// that fails with a count.
func (e *Engine) Drain(maxEvents int) int {
	n := 0
	for maxEvents <= 0 || n < maxEvents {
		if !e.Step() {
			break
		}
		n++
	}
	return n
}

// Run advances virtual time paced against the wall clock until ctx is done or
// the queue empties.
//
// speed is the ratio of virtual to real time: 1 runs in real time, 60 runs a
// minute per second, and any value <= 0 runs as fast as the CPU allows. Paced
// mode exists for the demo binary, where a human watching a store come up wants
// the mesh to form at a speed they can follow; every test uses RunUntil.
func (e *Engine) Run(ctx context.Context, speed float64) error {
	// Pacing is measured from where virtual time already is, not from the epoch.
	// An engine that has been fast-forwarded through a mesh formation and is
	// then handed to a paced runner must carry on from there, not sleep off the
	// three minutes it has already simulated.
	start := time.Now()
	startVirtual := e.Elapsed()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			return ErrStopped
		}
		e.mu.Unlock()

		next, ok := e.NextAt()
		if !ok {
			// An empty queue in paced mode is normal: the store is idle and
			// something outside the engine (an MQTT message, an operator) will
			// schedule the next event. Yield rather than spin.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-e.wake:
			case <-time.After(time.Millisecond):
			}
			continue
		}
		if speed > 0 {
			target := start.Add(time.Duration(float64(next-startVirtual) / speed))
			if wait := time.Until(target); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-e.wake:
					// Something scheduled work ahead of what we were waiting
					// for. Re-evaluate rather than sleeping through it.
					timer.Stop()
					continue
				case <-timer.C:
				}
			}
		}
		e.Step()
	}
}

// Stop makes the engine refuse further scheduling and stops any Run loop. It is
// idempotent, because shutting down a simulated store races with the store's
// own goroutines by construction.
func (e *Engine) Stop() {
	e.mu.Lock()
	e.stopped = true
	e.q = e.q[:0]
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

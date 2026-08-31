package eventstore

import (
	"context"
	"errors"
	"sync"
)

// Handler receives events in global position order. Returning an error stops
// the subscription and surfaces the error to whoever started it.
type Handler func(ctx context.Context, rec Recorded) error

// subscriber is one live tail. It buffers into an unbounded slice guarded by a
// condition variable rather than a fixed channel because the append path fans
// out while holding the append lock: a bounded channel would let one slow
// projection stall every price change in the store.
type subscriber struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []Recorded
	closed bool
}

func newSubscriber() *subscriber {
	s := &subscriber{}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *subscriber) enqueue(recs []Recorded) {
	s.mu.Lock()
	if !s.closed {
		s.queue = append(s.queue, recs...)
		s.cond.Signal()
	}
	s.mu.Unlock()
}

// take blocks until events are buffered or the subscriber is closed, then
// hands over everything buffered so far.
func (s *subscriber) take() ([]Recorded, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.queue) == 0 && !s.closed {
		s.cond.Wait()
	}
	if len(s.queue) == 0 {
		return nil, false
	}
	out := s.queue
	s.queue = nil
	return out, true
}

func (s *subscriber) close() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// SubscribeAll delivers every event from fromPosition (inclusive) onwards to
// handler, in global position order, first from history and then live, and
// blocks until the context is cancelled, the store is closed, or the handler
// returns an error. Cancelling the context is a clean stop and returns nil.
//
// The switch from history to the live tail is where a naive implementation
// loses events, so it is worth being explicit about why this one cannot:
//
//  1. The subscription registers as a live listener *first*. From that instant
//     every appended event is copied into its buffer. Nothing that happens from
//     now on can be missed, however long the next step takes.
//  2. Only then does it read history with ReadAll, repeatedly, until a read
//     comes back empty. Because appends make an event durable before they fan
//     it out, an event is either already durable (so this read finds it) or not
//     yet durable (so it is appended after registration and lands in the
//     buffer). There is no third case, and therefore no gap.
//  3. The two sources overlap — an event written just before the last history
//     read is in both — so the subscription tracks the highest position it has
//     delivered and drops anything at or below it. That makes the overlap
//     harmless and delivery exactly-once for the lifetime of the subscription.
//
// The fan-out itself happens while the append lock is held, so buffered events
// are always in position order; the subscription never has to sort or reorder.
func (s *Store) SubscribeAll(ctx context.Context, fromPosition int64, handler Handler) error {
	if handler == nil {
		return errors.New("eventstore: nil subscription handler")
	}
	if s.closed.Load() {
		return ErrClosed
	}
	if fromPosition < 1 {
		fromPosition = 1
	}

	sub := newSubscriber()
	s.subMu.Lock()
	if s.closed.Load() {
		s.subMu.Unlock()
		return ErrClosed
	}
	s.subs[sub] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, sub)
		s.subMu.Unlock()
		sub.close()
	}()

	// A condition variable cannot select on a context, so a watcher closes the
	// subscriber to wake the blocked take().
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			sub.close()
		case <-watchDone:
		}
	}()

	last := fromPosition - 1

	// Phase 1: history. The buffer is already filling behind us.
	const historyBatch = 512
	for {
		if ctx.Err() != nil {
			return nil
		}
		recs, err := s.ReadAll(ctx, last+1, historyBatch)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if len(recs) == 0 {
			break
		}
		for _, r := range recs {
			if err := handler(ctx, r); err != nil {
				return err
			}
			last = r.Position
		}
	}

	// Phase 2: the live tail, with the overlap discarded by position.
	for {
		recs, ok := sub.take()
		if !ok {
			if ctx.Err() != nil {
				return nil
			}
			return ErrClosed
		}
		for _, r := range recs {
			if r.Position <= last {
				continue
			}
			if err := handler(ctx, r); err != nil {
				return err
			}
			last = r.Position
		}
	}
}

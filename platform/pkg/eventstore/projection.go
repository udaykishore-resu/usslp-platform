package eventstore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// Apply is a projection's event handler.
//
// The batch handed in is the projection's unit of work: whatever read-model
// rows the handler stages on it are committed atomically together with the
// projection's checkpoint. That is the whole reason the signature carries a
// batch rather than nothing. A projection that wrote its rows and then saved
// its checkpoint separately would, on a crash between the two, either
// double-apply an event or skip one — and a shelf-price read model that has
// skipped a price change is a price on a shelf that nobody authorised.
//
// A handler must therefore write only through the batch. It may return an error
// to stop the projection; the batch is then discarded and the checkpoint does
// not move, so the event is retried on the next run.
type Apply func(ctx context.Context, rec Recorded, b *kvstore.Batch) error

// Projection is a named read-model builder with a durable checkpoint.
//
// Projections are the read side of CQRS here. They are disposable by design:
// the events are the truth, so a projection whose shape was wrong is fixed by
// deleting its checkpoint and its rows and replaying the log from zero, with no
// migration and no downtime for the write side.
//
// A Projection is safe for concurrent use, but running the same projection name
// from two goroutines at once would have them fight over one checkpoint; Run
// enforces one active run per Projection value.
type Projection struct {
	store *Store
	name  string
	apply Apply

	mu      sync.Mutex
	running bool
}

// NewProjection creates a named projection over the store. The name is the
// identity of its durable checkpoint, so it must be stable across restarts and
// unique within the store.
func (s *Store) NewProjection(name string, apply Apply) (*Projection, error) {
	if name == "" {
		return nil, errors.New("eventstore: projection needs a name")
	}
	if apply == nil {
		return nil, errors.New("eventstore: projection needs a handler")
	}
	if s.closed.Load() {
		return nil, ErrClosed
	}
	return &Projection{store: s, name: name, apply: apply}, nil
}

// Name returns the projection's name.
func (p *Projection) Name() string { return p.name }

// Position returns the durable checkpoint: the global position of the last
// event this projection has committed. Zero means it has consumed nothing.
func (p *Projection) Position() (int64, error) {
	if p.store.closed.Load() {
		return 0, ErrClosed
	}
	return p.store.readInt64(cKey(p.name))
}

// Reset clears the checkpoint so the next run replays from the beginning. It
// does not touch the rows the projection previously wrote; owning and clearing
// those is the caller's job, because only the caller knows their key space.
func (p *Projection) Reset() error {
	if p.store.closed.Load() {
		return ErrClosed
	}
	return p.store.kv.Delete(cKey(p.name))
}

// handle applies one event and advances the checkpoint in a single atomic
// write.
func (p *Projection) handle(ctx context.Context, rec Recorded) error {
	b := p.store.kv.NewBatch()
	if err := p.apply(ctx, rec, b); err != nil {
		return fmt.Errorf("eventstore: projection %s at position %d: %w", p.name, rec.Position, err)
	}
	b.Put(cKey(p.name), be8(rec.Position))
	return b.Write()
}

// CatchUp applies every event already in the store from the checkpoint onwards
// and returns the position it reached. It does not follow the live tail, which
// makes it the right call for a rebuild that must finish before a replica
// starts serving reads.
func (p *Projection) CatchUp(ctx context.Context) (int64, error) {
	if p.store.closed.Load() {
		return 0, ErrClosed
	}
	pos, err := p.Position()
	if err != nil {
		return 0, err
	}
	const batch = 512
	for {
		if err := ctx.Err(); err != nil {
			return pos, err
		}
		recs, err := p.store.ReadAll(ctx, pos+1, batch)
		if err != nil {
			return pos, err
		}
		if len(recs) == 0 {
			return pos, nil
		}
		for _, r := range recs {
			if err := p.handle(ctx, r); err != nil {
				return pos, err
			}
			pos = r.Position
		}
	}
}

// Run applies history from the checkpoint and then follows the live tail,
// blocking until the context is cancelled, the store closes, or the handler
// fails. Cancelling is a clean stop and returns nil, with the checkpoint left
// exactly where the last committed event put it, so a restart resumes without
// replaying or skipping anything.
func (p *Projection) Run(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return fmt.Errorf("eventstore: projection %s is already running", p.name)
	}
	p.running = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	pos, err := p.Position()
	if err != nil {
		return err
	}
	return p.store.SubscribeAll(ctx, pos+1, p.handle)
}

// Rebuild throws the projection away and builds it again from position zero.
// clear, when non-nil, is called after the checkpoint is reset and before any
// event is applied; it is where the caller deletes the rows the old projection
// wrote. Rebuild then catches up on all history and returns, leaving the
// decision to follow the live tail to the caller.
func (p *Projection) Rebuild(ctx context.Context, clear func(context.Context) error) (int64, error) {
	if err := p.Reset(); err != nil {
		return 0, err
	}
	if clear != nil {
		if err := clear(ctx); err != nil {
			return 0, fmt.Errorf("eventstore: projection %s clear: %w", p.name, err)
		}
	}
	return p.CatchUp(ctx)
}

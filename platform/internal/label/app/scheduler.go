package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/label/ports"
)

// Scheduler defaults.
const (
	// DefaultScheduleTick is how often the runner looks for due changes.
	//
	// Fifteen seconds is chosen against what a shopper notices rather than
	// against what a clock can do. A promotion that starts "at 9am" and appears
	// on the shelf by 9:00:15 is indistinguishable from one that appeared at
	// 9:00:00 to everyone except the person holding the stopwatch, and a
	// one-second tick across a fleet of replicas is a hundred times the
	// index scans for no perceptible gain.
	DefaultScheduleTick = 15 * time.Second
	// DefaultScheduleBatch bounds how many activations one tick starts. It is
	// the backpressure valve for the 9am cliff, when a chain's entire morning
	// promotion set becomes due in the same second.
	DefaultScheduleBatch = 500
)

// ScheduledPriceRunner activates future-dated price changes at their effective
// time.
//
// It exists because a price with a future effective time must not sit in the
// aggregate waiting for something to touch the label — the whole point of
// scheduling a promotion for 9am is that nobody has to be there at 9am. The
// runner is the only component in the service that acts without an inbound
// event, which is why it is deliberately the simplest: an ordered due-index, a
// bounded batch, and the same commit path every other price change takes.
//
// Several replicas may run it. Activation is safe under concurrency because the
// aggregate's optimistic concurrency check makes exactly one of them land and
// the losers see the schedule already gone.
type ScheduledPriceRunner struct {
	handler   *UpdatePriceHandler
	schedules ports.ScheduleStore
	deps      Deps
	tick      time.Duration
	batch     int
}

// ScheduledPriceRunnerConfig configures the runner.
type ScheduledPriceRunnerConfig struct {
	// Tick is the polling interval. Zero means DefaultScheduleTick.
	Tick time.Duration
	// Batch bounds activations per tick. Zero means DefaultScheduleBatch.
	Batch int
}

// NewScheduledPriceRunner builds the runner.
func NewScheduledPriceRunner(handler *UpdatePriceHandler, deps Deps, cfg ScheduledPriceRunnerConfig) (*ScheduledPriceRunner, error) {
	deps = deps.withDefaults()
	if handler == nil {
		return nil, fmt.Errorf("%w: UpdatePriceHandler", ErrMissingDependency)
	}
	if deps.Schedules == nil {
		return nil, fmt.Errorf("%w: Schedules", ErrMissingDependency)
	}
	if cfg.Tick <= 0 {
		cfg.Tick = DefaultScheduleTick
	}
	if cfg.Batch <= 0 {
		cfg.Batch = DefaultScheduleBatch
	}
	return &ScheduledPriceRunner{
		handler: handler, schedules: deps.Schedules, deps: deps,
		tick: cfg.Tick, batch: cfg.Batch,
	}, nil
}

// RunOnce activates everything due at the given instant, up to the batch limit,
// and reports how many reached the glass.
//
// A failure on one schedule does not abandon the rest: the 9am cliff is exactly
// when one unreachable store must not stop the other nine hundred.
func (r *ScheduledPriceRunner) RunOnce(ctx context.Context, at time.Time) (activated int, err error) {
	due, err := r.schedules.Due(ctx, at, r.batch)
	if err != nil {
		return 0, fmt.Errorf("label: reading due schedules: %w", err)
	}
	var firstErr error
	for _, entry := range due {
		if cerr := ctx.Err(); cerr != nil {
			return activated, cerr
		}
		res, aerr := r.handler.Activate(ctx, entry)
		if aerr != nil {
			if firstErr == nil {
				firstErr = aerr
			}
			r.deps.Metrics.ScheduledActivations.With(OutcomeError).Inc()
			r.deps.Log.FromContext(ctx).Error("activating scheduled price failed",
				"label_id", string(entry.LabelID), "schedule_id", entry.ScheduleID, "error", aerr)
			continue
		}
		r.deps.Metrics.ScheduledActivations.With(res.Outcome).Inc()
		if res.Applied() {
			activated++
		}
	}
	return activated, firstErr
}

// Run polls until ctx is cancelled. It returns ctx.Err() on cancellation, which
// is what the service's shutdown path expects.
func (r *ScheduledPriceRunner) Run(ctx context.Context) error {
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			n, err := r.RunOnce(ctx, r.deps.Clock.Now())
			if err != nil && !errors.Is(err, context.Canceled) {
				r.deps.Log.Error("scheduled price sweep completed with errors",
					"activated", n, "error", err)
			}
		}
	}
}

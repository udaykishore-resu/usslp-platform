// Package retry implements the platform's backoff policy.
//
// One policy, used everywhere, because retry behaviour is a system-level
// property rather than a per-call-site preference: uncoordinated retries are
// how a two-second blip in one service becomes a thundering herd that keeps it
// down for ten minutes. Full jitter is the default for exactly that reason —
// it decorrelates the retry schedules of the 50,000 clients that all failed at
// the same instant.
package retry

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"
)

// Policy describes a backoff schedule.
type Policy struct {
	// MaxAttempts includes the first attempt. Zero means unlimited, which is
	// only appropriate for a background reconnect loop.
	MaxAttempts int
	// Base is the delay before the second attempt.
	Base time.Duration
	// Max caps the delay.
	Max time.Duration
	// Multiplier is applied per attempt.
	Multiplier float64
	// Jitter applies full jitter: the actual delay is uniform in [0, computed].
	Jitter bool
}

// Default is the general-purpose policy: five attempts over roughly a second.
var Default = Policy{MaxAttempts: 5, Base: 50 * time.Millisecond, Max: 2 * time.Second, Multiplier: 2, Jitter: true}

// Aggressive is for the price hot path, where the three-second SLO leaves no
// room for a slow schedule.
var Aggressive = Policy{MaxAttempts: 3, Base: 20 * time.Millisecond, Max: 200 * time.Millisecond, Multiplier: 2, Jitter: true}

// Persistent is for background reconnection (SGU to cloud, client to broker):
// never give up, but back off to a 30-second poll so an outage does not
// generate load.
var Persistent = Policy{MaxAttempts: 0, Base: 250 * time.Millisecond, Max: 30 * time.Second, Multiplier: 1.6, Jitter: true}

// Delay returns the wait before attempt n (1-based; Delay(1) is 0).
func (p Policy) Delay(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	base := p.Base
	if base <= 0 {
		base = 50 * time.Millisecond
	}
	mult := p.Multiplier
	if mult < 1 {
		mult = 2
	}
	d := float64(base) * math.Pow(mult, float64(attempt-2))
	maxD := p.Max
	if maxD <= 0 {
		maxD = 30 * time.Second
	}
	if d > float64(maxD) {
		d = float64(maxD)
	}
	if p.Jitter {
		d = rand.Float64() * d
	}
	return time.Duration(d)
}

// Permanent wraps an error to stop retrying. A 400 from a POS API will fail
// identically forever; retrying it only delays the dead-letter that a human
// needs to see.
type Permanent struct{ Err error }

func (p *Permanent) Error() string { return "permanent: " + p.Err.Error() }
func (p *Permanent) Unwrap() error { return p.Err }

// Stop marks an error as not worth retrying.
func Stop(err error) error {
	if err == nil {
		return nil
	}
	return &Permanent{Err: err}
}

// IsPermanent reports whether an error is marked permanent.
func IsPermanent(err error) bool {
	var p *Permanent
	return errors.As(err, &p)
}

// Do runs fn until it succeeds, the policy is exhausted, ctx is cancelled, or
// the error is permanent. The returned error is the last one seen, unwrapped
// from any Permanent marker so callers match on the underlying cause.
func Do(ctx context.Context, p Policy, fn func(ctx context.Context, attempt int) error) error {
	var last error
	for attempt := 1; p.MaxAttempts == 0 || attempt <= p.MaxAttempts; attempt++ {
		if d := p.Delay(attempt); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				if last != nil {
					return last
				}
				return ctx.Err()
			case <-t.C:
			}
		}
		if err := ctx.Err(); err != nil {
			if last != nil {
				return last
			}
			return err
		}
		err := fn(ctx, attempt)
		if err == nil {
			return nil
		}
		var perm *Permanent
		if errors.As(err, &perm) {
			return perm.Err
		}
		last = err
	}
	return last
}

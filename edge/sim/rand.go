package sim

import (
	"math"
	"math/rand/v2"
	"sync"
)

// Rand is a seeded, concurrency-safe random source.
//
// Every stochastic decision in the edge simulators draws from one of these so
// that a scenario is reproducible from a seed: a mesh test that fails on a
// particular packet-loss draw must fail the same way on the next run, or it is
// not a test, it is a coin toss. The mutex is not free, but the alternative —
// a per-goroutine source — sacrifices exactly the reproducibility the type
// exists to provide.
//
// PCG is used rather than the older LCG because math/rand/v2's PCG is
// explicitly seedable and its stream is stable across Go releases, which
// matters for the golden values a few of these tests assert on.
type Rand struct {
	mu sync.Mutex
	r  *rand.Rand
}

// NewRand returns a source seeded deterministically from seed.
func NewRand(seed uint64) *Rand {
	// The second PCG seed word is derived rather than fixed so that two engines
	// seeded 1 and 2 do not produce correlated streams.
	return &Rand{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

// Float64 returns a uniform value in [0, 1).
func (r *Rand) Float64() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.Float64()
}

// IntN returns a uniform integer in [0, n). It returns 0 for n <= 0 rather than
// panicking, because a simulator asked to choose among zero candidates has a
// configuration problem, not a crashing one.
func (r *Rand) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.IntN(n)
}

// NormFloat64 returns a standard normal deviate. Radio shadow fading and RSSI
// measurement noise are both modelled as Gaussian, which is what the
// log-distance path-loss literature uses.
func (r *Rand) NormFloat64() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.NormFloat64()
}

// Normal returns a normal deviate with the given mean and standard deviation.
func (r *Rand) Normal(mean, stddev float64) float64 {
	return mean + stddev*r.NormFloat64()
}

// Duration returns a uniform duration in [0, d).
func (r *Rand) Duration(d int64) int64 {
	if d <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.Int64N(d)
}

// Jitter returns d scaled by a uniform factor in [1-frac, 1+frac], clamped at
// zero. Per-hop mesh latency and E-Ink waveform timing both vary a few percent
// run to run, and modelling them as exact constants would make the latency
// percentiles this platform publishes suspiciously clean.
func (r *Rand) Jitter(d int64, frac float64) int64 {
	if d <= 0 {
		return 0
	}
	if frac <= 0 {
		return d
	}
	f := 1 + frac*(2*r.Float64()-1)
	v := int64(math.Round(float64(d) * f))
	if v < 0 {
		return 0
	}
	return v
}

// Bernoulli reports whether an event with probability p occurred.
func (r *Rand) Bernoulli(p float64) bool {
	switch {
	case p <= 0:
		return false
	case p >= 1:
		return true
	}
	return r.Float64() < p
}

// Perm returns a deterministic permutation of [0, n). The mesh uses it to
// randomise join order so that network formation does not depend on the order
// nodes happen to be listed in a planogram file.
func (r *Rand) Perm(n int) []int {
	if n <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.Perm(n)
}

package eventlog

import (
	"time"

	"github.com/usslp/usslp/platform/pkg/obs"
)

// Default tuning. These are the values `make dev` runs with.
const (
	// DefaultSegmentBytes is the size at which a segment rolls. 64 MiB is small
	// enough that retention deletes storage in useful increments and large
	// enough that a partition sustaining the price hot path rolls a segment
	// every few minutes rather than every few seconds.
	DefaultSegmentBytes int64 = 64 << 20

	// DefaultIndexInterval is how many bytes of segment are covered by one
	// sparse index entry. At 4 KiB an index costs ~0.4% of segment size and
	// bounds the scan needed to reach an arbitrary offset to one page.
	DefaultIndexInterval int64 = 4 << 10

	// DefaultRetentionInterval is how often retention and compaction run. Both
	// walk every closed segment, so this is deliberately slow relative to the
	// write path.
	DefaultRetentionInterval = time.Minute

	// DefaultReadBatch is the maximum number of records one poll hands to a
	// consumer before offsets are advanced.
	DefaultReadBatch = 256
)

// syncMode enumerates the fsync policies.
type syncMode int

const (
	syncModeAlways syncMode = iota
	syncModeInterval
	syncModeNever
)

// SyncPolicy decides when a written batch is forced to stable storage.
//
// The three policies trade a different amount of durability for throughput:
//
//   - SyncAlways fsyncs the segment before Publish returns. Survives a kernel
//     panic or power loss with zero acknowledged records lost. This is the
//     default because USSLP acknowledges a price change to a retailer's POS
//     before the label is updated: having told the POS "accepted" and then
//     losing the record is a pricing-compliance incident, not a hiccup.
//   - SyncInterval(d) fsyncs at most every d. Survives process death intact
//     (the data is already in the page cache) but a machine that loses power
//     can lose up to d of acknowledged writes. Appropriate for telemetry-shaped
//     traffic where the value of an individual record is below the cost of a
//     flush.
//   - SyncNever never fsyncs except at Close. Survives process death, loses
//     everything not yet written back on power loss. For tests only.
type SyncPolicy struct {
	mode     syncMode
	interval time.Duration
}

// SyncAlways fsyncs every batch before Publish returns.
var SyncAlways = SyncPolicy{mode: syncModeAlways}

// SyncNever leaves write-back entirely to the kernel until Close.
var SyncNever = SyncPolicy{mode: syncModeNever}

// SyncInterval fsyncs no more often than every d, bounding the window of
// acknowledged-but-unflushed records. A non-positive d degrades to SyncAlways
// rather than silently becoming SyncNever, because the failure mode of
// misconfiguring durability should be "slow", not "lossy".
func SyncInterval(d time.Duration) SyncPolicy {
	if d <= 0 {
		return SyncAlways
	}
	return SyncPolicy{mode: syncModeInterval, interval: d}
}

// String renders the policy for logs and tests.
func (p SyncPolicy) String() string {
	switch p.mode {
	case syncModeInterval:
		return "interval(" + p.interval.String() + ")"
	case syncModeNever:
		return "never"
	default:
		return "always"
	}
}

type options struct {
	segmentBytes      int64
	indexInterval     int64
	retentionInterval time.Duration
	readBatch         int
	sync              SyncPolicy
	registry          *obs.Registry
	logger            *obs.Logger
}

func defaultOptions() options {
	return options{
		segmentBytes:      DefaultSegmentBytes,
		indexInterval:     DefaultIndexInterval,
		retentionInterval: DefaultRetentionInterval,
		readBatch:         DefaultReadBatch,
		sync:              SyncAlways,
	}
}

// Option configures a Log at Open time. Every knob that changes on-disk layout
// (segment size, index density) is an Open-time decision rather than a runtime
// one, so that a directory's layout cannot change under a live reader.
type Option func(*options)

// WithSegmentBytes sets the size at which segments roll. Retention and
// compaction operate on whole segments, so this is also the granularity at
// which space is reclaimed: a smaller value reclaims sooner at the cost of more
// file handles and more index files. Values below 1 KiB are raised to 1 KiB.
func WithSegmentBytes(n int64) Option {
	return func(o *options) {
		if n < 1<<10 {
			n = 1 << 10
		}
		o.segmentBytes = n
	}
}

// WithIndexInterval sets how many bytes of segment one sparse index entry
// covers. Lower means faster seeks to an arbitrary offset and a larger index.
func WithIndexInterval(n int64) Option {
	return func(o *options) {
		if n < 64 {
			n = 64
		}
		o.indexInterval = n
	}
}

// WithSync selects the fsync policy. See SyncPolicy for what each one costs.
func WithSync(p SyncPolicy) Option {
	return func(o *options) { o.sync = p }
}

// WithRetentionInterval sets how often retention and compaction sweep. Tests
// that want deterministic behaviour should leave this long and drive the sweep
// directly rather than racing a ticker.
func WithRetentionInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.retentionInterval = d
		}
	}
}

// WithReadBatch caps how many records one consumer poll returns. Larger batches
// amortise the offset commit; smaller ones bound the redelivery window after a
// crash.
func WithReadBatch(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.readBatch = n
		}
	}
}

// WithMetrics attaches an obs.Registry, into which the log registers append,
// lag, handler-latency and dead-letter series.
//
// A registry panics on duplicate registration, so one registry backs at most
// one Log; sharing one between two Opens is a programmer error and will panic
// at the second Open.
func WithMetrics(r *obs.Registry) Option {
	return func(o *options) { o.registry = r }
}

// WithLogger replaces the default logger. The default writes warnings and above
// to stderr, because the things this package logs — a truncated segment, a
// consumer that fell off the log, a dead-letter that could not be written — are
// exactly the events an operator must not miss.
func WithLogger(l *obs.Logger) Option {
	return func(o *options) { o.logger = l }
}

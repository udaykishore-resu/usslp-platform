// Package idem is the idempotency guard used at the Universal Integration
// Gateway's ingress.
//
// Every POS integration USSLP speaks to retries. SAP resends an IDoc when the
// ALE acknowledgement is slow; Shopify redelivers a webhook for eight hours
// after a single non-2xx; an NCR till replays its outbox after a network blip;
// a nightly CSV drop gets re-uploaded because someone was not sure the first
// upload worked. None of those are faults — they are the correct behaviour of
// an at-least-once producer. What would be a fault is the platform turning one
// retailer price decision into two price changes on a shelf.
//
// The guard therefore deduplicates deliveries within a window (24 hours by
// default, which covers every retry schedule the platform's adapters face) on a
// key the adapter derives from the payload. Two properties matter:
//
//   - Exactly one caller is ever told "first seen" for a given key, even when
//     two redeliveries of the same message arrive at the same instant on two
//     goroutines. That is a compare-and-set, not a read followed by a write.
//   - A duplicate delivery can be answered with the *original* result, so the
//     retrying system gets the same response it would have got the first time
//     and stops retrying.
//
// The Backend interface is deliberately narrow and Redis-shaped, because a
// single-store gateway wants this backed by the embedded kvstore while the
// multi-replica cloud ingress must share one guard across replicas and will
// back it with Redis. See Backend for the exact command mapping.
package idem

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// DefaultWindow is how long a key is remembered. Twenty-four hours is chosen to
// outlast every retry schedule the platform's adapters use — Shopify's webhook
// redelivery runs for eight hours, SAP ALE resend queues are typically drained
// within one business day — so a redelivery always lands inside the window.
const DefaultWindow = 24 * time.Hour

// keyDomain namespaces the key hash. Including it means a digest produced here
// can never collide with a digest of the same parts computed for some other
// purpose, and the "/v1" lets the derivation be changed later without silently
// reinterpreting keys already in flight.
const keyDomain = "usslp/idem/v1"

// Key derives a stable idempotency key from the parts a POS adapter can supply
// — typically the source system, the tenant, the external message id, and, when
// the source gives no id at all, a digest of the payload itself.
//
// The derivation is: SHA-256 over the domain string "usslp/idem/v1", followed
// by each part preceded by its length as an unsigned varint, rendered as 64
// lowercase hex characters. Length prefixing rather than a separator is what
// makes it unambiguous: Key("ab", "c") and Key("a", "bc") must not collide,
// and with a separator-joined scheme they would as soon as a part contained the
// separator — and SKUs, store codes and vendor message ids contain everything.
//
// The result is deterministic across processes, releases and machines, which is
// what lets two ingress replicas independently derive the same key for the same
// redelivered message.
func Key(parts ...string) string {
	h := sha256.New()
	h.Write([]byte(keyDomain))
	var buf [binary.MaxVarintLen64]byte
	for _, p := range parts {
		n := binary.PutUvarint(buf[:], uint64(len(p)))
		h.Write(buf[:n])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// State is the lifecycle of a guarded key.
type State string

const (
	// StatePending marks a key claimed by a delivery that has not yet finished
	// processing. A second delivery seeing it knows the work is in flight and
	// that it must not start it again.
	StatePending State = "pending"
	// StateDone marks a key whose result has been recorded, so a duplicate can
	// be answered immediately with the original response.
	StateDone State = "done"
)

// Entry is what the guard stores against a key. It is JSON encoded so that a
// Redis-backed guard and a kvstore-backed guard hold byte-identical values, and
// so an operator debugging a stuck ingress can read it with redis-cli.
type Entry struct {
	// State is pending or done.
	State State `json:"state"`
	// Result is the response the first delivery produced, replayed to
	// duplicates. It is empty while the entry is pending.
	Result []byte `json:"result,omitempty"`
	// RecordedAt is when the entry reached its current state.
	RecordedAt time.Time `json:"recorded_at"`
}

// ErrNotFound is returned by a Backend for a key it does not hold.
var ErrNotFound = errors.New("idem: key not found")

// Backend is the storage primitive the guard needs. It is intentionally the
// smallest set of operations that a single-node embedded store and a shared
// Redis can both provide atomically.
//
// Redis mapping, for the multi-replica cloud ingress where the guard must be
// shared across replicas rather than per-process:
//
//	Reserve -> SET <ns>:<key> <json> NX PX <ttl-ms>
//	           A reply of OK means claimed; a nil reply means someone else holds
//	           it, and the caller follows with GET to read their entry.
//	Store   -> SET <ns>:<key> <json> PX <ttl-ms>
//	Load    -> GET <ns>:<key>
//	Forget  -> DEL <ns>:<key>
//
// The TTL is the deduplication window, so Redis expiry does the housekeeping
// for free. SET NX is a single round trip and is atomic across replicas, which
// is the property that makes "exactly one first seen" hold when two ingress
// pods receive the same redelivery simultaneously.
//
// An implementation must be safe for concurrent use.
type Backend interface {
	// Reserve atomically claims key with entry when no live entry exists,
	// reporting whether it claimed it. It must not overwrite an existing entry.
	Reserve(ctx context.Context, key string, entry Entry, ttl time.Duration) (claimed bool, err error)
	// Store writes entry unconditionally, replacing a reservation with a
	// result.
	Store(ctx context.Context, key string, entry Entry, ttl time.Duration) error
	// Load returns the entry held for key, or ErrNotFound.
	Load(ctx context.Context, key string) (Entry, error)
	// Forget removes the entry so the key can be claimed again.
	Forget(ctx context.Context, key string) error
}

// ---------------------------------------------------------------------------
// kvstore backend
// ---------------------------------------------------------------------------

// KVBackend implements Backend over the embedded kvstore. It is what a Store
// Gateway Unit uses: the gateway is a single process, so a local guard is both
// sufficient and available during a WAN outage, when a shared Redis would not
// be reachable and the guard would fail exactly when the store needs it most.
type KVBackend struct {
	kv     *kvstore.Store
	prefix string
}

// DefaultPrefix namespaces guard keys inside a shared kvstore.
const DefaultPrefix = "idem/"

// NewKVBackend wraps a kvstore. An empty prefix uses DefaultPrefix; supplying
// one lets several guards (say, one per source system) share a store without
// colliding.
func NewKVBackend(kv *kvstore.Store, prefix string) (*KVBackend, error) {
	if kv == nil {
		return nil, errors.New("idem: nil kvstore")
	}
	if prefix == "" {
		prefix = DefaultPrefix
	}
	return &KVBackend{kv: kv, prefix: prefix}, nil
}

func (b *KVBackend) key(k string) []byte { return []byte(b.prefix + k) }

// Reserve claims the key using the store's atomic put-if-absent, which is the
// compare-and-set that makes "exactly one first seen" true under concurrency.
func (b *KVBackend) Reserve(ctx context.Context, key string, entry Entry, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return false, fmt.Errorf("idem: encode entry: %w", err)
	}
	return b.kv.PutIfAbsent(b.key(key), body, ttl)
}

// Store replaces whatever is held for key.
func (b *KVBackend) Store(ctx context.Context, key string, entry Entry, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("idem: encode entry: %w", err)
	}
	return b.kv.PutTTL(b.key(key), body, ttl)
}

// Load reads the entry held for key.
func (b *KVBackend) Load(ctx context.Context, key string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	raw, err := b.kv.Get(b.key(key))
	if errors.Is(err, kvstore.ErrNotFound) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return Entry{}, fmt.Errorf("idem: decode entry for %s: %w", key, err)
	}
	return e, nil
}

// Forget removes the entry.
func (b *KVBackend) Forget(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.kv.Delete(b.key(key))
}

// ---------------------------------------------------------------------------
// Guard
// ---------------------------------------------------------------------------

// Guard deduplicates deliveries within a window.
//
// The usage at ingress is:
//
//	first, previous, err := guard.Check(ctx, key)
//	if err != nil { ... }
//	if !first {
//	    // A duplicate. previous is the original response if the first
//	    // delivery finished, or nil if it is still in flight.
//	    return replay(previous)
//	}
//	result, err := process(msg)
//	if err != nil {
//	    guard.Release(ctx, key) // let the producer's retry try again
//	    return err
//	}
//	guard.Record(ctx, key, result, 0)
//
// A Guard is safe for concurrent use by any number of goroutines.
type Guard struct {
	backend Backend
	window  time.Duration
	now     func() time.Time
}

// Option configures a Guard.
type Option func(*Guard)

// WithWindow sets the deduplication window. It must be long enough to outlast
// the retry schedule of every producer the guard sits in front of; shorter and
// a late redelivery is processed a second time.
func WithWindow(d time.Duration) Option {
	return func(g *Guard) {
		if d > 0 {
			g.window = d
		}
	}
}

// WithClock injects a clock, so tests can reason about the window without
// sleeping through it.
func WithClock(now func() time.Time) Option {
	return func(g *Guard) {
		if now != nil {
			g.now = now
		}
	}
}

// New builds a guard over a backend.
func New(b Backend, opts ...Option) (*Guard, error) {
	if b == nil {
		return nil, errors.New("idem: nil backend")
	}
	g := &Guard{backend: b, window: DefaultWindow, now: time.Now}
	for _, o := range opts {
		o(g)
	}
	return g, nil
}

// Window returns the deduplication window.
func (g *Guard) Window() time.Duration { return g.window }

// Check claims a key for processing.
//
// It returns firstSeen true to exactly one caller per key per window; that
// caller owns the work. Every other caller gets firstSeen false along with the
// previous result if the original delivery has finished, or a nil result if it
// is still in flight — a distinction the caller needs, because replaying a
// recorded response and telling a producer "still processing, retry later" are
// different answers.
func (g *Guard) Check(ctx context.Context, key string) (firstSeen bool, previous []byte, err error) {
	if key == "" {
		return false, nil, errors.New("idem: empty key")
	}
	entry := Entry{State: StatePending, RecordedAt: g.now().UTC()}
	// The reserve-then-load pair is not one atomic step in any backend that can
	// also be Redis, so the rare interleaving where the winner's entry expires
	// between our failed reserve and our load is retried rather than reported
	// as a phantom in-flight delivery. A handful of attempts is ample: each one
	// requires another delivery to claim and lose the key inside the window.
	for attempt := 0; attempt < 3; attempt++ {
		claimed, err := g.backend.Reserve(ctx, key, entry, g.window)
		if err != nil {
			return false, nil, err
		}
		if claimed {
			return true, nil, nil
		}
		prev, err := g.backend.Load(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, nil, err
		}
		if prev.State == StateDone {
			return false, prev.Result, nil
		}
		return false, nil, nil
	}
	return false, nil, fmt.Errorf("idem: key %s kept vanishing between reserve and load", key)
}

// Record stores the result of processing so that later duplicates are answered
// with it. A ttl of zero uses the guard's window; passing an explicit ttl is
// how a caller keeps a particularly expensive result — a completed bulk
// catalogue import — for longer than the default.
func (g *Guard) Record(ctx context.Context, key string, result []byte, ttl time.Duration) error {
	if key == "" {
		return errors.New("idem: empty key")
	}
	if ttl <= 0 {
		ttl = g.window
	}
	return g.backend.Store(ctx, key, Entry{
		State:      StateDone,
		Result:     append([]byte(nil), result...),
		RecordedAt: g.now().UTC(),
	}, ttl)
}

// Release drops a reservation so the producer's next retry is treated as a
// first delivery.
//
// This is the counterpart to Check that makes the guard safe to put in front of
// work that can fail. Without it, a delivery that claimed a key and then
// crashed would suppress every retry for the whole window, and the price change
// would simply never be applied — a silent loss, which is the worst possible
// failure mode for a pricing system.
func (g *Guard) Release(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("idem: empty key")
	}
	return g.backend.Forget(ctx, key)
}

// Lookup reports the current entry for a key without claiming it. It is for
// diagnostics and for an operator asking "did we already process this IDoc".
func (g *Guard) Lookup(ctx context.Context, key string) (Entry, error) {
	if key == "" {
		return Entry{}, errors.New("idem: empty key")
	}
	return g.backend.Load(ctx, key)
}

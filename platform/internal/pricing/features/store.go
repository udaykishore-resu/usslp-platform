// Package features is the point-in-time-correct feature store.
//
// # The failure this package exists to prevent
//
// Retail pricing models fail in one characteristic way: leakage. A training row
// for last Tuesday is assembled today, and the waste rate it carries is the
// waste rate the platform knows *today* — which incorporates the write-offs
// that happened on Tuesday evening, after the pricing decision the row is
// supposed to be teaching the model to make. The model learns that high waste
// predicts a markdown, achieves an excellent holdout score, and is useless in
// production, where the evening's waste is not yet known at the moment the
// morning's price is set.
//
// The fix is bitemporal storage. Every value carries two timestamps: when the
// fact became true (ValidFrom) and when USSLP first knew it (KnownAt). A
// training row assembled "as of" an instant may only see values whose KnownAt
// is at or before that instant, no matter how long ago they became true. That
// rule is enforced here, in the read path, rather than left to each caller —
// because a caller who forgets it gets a better-looking model, which is exactly
// the wrong incentive.
package features

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// ErrNotFound is returned when no value is known for a feature as of an
// instant. It is distinct from a zero value: "we did not know the inventory
// level yet" and "the inventory level was zero" must not be confused, because
// the first should exclude the training row and the second is a strong signal.
var ErrNotFound = errors.New("features: no value known as of that instant")

// ErrInvalid marks a malformed write.
var ErrInvalid = errors.New("features: invalid record")

// Key identifies one feature series.
type Key struct {
	Tenant canon.TenantID
	Store  canon.StoreID
	SKU    canon.SKU
	// Name is one of domain.FeatureNames, or any tenant-defined series name.
	Name string
}

// Validate rejects keys that would corrupt the key space. The separator is the
// same reserved-character set canon enforces on identifiers, so a store ID that
// passed ingress validation cannot break out of its own prefix here.
func (k Key) Validate() error {
	switch {
	case k.Tenant == "" || !canon.ValidID(string(k.Tenant)):
		return fmt.Errorf("%w: tenant %q", ErrInvalid, k.Tenant)
	case k.Store == "" || !canon.ValidID(string(k.Store)):
		return fmt.Errorf("%w: store %q", ErrInvalid, k.Store)
	case k.SKU == "" || !canon.ValidID(string(k.SKU)):
		return fmt.Errorf("%w: sku %q", ErrInvalid, k.SKU)
	case k.Name == "" || strings.ContainsAny(k.Name, "\x00\xff"):
		return fmt.Errorf("%w: feature name %q", ErrInvalid, k.Name)
	}
	return nil
}

// Value is one observation of a feature.
type Value struct {
	// Number is the value.
	Number float64 `json:"value"`
	// ValidFrom is when the fact became true in the world.
	ValidFrom time.Time `json:"valid_from"`
	// KnownAt is when USSLP first learned it. It is never earlier than
	// ValidFrom in practice, and the store does not require that it is: a
	// backdated correction has a KnownAt well after its ValidFrom, and that is
	// precisely the case point-in-time reads must get right.
	KnownAt time.Time `json:"known_at"`
	// Source names the producer, for lineage.
	Source string `json:"source,omitempty"`
}

// Store is the point-in-time feature store over a kvstore.
//
// # Key layout
//
//	f\x00{tenant}\x00{store}\x00{sku}\x00{name}\x00{^knownAt}{^validFrom}
//
// The two timestamps are stored as big-endian *complemented* nanoseconds, so
// keys sort by descending knowledge time and then by descending validity time.
// A point-in-time read therefore seeks to the complement of the as-of instant
// and takes the first key it finds: exactly one forward iterator step, no
// backward scan, and no reading of records the caller is not allowed to see.
// Sorting ascending instead would force either a full-series scan or a reverse
// iterator the kvstore does not offer.
type Store struct {
	kv *kvstore.Store
	// retention bounds how long history is kept. Zero means forever.
	retention time.Duration
}

// Config configures the store.
type Config struct {
	// KV is the backing store.
	KV *kvstore.Store
	// Retention is the TTL applied to written observations. A pricing model is
	// trained on at most two years of history, and unbounded feature history on
	// a gateway's flash is a slow-motion disk-full outage.
	Retention time.Duration
}

// New builds a feature store.
func New(cfg Config) (*Store, error) {
	if cfg.KV == nil {
		return nil, fmt.Errorf("%w: nil kv store", ErrInvalid)
	}
	return &Store{kv: cfg.KV, retention: cfg.Retention}, nil
}

const keyPrefix = "f\x00"

// seriesPrefix builds the prefix shared by every observation of one series.
func seriesPrefix(k Key) []byte {
	b := make([]byte, 0, 64)
	b = append(b, keyPrefix...)
	b = append(b, k.Tenant...)
	b = append(b, 0)
	b = append(b, k.Store...)
	b = append(b, 0)
	b = append(b, k.SKU...)
	b = append(b, 0)
	b = append(b, k.Name...)
	b = append(b, 0)
	return b
}

// complement encodes a time as the big-endian complement of its nanosecond
// value, so that later instants sort first.
func complement(t time.Time) []byte {
	var b [8]byte
	// A zero time's UnixNano is meaningless; pinning it to the earliest
	// representable instant keeps the ordering total rather than wrapping.
	n := t.UnixNano()
	if t.IsZero() {
		n = math.MinInt64
	}
	// Flipping the sign bit maps the signed range onto the unsigned range in
	// order (so pre-1970 instants, which backfills genuinely produce, sort
	// before later ones); complementing then reverses it, so later instants
	// sort first.
	binary.BigEndian.PutUint64(b[:], ^(uint64(n) ^ (1 << 63)))
	return b[:]
}

func observationKey(k Key, v Value) []byte {
	b := seriesPrefix(k)
	b = append(b, complement(v.KnownAt)...)
	b = append(b, complement(v.ValidFrom)...)
	return b
}

// encodeValue serialises an observation. The layout is fixed-width so a read is
// one allocation and no parsing.
func encodeValue(v Value) []byte {
	b := make([]byte, 0, 24+len(v.Source))
	b = binary.BigEndian.AppendUint64(b, math.Float64bits(v.Number))
	b = binary.BigEndian.AppendUint64(b, uint64(v.ValidFrom.UnixNano()))
	b = binary.BigEndian.AppendUint64(b, uint64(v.KnownAt.UnixNano()))
	b = append(b, v.Source...)
	return b
}

func decodeValue(b []byte) (Value, error) {
	if len(b) < 24 {
		return Value{}, fmt.Errorf("%w: %d-byte value record", ErrInvalid, len(b))
	}
	return Value{
		Number:    math.Float64frombits(binary.BigEndian.Uint64(b[0:8])),
		ValidFrom: time.Unix(0, int64(binary.BigEndian.Uint64(b[8:16]))).UTC(),
		KnownAt:   time.Unix(0, int64(binary.BigEndian.Uint64(b[16:24]))).UTC(),
		Source:    string(b[24:]),
	}, nil
}

// Put records one observation.
//
// A missing KnownAt defaults to ValidFrom, which is the honest default for a
// live feed where the platform learns a fact as it happens. It is *not* defaulted
// to "now", because that would silently make every backfilled row appear to have
// been known at backfill time — correct for leakage purposes, but it would make
// a legitimate historical backfill unusable for training, and the caller is the
// only party that knows which of the two it is doing.
func (s *Store) Put(k Key, v Value) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if v.ValidFrom.IsZero() {
		return fmt.Errorf("%w: observation has no valid_from", ErrInvalid)
	}
	if v.KnownAt.IsZero() {
		v.KnownAt = v.ValidFrom
	}
	if math.IsNaN(v.Number) || math.IsInf(v.Number, 0) {
		return fmt.Errorf("%w: %s is not finite", ErrInvalid, k.Name)
	}
	key := observationKey(k, v)
	if s.retention > 0 {
		return s.kv.PutTTL(key, encodeValue(v), s.retention)
	}
	return s.kv.Put(key, encodeValue(v))
}

// PutBatch records many observations atomically.
//
// Ingest arrives as a batch per (store, period) from the inventory-sync and
// price-updates streams; committing the batch in one write means a consumer
// that crashes mid-batch replays cleanly rather than leaving a half-populated
// row that trains a model on a partly-observed day.
func (s *Store) PutBatch(records []Record) error {
	b := s.kv.NewBatch()
	for _, rec := range records {
		if err := rec.Key.Validate(); err != nil {
			return err
		}
		v := rec.Value
		if v.ValidFrom.IsZero() {
			return fmt.Errorf("%w: observation for %s has no valid_from", ErrInvalid, rec.Key.Name)
		}
		if v.KnownAt.IsZero() {
			v.KnownAt = v.ValidFrom
		}
		if math.IsNaN(v.Number) || math.IsInf(v.Number, 0) {
			return fmt.Errorf("%w: %s is not finite", ErrInvalid, rec.Key.Name)
		}
		if s.retention > 0 {
			b.PutTTL(observationKey(rec.Key, v), encodeValue(v), s.retention)
		} else {
			b.Put(observationKey(rec.Key, v), encodeValue(v))
		}
	}
	return b.Write()
}

// Record pairs a key with a value for batch writes.
type Record struct {
	Key   Key
	Value Value
}

// AsOf returns the value of a feature as it was known at an instant.
//
// This is the function the whole package exists for. It returns the most
// recently *known* observation whose KnownAt is at or before asOf; among
// observations sharing a KnownAt it returns the one with the latest ValidFrom.
// An observation recorded later — even one describing an earlier moment — is
// invisible, which is what makes a training row assembled with AsOf reproduce
// the information state of the decision it is modelling.
func (s *Store) AsOf(k Key, asOf time.Time) (Value, error) {
	if err := k.Validate(); err != nil {
		return Value{}, err
	}
	prefix := seriesPrefix(k)
	// Seek to the first key at or after the complement of asOf. Because keys
	// descend in knowledge time, that is the newest observation known at or
	// before asOf.
	start := append(append([]byte{}, prefix...), complement(asOf)...)
	end := prefixEnd(prefix)
	it := s.kv.Range(start, end)
	defer it.Close()
	if !it.Next() {
		if err := it.Err(); err != nil {
			return Value{}, err
		}
		return Value{}, fmt.Errorf("%w: %s/%s as of %s", ErrNotFound, k.SKU, k.Name, asOf.UTC().Format(time.RFC3339))
	}
	v, err := decodeValue(it.Value())
	if err != nil {
		return Value{}, err
	}
	// Defensive: the key ordering should make this impossible, but a value
	// whose encoded KnownAt disagrees with its key would silently leak, and a
	// leak that only shows up as a suspiciously good model is not worth the
	// saved comparison.
	if v.KnownAt.After(asOf) {
		return Value{}, fmt.Errorf("%w: %s/%s as of %s", ErrNotFound, k.SKU, k.Name, asOf.UTC().Format(time.RFC3339))
	}
	return v, nil
}

// Latest returns the most recently known value, with no point-in-time cut.
// It is the serving-path read: at inference the current instant *is* the as-of
// instant, so there is nothing to hide.
func (s *Store) Latest(k Key) (Value, error) { return s.AsOf(k, time.Now().UTC()) }

// History returns every observation of a series known at or before asOf, newest
// knowledge first, capped at limit.
//
// It is what the elasticity estimator and the API's audit view read. Returning
// the observations rather than only the latest is what lets an operator answer
// "what did the platform believe when it made that decision", which is the
// first question after any surprising price.
func (s *Store) History(k Key, asOf time.Time, limit int) ([]Value, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 1000
	}
	prefix := seriesPrefix(k)
	start := append(append([]byte{}, prefix...), complement(asOf)...)
	it := s.kv.Range(start, prefixEnd(prefix))
	defer it.Close()
	out := make([]Value, 0, 16)
	for it.Next() && len(out) < limit {
		v, err := decodeValue(it.Value())
		if err != nil {
			return nil, err
		}
		if v.KnownAt.After(asOf) {
			continue
		}
		out = append(out, v)
	}
	return out, it.Err()
}

// Vector assembles a full feature row as of an instant.
//
// Missing features are reported rather than defaulted. A caller building a
// training set drops the row; a caller serving an inference decides whether the
// missing feature is one it can proceed without. Substituting zero here would
// teach the model that "no competitor tracked" means "competitor charges
// nothing", which is the second most common way a retail pricing model goes
// wrong after leakage.
func (s *Store) Vector(tenant canon.TenantID, store canon.StoreID, sku canon.SKU, names []string, asOf time.Time) (values []float64, missing []string, err error) {
	values = make([]float64, len(names))
	for i, name := range names {
		v, err := s.AsOf(Key{Tenant: tenant, Store: store, SKU: sku, Name: name}, asOf)
		if errors.Is(err, ErrNotFound) {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		values[i] = v.Number
	}
	return values, missing, nil
}

// prefixEnd returns the exclusive upper bound of a prefix scan.
func prefixEnd(prefix []byte) []byte {
	end := make([]byte, len(prefix))
	copy(end, prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	// An all-0xff prefix has no successor; a nil end means "to the end of the
	// key space", which is correct for that degenerate case.
	return nil
}

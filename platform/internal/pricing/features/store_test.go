package features

import (
	"errors"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	kv, err := kvstore.OpenWith(kvstore.Options{Dir: t.TempDir(), Sync: kvstore.SyncNever})
	if err != nil {
		t.Fatalf("open kv: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	fs, err := New(Config{KV: kv})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return fs
}

func at(day int, hour int) time.Time {
	return time.Date(2026, 3, day, hour, 0, 0, 0, time.UTC)
}

func testKey(name string) Key {
	return Key{Tenant: "acme", Store: "store-001", SKU: "sku-42", Name: name}
}

// TestPointInTimeReadsSeeOnlyWhatWasKnown is the leakage test. It fails if the
// store ever returns a value the platform did not yet know at the as-of
// instant, which is the single defect that makes a retail pricing model look
// excellent in evaluation and useless in production.
func TestPointInTimeReadsSeeOnlyWhatWasKnown(t *testing.T) {
	fs := newTestStore(t)
	k := testKey("waste_rate")

	// Three observations of the same daily fact, learned at different times.
	// The Tuesday waste rate is only known on Wednesday morning, after the
	// evening's write-offs are counted — which is exactly the value a model
	// trained "as of Tuesday morning" must not be able to see.
	obs := []Value{
		{Number: 0.01, ValidFrom: at(2, 0), KnownAt: at(3, 6), Source: "nightly-waste"},
		{Number: 0.02, ValidFrom: at(3, 0), KnownAt: at(4, 6), Source: "nightly-waste"},
		{Number: 0.03, ValidFrom: at(4, 0), KnownAt: at(5, 6), Source: "nightly-waste"},
	}
	for _, v := range obs {
		if err := fs.Put(k, v); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	cases := []struct {
		name    string
		asOf    time.Time
		want    float64
		wantErr bool
	}{
		{
			name:    "before anything was known",
			asOf:    at(2, 9),
			wantErr: true,
		},
		{
			// The Tuesday fact is true on Tuesday, but the platform does not
			// learn it until Wednesday at 06:00. A decision made on Wednesday
			// at 05:00 cannot have used it.
			name:    "an hour before the first value was learned",
			asOf:    at(3, 5),
			wantErr: true,
		},
		{
			name: "just after the first value was learned",
			asOf: at(3, 7),
			want: 0.01,
		},
		{
			name: "the second value is invisible until it is learned",
			asOf: at(4, 5),
			want: 0.01,
		},
		{
			name: "and visible immediately after",
			asOf: at(4, 7),
			want: 0.02,
		},
		{
			name: "the latest known value at the end",
			asOf: at(9, 0),
			want: 0.03,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fs.AsOf(k, tt.asOf)
			if tt.wantErr {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("err = %v (value %+v), want ErrNotFound", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("as-of: %v", err)
			}
			if got.Number != tt.want {
				t.Errorf("value = %v (valid from %s, known at %s), want %v",
					got.Number, got.ValidFrom.Format(time.RFC3339), got.KnownAt.Format(time.RFC3339), tt.want)
			}
		})
	}
}

// TestBackdatedCorrectionDoesNotLeakBackwards covers the harder half of
// bitemporality: a correction written today that describes last week must be
// visible to a read as-of today and invisible to a read as-of last week, even
// though its validity time is in the past.
func TestBackdatedCorrectionDoesNotLeakBackwards(t *testing.T) {
	fs := newTestStore(t)
	k := testKey("units_sold")

	// The original figure, learned the morning after.
	if err := fs.Put(k, Value{Number: 100, ValidFrom: at(2, 0), KnownAt: at(3, 6)}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A correction for the same day, learned a week later after a stock count.
	if err := fs.Put(k, Value{Number: 87, ValidFrom: at(2, 0), KnownAt: at(10, 12), Source: "stock-count"}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// A model trained as of the fourth must see the original figure.
	got, err := fs.AsOf(k, at(4, 0))
	if err != nil {
		t.Fatalf("as-of: %v", err)
	}
	if got.Number != 100 {
		t.Errorf("as of the 4th the value is %v, want the 100 that was known then", got.Number)
	}

	// A model trained today sees the correction.
	got, err = fs.AsOf(k, at(12, 0))
	if err != nil {
		t.Fatalf("as-of: %v", err)
	}
	if got.Number != 87 {
		t.Errorf("as of the 12th the value is %v, want the corrected 87", got.Number)
	}
	if got.Source != "stock-count" {
		t.Errorf("source = %q, want the correction's", got.Source)
	}
}

// TestWritesOutOfOrderStillReadCorrectly checks the ordering property holds
// regardless of the order observations arrive in, since a replayed stream
// delivers them in whatever order the partitions happen to interleave.
func TestWritesOutOfOrderStillReadCorrectly(t *testing.T) {
	fs := newTestStore(t)
	k := testKey("inventory_level")
	writes := []Value{
		{Number: 30, ValidFrom: at(6, 0), KnownAt: at(6, 1)},
		{Number: 10, ValidFrom: at(2, 0), KnownAt: at(2, 1)},
		{Number: 20, ValidFrom: at(4, 0), KnownAt: at(4, 1)},
	}
	for _, v := range writes {
		if err := fs.Put(k, v); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	for _, tt := range []struct {
		asOf time.Time
		want float64
	}{{at(3, 0), 10}, {at(5, 0), 20}, {at(7, 0), 30}} {
		got, err := fs.AsOf(k, tt.asOf)
		if err != nil {
			t.Fatalf("as-of %s: %v", tt.asOf, err)
		}
		if got.Number != tt.want {
			t.Errorf("as of %s the value is %v, want %v", tt.asOf.Format(time.RFC3339), got.Number, tt.want)
		}
	}
}

func TestHistoryIsNewestKnowledgeFirstAndRespectsTheCut(t *testing.T) {
	fs := newTestStore(t)
	k := testKey("price_minor")
	for day := 2; day <= 8; day++ {
		if err := fs.Put(k, Value{Number: float64(100 + day), ValidFrom: at(day, 0), KnownAt: at(day, 1)}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	hist, err := fs.History(k, at(5, 12), 100)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 4 { // days 2, 3, 4, 5
		t.Fatalf("got %d observations, want 4: %+v", len(hist), hist)
	}
	for i := 1; i < len(hist); i++ {
		if !hist[i-1].KnownAt.After(hist[i].KnownAt) {
			t.Errorf("history is not newest-first at %d: %s then %s",
				i, hist[i-1].KnownAt, hist[i].KnownAt)
		}
	}
	if hist[0].Number != 105 {
		t.Errorf("newest value = %v, want 105", hist[0].Number)
	}
	for _, v := range hist {
		if v.KnownAt.After(at(5, 12)) {
			t.Errorf("history leaked a value known at %s, after the cut", v.KnownAt)
		}
	}
}

func TestVectorReportsMissingRatherThanDefaulting(t *testing.T) {
	fs := newTestStore(t)
	tenant, store, sku := canon.TenantID("acme"), canon.StoreID("store-001"), canon.SKU("sku-42")
	if err := fs.Put(Key{Tenant: tenant, Store: store, SKU: sku, Name: "price_minor"},
		Value{Number: 199, ValidFrom: at(2, 0), KnownAt: at(2, 0)}); err != nil {
		t.Fatalf("put: %v", err)
	}
	values, missing, err := fs.Vector(tenant, store, sku,
		[]string{"price_minor", "competitor_price_minor", "waste_rate"}, at(3, 0))
	if err != nil {
		t.Fatalf("vector: %v", err)
	}
	if values[0] != 199 {
		t.Errorf("price = %v, want 199", values[0])
	}
	if len(missing) != 2 || missing[0] != "competitor_price_minor" || missing[1] != "waste_rate" {
		t.Errorf("missing = %v, want the two unwritten features named", missing)
	}
}

func TestBatchWriteIsAtomicAndReadable(t *testing.T) {
	fs := newTestStore(t)
	recs := []Record{
		{Key: testKey("price_minor"), Value: Value{Number: 249, ValidFrom: at(2, 0), KnownAt: at(2, 0)}},
		{Key: testKey("units_sold"), Value: Value{Number: 12, ValidFrom: at(2, 0), KnownAt: at(2, 0)}},
	}
	if err := fs.PutBatch(recs); err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, r := range recs {
		got, err := fs.AsOf(r.Key, at(3, 0))
		if err != nil {
			t.Fatalf("as-of %s: %v", r.Key.Name, err)
		}
		if got.Number != r.Value.Number {
			t.Errorf("%s = %v, want %v", r.Key.Name, got.Number, r.Value.Number)
		}
	}
}

func TestKeyValidationRejectsNamespaceBreakouts(t *testing.T) {
	fs := newTestStore(t)
	bad := []Key{
		{Tenant: "", Store: "s", SKU: "k", Name: "n"},
		{Tenant: "a/b", Store: "s", SKU: "k", Name: "n"},
		{Tenant: "a", Store: "s#1", SKU: "k", Name: "n"},
		{Tenant: "a", Store: "s", SKU: "k:1", Name: "n"},
		{Tenant: "a", Store: "s", SKU: "k", Name: ""},
	}
	for _, k := range bad {
		if err := fs.Put(k, Value{Number: 1, ValidFrom: at(2, 0)}); !errors.Is(err, ErrInvalid) {
			t.Errorf("key %+v was accepted (err %v)", k, err)
		}
	}
}

func TestPutRejectsNonFiniteAndUndatedValues(t *testing.T) {
	fs := newTestStore(t)
	k := testKey("waste_rate")
	if err := fs.Put(k, Value{Number: 1, ValidFrom: time.Time{}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an undated observation was accepted: %v", err)
	}
	inf := 1.0
	for i := 0; i < 400; i++ {
		inf *= 10 // overflows to +Inf
	}
	if err := fs.Put(k, Value{Number: inf, ValidFrom: at(2, 0)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an infinite observation was accepted: %v", err)
	}
}

// TestSeriesAreIsolated proves the key layout keeps tenants, stores, SKUs and
// features apart — a cross-tenant read here would be a data-isolation breach,
// not merely a wrong number.
func TestSeriesAreIsolated(t *testing.T) {
	fs := newTestStore(t)
	base := Key{Tenant: "acme", Store: "s1", SKU: "sku1", Name: "price_minor"}
	variants := []Key{
		base,
		{Tenant: "other", Store: "s1", SKU: "sku1", Name: "price_minor"},
		{Tenant: "acme", Store: "s2", SKU: "sku1", Name: "price_minor"},
		{Tenant: "acme", Store: "s1", SKU: "sku2", Name: "price_minor"},
		{Tenant: "acme", Store: "s1", SKU: "sku1", Name: "waste_rate"},
	}
	for i, k := range variants {
		if err := fs.Put(k, Value{Number: float64(i), ValidFrom: at(2, 0), KnownAt: at(2, 0)}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	for i, k := range variants {
		got, err := fs.AsOf(k, at(3, 0))
		if err != nil {
			t.Fatalf("as-of %d: %v", i, err)
		}
		if got.Number != float64(i) {
			t.Errorf("series %+v returned %v, want %d — the key layout is not isolating series", k, got.Number, i)
		}
	}
}

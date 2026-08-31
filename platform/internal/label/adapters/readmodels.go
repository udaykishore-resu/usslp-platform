package adapters

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// Key-space layout for the Label Service's read models.
//
// Every key is a NUL-separated tuple with a two-byte tag, so that a prefix scan
// over one tag can never stray into another and so that the byte ordering the
// store guarantees is the ordering the index needs. The identifiers embedded in
// these keys are validated by canon.ValidID, which forbids NUL — without that,
// a store id containing a separator would let one tenant's scan reach another's
// rows.
//
//	d\0<label>                           -> placement (the label → placement map)
//	x\0<tenant>\0<store>\0<sku>\0<label> -> placement (the (tenant,store,sku) → labels map)
//	r\0<tenant>\0<store>\0<label>        -> placement (the store roster)
//	s\0<label>                           -> label state row
//	i\0<tenant>\0<store>\0<label>        -> label id (state rows by store)
//	t\0<tenant>\0<store>                 -> presence marker (a tenant's stores)
//	q\0<due:be8>\0<label>\0<schedule>    -> schedule entry (the due index)
//	k\0<label>\0<schedule>               -> the due key, so a cancel can find it
var (
	tagPlacement = []byte("d\x00")
	tagBySKU     = []byte("x\x00")
	tagRoster    = []byte("r\x00")
	tagState     = []byte("s\x00")
	tagStateIdx  = []byte("i\x00")
	tagStoreIdx  = []byte("t\x00")
	tagDue       = []byte("q\x00")
	tagDueRef    = []byte("k\x00")
)

func key(tag []byte, parts ...string) []byte {
	n := len(tag)
	for _, p := range parts {
		n += len(p) + 1
	}
	b := make([]byte, 0, n)
	b = append(b, tag...)
	for i, p := range parts {
		if i > 0 {
			b = append(b, 0)
		}
		b = append(b, p...)
	}
	return b
}

func prefix(tag []byte, parts ...string) []byte {
	b := key(tag, parts...)
	return append(b, 0)
}

func be8(v int64) []byte {
	b := make([]byte, 8)
	// Offset into the unsigned range so that a negative instant — which only a
	// misconfigured clock produces, but which must still sort correctly —
	// orders before a positive one under byte comparison.
	binary.BigEndian.PutUint64(b, uint64(v)+1<<63)
	return b
}

// KVDirectory is the kvstore-backed label placement read model.
//
// It maintains three indexes over one fact. The (tenant, store, sku) index is
// the hot one: it is what the price path scans forty thousand times during a
// store-wide promotion, and it exists so that resolving a fan-out is an ordered
// range scan rather than a filter over every label in the estate.
type KVDirectory struct {
	kv *kvstore.Store
}

// NewKVDirectory builds the directory.
func NewKVDirectory(kv *kvstore.Store) (*KVDirectory, error) {
	if kv == nil {
		return nil, errors.New("label/adapters: nil kvstore for directory")
	}
	return &KVDirectory{kv: kv}, nil
}

var _ ports.Directory = (*KVDirectory)(nil)

// Upsert records a placement, moving the SKU index when the assignment changed.
//
// The three index writes and the removal of the stale SKU entry go in one
// batch. Anything less would leave a window in which a fan-out scan sees the
// label under both its old SKU and its new one, and would price a shelf for a
// product it no longer holds.
func (d *KVDirectory) Upsert(ctx context.Context, p ports.Placement) error {
	if p.LabelID == "" {
		return fmt.Errorf("%w: placement without a label id", canon.ErrEnvelopeInvalid)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("label: encoding placement for %s: %w", p.LabelID, err)
	}
	batch := d.kv.NewBatch()
	if prev, err := d.Lookup(ctx, p.LabelID); err == nil {
		if prev.SKU != "" && (prev.SKU != p.SKU || prev.StoreID != p.StoreID || prev.TenantID != p.TenantID) {
			batch.Delete(key(tagBySKU, string(prev.TenantID), string(prev.StoreID), string(prev.SKU), string(prev.LabelID)))
		}
		if prev.StoreID != p.StoreID || prev.TenantID != p.TenantID {
			batch.Delete(key(tagRoster, string(prev.TenantID), string(prev.StoreID), string(prev.LabelID)))
		}
	} else if !errors.Is(err, ports.ErrNotFound) {
		return err
	}
	batch.Put(key(tagPlacement, string(p.LabelID)), body)
	batch.Put(key(tagRoster, string(p.TenantID), string(p.StoreID), string(p.LabelID)), body)
	if p.SKU != "" && !p.Retired {
		batch.Put(key(tagBySKU, string(p.TenantID), string(p.StoreID), string(p.SKU), string(p.LabelID)), body)
	} else {
		batch.Delete(key(tagBySKU, string(p.TenantID), string(p.StoreID), string(p.SKU), string(p.LabelID)))
	}
	return batch.Write()
}

// Lookup resolves one label's placement.
func (d *KVDirectory) Lookup(ctx context.Context, id canon.LabelID) (ports.Placement, error) {
	raw, err := d.kv.Get(key(tagPlacement, string(id)))
	if errors.Is(err, kvstore.ErrNotFound) {
		return ports.Placement{}, fmt.Errorf("%w: label %s", ports.ErrNotFound, id)
	}
	if err != nil {
		return ports.Placement{}, err
	}
	var p ports.Placement
	if err := json.Unmarshal(raw, &p); err != nil {
		return ports.Placement{}, fmt.Errorf("label: decoding placement for %s: %w", id, err)
	}
	return p, nil
}

// LabelsForSKU resolves the fan-out set for a price change.
func (d *KVDirectory) LabelsForSKU(ctx context.Context, tenant canon.TenantID, store canon.StoreID, sku canon.SKU) ([]ports.Placement, error) {
	return d.scan(ctx, prefix(tagBySKU, string(tenant), string(store), string(sku)))
}

// StoreLabels lists every placement in a store.
func (d *KVDirectory) StoreLabels(ctx context.Context, tenant canon.TenantID, store canon.StoreID) ([]ports.Placement, error) {
	return d.scan(ctx, prefix(tagRoster, string(tenant), string(store)))
}

func (d *KVDirectory) scan(ctx context.Context, pfx []byte) ([]ports.Placement, error) {
	it := d.kv.Scan(pfx)
	defer it.Close()
	var out []ports.Placement
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var p ports.Placement
		if err := json.Unmarshal(it.Value(), &p); err != nil {
			return nil, fmt.Errorf("label: decoding placement: %w", err)
		}
		out = append(out, p)
	}
	return out, it.Err()
}

// Remove deletes a placement and all of its index entries.
func (d *KVDirectory) Remove(ctx context.Context, id canon.LabelID) error {
	p, err := d.Lookup(ctx, id)
	if errors.Is(err, ports.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	batch := d.kv.NewBatch()
	batch.Delete(key(tagPlacement, string(id)))
	batch.Delete(key(tagRoster, string(p.TenantID), string(p.StoreID), string(id)))
	batch.Delete(key(tagBySKU, string(p.TenantID), string(p.StoreID), string(p.SKU), string(id)))
	return batch.Write()
}

// Clear empties the directory ahead of a rebuild.
func (d *KVDirectory) Clear(ctx context.Context) error {
	return clearPrefixes(ctx, d.kv, tagPlacement, tagBySKU, tagRoster)
}

// KVStateStore is the kvstore-backed query-side read model.
type KVStateStore struct {
	kv *kvstore.Store
}

// NewKVStateStore builds the read model.
func NewKVStateStore(kv *kvstore.Store) (*KVStateStore, error) {
	if kv == nil {
		return nil, errors.New("label/adapters: nil kvstore for state store")
	}
	return &KVStateStore{kv: kv}, nil
}

var _ ports.StateStore = (*KVStateStore)(nil)

// Put writes one row and its by-store index entry in a single atomic write, so
// a roster query can never see a label the row lookup then fails to find.
func (s *KVStateStore) Put(ctx context.Context, row ports.LabelState) error {
	if row.LabelID == "" {
		return fmt.Errorf("%w: state row without a label id", canon.ErrEnvelopeInvalid)
	}
	body, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("label: encoding state for %s: %w", row.LabelID, err)
	}
	batch := s.kv.NewBatch()
	batch.Put(key(tagState, string(row.LabelID)), body)
	if row.TenantID != "" && row.StoreID != "" {
		batch.Put(key(tagStateIdx, string(row.TenantID), string(row.StoreID), string(row.LabelID)), body)
		// A presence marker per (tenant, store), so "which stores does this
		// tenant have labels in" is a short prefix scan rather than a walk over
		// every row in the estate. The value is the store id itself so the scan
		// does not have to parse keys back apart.
		batch.Put(key(tagStoreIdx, string(row.TenantID), string(row.StoreID)), []byte(row.StoreID))
	}
	return batch.Write()
}

// Stores lists the stores a tenant has labels in.
func (s *KVStateStore) Stores(ctx context.Context, tenant canon.TenantID) ([]canon.StoreID, error) {
	it := s.kv.Scan(prefix(tagStoreIdx, string(tenant)))
	defer it.Close()
	var out []canon.StoreID
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, canon.StoreID(it.Value()))
	}
	return out, it.Err()
}

// Get reads one row.
func (s *KVStateStore) Get(ctx context.Context, id canon.LabelID) (ports.LabelState, error) {
	raw, err := s.kv.Get(key(tagState, string(id)))
	if errors.Is(err, kvstore.ErrNotFound) {
		return ports.LabelState{}, fmt.Errorf("%w: label %s", ports.ErrNotFound, id)
	}
	if err != nil {
		return ports.LabelState{}, err
	}
	var row ports.LabelState
	if err := json.Unmarshal(raw, &row); err != nil {
		return ports.LabelState{}, fmt.Errorf("label: decoding state for %s: %w", id, err)
	}
	return row, nil
}

// ListByStore returns every row for a store, in label-id order.
func (s *KVStateStore) ListByStore(ctx context.Context, tenant canon.TenantID, store canon.StoreID) ([]ports.LabelState, error) {
	it := s.kv.Scan(prefix(tagStateIdx, string(tenant), string(store)))
	defer it.Close()
	var out []ports.LabelState
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var row ports.LabelState
		if err := json.Unmarshal(it.Value(), &row); err != nil {
			return nil, fmt.Errorf("label: decoding state row: %w", err)
		}
		out = append(out, row)
	}
	return out, it.Err()
}

// Clear empties the read model ahead of a rebuild.
func (s *KVStateStore) Clear(ctx context.Context) error {
	return clearPrefixes(ctx, s.kv, tagState, tagStateIdx, tagStoreIdx)
}

// KVScheduleStore is the due-index for future-dated price changes.
//
// It is keyed by effective time first so that "what is due now" is a bounded
// range scan from the beginning of the index, which is the only shape that
// works when the index holds a chain's entire promotional calendar and the
// runner has to answer the question every fifteen seconds.
type KVScheduleStore struct {
	kv *kvstore.Store
}

// NewKVScheduleStore builds the index.
func NewKVScheduleStore(kv *kvstore.Store) (*KVScheduleStore, error) {
	if kv == nil {
		return nil, errors.New("label/adapters: nil kvstore for schedule store")
	}
	return &KVScheduleStore{kv: kv}, nil
}

var _ ports.ScheduleStore = (*KVScheduleStore)(nil)

func dueKey(at time.Time, label canon.LabelID, schedule string) []byte {
	b := append([]byte(nil), tagDue...)
	b = append(b, be8(at.UTC().UnixNano())...)
	b = append(b, 0)
	b = append(b, label...)
	b = append(b, 0)
	b = append(b, schedule...)
	return b
}

// Add records a scheduled change, replacing any earlier entry for the same
// schedule identifier so that a rescheduled promotion does not fire twice.
func (s *KVScheduleStore) Add(ctx context.Context, e ports.ScheduleEntry) error {
	if e.LabelID == "" || e.ScheduleID == "" {
		return fmt.Errorf("%w: schedule entry without a label or schedule id", canon.ErrEnvelopeInvalid)
	}
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("label: encoding schedule %s: %w", e.ScheduleID, err)
	}
	dk := dueKey(e.EffectiveAt, e.LabelID, e.ScheduleID)
	ref := key(tagDueRef, string(e.LabelID), e.ScheduleID)

	batch := s.kv.NewBatch()
	if old, gerr := s.kv.Get(ref); gerr == nil {
		batch.Delete(old)
	} else if !errors.Is(gerr, kvstore.ErrNotFound) {
		return gerr
	}
	batch.Put(dk, body)
	batch.Put(ref, dk)
	return batch.Write()
}

// Remove drops a scheduled change, whether it fired or was cancelled.
func (s *KVScheduleStore) Remove(ctx context.Context, label canon.LabelID, scheduleID string) error {
	ref := key(tagDueRef, string(label), scheduleID)
	dk, err := s.kv.Get(ref)
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	batch := s.kv.NewBatch()
	batch.Delete(dk)
	batch.Delete(ref)
	return batch.Write()
}

// Due returns entries whose effective time has arrived, in effective-time
// order.
func (s *KVScheduleStore) Due(ctx context.Context, at time.Time, limit int) ([]ports.ScheduleEntry, error) {
	start := append([]byte(nil), tagDue...)
	end := append(append([]byte(nil), tagDue...), be8(at.UTC().UnixNano())...)
	// The bound is exclusive of the key but inclusive of the instant: appending
	// a 0xff byte places it after every (label, schedule) suffix sharing that
	// nanosecond, so a change effective at exactly `at` is due now rather than
	// on the next tick.
	end = append(end, 0xff)

	it := s.kv.Range(start, end)
	defer it.Close()
	var out []ports.ScheduleEntry
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var e ports.ScheduleEntry
		if err := json.Unmarshal(it.Value(), &e); err != nil {
			return nil, fmt.Errorf("label: decoding schedule entry: %w", err)
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, it.Err()
}

// Clear empties the index ahead of a rebuild.
func (s *KVScheduleStore) Clear(ctx context.Context) error {
	return clearPrefixes(ctx, s.kv, tagDue, tagDueRef)
}

// clearPrefixes deletes every key under each tag.
//
// It collects the keys before deleting rather than deleting during the scan,
// because an iterator reads a consistent view taken at construction and
// mutating underneath it would leave the outcome dependent on timing — which is
// the one thing a rebuild must not be.
func clearPrefixes(ctx context.Context, kv *kvstore.Store, tags ...[]byte) error {
	for _, tag := range tags {
		var keys [][]byte
		it := kv.Scan(tag)
		for it.Next() {
			if err := ctx.Err(); err != nil {
				it.Close()
				return err
			}
			keys = append(keys, append([]byte(nil), it.Key()...))
		}
		err := it.Err()
		it.Close()
		if err != nil {
			return err
		}
		batch := kv.NewBatch()
		for _, k := range keys {
			batch.Delete(k)
		}
		if err := batch.Write(); err != nil {
			return err
		}
	}
	return nil
}

// Stage writes a state row into a caller-owned batch instead of committing it
// immediately. It is how the projection runner puts a read-model row and the
// projection's checkpoint into one atomic write.
func (s *KVStateStore) Stage(b *kvstore.Batch, row ports.LabelState) error {
	if row.LabelID == "" {
		return fmt.Errorf("%w: state row without a label id", canon.ErrEnvelopeInvalid)
	}
	body, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("label: encoding state for %s: %w", row.LabelID, err)
	}
	b.Put(key(tagState, string(row.LabelID)), body)
	if row.TenantID != "" && row.StoreID != "" {
		b.Put(key(tagStateIdx, string(row.TenantID), string(row.StoreID), string(row.LabelID)), body)
		b.Put(key(tagStoreIdx, string(row.TenantID), string(row.StoreID)), []byte(row.StoreID))
	}
	return nil
}

package adapters

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

func newKV(t *testing.T) *kvstore.Store {
	t.Helper()
	kv, err := kvstore.OpenWith(kvstore.Options{Sync: kvstore.SyncNever})
	if err != nil {
		t.Fatalf("open kvstore: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	return kv
}

// TestDirectoryReindexesOnReassignment covers the key-layout hazard that a
// planogram reset creates: a label moved between products must not remain
// resolvable under its old SKU, or a price change for the old product would
// reprice a shelf holding the new one.
func TestDirectoryReindexesOnReassignment(t *testing.T) {
	d, err := NewKVDirectory(newKV(t))
	if err != nil {
		t.Fatalf("directory: %v", err)
	}
	ctx := context.Background()
	base := ports.Placement{
		LabelID: "lbl-1", SECID: "sec-01", TenantID: "acme",
		StoreID: "store-01", Region: "us-east-1", SKU: "sku-eggs",
	}
	if err := d.Upsert(ctx, base); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	moved := base
	moved.SKU = "sku-milk"
	moved.SECID = "sec-02"
	if err := d.Upsert(ctx, moved); err != nil {
		t.Fatalf("reassign: %v", err)
	}

	eggs, err := d.LabelsForSKU(ctx, "acme", "store-01", "sku-eggs")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(eggs) != 0 {
		t.Fatalf("the stale SKU index survived reassignment: %+v", eggs)
	}
	milk, err := d.LabelsForSKU(ctx, "acme", "store-01", "sku-milk")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(milk) != 1 || milk[0].SECID != "sec-02" {
		t.Fatalf("reassigned placement = %+v", milk)
	}
	roster, err := d.StoreLabels(ctx, "acme", "store-01")
	if err != nil || len(roster) != 1 {
		t.Fatalf("roster = %d entries (%v)", len(roster), err)
	}
}

// TestDirectoryIsolatesTenants covers the isolation property the key layout has
// to provide: one tenant's scan must never reach another's rows, whatever the
// identifiers look like.
func TestDirectoryIsolatesTenants(t *testing.T) {
	d, err := NewKVDirectory(newKV(t))
	if err != nil {
		t.Fatalf("directory: %v", err)
	}
	ctx := context.Background()
	for _, p := range []ports.Placement{
		{LabelID: "lbl-a", TenantID: "acme", StoreID: "store-01", SKU: "sku-milk", SECID: "sec-01"},
		{LabelID: "lbl-b", TenantID: "acme-holdings", StoreID: "store-01", SKU: "sku-milk", SECID: "sec-01"},
		{LabelID: "lbl-c", TenantID: "acme", StoreID: "store-01-annex", SKU: "sku-milk", SECID: "sec-01"},
	} {
		if err := d.Upsert(ctx, p); err != nil {
			t.Fatalf("upsert %s: %v", p.LabelID, err)
		}
	}
	// "acme" is a prefix of "acme-holdings" and "store-01" of "store-01-annex":
	// without the NUL separators these three would collide in one scan.
	got, err := d.LabelsForSKU(ctx, "acme", "store-01", "sku-milk")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].LabelID != "lbl-a" {
		t.Fatalf("scan crossed a tenant or store boundary: %+v", got)
	}
}

func TestDirectoryRemoveAndClear(t *testing.T) {
	d, err := NewKVDirectory(newKV(t))
	if err != nil {
		t.Fatalf("directory: %v", err)
	}
	ctx := context.Background()
	p := ports.Placement{LabelID: "lbl-1", TenantID: "acme", StoreID: "store-01", SKU: "sku-milk", SECID: "sec-01"}
	if err := d.Upsert(ctx, p); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.Remove(ctx, "lbl-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := d.Lookup(ctx, "lbl-1"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("lookup after remove = %v, want ErrNotFound", err)
	}
	// Removing an absent placement is a no-op, so a rebuild can call it freely.
	if err := d.Remove(ctx, "lbl-absent"); err != nil {
		t.Fatalf("remove absent: %v", err)
	}
	if err := d.Upsert(ctx, p); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if err := d.Clear(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	roster, err := d.StoreLabels(ctx, "acme", "store-01")
	if err != nil || len(roster) != 0 {
		t.Fatalf("clear left %d entries (%v)", len(roster), err)
	}
}

// TestScheduleStoreOrdersByEffectiveTime covers the due-index's whole reason for
// existing: "what is due now" has to be a bounded range scan from the front of
// the index, in effective-time order, across a chain's entire promotional
// calendar.
func TestScheduleStoreOrdersByEffectiveTime(t *testing.T) {
	s, err := NewKVScheduleStore(newKV(t))
	if err != nil {
		t.Fatalf("schedule store: %v", err)
	}
	ctx := context.Background()
	base := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	add := func(id string, offset time.Duration) {
		t.Helper()
		if err := s.Add(ctx, ports.ScheduleEntry{
			ScheduleID: id, LabelID: canon.LabelID("lbl-" + id),
			TenantID: "acme", StoreID: "store-01", EffectiveAt: base.Add(offset),
		}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	add("c", 3*time.Hour)
	add("a", time.Hour)
	add("d", 4*time.Hour)
	add("b", 2*time.Hour)

	due, err := s.Due(ctx, base.Add(3*time.Hour), 0)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 3 {
		t.Fatalf("due = %d entries, want 3", len(due))
	}
	// A change effective at exactly the sweep instant is due now, not on the
	// next tick.
	want := []string{"a", "b", "c"}
	for i, id := range want {
		if due[i].ScheduleID != id {
			t.Fatalf("due[%d] = %s, want %s (order: %v)", i, due[i].ScheduleID, id, due)
		}
	}

	limited, err := s.Due(ctx, base.Add(4*time.Hour), 2)
	if err != nil {
		t.Fatalf("due with limit: %v", err)
	}
	if len(limited) != 2 || limited[0].ScheduleID != "a" {
		t.Fatalf("limited sweep = %+v", limited)
	}
}

// TestScheduleStoreRescheduleReplacesTheEntry covers the double-fire hazard: a
// promotion moved to a new time must not remain due at its old one.
func TestScheduleStoreRescheduleReplacesTheEntry(t *testing.T) {
	s, err := NewKVScheduleStore(newKV(t))
	if err != nil {
		t.Fatalf("schedule store: %v", err)
	}
	ctx := context.Background()
	base := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	entry := ports.ScheduleEntry{
		ScheduleID: "sch-1", LabelID: "lbl-1", TenantID: "acme",
		StoreID: "store-01", EffectiveAt: base.Add(time.Hour),
	}
	if err := s.Add(ctx, entry); err != nil {
		t.Fatalf("add: %v", err)
	}
	moved := entry
	moved.EffectiveAt = base.Add(5 * time.Hour)
	if err := s.Add(ctx, moved); err != nil {
		t.Fatalf("reschedule: %v", err)
	}

	early, err := s.Due(ctx, base.Add(2*time.Hour), 0)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("the superseded entry is still due at its old time: %+v", early)
	}
	later, err := s.Due(ctx, base.Add(6*time.Hour), 0)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(later) != 1 {
		t.Fatalf("rescheduled entry = %d, want 1", len(later))
	}

	if err := s.Remove(ctx, "lbl-1", "sch-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	gone, err := s.Due(ctx, base.Add(6*time.Hour), 0)
	if err != nil || len(gone) != 0 {
		t.Fatalf("removed entry is still due: %d (%v)", len(gone), err)
	}
	// Removing an entry that already fired is a no-op, which is what makes the
	// runner's cleanup safe to repeat.
	if err := s.Remove(ctx, "lbl-1", "sch-1"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

// TestStateStoreListsATenantsStores covers the index the promotion fan-out
// walks when a rule names no stores of its own, which is what a national
// promotion looks like.
func TestStateStoreListsATenantsStores(t *testing.T) {
	s, err := NewKVStateStore(newKV(t))
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	ctx := context.Background()
	rows := []ports.LabelState{
		{LabelID: "lbl-1", TenantID: "acme", StoreID: "store-01", State: "active"},
		{LabelID: "lbl-2", TenantID: "acme", StoreID: "store-01", State: "active"},
		{LabelID: "lbl-3", TenantID: "acme", StoreID: "store-02", State: "active"},
		// A tenant whose name is a prefix of another's, and a store whose name
		// is a prefix of another's: without the NUL separators these would leak
		// across the boundary.
		{LabelID: "lbl-4", TenantID: "acme-holdings", StoreID: "store-01", State: "active"},
		{LabelID: "lbl-5", TenantID: "acme", StoreID: "store-01-annex", State: "active"},
	}
	for _, row := range rows {
		if err := s.Put(ctx, row); err != nil {
			t.Fatalf("put %s: %v", row.LabelID, err)
		}
	}
	got, err := s.Stores(ctx, "acme")
	if err != nil {
		t.Fatalf("stores: %v", err)
	}
	want := []canon.StoreID{"store-01", "store-01-annex", "store-02"}
	if len(got) != len(want) {
		t.Fatalf("stores = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stores = %v, want %v (ordered)", got, want)
		}
	}
	if other, err := s.Stores(ctx, "acme-holdings"); err != nil || len(other) != 1 {
		t.Fatalf("the other tenant sees %v (%v)", other, err)
	}
	if none, err := s.Stores(ctx, "unknown"); err != nil || len(none) != 0 {
		t.Fatalf("an unknown tenant sees %v (%v)", none, err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if after, err := s.Stores(ctx, "acme"); err != nil || len(after) != 0 {
		t.Fatalf("clear left %v (%v)", after, err)
	}
}

func TestStateStoreRoundTripAndByStoreIndex(t *testing.T) {
	s, err := NewKVStateStore(newKV(t))
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	ctx := context.Background()
	row := ports.LabelState{
		LabelID: "lbl-1", TenantID: "acme", StoreID: "store-01", SECID: "sec-01",
		SKU: "sku-milk", Price: canon.NewMoney(279, "USD"),
		BasePrice: canon.NewMoney(349, "USD"), Category: "dairy", Sequence: 4,
		State: "active", Template: "standard", Version: 9,
		UpdatedAt: time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC),
	}
	if err := s.Put(ctx, row); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(ctx, "lbl-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Price != row.Price || got.Sequence != row.Sequence || got.Version != row.Version {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, row)
	}
	if !got.Healthy() {
		t.Fatalf("an active row with nothing outstanding should be healthy")
	}

	pending := row
	pending.LabelID = "lbl-2"
	pending.PendingSequence = 5
	if err := s.Put(ctx, pending); err != nil {
		t.Fatalf("put: %v", err)
	}
	if pending.Healthy() {
		t.Fatalf("a row with an unconfirmed update is not healthy")
	}

	rows, err := s.ListByStore(ctx, "acme", "store-01")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("store index holds %d rows, want 2", len(rows))
	}
	if got.BasePrice != row.BasePrice {
		t.Fatalf("base price did not round trip: %v", got.BasePrice)
	}
	if _, err := s.Get(ctx, "lbl-absent"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("get absent = %v, want ErrNotFound", err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	rows, err = s.ListByStore(ctx, "acme", "store-01")
	if err != nil || len(rows) != 0 {
		t.Fatalf("clear left %d rows (%v)", len(rows), err)
	}
}

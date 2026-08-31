package adapters

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
)

func newRepo(t *testing.T, snapshotEvery int64) (*EventStoreRepository, *eventstore.Store) {
	t.Helper()
	kv := newKV(t)
	store, err := eventstore.New(kv)
	if err != nil {
		t.Fatalf("event store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repo, err := NewEventStoreRepository(store, RepositoryConfig{SnapshotEvery: snapshotEvery})
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	return repo, store
}

var repoNow = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

func repoMeta(key string) ports.AppendMeta {
	return ports.AppendMeta{
		TenantID: "acme", StoreID: "store-01", Region: "us-east-1",
		Source: "label-service", IdempotencyKey: key, OccurredAt: repoNow,
	}
}

func provisionEvents(id canon.LabelID) []domain.Event {
	return []domain.Event{
		domain.LabelProvisioned{
			LabelID: id, TenantID: "acme", StoreID: "store-01", Region: "us-east-1",
			SECID: "sec-01", Currency: "USD", Template: domain.TemplateStandard,
			OccurredAt: repoNow.Add(-time.Hour),
		},
		domain.LabelAssigned{
			LabelID: id, SKU: "sku-milk", OccurredAt: repoNow.Add(-time.Hour),
		},
	}
}

// TestRepositoryLoadRebuildsFromEvents covers the write side's basic contract:
// state is a fold of the stream, and an unprovisioned label loads as its zero
// value rather than as an error.
func TestRepositoryLoadRebuildsFromEvents(t *testing.T) {
	repo, _ := newRepo(t, DefaultSnapshotEvery)
	ctx := context.Background()

	fresh, err := repo.Load(ctx, "lbl-unknown")
	if err != nil {
		t.Fatalf("loading an unknown label must not error: %v", err)
	}
	if fresh.Exists() || fresh.Version != 0 {
		t.Fatalf("unknown label = %+v", fresh)
	}

	if _, err := repo.Append(ctx, "lbl-1", 0, provisionEvents("lbl-1"), repoMeta("seed")); err != nil {
		t.Fatalf("append: %v", err)
	}
	agg, err := repo.Load(ctx, "lbl-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.State != domain.StateAssigned || agg.SKU != "sku-milk" || agg.Currency != "USD" {
		t.Fatalf("rebuilt aggregate = %+v", agg)
	}
	if agg.Version != 2 {
		t.Fatalf("version = %d, want 2", agg.Version)
	}
}

// TestRepositoryEnforcesOptimisticConcurrency covers the mechanism that makes
// two tills repricing one shelf safe: exactly one append lands, and the loser
// is told to re-read rather than silently overwriting.
func TestRepositoryEnforcesOptimisticConcurrency(t *testing.T) {
	repo, _ := newRepo(t, DefaultSnapshotEvery)
	ctx := context.Background()
	if _, err := repo.Append(ctx, "lbl-1", 0, provisionEvents("lbl-1"), repoMeta("seed")); err != nil {
		t.Fatalf("append: %v", err)
	}
	price := []domain.Event{domain.PriceApplied{
		LabelID: "lbl-1", StoreID: "store-01", SKU: "sku-milk",
		Price: canon.NewMoney(279, "USD"), EffectiveAt: repoNow, Sequence: 1,
		OccurredAt: repoNow,
	}}
	if _, err := repo.Append(ctx, "lbl-1", 2, price, repoMeta("a")); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	_, err := repo.Append(ctx, "lbl-1", 2, price, repoMeta("b"))
	if !errors.Is(err, ports.ErrConcurrency) {
		t.Fatalf("second writer at a stale version = %v, want ErrConcurrency", err)
	}
}

// TestRepositoryAppendIsIdempotent covers the at-least-once delivery guarantee
// at the aggregate boundary: a redelivered record must not append a second copy
// of the same decision.
func TestRepositoryAppendIsIdempotent(t *testing.T) {
	repo, _ := newRepo(t, DefaultSnapshotEvery)
	ctx := context.Background()
	events := provisionEvents("lbl-1")
	if _, err := repo.Append(ctx, "lbl-1", 0, events, repoMeta("webhook-1")); err != nil {
		t.Fatalf("first: %v", err)
	}
	out, err := repo.Append(ctx, "lbl-1", 0, events, repoMeta("webhook-1"))
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if !out.Duplicate {
		t.Fatalf("redelivery was not recognised as a duplicate")
	}
	agg, err := repo.Load(ctx, "lbl-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if agg.Version != 2 {
		t.Fatalf("version = %d after two deliveries of one record, want 2", agg.Version)
	}
}

// TestRepositorySnapshotsBoundReplay is the reason snapshots exist: a long-lived
// label in a high-churn category must not replay its whole history on every
// command inside a 120 ms budget slice.
func TestRepositorySnapshotsBoundReplay(t *testing.T) {
	const snapshotEvery = 8
	repo, store := newRepo(t, snapshotEvery)
	ctx := context.Background()
	if _, err := repo.Append(ctx, "lbl-1", 0, provisionEvents("lbl-1"), repoMeta("seed")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	version := int64(2)
	const changes = 40
	for i := 0; i < changes; i++ {
		at := repoNow.Add(time.Duration(i) * time.Minute)
		events := []domain.Event{domain.PriceApplied{
			LabelID: "lbl-1", StoreID: "store-01", SKU: "sku-milk",
			Price: canon.NewMoney(int64(200+i), "USD"), EffectiveAt: at,
			Sequence: int64(i + 1), OccurredAt: at,
		}}
		if _, err := repo.Append(ctx, "lbl-1", version, events, repoMeta(fmt.Sprintf("chg-%d", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		version++
	}

	snap, err := store.LoadSnapshot(eventstore.Stream(domain.AggregateType, "lbl-1"))
	if err != nil {
		t.Fatalf("no snapshot was written after %d events: %v", version, err)
	}
	if version-snap.Version > snapshotEvery {
		t.Fatalf("snapshot is %d events behind the head, want at most %d",
			version-snap.Version, snapshotEvery)
	}

	// A load after the snapshot must read only the tail, and must still produce
	// exactly the state a full replay produces.
	before := store.Stats().EventsRead
	agg, err := repo.Load(ctx, "lbl-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	read := store.Stats().EventsRead - before
	if read > snapshotEvery {
		t.Fatalf("loading read %d events; the snapshot is not bounding replay", read)
	}
	if agg.Sequence != changes || agg.Version != version {
		t.Fatalf("snapshotted load = seq %d version %d, want %d/%d",
			agg.Sequence, agg.Version, changes, version)
	}

	// Deleting the snapshot must change nothing but the cost.
	if err := store.DeleteSnapshot(eventstore.Stream(domain.AggregateType, "lbl-1")); err != nil {
		t.Fatalf("delete snapshot: %v", err)
	}
	replayed, err := repo.Load(ctx, "lbl-1")
	if err != nil {
		t.Fatalf("load after snapshot deletion: %v", err)
	}
	if replayed.Sequence != agg.Sequence || replayed.Price != agg.Price || replayed.Version != agg.Version {
		t.Fatalf("full replay diverged from the snapshotted load:\n got %+v\nwant %+v", replayed, agg)
	}
}

// TestRepositoryHistoryIsNewestFirstAndBounded covers the shape every caller
// wants: the API's price history, the compliance export's page, and the
// republish path's search all ask for the most recent events, bounded.
func TestRepositoryHistoryIsNewestFirstAndBounded(t *testing.T) {
	repo, _ := newRepo(t, DefaultSnapshotEvery)
	ctx := context.Background()
	if _, err := repo.Append(ctx, "lbl-1", 0, provisionEvents("lbl-1"), repoMeta("seed")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	version := int64(2)
	for i := 0; i < 5; i++ {
		at := repoNow.Add(time.Duration(i) * time.Minute)
		if _, err := repo.Append(ctx, "lbl-1", version, []domain.Event{domain.PriceApplied{
			LabelID: "lbl-1", StoreID: "store-01", SKU: "sku-milk",
			Price: canon.NewMoney(int64(200+i), "USD"), EffectiveAt: at,
			Sequence: int64(i + 1), OccurredAt: at,
		}}, repoMeta(fmt.Sprintf("chg-%d", i))); err != nil {
			t.Fatalf("append: %v", err)
		}
		version++
	}

	history, err := repo.History(ctx, "lbl-1", 3)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history = %d events, want 3", len(history))
	}
	for i := 1; i < len(history); i++ {
		if history[i-1].Version <= history[i].Version {
			t.Fatalf("history is not newest-first: %v then %v", history[i-1].Version, history[i].Version)
		}
	}
	newest, ok := history[0].Event.(*domain.PriceApplied)
	if !ok {
		t.Fatalf("newest event is %T", history[0].Event)
	}
	if newest.Sequence != 5 {
		t.Fatalf("newest sequence = %d, want 5", newest.Sequence)
	}
	if history[0].Envelope.TenantID != "acme" || history[0].Envelope.Source != "label-service" {
		t.Fatalf("stored envelope lost its identity: %+v", history[0].Envelope)
	}

	all, err := repo.History(ctx, "lbl-1", 0)
	if err != nil {
		t.Fatalf("unbounded history: %v", err)
	}
	if len(all) != 7 {
		t.Fatalf("unbounded history = %d events, want 7", len(all))
	}
	none, err := repo.History(ctx, "lbl-absent", 10)
	if err != nil || len(none) != 0 {
		t.Fatalf("history of an unknown label = %d (%v)", len(none), err)
	}
}

// TestRepositoryRefusesAnUntenantedEvent covers the routing invariant: an
// envelope that cannot name its tenant has nowhere to go, so it is refused at
// the boundary rather than written and discovered later.
func TestRepositoryRefusesAnUntenantedEvent(t *testing.T) {
	repo, _ := newRepo(t, DefaultSnapshotEvery)
	_, err := repo.Append(context.Background(), "lbl-1", 0, provisionEvents("lbl-1"), ports.AppendMeta{})
	if !errors.Is(err, canon.ErrEnvelopeInvalid) {
		t.Fatalf("error = %v, want ErrEnvelopeInvalid", err)
	}
}

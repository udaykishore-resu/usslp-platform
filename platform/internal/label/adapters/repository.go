// Package adapters implements the Label Service's ports against the platform's
// infrastructure: the event store for the write side, the key/value store for
// the read models, the event bus for stream traffic and MQTT for the device
// tier.
//
// Everything that knows a storage layout, a topic name or a wire encoding lives
// here. Nothing here makes a business decision — the moment an adapter starts
// deciding whether a price is acceptable, the rule becomes untestable without
// standing up its infrastructure, which is exactly the failure mode the
// hexagonal split exists to prevent.
package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
)

// DefaultSnapshotEvery is how many events may accumulate on a label's stream
// before a snapshot is written.
//
// Sixty-four is sized from the worst realistic stream rather than from a round
// number. A label in a high-churn category — fresh produce marked down three
// times a day, plus delivery confirmations for each — accumulates a few
// thousand events a year, and without snapshots every command replays all of
// them inside a 120 ms budget slice. With a snapshot every 64 events the write
// side reads one blob plus at most 63 events, whatever the label's age. The
// cost is one extra durable write per 64, which is noise beside the append it
// rides behind.
const DefaultSnapshotEvery = 64

// EventStoreRepository is the aggregate repository.
type EventStoreRepository struct {
	store         *eventstore.Store
	snapshotEvery int64
	source        string
}

// RepositoryConfig configures the repository.
type RepositoryConfig struct {
	// SnapshotEvery is the snapshot interval in stream versions. Zero means
	// DefaultSnapshotEvery; a negative value disables snapshots, which is only
	// appropriate for a test that wants to exercise full replay.
	SnapshotEvery int64
	// Source is the value written to Envelope.Source. Empty means
	// "label-service".
	Source string
}

// NewEventStoreRepository builds the repository.
func NewEventStoreRepository(store *eventstore.Store, cfg RepositoryConfig) (*EventStoreRepository, error) {
	if store == nil {
		return nil, errors.New("label/adapters: nil event store")
	}
	if cfg.SnapshotEvery == 0 {
		cfg.SnapshotEvery = DefaultSnapshotEvery
	}
	if cfg.Source == "" {
		cfg.Source = "label-service"
	}
	return &EventStoreRepository{store: store, snapshotEvery: cfg.SnapshotEvery, source: cfg.Source}, nil
}

var _ ports.Repository = (*EventStoreRepository)(nil)

func streamOf(id canon.LabelID) eventstore.StreamID {
	return eventstore.Stream(domain.AggregateType, string(id))
}

// Load rebuilds a label from its snapshot plus the events since.
//
// A snapshot that cannot be decoded is not an error: snapshots are strictly
// derived data, and the correct response to one written by an older encoding of
// the aggregate is to ignore it and replay from the beginning. Failing the load
// instead would take a label out of service over an optimisation.
func (r *EventStoreRepository) Load(ctx context.Context, id canon.LabelID) (*domain.Label, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: empty label id", ports.ErrNotFound)
	}
	stream := streamOf(id)
	agg := domain.New(id)
	from := int64(1)

	snap, err := r.store.LoadSnapshot(stream)
	switch {
	case err == nil:
		var state domain.Label
		if uerr := json.Unmarshal(snap.State, &state); uerr == nil && state.ID == id {
			agg = &state
			agg.Version = snap.Version
			from = snap.Version + 1
		}
	case errors.Is(err, eventstore.ErrNoSnapshot):
		// Expected for a young label.
	default:
		return nil, fmt.Errorf("label: loading snapshot for %s: %w", id, err)
	}

	records, err := r.store.ReadStream(ctx, stream, from, 0)
	if err != nil {
		return nil, fmt.Errorf("label: reading stream for %s: %w", id, err)
	}
	for _, rec := range records {
		ev, derr := domain.DecodeEvent(rec.Event.EventType, rec.Event.Payload)
		if derr != nil {
			return nil, fmt.Errorf("label: replaying %s version %d: %w", id, rec.Version, derr)
		}
		agg.Apply(ev)
		agg.Version = rec.Version
	}
	return agg, nil
}

// Append persists events under optimistic concurrency control.
func (r *EventStoreRepository) Append(ctx context.Context, id canon.LabelID, expectedVersion int64, events []domain.Event, meta ports.AppendMeta) (ports.AppendOutcome, error) {
	if len(events) == 0 {
		return ports.AppendOutcome{Version: expectedVersion}, nil
	}
	envs := make([]canon.Envelope, 0, len(events))
	for i, e := range events {
		env, err := r.envelope(id, e, meta, i == 0)
		if err != nil {
			return ports.AppendOutcome{}, err
		}
		envs = append(envs, env)
	}
	stream := streamOf(id)
	res, err := r.store.AppendWithResult(ctx, stream, expectedVersion, envs...)
	if err != nil {
		if errors.Is(err, eventstore.ErrConcurrency) {
			// Translate at the boundary so the application layer never imports
			// the event store just to recognise a conflict.
			return ports.AppendOutcome{}, fmt.Errorf("%w: %v", ports.ErrConcurrency, err)
		}
		return ports.AppendOutcome{}, fmt.Errorf("label: appending to %s: %w", id, err)
	}

	out := ports.AppendOutcome{
		Duplicate: res.Duplicate,
		Version:   res.LastVersion,
		Events:    make([]ports.StoredEvent, 0, len(res.Events)),
	}
	for _, rec := range res.Events {
		ev, derr := domain.DecodeEvent(rec.Event.EventType, rec.Event.Payload)
		if derr != nil {
			return ports.AppendOutcome{}, derr
		}
		out.Events = append(out.Events, ports.StoredEvent{
			Position: rec.Position, Version: rec.Version, Event: ev, Envelope: rec.Event,
		})
	}
	if !res.Duplicate {
		r.maybeSnapshot(ctx, id, expectedVersion, res.LastVersion)
	}
	return out, nil
}

// maybeSnapshot writes a snapshot when the append crossed an interval boundary.
//
// It reloads rather than folding the events onto a caller-supplied aggregate,
// because the repository's contract is deliberately state-free: an aggregate
// handed in by a caller might already have events applied, or might be a
// different generation of the same label, and a snapshot written from it would
// be wrong in a way nothing would detect until a replay disagreed with a shelf.
// The reload costs one replay per interval, which is what the interval is for.
func (r *EventStoreRepository) maybeSnapshot(ctx context.Context, id canon.LabelID, before, after int64) {
	if r.snapshotEvery <= 0 {
		return
	}
	if before/r.snapshotEvery == after/r.snapshotEvery {
		return
	}
	agg, err := r.Load(ctx, id)
	if err != nil {
		return
	}
	state, err := json.Marshal(agg)
	if err != nil {
		return
	}
	// A failed snapshot costs replay time on the next load and nothing else, so
	// it is deliberately not surfaced as a command failure.
	_ = r.store.SaveSnapshot(streamOf(id), agg.Version, state)
}

// History returns a label's most recent events, newest first.
//
// "Newest first, bounded" is the shape every caller wants: the API's price
// history, the compliance export's page, and the republish path's search for
// the update it failed to send. Reading the whole stream and reversing it would
// make an audit query on a five-year-old label an unbounded read.
func (r *EventStoreRepository) History(ctx context.Context, id canon.LabelID, limit int) ([]ports.StoredEvent, error) {
	stream := streamOf(id)
	version, err := r.store.Version(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("label: reading version of %s: %w", id, err)
	}
	if version == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = int(version)
	}
	from := version - int64(limit) + 1
	if from < 1 {
		from = 1
	}
	records, err := r.store.ReadStream(ctx, stream, from, limit)
	if err != nil {
		return nil, fmt.Errorf("label: reading history of %s: %w", id, err)
	}
	out := make([]ports.StoredEvent, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		ev, derr := domain.DecodeEvent(rec.Event.EventType, rec.Event.Payload)
		if derr != nil {
			return nil, derr
		}
		out = append(out, ports.StoredEvent{
			Position: rec.Position, Version: rec.Version, Event: ev, Envelope: rec.Event,
		})
	}
	return out, nil
}

func (r *EventStoreRepository) envelope(id canon.LabelID, e domain.Event, meta ports.AppendMeta, first bool) (canon.Envelope, error) {
	body, err := domain.EncodeEvent(e)
	if err != nil {
		return canon.Envelope{}, err
	}
	occurred := e.At()
	if occurred.IsZero() {
		occurred = meta.OccurredAt
	}
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	source := meta.Source
	if source == "" {
		source = r.source
	}
	env := canon.Envelope{
		EventID:       canon.NewEventID(),
		EventType:     e.EventType(),
		AggregateType: domain.AggregateType,
		AggregateID:   string(id),
		TenantID:      meta.TenantID,
		StoreID:       meta.StoreID,
		Region:        meta.Region,
		OccurredAt:    occurred.UTC(),
		RecordedAt:    time.Now().UTC(),
		TraceID:       meta.TraceID,
		SpanID:        meta.SpanID,
		CorrelationID: meta.CorrelationID,
		CausationID:   meta.CausationID,
		Source:        source,
		SchemaVersion: canon.SchemaVersion,
		Payload:       body,
	}
	if first {
		// The key goes on the first event only. The event store treats a batch
		// whose keys have all been seen as one no-op, so keying every event
		// would be redundant, and keying a subset would trip its
		// partial-duplicate guard on a legitimate retry.
		env.IdempotencyKey = meta.IdempotencyKey
	}
	if env.TenantID == "" {
		// Every envelope must name its tenant: it is the routing key, the ACL
		// boundary and the audit partition. An event that cannot name one has
		// nowhere to go.
		return canon.Envelope{}, fmt.Errorf("%w: no tenant for %s on %s",
			canon.ErrEnvelopeInvalid, e.EventType(), id)
	}
	return env, nil
}

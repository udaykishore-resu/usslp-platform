package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// StreamConsumer runs one event-bus subscription against one handler.
//
// It is a thin wrapper, and deliberately so: the bus already owns retries,
// dead-lettering and group rebalancing, and a second layer of policy here would
// only make the two disagree. What it adds is the standard instrumentation
// every USSLP consumer reports under the same names, so one dashboard covers a
// service that did not exist when the dashboard was written.
type StreamConsumer struct {
	consumer eventbus.Consumer
	handler  eventbus.Handler
	group    string
	topics   []string
	metrics  *obs.StandardMetrics
	log      *obs.Logger
}

// NewStreamConsumer subscribes a group and prepares it to run.
func NewStreamConsumer(bus eventbus.Bus, opts eventbus.SubscribeOptions, h eventbus.Handler, metrics *obs.StandardMetrics, log *obs.Logger) (*StreamConsumer, error) {
	if bus == nil {
		return nil, errors.New("label/adapters: nil event bus")
	}
	if h == nil {
		return nil, errors.New("label/adapters: nil handler")
	}
	if log == nil {
		log = obs.NopLogger()
	}
	consumer, err := bus.Subscribe(opts)
	if err != nil {
		return nil, fmt.Errorf("label: subscribing %s to %v: %w", opts.Group, opts.Topics, err)
	}
	return &StreamConsumer{
		consumer: consumer, handler: h, group: opts.Group,
		topics: opts.Topics, metrics: metrics, log: log,
	}, nil
}

// Run delivers records until ctx is cancelled.
func (c *StreamConsumer) Run(ctx context.Context) error {
	return c.consumer.Run(ctx, func(ctx context.Context, m eventbus.Message) error {
		start := time.Now()
		err := c.handler(ctx, m)
		if c.metrics != nil {
			outcome := "ok"
			if err != nil {
				outcome = "error"
			}
			c.metrics.EventsConsumed.With(m.Topic, c.group, outcome).Inc()
			c.metrics.EventHandlerDuration.With(m.Topic, c.group).Observe(time.Since(start).Seconds())
		}
		if err != nil {
			c.log.FromContext(ctx).Error("event handler failed",
				"topic", m.Topic, "group", c.group,
				"partition", m.Partition, "offset", m.Offset, "error", err)
		}
		return err
	})
}

// PublishLag exports the subscription's per-partition lag. The autoscaler keys
// off it, and it is the number that distinguishes "we are slow" from "we are
// behind".
func (c *StreamConsumer) PublishLag(ctx context.Context) error {
	if c.metrics == nil {
		return nil
	}
	lag, err := c.consumer.Lag(ctx)
	if err != nil {
		return err
	}
	for _, topic := range c.topics {
		for partition, n := range lag {
			c.metrics.SetLag(topic, c.group, partition, n)
		}
	}
	return nil
}

// Close releases the subscription.
func (c *StreamConsumer) Close() error { return c.consumer.Close() }

// StateProjectionRunner drives the query-side read model from the event store.
type StateProjectionRunner struct {
	projection *eventstore.Projection
	state      *KVStateStore
	view       *app.LabelStateProjection
}

// NewStateProjectionRunner wires the projection to the store.
//
// Each event's read-model row and the projection's checkpoint are committed in
// one atomic write. Without that, a crash between the row and the checkpoint
// would either double-apply an event or skip one, and a read model that has
// skipped a price change reports a price nobody authorised.
func NewStateProjectionRunner(store *eventstore.Store, state *KVStateStore, view *app.LabelStateProjection, name string) (*StateProjectionRunner, error) {
	if store == nil {
		return nil, errors.New("label/adapters: nil event store")
	}
	if state == nil || view == nil {
		return nil, errors.New("label/adapters: state projection needs a store and a view")
	}
	if name == "" {
		name = "label-state"
	}
	r := &StateProjectionRunner{state: state, view: view}
	proj, err := store.NewProjection(name, func(ctx context.Context, rec eventstore.Recorded, b *kvstore.Batch) error {
		aggType, _ := rec.Stream.Split()
		if aggType != domain.AggregateType {
			return nil
		}
		ev, err := domain.DecodeEvent(rec.Event.EventType, rec.Event.Payload)
		if err != nil {
			return err
		}
		return view.WithStore(&batchStateStore{store: state, batch: b}).Apply(ctx, ports.StoredEvent{
			Position: rec.Position, Version: rec.Version, Event: ev, Envelope: rec.Event,
		})
	})
	if err != nil {
		return nil, err
	}
	r.projection = proj
	return r, nil
}

// CatchUp applies everything already in the store and returns the position it
// reached. A replica calls it before reporting ready, so it never serves a read
// model it is still building.
func (r *StateProjectionRunner) CatchUp(ctx context.Context) (int64, error) {
	return r.projection.CatchUp(ctx)
}

// Run follows the live tail until ctx is cancelled.
func (r *StateProjectionRunner) Run(ctx context.Context) error { return r.projection.Run(ctx) }

// Position returns the durable checkpoint.
func (r *StateProjectionRunner) Position() (int64, error) { return r.projection.Position() }

// Rebuild drops the read model and builds it again from position zero. It is
// the entire migration story for a read-model change: the events are the truth,
// so a projection whose shape was wrong is fixed by replaying, with no
// migration and no write-side downtime.
func (r *StateProjectionRunner) Rebuild(ctx context.Context) (int64, error) {
	return r.projection.Rebuild(ctx, r.state.Clear)
}

// batchStateStore stages read-model writes into the projection's atomic batch.
type batchStateStore struct {
	store *KVStateStore
	batch *kvstore.Batch
}

// Put stages the row on the projection's batch.
func (b *batchStateStore) Put(ctx context.Context, row ports.LabelState) error {
	return b.store.Stage(b.batch, row)
}

// Get reads committed state. One batch covers one event, which writes at most
// one row, so there is never a staged write to read back.
func (b *batchStateStore) Get(ctx context.Context, id canon.LabelID) (ports.LabelState, error) {
	return b.store.Get(ctx, id)
}

// ListByStore delegates; the projection never calls it.
func (b *batchStateStore) ListByStore(ctx context.Context, tenant canon.TenantID, store canon.StoreID) ([]ports.LabelState, error) {
	return b.store.ListByStore(ctx, tenant, store)
}

// Stores delegates; the projection never calls it.
func (b *batchStateStore) Stores(ctx context.Context, tenant canon.TenantID) ([]canon.StoreID, error) {
	return b.store.Stores(ctx, tenant)
}

// Clear is refused inside a projection batch: emptying the read model is a
// rebuild operation, and doing it from inside one event's unit of work would
// make the outcome depend on where the rebuild happened to be interrupted.
func (b *batchStateStore) Clear(ctx context.Context) error {
	return errors.New("label/adapters: cannot clear the read model from inside a projection batch")
}

var _ ports.StateStore = (*batchStateStore)(nil)

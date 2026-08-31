package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// MQTTDevicePublisher pushes authorised updates to the store tier.
type MQTTDevicePublisher struct {
	client msgbus.Client
}

// NewMQTTDevicePublisher builds the publisher.
func NewMQTTDevicePublisher(client msgbus.Client) (*MQTTDevicePublisher, error) {
	if client == nil {
		return nil, errors.New("label/adapters: nil MQTT client")
	}
	return &MQTTDevicePublisher{client: client}, nil
}

var _ ports.DevicePublisher = (*MQTTDevicePublisher)(nil)

// PublishPrice sends one update to the controller that owns the label.
//
// Three choices here are contract, not preference:
//
//   - The topic is the controller's zone topic, not a flat per-label one. A
//     store has ~25 controllers and up to 40,000 labels; with a flat namespace
//     each controller would receive and discard 39,000 messages it does not
//     own.
//   - QoS 1. A duplicated update is harmless because the label discards any
//     sequence it has already displayed; a lost one is a compliance incident.
//   - Retained. A controller rebooting after a power cut recovers the current
//     price of every label in its zone from the local broker, with no round
//     trip to a cloud that may be unreachable. This single flag is most of what
//     makes a store survive a WAN outage with correct prices on the glass.
func (p *MQTTDevicePublisher) PublishPrice(ctx context.Context, target ports.Placement, env canon.Envelope) error {
	scope := canon.TopicScope{Tenant: target.TenantID, Region: target.Region, Store: target.StoreID}
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("label: topic scope for %s: %w", target.LabelID, err)
	}
	if target.SECID == "" {
		return fmt.Errorf("%w: label %s has no controller", canon.ErrEnvelopeInvalid, target.LabelID)
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("label: encoding update for %s: %w", target.LabelID, err)
	}
	return p.client.Publish(ctx, msgbus.Message{
		Topic:   scope.SECLabelTopic(target.SECID, target.LabelID, canon.LeafPrice),
		Payload: body,
		QoS:     msgbus.AtLeastOnce,
		Retain:  true,
	})
}

// Connected reports link state for the readiness check. A broker blip must
// remove the pod from the load balancer, never restart it.
func (p *MQTTDevicePublisher) Connected() bool { return p.client.Connected() }

// BusStreamPublisher publishes envelopes onto the platform's event streams.
type BusStreamPublisher struct {
	bus     eventbus.Publisher
	metrics *obs.StandardMetrics
}

// NewBusStreamPublisher builds the publisher. Metrics may be nil.
func NewBusStreamPublisher(bus eventbus.Publisher, metrics *obs.StandardMetrics) (*BusStreamPublisher, error) {
	if bus == nil {
		return nil, errors.New("label/adapters: nil event bus")
	}
	return &BusStreamPublisher{bus: bus, metrics: metrics}, nil
}

var _ ports.StreamPublisher = (*BusStreamPublisher)(nil)

// Publish appends one envelope to a stream, returning only once it is durable.
func (p *BusStreamPublisher) Publish(ctx context.Context, stream string, env canon.Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("label: encoding %s for %s: %w", env.EventType, stream, err)
	}
	if err := p.bus.Publish(ctx, eventbus.Message{
		Topic: stream,
		Key:   partitionKeyFor(stream, env),
		Value: body,
		Headers: map[string]string{
			eventbus.HeaderEventType:     env.EventType,
			eventbus.HeaderTenantID:      string(env.TenantID),
			eventbus.HeaderStoreID:       string(env.StoreID),
			eventbus.HeaderCorrelationID: string(env.CorrelationID),
			eventbus.HeaderSchemaVersion: "1",
			eventbus.HeaderIdempotency:   env.IdempotencyKey,
		},
		Timestamp: env.RecordedAt,
	}); err != nil {
		return fmt.Errorf("label: publishing %s to %s: %w", env.EventType, stream, err)
	}
	if p.metrics != nil {
		p.metrics.EventsPublished.With(stream, env.EventType).Inc()
	}
	return nil
}

// partitionKeyFor returns the key each stream is defined to be partitioned by.
//
// canon.Envelope.PartitionKey derives its key from the payload and answers
// "store:sku", which is right for `price-updates` — two changes to the same
// product in the same store must be strictly ordered — and wrong for the two
// streams this service produces:
//
//   - `label-state` is compacted, keyed by label. Keying it by store:sku would
//     make compaction collapse every facing of a product onto one row, so a
//     read model rebuilt from it would know the current price of one label per
//     product and nothing about the rest.
//   - `audit-log` is keyed by tenant, so one retailer's compliance record is one
//     ordered sequence a regulator can read end to end.
//   - `label-delivery` is keyed by label, so one label's confirmations stay in
//     order while different labels proceed in parallel.
//
// Anything else falls back to the envelope's own answer.
func partitionKeyFor(stream string, env canon.Envelope) string {
	switch stream {
	case canon.StreamLabelState.Name, canon.StreamDelivery.Name:
		if env.AggregateID != "" {
			return env.AggregateID
		}
	case canon.StreamAudit.Name:
		return string(env.TenantID)
	}
	return env.PartitionKey()
}

// ACKBridge subscribes to every delivery acknowledgement in the estate and
// republishes it onto the `label-delivery` stream.
//
// It exists because MQTT and the event log answer different questions. The
// broker's job is to get a message between two machines; it has no durable
// ordered history, no consumer groups and no replay. Analytics, the SLO report
// and the compliance archive all need those, so the ACK crosses from the device
// world into the event world exactly once, here, and everything downstream
// reads a stream instead of a broker subscription.
type ACKBridge struct {
	client  msgbus.Client
	streams ports.StreamPublisher
	log     *obs.Logger
	stream  string
}

// ACKBridgeConfig configures the bridge.
type ACKBridgeConfig struct {
	// Stream overrides the destination stream. Empty means `label-delivery`.
	Stream string
	// Log receives malformed-payload warnings. Nil is silent.
	Log *obs.Logger
}

// NewACKBridge builds the bridge.
func NewACKBridge(client msgbus.Client, streams ports.StreamPublisher, cfg ACKBridgeConfig) (*ACKBridge, error) {
	if client == nil {
		return nil, errors.New("label/adapters: nil MQTT client for ACK bridge")
	}
	if streams == nil {
		return nil, errors.New("label/adapters: nil stream publisher for ACK bridge")
	}
	if cfg.Stream == "" {
		cfg.Stream = canon.StreamDelivery.Name
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	return &ACKBridge{client: client, streams: streams, log: cfg.Log, stream: cfg.Stream}, nil
}

// Start subscribes to the cross-tenant ACK filter. The subscription is QoS 1:
// an acknowledgement that never arrives leaves a label counted as pending
// forever, which is the difference between an accurate SLO and a fictional one.
func (b *ACKBridge) Start(ctx context.Context) error {
	return b.client.Subscribe(ctx, canon.FilterAllACKs, msgbus.AtLeastOnce, b.handle)
}

// Stop removes the subscription.
func (b *ACKBridge) Stop(ctx context.Context) error {
	return b.client.Unsubscribe(ctx, canon.FilterAllACKs)
}

// handle republishes one acknowledgement.
//
// The topic is the authority on tenancy, not the payload. A controller can put
// anything in a message body, but it can only publish to topics its credential
// authorises — `usslp/{its own tenant}/#` — so deriving the tenant from the
// topic is what stops a compromised controller from filing acknowledgements
// against another retailer's labels.
func (b *ACKBridge) handle(ctx context.Context, m msgbus.Message) {
	scope, sec, label, leaf, ok := canon.ParseSECLabelTopic(m.Topic)
	if !ok || leaf != canon.LeafACK {
		b.log.Warn("discarding acknowledgement on an unexpected topic", "topic", m.Topic)
		return
	}
	var env canon.Envelope
	if err := json.Unmarshal(m.Payload, &env); err != nil {
		b.log.Warn("discarding malformed acknowledgement",
			"topic", m.Topic, "label_id", string(label), "error", err)
		return
	}
	env.TenantID = scope.Tenant
	env.StoreID = scope.Store
	env.Region = scope.Region
	if env.EventID == "" {
		env.EventID = canon.NewEventID()
	}
	if env.EventType == "" {
		env.EventType = canon.EvtLabelDelivered
	}
	if env.AggregateType == "" {
		env.AggregateType = "label"
	}
	if env.AggregateID == "" {
		env.AggregateID = string(label)
	}
	if env.Source == "" {
		env.Source = "sec/" + string(sec)
	}
	if env.SchemaVersion == 0 {
		env.SchemaVersion = canon.SchemaVersion
	}
	if env.OccurredAt.IsZero() {
		env.OccurredAt = messageTime(m)
	}
	env.RecordedAt = time.Now().UTC()
	if err := env.Validate(); err != nil {
		b.log.Warn("discarding unusable acknowledgement",
			"topic", m.Topic, "label_id", string(label), "error", err)
		return
	}
	if err := b.streams.Publish(ctx, b.stream, env); err != nil {
		// The broker has already released the message; there is nothing to
		// retry against. Logging loudly is the honest response — the symptom
		// downstream is a label that stays pending, and this line is what
		// explains it.
		b.log.Error("republishing acknowledgement to the delivery stream failed",
			"topic", m.Topic, "label_id", string(label), "error", err)
	}
}

func messageTime(m msgbus.Message) time.Time {
	if !m.ReceivedAt.IsZero() {
		return m.ReceivedAt.UTC()
	}
	return time.Now().UTC()
}

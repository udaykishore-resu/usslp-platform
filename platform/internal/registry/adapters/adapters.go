// Package adapters implements the Device Registry's ports against the
// platform's real infrastructure: the durable event bus, an MQTT broker and the
// certificate hierarchy.
//
// Each adapter is deliberately thin. The rule this package follows is that an
// adapter translates and does not decide: it may reshape an envelope into a bus
// message or a PEM chain into a verified identity, but every question with a
// business answer — is this device a clone, may this label be addressed, should
// this job roll back — belongs in the domain and application layers, where it
// can be tested without a broker.
package adapters

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// BusPublisher publishes registry events onto the platform's durable streams
// through the eventbus port.
type BusPublisher struct {
	bus eventbus.Publisher
}

// NewBusPublisher wraps an event bus.
func NewBusPublisher(bus eventbus.Publisher) *BusPublisher { return &BusPublisher{bus: bus} }

// StreamKey returns the partition key an envelope must carry on a stream.
//
// It exists because canon.Envelope.PartitionKey answers the *price* path's
// question — it prefers "store:sku" whenever a payload carries a SKU, which is
// exactly right for `price-updates`, where two repricings of one product in one
// store must be strictly ordered. Interface contract §2 keys `device-events` by
// `device_id` and `label-telemetry` by `label_id` instead, and a device event
// carrying a SKU (an assignment does) would otherwise be keyed by the product
// it happens to face. The consequence would be real: two assignments moving one
// label between two SKUs would land on different partitions and could be
// applied out of order, leaving the Label Service's directory pointing that
// label at a product that is no longer in front of it.
//
// Both registry streams are therefore keyed by the aggregate — the device —
// which is what makes per-label ordering the guarantee downstream consumers
// actually rely on.
func StreamKey(stream string, env canon.Envelope) string {
	switch stream {
	case canon.StreamDeviceEvents.Name, canon.StreamTelemetry.Name:
		if env.AggregateID != "" {
			return env.AggregateID
		}
	}
	return env.PartitionKey()
}

// PublishEvents serialises each envelope and publishes the batch, keyed per
// [StreamKey].
func (p *BusPublisher) PublishEvents(ctx context.Context, stream string, envs ...canon.Envelope) error {
	if p == nil || p.bus == nil || len(envs) == 0 {
		return nil
	}
	msgs := make([]eventbus.Message, 0, len(envs))
	for _, env := range envs {
		if err := env.Validate(); err != nil {
			return fmt.Errorf("adapters: refusing to publish an invalid envelope: %w", err)
		}
		body, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("adapters: encode envelope %s: %w", env.EventID, err)
		}
		msgs = append(msgs, eventbus.Message{
			Topic: stream,
			Key:   StreamKey(stream, env),
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
		})
	}
	return p.bus.Publish(ctx, msgs...)
}

// Messenger adapts an msgbus.Client to the registry's DeviceMessenger port.
type Messenger struct {
	client msgbus.Client
}

// NewMessenger wraps an MQTT client.
func NewMessenger(c msgbus.Client) *Messenger { return &Messenger{client: c} }

// Publish sends one message to the broker.
func (m *Messenger) Publish(ctx context.Context, msg msgbus.Message) error {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client.Publish(ctx, msg)
}

// Subscribe registers a handler for a topic filter.
func (m *Messenger) Subscribe(ctx context.Context, filter string, qos msgbus.QoS, h msgbus.Handler) error {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client.Subscribe(ctx, filter, qos, h)
}

// NopMessenger discards everything published to it and accepts every
// subscription.
//
// It exists so that a registry can run — and be demonstrated — with no broker
// reachable. That is not a test-only situation: `make dev` brings services up
// in an order that does not guarantee a broker, and a registry that refused to
// start without one would make the HTTP surface hostage to the messaging tier.
type NopMessenger struct{}

// Publish discards the message.
func (NopMessenger) Publish(context.Context, msgbus.Message) error { return nil }

// Subscribe accepts the filter and delivers nothing.
func (NopMessenger) Subscribe(context.Context, string, msgbus.QoS, msgbus.Handler) error { return nil }

// RecordingMessenger keeps every published message in memory.
//
// It is the adapter the tests use to assert on the retained-message behaviour
// that interface contract §3 requires — in particular that a label reassigned
// between controllers produces a zero-length retained publish on the old zone
// topic. That property is invisible to any assertion made on the registry's own
// state, so it needs a messenger that remembers.
type RecordingMessenger struct {
	mu       sync.Mutex
	messages []msgbus.Message
	handlers map[string]msgbus.Handler
}

// NewRecordingMessenger returns an empty recorder.
func NewRecordingMessenger() *RecordingMessenger {
	return &RecordingMessenger{handlers: make(map[string]msgbus.Handler)}
}

// Publish records the message.
func (r *RecordingMessenger) Publish(_ context.Context, m msgbus.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m.ReceivedAt = time.Now().UTC()
	r.messages = append(r.messages, m)
	return nil
}

// Subscribe records the handler so a test can deliver to it.
func (r *RecordingMessenger) Subscribe(_ context.Context, filter string, _ msgbus.QoS, h msgbus.Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[filter] = h
	return nil
}

// Deliver hands a message to the handler registered for a filter, if any.
func (r *RecordingMessenger) Deliver(ctx context.Context, filter string, m msgbus.Message) bool {
	r.mu.Lock()
	h := r.handlers[filter]
	r.mu.Unlock()
	if h == nil {
		return false
	}
	h(ctx, m)
	return true
}

// Messages returns a copy of everything published so far.
func (r *RecordingMessenger) Messages() []msgbus.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]msgbus.Message(nil), r.messages...)
}

// Reset discards the recorded messages.
func (r *RecordingMessenger) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = nil
}

// HierarchyAuthenticator verifies device certificates against the platform's
// certificate hierarchy.
type HierarchyAuthenticator struct {
	h *pki.Hierarchy
}

// NewHierarchyAuthenticator wraps a hierarchy.
func NewHierarchyAuthenticator(h *pki.Hierarchy) *HierarchyAuthenticator {
	return &HierarchyAuthenticator{h: h}
}

// Authenticate verifies the chain and returns the identity it asserts.
//
// Revocation checking is left on. It is the one option here whose absence would
// be invisible in every test that passes and catastrophic in the one case that
// matters: a label whose key was extracted from a stolen unit presents a
// perfectly valid chain, and the CRL is the only thing that says otherwise.
//
// The extended key usage is pinned to client authentication because that is the
// only usage a device certificate is issued for. Leaving it unset would accept a
// certificate minted for something else in the hierarchy, and crypto/x509's own
// default of server authentication would reject every legitimate device.
func (a *HierarchyAuthenticator) Authenticate(chain []*x509.Certificate, now time.Time) (pki.Identity, *x509.Certificate, error) {
	if a == nil || a.h == nil {
		return pki.Identity{}, nil, fmt.Errorf("adapters: no certificate hierarchy configured")
	}
	if len(chain) == 0 {
		return pki.Identity{}, nil, fmt.Errorf("adapters: empty certificate chain")
	}
	leaf := chain[0]
	id, err := a.h.VerifyPeer(leaf, chain[1:], pki.VerifyOptions{
		At:        now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return pki.Identity{}, nil, err
	}
	return id, leaf, nil
}

// HierarchyIssuer issues device certificates from the platform hierarchy. It
// backs the development seeding endpoint only; see ports.DeviceIssuer for why
// issuance is a separate capability from verification.
type HierarchyIssuer struct {
	h *pki.Hierarchy
}

// NewHierarchyIssuer wraps a hierarchy.
func NewHierarchyIssuer(h *pki.Hierarchy) *HierarchyIssuer { return &HierarchyIssuer{h: h} }

// IssueDevice mints a certificate and discards the private key.
//
// Discarding the key is not laziness: a seeded device is a record in a registry,
// not a radio on a shelf, and nothing in the platform will ever ask it to prove
// possession. Returning a key would create the only copy of a credential that
// nobody needs, in a process that has no business holding one.
func (i *HierarchyIssuer) IssueDevice(id pki.Identity, now time.Time) (*pki.Issued, error) {
	if i == nil || i.h == nil {
		return nil, fmt.Errorf("adapters: no certificate hierarchy configured")
	}
	issued, _, err := i.h.IssueLeaf(id, pki.LeafOptions{Now: now})
	if err != nil {
		return nil, err
	}
	return issued, nil
}

// Assert that the adapters satisfy the ports they are written against. The
// checks are here rather than in the ports package so that a port stays
// importable by a component that has no adapter at all.
var (
	_ ports.EventStreamPublisher = (*BusPublisher)(nil)
	_ ports.DeviceMessenger      = (*Messenger)(nil)
	_ ports.DeviceMessenger      = NopMessenger{}
	_ ports.DeviceMessenger      = (*RecordingMessenger)(nil)
	_ ports.DeviceAuthenticator  = (*HierarchyAuthenticator)(nil)
	_ ports.DeviceIssuer         = (*HierarchyIssuer)(nil)
)

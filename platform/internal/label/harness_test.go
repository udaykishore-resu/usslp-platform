package label

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/label/app"
	"github.com/usslp/usslp/platform/internal/label/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/pki"
)

const (
	testTenant = canon.TenantID("acme")
	testStore  = canon.StoreID("store-01")
	testRegion = canon.Region("us-east-1")
	testSEC    = canon.SECID("sec-07")
)

// harness stands the whole service up against real infrastructure: a real
// durable event store, a real embedded event log with real consumer groups, a
// real in-process MQTT broker speaking the wire protocol, and a real Ed25519
// price authority.
//
// Fakes are deliberately absent. The properties this service has to hold — a
// retained QoS 1 publish on exactly the right topic, an attestation that
// verifies under the published key ring, an idempotent redelivery, an
// optimistic-concurrency retry — are properties of the interaction with that
// infrastructure, and a test against mocks would assert only that the mocks
// were called.
type harness struct {
	t         testing.TB
	kv        *kvstore.Store
	store     *eventstore.Store
	bus       *eventlog.Log
	broker    *mqtt.Broker
	client    *mqtt.Client
	observer  *mqtt.Client
	authority *pki.PriceAuthority
	svc       *Service
	clock     *testClock

	mu       sync.Mutex
	captured []msgbus.Message
	waiters  []chan struct{}
}

// testLogger writes at warn level to stderr so a failing test explains itself.
// Handler errors on a consumer are otherwise invisible: the bus retries and
// dead-letters them, and the test only sees a timeout.
func testLogger() *obs.Logger {
	if os.Getenv("USSLP_TEST_DEBUG") != "" {
		return obs.NewLogger(obs.LogConfig{Service: "label-test", Level: "debug", Format: "text", Output: os.Stderr})
	}
	return obs.NewLogger(obs.LogConfig{Service: "label-test", Level: "error", Format: "text", Output: os.Stderr})
}

// testClock is the service's business clock, offset from real time.
//
// Real time keeps flowing underneath it, so brokers, timers and the MQTT
// handshake behave normally, while Advance lets a test move the platform's view
// of "now" past a scheduled effective time without sleeping through it. Every
// temporal rule in the domain takes its instant from here, which is what makes
// a four-hour-ahead promotion testable in a millisecond.
type testClock struct {
	mu     sync.Mutex
	offset time.Duration
}

// Now implements ports.Clock.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().UTC().Add(c.offset)
}

// Advance moves the clock forward.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// testStreams narrows canon's catalogue to the partition counts a single-node
// test needs, keeping every other property — names, retention, compaction —
// exactly as the platform defines them.
func testStreams() []canon.Stream {
	const partitions = 4
	out := []canon.Stream{
		canon.StreamPriceUpdates, canon.StreamDelivery, canon.StreamDeviceEvents,
		canon.StreamPromotions, canon.StreamLabelState, canon.StreamAudit, canon.StreamDLQ,
	}
	for i := range out {
		out[i].Partitions = partitions
	}
	return out
}

func newHarness(t testing.TB) *harness {
	t.Helper()
	h := &harness{t: t}

	kv, err := kvstore.OpenWith(kvstore.Options{Sync: kvstore.SyncNever})
	if err != nil {
		t.Fatalf("open kvstore: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	h.kv = kv

	store, err := eventstore.New(kv)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h.store = store

	bus, err := eventlog.Open("", eventlog.WithSync(eventlog.SyncNever))
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	h.bus = bus

	broker := mqtt.NewBroker(mqtt.Options{Addr: "127.0.0.1:0"})
	addr, err := broker.Start()
	if err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = broker.Shutdown(ctx)
	})
	h.broker = broker

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "tcp://" + addr.String()

	client, err := mqtt.Dial(ctx, msgbus.Config{BrokerURL: url, ClientID: "label-service-test"})
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	h.client = client

	observer, err := mqtt.Dial(ctx, msgbus.Config{BrokerURL: url, ClientID: "observer-test"})
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	h.observer = observer
	if err := observer.Subscribe(ctx, canon.MQTTRoot+"/#", msgbus.AtLeastOnce, h.capture); err != nil {
		t.Fatalf("observer subscribe: %v", err)
	}

	authority, err := pki.NewPriceAuthority(pki.PriceAuthorityConfig{})
	if err != nil {
		t.Fatalf("price authority: %v", err)
	}
	h.authority = authority
	h.clock = &testClock{}

	svc, err := New(Config{
		Store: store, ReadModels: kv, Bus: bus, Broker: client,
		Attestor: authority, Currency: app.FixedCurrency("USD"), Clock: h.clock,
		// Each harness gets its own registry: obs.Registry panics on a
		// duplicate metric name, so a shared one would make the second test in
		// a package fail at construction.
		Registry: obs.NewRegistry(), Log: testLogger(),
		Standard: obs.NewStandardMetrics(obs.NewRegistry()),
		// Narrow streams. canon's 1,024 price partitions are sized for the
		// estate; a test process would spend its time scheduling one consumer
		// goroutine per partition instead of exercising the price path.
		Streams: testStreams(),
	})
	if err != nil {
		t.Fatalf("build service: %v", err)
	}
	h.svc = svc
	if err := svc.EnsureStreams(ctx); err != nil {
		t.Fatalf("ensure streams: %v", err)
	}
	return h
}

func (h *harness) capture(_ context.Context, m msgbus.Message) {
	h.mu.Lock()
	h.captured = append(h.captured, m)
	waiters := h.waiters
	h.waiters = nil
	h.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
}

// messages returns the captured publishes whose topic ends in the given leaf.
func (h *harness) messages(leaf string) []msgbus.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []msgbus.Message
	for _, m := range h.captured {
		if _, _, _, got, ok := canon.ParseSECLabelTopic(m.Topic); ok && got == leaf {
			out = append(out, m)
		}
	}
	return out
}

// waitForMessages blocks until at least n publishes with the given leaf have
// arrived, or the deadline passes. MQTT delivery is asynchronous, so a test
// that asserted immediately after a publish would be asserting on the network.
func (h *harness) waitForMessages(leaf string, n int, within time.Duration) []msgbus.Message {
	h.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got := h.messages(leaf); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %d %q messages; got %d", n, leaf, len(h.messages(leaf)))
		}
		ch := make(chan struct{})
		h.mu.Lock()
		h.waiters = append(h.waiters, ch)
		h.mu.Unlock()
		select {
		case <-ch:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (h *harness) resetCaptured() {
	h.mu.Lock()
	h.captured = nil
	h.mu.Unlock()
}

// provisionLabel drives a label into the write side and the directory the way
// production does: through `device-events`, not through a back door. A test
// that seeded the aggregate directly would not exercise the projection that
// every price change depends on.
func (h *harness) provisionLabel(id canon.LabelID, sec canon.SECID, sku canon.SKU) {
	h.t.Helper()
	ctx := context.Background()
	for _, env := range h.deviceEvents(id, sec, sku) {
		if err := h.svc.DirectoryProjection().HandleEnvelope(ctx, env); err != nil {
			h.t.Fatalf("device event %s for %s: %v", env.EventType, id, err)
		}
	}
}

// deviceEvents builds the provisioning and assignment envelopes for a label.
func (h *harness) deviceEvents(id canon.LabelID, sec canon.SECID, sku canon.SKU) []canon.Envelope {
	h.t.Helper()
	now := time.Now().UTC().Add(-time.Hour)
	provisioned, err := canon.NewEnvelope(canon.EvtLabelProvisioned, "device", string(id), testTenant,
		canon.DeviceProvisioned{
			LabelID: id, Serial: "USSLP-" + string(id), StoreID: testStore, SECID: sec,
			HardwareTier: "gen3", FirmwareVer: "1.4.2", ProvisionedAt: now,
		})
	if err != nil {
		h.t.Fatalf("provisioned envelope: %v", err)
	}
	provisioned.StoreID = testStore
	provisioned.Region = testRegion
	provisioned.OccurredAt = now
	provisioned.Source = "device-registry"

	assigned, err := canon.NewEnvelope(canon.EvtLabelAssigned, "device", string(id), testTenant,
		app.LabelAssignment{LabelID: id, SKU: sku, SECID: sec, StoreID: testStore})
	if err != nil {
		h.t.Fatalf("assigned envelope: %v", err)
	}
	assigned.StoreID = testStore
	assigned.Region = testRegion
	assigned.OccurredAt = now.Add(time.Minute)
	assigned.Source = "device-registry"
	return []canon.Envelope{provisioned, assigned}
}

// priceEnvelope builds a `pricing.change.requested` envelope as the Universal
// Integration Gateway would publish it.
func (h *harness) priceEnvelope(sku canon.SKU, amount int64, idem string) canon.Envelope {
	h.t.Helper()
	now := time.Now().UTC()
	env, err := canon.NewEnvelope(canon.EvtPriceChangeRequested, "pricing",
		string(testStore)+":"+string(sku), testTenant, canon.PriceChangeRequested{
			SKU: sku, StoreID: testStore, Price: canon.NewMoney(amount, "USD"),
			EffectiveAt: now, InitiatedBy: "pos/lane-3", SourceSystem: "ncr",
		})
	if err != nil {
		h.t.Fatalf("price envelope: %v", err)
	}
	env.StoreID = testStore
	env.Region = testRegion
	env.Source = "uig/ncr"
	env.TraceID = canon.NewTraceID()
	env.SpanID = canon.NewSpanID()
	env.CorrelationID = canon.NewCorrelationID()
	env.IdempotencyKey = idem
	return env
}

// decodeUpdate unpacks a captured MQTT payload into its envelope and price.
func (h *harness) decodeUpdate(m msgbus.Message) (canon.Envelope, canon.PriceUpdated) {
	h.t.Helper()
	var env canon.Envelope
	if err := json.Unmarshal(m.Payload, &env); err != nil {
		h.t.Fatalf("decoding MQTT payload: %v", err)
	}
	var update canon.PriceUpdated
	if err := env.Decode(&update); err != nil {
		h.t.Fatalf("decoding price update: %v", err)
	}
	return env, update
}

// verifyAttestation checks a captured update the way a Shelf Edge Controller
// does: by recomputing the digest from the message it is holding and verifying
// it against the published key ring, never by trusting the transmitted digest.
func (h *harness) verifyAttestation(update canon.PriceUpdated) {
	h.t.Helper()
	ring, err := h.authority.KeyRing()
	if err != nil {
		h.t.Fatalf("key ring: %v", err)
	}
	input := canon.AttestationInputFrom(testTenant, update)
	if err := ring.Verify(input, update.Attestation); err != nil {
		h.t.Fatalf("attestation does not verify: %v", err)
	}
}

// ackEnvelope builds the delivery acknowledgement a controller republishes.
func ackEnvelope(t testing.TB, id canon.LabelID, sec canon.SECID, sequence int64, latency time.Duration) canon.Envelope {
	t.Helper()
	now := time.Now().UTC()
	env, err := canon.NewEnvelope(canon.EvtLabelDelivered, domain.AggregateType, string(id), testTenant,
		canon.LabelDelivered{
			LabelID: id, StoreID: testStore, SECID: sec, Sequence: sequence,
			DeliveredAt: now, LatencyMS: latency.Milliseconds(),
			MeshHops: 2, RefreshMS: 300, Partial: true,
		})
	if err != nil {
		t.Fatalf("ack envelope: %v", err)
	}
	env.StoreID = testStore
	env.Region = testRegion
	env.Source = "sec/" + string(sec)
	return env
}

func labelID(i int) canon.LabelID { return canon.LabelID(fmt.Sprintf("lbl-%05d", i)) }

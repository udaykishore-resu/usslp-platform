package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/mqtt"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// priceTap is an MQTT client on a store's own broker, watching every price
// update on its way to a controller.
//
// It is the vantage point the security argument in INTERFACE-CONTRACTS §5 is
// written about: an attacker with write access to the store's broker. Reading
// from here proves what actually crossed the last hop; writing to it is how the
// tamper test attacks the platform at exactly the point the contract claims is
// defended.
type priceTap struct {
	client msgbus.Client
	scope  canon.TopicScope

	mu     sync.Mutex
	latest map[canon.LabelID]tappedUpdate
	seen   []tappedUpdate
}

type tappedUpdate struct {
	Envelope canon.Envelope
	Update   canon.PriceUpdated
	Topic    string
	Raw      []byte
	At       time.Time
}

// taps are cached per store: one subscription serves every test in the package,
// and a second client with the same identity would take the first one's session
// off the broker.
// Keyed by the runtime *and* the store, not by either alone.
//
// Two tests each boot their own runtime and both call their store
// "demo-retail-store-01", so keying by identifier alone hands the second test a
// client attached to the first test's broker. Keying by broker address is no
// better: the first runtime's ephemeral ports are released when it stops and
// the operating system happily gives the same one to the next runtime, at which
// point the cache looks like a hit and returns a closed client holding the
// previous store's messages. That failure is intermittent, and it presents as
// "an unsigned price moved the shelf", which is the most alarming possible way
// for a test harness bug to announce itself.
var (
	tapMu sync.Mutex
	taps  = map[string]*priceTap{}
)

func tapKey(st *stack.Stack, store *stack.Store) string {
	return fmt.Sprintf("%p/%s", st, store.ID)
}

func priceTapFor(t *testing.T, st *stack.Stack, store *stack.Store) *priceTap {
	t.Helper()
	tapMu.Lock()
	defer tapMu.Unlock()
	key := tapKey(st, store)
	if tp, ok := taps[key]; ok {
		return tp
	}
	tp := &priceTap{scope: store.Scope, latest: map[canon.LabelID]tappedUpdate{}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mqtt.Dial(ctx, msgbus.Config{
		BrokerURL: "tcp://" + store.BrokerAddr, ClientID: "e2e-tap-" + string(store.ID),
		CleanSession: true, KeepAlive: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("connecting a tap to %s's broker: %v", store.ID, err)
	}
	tp.client = client

	// The wildcard covers every controller in the store, which is what the
	// bridge publishes onto and what each controller subscribes to a slice of.
	filter := store.Scope.SubscribeZoneLabels("+", canon.LeafPrice)
	if err := client.Subscribe(context.Background(), filter, canon.QoSPrice,
		func(_ context.Context, m msgbus.Message) {
			var env canon.Envelope
			if err := json.Unmarshal(m.Payload, &env); err != nil {
				return
			}
			if env.EventType != canon.EvtPriceUpdated {
				return
			}
			var upd canon.PriceUpdated
			if err := env.Decode(&upd); err != nil {
				return
			}
			u := tappedUpdate{Envelope: env, Update: upd, Topic: m.Topic,
				Raw: append([]byte(nil), m.Payload...), At: time.Now()}
			tp.mu.Lock()
			tp.latest[upd.LabelID] = u
			if len(tp.seen) < 4096 {
				tp.seen = append(tp.seen, u)
			}
			tp.mu.Unlock()
		}); err != nil {
		t.Fatalf("subscribing the tap to %s: %v", filter, err)
	}
	taps[key] = tp
	return tp
}

func (tp *priceTap) latestFor(id canon.LabelID) (tappedUpdate, bool) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	u, ok := tp.latest[id]
	return u, ok
}

// last returns the most recent update seen for a label.
func (tp *priceTap) last(id canon.LabelID) (canon.Envelope, canon.PriceUpdated, bool) {
	u, ok := tp.latestFor(id)
	return u.Envelope, u.Update, ok
}

// await waits for an update for a label at a given sequence to cross the wire.
func (tp *priceTap) await(id canon.LabelID, seq int64, within time.Duration) (tappedUpdate, bool) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if u, ok := tp.latestFor(id); ok && (seq == 0 || u.Update.Sequence >= seq) {
			return u, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return tappedUpdate{}, false
}

// publish injects a message onto the store's broker, which is what an attacker
// with write access to it would do.
func (tp *priceTap) publish(t *testing.T, topic string, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tp.client.Publish(ctx, msgbus.Message{
		Topic: topic, Payload: payload, QoS: canon.QoSPrice,
	}); err != nil {
		t.Fatalf("publishing to %s: %v", topic, err)
	}
}

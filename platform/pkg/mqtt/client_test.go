package mqtt

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
	"github.com/usslp/usslp/platform/pkg/retry"
)

// fastBackoff keeps the reconnect tests quick without removing the backoff
// behaviour they are meant to exercise.
var fastBackoff = retry.Policy{Base: 20 * time.Millisecond, Max: 100 * time.Millisecond, Multiplier: 2, Jitter: true}

func TestParseBrokerURL(t *testing.T) {
	cases := []struct {
		raw     string
		addr    string
		useTLS  bool
		wantErr bool
	}{
		{raw: "tcp://127.0.0.1:1883", addr: "127.0.0.1:1883"},
		{raw: "mqtt://sgu.store-7.local:1883", addr: "sgu.store-7.local:1883"},
		{raw: "tcp://sgu.local", addr: "sgu.local:1883"},
		{raw: "tls://broker.usslp.io:8883", addr: "broker.usslp.io:8883", useTLS: true},
		{raw: "mqtts://broker.usslp.io", addr: "broker.usslp.io:8883", useTLS: true},
		{raw: "ssl://broker.usslp.io:9999", addr: "broker.usslp.io:9999", useTLS: true},
		{raw: "ws://broker.usslp.io", wantErr: true},
		{raw: "tcp://", wantErr: true},
		{raw: "::not a url", wantErr: true},
	}
	for _, tc := range cases {
		addr, useTLS, err := parseBrokerURL(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseBrokerURL(%q) accepted an unusable URL", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBrokerURL(%q): %v", tc.raw, err)
			continue
		}
		if addr != tc.addr || useTLS != tc.useTLS {
			t.Errorf("parseBrokerURL(%q) = (%q, %v), want (%q, %v)", tc.raw, addr, useTLS, tc.addr, tc.useTLS)
		}
	}
}

// TestClientReconnectsAfterBrokerRestart is the WAN-cut rehearsal: the broker
// goes away entirely, the client keeps trying, and when it comes back the
// subscription is restored without the application doing anything.
func TestClientReconnectsAfterBrokerRestart(t *testing.T) {
	first, addr := startBroker(t, Options{})

	cfg := testConfig(addr, "sgu-17")
	cfg.CleanSession = false
	sub := dialClient(t, cfg, WithBackoff(fastBackoff))

	got := newCollector()
	ctx := context.Background()
	if err := sub.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	if !sub.Connected() {
		t.Fatal("Connected() is false on a working link")
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	if err := first.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutting the broker down: %v", err)
	}
	waitFor(t, "Connected() to report the link is down", func() bool { return !sub.Connected() })

	// Publishing while the store is cut off is what makes the SGU switch to
	// autonomous mode, so it must fail fast rather than block.
	if err := sub.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("1"),
		QoS: msgbus.AtLeastOnce}); err != msgbus.ErrNotConnected {
		t.Errorf("publish while disconnected returned %v, want msgbus.ErrNotConnected", err)
	}

	second := startBrokerAt(t, addr, Options{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		second.Shutdown(ctx)
	})
	waitFor(t, "the client to reconnect", func() bool { return sub.Connected() })

	// The subscription came back with the connection.
	pub := dialClient(t, testConfig(addr, "gateway-1"))
	waitFor(t, "the restored subscription to deliver", func() bool {
		if err := pub.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("399"),
			QoS: msgbus.AtLeastOnce}); err != nil {
			return false
		}
		select {
		case <-got.ch:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	})
}

// TestClientResendsInFlightAfterReconnect is the other half of a WAN cut: a
// publish that was in flight when the link died completes after it returns,
// without the caller learning that anything happened.
func TestClientResendsInFlightAfterReconnect(t *testing.T) {
	first, addr := startBroker(t, Options{})

	cfg := testConfig(addr, "sgu-17")
	cfg.CleanSession = false
	cfg.AckTimeout = 4 * time.Second
	pub := dialClient(t, cfg, WithBackoff(fastBackoff))

	// A subscriber that survives the restart by resuming its session, so the
	// re-sent message has somewhere to land.
	sec := dialRaw(t, addr)
	sec.connect(&connectPacket{ClientID: "sec-1", CleanSession: false})
	sec.subscribe(1, zoneFilter, msgbus.AtLeastOnce)

	// Stop the broker so nothing can acknowledge, then publish.
	ctx := context.Background()
	shutdownCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	if err := first.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutting the broker down: %v", err)
	}
	waitFor(t, "the link to drop", func() bool { return !pub.Connected() })

	// Force a message into the in-flight set while the link is down, exactly as
	// a publish interrupted mid-handshake would leave one.
	pub.mu.Lock()
	id, ok := pub.allocIDLocked()
	if !ok {
		pub.mu.Unlock()
		t.Fatal("no free packet identifier")
	}
	f := &clientInflight{id: id, state: awaitingPuback, done: make(chan struct{}),
		msg: msgbus.Message{Topic: priceTopic, Payload: []byte("399"), QoS: msgbus.AtLeastOnce}}
	pub.inflight[id] = f
	pub.order = append(pub.order, id)
	pub.mu.Unlock()

	second := startBrokerAt(t, addr, Options{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		second.Shutdown(ctx)
	})
	waitFor(t, "the client to reconnect", func() bool { return pub.Connected() })

	select {
	case <-f.done:
	case <-time.After(testTimeout):
		t.Fatal("the in-flight message was never acknowledged after the reconnect")
	}
	pub.mu.Lock()
	_, still := pub.inflight[id]
	pub.mu.Unlock()
	if still {
		t.Error("the acknowledged message was left in the in-flight set")
	}
}

func TestClosedClientRejectsOperations(t *testing.T) {
	_, addr := startBroker(t, Options{})
	ctx := context.Background()
	c, err := Dial(ctx, testConfig(addr, "sec-1"))
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if c.Connected() {
		t.Error("Connected() is true after Close")
	}
	if err := c.Publish(ctx, msgbus.Message{Topic: priceTopic}); err != msgbus.ErrClosed {
		t.Errorf("Publish after Close returned %v, want msgbus.ErrClosed", err)
	}
	if err := c.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, func(context.Context, msgbus.Message) {}); err != msgbus.ErrClosed {
		t.Errorf("Subscribe after Close returned %v, want msgbus.ErrClosed", err)
	}
	if err := c.Unsubscribe(ctx, zoneFilter); err != msgbus.ErrClosed {
		t.Errorf("Unsubscribe after Close returned %v, want msgbus.ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close returned %v, want nil", err)
	}
}

func TestClientRejectsInvalidTopics(t *testing.T) {
	_, addr := startBroker(t, Options{})
	c := dialClient(t, testConfig(addr, "sec-1"))
	ctx := context.Background()

	if err := c.Publish(ctx, msgbus.Message{Topic: "usslp/acme/+/price"}); !errors.Is(err, ErrMalformedPacket) {
		t.Errorf("publishing to a wildcard returned %v, want ErrMalformedPacket", err)
	}
	if err := c.Publish(ctx, msgbus.Message{Topic: priceTopic, QoS: 3}); !errors.Is(err, ErrProtocolViolation) {
		t.Errorf("publishing at QoS 3 returned %v, want ErrProtocolViolation", err)
	}
	if err := c.Subscribe(ctx, "a/#/b", msgbus.AtLeastOnce, func(context.Context, msgbus.Message) {}); !errors.Is(err, ErrMalformedPacket) {
		t.Errorf("subscribing to a misplaced '#' returned %v, want ErrMalformedPacket", err)
	}
	if err := c.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, nil); err == nil {
		t.Error("Subscribe accepted a nil handler")
	}
}

// TestHandlerPoolAppliesBackpressure proves the dispatch pool is bounded: with
// one worker blocked, the queue fills and the reader stops, rather than the
// process growing a goroutine per undelivered message.
func TestHandlerPoolAppliesBackpressure(t *testing.T) {
	_, addr := startBroker(t, Options{})
	sub := dialClient(t, testConfig(addr, "sec-1"), WithHandlerPool(1, 1))
	pub := dialClient(t, testConfig(addr, "gateway-1"))
	ctx := context.Background()

	release := make(chan struct{})
	var started, finished atomic.Int64
	h := func(context.Context, msgbus.Message) {
		started.Add(1)
		<-release
		finished.Add(1)
	}
	if err := sub.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, h); err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	const sent = 12
	var wg sync.WaitGroup
	for i := 0; i < sent; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			pub.Publish(ctx, msgbus.Message{Topic: priceTopic,
				Payload: []byte(strings.Repeat("x", n+1)), QoS: msgbus.AtMostOnce})
		}(i)
	}
	wg.Wait()

	// One handler is running and at most one more is queued; the rest are held
	// in the broker and in the socket, not in this process's scheduler.
	time.Sleep(200 * time.Millisecond)
	if n := started.Load(); n != 1 {
		t.Fatalf("%d handlers were started with a single blocked worker, want 1", n)
	}

	close(release)
	waitFor(t, "the blocked handlers to drain", func() bool { return finished.Load() >= 2 })
}

func TestClientMetrics(t *testing.T) {
	_, addr := startBroker(t, Options{})
	reg := obs.NewRegistry("service", "sec")
	c := dialClient(t, testConfig(addr, "sec-1"), WithClientRegistry(reg))
	ctx := context.Background()

	got := newCollector()
	if err := c.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	if err := c.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("399"),
		QoS: msgbus.AtLeastOnce}); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	got.next(t)

	if v := c.met.connected.Value(); v != 1 {
		t.Errorf("connected gauge is %v, want 1", v)
	}
	if v := c.met.published.With("1").Value(); v != 1 {
		t.Errorf("published counter is %d, want 1", v)
	}
	if v := c.met.received.With("1").Value(); v != 1 {
		t.Errorf("received counter is %d, want 1", v)
	}
	if v := c.met.inflight.Value(); v != 0 {
		t.Errorf("in-flight gauge is %v after the handshake completed, want 0", v)
	}

	var sb strings.Builder
	reg.WriteText(&sb)
	if !strings.Contains(sb.String(), metricClientPublished) {
		t.Error("the registry did not render the client's metrics")
	}
}

// TestClientImplementsMsgbusClient is a compile-time check that the port is
// actually satisfied; the whole package exists to be substitutable for EMQX
// behind this interface.
func TestClientImplementsMsgbusClient(t *testing.T) {
	var _ msgbus.Client = (*Client)(nil)
}

func TestDialRejectsUnreachableBroker(t *testing.T) {
	cfg := testConfig("127.0.0.1:1", "sec-1")
	cfg.ConnectTimeout = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if _, err := Dial(ctx, cfg); err == nil {
		t.Fatal("Dial reported success against a closed port")
	}
}

func TestDialRejectsWrongTLSConfigType(t *testing.T) {
	cfg := testConfig("127.0.0.1:1", "sec-1")
	cfg.TLSConfig = "not a tls config"
	if _, err := Dial(context.Background(), cfg); err == nil {
		t.Fatal("Dial accepted a TLSConfig of the wrong type")
	}
}

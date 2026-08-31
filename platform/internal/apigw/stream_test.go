package apigw

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// streamHarness builds a gateway whose live feed is driven by the test.
func streamHarness(t *testing.T, queue int) (*harness, *channelSource) {
	t.Helper()
	source := newChannelSource()
	h := newHarness(t, func(o *harnessOptions) {
		o.source = source
		o.stream = StreamConfig{
			QueueDepth: queue,
			// Long enough that no test trips the keepalive, short enough that
			// a wedged test fails rather than hangs.
			PingInterval: 30 * time.Second,
			PongTimeout:  30 * time.Second,
			CloseGrace:   time.Second,
		}
	})
	return h, source
}

// connectStream opens an authenticated stream and consumes the ready message.
func connectStream(t *testing.T, h *harness, credential, query string, protocols []string) *wsClient {
	t.Helper()
	client, res, err := dialWebSocket(t, h.server.URL, "/v1/stream"+query, protocols, nil)
	if err != nil {
		t.Fatalf("dialling the stream: %v", err)
	}
	if client == nil {
		t.Fatalf("handshake refused with %d", res.StatusCode)
	}
	var ready streamControl
	if err := client.readJSON(t, &ready); err != nil {
		t.Fatalf("reading the ready message: %v", err)
	}
	if ready.Type != "ready" {
		t.Fatalf("first message is %q, want ready", ready.Type)
	}
	return client
}

func credentialProtocols(credential string) []string {
	return []string{wsProtocol, wsCredentialProtocol + credential}
}

// waitForSubscribers blocks until the hub has n subscribers, so a test does
// not publish into a hub the connection has not joined yet.
func waitForSubscribers(t *testing.T, h *harness, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.gw.Hub().Len() == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("hub has %d subscribers, want %d", h.gw.Hub().Len(), n)
}

func TestStreamRequiresACredential(t *testing.T) {
	t.Parallel()
	h, _ := streamHarness(t, 16)
	client, res, err := dialWebSocket(t, h.server.URL, "/v1/stream", []string{wsProtocol}, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if client != nil {
		t.Fatal("an unauthenticated handshake was upgraded")
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.StatusCode)
	}
}

func TestStreamAcceptsACredentialInTheSubprotocol(t *testing.T) {
	t.Parallel()
	h, _ := streamHarness(t, 16)
	key := h.issueKey("acme", []Role{RoleReadOnly})

	client, res, err := dialWebSocket(t, h.server.URL, "/v1/stream", credentialProtocols(key), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if client == nil {
		t.Fatalf("handshake refused with %d", res.StatusCode)
	}
	// The gateway agrees to usslp.v1 and must never echo the credential back:
	// a response header is far more likely to be logged than a request one.
	if client.Subprotocol != wsProtocol {
		t.Fatalf("selected subprotocol %q, want %q", client.Subprotocol, wsProtocol)
	}
	if got := res.Header.Get("Sec-WebSocket-Protocol"); got != wsProtocol {
		t.Fatalf("Sec-WebSocket-Protocol = %q; the credential must not be echoed", got)
	}
}

func TestStreamDeliversTenantEventsOnly(t *testing.T) {
	t.Parallel()
	h, source := streamHarness(t, 32)
	acme := h.issueKey("acme", []Role{RoleReadOnly})
	beta := h.issueKey("beta", []Role{RoleReadOnly})

	acmeClient := connectStream(t, h, acme, "", credentialProtocols(acme))
	betaClient := connectStream(t, h, beta, "", credentialProtocols(beta))
	waitForSubscribers(t, h, 2)

	// One event per tenant, interleaved, so a subscriber that leaked would
	// see the other's before its own.
	source.emit(envelopeFor("beta", "store-b", canon.EvtPriceUpdated,
		map[string]any{"sku": "beta-secret-sku"}))
	source.emit(envelopeFor("acme", "store-a", canon.EvtPriceUpdated,
		map[string]any{"sku": "acme-sku"}))

	var got StreamEvent
	if err := acmeClient.readJSON(t, &got); err != nil {
		t.Fatalf("acme read: %v", err)
	}
	if got.StoreID != "store-a" {
		t.Fatalf("acme received an event for store %q; it must only ever see its own tenant",
			got.StoreID)
	}
	if string(got.Payload) == "" {
		t.Fatal("the event payload was dropped")
	}

	if err := betaClient.readJSON(t, &got); err != nil {
		t.Fatalf("beta read: %v", err)
	}
	if got.StoreID != "store-b" {
		t.Fatalf("beta received store %q", got.StoreID)
	}
}

func TestStreamFiltersByStoreAndType(t *testing.T) {
	t.Parallel()
	h, source := streamHarness(t, 32)
	key := h.issueKey("acme", []Role{RoleReadOnly})

	client := connectStream(t, h, key,
		"?stores=store-1&types="+canon.EvtPriceUpdated, credentialProtocols(key))
	waitForSubscribers(t, h, 1)

	// Three events that must not arrive, then one that must.
	source.emit(envelopeFor("acme", "store-2", canon.EvtPriceUpdated, map[string]any{"n": 1}))
	source.emit(envelopeFor("acme", "store-1", canon.EvtDeviceOffline, map[string]any{"n": 2}))
	source.emit(envelopeFor("acme", "store-2", canon.EvtDeviceOffline, map[string]any{"n": 3}))
	source.emit(envelopeFor("acme", "store-1", canon.EvtPriceUpdated, map[string]any{"n": 4}))

	var got StreamEvent
	if err := client.readJSON(t, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.StoreID != "store-1" || got.EventType != canon.EvtPriceUpdated {
		t.Fatalf("received %s for %s; the filter let something through", got.EventType, got.StoreID)
	}
	if string(got.Payload) != `{"n":4}` {
		t.Fatalf("payload %s, want the fourth event", got.Payload)
	}
}

func TestStreamCannotWidenPastTheCredentialsStoreScope(t *testing.T) {
	t.Parallel()
	h, source := streamHarness(t, 32)
	scoped := h.issueKey("acme", []Role{RoleStoreManager}, "store-1")

	// Asking for a store outside the scope is refused at the handshake, with
	// a 404 rather than a 403 so the subscription cannot enumerate stores.
	_, res, err := dialWebSocket(t, h.server.URL, "/v1/stream?stores=store-9",
		credentialProtocols(scoped), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", res.StatusCode)
	}

	// And a subscription message sent after the upgrade cannot widen either.
	client := connectStream(t, h, scoped, "", credentialProtocols(scoped))
	waitForSubscribers(t, h, 1)
	if err := client.conn.WriteMessage(OpText,
		[]byte(`{"type":"subscribe","stores":["store-1","store-9"]}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	var ack streamControl
	if err := client.readJSON(t, &ack); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ack.Type != "subscribed" {
		t.Fatalf("control message %q, want subscribed", ack.Type)
	}
	for _, s := range ack.Stores {
		if s == "store-9" {
			t.Fatal("the gateway accepted a store outside the credential's scope")
		}
	}

	source.emit(envelopeFor("acme", "store-9", canon.EvtPriceUpdated, map[string]any{"n": 1}))
	source.emit(envelopeFor("acme", "store-1", canon.EvtPriceUpdated, map[string]any{"n": 2}))

	var got StreamEvent
	if err := client.readJSON(t, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.StoreID != "store-1" {
		t.Fatalf("received an event for %q, which is outside the credential's scope", got.StoreID)
	}
}

func TestStreamSubscriptionCanBeNarrowedInPlace(t *testing.T) {
	t.Parallel()
	h, source := streamHarness(t, 32)
	key := h.issueKey("acme", []Role{RoleReadOnly})
	client := connectStream(t, h, key, "", credentialProtocols(key))
	waitForSubscribers(t, h, 1)

	// Narrowing without reconnecting is what lets the console's store
	// selector change stores without dropping events in the gap.
	if err := client.conn.WriteMessage(OpText,
		[]byte(`{"type":"subscribe","stores":["store-7"]}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	var ack streamControl
	if err := client.readJSON(t, &ack); err != nil {
		t.Fatalf("read: %v", err)
	}
	if ack.Type != "subscribed" || len(ack.Stores) != 1 || ack.Stores[0] != "store-7" {
		t.Fatalf("ack %+v, want a subscription to store-7", ack)
	}

	source.emit(envelopeFor("acme", "store-1", canon.EvtPriceUpdated, map[string]any{"n": 1}))
	source.emit(envelopeFor("acme", "store-7", canon.EvtPriceUpdated, map[string]any{"n": 2}))

	var got StreamEvent
	if err := client.readJSON(t, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.StoreID != "store-7" {
		t.Fatalf("store %q, want store-7", got.StoreID)
	}
}

func TestStreamRejectsAMalformedCommand(t *testing.T) {
	t.Parallel()
	h, _ := streamHarness(t, 32)
	key := h.issueKey("acme", []Role{RoleReadOnly})
	client := connectStream(t, h, key, "", credentialProtocols(key))
	waitForSubscribers(t, h, 1)

	if err := client.conn.WriteMessage(OpText, []byte(`{"type":"take-over"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	var msg streamControl
	if err := client.readJSON(t, &msg); err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Type != "error" {
		t.Fatalf("control message %q, want error", msg.Type)
	}
	if msg.Message == "" {
		t.Fatal("the error message says nothing about what was expected")
	}
}

// TestSlowConsumerIsEvicted is the property that keeps one stalled browser tab
// from becoming an outage.
func TestSlowConsumerIsEvicted(t *testing.T) {
	t.Parallel()
	h, source := streamHarness(t, 4)
	key := h.issueKey("acme", []Role{RoleReadOnly})
	client := connectStream(t, h, key, "", credentialProtocols(key))
	waitForSubscribers(t, h, 1)

	// Stop reading, in the way a laptop that went to sleep stops reading, and
	// push far more than the queue can hold.
	for i := 0; i < 500; i++ {
		source.emit(envelopeFor("acme", "store-1", canon.EvtPriceUpdated, map[string]any{"n": i}))
	}

	// Drain until the connection is closed. The queued events arrive first;
	// the close frame is what the test is looking for.
	deadline := time.Now().Add(5 * time.Second)
	var closeErr *CloseError
	for time.Now().Before(deadline) {
		_, _, err := client.conn.ReadMessage()
		if err == nil {
			continue
		}
		if IsCloseError(err, CloseTryAgainLater) {
			closeErr = &CloseError{Code: CloseTryAgainLater}
			break
		}
		t.Fatalf("connection ended with %v, want a 1013 close", err)
	}
	if closeErr == nil {
		t.Fatal("a consumer that never kept up was not evicted; the send queue is unbounded")
	}

	// The eviction is accounted for, and the subscriber is gone from the hub
	// rather than leaking.
	if got := h.gw.Metrics().StreamEvictions.With("slow_consumer").Value(); got == 0 {
		t.Error("the eviction was not counted")
	}
	if got := h.gw.Metrics().StreamEvents.With("dropped").Value(); got == 0 {
		t.Error("the dropped events were not counted")
	}
	waitForSubscribers(t, h, 0)
}

func TestStreamClosesCleanlyOnShutdown(t *testing.T) {
	t.Parallel()
	h, _ := streamHarness(t, 16)
	key := h.issueKey("acme", []Role{RoleReadOnly})
	client := connectStream(t, h, key, "", credentialProtocols(key))
	waitForSubscribers(t, h, 1)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.gw.Shutdown(ctx)
	}()

	// A draining gateway sends 1001 Going Away, not a TCP reset: the
	// difference is whether a client reconnects or reports an error.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, _, err := client.conn.ReadMessage()
		if err == nil {
			continue
		}
		if IsCloseError(err, CloseGoingAway) {
			return
		}
		t.Fatalf("connection ended with %v, want a 1001 going-away close", err)
	}
	t.Fatal("the stream was never closed by the draining gateway")
}

func TestHubFanOutIsTenantFilteredWithoutAConnection(t *testing.T) {
	t.Parallel()
	// A unit-level check of the fan-out predicate itself, independent of the
	// WebSocket layer, so a filtering regression is diagnosable without
	// reading frames.
	sub := &subscriber{
		tenant:  "acme",
		allowed: map[canon.StoreID]bool{"store-1": true},
		stores:  nil,
		types:   map[string]bool{canon.EvtPriceUpdated: true},
	}
	cases := []struct {
		tenant canon.TenantID
		store  canon.StoreID
		typ    string
		want   bool
	}{
		{"acme", "store-1", canon.EvtPriceUpdated, true},
		{"beta", "store-1", canon.EvtPriceUpdated, false},
		{"acme", "store-2", canon.EvtPriceUpdated, false},
		{"acme", "store-1", canon.EvtDeviceOffline, false},
		{"", "store-1", canon.EvtPriceUpdated, false},
	}
	for _, tc := range cases {
		if got := sub.matches(tc.tenant, tc.store, tc.typ); got != tc.want {
			t.Errorf("matches(%q,%q,%q) = %v, want %v", tc.tenant, tc.store, tc.typ, got, tc.want)
		}
	}
}

func TestStreamTopicsExcludeTelemetry(t *testing.T) {
	t.Parallel()
	for _, topic := range StreamTopics {
		if topic == canon.StreamTelemetry.Name {
			t.Fatal("label-telemetry is in the console fan-out; at ~167,000 events per second " +
				"it would drown every browser on the platform")
		}
	}
	if len(StreamTopics) == 0 {
		t.Fatal("the stream forwards nothing")
	}
}

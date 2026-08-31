package mqtt

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

const (
	priceTopic = "usslp/acme/eu-west-1/store-7/labels/L-0001/price"
	zoneFilter = "usslp/acme/eu-west-1/store-7/labels/+/price"
)

func TestConnectPublishSubscribeRoundTrip(t *testing.T) {
	_, addr := startBroker(t, Options{})
	sub := dialClient(t, testConfig(addr, "sec-1"))
	pub := dialClient(t, testConfig(addr, "gateway-1"))

	got := newCollector()
	ctx := context.Background()
	if err := sub.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	if err := pub.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("399"), QoS: msgbus.AtLeastOnce}); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	m := got.next(t)
	if m.Topic != priceTopic {
		t.Errorf("delivered topic %q, want %q", m.Topic, priceTopic)
	}
	if string(m.Payload) != "399" {
		t.Errorf("delivered payload %q, want 399", m.Payload)
	}
	if m.QoS != msgbus.AtLeastOnce {
		t.Errorf("delivered at QoS %d, want 1", m.QoS)
	}
	if m.Retain {
		t.Error("a live delivery must not carry the retain flag")
	}
	if !sub.Connected() || !pub.Connected() {
		t.Error("Connected() reported false on a working link")
	}
}

func TestSubscriptionQoSIsDowngradedToTheGrant(t *testing.T) {
	_, addr := startBroker(t, Options{})
	sub := dialClient(t, testConfig(addr, "sec-1"))
	pub := dialClient(t, testConfig(addr, "gateway-1"))

	got := newCollector()
	ctx := context.Background()
	if err := sub.Subscribe(ctx, zoneFilter, msgbus.AtMostOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	if err := pub.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("399"), QoS: msgbus.ExactlyOnce}); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if m := got.next(t); m.QoS != msgbus.AtMostOnce {
		t.Errorf("a QoS 2 publication reached a QoS 0 subscriber at QoS %d, want 0", m.QoS)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	_, addr := startBroker(t, Options{})
	sub := dialClient(t, testConfig(addr, "sec-1"))
	pub := dialClient(t, testConfig(addr, "gateway-1"))
	ctx := context.Background()

	got := newCollector()
	if err := sub.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	if err := pub.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("1"), QoS: msgbus.AtLeastOnce}); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	got.next(t)

	if err := sub.Unsubscribe(ctx, zoneFilter); err != nil {
		t.Fatalf("unsubscribing: %v", err)
	}
	if err := pub.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("2"), QoS: msgbus.AtLeastOnce}); err != nil {
		t.Fatalf("publishing after unsubscribe: %v", err)
	}
	got.none(t, 200*time.Millisecond)
}

func TestRetainedDeliveryAndClearing(t *testing.T) {
	b, addr := startBroker(t, Options{})
	pub := dialClient(t, testConfig(addr, "gateway-1"))
	ctx := context.Background()

	// The store's current prices, published before any controller is listening.
	for i, price := range []string{"399", "499"} {
		topic := fmt.Sprintf("usslp/acme/eu-west-1/store-7/labels/L-%04d/price", i+1)
		if err := pub.Publish(ctx, msgbus.Message{Topic: topic, Payload: []byte(price),
			QoS: msgbus.AtLeastOnce, Retain: true}); err != nil {
			t.Fatalf("retaining %s: %v", topic, err)
		}
	}
	waitFor(t, "two retained topics", func() bool { return b.RetainedCount() == 2 })

	// A controller rebooting after a power cut recovers its zone by subscribing.
	sub := dialClient(t, testConfig(addr, "sec-1"))
	got := newCollector()
	if err := sub.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	seen := map[string]string{}
	for i := 0; i < 2; i++ {
		m := got.next(t)
		if !m.Retain {
			t.Errorf("retained delivery of %q did not set the retain flag", m.Topic)
		}
		seen[m.Topic] = string(m.Payload)
	}
	if seen["usslp/acme/eu-west-1/store-7/labels/L-0001/price"] != "399" ||
		seen["usslp/acme/eu-west-1/store-7/labels/L-0002/price"] != "499" {
		t.Fatalf("wildcard subscribe recovered %v, want both retained prices", seen)
	}

	// A zero-length retained publish decommissions the label.
	if err := pub.Publish(ctx, msgbus.Message{Topic: priceTopic, QoS: msgbus.AtLeastOnce, Retain: true}); err != nil {
		t.Fatalf("clearing retained message: %v", err)
	}
	waitFor(t, "one retained topic", func() bool { return b.RetainedCount() == 1 })

	later := dialClient(t, testConfig(addr, "sec-2"))
	fresh := newCollector()
	if err := later.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, fresh.handle); err != nil {
		t.Fatalf("subscribing after the clear: %v", err)
	}
	m := fresh.next(t)
	if m.Topic == priceTopic {
		t.Errorf("cleared topic %q was still delivered as retained", m.Topic)
	}
	fresh.none(t, 200*time.Millisecond)
}

func TestQoS1AcknowledgementCompletesPublish(t *testing.T) {
	_, addr := startBroker(t, Options{})
	pub := dialClient(t, testConfig(addr, "gateway-1"))

	start := time.Now()
	err := pub.Publish(context.Background(), msgbus.Message{
		Topic: priceTopic, Payload: []byte("399"), QoS: msgbus.AtLeastOnce})
	if err != nil {
		t.Fatalf("QoS 1 publish: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("QoS 1 publish took %v; the PUBACK was not seen promptly", elapsed)
	}
}

// TestQoS1PublishTimesOutWithoutAck points the client at a socket that accepts
// a CONNECT and then says nothing, which is what a wedged broker looks like.
func TestQoS1PublishTimesOutWithoutAck(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer l.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		nc, err := l.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		silent := &rawConn{t: t, nc: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}
		if _, err := silent.read(testTimeout); err != nil {
			return
		}
		silent.write(&connackPacket{ReturnCode: ConnectAccepted})
		<-time.After(3 * time.Second)
	}()

	cfg := testConfig(l.Addr().String(), "gateway-1")
	cfg.AckTimeout = 300 * time.Millisecond
	c := dialClient(t, cfg)
	err = c.Publish(context.Background(), msgbus.Message{
		Topic: priceTopic, Payload: []byte("399"), QoS: msgbus.AtLeastOnce})
	if err != msgbus.ErrTimeout {
		t.Fatalf("publish with no PUBACK returned %v, want msgbus.ErrTimeout", err)
	}
	c.Close()
	<-done
}

func TestQoS2HandshakeDeliversExactlyOnce(t *testing.T) {
	_, addr := startBroker(t, Options{})
	sub := dialClient(t, testConfig(addr, "sec-1"))
	got := newCollector()
	if err := sub.Subscribe(context.Background(), zoneFilter, msgbus.ExactlyOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	// Publishing by hand so the whole four-way handshake is observable, and so
	// the PUBLISH can be repeated with DUP the way a publisher that lost our
	// PUBREC would repeat it.
	pub := dialRaw(t, addr)
	pub.connect(&connectPacket{ClientID: "gateway-1", CleanSession: true})

	otaTopic := "usslp/acme/eu-west-1/store-7/labels/L-0001/price"
	publish := &publishPacket{QoS: msgbus.ExactlyOnce, Topic: otaTopic, PacketID: 77, Payload: []byte("399")}
	pub.write(publish)
	if p := pub.mustRead(); p.(*ackPacket).Type != pktPUBREC {
		t.Fatalf("expected PUBREC, got %s", p.pktType())
	}

	// The publisher believes its PUBREC was lost and repeats the PUBLISH.
	dup := *publish
	dup.Dup = true
	pub.write(&dup)
	if p := pub.mustRead(); p.(*ackPacket).Type != pktPUBREC {
		t.Fatalf("expected a second PUBREC for the duplicate, got %s", p.pktType())
	}

	pub.write(&ackPacket{Type: pktPUBREL, PacketID: 77})
	if p := pub.mustRead(); p.(*ackPacket).Type != pktPUBCOMP {
		t.Fatalf("expected PUBCOMP, got %s", p.pktType())
	}

	if m := got.next(t); string(m.Payload) != "399" {
		t.Errorf("delivered payload %q, want 399", m.Payload)
	}
	got.none(t, 300*time.Millisecond)
	if n := got.count(); n != 1 {
		t.Fatalf("a duplicated QoS 2 publication reached the application %d times, want 1", n)
	}
}

func TestQoS2PublishFromClientCompletes(t *testing.T) {
	_, addr := startBroker(t, Options{})
	sub := dialClient(t, testConfig(addr, "sec-1"))
	pub := dialClient(t, testConfig(addr, "gateway-1"))
	ctx := context.Background()

	got := newCollector()
	if err := sub.Subscribe(ctx, "usslp/acme/eu-west-1/store-7/sec/+/ota", msgbus.ExactlyOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	err := pub.Publish(ctx, msgbus.Message{
		Topic:   "usslp/acme/eu-west-1/store-7/sec/S1/ota",
		Payload: []byte("firmware-2.4.1"), QoS: msgbus.ExactlyOnce})
	if err != nil {
		t.Fatalf("QoS 2 publish: %v", err)
	}
	if m := got.next(t); m.QoS != msgbus.ExactlyOnce {
		t.Errorf("delivered at QoS %d, want 2", m.QoS)
	}
	got.none(t, 200*time.Millisecond)
}

func TestLastWillFiresOnAbruptCloseAndNotOnDisconnect(t *testing.T) {
	_, addr := startBroker(t, Options{})
	statusFilter := "usslp/acme/eu-west-1/store-7/sec/+/status"
	willTopic := "usslp/acme/eu-west-1/store-7/sec/S1/status"

	watcher := dialClient(t, testConfig(addr, "gateway-1"))
	got := newCollector()
	if err := watcher.Subscribe(context.Background(), statusFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	// A controller that loses power: the socket dies with no DISCONNECT.
	dying := dialRaw(t, addr)
	dying.connect(&connectPacket{
		ClientID: "sec-1", CleanSession: true, WillFlag: true,
		WillTopic: willTopic, WillMessage: []byte("offline"), WillQoS: msgbus.AtLeastOnce,
	})
	dying.nc.Close()

	m := got.next(t)
	if m.Topic != willTopic || string(m.Payload) != "offline" {
		t.Fatalf("will delivered as %q/%q, want %q/offline", m.Topic, m.Payload, willTopic)
	}

	// A controller switched off for maintenance: a clean DISCONNECT, so the
	// gateway must not be told it died.
	cfg := testConfig(addr, "sec-2")
	willTopic2 := "usslp/acme/eu-west-1/store-7/sec/S2/status"
	cfg.Will = &msgbus.Will{Topic: willTopic2, Payload: []byte("offline"), QoS: msgbus.AtLeastOnce}
	polite, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	if err := polite.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	got.none(t, 500*time.Millisecond)
}

// TestSessionResumptionRedeliversUnackedQoS1 is the reboot-mid-price-change
// case: a controller receives an update, dies before acknowledging it, and must
// be handed the same message again when it returns.
func TestSessionResumptionRedeliversUnackedQoS1(t *testing.T) {
	_, addr := startBroker(t, Options{})

	sec := dialRaw(t, addr)
	sec.connect(&connectPacket{ClientID: "sec-1", CleanSession: false})
	if sa := sec.subscribe(1, zoneFilter, msgbus.AtLeastOnce); sa.Codes[0] != byte(msgbus.AtLeastOnce) {
		t.Fatalf("SUBACK granted 0x%02x, want QoS 1", sa.Codes[0])
	}

	pub := dialClient(t, testConfig(addr, "gateway-1"))
	if err := pub.Publish(context.Background(), msgbus.Message{
		Topic: priceTopic, Payload: []byte("399"), QoS: msgbus.AtLeastOnce}); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	first, ok := sec.mustRead().(*publishPacket)
	if !ok {
		t.Fatal("expected a PUBLISH")
	}
	if first.Dup {
		t.Error("first delivery must not set DUP")
	}
	// Power cut: no PUBACK, socket gone.
	sec.nc.Close()

	// The controller comes back with the same identity and no clean session.
	back := dialRaw(t, addr)
	ack := back.connect(&connectPacket{ClientID: "sec-1", CleanSession: false})
	if !ack.SessionPresent {
		t.Error("CONNACK did not report the session as present")
	}
	again, ok := back.mustRead().(*publishPacket)
	if !ok {
		t.Fatal("expected the un-acknowledged PUBLISH to be redelivered")
	}
	if !again.Dup {
		t.Error("redelivery did not set the DUP flag")
	}
	if again.PacketID != first.PacketID {
		t.Errorf("redelivered with packet id %d, want the original %d", again.PacketID, first.PacketID)
	}
	if string(again.Payload) != "399" {
		t.Errorf("redelivered payload %q, want 399", again.Payload)
	}
	back.write(&ackPacket{Type: pktPUBACK, PacketID: again.PacketID})
}

func TestCleanSessionDiscardsSubscriptions(t *testing.T) {
	b, addr := startBroker(t, Options{})

	first := dialRaw(t, addr)
	first.connect(&connectPacket{ClientID: "sec-1", CleanSession: true})
	first.subscribe(1, zoneFilter, msgbus.AtLeastOnce)
	first.nc.Close()
	waitFor(t, "the clean session to be discarded", func() bool { return b.SessionCount() == 0 })

	second := dialRaw(t, addr)
	ack := second.connect(&connectPacket{ClientID: "sec-1", CleanSession: true})
	if ack.SessionPresent {
		t.Error("CONNACK reported a session present after a clean-session disconnect")
	}

	pub := dialClient(t, testConfig(addr, "gateway-1"))
	if err := pub.Publish(context.Background(), msgbus.Message{
		Topic: priceTopic, Payload: []byte("399"), QoS: msgbus.AtMostOnce}); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if _, err := second.read(300 * time.Millisecond); err == nil {
		t.Fatal("a discarded subscription still delivered a message")
	}
}

// TestOfflineQueueOverflowDropsOldest pins the documented overflow policy: the
// newest price is the correct one, so the head of the queue is what goes.
func TestOfflineQueueOverflowDropsOldest(t *testing.T) {
	b, addr := startBroker(t, Options{OfflineQueueSize: 2})

	sec := dialRaw(t, addr)
	sec.connect(&connectPacket{ClientID: "sec-1", CleanSession: false})
	sec.subscribe(1, zoneFilter, msgbus.AtLeastOnce)
	sec.nc.Close()

	pub := dialClient(t, testConfig(addr, "gateway-1"))
	for _, price := range []string{"100", "200", "300", "400"} {
		if err := pub.Publish(context.Background(), msgbus.Message{
			Topic: priceTopic, Payload: []byte(price), QoS: msgbus.AtLeastOnce}); err != nil {
			t.Fatalf("publishing %s: %v", price, err)
		}
	}

	b.mu.RLock()
	held := b.sessions["sec-1"]
	b.mu.RUnlock()
	if held == nil {
		t.Fatal("the persistent session was discarded on disconnect")
	}
	waitFor(t, "the queue to settle at its bound", func() bool { return held.queueLen() == 2 })
	if n := held.inflightCount(); n != 0 {
		t.Errorf("an offline session holds %d in-flight messages, want 0", n)
	}

	back := dialRaw(t, addr)
	back.connect(&connectPacket{ClientID: "sec-1", CleanSession: false})
	var payloads []string
	for i := 0; i < 2; i++ {
		p, ok := back.mustRead().(*publishPacket)
		if !ok {
			t.Fatal("expected a queued PUBLISH")
		}
		payloads = append(payloads, string(p.Payload))
		back.write(&ackPacket{Type: pktPUBACK, PacketID: p.PacketID})
	}
	if payloads[0] != "300" || payloads[1] != "400" {
		t.Errorf("queue held %v, want the two newest prices [300 400]", payloads)
	}
	if _, err := back.read(300 * time.Millisecond); err == nil {
		t.Error("more than the queue's capacity was delivered")
	}
}

func TestKeepaliveDisconnectsSilentClient(t *testing.T) {
	_, addr := startBroker(t, Options{})
	c := dialRaw(t, addr)
	c.connect(&connectPacket{ClientID: "sec-1", CleanSession: true, KeepAlive: 1})

	start := time.Now()
	_, err := c.read(4 * time.Second)
	if err == nil {
		t.Fatal("a silent client was not disconnected")
	}
	elapsed := time.Since(start)
	if elapsed < time.Second {
		t.Errorf("disconnected after %v; 1.5x a 1s keepalive is 1.5s", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("disconnected after %v; the keepalive timer is not firing at 1.5x", elapsed)
	}
}

func TestPingKeepsConnectionAlive(t *testing.T) {
	_, addr := startBroker(t, Options{})
	c := dialRaw(t, addr)
	c.connect(&connectPacket{ClientID: "sec-1", CleanSession: true, KeepAlive: 1})

	for i := 0; i < 4; i++ {
		time.Sleep(400 * time.Millisecond)
		c.write(&emptyPacket{Type: pktPINGREQ})
		p, err := c.read(time.Second)
		if err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
		if e, ok := p.(*emptyPacket); !ok || e.Type != pktPINGRESP {
			t.Fatalf("ping %d answered with %s, want PINGRESP", i, p.pktType())
		}
	}
}

// TestMalformedPacketDisconnectsWithoutKillingTheBroker is the hostile-input
// case: a device with corrupt firmware on a store LAN must cost exactly one
// connection, not the store's messaging.
func TestMalformedPacketDisconnectsWithoutKillingTheBroker(t *testing.T) {
	_, addr := startBroker(t, Options{})

	garbageCases := [][]byte{
		{0x00, 0x00},                                // reserved packet type
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},        // unterminated remaining length
		{0x10, 0x04, 0x00, 0x02, 'X', 'Y'},          // CONNECT for protocol "XY"
		{0x30, 0x05, 0x00, 0x10, 'a', 'b', 'c'},     // string longer than the body
		[]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), // an HTTP client on the MQTT port
	}
	for i, garbage := range garbageCases {
		nc, err := net.DialTimeout("tcp", addr, testTimeout)
		if err != nil {
			t.Fatalf("case %d: dialing: %v", i, err)
		}
		if _, err := nc.Write(garbage); err != nil {
			nc.Close()
			continue
		}
		if err := nc.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		buf := make([]byte, 64)
		for {
			if _, err := nc.Read(buf); err != nil {
				break
			}
		}
		nc.Close()
	}

	// The broker is still serving.
	sub := dialClient(t, testConfig(addr, "sec-1"))
	pub := dialClient(t, testConfig(addr, "gateway-1"))
	got := newCollector()
	ctx := context.Background()
	if err := sub.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing after the garbage: %v", err)
	}
	if err := pub.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("399"), QoS: msgbus.AtLeastOnce}); err != nil {
		t.Fatalf("publishing after the garbage: %v", err)
	}
	got.next(t)
}

func TestSecondConnectOnOneConnectionIsRejected(t *testing.T) {
	_, addr := startBroker(t, Options{})
	c := dialRaw(t, addr)
	c.connect(&connectPacket{ClientID: "sec-1", CleanSession: true})
	c.write(&connectPacket{ProtocolName: protocolName, ProtocolLevel: protocolLevel,
		ClientID: "sec-1", CleanSession: true})
	if _, err := c.read(testTimeout); err == nil {
		t.Fatal("a second CONNECT on one connection was accepted")
	}
}

func TestUnsupportedProtocolLevelIsRefused(t *testing.T) {
	_, addr := startBroker(t, Options{})
	c := dialRaw(t, addr)
	c.write(&connectPacket{ProtocolName: "MQIsdp", ProtocolLevel: 3, ClientID: "old-device", CleanSession: true})
	p, err := c.read(testTimeout)
	if err != nil {
		t.Fatalf("reading CONNACK: %v", err)
	}
	ack, ok := p.(*connackPacket)
	if !ok {
		t.Fatalf("got %s, want CONNACK", p.pktType())
	}
	if ack.ReturnCode != ConnectUnacceptableProto {
		t.Errorf("return code %s, want %s", ack.ReturnCode, ConnectUnacceptableProto)
	}
}

func TestEmptyClientIDWithoutCleanSessionIsRefused(t *testing.T) {
	_, addr := startBroker(t, Options{})
	c := dialRaw(t, addr)
	c.write(&connectPacket{ProtocolName: protocolName, ProtocolLevel: protocolLevel, CleanSession: false})
	p, err := c.read(testTimeout)
	if err != nil {
		t.Fatalf("reading CONNACK: %v", err)
	}
	if ack := p.(*connackPacket); ack.ReturnCode != ConnectIdentifierRejected {
		t.Errorf("return code %s, want %s", ack.ReturnCode, ConnectIdentifierRejected)
	}
}

func TestSessionTakeoverClosesTheOlderConnection(t *testing.T) {
	_, addr := startBroker(t, Options{})
	first := dialRaw(t, addr)
	first.connect(&connectPacket{ClientID: "sec-1", CleanSession: false})

	second := dialRaw(t, addr)
	second.connect(&connectPacket{ClientID: "sec-1", CleanSession: false})

	if _, err := first.read(testTimeout); err == nil {
		t.Fatal("the displaced connection was left open")
	}

	// The surviving connection still works.
	second.write(&emptyPacket{Type: pktPINGREQ})
	if p := second.mustRead(); p.pktType() != pktPINGRESP {
		t.Fatalf("takeover left the new connection broken: got %s", p.pktType())
	}
}

func TestBrokerPublishInjectsRetainedState(t *testing.T) {
	b, addr := startBroker(t, Options{})
	if err := b.Publish(msgbus.Message{Topic: priceTopic, Payload: []byte("399"),
		QoS: msgbus.AtLeastOnce, Retain: true}); err != nil {
		t.Fatalf("injecting: %v", err)
	}
	sub := dialClient(t, testConfig(addr, "sec-1"))
	got := newCollector()
	if err := sub.Subscribe(context.Background(), zoneFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	if m := got.next(t); string(m.Payload) != "399" {
		t.Errorf("recovered %q, want 399", m.Payload)
	}

	if err := b.Publish(msgbus.Message{Topic: "usslp/acme/+/x"}); err == nil {
		t.Error("Broker.Publish accepted a wildcard as a topic name")
	}
}

func TestBrokerMetrics(t *testing.T) {
	reg := obs.NewRegistry("service", "sgu")
	b, addr := startBroker(t, Options{Registry: reg})
	sub := dialClient(t, testConfig(addr, "sec-1"))
	pub := dialClient(t, testConfig(addr, "gateway-1"))
	ctx := context.Background()

	got := newCollector()
	if err := sub.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	if err := pub.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("399"),
		QoS: msgbus.AtLeastOnce, Retain: true}); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	got.next(t)

	waitFor(t, "two connected clients", func() bool { return b.metrics.connected.Value() == 2 })
	if v := b.metrics.retained.Value(); v != 1 {
		t.Errorf("retained gauge is %v, want 1", v)
	}
	if v := b.metrics.subs.Value(); v != 1 {
		t.Errorf("subscription gauge is %v, want 1", v)
	}
	if v := b.metrics.in.With("1").Value(); v != 1 {
		t.Errorf("received counter for QoS 1 is %d, want 1", v)
	}
	if v := b.metrics.out.With("1").Value(); v != 1 {
		t.Errorf("sent counter for QoS 1 is %d, want 1", v)
	}
	if v := b.metrics.connects.With("accepted").Value(); v != 2 {
		t.Errorf("accepted-connection counter is %d, want 2", v)
	}

	var sb strings.Builder
	reg.WriteText(&sb)
	if !strings.Contains(sb.String(), metricBrokerRetained) {
		t.Error("the registry did not render the broker's metrics")
	}
}

// TestConcurrentPublishersAndSubscribers is the race-detector workload: many
// goroutines publishing while many subscribers receive, across all three QoS
// levels, with the broker fanning out between them.
func TestConcurrentPublishersAndSubscribers(t *testing.T) {
	_, addr := startBroker(t, Options{})
	ctx := context.Background()

	const (
		subscribers  = 6
		publishers   = 8
		perPublisher = 15
	)

	var delivered atomic.Int64
	for i := 0; i < subscribers; i++ {
		c := dialClient(t, testConfig(addr, fmt.Sprintf("sec-%d", i)))
		h := func(_ context.Context, m msgbus.Message) { delivered.Add(1) }
		if err := c.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, h); err != nil {
			t.Fatalf("subscriber %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, publishers)
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c, err := Dial(ctx, testConfig(addr, fmt.Sprintf("gateway-%d", n)))
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			for j := 0; j < perPublisher; j++ {
				topic := fmt.Sprintf("usslp/acme/eu-west-1/store-7/labels/L-%02d%02d/price", n, j)
				qos := msgbus.QoS(j % 3)
				if qos == msgbus.AtMostOnce {
					// QoS 0 may legitimately be dropped by a full send buffer,
					// which would make the delivery count non-deterministic.
					qos = msgbus.AtLeastOnce
				}
				if err := c.Publish(ctx, msgbus.Message{Topic: topic,
					Payload: []byte(fmt.Sprintf("%d-%d", n, j)), QoS: qos}); err != nil {
					errs <- fmt.Errorf("publisher %d message %d: %w", n, j, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent publish: %v", err)
	}

	want := int64(subscribers * publishers * perPublisher)
	waitFor(t, "every subscriber to receive every message", func() bool {
		return delivered.Load() >= want
	})
	if got := delivered.Load(); got != want {
		t.Errorf("delivered %d messages, want exactly %d", got, want)
	}
}

func TestShutdownIsGraceful(t *testing.T) {
	b := NewBroker(Options{Addr: "127.0.0.1:0"})
	addr, err := b.Start()
	if err != nil {
		t.Fatalf("starting: %v", err)
	}
	c := dialRaw(t, addr.String())
	c.connect(&connectPacket{ClientID: "sec-1", CleanSession: true})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := c.read(time.Second); err == nil {
		t.Error("shutdown left a client connection open")
	}
	if err := b.Publish(msgbus.Message{Topic: priceTopic}); err != ErrBrokerClosed {
		t.Errorf("Publish after shutdown returned %v, want ErrBrokerClosed", err)
	}
	if err := b.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown returned %v, want nil", err)
	}
}

// TestListenAndServeAndAddrs covers the blocking entry point a long-running SGU
// process uses, and the address lookup a supervisor needs after an ephemeral
// bind.
func TestListenAndServeAndAddrs(t *testing.T) {
	b := NewBroker(Options{Addr: "127.0.0.1:0"})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- b.Serve(l) }()

	waitFor(t, "the listener to be registered", func() bool { return len(b.Addrs()) == 1 })
	if got := b.Addrs()[0].String(); got != l.Addr().String() {
		t.Errorf("Addrs reported %s, want %s", got, l.Addr().String())
	}

	c := dialRaw(t, l.Addr().String())
	c.connect(&connectPacket{ClientID: "sec-1", CleanSession: true})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-served:
		if err != ErrBrokerClosed {
			t.Errorf("Serve returned %v, want ErrBrokerClosed", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Serve did not return after Shutdown")
	}

	// ListenAndServe on a closed broker must refuse rather than bind a socket
	// nothing will ever accept on.
	if err := b.ListenAndServe(); err != ErrBrokerClosed {
		t.Errorf("ListenAndServe on a closed broker returned %v, want ErrBrokerClosed", err)
	}
}

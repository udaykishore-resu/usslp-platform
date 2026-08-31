package mqtt

import (
	"bufio"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// testTimeout bounds any single wait in these tests. It is generous relative to
// loopback latency so a loaded CI machine does not produce a false failure, and
// short enough that a genuine hang fails the run rather than the package timer.
const testTimeout = 5 * time.Second

// startBroker brings up a broker on an ephemeral loopback port and returns its
// address. Every test gets its own broker so that session state, retained
// messages and metrics cannot leak between them.
func startBroker(t *testing.T, opts Options) (*Broker, string) {
	t.Helper()
	opts.Addr = "127.0.0.1:0"
	b := NewBroker(opts)
	addr, err := b.Start()
	if err != nil {
		t.Fatalf("starting broker: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if err := b.Shutdown(ctx); err != nil {
			t.Errorf("broker shutdown: %v", err)
		}
	})
	return b, addr.String()
}

// startBrokerAt binds a specific address, used by the reconnect test which has
// to restart a broker on the port its client is still trying to reach.
func startBrokerAt(t *testing.T, addr string, opts Options) *Broker {
	t.Helper()
	opts.Addr = addr
	b := NewBroker(opts)
	if _, err := b.Start(); err != nil {
		t.Fatalf("starting broker on %s: %v", addr, err)
	}
	return b
}

func testConfig(addr, clientID string) msgbus.Config {
	return msgbus.Config{
		BrokerURL:      "tcp://" + addr,
		ClientID:       clientID,
		CleanSession:   true,
		ConnectTimeout: testTimeout,
		AckTimeout:     2 * time.Second,
	}
}

func dialClient(t *testing.T, cfg msgbus.Config, opts ...ClientOption) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	c, err := Dial(ctx, cfg, opts...)
	if err != nil {
		t.Fatalf("dialing broker as %q: %v", cfg.ClientID, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// collector is a message sink for handler assertions.
type collector struct {
	mu   sync.Mutex
	msgs []msgbus.Message
	ch   chan msgbus.Message
}

func newCollector() *collector {
	return &collector{ch: make(chan msgbus.Message, 64)}
}

func (c *collector) handle(_ context.Context, m msgbus.Message) {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
	select {
	case c.ch <- m:
	default:
	}
}

// next waits for one message, failing the test if none arrives.
func (c *collector) next(t *testing.T) msgbus.Message {
	t.Helper()
	select {
	case m := <-c.ch:
		return m
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a message")
		return msgbus.Message{}
	}
}

// none asserts that nothing arrives within d, which is how "delivered exactly
// once" is checked after the first delivery has been consumed.
func (c *collector) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-c.ch:
		t.Fatalf("unexpected extra message on %q", m.Topic)
	case <-time.After(d):
	}
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// rawConn is a hand-driven MQTT client used where the real Client is too
// well-behaved to exercise the broker: abrupt socket closes, deliberately
// duplicated QoS 2 publishes, garbage bytes, and un-acknowledged deliveries.
type rawConn struct {
	t  *testing.T
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer
}

func dialRaw(t *testing.T, addr string) *rawConn {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, testTimeout)
	if err != nil {
		t.Fatalf("dialing %s: %v", addr, err)
	}
	c := &rawConn{t: t, nc: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}
	t.Cleanup(func() { nc.Close() })
	return c
}

func (c *rawConn) write(p packet) {
	c.t.Helper()
	if err := c.nc.SetWriteDeadline(time.Now().Add(testTimeout)); err != nil {
		c.t.Fatalf("setting write deadline: %v", err)
	}
	if err := writePacket(c.w, p); err != nil {
		c.t.Fatalf("writing %s: %v", p.pktType(), err)
	}
	if err := c.w.Flush(); err != nil {
		c.t.Fatalf("flushing %s: %v", p.pktType(), err)
	}
}

func (c *rawConn) read(d time.Duration) (packet, error) {
	if err := c.nc.SetReadDeadline(time.Now().Add(d)); err != nil {
		return nil, err
	}
	return readPacket(c.r, defaultMaxPacketSize)
}

func (c *rawConn) mustRead() packet {
	c.t.Helper()
	p, err := c.read(testTimeout)
	if err != nil {
		c.t.Fatalf("reading packet: %v", err)
	}
	return p
}

// connect performs the handshake and asserts it was accepted.
func (c *rawConn) connect(cp *connectPacket) *connackPacket {
	c.t.Helper()
	cp.ProtocolName = protocolName
	cp.ProtocolLevel = protocolLevel
	c.write(cp)
	p := c.mustRead()
	ack, ok := p.(*connackPacket)
	if !ok {
		c.t.Fatalf("expected CONNACK, got %s", p.pktType())
	}
	if ack.ReturnCode != ConnectAccepted {
		c.t.Fatalf("CONNACK refused the connection: %s", ack.ReturnCode)
	}
	return ack
}

func (c *rawConn) subscribe(id uint16, filter string, qos msgbus.QoS) *subackPacket {
	c.t.Helper()
	c.write(&subscribePacket{PacketID: id, Filters: []topicFilter{{Filter: filter, QoS: qos}}})
	p := c.mustRead()
	sa, ok := p.(*subackPacket)
	if !ok {
		c.t.Fatalf("expected SUBACK, got %s", p.pktType())
	}
	return sa
}

// waitFor polls cond until it holds or the timeout expires. Used for state that
// settles asynchronously — a reconnect, a session teardown — where sleeping a
// fixed interval would be both slower and less reliable.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

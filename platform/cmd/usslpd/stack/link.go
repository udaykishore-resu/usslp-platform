package stack

import (
	"errors"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// wanLink is the wide-area link between one store and the cloud.
//
// # Why a proxy rather than a flag
//
// The store gateway already has a mode switch (sgu.Detector.ForceMode) and it
// would be easy to flip it and call that an outage. It would also prove
// nothing. The behaviour worth testing is what happens when the *network* goes
// away: whether the detector notices on its own, whether the bridge's publishes
// fail and get buffered, whether the MQTT client's own reconnect loop finds its
// way back, whether the cloud's retained state arrives on re-subscription and
// is treated as a view to merge rather than as instructions to apply. Flipping
// a flag skips every one of those.
//
// So the gateway dials this listener instead of the cloud broker, and Cut
// closes every established connection and refuses new ones — which is what a
// severed uplink looks like from inside a store. Degrade adds one-way latency
// and packet loss instead, for the "the link is bad but not gone" case that is
// far more common in a real store and much harder to handle.
type wanLink struct {
	upstream string
	ln       net.Listener

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool

	// cut, delayNS and lossPercent are read on the data path, so they are
	// atomics rather than mutex-guarded: an outage injected from an HTTP
	// handler must take effect on bytes already in flight.
	cut         atomic.Bool
	delayNS     atomic.Int64
	lossPercent atomic.Int64

	accepted atomic.Int64
	refused  atomic.Int64
	dropped  atomic.Int64

	rndMu sync.Mutex
	rnd   *rand.Rand
}

// newWANLink binds a loopback listener that proxies to upstream.
func newWANLink(upstream string, seed int64) (*wanLink, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l := &wanLink{
		upstream: upstream, ln: ln,
		conns: map[net.Conn]struct{}{},
		rnd:   rand.New(rand.NewSource(seed)),
	}
	go l.accept()
	return l, nil
}

// URL is the broker URL a store gateway should dial.
func (l *wanLink) URL() string { return "tcp://" + l.ln.Addr().String() }

func (l *wanLink) accept() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		if l.cut.Load() {
			// Refusing at accept time, rather than never listening, is what
			// makes the reconnect path realistic: the client gets a connection
			// that dies immediately, which is what a stateful firewall or a
			// dead load balancer produces, and is harder for a client to handle
			// correctly than a refused SYN.
			l.refused.Add(1)
			_ = c.Close()
			continue
		}
		l.accepted.Add(1)
		go l.proxy(c)
	}
}

func (l *wanLink) proxy(down net.Conn) {
	up, err := net.DialTimeout("tcp", l.upstream, 5*time.Second)
	if err != nil {
		_ = down.Close()
		return
	}
	l.track(down)
	l.track(up)
	defer func() {
		l.untrack(down)
		l.untrack(up)
		_ = down.Close()
		_ = up.Close()
	}()

	done := make(chan struct{}, 2)
	go func() { l.copy(up, down); done <- struct{}{} }()
	go func() { l.copy(down, up); done <- struct{}{} }()
	<-done
}

// copy moves bytes one way, applying the injected delay and loss.
//
// Loss is applied per read rather than per TCP segment, which is a
// simplification worth naming: on a real link loss happens below TCP and is
// repaired by retransmission, so what a lossy WAN actually costs an MQTT client
// is latency and stalls rather than missing bytes. Dropping a chunk here
// corrupts the stream, which the MQTT codec detects and treats as a broken
// connection — so what this models honestly is "the link fails intermittently",
// which is the behaviour a store gateway has to survive.
func (l *wanLink) copy(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if l.cut.Load() {
				return
			}
			if d := time.Duration(l.delayNS.Load()); d > 0 {
				time.Sleep(d)
			}
			if l.drop() {
				l.dropped.Add(1)
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return
			}
			return
		}
	}
}

func (l *wanLink) drop() bool {
	pct := l.lossPercent.Load()
	if pct <= 0 {
		return false
	}
	l.rndMu.Lock()
	defer l.rndMu.Unlock()
	return l.rnd.Int63n(100) < pct
}

func (l *wanLink) track(c net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		_ = c.Close()
		return
	}
	l.conns[c] = struct{}{}
}

func (l *wanLink) untrack(c net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.conns, c)
}

// Cut severs the link: every established connection is closed and new ones are
// refused until Restore.
func (l *wanLink) Cut() {
	l.cut.Store(true)
	l.mu.Lock()
	for c := range l.conns {
		_ = c.SetDeadline(time.Now())
		_ = c.Close()
	}
	l.conns = map[net.Conn]struct{}{}
	l.mu.Unlock()
}

// Restore brings the link back.
func (l *wanLink) Restore() {
	l.cut.Store(false)
	l.delayNS.Store(0)
	l.lossPercent.Store(0)
}

// Degrade adds one-way latency and a loss probability without cutting the link.
func (l *wanLink) Degrade(delay time.Duration, lossPercent int) {
	if delay < 0 {
		delay = 0
	}
	if lossPercent < 0 {
		lossPercent = 0
	}
	if lossPercent > 100 {
		lossPercent = 100
	}
	l.delayNS.Store(int64(delay))
	l.lossPercent.Store(int64(lossPercent))
}

// IsCut reports the link state.
func (l *wanLink) IsCut() bool { return l.cut.Load() }

// linkStats is what the control surface reports.
type linkStats struct {
	Cut         bool   `json:"cut"`
	DelayMS     int64  `json:"injected_delay_ms"`
	LossPercent int64  `json:"injected_loss_percent"`
	Accepted    int64  `json:"connections_accepted"`
	Refused     int64  `json:"connections_refused"`
	Dropped     int64  `json:"streams_dropped"`
	Upstream    string `json:"upstream"`
}

func (l *wanLink) stats() linkStats {
	return linkStats{
		Cut: l.cut.Load(), DelayMS: l.delayNS.Load() / int64(time.Millisecond),
		LossPercent: l.lossPercent.Load(), Accepted: l.accepted.Load(),
		Refused: l.refused.Load(), Dropped: l.dropped.Load(), Upstream: l.upstream,
	}
}

// Close stops the proxy.
func (l *wanLink) Close() error {
	l.mu.Lock()
	l.closed = true
	for c := range l.conns {
		_ = c.Close()
	}
	l.conns = map[net.Conn]struct{}{}
	l.mu.Unlock()
	return l.ln.Close()
}

package eventlog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/retry"
)

// idlePollInterval is a safety net, not the delivery mechanism: a partition
// wakes its readers directly on append, and this only bounds how long a reader
// sleeps if a wake-up is ever lost.
const idlePollInterval = 25 * time.Millisecond

// dlqReasonLimit caps the length of the failure text copied into a header.
// Headers are read by the audit sink without deserialising the body, so one
// pathological error string must not make every dead-lettered record huge.
const dlqReasonLimit = 512

// group is the in-process consumer-group coordinator.
//
// Members are Run calls, not network peers, so there is no session timeout, no
// heartbeat and no generation fencing to get wrong: a member that stops is a
// goroutine that returned, and the rebalance is synchronous with its departure.
// What is preserved from Kafka is the part services depend on — partitions are
// split between members, every partition has exactly one owner, and a member
// leaving hands its partitions to the survivors.
type group struct {
	id      string
	mu      sync.Mutex
	members []*member
	nextID  uint64
	// claims is a one-slot semaphore per topic-partition. A rebalance is not
	// instantaneous — the member losing a partition only stops its worker when
	// its Run loop next wakes — so without an explicit hand-off token the new
	// owner would start reading while the old one is still mid-batch and the
	// group would see the same record twice. Holding the claim for the life of
	// a worker makes "exactly one owner per partition" true rather than merely
	// intended.
	claims map[tp]chan struct{}
}

// claim returns the hand-off token for a topic-partition, creating it on first
// use.
func (g *group) claim(t tp) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.claims == nil {
		g.claims = make(map[tp]chan struct{})
	}
	ch, ok := g.claims[t]
	if !ok {
		ch = make(chan struct{}, 1)
		g.claims[t] = ch
	}
	return ch
}

// member is one Run call's stake in a group.
type member struct {
	id     uint64
	topics []string
	mu     sync.Mutex
	owned  map[tp]bool
	notify chan struct{}
}

func (m *member) assignment() map[tp]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[tp]bool, len(m.owned))
	for t := range m.owned {
		out[t] = true
	}
	return out
}

func (m *member) setAssignment(a map[tp]bool) {
	m.mu.Lock()
	m.owned = a
	m.mu.Unlock()
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

// joinLocked adds a member and rebalances. The caller holds l.mu; the lock
// order in this package is always Log then group, never the reverse.
func (g *group) joinLocked(m *member, counts map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m.id = g.nextID
	g.nextID++
	g.members = append(g.members, m)
	g.rebalanceLocked(counts)
}

func (g *group) leaveLocked(m *member, counts map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, other := range g.members {
		if other == m {
			g.members = append(g.members[:i], g.members[i+1:]...)
			break
		}
	}
	m.setAssignment(map[tp]bool{})
	g.rebalanceLocked(counts)
}

// rebalance recomputes the assignment, used when the stream catalogue changes
// under a running group.
func (g *group) rebalance(counts map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rebalanceLocked(counts)
}

// rebalanceLocked distributes each topic's partitions round-robin over the
// members that subscribe to it.
//
// Round-robin over members in join order (rather than, say, hashing) is chosen
// for stability: re-running the plan with the same membership produces the same
// assignment, so a rebalance triggered by an unrelated topic does not shuffle
// partitions that did not need to move, and every needless partition move costs
// a re-read from the last committed offset.
func (g *group) rebalanceLocked(counts map[string]int) {
	staged := make(map[*member]map[tp]bool, len(g.members))
	byTopic := make(map[string][]*member)
	for _, m := range g.members {
		staged[m] = make(map[tp]bool)
		for _, name := range m.topics {
			byTopic[name] = append(byTopic[name], m)
		}
	}
	names := make([]string, 0, len(byTopic))
	for name := range byTopic {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ms := byTopic[name]
		for i := 0; i < counts[name]; i++ {
			staged[ms[i%len(ms)]][tp{topic: name, partition: i}] = true
		}
	}
	for m, a := range staged {
		m.setAssignment(a)
	}
}

// countsLocked snapshots the partition count of every stream. The caller holds
// l.mu.
func (l *Log) countsLocked() map[string]int {
	counts := make(map[string]int, len(l.topics))
	for name, t := range l.topics {
		counts[name] = t.stream.Partitions
	}
	return counts
}

func (l *Log) joinGroup(id string, m *member) (*group, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, eventbus.ErrClosed
	}
	g, ok := l.groups[id]
	if !ok {
		g = &group{id: id}
		l.groups[id] = g
	}
	g.joinLocked(m, l.countsLocked())
	return g, nil
}

func (l *Log) leaveGroup(id string, m *member) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, ok := l.groups[id]
	if !ok {
		return
	}
	g.leaveLocked(m, l.countsLocked())
}

// Consumer is a handle on a consumer group. Every Run call on it is a separate
// member, so a service scales its consumption by calling Run N times rather
// than by creating N consumers.
type Consumer struct {
	log    *Log
	opts   eventbus.SubscribeOptions
	policy retry.Policy

	mu      sync.Mutex
	closed  bool
	nextRun uint64
	cancels map[uint64]context.CancelFunc
}

var _ eventbus.Consumer = (*Consumer)(nil)

// Subscribe creates a consumer for a group.
//
// Every referenced stream — the subscribed topics and the dead-letter topic —
// must already exist. Refusing to start a consumer whose dead-letter stream is
// missing is deliberate: the alternative is discovering the typo at 3am, when
// the first poison record arrives and has nowhere to go.
func (l *Log) Subscribe(opts eventbus.SubscribeOptions) (eventbus.Consumer, error) {
	opts = opts.WithDefaults()
	if opts.Group == "" {
		return nil, errors.New("eventlog: subscribe requires a group id")
	}
	if len(opts.Topics) == 0 {
		return nil, errors.New("eventlog: subscribe requires at least one topic")
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}

	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return nil, eventbus.ErrClosed
	}
	for _, name := range opts.Topics {
		if l.topicByName(name) == nil {
			return nil, fmt.Errorf("%w: %q", eventbus.ErrNoTopic, name)
		}
	}
	if l.topicByName(opts.DLQTopic) == nil {
		return nil, fmt.Errorf("%w: dead-letter stream %q", eventbus.ErrNoTopic, opts.DLQTopic)
	}

	maxDelay := retry.Default.Max
	if d := opts.RetryBackoff * 16; d > maxDelay {
		maxDelay = d
	}
	topics := make([]string, len(opts.Topics))
	copy(topics, opts.Topics)
	opts.Topics = topics

	return &Consumer{
		log:  l,
		opts: opts,
		policy: retry.Policy{
			// MaxAttempts counts the first try, so MaxRetries genuinely means
			// retries: a record is delivered at most 1+MaxRetries times before
			// it is dead-lettered.
			MaxAttempts: opts.MaxRetries + 1,
			Base:        opts.RetryBackoff,
			Max:         maxDelay,
			Multiplier:  2,
			Jitter:      true,
		},
		cancels: make(map[uint64]context.CancelFunc),
	}, nil
}

// Run joins the group as a member and delivers records until ctx is cancelled,
// the consumer is closed, or the log is closed.
//
// Calling Run n times splits the group's partitions n ways and rebalances on
// every join and departure. It returns ctx.Err() on cancellation and
// eventbus.ErrClosed if the log or the consumer shuts down underneath it.
func (c *Consumer) Run(ctx context.Context, h eventbus.Handler) error {
	if h == nil {
		return errors.New("eventlog: Run requires a handler")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return eventbus.ErrClosed
	}
	id := c.nextRun
	c.nextRun++
	c.cancels[id] = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.cancels, id)
		c.mu.Unlock()
	}()

	m := &member{topics: c.opts.Topics, owned: make(map[tp]bool), notify: make(chan struct{}, 1)}
	g, err := c.log.joinGroup(c.opts.Group, m)
	if err != nil {
		return err
	}
	defer c.log.leaveGroup(c.opts.Group, m)

	workers := make(map[tp]*worker)
	defer func() {
		for t, w := range workers {
			w.stop()
			delete(workers, t)
		}
	}()

	for {
		want := m.assignment()
		for t, w := range workers {
			if !want[t] {
				w.stop()
				delete(workers, t)
			}
		}
		for t := range want {
			if _, running := workers[t]; running {
				continue
			}
			p, ok := c.log.partitionOf(t)
			if !ok {
				continue
			}
			w := newWorker(ctx, g, t, p)
			go c.runPartition(w, h)
			workers[t] = w
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.log.done:
			return eventbus.ErrClosed
		case <-m.notify:
		}
	}
}

// partitionOf resolves a topic-partition to its partition, reporting false if
// the stream vanished (only possible if the log was closed).
func (l *Log) partitionOf(t tp) (*partition, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	topic := l.topicByName(t.topic)
	if topic == nil || t.partition < 0 || t.partition >= len(topic.parts) {
		return nil, false
	}
	return topic.parts[t.partition], true
}

// worker owns one partition for as long as this member is assigned it.
type worker struct {
	tp     tp
	g      *group
	p      *partition
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func newWorker(parent context.Context, g *group, t tp, p *partition) *worker {
	ctx, cancel := context.WithCancel(parent)
	return &worker{tp: t, g: g, p: p, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// stop revokes the partition and waits for the in-flight handler to finish, so
// that a rebalance never has two members inside the same partition.
func (w *worker) stop() {
	w.cancel()
	<-w.done
}

// runPartition is the delivery loop for one owned partition.
func (c *Consumer) runPartition(w *worker, h eventbus.Handler) {
	defer close(w.done)

	// Wait for the previous owner to let go before touching the partition.
	claim := w.g.claim(w.tp)
	select {
	case claim <- struct{}{}:
	case <-w.ctx.Done():
		return
	}
	defer func() { <-claim }()

	group := c.opts.Group
	pos := c.log.offsets.resolveStart(group, w.tp, func() int64 {
		if c.opts.FromBeginning {
			return w.p.base.Load()
		}
		return w.p.next.Load()
	})

	// Go 1.23 timer semantics make Stop-then-Reset safe without draining: a
	// stopped timer can no longer deliver a stale value.
	idle := time.NewTimer(idlePollInterval)
	defer idle.Stop()

	for {
		if w.ctx.Err() != nil {
			return
		}
		// Take the wake-up channel before reading, so an append that lands
		// between the read and the sleep still wakes us.
		wake := w.p.waitCh()
		recs, next, err := c.log.readPartition(w.p, pos, c.log.opts.readBatch)
		if err != nil {
			if errors.Is(err, eventbus.ErrClosed) {
				return
			}
			c.log.lg.Error("eventlog: reading partition",
				"topic", w.tp.topic, "partition", w.tp.partition, "offset", pos, "error", err)
		}

		if len(recs) == 0 {
			if next > pos {
				// The poll landed entirely in compaction gaps; skip them so the
				// consumer makes progress rather than re-reading them forever.
				pos = next
				c.log.offsets.commit(group, w.tp, pos)
				continue
			}
			idle.Stop()
			idle.Reset(idlePollInterval)
			select {
			case <-w.ctx.Done():
				return
			case <-wake:
			case <-idle.C:
			}
			continue
		}

		for i := 0; i < len(recs); i += c.opts.Concurrency {
			end := min(i+c.opts.Concurrency, len(recs))
			if !c.deliverChunk(w, h, recs[i:end]) {
				// Cancelled part-way: leave the offset where it was and let the
				// next owner re-deliver. At-least-once, never at-most-once.
				return
			}
			pos = recs[end-1].offset + 1
			c.log.offsets.commit(group, w.tp, pos)
		}
		if next > pos {
			pos = next
			c.log.offsets.commit(group, w.tp, pos)
		}
		c.log.m.setLag(group, w.tp.topic, w.tp.partition, w.p.next.Load()-pos)
	}
}

// deliverChunk runs up to Concurrency handlers and reports whether all of them
// reached a terminal state (handled or dead-lettered) rather than being
// cancelled.
//
// Offsets advance only once the whole chunk is terminal, which is what makes
// Concurrency > 1 safe to commit: with handlers finishing out of order there is
// no single "latest processed" record, so the batch boundary is the only
// honest commit point. The cost is the one documented on SubscribeOptions —
// ordering within the partition is forfeited, so only order-insensitive
// consumers (telemetry, analytics, audit forwarding) should raise it.
func (c *Consumer) deliverChunk(w *worker, h eventbus.Handler, recs []record) bool {
	if len(recs) == 1 {
		return c.deliverOne(w, h, recs[0])
	}
	var wg sync.WaitGroup
	var ok atomic.Bool
	ok.Store(true)
	for i := range recs {
		wg.Add(1)
		go func(r record) {
			defer wg.Done()
			if !c.deliverOne(w, h, r) {
				ok.Store(false)
			}
		}(recs[i])
	}
	wg.Wait()
	return ok.Load()
}

// deliverOne invokes the handler with backoff and dead-letters on exhaustion.
// It returns false only if the context was cancelled before the record reached
// a terminal state.
func (c *Consumer) deliverOne(w *worker, h eventbus.Handler, rec record) bool {
	err := retry.Do(w.ctx, c.policy, func(ctx context.Context, attempt int) error {
		if attempt > 1 {
			c.log.m.retried(w.tp.topic, c.opts.Group)
		}
		start := time.Now()
		herr := h(ctx, c.message(w.tp, rec, attempt-1))
		c.log.m.observeHandler(w.tp.topic, c.opts.Group, time.Since(start).Seconds())
		return herr
	})
	if err == nil {
		return true
	}
	if w.ctx.Err() != nil {
		return false
	}
	c.deadLetter(w, rec, err)
	return true
}

// message converts a stored record into the port's Message.
//
// Headers are copied per delivery: the handler owns the map it is given, and
// sharing one across retries would let a handler that stashes it observe a
// later attempt's retry count. HeaderRetryCount describes this delivery, not
// the record, so it is stamped here and overwrites any stored value of the
// same name.
func (c *Consumer) message(t tp, rec record, retries int) eventbus.Message {
	headers := make(map[string]string, len(rec.headers)+1)
	for k, v := range rec.headers {
		headers[k] = v
	}
	headers[eventbus.HeaderRetryCount] = strconv.Itoa(retries)
	return eventbus.Message{
		Topic:     t.topic,
		Key:       string(rec.key),
		Value:     rec.value,
		Headers:   headers,
		Partition: t.partition,
		Offset:    rec.offset,
		Timestamp: time.Unix(0, rec.timestamp),
	}
}

// deadLetter routes a record that exhausted its retries to the dead-letter
// stream and lets the caller commit past it.
//
// If the dead-letter write itself fails after its own retries the record is
// dropped, loudly, and the offset still advances. That is the least-bad option:
// blocking here would wedge a partition — and with it every key that hashes to
// it — on one bad record, which is the exact failure the dead-letter stream
// exists to prevent.
func (c *Consumer) deadLetter(w *worker, rec record, cause error) {
	reason := cause.Error()
	if len(reason) > dlqReasonLimit {
		reason = reason[:dlqReasonLimit]
	}
	headers := make(map[string]string, len(rec.headers)+2)
	for k, v := range rec.headers {
		headers[k] = v
	}
	headers[eventbus.HeaderDLQReason] = reason
	headers[eventbus.HeaderDLQOrigin] = w.tp.topic

	msg := eventbus.Message{
		Topic:     c.opts.DLQTopic,
		Key:       string(rec.key),
		Value:     rec.value,
		Headers:   headers,
		Timestamp: time.Unix(0, rec.timestamp),
	}
	err := retry.Do(w.ctx, retry.Aggressive, func(ctx context.Context, _ int) error {
		return c.log.Publish(ctx, msg)
	})
	if err != nil {
		c.log.lg.Error("eventlog: dead-letter write failed, record dropped",
			"topic", w.tp.topic, "partition", w.tp.partition, "offset", rec.offset,
			"group", c.opts.Group, "cause", reason, "error", err)
		c.log.m.deadLettered(w.tp.topic, c.opts.Group, false)
		return
	}
	c.log.m.deadLettered(w.tp.topic, c.opts.Group, true)
}

// Lag reports un-consumed records per partition across every subscribed topic.
//
// It is computed from in-memory counters — the partition's next offset minus
// the group's committed offset — so the autoscaler and the OTA service can call
// it on a tight loop without touching the disk. Note that the map is keyed by
// partition number alone, as the port requires: a consumer subscribed to
// several streams gets the sum across them for each partition index.
func (c *Consumer) Lag(ctx context.Context) (map[int]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l := c.log
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return nil, eventbus.ErrClosed
	}
	out := make(map[int]int64)
	for _, name := range c.opts.Topics {
		t := l.topicByName(name)
		if t == nil {
			return nil, fmt.Errorf("%w: %q", eventbus.ErrNoTopic, name)
		}
		for _, p := range t.parts {
			end := p.next.Load()
			pos, ok := l.offsets.committed(c.opts.Group, tp{topic: name, partition: p.id})
			if !ok {
				// A group that has never seen this partition will start where
				// its FromBeginning setting says, so that is where its lag is
				// measured from.
				if c.opts.FromBeginning {
					pos = p.base.Load()
				} else {
					pos = end
				}
			}
			lag := end - pos
			if lag < 0 {
				lag = 0
			}
			out[p.id] += lag
			l.m.setLag(c.opts.Group, name, p.id, lag)
		}
	}
	return out, nil
}

// Close stops every Run started from this consumer. It does not wait for them:
// the caller owns those goroutines and is the only one that can join them.
// Committed offsets are unaffected, so a re-Subscribe resumes where this one
// stopped.
func (c *Consumer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cancels := make([]context.CancelFunc, 0, len(c.cancels))
	for _, cancel := range c.cancels {
		cancels = append(cancels, cancel)
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return nil
}

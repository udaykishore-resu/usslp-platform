package mqtt

import (
	"strings"
	"sync"

	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// Wildcard characters, named so the matching code reads as the spec does.
const (
	wildcardSingle = "+"
	wildcardMulti  = "#"
	levelSeparator = "/"
)

// ValidTopicName reports whether s may be published to. Publication topics are
// concrete: a wildcard in a PUBLISH would mean a POS integration could write to
// every tenant at once, so it is rejected at the wire boundary rather than left
// to the ACL.
func ValidTopicName(s string) bool {
	if s == "" || len(s) > 65535 || !validUTF8(s) {
		return false
	}
	return !strings.ContainsAny(s, "+#")
}

// ValidTopicFilter reports whether s may be subscribed to. It enforces the two
// structural rules of MQTT 3.1.1 section 4.7.1: '#' occupies a whole level and
// must be the last one, and '+' occupies a whole level.
func ValidTopicFilter(s string) bool {
	if s == "" || len(s) > 65535 || !validUTF8(s) {
		return false
	}
	levels := strings.Split(s, levelSeparator)
	for i, l := range levels {
		switch {
		case l == wildcardMulti:
			if i != len(levels)-1 {
				return false
			}
		case l == wildcardSingle:
			// A '+' level is always legal, wherever it sits.
		case strings.ContainsAny(l, "+#"):
			// "sport/tennis#" and "sport/+x" are both malformed: a wildcard
			// character may not share a level with anything else.
			return false
		}
	}
	return true
}

// MatchTopic reports whether filter matches topic, following MQTT 3.1.1
// section 4.7. The subtleties that this implements, and that the table-driven
// test pins down:
//
//   - "sport/#" matches the parent level "sport" itself, because '#' matches
//     zero or more levels.
//   - "+" matches exactly one level, never several, so "sport/+" does not match
//     "sport/tennis/player1".
//   - A leading '$' level is not matched by a leading wildcard, which keeps a
//     tenant's "usslp/acme/#" subscription from silently harvesting broker
//     internals should any ever be published.
//
// The broker routes through the trie below rather than this function; this is
// the reference the trie is tested against and what the client uses to dispatch
// an inbound message to its handlers.
func MatchTopic(filter, topic string) bool {
	if filter == "" || topic == "" {
		return false
	}
	f := strings.Split(filter, levelSeparator)
	t := strings.Split(topic, levelSeparator)
	if strings.HasPrefix(t[0], "$") && (f[0] == wildcardMulti || f[0] == wildcardSingle) {
		return false
	}
	return matchLevels(f, t)
}

func matchLevels(f, t []string) bool {
	for i := 0; i < len(f); i++ {
		if f[i] == wildcardMulti {
			return true
		}
		if i >= len(t) {
			// "sport/tennis/+" does not match "sport/tennis": the '+' level
			// still has to exist.
			return false
		}
		if f[i] != wildcardSingle && f[i] != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}

// ---------------------------------------------------------------------------
// Subscription trie
// ---------------------------------------------------------------------------

// subTrie indexes subscriptions by topic level. A store gateway holds thousands
// of filters — one per SEC zone plus per-label subscriptions — and every price
// update must be routed in the time it takes to walk the levels of one topic,
// not in time proportional to the number of subscribers. Walking a trie makes
// routing O(levels) plus the number of actual matches; a linear scan over
// filters would make a 40,000-label store quadratic on its busiest path.
type subTrie struct {
	mu   sync.RWMutex
	root *subNode
}

type subNode struct {
	children map[string]*subNode
	// subscribers maps client ID to the QoS granted for the filter that ends at
	// this node. Keying by client means a re-SUBSCRIBE to the same filter
	// replaces the grant rather than duplicating delivery, as the spec requires.
	subscribers map[string]msgbus.QoS
}

func newSubTrie() *subTrie { return &subTrie{root: &subNode{}} }

func (n *subNode) child(level string, create bool) *subNode {
	if n.children == nil {
		if !create {
			return nil
		}
		n.children = make(map[string]*subNode, 4)
	}
	c, ok := n.children[level]
	if !ok {
		if !create {
			return nil
		}
		c = &subNode{}
		n.children[level] = c
	}
	return c
}

// Subscribe records a grant, replacing any previous grant by the same client
// for the same filter.
func (t *subTrie) Subscribe(filter, clientID string, qos msgbus.QoS) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.root
	for _, level := range strings.Split(filter, levelSeparator) {
		n = n.child(level, true)
	}
	if n.subscribers == nil {
		n.subscribers = make(map[string]msgbus.QoS, 1)
	}
	n.subscribers[clientID] = qos
}

// Unsubscribe removes one grant and reports whether it existed. Emptied nodes
// are pruned so that a gateway which churns through per-label subscriptions for
// a year does not accumulate a trie of dead levels.
func (t *subTrie) Unsubscribe(filter, clientID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.remove(t.root, strings.Split(filter, levelSeparator), clientID)
}

func (t *subTrie) remove(n *subNode, levels []string, clientID string) bool {
	if len(levels) == 0 {
		if n.subscribers == nil {
			return false
		}
		_, ok := n.subscribers[clientID]
		delete(n.subscribers, clientID)
		return ok
	}
	c := n.child(levels[0], false)
	if c == nil {
		return false
	}
	removed := t.remove(c, levels[1:], clientID)
	if len(c.children) == 0 && len(c.subscribers) == 0 {
		delete(n.children, levels[0])
	}
	return removed
}

// UnsubscribeAll drops every grant held by one client, which is what a
// CleanSession=true disconnect and a session takeover both need.
func (t *subTrie) UnsubscribeAll(clientID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeAll(t.root, clientID)
}

func (t *subTrie) removeAll(n *subNode, clientID string) {
	delete(n.subscribers, clientID)
	for level, c := range n.children {
		t.removeAll(c, clientID)
		if len(c.children) == 0 && len(c.subscribers) == 0 {
			delete(n.children, level)
		}
	}
}

// Match returns the maximum QoS granted to each client whose subscriptions
// match topic. Overlapping filters collapse to one entry at the highest granted
// QoS: MQTT 3.1.1 permits either behaviour, and delivering once is what a shelf
// label needs — a SEC subscribed to both "…/labels/+/price" and
// "…/labels/L1/price" must not redraw its display twice.
func (t *subTrie) Match(topic string) map[string]msgbus.QoS {
	levels := strings.Split(topic, levelSeparator)
	out := make(map[string]msgbus.QoS)
	t.mu.RLock()
	defer t.mu.RUnlock()
	// Topics beginning with '$' are reserved; a wildcard at the root level does
	// not reach them.
	dollar := strings.HasPrefix(levels[0], "$")
	t.match(t.root, levels, out, dollar)
	return out
}

func (t *subTrie) match(n *subNode, levels []string, out map[string]msgbus.QoS, rootIsDollar bool) {
	if n == nil {
		return
	}
	if len(levels) == 0 {
		collect(n.subscribers, out)
		// "sport/#" matches "sport": the '#' child of the final node counts as a
		// match even though there are no levels left to consume.
		if c := n.child(wildcardMulti, false); c != nil {
			collect(c.subscribers, out)
		}
		return
	}
	if !rootIsDollar {
		if c := n.child(wildcardMulti, false); c != nil {
			collect(c.subscribers, out)
		}
		if c := n.child(wildcardSingle, false); c != nil {
			t.match(c, levels[1:], out, false)
		}
	}
	if c := n.child(levels[0], false); c != nil {
		t.match(c, levels[1:], out, false)
	}
}

func collect(subs map[string]msgbus.QoS, out map[string]msgbus.QoS) {
	for id, q := range subs {
		if prev, ok := out[id]; !ok || q > prev {
			out[id] = q
		}
	}
}

// ---------------------------------------------------------------------------
// Retained message store
// ---------------------------------------------------------------------------

// retainedMessage is the last-known value of one topic.
type retainedMessage struct {
	Topic   string
	Payload []byte
	QoS     msgbus.QoS
}

// retainStore holds one retained message per topic in a trie keyed the same way
// as subscriptions. It is a trie rather than a map because the operation that
// matters is the wildcard one: when a SEC reboots after a power cut and
// subscribes to "usslp/t/r/s/labels/+/price", the broker must find that zone's
// retained prices without scanning every retained topic in the store.
type retainStore struct {
	mu    sync.RWMutex
	root  *retainNode
	count int
}

type retainNode struct {
	children map[string]*retainNode
	msg      *retainedMessage
}

func newRetainStore() *retainStore { return &retainStore{root: &retainNode{}} }

func (n *retainNode) child(level string, create bool) *retainNode {
	if n.children == nil {
		if !create {
			return nil
		}
		n.children = make(map[string]*retainNode, 4)
	}
	c, ok := n.children[level]
	if !ok {
		if !create {
			return nil
		}
		c = &retainNode{}
		n.children[level] = c
	}
	return c
}

// Store sets or clears the retained value of a topic. A zero-length payload
// clears it, per the specification — that is how USSLP decommissions a label:
// publish an empty retained message to its price topic and no future subscriber
// will be told about a shelf that no longer exists. Returns the new total.
func (s *retainStore) Store(m retainedMessage) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	levels := strings.Split(m.Topic, levelSeparator)
	if len(m.Payload) == 0 {
		s.clear(s.root, levels)
		return s.count
	}
	n := s.root
	for _, level := range levels {
		n = n.child(level, true)
	}
	if n.msg == nil {
		s.count++
	}
	cp := m
	n.msg = &cp
	return s.count
}

func (s *retainStore) clear(n *retainNode, levels []string) {
	if len(levels) == 0 {
		if n.msg != nil {
			n.msg = nil
			s.count--
		}
		return
	}
	c := n.child(levels[0], false)
	if c == nil {
		return
	}
	s.clear(c, levels[1:])
	if c.msg == nil && len(c.children) == 0 {
		delete(n.children, levels[0])
	}
}

// Match returns every retained message whose topic matches filter, which is
// what a new subscription is served with.
func (s *retainStore) Match(filter string) []retainedMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []retainedMessage
	levels := strings.Split(filter, levelSeparator)
	s.walk(s.root, levels, true, &out)
	return out
}

// walk descends the retained trie consuming filter levels. atRoot carries the
// '$' rule: a wildcard in the first filter level does not match a reserved
// topic.
func (s *retainStore) walk(n *retainNode, levels []string, atRoot bool, out *[]retainedMessage) {
	if n == nil {
		return
	}
	if len(levels) == 0 {
		if n.msg != nil {
			*out = append(*out, *n.msg)
		}
		return
	}
	switch levels[0] {
	case wildcardMulti:
		// '#' matches this level and everything below it, including the node the
		// filter's parent landed on.
		s.collectSubtree(n, atRoot, out)
	case wildcardSingle:
		for level, c := range n.children {
			if atRoot && strings.HasPrefix(level, "$") {
				continue
			}
			s.walk(c, levels[1:], false, out)
		}
	default:
		s.walk(n.child(levels[0], false), levels[1:], false, out)
	}
}

func (s *retainStore) collectSubtree(n *retainNode, atRoot bool, out *[]retainedMessage) {
	if n.msg != nil {
		*out = append(*out, *n.msg)
	}
	for level, c := range n.children {
		if atRoot && strings.HasPrefix(level, "$") {
			continue
		}
		s.collectSubtree(c, false, out)
	}
}

// Count reports how many topics currently hold a retained value, for the gauge
// an operator watches to spot a runaway publisher retaining per-transaction
// topics.
func (s *retainStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.count
}

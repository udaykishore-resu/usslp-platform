package mqtt

import (
	"sort"
	"testing"

	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// wildcardCases is the specification's own matching table plus the USSLP topics
// it has to get right. Both MatchTopic and the broker's routing trie are run
// against it, so the fast path and the reference can never disagree.
var wildcardCases = []struct {
	filter string
	topic  string
	match  bool
}{
	// Exact names.
	{"sport/tennis/player1", "sport/tennis/player1", true},
	{"sport/tennis/player1", "sport/tennis/player2", false},
	{"sport", "sport", true},
	{"sport", "sport/tennis", false},

	// Multi-level '#'.
	{"sport/tennis/player1/#", "sport/tennis/player1", true},
	{"sport/tennis/player1/#", "sport/tennis/player1/ranking", true},
	{"sport/tennis/player1/#", "sport/tennis/player1/score/wimbledon", true},
	{"sport/#", "sport", true},
	{"sport/#", "sport/tennis", true},
	{"#", "sport/tennis/player1", true},
	{"#", "a", true},
	{"sport/#", "sports/tennis", false},

	// Single-level '+'.
	{"sport/tennis/+", "sport/tennis/player1", true},
	{"sport/tennis/+", "sport/tennis/player1/ranking", false},
	{"sport/tennis/+", "sport/tennis", false},
	{"sport/+", "sport", false},
	{"sport/+", "sport/", true},
	{"+/tennis/#", "sport/tennis/player1", true},
	{"+", "sport", true},
	{"+", "sport/tennis", false},
	{"+/+", "/finance", true},
	{"/+", "/finance", true},
	{"+", "/finance", false},

	// Reserved topics are not swept up by a leading wildcard.
	{"#", "$SYS/broker/uptime", false},
	{"+/broker/uptime", "$SYS/broker/uptime", false},
	{"$SYS/#", "$SYS/broker/uptime", true},

	// USSLP namespace.
	{"usslp/acme/eu-west-1/store-7/labels/+/price",
		"usslp/acme/eu-west-1/store-7/labels/L-0001/price", true},
	{"usslp/acme/eu-west-1/store-7/labels/+/price",
		"usslp/acme/eu-west-1/store-7/labels/L-0001/display", false},
	{"usslp/acme/#", "usslp/acme/eu-west-1/store-7/sec/S1/heartbeat", true},
	{"usslp/acme/#", "usslp/globex/eu-west-1/store-7/sec/S1/heartbeat", false},
}

func TestMatchTopic(t *testing.T) {
	for _, tc := range wildcardCases {
		if got := MatchTopic(tc.filter, tc.topic); got != tc.match {
			t.Errorf("MatchTopic(%q, %q) = %v, want %v", tc.filter, tc.topic, got, tc.match)
		}
	}
}

// TestSubTrieMatchesReference pins the routing trie to MatchTopic. The trie is
// the code that actually runs on the price hot path, and the only guarantee
// worth having is that it agrees with the reference on every case.
func TestSubTrieMatchesReference(t *testing.T) {
	for _, tc := range wildcardCases {
		trie := newSubTrie()
		trie.Subscribe(tc.filter, "sec-1", msgbus.AtLeastOnce)
		_, got := trie.Match(tc.topic)["sec-1"]
		if got != tc.match {
			t.Errorf("trie.Match(%q) for filter %q = %v, want %v", tc.topic, tc.filter, got, tc.match)
		}
	}
}

func TestValidTopicFilter(t *testing.T) {
	cases := []struct {
		filter string
		valid  bool
	}{
		{"sport/tennis/#", true},
		{"#", true},
		{"+", true},
		{"+/tennis/#", true},
		{"sport/+/player1", true},
		{"/", true},
		{"", false},
		{"sport/tennis#", false},
		{"sport/tennis/#/ranking", false},
		{"sport/#/player", false},
		{"sport+", false},
		{"sport/+player", false},
		{"sp+ort/x", false},
	}
	for _, tc := range cases {
		if got := ValidTopicFilter(tc.filter); got != tc.valid {
			t.Errorf("ValidTopicFilter(%q) = %v, want %v", tc.filter, got, tc.valid)
		}
	}
}

func TestValidTopicName(t *testing.T) {
	cases := []struct {
		topic string
		valid bool
	}{
		{"usslp/acme/eu-west-1/store-7/labels/L1/price", true},
		{"/", true},
		{"a/b/", true},
		{"", false},
		{"a/#", false},
		{"a/+/b", false},
	}
	for _, tc := range cases {
		if got := ValidTopicName(tc.topic); got != tc.valid {
			t.Errorf("ValidTopicName(%q) = %v, want %v", tc.topic, got, tc.valid)
		}
	}
}

func TestSubTrieOverlappingFiltersCollapseToHighestQoS(t *testing.T) {
	trie := newSubTrie()
	trie.Subscribe("usslp/acme/r/s/labels/+/price", "sec-1", msgbus.AtMostOnce)
	trie.Subscribe("usslp/acme/#", "sec-1", msgbus.ExactlyOnce)
	trie.Subscribe("usslp/acme/r/s/labels/L1/price", "sec-1", msgbus.AtLeastOnce)

	got := trie.Match("usslp/acme/r/s/labels/L1/price")
	if len(got) != 1 {
		t.Fatalf("three overlapping filters produced %d deliveries, want 1", len(got))
	}
	if got["sec-1"] != msgbus.ExactlyOnce {
		t.Errorf("granted QoS %d, want the highest of the matching subscriptions (2)", got["sec-1"])
	}
}

func TestSubTrieUnsubscribe(t *testing.T) {
	trie := newSubTrie()
	trie.Subscribe("a/+/c", "c1", msgbus.AtLeastOnce)
	trie.Subscribe("a/+/c", "c2", msgbus.AtLeastOnce)

	if !trie.Unsubscribe("a/+/c", "c1") {
		t.Fatal("Unsubscribe reported no such subscription")
	}
	if trie.Unsubscribe("a/+/c", "c1") {
		t.Fatal("Unsubscribe reported a second removal of the same subscription")
	}
	got := trie.Match("a/b/c")
	if _, still := got["c1"]; still {
		t.Error("c1 still matches after unsubscribing")
	}
	if _, ok := got["c2"]; !ok {
		t.Error("c2 lost its subscription when c1 unsubscribed")
	}

	trie.UnsubscribeAll("c2")
	if len(trie.Match("a/b/c")) != 0 {
		t.Error("UnsubscribeAll left subscriptions behind")
	}
	if len(trie.root.children) != 0 {
		t.Errorf("emptied trie kept %d root children; nodes are not being pruned", len(trie.root.children))
	}
}

func TestRetainStore(t *testing.T) {
	s := newRetainStore()
	s.Store(retainedMessage{Topic: "usslp/acme/r/s/labels/L1/price", Payload: []byte("399"), QoS: msgbus.AtLeastOnce})
	s.Store(retainedMessage{Topic: "usslp/acme/r/s/labels/L2/price", Payload: []byte("499"), QoS: msgbus.AtLeastOnce})
	s.Store(retainedMessage{Topic: "usslp/acme/r/s/labels/L1/display", Payload: []byte("ok"), QoS: msgbus.AtMostOnce})
	s.Store(retainedMessage{Topic: "usslp/globex/r/s/labels/L1/price", Payload: []byte("999"), QoS: msgbus.AtLeastOnce})
	if s.Count() != 4 {
		t.Fatalf("stored 4 retained topics, Count reports %d", s.Count())
	}

	cases := []struct {
		filter string
		want   []string
	}{
		{"usslp/acme/r/s/labels/+/price", []string{
			"usslp/acme/r/s/labels/L1/price", "usslp/acme/r/s/labels/L2/price"}},
		{"usslp/acme/#", []string{
			"usslp/acme/r/s/labels/L1/display", "usslp/acme/r/s/labels/L1/price",
			"usslp/acme/r/s/labels/L2/price"}},
		{"usslp/acme/r/s/labels/L1/price", []string{"usslp/acme/r/s/labels/L1/price"}},
		{"usslp/nobody/#", nil},
	}
	for _, tc := range cases {
		var got []string
		for _, m := range s.Match(tc.filter) {
			got = append(got, m.Topic)
		}
		sort.Strings(got)
		if len(got) != len(tc.want) {
			t.Errorf("Match(%q) returned %v, want %v", tc.filter, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("Match(%q) returned %v, want %v", tc.filter, got, tc.want)
				break
			}
		}
	}

	// A zero-length retained publish clears the topic: this is how a
	// decommissioned label stops being announced to future subscribers.
	s.Store(retainedMessage{Topic: "usslp/acme/r/s/labels/L1/price"})
	if got := s.Match("usslp/acme/r/s/labels/L1/price"); len(got) != 0 {
		t.Errorf("retained message survived a zero-length publish: %v", got)
	}
	if s.Count() != 3 {
		t.Errorf("Count after clearing one topic is %d, want 3", s.Count())
	}
}

func TestRetainStoreParentMatchOnMultiLevel(t *testing.T) {
	// "sport/#" must match the retained value of "sport" itself, the same rule
	// the subscription trie implements.
	s := newRetainStore()
	s.Store(retainedMessage{Topic: "sport", Payload: []byte("x")})
	if got := s.Match("sport/#"); len(got) != 1 {
		t.Fatalf(`Match("sport/#") returned %d retained messages, want 1`, len(got))
	}
}

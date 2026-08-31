package adapters

import (
	"context"
	"errors"
	"testing"

	"github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// fakeClient records publishes without a broker. The broker interaction itself
// is tested against the real in-process broker in the parent package; what is
// under test here is the topic, QoS and retain flag this adapter chooses.
type fakeClient struct {
	sent      []msgbus.Message
	connected bool
	err       error
}

func (c *fakeClient) Publish(_ context.Context, m msgbus.Message) error {
	if c.err != nil {
		return c.err
	}
	c.sent = append(c.sent, m)
	return nil
}

func (c *fakeClient) Subscribe(context.Context, string, msgbus.QoS, msgbus.Handler) error { return nil }
func (c *fakeClient) Unsubscribe(context.Context, string) error                           { return nil }
func (c *fakeClient) Connected() bool                                                     { return c.connected }
func (c *fakeClient) Close() error                                                        { return nil }

func TestDevicePublisherTopicAndDelivery(t *testing.T) {
	client := &fakeClient{connected: true}
	p, err := NewMQTTDevicePublisher(client)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	env, err := canon.NewEnvelope(canon.EvtPriceUpdated, "label", "lbl-1", "acme",
		canon.PriceUpdated{LabelID: "lbl-1", SKU: "sku-milk", Price: canon.NewMoney(279, "USD")})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	target := ports.Placement{
		LabelID: "lbl-1", SECID: "sec-07", TenantID: "acme",
		StoreID: "store-01", Region: "us-east-1",
	}
	if err := p.PublishPrice(context.Background(), target, env); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(client.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(client.sent))
	}
	m := client.sent[0]
	want := "usslp/acme/us-east-1/store-01/sec/sec-07/labels/lbl-1/price"
	if m.Topic != want {
		t.Fatalf("topic = %q, want %q", m.Topic, want)
	}
	if m.QoS != msgbus.AtLeastOnce {
		t.Fatalf("QoS = %d, want 1: a lost price update is a compliance incident", m.QoS)
	}
	if !m.Retain {
		t.Fatalf("not retained: a controller rebooting after a power cut must recover its zone from the local broker")
	}
	if !p.Connected() {
		t.Fatalf("Connected must report the client's link state")
	}
}

func TestDevicePublisherRejectsAnUnroutableTarget(t *testing.T) {
	client := &fakeClient{connected: true}
	p, err := NewMQTTDevicePublisher(client)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	env, err := canon.NewEnvelope(canon.EvtPriceUpdated, "label", "lbl-1", "acme",
		canon.PriceUpdated{LabelID: "lbl-1"})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	tests := []struct {
		name   string
		target ports.Placement
	}{
		{"no controller", ports.Placement{LabelID: "lbl-1", TenantID: "acme", StoreID: "store-01"}},
		{"no store", ports.Placement{LabelID: "lbl-1", TenantID: "acme", SECID: "sec-07"}},
		{
			// A store id containing a wildcard would break out of the tenant's
			// namespace, which is the whole isolation boundary.
			"store id with a reserved character",
			ports.Placement{LabelID: "lbl-1", TenantID: "acme", StoreID: "store/#", SECID: "sec-07"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.PublishPrice(context.Background(), tc.target, env); err == nil {
				t.Fatalf("published to an unroutable target")
			}
		})
	}
	if len(client.sent) != 0 {
		t.Fatalf("an unroutable target reached the broker")
	}
}

func TestDevicePublisherSurfacesBrokerFailures(t *testing.T) {
	client := &fakeClient{err: errors.New("broker unreachable")}
	p, err := NewMQTTDevicePublisher(client)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	env, err := canon.NewEnvelope(canon.EvtPriceUpdated, "label", "lbl-1", "acme",
		canon.PriceUpdated{LabelID: "lbl-1"})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if err := p.PublishPrice(context.Background(), ports.Placement{
		LabelID: "lbl-1", SECID: "sec-07", TenantID: "acme", StoreID: "store-01",
	}, env); err == nil {
		t.Fatalf("a broker failure must be reported, not swallowed")
	}
}

// TestPartitionKeyPerStream covers the compaction hazard: canon's default key
// is derived from the payload and answers store:sku, which would collapse every
// facing of a product onto one row of the compacted label-state stream.
func TestPartitionKeyPerStream(t *testing.T) {
	env, err := canon.NewEnvelope(canon.EvtPriceUpdated, "label", "lbl-1", "acme",
		canon.PriceUpdated{LabelID: "lbl-1", SKU: "sku-milk", StoreID: "store-01"})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	env.StoreID = "store-01"

	tests := []struct {
		stream string
		want   string
		why    string
	}{
		{canon.StreamLabelState.Name, "lbl-1", "compacted per label"},
		{canon.StreamDelivery.Name, "lbl-1", "one label's confirmations stay ordered"},
		{canon.StreamAudit.Name, "acme", "one retailer's record is one ordered sequence"},
		{canon.StreamPriceUpdates.Name, "store-01:sku-milk", "two changes to one product in one store are ordered"},
	}
	for _, tc := range tests {
		t.Run(tc.stream, func(t *testing.T) {
			if got := partitionKeyFor(tc.stream, env); got != tc.want {
				t.Fatalf("key = %q, want %q (%s)", got, tc.want, tc.why)
			}
		})
	}
}

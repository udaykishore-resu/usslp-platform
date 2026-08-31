// Package msgbus defines the device-messaging port: the MQTT-shaped interface
// that carries updates from the cloud down to the Store Gateway Unit, from the
// SGU to each Shelf Edge Controller, and acknowledgements back up.
//
// It is deliberately narrower than MQTT itself. The platform uses publish,
// subscribe, QoS 0/1/2, retained messages and last-will — and nothing else —
// so the port stays small enough that pkg/mqtt (the in-tree broker and client
// used by `make dev` and the edge tier) and EMQX in production are genuinely
// interchangeable.
package msgbus

import (
	"context"
	"errors"
	"time"
)

// QoS levels, matching MQTT semantics.
type QoS byte

const (
	// AtMostOnce: fire and forget. Telemetry only.
	AtMostOnce QoS = 0
	// AtLeastOnce: acknowledged, may be duplicated. Every price update.
	// Duplicates are harmless because updates carry a monotonic sequence.
	AtLeastOnce QoS = 1
	// ExactlyOnce: four-way handshake. OTA triggers only, where a duplicate
	// costs a battery-powered device an entire redundant firmware download.
	ExactlyOnce QoS = 2
)

// Message is one MQTT publication.
type Message struct {
	Topic   string
	Payload []byte
	QoS     QoS
	// Retain asks the broker to keep this as the last-known value for the
	// topic and deliver it immediately to future subscribers. USSLP retains
	// label state so that a SEC rebooting after a power cut recovers the
	// current price of every label in its zone without asking the cloud.
	Retain bool
	// Duplicate is set by the broker on redelivery.
	Duplicate bool
	// ReceivedAt is set on inbound messages.
	ReceivedAt time.Time
}

// Handler receives a message. It must not block: the client dispatches on a
// bounded worker pool and a slow handler applies backpressure to the whole
// connection.
type Handler func(ctx context.Context, m Message)

var (
	// ErrNotConnected is returned when publishing while the link is down. The
	// SGU treats this as the trigger for autonomous store mode rather than an
	// error to surface.
	ErrNotConnected = errors.New("msgbus: not connected")
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("msgbus: closed")
	// ErrTimeout is returned when a QoS 1/2 acknowledgement does not arrive.
	ErrTimeout = errors.New("msgbus: acknowledgement timeout")
)

// Client is a connection to a broker.
type Client interface {
	// Publish sends a message, blocking until the QoS handshake completes for
	// QoS 1 and 2.
	Publish(ctx context.Context, m Message) error
	// Subscribe registers a handler for a topic filter, which may contain the
	// single-level '+' and multi-level '#' wildcards.
	Subscribe(ctx context.Context, filter string, qos QoS, h Handler) error
	// Unsubscribe removes a filter.
	Unsubscribe(ctx context.Context, filter string) error
	// Connected reports link state. The SGU polls this to decide when to enter
	// and leave autonomous mode.
	Connected() bool
	// Close disconnects cleanly, sending DISCONNECT so the broker suppresses
	// the last-will message.
	Close() error
}

// Will is a last-will-and-testament: the message the broker publishes on the
// client's behalf if the connection drops without a clean DISCONNECT. Every
// SEC registers one on its status topic, which is how the SGU learns about a
// controller failure in under 30 seconds without polling.
type Will struct {
	Topic   string
	Payload []byte
	QoS     QoS
	Retain  bool
}

// Config describes a client connection.
type Config struct {
	// BrokerURL is "tcp://host:port" or "tls://host:port".
	BrokerURL string
	// ClientID must be stable across reconnects so the broker can resume the
	// session and redeliver un-acknowledged QoS 1 messages.
	ClientID string
	Username string
	Password string
	// CleanSession false asks the broker to persist subscriptions and inflight
	// messages across disconnects. The edge tier always uses false: a SEC that
	// reboots mid-price-change must receive the update it missed.
	CleanSession bool
	KeepAlive    time.Duration
	// ConnectTimeout bounds the initial handshake.
	ConnectTimeout time.Duration
	// AckTimeout bounds a QoS 1/2 handshake.
	AckTimeout time.Duration
	Will       *Will
	// TLS enables mutual TLS. The edge tier authenticates with a SPIFFE SVID
	// or a device certificate; there are no password-only clients in
	// production.
	TLSConfig any
}

// WithDefaults fills unset fields.
func (c Config) WithDefaults() Config {
	if c.KeepAlive == 0 {
		c.KeepAlive = 30 * time.Second
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.AckTimeout == 0 {
		c.AckTimeout = 5 * time.Second
	}
	return c
}

// Package eventbus defines the durable event-streaming port of the platform.
//
// USSLP's cloud tier is event-driven end to end: no service mutates another
// service's state, it publishes a fact and lets interested parties project it.
// This package is the interface that decoupling hangs on.
//
// One implementation satisfies it today: pkg/eventlog, an embedded,
// file-backed, partitioned log with consumer groups and replay. It is what
// `make dev` runs, what the test suite runs against, and what a single-store
// deployment can run in production. Its one architectural limit is that
// consumer-group coordination lives in memory, so two OS processes must not
// share one log directory — which is why the multi-process compose profile
// gives each service its own log and the cross-service stream does not close.
//
// The second implementation — Apache Kafka (MSK / Confluent) as the
// multi-region production backbone — is **the documented production work and
// does not exist in this tree**. There is no pkg/eventbus/kafka and no build
// tag that would select one. What does exist is everything an adapter needs to
// slot into: this port, deploy/terraform's MSK module, the topic-provisioning
// Job that creates canon.AllStreams() with its real partition counts, and every
// caller already written against Bus rather than against eventlog.
//
// Everything above this port — Label Service, UIG, OTA, analytics — is written
// once and will run unchanged against either.
package eventbus

import (
	"context"
	"errors"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Message is one record on a stream.
type Message struct {
	// Topic is the stream name, e.g. "price-updates".
	Topic string
	// Key determines partition assignment and therefore ordering. Records with
	// the same key are always delivered to the same partition in the order they
	// were produced. USSLP keys price traffic by "store:sku".
	Key string
	// Value is the serialised canon.Envelope.
	Value []byte
	// Headers carry routing and trace metadata that a consumer can read without
	// deserialising the body — critical for the audit sink, which forwards
	// millions of records per minute without ever parsing one.
	Headers map[string]string

	// Assigned by the broker on read.
	Partition int
	Offset    int64
	Timestamp time.Time
}

// Header keys that the platform relies on being present.
const (
	HeaderEventType     = "usslp-event-type"
	HeaderTenantID      = "usslp-tenant-id"
	HeaderStoreID       = "usslp-store-id"
	HeaderTraceParent   = "traceparent"
	HeaderCorrelationID = "usslp-correlation-id"
	HeaderSchemaVersion = "usslp-schema-version"
	HeaderIdempotency   = "usslp-idempotency-key"
	HeaderRetryCount    = "usslp-retry-count"
	HeaderDLQReason     = "usslp-dlq-reason"
	HeaderDLQOrigin     = "usslp-dlq-origin-topic"
)

// ErrClosed is returned by a bus that has been shut down.
var ErrClosed = errors.New("eventbus: closed")

// ErrNoTopic is returned when publishing to a stream that was never created.
// Auto-creation is deliberately not supported: a typo in a topic name must fail
// loudly at deploy time, not silently create a stream nobody consumes.
var ErrNoTopic = errors.New("eventbus: unknown topic")

// Publisher writes records to streams.
type Publisher interface {
	// Publish appends records. It returns only once the records are durable
	// (fsync'd locally, or acks=all on Kafka). Partial success is reported as
	// an error with the records that failed; callers must treat the call as
	// all-or-nothing at the batch level and rely on idempotency keys for
	// safe retry.
	Publish(ctx context.Context, msgs ...Message) error
	Close() error
}

// Handler processes one record.
//
// Returning nil commits the offset. Returning an error causes the record to be
// retried with backoff up to the subscription's limit, after which it is routed
// to the dead-letter stream and the offset is committed — a poison record must
// never wedge a consumer group serving 1,024 partitions.
type Handler func(ctx context.Context, m Message) error

// Consumer reads records as a member of a consumer group.
type Consumer interface {
	// Run blocks, delivering records to h until ctx is cancelled. It is safe to
	// call Run from multiple goroutines: partitions are distributed between
	// members of the same group.
	Run(ctx context.Context, h Handler) error
	// Lag reports the number of un-consumed records per partition. The OTA
	// service and the autoscaler both key off this.
	Lag(ctx context.Context) (map[int]int64, error)
	Close() error
}

// SubscribeOptions tune a consumer group.
type SubscribeOptions struct {
	// Group is the consumer group id. Members of a group share partitions;
	// separate groups each see every record.
	Group string
	// Topics to consume.
	Topics []string
	// FromBeginning starts a brand-new group at offset 0 instead of at the tail.
	// Read-model rebuilds set this; live services do not.
	FromBeginning bool
	// MaxRetries before a record is dead-lettered.
	MaxRetries int
	// RetryBackoff is the base delay; the consumer applies exponential backoff
	// with jitter.
	RetryBackoff time.Duration
	// DLQTopic overrides the default dead-letter stream.
	DLQTopic string
	// Concurrency is the number of in-flight handler invocations per partition.
	// Above 1, ordering within a partition is no longer guaranteed, so only
	// order-insensitive consumers (telemetry, analytics) should raise it.
	Concurrency int
}

// WithDefaults fills unset fields with production-safe values.
func (o SubscribeOptions) WithDefaults() SubscribeOptions {
	if o.MaxRetries == 0 {
		o.MaxRetries = 5
	}
	if o.RetryBackoff == 0 {
		o.RetryBackoff = 50 * time.Millisecond
	}
	if o.DLQTopic == "" {
		o.DLQTopic = canon.StreamDLQ.Name
	}
	if o.Concurrency == 0 {
		o.Concurrency = 1
	}
	return o
}

// Bus is a complete event-streaming implementation.
type Bus interface {
	Publisher
	// Subscribe creates a consumer for a group.
	Subscribe(opts SubscribeOptions) (Consumer, error)
	// EnsureStreams creates any missing streams. It is idempotent and is called
	// on service start-up so a fresh environment self-provisions.
	EnsureStreams(ctx context.Context, streams ...canon.Stream) error
}

// PublishEnvelope is the helper every producer uses: it validates, serialises
// and attaches headers so no service hand-rolls that logic.
func PublishEnvelope(ctx context.Context, p Publisher, topic string, env canon.Envelope, body []byte) error {
	if err := env.Validate(); err != nil {
		return err
	}
	return p.Publish(ctx, Message{
		Topic: topic,
		Key:   env.PartitionKey(),
		Value: body,
		Headers: map[string]string{
			HeaderEventType:     env.EventType,
			HeaderTenantID:      string(env.TenantID),
			HeaderStoreID:       string(env.StoreID),
			HeaderCorrelationID: string(env.CorrelationID),
			HeaderSchemaVersion: "1",
			HeaderIdempotency:   env.IdempotencyKey,
		},
		Timestamp: env.RecordedAt,
	})
}

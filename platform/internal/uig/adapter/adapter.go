// Package adapter defines the protocol-adapter contract that makes USSLP
// universal.
//
// Every existing electronic-shelf-label vendor ships bespoke middleware per POS
// vendor, which is why a retailer on a custom or legacy ERP simply cannot buy
// ESLs. The UIG inverts that: one pipeline, one canonical event, and a small,
// sharply defined seam — this package — that a new source of price changes
// plugs into. Shopify's webhook, an Oracle Retail SOAP envelope, a SAP IDoc and
// a fixed-width AS/400 drop from 1994 all arrive at the rest of the platform as
// the same canon.PriceChangeRequested.
//
// The seam is four methods, and the split between them is the whole design:
//
//	Verify            is authentication, and runs first, on raw bytes.
//	IdempotencyParts  identifies the delivery, and runs before parsing, so a
//	                  redelivery is recognised without paying to parse it twice.
//	Ingest            is the parse and the normalisation.
//	Name              is the label on every metric, log line and event source.
//
// Verify and IdempotencyParts operate on the raw body because that is what the
// vendor signed and what the vendor's message id lives in. An adapter that
// needed to parse before it could authenticate would be parsing unauthenticated
// input on the price path, which is the wrong order to do those two things in.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Delivery is one inbound message from a POS: an HTTP request, a SOAP call, or
// a file the legacy poller picked up off a share.
//
// The body is the exact bytes as received, never re-encoded. HMAC signatures
// are computed over those bytes, and a JSON round-trip that reorders keys or
// normalises whitespace invalidates every signature the platform is asked to
// check. That is the single most common way a webhook integration is broken,
// so the type makes the raw form the primary representation and everything else
// derived from it.
type Delivery struct {
	// ID is the platform's identifier for this delivery, assigned at ingress.
	// It is what an operator quotes in a support ticket and what the replay
	// endpoint takes.
	ID string
	// TenantID and BindingID say whose integration this is. Both are resolved
	// from the URL before the adapter sees the delivery.
	TenantID  canon.TenantID
	BindingID string
	// Binding is the resolved configuration: credentials, store mapping,
	// currency defaults and any adapter-specific options.
	Binding *Binding
	// Method, URL, Path and Headers are the transport envelope. Square signs
	// the notification URL together with the body, so the URL is not optional
	// metadata — it is part of the signed message.
	Method  string
	URL     string
	Path    string
	Headers http.Header
	Query   url.Values
	// ContentType is the declared media type, which for NCR selects between the
	// XML and JSON encodings of the same message.
	ContentType string
	// Body is the raw, unmodified payload.
	Body []byte
	// ReceivedAt is when the UIG took the bytes off the socket. It is the
	// fallback for an effective-at that the source did not send, and the start
	// of the latency measurement against the gateway's 50ms slice of the
	// 3-second budget.
	ReceivedAt time.Time
	// RemoteAddr is retained for abuse triage.
	RemoteAddr string
	// PeerIdentity is the verified mTLS subject when the transport authenticated
	// the caller, empty otherwise. An adapter may accept it in place of a
	// signature; it may never accept an unverified claim of identity, which is
	// why this is populated by the server from the TLS state and never from a
	// header.
	PeerIdentity string
	// Replay marks a delivery re-ingested by an operator after a mapping fix
	// rather than sent by the POS. Adapters that would otherwise reject a stale
	// timestamp use it; the pipeline uses it to bypass the dedupe window, since
	// the whole point of a replay is to process something already seen.
	Replay bool
	// ReplayOf is the id of the stored delivery this one re-ingests, and
	// ReplayCount how many times that delivery has now been replayed. Both are
	// recorded so a replay that also fails is visibly a replay rather than
	// looking like fresh traffic from the POS.
	ReplayOf    string
	ReplayCount int

	// SourceTime is the source system's own clock for this delivery. Adapters
	// set it during Ingest when the payload carries one — an IDoc's CREDAT, a
	// Square event's created_at — and the pipeline writes it to
	// Envelope.OccurredAt. Keeping it distinct from RecordedAt is what lets
	// analytics tell a backfill from live traffic, which they otherwise cannot.
	SourceTime time.Time
}

// Header returns a header value, case-insensitively, or the empty string.
func (d *Delivery) Header(name string) string {
	if d.Headers == nil {
		return ""
	}
	return d.Headers.Get(name)
}

// Options returns the adapter-specific configuration compiled for this
// delivery's binding, or nil when the adapter declares none.
func (d *Delivery) Options() any {
	if d.Binding == nil {
		return nil
	}
	return d.Binding.options
}

// Adapter turns one vendor's protocol into canonical price changes.
//
// Implementations must be safe for concurrent use: one adapter instance serves
// every tenant bound to it, and per-tenant state belongs in the Binding.
type Adapter interface {
	// Name is the adapter's stable identifier — "shopify", "sap-idoc". It
	// appears in metric labels, in Envelope.Source and in binding
	// configuration, so it is a public contract and is never renamed.
	Name() string
	// Ingest turns one inbound delivery into zero or more canonical changes.
	//
	// Zero changes with a nil error is a legitimate and common outcome: a
	// webhook topic the adapter does not act on, or a catalogue update that
	// touched a description rather than a price. The pipeline acknowledges it
	// and emits nothing, which is what stops a source from being told to retry
	// something that was never going to produce work.
	//
	// A partially usable delivery — 999 good CSV rows and one that is not —
	// returns the good changes together with a *PartialError, so one bad row
	// cannot discard a file.
	Ingest(ctx context.Context, req *Delivery) ([]canon.PriceChangeRequested, error)
	// IdempotencyParts returns the fields that identify this delivery uniquely
	// for the vendor, used to build the dedupe key.
	//
	// It runs before parsing and must not fail: an adapter that cannot find the
	// vendor's message id returns nil, and the pipeline falls back to a digest
	// of the raw body. That fallback is always correct and merely coarser — it
	// dedupes byte-identical redeliveries but not a source that re-sends the
	// same facts with a new timestamp.
	IdempotencyParts(req *Delivery) []string
	// Verify authenticates the delivery (HMAC, shared secret, mTLS identity).
	// It must use constant-time comparison for every secret-derived value, and
	// must fail closed when the binding carries no credential rather than
	// treating an unconfigured secret as "no authentication required".
	Verify(ctx context.Context, req *Delivery) error
}

// Configurable is implemented by adapters that take per-binding configuration
// beyond the standard credential and store-mapping fields — a mapping document,
// a column layout, an outbound base URL.
//
// Compiling it at binding-install time rather than per delivery is what lets a
// broken column layout be rejected in front of the engineer who wrote it, and
// keeps the price path free of configuration parsing.
type Configurable interface {
	// CompileOptions validates raw adapter options and returns the compiled
	// form, which is handed back to the adapter via Delivery.Options.
	CompileOptions(raw json.RawMessage) (any, error)
}

// ---------------------------------------------------------------------------
// Failure classification
//
// The status code the UIG returns decides whether a POS retries. Getting it
// wrong is not cosmetic: a 5xx for a body that will never parse tells Shopify
// to redeliver it every few minutes for eight hours, and tells an SAP ALE queue
// to block behind it. So failures are classified at the point they are raised,
// by the code that knows what actually went wrong, rather than guessed at by
// the HTTP layer.
// ---------------------------------------------------------------------------

// FailureKind is why a delivery could not be accepted.
type FailureKind string

const (
	// FailureUnauthorized means the signature, secret or peer identity did not
	// check out. Answered 401. Never retried usefully, but also never
	// quarantined with its body retained: storing the payloads of unverified
	// callers is how an ingress endpoint becomes free storage for an attacker.
	FailureUnauthorized FailureKind = "unauthorized"
	// FailureMalformed means the body could not be parsed or mapped. Answered
	// 422 and quarantined with the raw body retained for support.
	FailureMalformed FailureKind = "malformed"
	// FailureInvalid means the body parsed but the resulting change violates a
	// canonical invariant — a negative price, an unknown currency, a store this
	// tenant does not own. Answered 422 and quarantined.
	FailureInvalid FailureKind = "invalid"
	// FailureNotFound means the tenant or binding does not exist. Answered 404.
	FailureNotFound FailureKind = "not_found"
	// FailureRateLimited means the binding is over its budget. Answered 429
	// with Retry-After, which is the one 4xx a POS *should* retry.
	FailureRateLimited FailureKind = "rate_limited"
	// FailureUnavailable means USSLP could not durably record the delivery.
	// Answered 503. This is the only class that invites a retry, because it is
	// the only class where a retry can succeed.
	FailureUnavailable FailureKind = "unavailable"
)

// Error is a classified ingestion failure.
type Error struct {
	// Kind decides the status code and whether the body is retained.
	Kind FailureKind
	// Reason is a short, stable, low-cardinality token used as a metric label
	// and as the quarantine reason. It must never contain payload data: it ends
	// up in a Prometheus label, and unbounded label cardinality takes out the
	// monitoring stack of the system you are trying to observe.
	Reason string
	// Detail is the human-readable explanation returned to the caller and
	// stored with the quarantined delivery.
	Detail string
	// Err is the wrapped cause.
	Err error
}

// Error renders the failure as kind/reason: detail, which is the form that
// appears in logs and in the message a POS integrator quotes back.
func (e *Error) Error() string {
	if e.Err != nil {
		return string(e.Kind) + "/" + e.Reason + ": " + e.Detail + ": " + e.Err.Error()
	}
	return string(e.Kind) + "/" + e.Reason + ": " + e.Detail
}

// Unwrap exposes the cause so callers can match on sentinel errors.
func (e *Error) Unwrap() error { return e.Err }

// Unauthorized builds an authentication failure.
func Unauthorized(reason, detail string) *Error {
	return &Error{Kind: FailureUnauthorized, Reason: reason, Detail: detail}
}

// Malformed builds an unparseable-body failure.
func Malformed(reason, detail string, err error) *Error {
	return &Error{Kind: FailureMalformed, Reason: reason, Detail: detail, Err: err}
}

// Invalid builds a parsed-but-unusable failure.
func Invalid(reason, detail string, err error) *Error {
	return &Error{Kind: FailureInvalid, Reason: reason, Detail: detail, Err: err}
}

// NotFound builds an unknown-tenant-or-binding failure.
func NotFound(reason, detail string) *Error {
	return &Error{Kind: FailureNotFound, Reason: reason, Detail: detail}
}

// Unavailable builds a retryable infrastructure failure.
func Unavailable(reason, detail string, err error) *Error {
	return &Error{Kind: FailureUnavailable, Reason: reason, Detail: detail, Err: err}
}

// Classify reports the failure kind of an error. An error that was never
// classified is treated as malformed rather than as an internal fault: an
// adapter returning a bare fmt.Errorf from its parser is describing input it
// could not handle, and answering 5xx to that would make the source retry
// forever.
func Classify(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Kind: FailureMalformed, Reason: "parse_error", Detail: err.Error(), Err: err}
}

// HTTPStatus maps a failure kind to the status code the POS sees.
func (k FailureKind) HTTPStatus() int {
	switch k {
	case FailureUnauthorized:
		return http.StatusUnauthorized
	case FailureNotFound:
		return http.StatusNotFound
	case FailureRateLimited:
		return http.StatusTooManyRequests
	case FailureUnavailable:
		return http.StatusServiceUnavailable
	default:
		// 422 rather than 400 so that support can tell "we could not understand
		// your message" apart from "your request was structurally wrong",
		// which is the difference between a mapping bug and a routing bug.
		return http.StatusUnprocessableEntity
	}
}

// RetainsBody reports whether a failure of this kind should have its raw body
// stored for support triage.
func (k FailureKind) RetainsBody() bool {
	return k == FailureMalformed || k == FailureInvalid
}

// ---------------------------------------------------------------------------
// Partial success
// ---------------------------------------------------------------------------

// RowFailure describes one unusable record inside an otherwise usable delivery.
type RowFailure struct {
	// Index is the zero-based position of the record in the delivery.
	Index int `json:"index"`
	// Ref is a short identifier from the record itself — a SKU, an item code —
	// so that a support engineer can find it in the source system without
	// opening the retained body.
	Ref string `json:"ref,omitempty"`
	// Reason is a low-cardinality token suitable as a metric label.
	Reason string `json:"reason"`
	// Detail is the human-readable explanation.
	Detail string `json:"detail"`
}

// PartialError reports that some records in a delivery were unusable while
// others were fine.
//
// It exists because of a specific operational failure: a 40,000-line nightly
// price file with one row containing a stray quote character used to fail the
// whole file, and the store opened the next morning with yesterday's prices on
// every shelf. One bad row must cost one product, not a chain.
type PartialError struct {
	Failures []RowFailure
	// Total is how many records the delivery contained, so a caller can tell
	// "one row of a thousand" from "999 rows of a thousand" and alert
	// accordingly.
	Total int
}

// Error summarises how much of the delivery was unusable, which is the number
// that decides whether an operator investigates now or in the morning.
func (p *PartialError) Error() string {
	return fmt.Sprintf("uig/adapter: %d of %d records unusable", len(p.Failures), p.Total)
}

// IsPartial reports whether err carries per-record failures and returns them.
func IsPartial(err error) (*PartialError, bool) {
	var p *PartialError
	if errors.As(err, &p) {
		return p, true
	}
	return nil, false
}

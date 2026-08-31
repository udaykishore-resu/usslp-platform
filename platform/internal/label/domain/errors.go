package domain

import "errors"

// Rejection reasons.
//
// Every one of these strings reaches three audiences: the `reason` field of a
// `label.price.rejected` event retained for the statutory period, a metric
// label on usslp_price_updates_total, and an operator reading a dashboard at
// 3am. They are therefore short, stable, lower-case snake identifiers and are
// never reworded — a renamed reason silently breaks every alert rule keyed on
// it.
const (
	// ReasonSKUMismatch: the price is for a product this label is not showing.
	// Almost always a stale directory entry after a planogram reset.
	ReasonSKUMismatch = "sku_mismatch"
	// ReasonCurrencyMismatch: the price is denominated in a currency the store
	// does not trade in. Never recoverable by retry.
	ReasonCurrencyMismatch = "currency_mismatch"
	// ReasonEffectiveInPast: effective_at is older than the tenant's grace.
	ReasonEffectiveInPast = "effective_at_too_old"
	// ReasonScheduleTooFar: effective_at is beyond the scheduling horizon.
	ReasonScheduleTooFar = "effective_at_too_far"
	// ReasonGuardrail: the change exceeds the tenant's maximum price movement.
	ReasonGuardrail = "guardrail_exceeded"
	// ReasonNotAssigned: the label has no SKU, or has been retired.
	ReasonNotAssigned = "label_not_assigned"
	// ReasonInvalidPrice: the price itself is structurally unusable.
	ReasonInvalidPrice = "invalid_price"
)

// Errors returned by aggregate commands. They are sentinels because the
// application layer branches on them: a stale update is a benign no-op that
// commits its offset, while a guardrail rejection is a durable fact that must
// be published and counted.
var (
	// ErrStaleUpdate reports an update that does not advance the label: a
	// duplicated stream record, or one that arrived after a newer one. It
	// produces no events. Treating it as an error rather than silently
	// succeeding is what lets the handler distinguish "nothing to do" from
	// "applied" when deciding whether to publish to the device.
	ErrStaleUpdate = errors.New("label: stale update, sequence does not advance")
	// ErrOutOfOrder reports an update whose source clock is not after the
	// currently displayed price's. Same handling as ErrStaleUpdate.
	ErrOutOfOrder = errors.New("label: out-of-order update, source is older than displayed price")
	// ErrRejected reports a price refused by an invariant. The command still
	// produces a PriceRejected event, which the caller must persist.
	ErrRejected = errors.New("label: price change rejected")
	// ErrNotFound reports a command against a label that does not exist.
	ErrNotFound = errors.New("label: not found")
	// ErrInvalidCommand reports a structurally unusable command — a missing
	// identifier, an unparseable currency. It is a caller bug, never a data
	// condition, and is not retryable.
	ErrInvalidCommand = errors.New("label: invalid command")
	// ErrUnknownEvent reports an event type the aggregate cannot replay, which
	// means a stream written by a newer version of this service is being read
	// by an older one. Failing loudly is correct: silently skipping an event
	// would rebuild an aggregate that never existed.
	ErrUnknownEvent = errors.New("label: unknown event type")
)

// RejectionError carries the machine-readable reason alongside ErrRejected so
// that a handler can both branch on the sentinel and label a metric with the
// specific cause.
type RejectionError struct {
	// Reason is one of the Reason* constants.
	Reason string
	// Detail is a human-readable explanation for the audit record.
	Detail string
}

// Error implements error.
func (e *RejectionError) Error() string {
	return "label: price change rejected: " + e.Reason + ": " + e.Detail
}

// Unwrap makes errors.Is(err, ErrRejected) true for every rejection.
func (e *RejectionError) Unwrap() error { return ErrRejected }

// RejectionReason extracts the reason from an error, returning "" when the
// error is not a rejection.
func RejectionReason(err error) string {
	var re *RejectionError
	if errors.As(err, &re) {
		return re.Reason
	}
	return ""
}

func reject(reason, detail string) error { return &RejectionError{Reason: reason, Detail: detail} }

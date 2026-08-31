// Package generic implements the mapping-driven adapter: the one that turns
// "integrate our POS" from an engineering project into a configuration change.
//
// A binding for this adapter carries a mapping document as its options (see the
// mapping package for the grammar). Installing it is the entire integration.
// There is no Go code to write, no build, no deploy, and no release train
// between a retailer describing their payload and the platform ingesting it —
// which is the difference between onboarding a long-tail POS in an afternoon
// and quoting six weeks.
//
// The hand-written adapters remain because a handful of sources carry a quirk
// that configuration cannot express honestly: a signature computed over the
// notification URL, an IDoc in a legacy code page, a SOAP fault contract, an
// outbound callback. Everything else is a JSON document with fields in
// unfamiliar places, and for those this adapter is not a fallback — it is the
// preferred path.
package generic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/mapping"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Name is the adapter's registered name.
const Name = "mapping"

// Adapter ingests any JSON payload a mapping document describes.
type Adapter struct{}

// New creates the adapter.
func New() *Adapter { return &Adapter{} }

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return Name }

// CompileOptions compiles the binding's mapping document.
//
// Every selector, field name, type coercion and time layout is validated here,
// at install time, in front of the person who wrote the document. That is the
// whole safety story for a configuration-driven adapter: the alternative is a
// typo in a field name that produces no error and no price change, and a
// retailer discovering it from a shelf.
func (*Adapter) CompileOptions(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, errors.New("a mapping binding must carry a mapping document in its options")
	}
	return mapping.Compile(raw)
}

func mappingOf(d *adapter.Delivery) (*mapping.Mapping, error) {
	m, ok := d.Options().(*mapping.Mapping)
	if !ok || m == nil {
		return nil, adapter.Invalid("no_mapping",
			"this binding has no compiled mapping document", nil)
	}
	return m, nil
}

// Verify authenticates according to the mapping document's verify block.
//
// A document with no verify block is refused rather than treated as
// unauthenticated-by-choice. Configuration that silently opens a price endpoint
// is the failure mode this check exists to prevent; a binding that genuinely
// authenticates at the transport says so, either with peer common names or with
// an explicit "type": "none".
func (*Adapter) Verify(_ context.Context, d *adapter.Delivery) error {
	if accepted, configured := adapter.VerifyPeerIdentity(d.Binding, d.PeerIdentity); configured {
		if accepted {
			return nil
		}
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}
	m, err := mappingOf(d)
	if err != nil {
		return err
	}
	spec := m.Verify()
	if spec == nil {
		return adapter.Unauthorized("no_verification",
			"the mapping document declares no verification and the binding has no peer allow-list")
	}
	switch spec.Type {
	case mapping.VerifyNone:
		return nil
	case mapping.VerifySharedToken:
		return adapter.VerifySharedToken(
			d.Binding.Secrets.SharedToken, d.Header(spec.Header), "shared secret")
	case mapping.VerifyHMACSHA256:
		enc := adapter.EncodingBase64
		if spec.Encoding == "hex" {
			enc = adapter.EncodingHex
		}
		signed := d.Body
		if spec.SignURL {
			// Square-style: the notification URL is signed together with the
			// body. It comes from the binding rather than the request for the
			// same reason it does in the square adapter — a proxy rewriting the
			// host must not silently change what gets verified.
			u := d.Binding.Secrets.NotificationURL
			if u == "" {
				return adapter.Unauthorized("no_notification_url",
					"the mapping signs the URL with the body but the binding has no notification_url")
			}
			signed = append([]byte(u), d.Body...)
		}
		return adapter.VerifyHMACSHA256(d.Binding.Secrets.HMACKey, signed, d.Header(spec.Header), enc, spec.Prefix)
	default:
		return adapter.Unauthorized("bad_verification",
			fmt.Sprintf("the mapping declares an unknown verification type %q", spec.Type))
	}
}

// IdempotencyParts evaluates the mapping's idempotency selectors.
func (*Adapter) IdempotencyParts(d *adapter.Delivery) []string {
	m, err := mappingOf(d)
	if err != nil {
		return nil
	}
	return m.IdempotencyParts(d.Body)
}

// Ingest applies the mapping document to the payload.
func (*Adapter) Ingest(_ context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	m, err := mappingOf(d)
	if err != nil {
		return nil, err
	}
	changes, err := m.Apply(d.Body)
	if err != nil {
		// A mapping failure is always permanent and always the retailer's or
		// the integration engineer's to fix, so it is classified as malformed:
		// quarantined with its body retained, answered 4xx, and replayable once
		// the document is corrected.
		reason := "mapping_failed"
		if errors.Is(err, mapping.ErrPayload) {
			reason = "payload_mismatch"
		}
		return nil, adapter.Malformed(reason,
			fmt.Sprintf("mapping %q could not be applied: %v", m.Name(), err), err)
	}
	return changes, nil
}

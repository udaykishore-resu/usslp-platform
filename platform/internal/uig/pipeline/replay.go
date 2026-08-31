package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/deliveries"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Replay re-ingests a stored delivery.
//
// This is the operator's tool for the most common integration failure there is:
// a retailer changes a field, a mapping stops matching, several thousand price
// rows are quarantined with a 4xx, and once the mapping is corrected those rows
// have to be processed without asking the retailer to re-run a nightly job that
// cannot be re-run. The stored raw body is replayed against the *current*
// configuration, so the fix is exercised on exactly the bytes that failed.
//
// Three things about a replay differ from a live delivery, and each is a
// deliberate decision rather than a shortcut:
//
//   - The idempotency guard is bypassed. The delivery has already been seen by
//     definition; refusing to process it because of that would make replay
//     impossible. The safety this gives up is real, which is why replay is an
//     authenticated operator endpoint and not something a POS can trigger.
//   - Adapter verification is skipped. The signature was checked when the
//     delivery arrived, and re-checking it would fail for any binding whose
//     secret has since been rotated — which is exactly the situation where an
//     operator most needs to replay.
//   - The result is filed under a *new* delivery id that points back at the
//     original, so the audit trail shows a replay rather than a second arrival.
func (p *Pipeline) Replay(ctx context.Context, tenant canon.TenantID, deliveryID string) (*Result, error) {
	rec, err := p.cfg.Deliveries.Get(tenant, deliveryID)
	if err != nil {
		return nil, err
	}
	if ok, why := rec.Replayable(); !ok {
		p.cfg.Metrics.Replays.With(rec.Adapter, "not_replayable").Inc()
		return nil, fmt.Errorf("%w: %s", ErrNotReplayable, why)
	}
	b, err := p.cfg.Bindings.Get(tenant, rec.BindingID)
	if err != nil {
		p.cfg.Metrics.Replays.With(rec.Adapter, "binding_missing").Inc()
		return nil, fmt.Errorf("uig/pipeline: binding %s/%s no longer exists: %w", tenant, rec.BindingID, err)
	}

	d := &adapter.Delivery{
		ID:          canon.NewULID(),
		TenantID:    tenant,
		BindingID:   rec.BindingID,
		Binding:     b,
		Method:      rec.Method,
		URL:         rec.URL,
		Path:        rec.Path,
		Headers:     rec.HeaderValues(),
		ContentType: rec.ContentType,
		Body:        rec.Body,
		ReceivedAt:  p.now(),
		Replay:      true,
		ReplayOf:    rec.ID,
		ReplayCount: rec.ReplayCount + 1,
	}
	if rec.URL != "" {
		if u, err := url.Parse(rec.URL); err == nil {
			d.Query = u.Query()
		}
	}
	res := p.Ingest(ctx, d)
	outcome := string(res.Status)
	if res.HTTPStatus >= 400 {
		outcome = "failed"
	}
	p.cfg.Metrics.Replays.With(res.Adapter, outcome).Inc()

	// Bump the replay counter on the *original* record so an operator can see
	// that a delivery has been replayed three times already before trying a
	// fourth — a replay loop against a mapping that is still wrong is a real
	// way to double-publish a price book.
	rec.ReplayCount++
	rec.ForceRetainBody = true
	if err := p.cfg.Deliveries.Put(rec); err != nil {
		p.log.Error("uig: updating replay count failed", "delivery_id", rec.ID, "error", err)
	}
	return res, nil
}

// ErrNotReplayable means the stored delivery no longer carries the bytes needed
// to re-ingest it — normally because it succeeded and the binding does not have
// raw retention switched on.
var ErrNotReplayable = errors.New("uig/pipeline: delivery cannot be replayed")

// ListDeliveries is the support triage query behind
// GET /v1/deliveries/{tenant}?status=quarantined.
func (p *Pipeline) ListDeliveries(tenant canon.TenantID, q deliveries.Query) ([]*deliveries.Record, error) {
	return p.cfg.Deliveries.List(tenant, q)
}

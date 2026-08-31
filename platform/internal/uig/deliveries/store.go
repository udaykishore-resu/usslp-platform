// Package deliveries is the UIG's record of what arrived: the quarantine for
// deliveries that could not be processed, and the replay source for deliveries
// that can be processed once a mapping is fixed.
//
// Its reason for existing is a support workflow, not a technical one. When a
// retailer's ERP team changes a field name at 2am and 3,000 price rows stop
// mapping, three things have to be true by morning: the UIG must have answered
// 4xx so the ERP stopped retrying and its outbound queue did not back up; a
// support engineer must be able to see the exact bytes that were sent, because
// the retailer will insist they sent something else; and once the mapping is
// corrected, those exact bytes must be re-ingestable without asking the
// retailer to re-run a job they cannot re-run.
//
// Records are held in the embedded kvstore with a TTL rather than in the event
// stream, because they are operational state with a retention policy of their
// own — raw bodies from a failed integration are exactly the data a retailer
// asks you to delete, and a compacted-forever Kafka topic is the wrong place
// for it.
package deliveries

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// Status is the outcome recorded against a delivery.
type Status string

const (
	// StatusAccepted means the delivery produced canonical events that were
	// durably published.
	StatusAccepted Status = "accepted"
	// StatusPartial means some records were published and some were not. The
	// retained body plus the row failures are what a support engineer needs to
	// tell a retailer which of their 40,000 rows to look at.
	StatusPartial Status = "partial"
	// StatusQuarantined means nothing was published because the delivery could
	// not be parsed or mapped. Answered 4xx; body retained.
	StatusQuarantined Status = "quarantined"
	// StatusRejected means the delivery was refused before parsing —
	// authentication, rate limit, disabled binding. The body is not retained.
	StatusRejected Status = "rejected"
	// StatusIgnored means the delivery was understood and deliberately produced
	// no price changes: a webhook topic the adapter does not act on.
	StatusIgnored Status = "ignored"
)

// Valid reports whether s is a known status, used to validate query strings on
// the support triage endpoint.
func (s Status) Valid() bool {
	switch s {
	case StatusAccepted, StatusPartial, StatusQuarantined, StatusRejected, StatusIgnored:
		return true
	}
	return false
}

// Record is one stored delivery.
type Record struct {
	ID          string         `json:"id"`
	TenantID    canon.TenantID `json:"tenant_id"`
	BindingID   string         `json:"binding_id"`
	Adapter     string         `json:"adapter"`
	Status      Status         `json:"status"`
	ReceivedAt  time.Time      `json:"received_at"`
	CompletedAt time.Time      `json:"completed_at"`
	// DurationMS is the pipeline's own latency for this delivery, so an
	// operator triaging a slow integration does not have to correlate logs.
	DurationMS int64 `json:"duration_ms"`
	// HTTPStatus is what the POS was told.
	HTTPStatus int `json:"http_status"`
	// Reason is the low-cardinality failure token; Detail is the explanation.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Emitted is how many canonical changes were published.
	Emitted int `json:"emitted"`
	// RowFailures records per-record problems inside a partially usable
	// delivery.
	RowFailures []RowFailure `json:"row_failures,omitempty"`
	// Method, URL and Headers reproduce enough of the transport to replay the
	// delivery exactly, signature included.
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Path    string              `json:"path,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	// ContentType is kept separately because the replay path needs it even when
	// headers have been redacted.
	ContentType string `json:"content_type,omitempty"`
	// Body is the raw payload, retained only for statuses whose triage needs
	// it. It is base64-encoded by encoding/json, which keeps a binary
	// fixed-width drop intact.
	Body []byte `json:"body,omitempty"`
	// BodySize is always present even when the body is not retained, so an
	// operator can tell an empty delivery from a discarded one.
	BodySize int `json:"body_size"`
	// ReplayOf points at the delivery this one re-ingested, so a replay that
	// also fails does not look like fresh traffic from the POS.
	ReplayOf string `json:"replay_of,omitempty"`
	// ReplayCount is how many times this delivery has been replayed, which is
	// the number an operator checks before replaying it a fourth time.
	ReplayCount int `json:"replay_count,omitempty"`

	// ForceRetainBody keeps the raw body for a status that would not normally
	// keep one. It is set from the binding's RetainRaw flag while an
	// integration is being commissioned, so that a delivery which worked can
	// still be replayed against a corrected mapping. It is not persisted: the
	// presence of the body in the stored record is the durable fact.
	ForceRetainBody bool `json:"-"`
}

// RowFailure mirrors the adapter's per-record failure in storable form.
type RowFailure struct {
	Index  int    `json:"index"`
	Ref    string `json:"ref,omitempty"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// ErrNotFound means no delivery is stored under that id.
var ErrNotFound = errors.New("uig/deliveries: no such delivery")

// sensitiveHeaders are dropped before a record is stored.
//
// A quarantined delivery's headers are retained so it can be replayed with its
// original signature, but the *credential-bearing* ones are not: a support
// bundle containing a retailer's live API key is a breach, and quarantine
// records are exactly the thing that ends up in support bundles. The signature
// headers themselves are kept — a signature over a body that is already stored
// discloses nothing further, and without it a replay cannot re-verify.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"x-api-key":           true,
	"x-clover-auth":       true,
	"x-ncr-secret-key":    true,
}

// Store persists delivery records.
type Store struct {
	kv     *kvstore.Store
	prefix string
	// retention bounds how long raw bodies live. Fourteen days matches the
	// dead-letter stream's retention in canon.AllStreams, so a delivery and the
	// dead-lettered event it produced expire together rather than leaving an
	// operator with half a trail.
	retention time.Duration
	// maxBody caps how much of an oversized payload is retained. A truncated
	// body is still enough to diagnose a mapping failure, and an uncapped one
	// lets a caller store arbitrary data through a 4xx path.
	maxBody int
	now     func() time.Time
}

// DefaultRetention is how long delivery records are kept.
const DefaultRetention = 14 * 24 * time.Hour

// DefaultMaxBody is the retained-body cap.
const DefaultMaxBody = 1 << 20

// Options configure a Store.
type Options struct {
	// Prefix namespaces records inside a shared kvstore.
	Prefix string
	// Retention is the record TTL; zero uses DefaultRetention.
	Retention time.Duration
	// MaxBody is the retained-body cap; zero uses DefaultMaxBody.
	MaxBody int
	// Now injects a clock for tests.
	Now func() time.Time
}

// New creates a delivery store over a kvstore.
func New(kv *kvstore.Store, opts Options) (*Store, error) {
	if kv == nil {
		return nil, errors.New("uig/deliveries: nil kvstore")
	}
	s := &Store{
		kv:        kv,
		prefix:    opts.Prefix,
		retention: opts.Retention,
		maxBody:   opts.MaxBody,
		now:       opts.Now,
	}
	if s.prefix == "" {
		s.prefix = "uig/deliveries/"
	}
	if s.retention <= 0 {
		s.retention = DefaultRetention
	}
	if s.maxBody <= 0 {
		s.maxBody = DefaultMaxBody
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// key is prefix + tenant + '/' + id. Tenant-first ordering is what lets the
// support endpoint scan one tenant's deliveries without reading another's,
// which is both a performance property and an isolation one.
func (s *Store) key(tenant canon.TenantID, id string) []byte {
	return []byte(s.prefix + string(tenant) + "/" + id)
}

func (s *Store) tenantPrefix(tenant canon.TenantID) []byte {
	return []byte(s.prefix + string(tenant) + "/")
}

// Put stores a record, sanitising headers and capping the retained body.
func (s *Store) Put(rec *Record) error {
	if rec == nil || rec.ID == "" || rec.TenantID == "" {
		return errors.New("uig/deliveries: record needs an id and a tenant")
	}
	cp := *rec
	cp.Headers = sanitiseHeaders(rec.Headers)
	cp.BodySize = len(rec.Body)
	if !cp.Status.RetainsBody() && !cp.ForceRetainBody {
		cp.Body = nil
	} else if len(cp.Body) > s.maxBody {
		cp.Body = append([]byte(nil), cp.Body[:s.maxBody]...)
	}
	if cp.CompletedAt.IsZero() {
		cp.CompletedAt = s.now().UTC()
	}
	body, err := json.Marshal(&cp)
	if err != nil {
		return fmt.Errorf("uig/deliveries: encode %s: %w", cp.ID, err)
	}
	return s.kv.PutTTL(s.key(cp.TenantID, cp.ID), body, s.retention)
}

// RetainsBody reports whether a status keeps its raw payload.
//
// Accepted deliveries do not, by default: at peak the platform ingests tens of
// thousands of price changes a second, and retaining every raw body for two
// weeks is terabytes of storage to answer a question nobody asks about a
// delivery that worked. The pipeline overrides this per binding while an
// integration is being commissioned.
func (st Status) RetainsBody() bool {
	return st == StatusQuarantined || st == StatusPartial
}

func sanitiseHeaders(h map[string][]string) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, v := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			out[k] = []string{redactedHeader}
			continue
		}
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

const redactedHeader = "***redacted***"

// Get returns one stored delivery.
func (s *Store) Get(tenant canon.TenantID, id string) (*Record, error) {
	raw, err := s.kv.Get(s.key(tenant, id))
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, tenant, id)
	}
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("uig/deliveries: decode %s: %w", id, err)
	}
	return &rec, nil
}

// Query filters a tenant's deliveries for the support triage endpoint.
type Query struct {
	// Status, when set, restricts the results.
	Status Status
	// BindingID, when set, restricts the results.
	BindingID string
	// Since, when set, drops anything received earlier.
	Since time.Time
	// Limit caps the result count; zero means DefaultQueryLimit.
	Limit int
	// IncludeBodies returns the retained payloads. Off by default so that the
	// common "show me what is broken" call does not ship megabytes to a browser
	// and does not put raw retailer data in an access log.
	IncludeBodies bool
}

// DefaultQueryLimit bounds an unfiltered support query.
const DefaultQueryLimit = 100

// List returns matching records, newest first.
func (s *Store) List(tenant canon.TenantID, q Query) ([]*Record, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultQueryLimit
	}
	it := s.kv.Scan(s.tenantPrefix(tenant))
	defer it.Close()
	var out []*Record
	for it.Next() {
		var rec Record
		if err := json.Unmarshal(it.Value(), &rec); err != nil {
			// A single undecodable record must not fail an operator's triage
			// query; skipping it is strictly better than showing them nothing.
			continue
		}
		if q.Status != "" && rec.Status != q.Status {
			continue
		}
		if q.BindingID != "" && rec.BindingID != q.BindingID {
			continue
		}
		if !q.Since.IsZero() && rec.ReceivedAt.Before(q.Since) {
			continue
		}
		if !q.IncludeBodies {
			rec.Body = nil
		}
		r := rec
		out = append(out, &r)
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.After(out[j].ReceivedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete removes a record, which is how a retailer's deletion request is
// honoured ahead of the retention window.
func (s *Store) Delete(tenant canon.TenantID, id string) error {
	return s.kv.Delete(s.key(tenant, id))
}

// HeaderValues rebuilds an http.Header from a stored record so the replay path
// can reconstruct the original request, signature headers included.
func (r *Record) HeaderValues() http.Header {
	h := make(http.Header, len(r.Headers))
	for k, vs := range r.Headers {
		for _, v := range vs {
			if v == redactedHeader {
				continue
			}
			h.Add(k, v)
		}
	}
	return h
}

// Replayable reports whether a record still carries the bytes needed to
// re-ingest it, with the reason when it does not — which is the message an
// operator gets instead of a silent no-op.
func (r *Record) Replayable() (bool, string) {
	if len(r.Body) == 0 {
		if r.BodySize == 0 {
			return false, "the original delivery had an empty body"
		}
		return false, "the raw body was not retained for this delivery; enable retain_raw on the binding to make successful deliveries replayable"
	}
	return true, ""
}

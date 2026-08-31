package deliveries

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/kvstore"
)

func newTestStore(t *testing.T, opts Options) *Store {
	t.Helper()
	kv, err := kvstore.Open("")
	if err != nil {
		t.Fatalf("kvstore: %v", err)
	}
	t.Cleanup(func() { kv.Close() })
	s, err := New(kv, opts)
	if err != nil {
		t.Fatalf("deliveries.New: %v", err)
	}
	return s
}

func TestPutRetainsBodyOnlyWhereTriageNeedsIt(t *testing.T) {
	s := newTestStore(t, Options{})
	body := []byte(`{"raw":"payload"}`)

	for _, c := range []struct {
		status Status
		retain bool
	}{
		{StatusQuarantined, true},
		{StatusPartial, true},
		{StatusAccepted, false},
		{StatusRejected, false},
		{StatusIgnored, false},
	} {
		rec := &Record{
			ID: "d-" + string(c.status), TenantID: "acme", BindingID: "b", Adapter: "shopify",
			Status: c.status, Body: body, ReceivedAt: time.Now().UTC(),
		}
		if err := s.Put(rec); err != nil {
			t.Fatalf("Put(%s): %v", c.status, err)
		}
		got, err := s.Get("acme", rec.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", c.status, err)
		}
		if (len(got.Body) > 0) != c.retain {
			t.Errorf("%s: body retained = %v, want %v", c.status, len(got.Body) > 0, c.retain)
		}
		// The size is recorded even when the body is not, so an operator can
		// tell an empty delivery from a discarded one.
		if got.BodySize != len(body) {
			t.Errorf("%s: BodySize = %d, want %d", c.status, got.BodySize, len(body))
		}
	}
}

func TestForceRetainBodyForCommissioning(t *testing.T) {
	s := newTestStore(t, Options{})
	rec := &Record{
		ID: "d1", TenantID: "acme", Status: StatusAccepted,
		Body: []byte("keep me"), ForceRetainBody: true,
	}
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("acme", "d1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != "keep me" {
		t.Fatalf("body = %q; retain_raw must keep a successful delivery replayable", got.Body)
	}
	if ok, _ := got.Replayable(); !ok {
		t.Error("a retained accepted delivery must be replayable")
	}
}

func TestPutRedactsCredentialHeaders(t *testing.T) {
	s := newTestStore(t, Options{})
	rec := &Record{
		ID: "d1", TenantID: "acme", Status: StatusQuarantined,
		Body: []byte("x"),
		Headers: map[string][]string{
			"Authorization":         {"Bearer live-token"},
			"X-Api-Key":             {"live-key"},
			"X-Clover-Auth":         {"live-code"},
			"X-Shopify-Hmac-Sha256": {"c2ln"},
			"Content-Type":          {"application/json"},
		},
	}
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("acme", "d1")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Authorization", "X-Api-Key", "X-Clover-Auth"} {
		if got.Headers[h][0] != redactedHeader {
			t.Errorf("%s was stored in the clear: %v", h, got.Headers[h])
		}
	}
	// The signature header is kept: it discloses nothing beyond the body that
	// is already stored, and without it a replay cannot re-verify.
	if got.Headers["X-Shopify-Hmac-Sha256"][0] != "c2ln" {
		t.Error("the signature header must be retained for replay")
	}
	// A redacted header must not be reconstructed into a replayed request as a
	// literal placeholder, which would authenticate as nothing at all.
	h := got.HeaderValues()
	if h.Get("Authorization") != "" {
		t.Errorf("redacted header leaked into the replay request: %q", h.Get("Authorization"))
	}
	if h.Get("Content-Type") != "application/json" {
		t.Error("a normal header was lost")
	}
}

func TestBodyIsCappedRatherThanUnbounded(t *testing.T) {
	s := newTestStore(t, Options{MaxBody: 16})
	rec := &Record{
		ID: "d1", TenantID: "acme", Status: StatusQuarantined,
		Body: []byte(strings.Repeat("A", 1000)),
	}
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("acme", "d1")
	if len(got.Body) != 16 {
		t.Fatalf("stored body length = %d, want the 16 byte cap", len(got.Body))
	}
	if got.BodySize != 1000 {
		t.Errorf("BodySize = %d, want the true 1000", got.BodySize)
	}
}

func TestListFiltersAndOrders(t *testing.T) {
	s := newTestStore(t, Options{})
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	for i, c := range []struct {
		id      string
		status  Status
		binding string
		offset  time.Duration
	}{
		{"a", StatusAccepted, "b1", 0},
		{"b", StatusQuarantined, "b1", time.Minute},
		{"c", StatusQuarantined, "b2", 2 * time.Minute},
		{"d", StatusRejected, "b1", 3 * time.Minute},
	} {
		rec := &Record{
			ID: c.id, TenantID: "acme", BindingID: c.binding, Adapter: "shopify",
			Status: c.status, ReceivedAt: base.Add(c.offset), Body: []byte("body"),
		}
		if err := s.Put(rec); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	// Another tenant's records must never appear.
	if err := s.Put(&Record{ID: "z", TenantID: "other", Status: StatusQuarantined, Body: []byte("x")}); err != nil {
		t.Fatal(err)
	}

	all, err := s.List("acme", Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("unfiltered list = %d records, want 4 (and no cross-tenant leakage)", len(all))
	}
	// Newest first: an operator triaging an incident wants the most recent
	// failures at the top.
	if all[0].ID != "d" || all[3].ID != "a" {
		t.Errorf("order = %s..%s, want newest first", all[0].ID, all[3].ID)
	}
	// Bodies are withheld by default so that "show me what is broken" does not
	// ship megabytes of retailer data into an access log.
	if len(all[1].Body) != 0 {
		t.Error("bodies must be withheld unless explicitly requested")
	}

	q, err := s.List("acme", Query{Status: StatusQuarantined})
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 2 {
		t.Fatalf("quarantined = %d, want 2", len(q))
	}
	byBinding, _ := s.List("acme", Query{Status: StatusQuarantined, BindingID: "b2"})
	if len(byBinding) != 1 || byBinding[0].ID != "c" {
		t.Errorf("binding filter = %v", byBinding)
	}
	since, _ := s.List("acme", Query{Since: base.Add(90 * time.Second)})
	if len(since) != 2 {
		t.Errorf("since filter = %d, want 2", len(since))
	}
	limited, _ := s.List("acme", Query{Limit: 1})
	if len(limited) != 1 || limited[0].ID != "d" {
		t.Errorf("limit = %v", limited)
	}
	withBodies, _ := s.List("acme", Query{Status: StatusQuarantined, IncludeBodies: true})
	if len(withBodies[0].Body) == 0 {
		t.Error("include_bodies did not return the payload")
	}
}

func TestGetMissingAndDelete(t *testing.T) {
	s := newTestStore(t, Options{})
	if _, err := s.Get("acme", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := s.Put(&Record{ID: "d1", TenantID: "acme", Status: StatusQuarantined, Body: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	// Deleting ahead of the retention window is how a retailer's erasure
	// request is honoured.
	if err := s.Delete("acme", "d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("acme", "d1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err = %v", err)
	}
	if err := s.Put(&Record{ID: "", TenantID: "acme"}); err == nil {
		t.Error("a record with no id must be refused")
	}
}

func TestReplayableExplainsWhyNot(t *testing.T) {
	empty := &Record{BodySize: 0}
	if ok, why := empty.Replayable(); ok || !strings.Contains(why, "empty body") {
		t.Errorf("empty: %v %q", ok, why)
	}
	dropped := &Record{BodySize: 500}
	if ok, why := dropped.Replayable(); ok || !strings.Contains(why, "retain_raw") {
		t.Errorf("dropped: %v %q", ok, why)
	}
	if ok, _ := (&Record{Body: []byte("x"), BodySize: 1}).Replayable(); !ok {
		t.Error("a record with a body must be replayable")
	}
}

func TestStatusValidation(t *testing.T) {
	for _, s := range []Status{StatusAccepted, StatusPartial, StatusQuarantined, StatusRejected, StatusIgnored} {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if Status("bogus").Valid() {
		t.Error("an unknown status must not validate")
	}
}

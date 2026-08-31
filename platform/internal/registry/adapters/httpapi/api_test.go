package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/adapters"
	"github.com/usslp/usslp/platform/internal/registry/adapters/httpapi"
	"github.com/usslp/usslp/platform/internal/registry/app"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// newServer wires a complete registry behind an httptest server. The seeding
// endpoint is enabled, which is what lets one test exercise the whole surface
// against a store that provisioned itself through the real certificate path.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	at := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	profile := pki.TestProfile()
	hierarchy, err := pki.Bootstrap(pki.BootstrapConfig{Profile: &profile, Now: at})
	if err != nil {
		t.Fatalf("bootstrap pki: %v", err)
	}
	kv, err := kvstore.OpenWith(kvstore.Options{Dir: t.TempDir(), Sync: kvstore.SyncEvery})
	if err != nil {
		t.Fatalf("open kvstore: %v", err)
	}
	store, err := eventstore.New(kv)
	if err != nil {
		t.Fatalf("open eventstore: %v", err)
	}
	svc, err := app.Open(context.Background(), app.Config{
		Store:     store,
		Messenger: adapters.NopMessenger{},
		Auth:      adapters.NewHierarchyAuthenticator(hierarchy),
		Issuer:    adapters.NewHierarchyIssuer(hierarchy),
		Clock:     fixedClock{at},
		Region:    canon.Region("eu-west-1"),
	})
	if err != nil {
		t.Fatalf("open registry service: %v", err)
	}
	srv := httptest.NewServer(httpapi.New(svc, nil, nil).Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = store.Close()
		_ = kv.Close()
	})
	return srv
}

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

func do(t *testing.T, srv *httptest.Server, method, path string, body any, out any) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s response: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func TestHTTPSurfaceEndToEnd(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	var seeded app.SeedResult
	if code := do(t, srv, http.MethodPost, "/v1/dev/seed", app.SeedRequest{
		TenantID: "acme", StoreID: "store-0042", SECs: 2, LabelsPerSEC: 6,
		Seed: 99, WithTelemetry: true,
	}, &seeded); code != http.StatusCreated {
		t.Fatalf("seed returned %d", code)
	}
	if seeded.Labels != 12 || len(seeded.SECs) != 2 {
		t.Fatalf("seed result = %+v", seeded)
	}

	// A device is reachable by its platform identifier.
	var device map[string]any
	if code := do(t, srv, http.MethodGet, "/v1/devices/lbl-store-0042-01-001", nil, &device); code != http.StatusOK {
		t.Fatalf("get device returned %d", code)
	}
	if device["device"] == nil {
		t.Fatalf("device response = %+v", device)
	}

	// And by the serial a technician scans off the shelf.
	var bySerial map[string]any
	serial := device["device"].(map[string]any)["serial"].(string)
	if code := do(t, srv, http.MethodGet, "/v1/devices/"+serial, nil, &bySerial); code != http.StatusOK {
		t.Fatalf("get device by serial returned %d", code)
	}

	var devices struct {
		Count int `json:"count"`
	}
	if code := do(t, srv, http.MethodGet, "/v1/stores/store-0042/devices?kind=label", nil, &devices); code != http.StatusOK {
		t.Fatalf("list devices returned %d", code)
	}
	if devices.Count != 12 {
		t.Fatalf("store holds %d labels, want 12", devices.Count)
	}

	var mesh struct {
		Controllers  int `json:"controllers"`
		TotalOrphans int `json:"total_orphans"`
	}
	if code := do(t, srv, http.MethodGet, "/v1/stores/store-0042/mesh", nil, &mesh); code != http.StatusOK {
		t.Fatalf("get mesh returned %d", code)
	}
	if mesh.Controllers != 2 || mesh.TotalOrphans != 0 {
		t.Fatalf("mesh = %+v", mesh)
	}

	var health struct {
		Score  float64 `json:"score"`
		Grade  string  `json:"grade"`
		Labels int     `json:"labels"`
	}
	if code := do(t, srv, http.MethodGet, "/v1/stores/store-0042/health", nil, &health); code != http.StatusOK {
		t.Fatalf("get health returned %d", code)
	}
	if health.Labels != 12 || health.Score <= 0 {
		t.Fatalf("health = %+v", health)
	}

	var planogram struct {
		Revision  int64 `json:"revision"`
		Positions []struct {
			LabelID string `json:"label_id"`
		} `json:"positions"`
	}
	if code := do(t, srv, http.MethodGet, "/v1/stores/store-0042/planogram", nil, &planogram); code != http.StatusOK {
		t.Fatalf("get planogram returned %d", code)
	}
	if planogram.Revision != 1 || len(planogram.Positions) != 12 {
		t.Fatalf("planogram = revision %d with %d positions", planogram.Revision, len(planogram.Positions))
	}

	var summary struct {
		Devices int `json:"devices"`
		Stores  int `json:"stores"`
	}
	if code := do(t, srv, http.MethodGet, "/v1/fleet/summary", nil, &summary); code != http.StatusOK {
		t.Fatalf("fleet summary returned %d", code)
	}
	if summary.Devices != 14 || summary.Stores != 1 {
		t.Fatalf("fleet summary = %+v", summary)
	}

	// Retirement is reflected immediately.
	if code := do(t, srv, http.MethodPost, "/v1/devices/lbl-store-0042-01-001/retire",
		map[string]string{"reason": "screen cracked"}, nil); code != http.StatusOK {
		t.Fatalf("retire returned %d", code)
	}
	var retired map[string]any
	do(t, srv, http.MethodGet, "/v1/devices/lbl-store-0042-01-001", nil, &retired)
	if got := retired["device"].(map[string]any)["state"]; got != "retired" {
		t.Fatalf("state after retirement = %v", got)
	}
}

func TestHTTPErrorsMapToActionableStatuses(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	if code := do(t, srv, http.MethodGet, "/v1/devices/does-not-exist", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown device returned %d, want 404", code)
	}
	if code := do(t, srv, http.MethodGet, "/v1/stores/store-nowhere/planogram", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown planogram returned %d, want 404", code)
	}
	// A provisioning request with no certificate is a permanent failure, not a
	// retryable one.
	if code := do(t, srv, http.MethodPost, "/v1/provision",
		map[string]any{"eui64": "0011223344556677", "sec_id": "sec-01"}, nil); code != http.StatusBadRequest {
		t.Fatalf("provision with no certificate returned %d, want 400", code)
	}
	// A misspelled field is rejected rather than silently defaulted: a
	// planogram with a mistyped facing count would be discovered by a customer
	// looking at a shelf.
	if code := do(t, srv, http.MethodPost, "/v1/stores/store-0042/planogram",
		map[string]any{"store_id": "store-0042", "positions": []map[string]any{{
			"shelf": "A", "rail": "1", "position": 1, "label_id": "lbl-1",
			"sku": "SKU-1", "facing_count": 2, "sec_id": "sec-01",
		}}}, nil); code != http.StatusBadRequest {
		t.Fatalf("planogram with an unknown field returned %d, want 400", code)
	}
	// A planogram uploaded to the wrong store must not be applied.
	if code := do(t, srv, http.MethodPost, "/v1/stores/store-0042/planogram",
		map[string]any{"tenant_id": "acme", "store_id": "store-9999", "positions": []any{}},
		nil); code != http.StatusBadRequest {
		t.Fatalf("planogram for another store returned %d, want 400", code)
	}
}

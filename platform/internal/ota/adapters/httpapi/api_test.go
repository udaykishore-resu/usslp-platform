package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/ota/adapters"
	"github.com/usslp/usslp/platform/internal/ota/adapters/httpapi"
	"github.com/usslp/usslp/platform/internal/ota/app"
	"github.com/usslp/usslp/platform/internal/ota/domain"
	registryadapters "github.com/usslp/usslp/platform/internal/registry/adapters"
	registryapp "github.com/usslp/usslp/platform/internal/registry/app"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/pki"
)

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// fixture stands both services up in one process: a Device Registry with a
// seeded store, and an OTA service whose fleet directory reads from it.
//
// The two are separate deployables in production, but wiring them together here
// is what makes the test meaningful: the rollout targets devices that were
// provisioned through the real certificate path, and the "may we address this
// device" decision is made once, by the registry, exactly as it is in
// production.
type fixture struct {
	srv      *httptest.Server
	ctrl     *app.Controller
	registry *registryapp.Service
	key      ed25519.PrivateKey
	tier     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	// 03:00 UTC, inside the default quiet window a store would configure.
	at := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	clock := fixedClock{at}

	profile := pki.TestProfile()
	hierarchy, err := pki.Bootstrap(pki.BootstrapConfig{Profile: &profile, Now: at})
	if err != nil {
		t.Fatalf("bootstrap pki: %v", err)
	}

	regKV, err := kvstore.OpenWith(kvstore.Options{Dir: t.TempDir(), Sync: kvstore.SyncEvery})
	if err != nil {
		t.Fatalf("open registry kvstore: %v", err)
	}
	regStore, err := eventstore.New(regKV)
	if err != nil {
		t.Fatalf("open registry eventstore: %v", err)
	}
	registry, err := registryapp.Open(context.Background(), registryapp.Config{
		Store:     regStore,
		Messenger: registryadapters.NopMessenger{},
		Auth:      registryadapters.NewHierarchyAuthenticator(hierarchy),
		Issuer:    registryadapters.NewHierarchyIssuer(hierarchy),
		Clock:     clock,
		Region:    canon.Region("eu-west-1"),
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	if _, err := registry.Seed(context.Background(), registryapp.SeedRequest{
		TenantID: "acme", StoreID: "store-0042", SECs: 3, LabelsPerSEC: 20,
		Seed: 7, HardwareTier: "esl-2.9-bw", FirmwareVersion: "1.4.2", WithTelemetry: true,
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	otaKV, err := kvstore.OpenWith(kvstore.Options{Dir: t.TempDir(), Sync: kvstore.SyncEvery})
	if err != nil {
		t.Fatalf("open ota kvstore: %v", err)
	}
	otaStore, err := eventstore.New(otaKV)
	if err != nil {
		t.Fatalf("open ota eventstore: %v", err)
	}
	ctrl, err := app.Open(context.Background(), app.Config{
		Store:     otaStore,
		Artifacts: adapters.NewMemoryArtifactStore(),
		Keys:      domain.KeyRing{"fw-2026-a": pub},
		Fleet: adapters.NewRegistryDirectory(registry, map[canon.StoreID]string{
			"store-0042": "Europe/London",
		}),
		Messenger: adapters.NopMessenger{},
		Clock:     clock,
		Region:    canon.Region("eu-west-1"),
	})
	if err != nil {
		t.Fatalf("open ota controller: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(ctrl, nil, nil).Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = otaStore.Close()
		_ = otaKV.Close()
		_ = regStore.Close()
		_ = regKV.Close()
	})
	return &fixture{srv: srv, ctrl: ctrl, registry: registry, key: priv, tier: "esl-2.9-bw"}
}

func (f *fixture) do(t *testing.T, method, path string, body any, out any) int {
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
	req, err := http.NewRequest(method, f.srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.srv.Client().Do(req)
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

// uploadBody builds a signed firmware upload request.
func (f *fixture) uploadBody(version domain.Version, image []byte) map[string]any {
	return map[string]any{
		"version":       string(version),
		"hardware_tier": f.tier,
		"signature":     domain.SignArtifact(f.key, version, f.tier, image),
		"image":         base64.StdEncoding.EncodeToString(image),
	}
}

func TestFirmwareUploadAndRolloutOverHTTP(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	image := bytes.Repeat([]byte("firmware-v143-"), 5000)
	var artifact domain.Artifact
	if code := f.do(t, http.MethodPost, "/v1/firmware", f.uploadBody("1.4.3", image), &artifact); code != http.StatusCreated {
		t.Fatalf("upload returned %d", code)
	}
	if artifact.SigningKeyID != "fw-2026-a" || artifact.ArtifactID == "" {
		t.Fatalf("artifact = %+v", artifact)
	}

	var listing struct {
		Count int `json:"count"`
	}
	if code := f.do(t, http.MethodGet, "/v1/firmware?hardware_tier=esl-2.9-bw", nil, &listing); code != http.StatusOK {
		t.Fatalf("list firmware returned %d", code)
	}
	if listing.Count != 1 {
		t.Fatalf("firmware listing holds %d artifacts, want 1", listing.Count)
	}

	var job domain.Job
	if code := f.do(t, http.MethodPost, "/v1/ota/jobs", map[string]any{
		"tenant_id":    "acme",
		"artifact_id":  artifact.ArtifactID,
		"from_version": "1.4.2",
		"quiet_hours":  map[string]string{"start": "02:00", "end": "05:00"},
		// A 60-label store is far smaller than the fleet the default 1/5/25/100
		// schedule is sized for, where a 1% canary is half a million devices. A
		// coarser schedule is what a single-store rollout actually uses.
		"cohort_percentages":     []int{10, 50, 100},
		"health_gates":           map[string]any{"min_cohort_samples": 3},
		"max_concurrent_per_sec": 1000,
		"start":                  true,
	}, &job); code != http.StatusCreated {
		t.Fatalf("create job returned %d", code)
	}
	if job.State != domain.JobRunning {
		t.Fatalf("job state = %s, want running", job.State)
	}

	// Drive the first cohort.
	var tick app.TickResult
	if code := f.do(t, http.MethodPost, "/v1/ota/jobs/"+job.JobID+"/tick", nil, &tick); code != http.StatusOK {
		t.Fatalf("tick returned %d", code)
	}
	if tick.Dispatched == 0 {
		t.Fatalf("first cohort dispatched nothing: %s", tick.Reason)
	}

	// The live cohort progress must be visible on the job page.
	var jobView struct {
		CurrentWave int                   `json:"current_wave"`
		Waves       []domain.WaveProgress `json:"waves"`
	}
	if code := f.do(t, http.MethodGet, "/v1/ota/jobs/"+job.JobID, nil, &jobView); code != http.StatusOK {
		t.Fatalf("get job returned %d", code)
	}
	if len(jobView.Waves) != 3 || jobView.Waves[0].Dispatched != tick.Dispatched {
		t.Fatalf("job view = %+v", jobView)
	}

	// One device fails; the failed-device listing is what an operator opens.
	dispatched, err := f.ctrl.JobDevices(job.JobID, domain.StatusDispatched)
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	if code := f.do(t, http.MethodPost, "/v1/ota/results", map[string]any{
		"job_id": job.JobID, "device_id": dispatched[0].DeviceID,
		"store_id": "store-0042", "sec_id": string(dispatched[0].SECID),
		"status": "failed", "error": "flash write timeout", "to_version": "1.4.3",
	}, nil); code != http.StatusAccepted {
		t.Fatalf("record result returned %d", code)
	}
	var failed struct {
		Count   int `json:"count"`
		Devices []struct {
			DeviceID string `json:"device_id"`
			Error    string `json:"error"`
		} `json:"devices"`
	}
	if code := f.do(t, http.MethodGet, "/v1/ota/jobs/"+job.JobID+"/devices?status=failed", nil, &failed); code != http.StatusOK {
		t.Fatalf("list failed devices returned %d", code)
	}
	if failed.Count != 1 || failed.Devices[0].Error != "flash write timeout" {
		t.Fatalf("failed listing = %+v", failed)
	}

	// Pause, resume, abort all work through the API and are visible immediately.
	if code := f.do(t, http.MethodPost, "/v1/ota/jobs/"+job.JobID+"/pause", nil, nil); code != http.StatusOK {
		t.Fatalf("pause returned %d", code)
	}
	var paused struct {
		Job domain.Job `json:"job"`
	}
	f.do(t, http.MethodGet, "/v1/ota/jobs/"+job.JobID, nil, &paused)
	if paused.Job.State != domain.JobPaused {
		t.Fatalf("state after pause = %s", paused.Job.State)
	}
	if code := f.do(t, http.MethodPost, "/v1/ota/jobs/"+job.JobID+"/resume", nil, nil); code != http.StatusOK {
		t.Fatalf("resume returned %d", code)
	}
	if code := f.do(t, http.MethodPost, "/v1/ota/jobs/"+job.JobID+"/abort",
		map[string]string{"reason": "superseded"}, nil); code != http.StatusOK {
		t.Fatalf("abort returned %d", code)
	}
	var aborted struct {
		Job domain.Job `json:"job"`
	}
	f.do(t, http.MethodGet, "/v1/ota/jobs/"+job.JobID, nil, &aborted)
	if aborted.Job.State != domain.JobAborted {
		t.Fatalf("state after abort = %s", aborted.Job.State)
	}
}

func TestUnsignedAndMisSignedUploadsAreRefusedOverHTTP(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	image := []byte("firmware image")

	unsigned := f.uploadBody("1.4.3", image)
	delete(unsigned, "signature")
	if code := f.do(t, http.MethodPost, "/v1/firmware", unsigned, nil); code != http.StatusForbidden {
		t.Fatalf("unsigned upload returned %d, want 403", code)
	}

	_, rogue, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	misSigned := f.uploadBody("1.4.3", image)
	misSigned["signature"] = domain.SignArtifact(rogue, "1.4.3", f.tier, image)
	if code := f.do(t, http.MethodPost, "/v1/firmware", misSigned, nil); code != http.StatusForbidden {
		t.Fatalf("mis-signed upload returned %d, want 403", code)
	}

	// And no job can be created from an artifact that was never accepted.
	if code := f.do(t, http.MethodPost, "/v1/ota/jobs", map[string]any{
		"tenant_id": "acme", "artifact_id": domain.ArtifactIDFor(image),
	}, nil); code != http.StatusNotFound {
		t.Fatalf("job from an unaccepted artifact returned %d, want 404", code)
	}
}

func TestRolloutTargetsOnlyAddressableRegistryDevices(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Quarantine a label. The registry's decision must reach the rollout without
	// the OTA service reimplementing the rule.
	devices := f.registry.StoreDevices("store-0042")
	var quarantined string
	for _, d := range devices {
		if d.Kind == "label" {
			quarantined = d.ID
			break
		}
	}
	if err := f.registry.Quarantine(context.Background(), quarantined, "tamper flag"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	image := bytes.Repeat([]byte("firmware-v143-"), 2000)
	var artifact domain.Artifact
	if code := f.do(t, http.MethodPost, "/v1/firmware", f.uploadBody("1.4.3", image), &artifact); code != http.StatusCreated {
		t.Fatalf("upload returned %d", code)
	}
	var job domain.Job
	if code := f.do(t, http.MethodPost, "/v1/ota/jobs", map[string]any{
		"tenant_id": "acme", "artifact_id": artifact.ArtifactID, "from_version": "1.4.2",
		"cohort_percentages":     []int{100},
		"health_gates":           map[string]any{"min_cohort_samples": 3},
		"max_concurrent_per_sec": 1000,
		"start":                  true,
	}, &job); code != http.StatusCreated {
		t.Fatalf("create job returned %d", code)
	}
	var tick app.TickResult
	f.do(t, http.MethodPost, "/v1/ota/jobs/"+job.JobID+"/tick", nil, &tick)

	all, err := f.ctrl.JobDevices(job.JobID, "")
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	for _, d := range all {
		if d.DeviceID == quarantined {
			t.Fatalf("a quarantined device was addressed by a rollout")
		}
	}
	// Sixty labels were seeded and one is quarantined, leaving 59 addressable.
	// The controllers are updated by a different pipeline and must not appear at
	// all, and the seeded store's long-tailed battery distribution leaves a few
	// labels too flat to survive a download, which the rollout holds back rather
	// than finishing off.
	if len(all)+tick.SuppressedBattery != 59 {
		t.Fatalf("rollout touched %d devices and held back %d for battery, want 59 between them",
			len(all), tick.SuppressedBattery)
	}
	if tick.SuppressedBattery == 0 {
		t.Fatal("the seeded store should contain some nearly flat labels for the battery guard to hold back")
	}
}

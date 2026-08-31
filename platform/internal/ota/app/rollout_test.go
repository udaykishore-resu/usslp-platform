package app_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/ota/adapters"
	"github.com/usslp/usslp/platform/internal/ota/app"
	"github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/internal/ota/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// fakeClock is a manually advanced clock. Soak periods, silence windows and
// quiet hours are all time-dependent, and a test that slept through a
// thirty-minute soak would not be a test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t.UTC()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// capturedPublisher records everything handed to `ota-commands`.
type capturedPublisher struct {
	mu      sync.Mutex
	streams map[string][]canon.Envelope
}

func newCapturedPublisher() *capturedPublisher {
	return &capturedPublisher{streams: make(map[string][]canon.Envelope)}
}

func (p *capturedPublisher) PublishEvents(_ context.Context, stream string, envs ...canon.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streams[stream] = append(p.streams[stream], envs...)
	return nil
}

func (p *capturedPublisher) ofType(eventType string) []canon.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []canon.Envelope
	for _, e := range p.streams[canon.StreamOTA.Name] {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

// harness is a fully wired OTA controller with a fixed fleet.
type harness struct {
	t     *testing.T
	ctrl  *app.Controller
	clock *fakeClock
	pub   *capturedPublisher
	mqtt  *adapters.RecordingMessenger
	dir   *adapters.StaticDirectory
	store *eventstore.Store
	kv    *kvstore.Store
	files *adapters.MemoryArtifactStore
	dir1  string
	key   ed25519.PrivateKey
	ring  domain.KeyRing
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	return newHarnessWith(t, t.TempDir(),
		time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC),
		priv, domain.KeyRing{"fw-2026-a": pub},
		adapters.NewMemoryArtifactStore(), adapters.NewStaticDirectory())
}

func newHarnessWith(t *testing.T, dir string, at time.Time, key ed25519.PrivateKey,
	ring domain.KeyRing, files *adapters.MemoryArtifactStore, fleet *adapters.StaticDirectory) *harness {
	t.Helper()
	kv, err := kvstore.OpenWith(kvstore.Options{Dir: dir, Sync: kvstore.SyncEvery})
	if err != nil {
		t.Fatalf("open kvstore: %v", err)
	}
	es, err := eventstore.New(kv)
	if err != nil {
		t.Fatalf("open eventstore: %v", err)
	}
	clock := newFakeClock(at)
	pub := newCapturedPublisher()
	mqtt := adapters.NewRecordingMessenger()
	ctrl, err := app.Open(context.Background(), app.Config{
		Store: es, Artifacts: files, Keys: ring, Fleet: fleet,
		Events: pub, Messenger: mqtt, Clock: clock, Region: canon.Region("eu-west-1"),
	})
	if err != nil {
		t.Fatalf("open controller: %v", err)
	}
	h := &harness{
		t: t, ctrl: ctrl, clock: clock, pub: pub, mqtt: mqtt, dir: fleet,
		store: es, kv: kv, files: files, dir1: dir, key: key, ring: ring,
	}
	t.Cleanup(func() {
		_ = es.Close()
		_ = kv.Close()
	})
	return h
}

// reopen closes the stores and opens a fresh controller over the same
// directory, which is how the tests assert that a rollout survives a restart.
func (h *harness) reopen() *harness {
	h.t.Helper()
	if err := h.store.Close(); err != nil {
		h.t.Fatalf("close eventstore: %v", err)
	}
	if err := h.kv.Close(); err != nil {
		h.t.Fatalf("close kvstore: %v", err)
	}
	return newHarnessWith(h.t, h.dir1, h.clock.Now(), h.key, h.ring, h.files, h.dir)
}

// upload signs and stores a firmware image.
func (h *harness) upload(version domain.Version, tier string, image []byte) domain.Artifact {
	h.t.Helper()
	a, err := h.ctrl.UploadFirmware(context.Background(), domain.Artifact{
		Version:      version,
		HardwareTier: tier,
		Signature:    domain.SignArtifact(h.key, version, tier, image),
	}, image)
	if err != nil {
		h.t.Fatalf("upload %s: %v", version, err)
	}
	return a
}

// fleetOf builds n labels spread evenly across controllers.
func fleetOf(n, secs int, tier, firmware string, zone string) []ports.Target {
	out := make([]ports.Target, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ports.Target{
			DeviceID:        fmt.Sprintf("lbl-%05d", i),
			StoreID:         canon.StoreID("store-0042"),
			SECID:           canon.SECID(fmt.Sprintf("sec-%02d", i%secs)),
			HardwareTier:    tier,
			FirmwareVersion: firmware,
			BatteryPct:      85,
			TimeZone:        zone,
		})
	}
	return out
}

// reportAll records an outcome for every dispatched device in the current wave.
func (h *harness) reportAll(jobID string, status domain.DeviceStatus, failEvery int) (succeeded, failed int) {
	h.t.Helper()
	devices, err := h.ctrl.JobDevices(jobID, domain.StatusDispatched)
	if err != nil {
		h.t.Fatalf("list dispatched devices: %v", err)
	}
	for i, d := range devices {
		s := status
		if failEvery > 0 && i%failEvery == 0 {
			s = domain.StatusFailed
		}
		if err := h.ctrl.RecordOutcome(context.Background(), domain.DeviceUpdate{
			JobID: jobID, DeviceID: d.DeviceID, StoreID: d.StoreID, SECID: d.SECID,
			Wave: d.Wave, Status: s, At: h.clock.Now(),
		}); err != nil {
			h.t.Fatalf("record outcome for %s: %v", d.DeviceID, err)
		}
		if s == domain.StatusSucceeded {
			succeeded++
		} else {
			failed++
		}
	}
	return succeeded, failed
}

func (h *harness) tick(jobID string) *app.TickResult {
	h.t.Helper()
	res, err := h.ctrl.TickJob(context.Background(), jobID)
	if err != nil {
		h.t.Fatalf("tick: %v", err)
	}
	return res
}

func (h *harness) mustJob(jobID string) *domain.Job {
	h.t.Helper()
	job, err := h.ctrl.Job(jobID)
	if err != nil {
		h.t.Fatalf("job: %v", err)
	}
	return job
}

// ---------------------------------------------------------------------------
// Artifact management
// ---------------------------------------------------------------------------

func TestUploadRejectsATamperedArtifactAndAJobCannotBeCreatedFromIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	image := []byte("genuine firmware image")
	tampered := append([]byte(nil), image...)
	tampered[2] ^= 0xff

	_, err := h.ctrl.UploadFirmware(context.Background(), domain.Artifact{
		Version: "1.4.3", HardwareTier: "esl-2.9-bw",
		Signature: domain.SignArtifact(h.key, "1.4.3", "esl-2.9-bw", image),
	}, tampered)
	if !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}

	// The rejected image is not in the store, so there is no identifier a job
	// could be created from. That is the structural half of the guarantee: not
	// "we check again later", but "there is nothing to name".
	if _, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: domain.ArtifactIDFor(tampered),
	}); !errors.Is(err, domain.ErrArtifactNotFound) {
		t.Fatalf("error = %v, want ErrArtifactNotFound", err)
	}
}

func TestUploadIsIdempotentAndRefusesADuplicateVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	image := []byte("firmware v1")
	a := h.upload("1.4.3", "esl-2.9-bw", image)
	b := h.upload("1.4.3", "esl-2.9-bw", image)
	if a.ArtifactID != b.ArtifactID {
		t.Fatalf("re-upload produced %s then %s", a.ArtifactID, b.ArtifactID)
	}

	other := []byte("a different image claiming the same version")
	_, err := h.ctrl.UploadFirmware(context.Background(), domain.Artifact{
		Version: "1.4.3", HardwareTier: "esl-2.9-bw",
		Signature: domain.SignArtifact(h.key, "1.4.3", "esl-2.9-bw", other),
	}, other)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("error = %v; two different images must not both be version 1.4.3", err)
	}
}

func TestJobCreationPreparesADeltaWhenItIsWorthShipping(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := make([]byte, 120_000)
	for i := range base {
		base[i] = byte(i * 7 % 251)
	}
	target := append([]byte(nil), base...)
	copy(target[60_000:], []byte("this is the only changed region of the image"))

	h.upload("1.4.2", "esl-2.9-bw", base)
	newer := h.upload("1.4.3", "esl-2.9-bw", target)

	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: newer.ArtifactID,
		FromVersion: "1.4.2", UseDelta: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job.DeltaArtifactID == "" {
		t.Fatal("a one-region change should have produced a delta")
	}
	if job.DeltaSize >= job.ArtifactSize {
		t.Fatalf("delta is %d bytes against a %d-byte image", job.DeltaSize, job.ArtifactSize)
	}

	// The stored patch must actually reconstruct the target.
	patch, err := h.ctrl.ArtifactImage(job.DeltaArtifactID)
	if err != nil {
		t.Fatalf("read delta: %v", err)
	}
	got, err := domain.Apply(base, patch)
	if err != nil {
		t.Fatalf("apply stored delta: %v", err)
	}
	if string(got) != string(target) {
		t.Fatal("the stored delta does not reconstruct the target image")
	}
}

// ---------------------------------------------------------------------------
// Staged rollout
// ---------------------------------------------------------------------------

// runWaveToSuccess reports every dispatched device as succeeded and advances
// the clock past the soak period, then ticks.
func (h *harness) runWaveToSuccess(jobID string) *app.TickResult {
	h.t.Helper()
	h.reportAll(jobID, domain.StatusSucceeded, 0)
	h.clock.Advance(31 * time.Minute)
	return h.tick(jobID)
}

func TestStagedRolloutAdvancesThroughAllFourCohorts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.dir.Set(fleetOf(4000, 8, "esl-2.9-bw", "1.4.2", "UTC"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))

	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Gates: domain.HealthGates{MinCohortSamples: 5},
		// A generous per-controller cap so the test measures cohort gating
		// rather than bandwidth; the cap has its own test.
		MaxConcurrentPerSEC: 10000,
		Start:               true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	jobID := job.JobID

	seen := make([]int, 0, 4)
	for wave := 0; wave < 4; wave++ {
		res := h.tick(jobID)
		if res.Dispatched == 0 {
			t.Fatalf("wave %d dispatched nothing (%s)", wave, res.Reason)
		}
		current := h.mustJob(jobID)
		seen = append(seen, current.Cohorts[current.CurrentWave])
		t.Logf("wave %d (%d%%): dispatched %d", wave, current.Cohorts[current.CurrentWave], res.Dispatched)

		res = h.runWaveToSuccess(jobID)
		if wave < 3 {
			if res.Verdict != domain.VerdictAdvance {
				t.Fatalf("wave %d verdict = %s (%s), want advance", wave, res.Verdict, res.Reason)
			}
			if got := h.mustJob(jobID).CurrentWave; got != wave+1 {
				t.Fatalf("after wave %d the rollout is on wave %d", wave, got)
			}
		} else if res.Verdict != domain.VerdictComplete {
			t.Fatalf("final wave verdict = %s (%s), want complete", res.Verdict, res.Reason)
		}
	}

	if fmt.Sprint(seen) != fmt.Sprint([]int{1, 5, 25, 100}) {
		t.Fatalf("cohort schedule ran as %v, want [1 5 25 100]", seen)
	}
	final := h.mustJob(jobID)
	if final.State != domain.JobCompleted {
		t.Fatalf("final state = %s, want completed", final.State)
	}

	// Every wave transition must be on the stream, so a consumer outside this
	// service can follow the rollout without polling.
	advances := h.pub.ofType(canon.EvtOTACohortAdvanced)
	if len(advances) < 4 {
		t.Fatalf("published %d cohort-advance events, want at least 4", len(advances))
	}
	var first domain.CohortAdvanced
	if err := advances[0].Decode(&first); err != nil {
		t.Fatalf("decode cohort advance: %v", err)
	}
	if first.ToPercent != 5 || first.From != 0 || first.To != 1 {
		t.Fatalf("first advance = %+v", first)
	}
	if first.Metrics.Succeeded == 0 {
		t.Fatal("a cohort advance must carry the evidence for the decision, not only the decision")
	}
}

func TestRollbackFiresWhenTheFailureRateCrossesTheThreshold(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.dir.Set(fleetOf(4000, 8, "esl-2.9-bw", "1.4.2", "UTC"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("a bad firmware image"))

	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Gates:               domain.HealthGates{MinCohortSamples: 5},
		MaxConcurrentPerSEC: 10000,
		Start:               true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	jobID := job.JobID

	res := h.tick(jobID)
	if res.Dispatched == 0 {
		t.Fatal("first cohort dispatched nothing")
	}
	// One in ten fails: 10%, well over the 2% default.
	succeeded, failed := h.reportAll(jobID, domain.StatusSucceeded, 10)
	t.Logf("first cohort: %d succeeded, %d failed", succeeded, failed)

	res = h.tick(jobID)
	if res.Verdict != domain.VerdictRollback {
		t.Fatalf("verdict = %s (%s), want rollback", res.Verdict, res.Reason)
	}
	final := h.mustJob(jobID)
	if final.State != domain.JobHalted {
		t.Fatalf("state = %s, want halted", final.State)
	}
	if final.CurrentWave != 0 {
		t.Fatalf("the rollout advanced to wave %d despite a failed gate", final.CurrentWave)
	}

	events := h.pub.ofType(canon.EvtOTARolledBack)
	if len(events) != 1 {
		t.Fatalf("published %d rollback events, want 1", len(events))
	}
	var rb domain.RollbackTriggered
	if err := events[0].Decode(&rb); err != nil {
		t.Fatalf("decode rollback: %v", err)
	}
	if rb.Wave != 0 || rb.JobID != jobID {
		t.Fatalf("rollback event = %+v", rb)
	}
	if !strings.Contains(rb.Reason, "error rate") || !strings.Contains(rb.Reason, "%") {
		t.Fatalf("rollback reason %q must name the measurement and the threshold", rb.Reason)
	}
	if rb.Affected != succeeded {
		t.Fatalf("affected = %d, want the %d devices that took the new firmware", rb.Affected, succeeded)
	}

	// A halted rollout must not be restartable with a plain resume: the
	// controller stopped it, and a human has to decide.
	if err := h.ctrl.Resume(context.Background(), jobID, "operator"); !errors.Is(err, domain.ErrIllegalTransition) {
		t.Fatalf("resume on a halted rollout: error = %v, want ErrIllegalTransition", err)
	}
}

func TestRollbackReturnsDevicesToTheirPreviousFirmware(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.dir.Set(fleetOf(500, 4, "esl-2.9-bw", "1.4.2", "UTC"))
	h.upload("1.4.2", "esl-2.9-bw", []byte("firmware v1.4.2"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))

	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Gates: domain.HealthGates{MinCohortSamples: 3}, MaxConcurrentPerSEC: 10000, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	jobID := job.JobID
	h.tick(jobID)
	h.reportAll(jobID, domain.StatusSucceeded, 4)
	h.tick(jobID)
	if got := h.mustJob(jobID).State; got != domain.JobHalted {
		t.Fatalf("state = %s, want halted", got)
	}

	if err := h.ctrl.Rollback(context.Background(), jobID, "operator"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	final := h.mustJob(jobID)
	if final.State != domain.JobRolledBack {
		t.Fatalf("state = %s, want rolled_back", final.State)
	}
	rolled, err := h.ctrl.JobDevices(jobID, domain.StatusRolledBack)
	if err != nil {
		t.Fatalf("list rolled-back devices: %v", err)
	}
	if len(rolled) == 0 {
		t.Fatal("no devices were returned to their previous firmware")
	}
}

func TestPostUpdateSilenceHaltsARollout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.dir.Set(fleetOf(2000, 4, "esl-2.9-bw", "1.4.2", "UTC"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("an image that bricks the radio"))

	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Gates:               domain.HealthGates{MinCohortSamples: 3, SilenceWindow: 15 * time.Minute},
		MaxConcurrentPerSEC: 10000, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	jobID := job.JobID
	if res := h.tick(jobID); res.Dispatched == 0 {
		t.Fatal("first cohort dispatched nothing")
	}

	// Nobody reports anything. Every dispatched device simply goes quiet, which
	// is what an image that bricks the radio looks like — and which a
	// controller that only counted failures would read as a healthy cohort.
	h.clock.Advance(20 * time.Minute)
	res := h.tick(jobID)
	if res.MarkedSilent == 0 {
		t.Fatal("no devices were marked silent after the window elapsed")
	}
	if res.Verdict != domain.VerdictRollback {
		t.Fatalf("verdict = %s (%s), want rollback for post-update silence", res.Verdict, res.Reason)
	}
	if !strings.Contains(res.Reason, "silence") {
		t.Fatalf("reason %q should name silence", res.Reason)
	}
	if got := h.mustJob(jobID).State; got != domain.JobHalted {
		t.Fatalf("state = %s, want halted", got)
	}
}

func TestQuietHoursSuppressDispatchAcrossTwoTimeZones(t *testing.T) {
	t.Parallel()
	// 03:00 UTC in July. London is on BST (04:00 local): inside a 02:00-05:00
	// window. Los Angeles is on PDT (20:00 the previous evening): the shop is
	// open and its labels must not refresh.
	h := newHarnessAtTime(t, time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC))

	var fleet []ports.Target
	for i := 0; i < 400; i++ {
		fleet = append(fleet, ports.Target{
			DeviceID: fmt.Sprintf("lbl-ldn-%04d", i), StoreID: "store-london",
			SECID:        canon.SECID(fmt.Sprintf("sec-l%02d", i%4)),
			HardwareTier: "esl-2.9-bw", FirmwareVersion: "1.4.2",
			BatteryPct: 90, TimeZone: "Europe/London",
		})
	}
	for i := 0; i < 400; i++ {
		fleet = append(fleet, ports.Target{
			DeviceID: fmt.Sprintf("lbl-lax-%04d", i), StoreID: "store-losangeles",
			SECID:        canon.SECID(fmt.Sprintf("sec-x%02d", i%4)),
			HardwareTier: "esl-2.9-bw", FirmwareVersion: "1.4.2",
			BatteryPct: 90, TimeZone: "America/Los_Angeles",
		})
	}
	h.dir.Set(fleet)
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))

	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		QuietHours: domain.QuietHours{Start: "02:00", End: "05:00"},
		Cohorts:    []int{100},
		// A long silence window so that the London cohort dispatched in the
		// first window is still merely "waiting" when the Los Angeles window
		// opens eight hours later, and the test measures suppression rather
		// than the silence gate.
		Gates:               domain.HealthGates{MinCohortSamples: 5, SilenceWindow: 48 * time.Hour},
		MaxConcurrentPerSEC: 10000,
		Start:               true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	jobID := job.JobID

	res := h.tick(jobID)
	if res.SuppressedQuietHours == 0 {
		t.Fatal("nothing was suppressed; the Los Angeles store is in trading hours")
	}
	dispatched, err := h.ctrl.JobDevices(jobID, domain.StatusDispatched)
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	for _, d := range dispatched {
		if d.StoreID != "store-london" {
			t.Fatalf("%s in %s was dispatched at 20:00 local time", d.DeviceID, d.StoreID)
		}
	}
	if len(dispatched) != 400 {
		t.Fatalf("dispatched %d devices, want the 400 London labels", len(dispatched))
	}

	// Eight hours later it is 04:00 in Los Angeles and midday in London: the
	// windows have swapped, and the same job now reaches the other store.
	h.clock.Advance(8 * time.Hour)
	res = h.tick(jobID)
	if res.Dispatched != 400 {
		t.Fatalf("dispatched %d in the second window, want the 400 Los Angeles labels", res.Dispatched)
	}
	all, err := h.ctrl.JobDevices(jobID, "")
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if len(all) != 800 {
		t.Fatalf("%d devices touched in total, want 800", len(all))
	}
}

func TestBandwidthCapLimitsConcurrentDownloadsPerController(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// One controller, forty labels, a cap of four. A firmware image is about a
	// minute of exclusive airtime per label, and the mesh still has to carry
	// price updates.
	var fleet []ports.Target
	for i := 0; i < 40; i++ {
		fleet = append(fleet, ports.Target{
			DeviceID: fmt.Sprintf("lbl-%04d", i), StoreID: "store-0042", SECID: "sec-01",
			HardwareTier: "esl-2.9-bw", FirmwareVersion: "1.4.2", BatteryPct: 90, TimeZone: "UTC",
		})
	}
	h.dir.Set(fleet)
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))

	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Cohorts: []int{100}, Gates: domain.HealthGates{MinCohortSamples: 3},
		MaxConcurrentPerSEC: 4, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	jobID := job.JobID

	res := h.tick(jobID)
	if res.Dispatched != 4 {
		t.Fatalf("dispatched %d on one controller, want the cap of 4", res.Dispatched)
	}
	if res.SuppressedBandwidth != 36 {
		t.Fatalf("suppressed %d for bandwidth, want 36", res.SuppressedBandwidth)
	}

	// Ticking again while the four are still downloading changes nothing.
	if res := h.tick(jobID); res.Dispatched != 0 {
		t.Fatalf("dispatched %d more while the controller is saturated", res.Dispatched)
	}

	// As each download completes its slot frees.
	dispatched, err := h.ctrl.JobDevices(jobID, domain.StatusDispatched)
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	for _, d := range dispatched[:2] {
		if err := h.ctrl.RecordOutcome(context.Background(), domain.DeviceUpdate{
			JobID: jobID, DeviceID: d.DeviceID, SECID: d.SECID, StoreID: d.StoreID,
			Wave: d.Wave, Status: domain.StatusSucceeded, At: h.clock.Now(),
		}); err != nil {
			t.Fatalf("record outcome: %v", err)
		}
	}
	if res := h.tick(jobID); res.Dispatched != 2 {
		t.Fatalf("dispatched %d after two slots freed, want 2", res.Dispatched)
	}
}

func TestDispatchIsSuppressedForNearlyFlatBatteries(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	fleet := fleetOf(100, 2, "esl-2.9-bw", "1.4.2", "UTC")
	for i := range fleet {
		if i%4 == 0 {
			fleet[i].BatteryPct = 8
		}
	}
	h.dir.Set(fleet)
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))
	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Cohorts: []int{100}, Gates: domain.HealthGates{MinCohortSamples: 3},
		MaxConcurrentPerSEC: 10000, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	res := h.tick(job.JobID)
	if res.SuppressedBattery != 25 {
		t.Fatalf("suppressed %d for battery, want 25: a label that runs out mid-flash "+
			"is neither on the old firmware nor the new one", res.SuppressedBattery)
	}
	if res.Dispatched != 75 {
		t.Fatalf("dispatched %d, want 75", res.Dispatched)
	}
}

func TestPauseAndResumeAreDurableAcrossARestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.dir.Set(fleetOf(2000, 8, "esl-2.9-bw", "1.4.2", "UTC"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))
	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Gates: domain.HealthGates{MinCohortSamples: 5}, MaxConcurrentPerSEC: 10000, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	jobID := job.JobID

	h.tick(jobID)
	h.runWaveToSuccess(jobID)
	if got := h.mustJob(jobID).CurrentWave; got != 1 {
		t.Fatalf("wave = %d, want 1", got)
	}
	h.tick(jobID) // dispatch the second cohort
	dispatchedBefore, err := h.ctrl.JobDevices(jobID, domain.StatusDispatched)
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	if err := h.ctrl.Pause(context.Background(), jobID, "operator"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// The service is redeployed mid-rollout.
	h2 := h.reopen()
	after := h2.mustJob(jobID)
	if after.State != domain.JobPaused {
		t.Fatalf("state after restart = %s, want paused", after.State)
	}
	if after.CurrentWave != 1 {
		t.Fatalf("wave after restart = %d, want 1", after.CurrentWave)
	}
	if after.Waves[0].Succeeded != h.mustJob(jobID).Waves[0].Succeeded {
		t.Fatal("the first cohort's tally did not survive the restart")
	}
	dispatchedAfter, err := h2.ctrl.JobDevices(jobID, domain.StatusDispatched)
	if err != nil {
		t.Fatalf("list dispatched after restart: %v", err)
	}
	if len(dispatchedAfter) != len(dispatchedBefore) {
		t.Fatalf("dispatched devices went from %d to %d across a restart",
			len(dispatchedBefore), len(dispatchedAfter))
	}

	// A paused rollout dispatches nothing.
	if res := h2.tick(jobID); res.Dispatched != 0 {
		t.Fatalf("a paused rollout dispatched %d devices", res.Dispatched)
	}
	if err := h2.ctrl.Resume(context.Background(), jobID, "operator"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Cohort membership is a hash, so resuming continues with exactly the same
	// devices rather than reshuffling the fleet.
	stillDispatched, err := h2.ctrl.JobDevices(jobID, domain.StatusDispatched)
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	beforeIDs := map[string]bool{}
	for _, d := range dispatchedBefore {
		beforeIDs[d.DeviceID] = true
	}
	for _, d := range stillDispatched {
		if !beforeIDs[d.DeviceID] {
			t.Fatalf("%s appeared in the cohort after a restart; membership must be stable", d.DeviceID)
		}
	}
}

func TestAbortStopsARolloutPermanently(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.dir.Set(fleetOf(500, 4, "esl-2.9-bw", "1.4.2", "UTC"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))
	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Gates: domain.HealthGates{MinCohortSamples: 3}, MaxConcurrentPerSEC: 10000, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	h.tick(job.JobID)
	if err := h.ctrl.Abort(context.Background(), job.JobID, "operator", "superseded by 1.4.4"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got := h.mustJob(job.JobID).State; got != domain.JobAborted {
		t.Fatalf("state = %s, want aborted", got)
	}
	if res := h.tick(job.JobID); res.Dispatched != 0 {
		t.Fatalf("an aborted rollout dispatched %d devices", res.Dispatched)
	}
	if err := h.ctrl.Resume(context.Background(), job.JobID, "operator"); err == nil {
		t.Fatal("an aborted rollout was resumed")
	}
}

func TestTriggerIsPublishedAtQoS2AndNotRetained(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.dir.Set(fleetOf(200, 2, "esl-2.9-bw", "1.4.2", "UTC"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))
	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Cohorts: []int{100}, Gates: domain.HealthGates{MinCohortSamples: 3},
		MaxConcurrentPerSEC: 10000, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	h.tick(job.JobID)

	msgs := h.mqtt.Messages()
	if len(msgs) == 0 {
		t.Fatal("no OTA triggers were published")
	}
	for _, m := range msgs {
		// Interface contract §3: QoS 2, not retained, on the per-label zone topic.
		if m.QoS != msgbus.ExactlyOnce {
			t.Fatalf("trigger on %s published at QoS %d, want 2: a duplicated download "+
				"costs a battery-powered device days of its budget", m.Topic, m.QoS)
		}
		if m.Retain {
			t.Fatalf("trigger on %s was retained; it would be replayed to every label "+
				"that joins the zone afterwards", m.Topic)
		}
		scope, sec, label, leaf, ok := canon.ParseSECLabelTopic(m.Topic)
		if !ok || leaf != canon.LeafOTA {
			t.Fatalf("trigger topic %q is not a per-label OTA topic", m.Topic)
		}
		if scope.Tenant != "acme" || scope.Region != "eu-west-1" || sec == "" || label == "" {
			t.Fatalf("trigger topic %q has the wrong scope", m.Topic)
		}
	}

	// The payload must carry everything a device needs to verify what it
	// downloads without asking anything else.
	var env canon.Envelope
	if err := decodeJSON(msgs[0].Payload, &env); err != nil {
		t.Fatalf("decode trigger envelope: %v", err)
	}
	if env.EventType != canon.EvtOTADeviceUpdated {
		t.Fatalf("trigger event type = %s, want %s per contract §3", env.EventType, canon.EvtOTADeviceUpdated)
	}
	var update domain.DeviceUpdate
	if err := env.Decode(&update); err != nil {
		t.Fatalf("decode trigger payload: %v", err)
	}
	if update.SHA256 == "" || update.Signature == "" || update.ArtifactID == "" {
		t.Fatalf("trigger payload cannot be verified by the device: %+v", update)
	}
	if update.ToVersion != "1.4.3" {
		t.Fatalf("trigger targets %s", update.ToVersion)
	}
}

func TestDuplicateOutcomesAreNotDoubleCounted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.dir.Set(fleetOf(400, 2, "esl-2.9-bw", "1.4.2", "UTC"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))
	job, err := h.ctrl.CreateJob(context.Background(), app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Cohorts: []int{100}, Gates: domain.HealthGates{MinCohortSamples: 3},
		MaxConcurrentPerSEC: 10000, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	jobID := job.JobID
	h.tick(jobID)

	dispatched, err := h.ctrl.JobDevices(jobID, domain.StatusDispatched)
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	one := dispatched[0]
	for i := 0; i < 3; i++ {
		if err := h.ctrl.RecordOutcome(context.Background(), domain.DeviceUpdate{
			JobID: jobID, DeviceID: one.DeviceID, SECID: one.SECID, StoreID: one.StoreID,
			Wave: one.Wave, Status: domain.StatusSucceeded, At: h.clock.Now(),
		}); err != nil {
			t.Fatalf("record outcome %d: %v", i, err)
		}
	}
	if got := h.mustJob(jobID).Waves[0].Succeeded; got != 1 {
		t.Fatalf("three reports of one device counted as %d successes", got)
	}
}

// newHarnessAtTime builds a harness whose clock starts at a given instant.
func newHarnessAtTime(t *testing.T, at time.Time) *harness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	return newHarnessWith(t, t.TempDir(), at, priv, domain.KeyRing{"fw-2026-a": pub},
		adapters.NewMemoryArtifactStore(), adapters.NewStaticDirectory())
}

// decodeJSON is a small helper so the tests do not import encoding/json in
// every file that needs to look inside an envelope on the wire.
func decodeJSON(body []byte, dst any) error { return json.Unmarshal(body, dst) }

func TestFirmwareResultsArriveThroughTheMQTTSubscription(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.dir.Set(fleetOf(200, 2, "esl-2.9-bw", "1.4.2", "UTC"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("firmware v1.4.3"))
	job, err := h.ctrl.CreateJob(ctx, app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Cohorts: []int{100}, Gates: domain.HealthGates{MinCohortSamples: 3},
		MaxConcurrentPerSEC: 10000, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := h.ctrl.SubscribeResults(ctx); err != nil {
		t.Fatalf("subscribe to results: %v", err)
	}
	h.tick(job.JobID)

	dispatched, err := h.ctrl.JobDevices(job.JobID, domain.StatusDispatched)
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	one := dispatched[0]

	// The device reports on its own zone topic. Note that the payload below
	// deliberately claims a different device: the topic is authoritative,
	// because the broker's ACL already confines a device to its own topic and a
	// label must not be able to report an outcome on another's behalf.
	update := domain.DeviceUpdate{
		JobID: job.JobID, DeviceID: "lbl-somebody-else",
		Status: domain.StatusSucceeded, ToVersion: "1.4.3",
		BatteryPctBefore: 90, BatteryPctAfter: 87, DurationMS: 48_000, At: h.clock.Now(),
	}
	env, err := canon.NewEnvelope(canon.EvtOTADeviceUpdated, "ota-job", job.JobID, "acme", update)
	if err != nil {
		t.Fatalf("build result envelope: %v", err)
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("encode result envelope: %v", err)
	}
	topic := "usslp/acme/eu-west-1/store-0042/sec/" + string(one.SECID) +
		"/labels/" + one.DeviceID + "/" + domain.LeafOTAResult
	if !h.mqtt.Deliver(ctx, domain.FilterAllOTAResults, msgbus.Message{Topic: topic, Payload: body}) {
		t.Fatal("no handler was registered for the firmware-result filter")
	}

	succeeded, err := h.ctrl.JobDevices(job.JobID, domain.StatusSucceeded)
	if err != nil {
		t.Fatalf("list succeeded: %v", err)
	}
	if len(succeeded) != 1 || succeeded[0].DeviceID != one.DeviceID {
		t.Fatalf("result was attributed to %+v, want the device that owns the topic (%s)",
			succeeded, one.DeviceID)
	}

	// Garbage on the result topic is dropped rather than crashing the consumer.
	h.mqtt.Deliver(ctx, domain.FilterAllOTAResults, msgbus.Message{Topic: topic, Payload: []byte("not json")})
}

func TestBatteryDrainAnomaliesAreCountedFromDeviceReports(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.dir.Set(fleetOf(200, 2, "esl-2.9-bw", "1.4.2", "UTC"))
	artifact := h.upload("1.4.3", "esl-2.9-bw", []byte("an image with a broken sleep path"))
	job, err := h.ctrl.CreateJob(ctx, app.JobSpec{
		TenantID: "acme", ArtifactID: artifact.ArtifactID, FromVersion: "1.4.2",
		Cohorts: []int{100}, Gates: domain.HealthGates{MinCohortSamples: 3},
		MaxConcurrentPerSEC: 10000, Start: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	h.tick(job.JobID)

	dispatched, err := h.ctrl.JobDevices(job.JobID, domain.StatusDispatched)
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	// Every update succeeds, and every one of them costs far more battery than
	// a flash should. A controller that only counted failures would advance.
	for _, d := range dispatched {
		if err := h.ctrl.RecordOutcome(ctx, domain.DeviceUpdate{
			JobID: job.JobID, DeviceID: d.DeviceID, SECID: d.SECID, StoreID: d.StoreID,
			Wave: d.Wave, Status: domain.StatusSucceeded,
			BatteryPctBefore: 90, BatteryPctAfter: 60, At: h.clock.Now(),
		}); err != nil {
			t.Fatalf("record outcome: %v", err)
		}
	}
	res := h.tick(job.JobID)
	if res.Verdict != domain.VerdictRollback {
		t.Fatalf("verdict = %s (%s), want rollback for a battery-drain anomaly", res.Verdict, res.Reason)
	}
	if !strings.Contains(res.Reason, "battery") {
		t.Fatalf("reason %q should name the battery", res.Reason)
	}
}

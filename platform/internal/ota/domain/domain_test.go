package domain_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// ---------------------------------------------------------------------------
// Artifact signing
// ---------------------------------------------------------------------------

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	return pub, priv
}

func TestVerifyArtifactAcceptsAProperlySignedImage(t *testing.T) {
	t.Parallel()
	pub, priv := testKey(t)
	ring := domain.KeyRing{"fw-2026-a": pub}
	image := []byte("firmware image bytes")

	a := domain.Artifact{
		Version:      "1.4.3",
		HardwareTier: "esl-2.9-bw",
		Signature:    domain.SignArtifact(priv, "1.4.3", "esl-2.9-bw", image),
	}
	verified, err := domain.VerifyArtifact(ring, a, image)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.SigningKeyID != "fw-2026-a" {
		t.Fatalf("signing key = %s", verified.SigningKeyID)
	}
	if verified.ArtifactID != domain.ArtifactIDFor(image) {
		t.Fatalf("artifact id = %s, want the content address", verified.ArtifactID)
	}
	if verified.Size != int64(len(image)) {
		t.Fatalf("size = %d, want %d", verified.Size, len(image))
	}
}

func TestVerifyArtifactRejectsATamperedImage(t *testing.T) {
	t.Parallel()
	pub, priv := testKey(t)
	ring := domain.KeyRing{"fw-2026-a": pub}
	image := []byte("firmware image bytes")
	signature := domain.SignArtifact(priv, "1.4.3", "esl-2.9-bw", image)

	// One byte changed after signing. This is the case the whole pipeline
	// exists to catch: the bytes reach a device that has to be retrieved by
	// hand if they are wrong.
	tampered := append([]byte(nil), image...)
	tampered[3] ^= 0xff

	_, err := domain.VerifyArtifact(ring, domain.Artifact{
		Version: "1.4.3", HardwareTier: "esl-2.9-bw", Signature: signature,
	}, tampered)
	if !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}
}

func TestVerifyArtifactRejectsAnUnsignedImage(t *testing.T) {
	t.Parallel()
	pub, _ := testKey(t)
	ring := domain.KeyRing{"fw-2026-a": pub}
	_, err := domain.VerifyArtifact(ring, domain.Artifact{
		Version: "1.4.3", HardwareTier: "esl-2.9-bw",
	}, []byte("firmware"))
	if !errors.Is(err, domain.ErrUnsigned) {
		t.Fatalf("error = %v, want ErrUnsigned", err)
	}
}

func TestVerifyArtifactRejectsAForeignSigningKey(t *testing.T) {
	t.Parallel()
	pub, _ := testKey(t)
	_, rogue := testKey(t)
	ring := domain.KeyRing{"fw-2026-a": pub}
	image := []byte("firmware image bytes")

	_, err := domain.VerifyArtifact(ring, domain.Artifact{
		Version: "1.4.3", HardwareTier: "esl-2.9-bw",
		Signature: domain.SignArtifact(rogue, "1.4.3", "esl-2.9-bw", image),
	}, image)
	if !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}
}

func TestVerifyArtifactBindsTheHardwareTierAndVersion(t *testing.T) {
	t.Parallel()
	pub, priv := testKey(t)
	ring := domain.KeyRing{"fw-2026-a": pub}
	image := []byte("firmware image bytes")
	signature := domain.SignArtifact(priv, "1.4.3", "esl-2.9-bw", image)

	// The same signed image re-declared for a different display generation. A
	// 4.2-inch three-colour panel driven by a 2.9-inch monochrome waveform is a
	// brick, so the tier is inside the signature and this must fail.
	_, err := domain.VerifyArtifact(ring, domain.Artifact{
		Version: "1.4.3", HardwareTier: "esl-4.2-bwr", Signature: signature,
	}, image)
	if !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("re-declaring the hardware tier: error = %v, want ErrBadSignature", err)
	}

	// The same image re-declared as a different version, which is how a
	// downgrade would be smuggled past a version-based gate.
	_, err = domain.VerifyArtifact(ring, domain.Artifact{
		Version: "9.9.9", HardwareTier: "esl-2.9-bw", Signature: signature,
	}, image)
	if !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("re-declaring the version: error = %v, want ErrBadSignature", err)
	}
}

func TestVerifyArtifactRejectsADeclaredDigestThatDoesNotMatch(t *testing.T) {
	t.Parallel()
	pub, priv := testKey(t)
	ring := domain.KeyRing{"fw-2026-a": pub}
	image := []byte("firmware image bytes")
	_, err := domain.VerifyArtifact(ring, domain.Artifact{
		Version: "1.4.3", HardwareTier: "esl-2.9-bw",
		SHA256:    "0000000000000000000000000000000000000000000000000000000000000000",
		Signature: domain.SignArtifact(priv, "1.4.3", "esl-2.9-bw", image),
	}, image)
	if !errors.Is(err, domain.ErrDigestMismatch) {
		t.Fatalf("error = %v, want ErrDigestMismatch", err)
	}
}

func TestVerifyArtifactFailsClosedWithNoKeys(t *testing.T) {
	t.Parallel()
	_, priv := testKey(t)
	image := []byte("firmware")
	_, err := domain.VerifyArtifact(domain.KeyRing{}, domain.Artifact{
		Version: "1.0.0", HardwareTier: "esl-2.9-bw",
		Signature: domain.SignArtifact(priv, "1.0.0", "esl-2.9-bw", image),
	}, image)
	if !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("error = %v; a service with no signing keys must accept nothing", err)
	}
}

func TestVersionOrdering(t *testing.T) {
	t.Parallel()
	// The case string comparison gets backwards, and getting it backwards on an
	// OTA pipeline means re-flashing a fleet with older firmware.
	if domain.Version("1.10.0").Compare("1.9.0") <= 0 {
		t.Fatal("1.10.0 must sort after 1.9.0")
	}
	if domain.Version("2.0.0").Compare("10.0.0") >= 0 {
		t.Fatal("2.0.0 must sort before 10.0.0")
	}
	if domain.Version("1.4.3").Compare("1.4.3") != 0 {
		t.Fatal("equal versions must compare equal")
	}
	if domain.Version("not-a-version").Compare("0.0.1") >= 0 {
		t.Fatal("an unparseable version must never sort as the newest")
	}
}

// ---------------------------------------------------------------------------
// Cohort selection
// ---------------------------------------------------------------------------

func TestCohortMembershipIsDeterministic(t *testing.T) {
	t.Parallel()
	cohorts := domain.DefaultCohorts
	const job = "job-01HXYZ"

	for i := 0; i < 1000; i++ {
		device := fmt.Sprintf("lbl-%05d", i)
		first := domain.WaveFor(job, device, cohorts)
		for k := 0; k < 5; k++ {
			if got := domain.WaveFor(job, device, cohorts); got != first {
				t.Fatalf("%s moved between waves: %d then %d", device, first, got)
			}
		}
		if first < 0 || first >= len(cohorts) {
			t.Fatalf("%s landed in wave %d, outside the schedule", device, first)
		}
	}
}

func TestCohortSizesTrackTheSchedule(t *testing.T) {
	t.Parallel()
	cohorts := domain.DefaultCohorts
	const job = "job-01HXYZ"
	const n = 20000

	counts := make([]int, len(cohorts))
	for i := 0; i < n; i++ {
		counts[domain.WaveFor(job, fmt.Sprintf("lbl-%06d", i), cohorts)]++
	}
	cumulative := 0
	for i, pct := range cohorts {
		cumulative += counts[i]
		want := float64(n) * float64(pct) / 100
		got := float64(cumulative)
		// A hash is not a partition; a few tenths of a percent of drift on
		// twenty thousand devices is expected and harmless.
		if got < want*0.9 || got > want*1.1 {
			t.Fatalf("cumulative wave %d holds %.0f devices, want about %.0f (%d%%)", i, got, want, pct)
		}
	}
	if counts[0] == 0 {
		t.Fatal("the first cohort is empty; a 1% canary of 20,000 devices should hold about 200")
	}
}

func TestCohortMembershipDiffersBetweenJobs(t *testing.T) {
	t.Parallel()
	cohorts := domain.DefaultCohorts
	sameWave := 0
	const n = 2000
	for i := 0; i < n; i++ {
		device := fmt.Sprintf("lbl-%06d", i)
		if domain.WaveFor("job-a", device, cohorts) == domain.WaveFor("job-b", device, cohorts) {
			sameWave++
		}
	}
	// Two independent assignments agree on the large final cohort most of the
	// time; what must not happen is the *first* cohort being the same devices
	// every release, so that one store is the canary forever.
	firstCohortA := map[string]bool{}
	overlap := 0
	for i := 0; i < n; i++ {
		device := fmt.Sprintf("lbl-%06d", i)
		if domain.WaveFor("job-a", device, cohorts) == 0 {
			firstCohortA[device] = true
		}
	}
	for i := 0; i < n; i++ {
		device := fmt.Sprintf("lbl-%06d", i)
		if domain.WaveFor("job-b", device, cohorts) == 0 && firstCohortA[device] {
			overlap++
		}
	}
	if len(firstCohortA) > 0 && overlap == len(firstCohortA) {
		t.Fatal("two jobs picked exactly the same canary cohort; one store would be the canary forever")
	}
	if sameWave == n {
		t.Fatal("cohort assignment does not depend on the job at all")
	}
}

func TestInOrBefore(t *testing.T) {
	t.Parallel()
	cohorts := []int{10, 100}
	const job = "job-x"
	device := "lbl-000001"
	wave := domain.WaveFor(job, device, cohorts)
	if !domain.InOrBefore(job, device, cohorts, wave) {
		t.Fatal("a device must be inside its own wave's cumulative boundary")
	}
	if wave > 0 && domain.InOrBefore(job, device, cohorts, wave-1) {
		t.Fatal("a device must not be inside an earlier wave's boundary")
	}
}

func TestValidateCohorts(t *testing.T) {
	t.Parallel()
	if err := domain.ValidateCohorts(domain.DefaultCohorts); err != nil {
		t.Fatalf("the default schedule must validate: %v", err)
	}
	for _, bad := range [][]int{
		nil,
		{},
		{0, 100},
		{5, 5, 100},
		{25, 5, 100},
		{1, 5, 25},  // never reaches the whole fleet
		{1, 5, 120}, // beyond the fleet
		{-1, 100},
	} {
		if err := domain.ValidateCohorts(bad); err == nil {
			t.Fatalf("schedule %v was accepted", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Quiet hours
// ---------------------------------------------------------------------------

func TestQuietHoursAcrossTwoTimeZones(t *testing.T) {
	t.Parallel()
	window := domain.QuietHours{Start: "02:00", End: "05:00"}
	london := window.InZone("Europe/London")
	losAngeles := window.InZone("America/Los_Angeles")

	// 03:00 UTC in mid-summer. London is on BST (UTC+1), so it is 04:00 there
	// — inside the window. Los Angeles is on PDT (UTC-7), so it is 20:00 the
	// previous evening — the shop is open and the labels must not refresh.
	at := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	if !london.Allows(at) {
		t.Fatalf("London at %s (04:00 local) must be inside a 02:00-05:00 window", at)
	}
	if losAngeles.Allows(at) {
		t.Fatalf("Los Angeles at %s (20:00 local) must be outside a 02:00-05:00 window", at)
	}

	// Ten hours later it is Los Angeles's turn and London's shops are open.
	at = time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	if london.Allows(at) {
		t.Fatalf("London at %s (11:00 local) must be outside the window", at)
	}
	if !losAngeles.Allows(at) {
		t.Fatalf("Los Angeles at %s (03:00 local) must be inside the window", at)
	}
}

func TestQuietHoursFollowDaylightSaving(t *testing.T) {
	t.Parallel()
	london := domain.QuietHours{Start: "02:00", End: "05:00", TimeZone: "Europe/London"}

	// 02:30 UTC in January is 02:30 in London (GMT): inside the window.
	winter := time.Date(2026, 1, 15, 2, 30, 0, 0, time.UTC)
	if !london.Allows(winter) {
		t.Fatal("a London store must be inside its window at 02:30 GMT")
	}
	// The same UTC instant in July is 03:30 BST: still inside.
	summer := time.Date(2026, 7, 15, 2, 30, 0, 0, time.UTC)
	if !london.Allows(summer) {
		t.Fatal("a London store must be inside its window at 03:30 BST")
	}
	// 01:30 UTC in July is 02:30 BST: inside. In January it is 01:30 GMT:
	// outside. A stored UTC offset would get exactly this backwards.
	edge := time.Date(2026, 7, 15, 1, 30, 0, 0, time.UTC)
	if !london.Allows(edge) {
		t.Fatal("01:30 UTC in July is 02:30 in London and must be allowed")
	}
	edgeWinter := time.Date(2026, 1, 15, 1, 30, 0, 0, time.UTC)
	if london.Allows(edgeWinter) {
		t.Fatal("01:30 UTC in January is 01:30 in London and must be refused")
	}
}

func TestQuietHoursWrappingMidnight(t *testing.T) {
	t.Parallel()
	overnight := domain.QuietHours{Start: "22:00", End: "06:00", TimeZone: "UTC"}
	for _, tc := range []struct {
		hour  int
		allow bool
	}{{23, true}, {2, true}, {5, true}, {6, false}, {12, false}, {21, false}, {22, true}} {
		at := time.Date(2026, 3, 1, tc.hour, 0, 0, 0, time.UTC)
		if got := overnight.Allows(at); got != tc.allow {
			t.Fatalf("%02d:00 allowed = %v, want %v for a 22:00-06:00 window", tc.hour, got, tc.allow)
		}
	}
}

func TestQuietHoursUnsetAllowsEverything(t *testing.T) {
	t.Parallel()
	if !domain.AlwaysAllowed.Allows(time.Now()) {
		t.Fatal("an unset window must allow delivery at any time")
	}
	if domain.AlwaysAllowed.Configured() {
		t.Fatal("an unset window must not report itself as configured")
	}
}

func TestQuietHoursFailClosedOnAnUnresolvableZone(t *testing.T) {
	t.Parallel()
	bad := domain.QuietHours{Start: "02:00", End: "05:00", TimeZone: "Mars/Olympus_Mons"}
	if bad.Allows(time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatal("an unresolvable time zone must refuse delivery rather than silently widening the window")
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("an unresolvable time zone must fail validation")
	}
}

func TestQuietHoursNextOpen(t *testing.T) {
	t.Parallel()
	window := domain.QuietHours{Start: "02:00", End: "05:00", TimeZone: "UTC"}
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	next, ok := window.NextOpen(at)
	if !ok {
		t.Fatal("a daily window must always have a next opening")
	}
	if !next.After(at) || !window.Allows(next) {
		t.Fatalf("next open = %s, which is not a later instant inside the window", next)
	}
	if next.Hour() != 2 {
		t.Fatalf("next open = %s, want 02:00", next)
	}
}

func TestQuietHoursRoundTripThroughItsStringForm(t *testing.T) {
	t.Parallel()
	original := domain.QuietHours{Start: "02:00", End: "05:00", TimeZone: "Europe/London"}
	parsed, err := domain.ParseQuietHours(original.String())
	if err != nil {
		t.Fatalf("parse %q: %v", original.String(), err)
	}
	if parsed != original {
		t.Fatalf("round trip produced %+v, want %+v", parsed, original)
	}
	if empty, err := domain.ParseQuietHours(""); err != nil || empty.Configured() {
		t.Fatalf("an empty string must parse to an unconfigured window: %+v %v", empty, err)
	}
}

// ---------------------------------------------------------------------------
// Bandwidth budget
// ---------------------------------------------------------------------------

func TestBandwidthBudgetCapsPerController(t *testing.T) {
	t.Parallel()
	b := domain.NewBandwidthBudget(4)
	for i := 0; i < 4; i++ {
		if !b.Acquire("sec-01", canon.LabelID(fmt.Sprintf("lbl-%d", i))) {
			t.Fatalf("slot %d on sec-01 was refused inside the cap", i)
		}
	}
	if b.Acquire("sec-01", "lbl-4") {
		t.Fatal("a fifth concurrent download on one controller was allowed; " +
			"the mesh has to keep carrying price updates")
	}
	// A different controller has its own airtime.
	if !b.Acquire("sec-02", "lbl-9") {
		t.Fatal("a second controller must have its own budget")
	}
	if b.InFlight("sec-01") != 4 || b.InFlight("sec-02") != 1 {
		t.Fatalf("in flight = %d/%d", b.InFlight("sec-01"), b.InFlight("sec-02"))
	}
	// Re-acquiring for a device that already holds a slot must not consume a
	// second one, or a flapping label would monopolise a controller.
	if !b.Acquire("sec-01", "lbl-0") {
		t.Fatal("re-acquiring for a device that already holds a slot was refused")
	}
	if b.InFlight("sec-01") != 4 {
		t.Fatalf("re-acquire changed the count to %d", b.InFlight("sec-01"))
	}

	b.Release("sec-01", "lbl-0")
	if b.InFlight("sec-01") != 3 {
		t.Fatalf("after release, in flight = %d", b.InFlight("sec-01"))
	}
	if !b.Acquire("sec-01", "lbl-4") {
		t.Fatal("a freed slot was not reusable")
	}
	if b.Total() != 5 {
		t.Fatalf("total in flight = %d, want 5", b.Total())
	}
	b.Reset()
	if b.Total() != 0 {
		t.Fatalf("after reset, total = %d", b.Total())
	}
}

// ---------------------------------------------------------------------------
// Rollout gates
// ---------------------------------------------------------------------------

func newTestJob(cohorts []int) *domain.Job {
	j := &domain.Job{
		JobID: "job-1", TenantID: "acme", HardwareTier: "esl-2.9-bw",
		ToVersion: "1.4.3", ArtifactID: "sha256:aa", Signature: "sig",
		Cohorts: cohorts, Gates: domain.DefaultHealthGates(), State: domain.JobRunning,
	}
	j.InitWaves()
	return j
}

func TestGatesRollBackWhenTheErrorRateCrossesTheThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	j := newTestJob(domain.DefaultCohorts)
	w := j.Wave()
	w.Targeted, w.Dispatched = 100, 100
	w.Succeeded, w.Failed = 97, 3 // 3% > the 2% default

	res := j.EvaluateGates(now)
	if res.Verdict != domain.VerdictRollback {
		t.Fatalf("verdict = %s, want rollback; reason %q", res.Verdict, res.Reason)
	}
	if res.Reason == "" {
		t.Fatal("a rollback must name the measurement and the threshold")
	}
}

func TestGatesTolerateFailuresBelowTheThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	j := newTestJob(domain.DefaultCohorts)
	w := j.Wave()
	w.Targeted, w.Dispatched = 1000, 1000
	w.Succeeded, w.Failed = 990, 10 // 1% < 2%
	w.SatisfiedAt = now.Add(-time.Hour)

	res := j.EvaluateGates(now)
	if res.Verdict != domain.VerdictAdvance {
		t.Fatalf("verdict = %s (%s), want advance: a handful of flat batteries must not halt a rollout",
			res.Verdict, res.Reason)
	}
}

func TestGatesPreferRollbackOverAdvanceOnAMixedCohort(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	j := newTestJob(domain.DefaultCohorts)
	w := j.Wave()
	w.Targeted, w.Dispatched = 100, 100
	// 96% success looks like a pass to a naive gate, but four devices in every
	// hundred did not come back.
	w.Succeeded, w.BootFailed = 96, 4
	w.SatisfiedAt = now.Add(-time.Hour)

	res := j.EvaluateGates(now)
	if res.Verdict != domain.VerdictRollback {
		t.Fatalf("verdict = %s (%s), want rollback", res.Verdict, res.Reason)
	}
}

func TestGatesCatchPostUpdateSilence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	j := newTestJob(domain.DefaultCohorts)
	w := j.Wave()
	w.Targeted, w.Dispatched = 100, 100
	// Everything that reported, succeeded. Ten devices reported nothing at all,
	// which is what a bricked radio looks like.
	w.Succeeded, w.Silent = 90, 10

	res := j.EvaluateGates(now)
	if res.Verdict != domain.VerdictRollback {
		t.Fatalf("verdict = %s (%s), want rollback for post-update silence", res.Verdict, res.Reason)
	}
}

func TestGatesCatchBatteryDrainAnomalies(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	j := newTestJob(domain.DefaultCohorts)
	w := j.Wave()
	w.Targeted, w.Dispatched = 100, 100
	w.Succeeded = 100
	w.BatteryAnomalies = 10 // 10% > the 5% default
	w.SatisfiedAt = now.Add(-time.Hour)

	res := j.EvaluateGates(now)
	if res.Verdict != domain.VerdictRollback {
		t.Fatalf("verdict = %s (%s), want rollback for a battery-drain anomaly", res.Verdict, res.Reason)
	}
}

func TestGatesRequireASampleBeforeBelievingARate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	j := newTestJob(domain.DefaultCohorts)
	w := j.Wave()
	w.Targeted, w.Dispatched = 50, 50
	// One failure out of three reports reads as 33%, which is not evidence of
	// anything with 47 devices still to report.
	w.Succeeded, w.Failed = 2, 1

	res := j.EvaluateGates(now)
	if res.Verdict == domain.VerdictRollback {
		t.Fatalf("rolled back on three samples: %s", res.Reason)
	}
}

func TestGatesEnforceTheSoakPeriod(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	j := newTestJob(domain.DefaultCohorts)
	w := j.Wave()
	w.Targeted, w.Dispatched, w.Succeeded = 100, 100, 100
	w.SatisfiedAt = now.Add(-5 * time.Minute)

	if res := j.EvaluateGates(now); res.Verdict != domain.VerdictWait {
		t.Fatalf("verdict = %s, want wait: the cohort has soaked 5 of 30 minutes", res.Verdict)
	}
	if res := j.EvaluateGates(now.Add(30 * time.Minute)); res.Verdict != domain.VerdictAdvance {
		t.Fatalf("verdict = %s (%s), want advance after the soak", res.Verdict, res.Reason)
	}
}

func TestGatesCompleteOnTheFinalCohort(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	j := newTestJob(domain.DefaultCohorts)
	j.CurrentWave = len(j.Cohorts) - 1
	w := j.Wave()
	w.Targeted, w.Dispatched, w.Succeeded = 100, 100, 100
	w.SatisfiedAt = now.Add(-time.Hour)

	if res := j.EvaluateGates(now); res.Verdict != domain.VerdictComplete {
		t.Fatalf("verdict = %s, want complete on the last cohort", res.Verdict)
	}
}

func TestJobLifecycleTransitions(t *testing.T) {
	t.Parallel()
	legal := [][2]domain.JobState{
		{domain.JobPending, domain.JobRunning},
		{domain.JobRunning, domain.JobPaused},
		{domain.JobPaused, domain.JobRunning},
		{domain.JobRunning, domain.JobHalted},
		{domain.JobHalted, domain.JobRollingBack},
		{domain.JobRollingBack, domain.JobRolledBack},
		{domain.JobRunning, domain.JobCompleted},
		{domain.JobRunning, domain.JobAborted},
	}
	for _, tc := range legal {
		if !domain.CanTransitionJob(tc[0], tc[1]) {
			t.Fatalf("%s → %s should be legal", tc[0], tc[1])
		}
	}
	illegal := [][2]domain.JobState{
		{domain.JobHalted, domain.JobRunning},
		{domain.JobCompleted, domain.JobRunning},
		{domain.JobAborted, domain.JobRunning},
		{domain.JobRolledBack, domain.JobRunning},
		{domain.JobPending, domain.JobCompleted},
		{domain.JobPaused, domain.JobCompleted},
	}
	for _, tc := range illegal {
		if domain.CanTransitionJob(tc[0], tc[1]) {
			t.Fatalf("%s → %s must not be legal", tc[0], tc[1])
		}
	}

	j := newTestJob(domain.DefaultCohorts)
	j.State = domain.JobCompleted
	if err := j.TransitionTo(domain.JobRunning, "", time.Now()); !errors.Is(err, domain.ErrIllegalTransition) {
		t.Fatalf("error = %v, want ErrIllegalTransition", err)
	}
}

func TestJobValidationRefusesADowngradeAndAnUnsignedArtifact(t *testing.T) {
	t.Parallel()
	base := func() *domain.Job {
		return &domain.Job{
			JobID: "job-1", TenantID: "acme", HardwareTier: "esl-2.9-bw",
			FromVersion: "1.5.0", ToVersion: "1.4.0",
			ArtifactID: "sha256:aa", Signature: "sig", Cohorts: domain.DefaultCohorts,
		}
	}
	if err := base().Validate(); err == nil {
		t.Fatal("a rollout that installs an older version than it targets was accepted")
	}
	unsigned := base()
	unsigned.ToVersion, unsigned.Signature = "1.6.0", ""
	if err := unsigned.Validate(); !errors.Is(err, domain.ErrUnsigned) {
		t.Fatalf("error = %v, want ErrUnsigned", err)
	}
}

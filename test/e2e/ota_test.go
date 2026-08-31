package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestOTARollbackOnCohortFailure is the safety claim for firmware.
//
// A label that does not boot is a person walking an aisle with a screwdriver,
// so the rollout's safety properties are the product: cohorts start small, four
// independent health signals are watched, and a bad image halts itself without
// waiting for a human. This test rolls a signed image out, lets the first
// cohort succeed, fails the second, and asserts that:
//
//   - the rollout rolled itself back rather than continuing;
//   - no further cohort was dispatched after the failure;
//   - the devices that had not been updated are still on the working firmware.
func TestOTARollbackOnCohortFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("a staged rollout with a soak window; -short skips it")
	}
	st := newStack(t, smallStore(2, 20))
	store := st.Stores()[0]

	// A signed artifact. The signature is genuine and is verified at upload
	// against the ring the service was configured with; there is no path to
	// rolling out an image nobody verified, because a job can only name an
	// artifact that passed that check.
	image := bytes.Repeat([]byte("USSLP-FIRMWARE-1.5.0\n"), 512)
	sum := sha256.Sum256(image)
	artifact := uploadFirmware(t, st, map[string]any{
		"version": "1.5.0", "hardware_tier": stack.HardwareTier,
		"sha256":      hex.EncodeToString(sum[:]),
		"signature":   st.SignFirmware("1.5.0", stack.HardwareTier, image),
		"image":       base64.StdEncoding.EncodeToString(image),
		"uploaded_by": "e2e",
	})
	t.Logf("uploaded artifact %s (%s, %d bytes)", artifact["artifact_id"], artifact["version"], len(image))

	// An unsigned image must be refused outright: the check on the check.
	rejected := uploadFirmwareRaw(t, st, map[string]any{
		"version": "9.9.9", "hardware_tier": stack.HardwareTier,
		"sha256": hex.EncodeToString(sum[:]), "signature": "",
		"image": base64.StdEncoding.EncodeToString(image),
	})
	if rejected < 400 {
		t.Errorf("an unsigned firmware image was accepted with status %d", rejected)
	}

	// Two cohorts: a small first wave and everything else. The gates are the
	// platform's defaults with the soak and sample thresholds shortened, so the
	// test measures the rollback logic rather than the clock.
	job := createOTAJob(t, st, map[string]any{
		"tenant_id": store.Tenant, "stores": []canon.StoreID{store.ID},
		"artifact_id":  artifact["artifact_id"],
		"from_version": stack.InitialFirmware,
		// Twenty-five percent rather than ten. The first cohort has to contain
		// devices for the test to mean anything, and a percentage of a fleet
		// this size rounds: ten percent of the labels that share the target
		// hardware tier is four on a good day and can be zero when the fleet is
		// momentarily smaller. A canary that is sometimes empty is a test that
		// sometimes asserts nothing.
		"cohort_percentages": []int{25, 100},
		"health_gates": map[string]any{
			"max_error_rate": 0.02, "max_boot_failure_rate": 0.01,
			"max_silence_rate": 0.5, "max_battery_anomaly_rate": 0.5,
			"min_success_rate": 0.9, "min_cohort_samples": 2,
			// A one-nanosecond soak rather than zero: the gates treat a
			// non-positive value as "unset" and substitute the platform
			// default, which is measured in tens of minutes. The soak is a real
			// and important gate — battery drain and watchdog loops take time to
			// appear — and this test is about the rollback decision, not about
			// waiting for it.
			"soak_duration": 1, "silence_window": int64(time.Hour),
		},
		"created_by": "e2e", "start": true,
	})
	jobID := job["job_id"].(string)
	t.Logf("started rollout %s: %v -> 1.5.0", jobID, stack.InitialFirmware)

	// Cohort 0. The controller dispatches in batches bounded by
	// MaxConcurrentPerSEC — a label downloading firmware is the most expensive
	// thing it ever does with its radio — so the cohort is drained rather than
	// read once.
	wave0 := drainWave(t, st, jobID, store, 0, "succeeded", "", 60*time.Second)
	if wave0 == 0 {
		job := getOTAJob(t, st, jobID)
		t.Fatalf("the first cohort dispatched no devices; the rollout is in state %v "+
			"at wave %v. A rollback test needs a canary that actually flew",
			job["state"], job["current_wave"])
	}
	t.Logf("cohort 0: %d devices updated successfully", wave0)

	// Cohort 1 follows once cohort 0 has cleared its gates. Every device in it
	// reports a boot failure — the worst signal the platform has, a device that
	// took the image and did not come back.
	awaitWave(t, st, jobID, 1, 60*time.Second)
	wave1 := drainWave(t, st, jobID, store, 1, "boot_failed", "watchdog reset loop after flash", 30*time.Second)
	if wave1 == 0 {
		t.Fatal("the second cohort dispatched no devices")
	}
	t.Logf("cohort 1: %d devices reported a boot failure", wave1)

	// The rollout must halt or roll back on its own.
	var final map[string]any
	eventually(t, 60*time.Second, "the rollout to stop itself", func() bool {
		final = getOTAJob(t, st, jobID)
		state, _ := final["state"].(string)
		return state == "rolled_back" || state == "rolling_back" || state == "halted"
	})
	state, _ := final["state"].(string)
	t.Logf("rollout ended in state %q: %v", state, final["halt_reason"])
	if state == "completed" {
		t.Fatal("a rollout whose second cohort failed to boot completed anyway")
	}

	// No third cohort. There were only two here, so the assertion is that the
	// job never advanced past wave 1 and never dispatched anything more.
	if wave, ok := final["current_wave"].(float64); ok && int(wave) > 1 {
		t.Errorf("the rollout advanced to wave %d after a cohort failed to boot", int(wave))
	}
	// No further device is given the bad image. Devices that were already
	// downloading when the halt fired stay "dispatched" — the platform cannot
	// un-send a frame — so the assertion is that the count stops growing, which
	// is what "no further cohort is dispatched" actually means.
	atHalt := len(devicesWithStatus(t, st, jobID, "dispatched"))
	time.Sleep(4 * time.Second) // several control-loop ticks
	afterHalt := len(devicesWithStatus(t, st, jobID, "dispatched"))
	if afterHalt > atHalt {
		t.Errorf("the rollout dispatched %d more devices after halting (%d -> %d)",
			afterHalt-atHalt, atHalt, afterHalt)
	}
	t.Logf("%d devices were in flight when the rollout halted; none were added afterwards", atHalt)

	// The devices that were never updated are still on the firmware that works.
	stillGood := 0
	for _, z := range store.Zones {
		for _, id := range z.Labels() {
			dev := st.Services().Registry().Device(string(id))
			if dev == nil {
				continue
			}
			if dev.FirmwareVersion == stack.InitialFirmware {
				stillGood++
			}
		}
	}
	t.Logf("%d labels are still on the working firmware %s", stillGood, stack.InitialFirmware)
	if stillGood == 0 {
		t.Error("no label is still on the working firmware; the rollback protected nothing")
	}
}

// ---------------------------------------------------------------------------
// OTA service HTTP helpers
// ---------------------------------------------------------------------------

func uploadFirmware(t *testing.T, st *stack.Stack, body map[string]any) map[string]any {
	t.Helper()
	status, out := otaPost(t, st, "/v1/firmware", body)
	if status != http.StatusCreated {
		t.Fatalf("uploading firmware: status %d: %v", status, out)
	}
	return out
}

func uploadFirmwareRaw(t *testing.T, st *stack.Stack, body map[string]any) int {
	t.Helper()
	status, _ := otaPost(t, st, "/v1/firmware", body)
	return status
}

func createOTAJob(t *testing.T, st *stack.Stack, spec map[string]any) map[string]any {
	t.Helper()
	status, out := otaPost(t, st, "/v1/ota/jobs", spec)
	if status != http.StatusCreated {
		t.Fatalf("creating the rollout: status %d: %v", status, out)
	}
	return out
}

func reportOTAResult(t *testing.T, st *stack.Stack, jobID string, store *stack.Store,
	device canon.LabelID, wave int, status, detail string) {
	t.Helper()
	code, out := otaPost(t, st, "/v1/ota/results", map[string]any{
		"job_id": jobID, "device_id": device, "tenant_id": store.Tenant,
		"store_id": store.ID, "wave": wave, "status": status,
		"from_version": stack.InitialFirmware, "to_version": "1.5.0",
		"error": detail,
	})
	if code != http.StatusAccepted {
		t.Fatalf("recording an OTA result for %s: status %d: %v", device, code, out)
	}
}

func getOTAJob(t *testing.T, st *stack.Stack, jobID string) map[string]any {
	t.Helper()
	status, out := otaGet(t, st, "/v1/ota/jobs/"+jobID)
	if status != http.StatusOK {
		t.Fatalf("reading rollout %s: status %d: %v", jobID, status, out)
	}
	job, _ := out["job"].(map[string]any)
	if job == nil {
		t.Fatalf("the rollout response carried no job: %v", out)
	}
	if wave, ok := out["current_wave"]; ok {
		job["current_wave"] = wave
	}
	return job
}

// devicesWithStatus lists the devices in a rollout in one status.
func devicesWithStatus(t *testing.T, st *stack.Stack, jobID, status string) []canon.LabelID {
	t.Helper()
	code, out := otaGet(t, st, "/v1/ota/jobs/"+jobID+"/devices?status="+status)
	if code != http.StatusOK {
		t.Fatalf("listing rollout devices: status %d: %v", code, out)
	}
	list, _ := out["devices"].([]any)
	ids := make([]canon.LabelID, 0, len(list))
	for _, d := range list {
		m, _ := d.(map[string]any)
		if id, ok := m["device_id"].(string); ok {
			ids = append(ids, canon.LabelID(id))
		}
	}
	return ids
}

// awaitWave waits for the rollout to reach a cohort index.
func awaitWave(t *testing.T, st *stack.Stack, jobID string, wave int, within time.Duration) {
	t.Helper()
	eventually(t, within, "the rollout to reach the next cohort", func() bool {
		job := getOTAJob(t, st, jobID)
		w, _ := job["current_wave"].(float64)
		return int(w) >= wave
	})
}

// drainWave reports an outcome for every device the controller dispatches in
// one cohort, until it stops dispatching.
//
// Reading the dispatched set once is not enough: the rollout releases devices
// in batches bounded by the per-controller concurrency cap, so a cohort arrives
// over several ticks. Draining is also what a real fleet looks like — outcomes
// trickle back as devices finish downloading.
func drainWave(t *testing.T, st *stack.Stack, jobID string, store *stack.Store,
	wave int, status, detail string, within time.Duration) int {
	t.Helper()
	reported := map[canon.LabelID]bool{}
	// sweep reports every device that is dispatched and has not been answered
	// yet, and says how many were new.
	sweep := func() int {
		fresh := 0
		for _, id := range devicesWithStatus(t, st, jobID, "dispatched") {
			if reported[id] {
				continue
			}
			reported[id] = true
			fresh++
			reportOTAResult(t, st, jobID, store, id, wave, status, detail)
		}
		return fresh
	}

	deadline := time.Now().Add(within)
	idle := 0
	for time.Now().Before(deadline) {
		job := getOTAJob(t, st, jobID)
		waveMoved := false
		if w, _ := job["current_wave"].(float64); int(w) > wave {
			waveMoved = true
		}
		if s, _ := job["state"].(string); s != "running" && s != "pending" {
			waveMoved = true
		}
		if waveMoved {
			// One last look before giving up on this wave. The rollout's
			// control loop and this poll are independent clocks, and on a
			// loaded machine the loop can dispatch a cohort and move past it
			// between two polls — which used to read as "the first cohort
			// dispatched no devices" and fail a test about rollback for a
			// reason that had nothing to do with rollback.
			sweep()
			break
		}
		if sweep() == 0 {
			idle++
			// Several quiet control-loop ticks with nothing new dispatched
			// means the cohort is complete.
			if idle >= 8 && len(reported) > 0 {
				break
			}
		} else {
			idle = 0
		}
		time.Sleep(150 * time.Millisecond)
	}
	return len(reported)
}

func otaPost(t *testing.T, st *stack.Stack, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		st.Services().OTAURL()+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON(t, req)
}

func otaGet(t *testing.T, st *stack.Stack, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, st.Services().OTAURL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return doJSON(t, req)
}

func doJSON(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := map[string]any{}
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out
}

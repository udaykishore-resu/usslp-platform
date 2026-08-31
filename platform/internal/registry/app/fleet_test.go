package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/adapters"
	"github.com/usslp/usslp/platform/internal/registry/app"
	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// enrolLabels manufactures, ingests and provisions n labels under one
// controller, returning them in order.
func enrolLabels(t *testing.T, h *harness, sec canon.SECID, n int) []enrolled {
	t.Helper()
	devices := make([]enrolled, 0, n)
	for i := 1; i <= n; i++ {
		devices = append(devices, h.manufacture(fmt.Sprintf("lbl-%04d", i), domain.KindLabel, uint64(i)))
	}
	h.ingest(devices...)
	for _, d := range devices {
		h.provision(d, sec, "aisle-01")
	}
	return devices
}

func planogramFor(store canon.StoreID, sec canon.SECID, labels []enrolled) *domain.Planogram {
	pg := &domain.Planogram{TenantID: "acme", StoreID: store}
	for i, d := range labels {
		pg.Positions = append(pg.Positions, domain.Position{
			PositionKey: domain.PositionKey{Shelf: "A", Rail: "1", Position: i + 1},
			LabelID:     canon.LabelID(d.deviceID),
			SKU:         canon.SKU(fmt.Sprintf("SKU-%04d", i+1)),
			Facings:     1,
			Template:    "standard",
			SECID:       sec,
			Zone:        "aisle-01",
		})
	}
	return pg
}

func TestPlanogramUploadEmitsTheAssignmentContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	labels := enrolLabels(t, h, "sec-01", 3)

	res, err := h.svc.UploadPlanogram(context.Background(), planogramFor(h.storeID, "sec-01", labels))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Revision != 1 || res.Assigned != 3 || len(res.Diff.Added) != 3 {
		t.Fatalf("upload result = %+v", res)
	}

	events := h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtLabelAssigned)
	if len(events) != 3 {
		t.Fatalf("published %d assignment events, want 3", len(events))
	}
	var a domain.LabelAssigned
	if err := events[0].Decode(&a); err != nil {
		t.Fatalf("decode assignment: %v", err)
	}
	if a.LabelID != "lbl-0001" || a.SECID != "sec-01" || a.SKU != "SKU-0001" {
		t.Fatalf("assignment = %+v", a)
	}
	if a.Facings != 1 || a.Template != "standard" {
		t.Fatalf("assignment lost its render inputs: %+v", a)
	}
	if a.Shelf != "A" || a.Rail != "1" || a.Position != 1 {
		t.Fatalf("assignment lost its coordinates: %+v", a)
	}
	if a.Sequence != 1 {
		t.Fatalf("sequence = %d, want 1", a.Sequence)
	}
	// The stream key must be the label, so two assignments for one label are
	// strictly ordered while different labels proceed in parallel. Interface
	// contract §2 keys device-events by device_id, which is not what the generic
	// envelope key would produce for a payload that carries a SKU.
	if got := adapters.StreamKey(canon.StreamDeviceEvents.Name, events[0]); got != "lbl-0001" {
		t.Fatalf("stream key = %s, want the label id", got)
	}

	if got := h.mustDevice("lbl-0001"); got.State != domain.StateAssigned || got.Assignment == nil {
		t.Fatalf("device after assignment = %+v", got)
	}
}

func TestPlanogramReuploadOfAnIdenticalFileEmitsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	labels := enrolLabels(t, h, "sec-01", 3)

	if _, err := h.svc.UploadPlanogram(context.Background(), planogramFor(h.storeID, "sec-01", labels)); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	before := len(h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtLabelAssigned))

	res, err := h.svc.UploadPlanogram(context.Background(), planogramFor(h.storeID, "sec-01", labels))
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if !res.Diff.Empty() {
		t.Fatalf("re-uploading the same file produced %+v", res.Diff)
	}
	after := len(h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtLabelAssigned))
	if after != before {
		t.Fatalf("a no-op upload emitted %d extra assignment events; a nightly full-file export would reprice the store",
			after-before)
	}
	if res.Revision != 2 {
		t.Fatalf("revision = %d, want 2: the upload happened even though nothing changed", res.Revision)
	}
}

func TestPlanogramOrphanWithdrawsTheBinding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	labels := enrolLabels(t, h, "sec-01", 3)
	ctx := context.Background()

	if _, err := h.svc.UploadPlanogram(ctx, planogramFor(h.storeID, "sec-01", labels)); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// The third label disappears from the layout entirely.
	res, err := h.svc.UploadPlanogram(ctx, planogramFor(h.storeID, "sec-01", labels[:2]))
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if len(res.Diff.Orphaned) != 1 || res.Diff.Orphaned[0] != "lbl-0003" {
		t.Fatalf("orphaned = %v, want lbl-0003", res.Diff.Orphaned)
	}
	if res.Unassigned != 1 {
		t.Fatalf("unassigned = %d, want 1", res.Unassigned)
	}
	if got := h.mustDevice("lbl-0003"); got.Assignment != nil {
		t.Fatalf("lbl-0003 still holds an assignment: %+v", got.Assignment)
	}

	withdrawals := h.pub.ofType(canon.StreamDeviceEvents.Name, domain.EvtLabelUnassigned)
	if len(withdrawals) != 1 {
		t.Fatalf("published %d withdrawals, want 1", len(withdrawals))
	}
	var u domain.LabelAssigned
	if err := withdrawals[0].Decode(&u); err != nil {
		t.Fatalf("decode withdrawal: %v", err)
	}
	if !u.Unassigned || u.LabelID != "lbl-0003" {
		t.Fatalf("withdrawal = %+v", u)
	}
	if u.SKU != "SKU-0003" {
		t.Fatalf("withdrawal must name the binding it removes; sku = %q", u.SKU)
	}
	if u.Sequence <= 1 {
		t.Fatalf("withdrawal sequence = %d, must be greater than the assignment it undoes", u.Sequence)
	}
}

func TestPlanogramMoveBetweenControllersClearsTheOldZone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	labels := enrolLabels(t, h, "sec-01", 2)
	ctx := context.Background()

	if _, err := h.svc.UploadPlanogram(ctx, planogramFor(h.storeID, "sec-01", labels)); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	h.mqtt.Reset()

	// The shelf is re-bricked and the first label moves onto another controller.
	moved := planogramFor(h.storeID, "sec-01", labels)
	moved.Positions[0].SECID = "sec-02"
	moved.Positions[0].Zone = "aisle-02"
	moved.Positions[0].Shelf = "B"
	if _, err := h.svc.UploadPlanogram(ctx, moved); err != nil {
		t.Fatalf("second upload: %v", err)
	}

	events := h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtLabelAssigned)
	last := events[len(events)-1]
	var a domain.LabelAssigned
	if err := last.Decode(&a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.SECID != "sec-02" || a.PreviousSECID != "sec-01" {
		t.Fatalf("reassignment = %s → %s, want sec-01 → sec-02", a.PreviousSECID, a.SECID)
	}

	// Interface contract §3: the stale retained state on the old zone topic is
	// cleared with a zero-length retained publish.
	cleared := 0
	for _, m := range h.mqtt.Messages() {
		if strings.HasPrefix(m.Topic, "usslp/acme/eu-west-1/store-0042/sec/sec-01/labels/lbl-0001/") {
			if len(m.Payload) == 0 && m.Retain {
				cleared++
			}
		}
	}
	if cleared < 2 {
		t.Fatalf("cleared %d retained topics on the old controller, want the price and config topics", cleared)
	}
	if got := h.mustDevice("lbl-0001").Placement.SECID; got != "sec-02" {
		t.Fatalf("controller = %s, want sec-02", got)
	}
}

func TestPlanogramUploadedBeforeTheHardwareBindsOnProvisioning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	// Head office uploads the layout days before an engineer clips the hardware
	// onto the rail. That is the normal order for a store fit-out.
	pg := &domain.Planogram{TenantID: h.tenant, StoreID: h.storeID, Positions: []domain.Position{{
		PositionKey: domain.PositionKey{Shelf: "A", Rail: "1", Position: 1},
		LabelID:     "lbl-0001", SKU: "SKU-0001", Facings: 1, Template: "standard",
		SECID: "sec-01", Zone: "aisle-01",
	}}}
	res, err := h.svc.UploadPlanogram(ctx, pg)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Assigned != 0 || len(res.Pending) != 1 || res.Pending[0] != "lbl-0001" {
		t.Fatalf("upload result = %+v, want one pending binding", res)
	}

	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)
	h.provision(label, "sec-01", "aisle-01")

	dev := h.mustDevice("lbl-0001")
	if dev.Assignment == nil || dev.Assignment.SKU != "SKU-0001" {
		t.Fatalf("a label provisioned into a store with a stored planogram must bind immediately: %+v", dev)
	}
	if dev.State != domain.StateAssigned {
		t.Fatalf("state = %s, want assigned", dev.State)
	}
}

func TestRetiringALabelWithdrawsItsAssignment(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	labels := enrolLabels(t, h, "sec-01", 1)
	ctx := context.Background()
	if _, err := h.svc.UploadPlanogram(ctx, planogramFor(h.storeID, "sec-01", labels)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := h.svc.Retire(ctx, "lbl-0001", "screen cracked"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if n := len(h.pub.ofType(canon.StreamDeviceEvents.Name, domain.EvtLabelUnassigned)); n != 1 {
		t.Fatalf("retiring an assigned label published %d withdrawals, want 1", n)
	}
	if n := len(h.pub.ofType(canon.StreamDeviceEvents.Name, domain.EvtDeviceRetired)); n != 1 {
		t.Fatalf("published %d retirement events, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Telemetry, health and mesh
// ---------------------------------------------------------------------------

func TestHeartbeatDrivenOfflineDetection(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	labels := enrolLabels(t, h, "sec-01", 2)

	base := h.clock.Now()
	report := func(at time.Time, which ...int) {
		var readings []canon.Telemetry
		for _, i := range which {
			readings = append(readings, canon.Telemetry{
				LabelID: canon.LabelID(labels[i].deviceID), StoreID: h.storeID, SECID: "sec-01",
				ReportedAt: at, BatteryPct: 90, BatteryMV: 3000, LQI: 200, FirmwareVer: "1.0.0",
			})
		}
		if err := h.svc.IngestTelemetry(ctx, readings); err != nil {
			t.Fatalf("ingest telemetry: %v", err)
		}
	}

	report(base, 0, 1)
	if _, err := h.svc.SweepHealth(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, l := range labels {
		if got := h.mustDevice(l.deviceID).State; got != domain.StateActive {
			t.Fatalf("%s = %s, want active", l.deviceID, got)
		}
	}

	// One label keeps beaconing; the other goes quiet. After three missed
	// 30-second beacons the quiet one must be offline and the other must not.
	h.clock.Advance(91 * time.Second)
	report(h.clock.Now(), 0)
	applied, err := h.svc.SweepHealth(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if applied != 1 {
		t.Fatalf("sweep applied %d transitions, want exactly 1", applied)
	}
	if got := h.mustDevice(labels[0].deviceID).State; got != domain.StateActive {
		t.Fatalf("the beaconing label is %s, want active", got)
	}
	if got := h.mustDevice(labels[1].deviceID).State; got != domain.StateOffline {
		t.Fatalf("the silent label is %s, want offline", got)
	}

	// The transition must be published under the platform-wide name so that
	// existing device.status.offline consumers see it.
	offline := h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtDeviceOffline)
	if len(offline) != 1 {
		t.Fatalf("published %d offline events, want 1", len(offline))
	}
	var change domain.DeviceStateChanged
	if err := offline[0].Decode(&change); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if change.To != domain.StateOffline || change.DeviceID != labels[1].deviceID {
		t.Fatalf("offline event = %+v", change)
	}

	// It comes back when it is heard from again.
	h.clock.Advance(10 * time.Second)
	report(h.clock.Now(), 1)
	if _, err := h.svc.SweepHealth(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := h.mustDevice(labels[1].deviceID).State; got != domain.StateActive {
		t.Fatalf("state after a beacon = %s, want active", got)
	}
}

func TestBatteryCriticalIsRaisedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	labels := enrolLabels(t, h, "sec-01", 1)

	for i := 0; i < 5; i++ {
		h.clock.Advance(5 * time.Minute)
		if err := h.svc.IngestTelemetry(ctx, []canon.Telemetry{{
			LabelID: canon.LabelID(labels[0].deviceID), StoreID: h.storeID, SECID: "sec-01",
			ReportedAt: h.clock.Now(), BatteryPct: 8, BatteryMV: 2350, LQI: 180,
		}}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	alerts := h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtBatteryCritical)
	if len(alerts) != 1 {
		t.Fatalf("published %d battery alerts for one flat cell, want 1", len(alerts))
	}
	var b domain.BatteryCritical
	if err := alerts[0].Decode(&b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.BatteryPct != 8 || b.LabelID != canon.LabelID(labels[0].deviceID) {
		t.Fatalf("alert = %+v", b)
	}
}

func TestTelemetryIsForwardedToTheTelemetryStream(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	labels := enrolLabels(t, h, "sec-01", 3)

	readings := make([]canon.Telemetry, 0, len(labels))
	for _, l := range labels {
		readings = append(readings, canon.Telemetry{
			LabelID: canon.LabelID(l.deviceID), StoreID: h.storeID, SECID: "sec-01",
			ReportedAt: h.clock.Now(), BatteryPct: 88, BatteryMV: 2950, LQI: 200,
		})
	}
	if err := h.svc.IngestTelemetry(ctx, readings); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	published := h.pub.events(canon.StreamTelemetry.Name)
	if len(published) != 3 {
		t.Fatalf("forwarded %d readings, want 3: the batch must be fanned out per label", len(published))
	}
	seen := map[string]bool{}
	for _, e := range published {
		if e.EventType != canon.EvtDeviceTelemetry {
			t.Fatalf("event type = %s", e.EventType)
		}
		seen[e.PartitionKey()] = true
	}
	for _, l := range labels {
		if !seen[l.deviceID] {
			t.Fatalf("telemetry for %s was not keyed by its label", l.deviceID)
		}
	}
}

func TestTelemetryFromAQuarantinedDeviceIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	labels := enrolLabels(t, h, "sec-01", 1)
	if err := h.svc.Quarantine(ctx, labels[0].deviceID, "tamper"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if err := h.svc.IngestTelemetry(ctx, []canon.Telemetry{{
		LabelID: canon.LabelID(labels[0].deviceID), StoreID: h.storeID, SECID: "sec-01",
		ReportedAt: h.clock.Now(), BatteryPct: 95, LQI: 250,
	}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	dev := h.mustDevice(labels[0].deviceID)
	if dev.LastTelemetry != nil {
		t.Fatal("telemetry from a quarantined device was recorded; it must not be able to look healthy")
	}
	if dev.State != domain.StateQuarantined {
		t.Fatalf("state = %s, want quarantined", dev.State)
	}
}

func TestMeshIngestBuildsTheStoreTopology(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	labels := enrolLabels(t, h, "sec-01", 3)

	if err := h.svc.IngestMeshReport(ctx, canon.MeshTopology{
		SECID: "sec-01", StoreID: h.storeID, UpdatedAt: h.clock.Now(),
		Nodes: []canon.MeshNode{
			{LabelID: canon.LabelID(labels[0].deviceID), LQI: 220, Router: true},
			{LabelID: canon.LabelID(labels[1].deviceID), ParentID: canon.LabelID(labels[0].deviceID), LQI: 190},
			{LabelID: canon.LabelID(labels[2].deviceID), ParentID: "lbl-ghost", LQI: 150},
		},
	}); err != nil {
		t.Fatalf("ingest mesh: %v", err)
	}
	trees := h.svc.StoreMesh(h.storeID)
	if len(trees) != 1 {
		t.Fatalf("store mesh = %d controllers, want 1", len(trees))
	}
	if len(trees[0].Orphans) != 1 || trees[0].Orphans[0] != canon.LabelID(labels[2].deviceID) {
		t.Fatalf("orphans = %v", trees[0].Orphans)
	}

	// A mesh report is an independent source of liveness: the two reachable
	// labels must not be swept offline even though they have sent no telemetry.
	h.clock.Advance(60 * time.Second)
	if _, err := h.svc.SweepHealth(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := h.mustDevice(labels[0].deviceID).State; got != domain.StateActive {
		t.Fatalf("a label the controller can see is %s, want active", got)
	}
	if got := h.mustDevice(labels[2].deviceID).State; got == domain.StateActive {
		t.Fatal("an orphaned label must not be treated as heard from")
	}
}

func TestStoreHealthAndRunwayEndToEnd(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	labels := enrolLabels(t, h, "sec-01", 4)

	// Four hours of telemetry: three healthy cells and one draining fast.
	for hour := 0; hour < 5; hour++ {
		readings := make([]canon.Telemetry, 0, len(labels))
		for i, l := range labels {
			pct := 90 - hour
			if i == 3 {
				pct = 40 - hour*5
			}
			readings = append(readings, canon.Telemetry{
				LabelID: canon.LabelID(l.deviceID), StoreID: h.storeID, SECID: "sec-01",
				ReportedAt: h.clock.Now(), BatteryPct: pct, BatteryMV: 2200 + pct*8, LQI: 200,
			})
		}
		if err := h.svc.IngestTelemetry(ctx, readings); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		h.clock.Advance(time.Hour)
	}

	runway := h.svc.StoreRunway(h.storeID)
	if len(runway) != 4 {
		t.Fatalf("runway entries = %d, want 4", len(runway))
	}
	if runway[0].LabelID != canon.LabelID(labels[3].deviceID) {
		t.Fatalf("soonest replacement = %s, want the fast-draining label %s",
			runway[0].LabelID, labels[3].deviceID)
	}
	if runway[0].RunwayHours >= runway[1].RunwayHours {
		t.Fatalf("runway is not sorted soonest first: %v", runway)
	}

	health := h.svc.StoreHealth(h.storeID)
	if health.StoreID != h.storeID {
		t.Fatalf("health store = %s", health.StoreID)
	}
	if health.Labels != 4 {
		t.Fatalf("labels = %d, want 4", health.Labels)
	}
	if health.SoonestRunwayLabel != canon.LabelID(labels[3].deviceID) {
		t.Fatalf("soonest runway label = %s", health.SoonestRunwayLabel)
	}
}

func TestFleetSummaryCounts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	labels := enrolLabels(t, h, "sec-01", 3)
	ctx := context.Background()
	if _, err := h.svc.UploadPlanogram(ctx, planogramFor(h.storeID, "sec-01", labels)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := h.svc.Quarantine(ctx, labels[2].deviceID, "tamper"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	sum := h.svc.FleetSummary()
	if sum.Devices != 3 {
		t.Fatalf("devices = %d, want 3", sum.Devices)
	}
	if sum.Quarantined != 1 {
		t.Fatalf("quarantined = %d, want 1", sum.Quarantined)
	}
	if sum.Assigned != 3 {
		t.Fatalf("assigned = %d, want 3; a quarantine does not withdraw the planogram binding", sum.Assigned)
	}
	if sum.ByHardwareTier["esl-2.9-bw"] != 3 {
		t.Fatalf("hardware tiers = %v", sum.ByHardwareTier)
	}
	if sum.Stores != 1 || sum.Tenants != 1 {
		t.Fatalf("stores = %d tenants = %d", sum.Stores, sum.Tenants)
	}
}

func TestDevicesForOTAExcludesUnaddressableDevices(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	labels := enrolLabels(t, h, "sec-01", 3)
	if _, err := h.svc.UploadPlanogram(ctx, planogramFor(h.storeID, "sec-01", labels)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := h.svc.Quarantine(ctx, labels[0].deviceID, "tamper"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if err := h.svc.Retire(ctx, labels[1].deviceID, "scrapped"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	targets := h.svc.DevicesForOTA(h.storeID, "esl-2.9-bw")
	if len(targets) != 1 || targets[0].ID != labels[2].deviceID {
		t.Fatalf("ota targets = %v, want only the addressable label", targets)
	}
	if got := h.svc.DevicesForOTA(h.storeID, "esl-4.2-bwr"); len(got) != 0 {
		t.Fatalf("targets on a foreign hardware tier = %d, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// Seeding
// ---------------------------------------------------------------------------

func TestSeedIsDeterministic(t *testing.T) {
	t.Parallel()
	run := func() *app.SeedResult {
		h := newHarness(t)
		res, err := h.svc.Seed(context.Background(), app.SeedRequest{
			TenantID: "acme", StoreID: "store-seed", SECs: 2, LabelsPerSEC: 5,
			Seed: 42, WithTelemetry: true,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Fold the whole generated store into a comparable shape.
		pg := h.svc.Planogram("store-seed")
		if pg == nil {
			t.Fatal("seeded store has no planogram")
		}
		var b strings.Builder
		for _, p := range pg.Positions {
			fmt.Fprintf(&b, "%s|%s|%s|%d|%d|%s\n",
				p.PositionKey, p.LabelID, p.SKU, p.Facings, p.Position, p.Template)
		}
		for _, d := range h.svc.StoreDevices("store-seed") {
			pct, _ := d.BatteryPercent()
			fmt.Fprintf(&b, "%s|%s|%s|%s|%d\n", d.ID, d.Kind, d.EUI64, d.Placement.SECID, pct)
		}
		res.ElapsedMS = 0
		t.Logf("seed fingerprint length %d", b.Len())
		seedFingerprints = append(seedFingerprints, b.String())
		return res
	}
	seedFingerprints = nil
	a := run()
	b := run()

	if len(seedFingerprints) != 2 || seedFingerprints[0] != seedFingerprints[1] {
		t.Fatal("two runs with the same seed produced different stores")
	}
	if a.Labels != b.Labels || a.Positions != b.Positions || a.PlanogramRevision != b.PlanogramRevision {
		t.Fatalf("seed results differ: %+v vs %+v", a, b)
	}
	if a.Labels != 10 || a.Positions != 10 || a.Assigned != 10 {
		t.Fatalf("seed created %+v, want 10 labels all assigned", a)
	}
	if len(a.SECs) != 2 {
		t.Fatalf("controllers = %d, want 2", len(a.SECs))
	}
}

// seedFingerprints is written by TestSeedIsDeterministic's two runs. It is a
// package-level variable rather than a closure capture only so that the two
// harnesses, which each need their own temporary directory, stay independent.
var seedFingerprints []string

func TestSeedProducesAHealthyStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, err := h.svc.Seed(context.Background(), app.SeedRequest{
		TenantID: "acme", StoreID: "store-seed", SECs: 3, LabelsPerSEC: 8,
		Seed: 7, WithTelemetry: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	health := h.svc.StoreHealth("store-seed")
	if health.Labels != 24 || health.Controllers != 3 {
		t.Fatalf("health = %+v", health)
	}
	if health.MeshOrphans != 0 {
		t.Fatalf("a seeded store must have a connected mesh; orphans = %d", health.MeshOrphans)
	}
	if health.Score < 80 {
		t.Fatalf("seeded store scored %.1f, want a working store", health.Score)
	}
	mesh := h.svc.StoreMesh("store-seed")
	if len(mesh) != 3 {
		t.Fatalf("mesh controllers = %d, want 3", len(mesh))
	}
	if mesh[0].MaxDepth < 2 {
		t.Fatalf("seeded mesh is flat; max depth = %d", mesh[0].MaxDepth)
	}
}

func TestSeedIsRefusedWithoutAnIssuer(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// Rebuild the service without an issuer, which is the production
	// configuration.
	svc, err := app.Open(context.Background(), app.Config{
		Store: h.store, Events: h.pub, Messenger: h.mqtt,
		Auth: nil, Clock: h.clock,
	})
	if err == nil {
		t.Fatal("a service with no authenticator must not open")
	}
	_ = svc

	if _, err := app.Open(context.Background(), app.Config{Clock: h.clock}); err == nil {
		t.Fatal("a service with no event store must not open")
	}
}

func TestDeviceTrafficArrivesThroughTheMQTTSubscriptions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	labels := enrolLabels(t, h, "sec-01", 2)

	if err := h.svc.SubscribeDeviceTraffic(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// A controller publishes one batched telemetry message for its whole zone.
	// Batching per controller rather than per label is what keeps the fleet's
	// telemetry at 0.08 messages per second per store instead of 133.
	readings := []canon.Telemetry{
		{LabelID: canon.LabelID(labels[0].deviceID), StoreID: h.storeID, SECID: "sec-01",
			ReportedAt: h.clock.Now(), BatteryPct: 77, BatteryMV: 2800, LQI: 210},
		{LabelID: canon.LabelID(labels[1].deviceID), StoreID: h.storeID, SECID: "sec-01",
			ReportedAt: h.clock.Now(), BatteryPct: 81, BatteryMV: 2850, LQI: 190},
	}
	env, err := canon.NewEnvelope(canon.EvtDeviceTelemetry, "device", "sec-01", h.tenant, readings)
	if err != nil {
		t.Fatalf("build telemetry envelope: %v", err)
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("encode telemetry envelope: %v", err)
	}
	if !h.mqtt.Deliver(ctx, canon.FilterAllTelemetry, msgbus.Message{
		Topic:   "usslp/acme/eu-west-1/store-0042/sec/sec-01/telemetry",
		Payload: body,
	}) {
		t.Fatal("no handler was registered for the telemetry filter")
	}
	if got := h.mustDevice(labels[0].deviceID); got.LastTelemetry == nil || got.LastTelemetry.BatteryPct != 77 {
		t.Fatalf("telemetry did not reach the read model: %+v", got.LastTelemetry)
	}

	// And a mesh topology report on the same controller's status topic.
	report := canon.MeshTopology{
		SECID: "sec-01", StoreID: h.storeID, UpdatedAt: h.clock.Now(),
		Nodes: []canon.MeshNode{
			{LabelID: canon.LabelID(labels[0].deviceID), LQI: 220, Router: true},
			{LabelID: canon.LabelID(labels[1].deviceID), ParentID: canon.LabelID(labels[0].deviceID), LQI: 200},
		},
	}
	meshEnv, err := canon.NewEnvelope(canon.EvtMeshTopologyChanged, "device", "sec-01", h.tenant, report)
	if err != nil {
		t.Fatalf("build mesh envelope: %v", err)
	}
	meshBody, err := json.Marshal(meshEnv)
	if err != nil {
		t.Fatalf("encode mesh envelope: %v", err)
	}
	if !h.mqtt.Deliver(ctx, canon.FilterAllMesh, msgbus.Message{
		Topic:   "usslp/acme/eu-west-1/store-0042/sec/sec-01/mesh/status",
		Payload: meshBody,
	}) {
		t.Fatal("no handler was registered for the mesh filter")
	}
	if trees := h.svc.StoreMesh(h.storeID); len(trees) != 1 || trees[0].Size() != 2 {
		t.Fatalf("mesh report did not reach the read model: %+v", trees)
	}

	// A heartbeat carries no payload the registry needs; its value is entirely
	// in having arrived.
	h.clock.Advance(time.Minute)
	if !h.mqtt.Deliver(ctx, canon.FilterAllHeartbeats, msgbus.Message{
		Topic:      "usslp/acme/eu-west-1/store-0042/sec/sec-01/heartbeat",
		Payload:    []byte(`{}`),
		ReceivedAt: h.clock.Now(),
	}) {
		t.Fatal("no handler was registered for the heartbeat filter")
	}

	// Garbage on a telemetry topic is dropped rather than crashing the consumer:
	// it arrived at QoS 0 carrying observations, and a controller shipping
	// malformed telemetry needs a fix, not a redelivery.
	h.mqtt.Deliver(ctx, canon.FilterAllTelemetry, msgbus.Message{
		Topic:   "usslp/acme/eu-west-1/store-0042/sec/sec-01/telemetry",
		Payload: []byte("not json"),
	})
}

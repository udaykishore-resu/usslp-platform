package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

func TestLifecycleTransitions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	legal := []struct{ from, to domain.DeviceState }{
		{domain.StateManufactured, domain.StateProvisioned},
		{domain.StateProvisioned, domain.StateAssigned},
		{domain.StateAssigned, domain.StateActive},
		{domain.StateActive, domain.StateDegraded},
		{domain.StateDegraded, domain.StateOffline},
		{domain.StateOffline, domain.StateActive},
		{domain.StateActive, domain.StateQuarantined},
		{domain.StateQuarantined, domain.StateProvisioned},
		{domain.StateQuarantined, domain.StateRetired},
		{domain.StateAssigned, domain.StateAssigned},
	}
	for _, tc := range legal {
		d := &domain.Device{ID: "lbl-1", State: tc.from}
		if _, err := d.Transition(tc.to, "test", now); err != nil {
			t.Fatalf("%s → %s should be legal: %v", tc.from, tc.to, err)
		}
		if d.State != tc.to {
			t.Fatalf("state = %s after %s → %s", d.State, tc.from, tc.to)
		}
	}

	illegal := []struct {
		from, to domain.DeviceState
		why      string
	}{
		{domain.StateRetired, domain.StateActive, "a decommissioned serial must never be resurrected"},
		{domain.StateRetired, domain.StateProvisioned, "retired is terminal"},
		{domain.StateRetired, domain.StateQuarantined, "retired is terminal even for a security decision"},
		{domain.StateManufactured, domain.StateActive, "a device cannot be active before it is provisioned"},
		{domain.StateManufactured, domain.StateAssigned, "a device cannot be assigned before it is provisioned"},
		{domain.StateQuarantined, domain.StateActive, "a heartbeat must not release a quarantine"},
		{domain.StateQuarantined, domain.StateOffline, "silence must not release a quarantine"},
		{domain.StateActive, domain.StateManufactured, "a device cannot un-manufacture itself"},
		{domain.StateActive, domain.StateProvisioned, "re-enrolment goes through provisioning, not a state flip"},
	}
	for _, tc := range illegal {
		d := &domain.Device{ID: "lbl-1", State: tc.from}
		_, err := d.Transition(tc.to, "test", now)
		if err == nil {
			t.Fatalf("%s → %s was accepted; %s", tc.from, tc.to, tc.why)
		}
		if d.State != tc.from {
			t.Fatalf("a rejected transition mutated the device: state = %s", d.State)
		}
		if d.Version != 0 {
			t.Fatalf("a rejected transition bumped the version to %d", d.Version)
		}
	}
}

func TestTransitionProducesTheEdgeNotTheNode(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	d := &domain.Device{
		ID: "lbl-1", Kind: domain.KindLabel, TenantID: "acme",
		Placement: domain.Placement{StoreID: "store-1", SECID: "sec-1"},
		State:     domain.StateActive,
	}
	ev, err := d.Transition(domain.StateOffline, "beacon budget exhausted", now)
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if ev.From != domain.StateActive || ev.To != domain.StateOffline {
		t.Fatalf("event = %s → %s, want active → offline", ev.From, ev.To)
	}
	if ev.StoreID != "store-1" || ev.SECID != "sec-1" || ev.TenantID != "acme" {
		t.Fatalf("event lost its addressing: %+v", ev)
	}
	if d.Version != 1 {
		t.Fatalf("version = %d, want 1", d.Version)
	}
}

func TestAddressableIsTheSinglePredicate(t *testing.T) {
	t.Parallel()
	for state, want := range map[domain.DeviceState]bool{
		domain.StateManufactured: false,
		domain.StateProvisioned:  false,
		domain.StateAssigned:     true,
		domain.StateActive:       true,
		domain.StateDegraded:     true,
		domain.StateOffline:      true,
		domain.StateQuarantined:  false,
		domain.StateRetired:      false,
	} {
		if got := state.Addressable(); got != want {
			t.Fatalf("%s.Addressable() = %v, want %v", state, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Planogram diff
// ---------------------------------------------------------------------------

func pos(shelf, rail string, slot int, label canon.LabelID, sku canon.SKU, sec canon.SECID) domain.Position {
	return domain.Position{
		PositionKey: domain.PositionKey{Shelf: shelf, Rail: rail, Position: slot},
		LabelID:     label, SKU: sku, Facings: 1, Template: "standard", SECID: sec,
	}
}

func TestPlanogramDiffClassifiesEveryChange(t *testing.T) {
	t.Parallel()
	old := &domain.Planogram{TenantID: "acme", StoreID: "store-1", Positions: []domain.Position{
		pos("A", "1", 1, "lbl-1", "SKU-1", "sec-1"),
		pos("A", "1", 2, "lbl-2", "SKU-2", "sec-1"),
		pos("A", "1", 3, "lbl-3", "SKU-3", "sec-1"),
		pos("B", "1", 1, "lbl-4", "SKU-4", "sec-2"),
	}}
	next := &domain.Planogram{TenantID: "acme", StoreID: "store-1", Positions: []domain.Position{
		// lbl-1 unchanged
		pos("A", "1", 1, "lbl-1", "SKU-1", "sec-1"),
		// lbl-2 moved to a different coordinate, and onto another controller
		pos("B", "1", 1, "lbl-2", "SKU-2", "sec-2"),
		// lbl-3 stayed put but now faces a different product
		pos("A", "1", 3, "lbl-3", "SKU-9", "sec-1"),
		// lbl-5 is new
		pos("A", "1", 2, "lbl-5", "SKU-5", "sec-1"),
		// lbl-4 has disappeared entirely: it is orphaned
	}}

	d := domain.DiffPlanograms(old, next)

	if len(d.Added) != 1 || d.Added[0].LabelID != "lbl-5" {
		t.Fatalf("added = %+v, want exactly lbl-5", d.Added)
	}
	if len(d.Moved) != 1 || d.Moved[0].LabelID != "lbl-2" {
		t.Fatalf("moved = %+v, want exactly lbl-2", d.Moved)
	}
	if d.Moved[0].From.PositionKey.String() != "A/1/2" || d.Moved[0].To.PositionKey.String() != "B/1/1" {
		t.Fatalf("move did not carry both ends: %s → %s",
			d.Moved[0].From.PositionKey, d.Moved[0].To.PositionKey)
	}
	if len(d.Changed) != 1 || d.Changed[0].LabelID != "lbl-3" {
		t.Fatalf("changed = %+v, want exactly lbl-3", d.Changed)
	}
	if d.Changed[0].Detail == "" {
		t.Fatal("an amendment must say which field changed")
	}
	// B/1/1 is still declared (lbl-2 moved into it), so it is not removed.
	// Nothing was removed in this revision because every old coordinate is
	// still declared except none.
	if len(d.Removed) != 0 {
		t.Fatalf("removed = %+v, want none: every old coordinate is still declared", d.Removed)
	}
	if len(d.Orphaned) != 1 || d.Orphaned[0] != "lbl-4" {
		t.Fatalf("orphaned = %v, want exactly lbl-4", d.Orphaned)
	}
}

func TestPlanogramDiffSeparatesRemovedCoordinatesFromOrphanedLabels(t *testing.T) {
	t.Parallel()
	old := &domain.Planogram{TenantID: "acme", StoreID: "store-1", Positions: []domain.Position{
		pos("A", "1", 1, "lbl-1", "SKU-1", "sec-1"),
		pos("A", "2", 1, "lbl-2", "SKU-2", "sec-1"),
	}}
	// The rail A/2 is taken out and its label re-sited onto A/1/2. The
	// coordinate is removed; the label is not orphaned.
	next := &domain.Planogram{TenantID: "acme", StoreID: "store-1", Positions: []domain.Position{
		pos("A", "1", 1, "lbl-1", "SKU-1", "sec-1"),
		pos("A", "1", 2, "lbl-2", "SKU-2", "sec-1"),
	}}
	d := domain.DiffPlanograms(old, next)
	if len(d.Removed) != 1 || d.Removed[0].From.PositionKey.String() != "A/2/1" {
		t.Fatalf("removed = %+v, want the A/2/1 coordinate", d.Removed)
	}
	if len(d.Orphaned) != 0 {
		t.Fatalf("orphaned = %v; a re-sited label is not an orphan", d.Orphaned)
	}
	if len(d.Moved) != 1 {
		t.Fatalf("moved = %+v, want lbl-2", d.Moved)
	}
}

func TestPlanogramDiffOfAnUnchangedUploadIsEmpty(t *testing.T) {
	t.Parallel()
	build := func() *domain.Planogram {
		return &domain.Planogram{TenantID: "acme", StoreID: "store-1", Positions: []domain.Position{
			pos("A", "1", 2, "lbl-2", "SKU-2", "sec-1"),
			pos("A", "1", 1, "lbl-1", "SKU-1", "sec-1"),
		}}
	}
	// The same layout exported in a different row order must diff to nothing:
	// a nightly full-file export from a space-planning system would otherwise
	// reprice a whole store every night.
	a, b := build(), build()
	b.Positions[0], b.Positions[1] = b.Positions[1], b.Positions[0]
	if d := domain.DiffPlanograms(a, b); !d.Empty() {
		t.Fatalf("diff of a reordered identical upload = %+v, want empty", d)
	}
}

func TestPlanogramDiffAgainstNoPreviousRevisionIsAllAdditions(t *testing.T) {
	t.Parallel()
	next := &domain.Planogram{TenantID: "acme", StoreID: "store-1", Positions: []domain.Position{
		pos("A", "1", 1, "lbl-1", "SKU-1", "sec-1"),
	}}
	d := domain.DiffPlanograms(nil, next)
	if len(d.Added) != 1 || d.Total() != 1 {
		t.Fatalf("first upload diff = %+v, want one addition", d)
	}
}

func TestPlanogramValidationRejectsOneLabelInTwoSlots(t *testing.T) {
	t.Parallel()
	pg := &domain.Planogram{TenantID: "acme", StoreID: "store-1", Positions: []domain.Position{
		pos("A", "1", 1, "lbl-1", "SKU-1", "sec-1"),
		pos("A", "1", 2, "lbl-1", "SKU-2", "sec-1"),
	}}
	if err := pg.Validate(); err == nil {
		t.Fatal("a label bound to two coordinates was accepted; it would flicker between two prices forever")
	}
}

// ---------------------------------------------------------------------------
// Mesh topology
// ---------------------------------------------------------------------------

func TestMeshTreeAssemblyWithOrphans(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	report := canon.MeshTopology{
		SECID: "sec-1", StoreID: "store-1", UpdatedAt: at,
		Nodes: []canon.MeshNode{
			// Two routers parented directly to the controller.
			{LabelID: "lbl-r1", Depth: 1, LQI: 200, Router: true, Online: true},
			{LabelID: "lbl-r2", Depth: 1, LQI: 180, Router: true, Online: true},
			// A healthy leaf under r1.
			{LabelID: "lbl-a", ParentID: "lbl-r1", Depth: 2, LQI: 150, Online: true},
			// A leaf whose parent is not in the report at all: its router died
			// between the two scans, so the whole path is gone.
			{LabelID: "lbl-orphan", ParentID: "lbl-ghost", Depth: 2, LQI: 140, Online: true},
			// Two nodes in a cycle: the Zigbee stack briefly produces this during
			// a re-parent storm, and neither can reach the controller.
			{LabelID: "lbl-c1", ParentID: "lbl-c2", Depth: 2, LQI: 90, Online: true},
			{LabelID: "lbl-c2", ParentID: "lbl-c1", Depth: 2, LQI: 90, Online: true},
			// A weak but reachable link, and one whose self-reported depth is
			// wrong because it re-parented mid-scan.
			{LabelID: "lbl-weak", ParentID: "lbl-r2", Depth: 4, LQI: 30, Online: true},
		},
	}
	tree := domain.BuildMeshTree(report, domain.DefaultWeakLQI)

	if got := len(tree.Roots); got != 2 {
		t.Fatalf("roots = %d, want 2", got)
	}
	wantOrphans := map[canon.LabelID]bool{"lbl-orphan": true, "lbl-c1": true, "lbl-c2": true}
	if len(tree.Orphans) != len(wantOrphans) {
		t.Fatalf("orphans = %v, want %d entries", tree.Orphans, len(wantOrphans))
	}
	for _, o := range tree.Orphans {
		if !wantOrphans[o] {
			t.Fatalf("unexpected orphan %s", o)
		}
	}
	leaf, ok := tree.Node("lbl-a")
	if !ok || leaf.DerivedDepth != 2 || leaf.Orphaned {
		t.Fatalf("lbl-a = %+v, want a reachable node at depth 2", leaf)
	}
	r1, _ := tree.Node("lbl-r1")
	if len(r1.Children) != 1 || r1.Children[0] != "lbl-a" {
		t.Fatalf("lbl-r1 children = %v, want [lbl-a]", r1.Children)
	}
	weak, _ := tree.Node("lbl-weak")
	if !weak.WeakLink {
		t.Fatal("a node at LQI 30 must be flagged as a weak link")
	}
	if !weak.DepthDisagrees {
		t.Fatal("a node whose reported depth is 4 but whose derived depth is 2 must be flagged")
	}
	if weak.DerivedDepth != 2 {
		t.Fatalf("derived depth = %d, want 2; the tree walk is authoritative, not the report",
			weak.DerivedDepth)
	}
	if tree.MaxDepth != 2 {
		t.Fatalf("max depth = %d, want 2", tree.MaxDepth)
	}
	if tree.Routers != 2 {
		t.Fatalf("routers = %d, want 2", tree.Routers)
	}
	// The orphaned nodes must not drag the average link quality of the
	// reachable mesh: they are unreachable, not weak.
	if tree.AverageLQI <= 0 {
		t.Fatalf("average lqi = %f", tree.AverageLQI)
	}
}

func TestMeshTreeIgnoresASelfParentingNode(t *testing.T) {
	t.Parallel()
	tree := domain.BuildMeshTree(canon.MeshTopology{
		SECID: "sec-1", StoreID: "store-1",
		Nodes: []canon.MeshNode{{LabelID: "lbl-1", ParentID: "lbl-1", LQI: 200}},
	}, 0)
	if len(tree.Orphans) != 1 || tree.Orphans[0] != "lbl-1" {
		t.Fatalf("orphans = %v; a node claiming itself as parent has no path to the controller", tree.Orphans)
	}
}

// ---------------------------------------------------------------------------
// Health derivation
// ---------------------------------------------------------------------------

func TestDeriveStateHonoursTheBeaconBudget(t *testing.T) {
	t.Parallel()
	p := domain.DefaultHealthPolicy().WithDefaults()
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	if p.OfflineAfter() != 90*time.Second {
		t.Fatalf("offline budget = %s, want 90s (three 30-second beacons)", p.OfflineAfter())
	}

	d := &domain.Device{ID: "lbl-1", State: domain.StateActive, LastSeen: base}
	if _, changed := p.DeriveState(d, base.Add(89*time.Second)); changed {
		t.Fatal("a device inside the beacon budget must stay active")
	}
	want, changed := p.DeriveState(d, base.Add(91*time.Second))
	if !changed || want != domain.StateOffline {
		t.Fatalf("after 91s: state = %s changed = %v, want offline", want, changed)
	}
}

func TestDeriveStateNeverOverridesADecision(t *testing.T) {
	t.Parallel()
	p := domain.DefaultHealthPolicy()
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for _, state := range []domain.DeviceState{domain.StateQuarantined, domain.StateRetired} {
		d := &domain.Device{ID: "lbl-1", State: state, LastSeen: base}
		if _, changed := p.DeriveState(d, base.Add(time.Hour)); changed {
			t.Fatalf("silence changed a %s device's state; only an operator may", state)
		}
		d2 := &domain.Device{ID: "lbl-2", State: state, LastSeen: base.Add(time.Hour)}
		if _, changed := p.DeriveState(d2, base.Add(time.Hour)); changed {
			t.Fatalf("a heartbeat released a %s device", state)
		}
	}
}

func TestDeriveStateGivesANewDeviceItsFirstBeaconBudget(t *testing.T) {
	t.Parallel()
	p := domain.DefaultHealthPolicy()
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	d := &domain.Device{ID: "lbl-1", State: domain.StateProvisioned, ProvisionedAt: base}
	if _, changed := p.DeriveState(d, base.Add(30*time.Second)); changed {
		t.Fatal("a device provisioned 30 seconds ago must not be marked offline")
	}
	want, changed := p.DeriveState(d, base.Add(2*time.Minute))
	if !changed || want != domain.StateOffline {
		t.Fatalf("a device that never spoke = %s changed=%v, want offline", want, changed)
	}
}

func TestBatteryRunwayEstimation(t *testing.T) {
	t.Parallel()
	p := domain.DefaultHealthPolicy().WithDefaults()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// A cell draining a clean 1% per hour from 30%, with the temperature noise
	// a chilled aisle produces layered on top.
	d := &domain.Device{ID: "lbl-1"}
	noise := []int{0, 1, -1, 0, 1, -1, 0, 1, -1, 0}
	for i := 0; i < 10; i++ {
		d.RecordBattery(domain.BatterySample{
			At:      base.Add(time.Duration(i) * time.Hour),
			Percent: 30 - i + noise[i],
		})
	}
	hours, ok := p.BatteryRunway(d, base.Add(9*time.Hour))
	if !ok {
		t.Fatal("a ten-sample series over nine hours must produce an estimate")
	}
	// Last sample is 21%, end of life is 5%, drain is ~1%/h, so ~16 hours.
	if hours < 12 || hours > 20 {
		t.Fatalf("runway = %.1f hours, want roughly 16", hours)
	}

	// The estimate must be charged for time elapsed since the last report: the
	// drain does not pause because telemetry did.
	later, _ := p.BatteryRunway(d, base.Add(15*time.Hour))
	if later >= hours {
		t.Fatalf("runway did not shrink with elapsed time: %.1f then %.1f", hours, later)
	}
}

func TestBatteryRunwayRefusesToGuess(t *testing.T) {
	t.Parallel()
	p := domain.DefaultHealthPolicy()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	sparse := &domain.Device{ID: "lbl-1"}
	sparse.RecordBattery(domain.BatterySample{At: base, Percent: 50})
	sparse.RecordBattery(domain.BatterySample{At: base.Add(time.Hour), Percent: 49})
	if _, ok := p.BatteryRunway(sparse, base.Add(time.Hour)); ok {
		t.Fatal("two samples must not produce an estimate")
	}

	// A battery that appears to be charging is a sensor fault. Reporting an
	// infinite runway would quietly drop a failing label off the schedule.
	rising := &domain.Device{ID: "lbl-2"}
	for i := 0; i < 6; i++ {
		rising.RecordBattery(domain.BatterySample{
			At: base.Add(time.Duration(i) * time.Hour), Percent: 40 + i,
		})
	}
	if _, ok := p.BatteryRunway(rising, base.Add(5*time.Hour)); ok {
		t.Fatal("a rising battery must be refused, not extrapolated")
	}
}

func TestBatterySamplesAreKeptInTimeOrderAndBounded(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	d := &domain.Device{ID: "lbl-1"}
	// A gateway replaying buffered telemetry after a WAN outage delivers
	// samples out of order.
	d.RecordBattery(domain.BatterySample{At: base.Add(2 * time.Hour), Percent: 48})
	d.RecordBattery(domain.BatterySample{At: base, Percent: 50})
	d.RecordBattery(domain.BatterySample{At: base.Add(time.Hour), Percent: 49})
	for i, s := range d.BatteryHistory {
		if i > 0 && s.At.Before(d.BatteryHistory[i-1].At) {
			t.Fatalf("battery history is out of order at %d: %v", i, d.BatteryHistory)
		}
	}
	for i := 0; i < 200; i++ {
		d.RecordBattery(domain.BatterySample{At: base.Add(time.Duration(10+i) * time.Hour), Percent: 40})
	}
	if len(d.BatteryHistory) > 32 {
		t.Fatalf("battery history grew to %d samples; it must stay bounded", len(d.BatteryHistory))
	}
}

func TestStoreHealthScore(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	p := domain.DefaultHealthPolicy()

	healthy := make([]*domain.Device, 0, 10)
	for i := 0; i < 10; i++ {
		d := &domain.Device{
			ID: "lbl", Kind: domain.KindLabel, TenantID: "acme",
			Placement: domain.Placement{StoreID: "store-1"},
			State:     domain.StateActive, LastSeen: now,
		}
		d.RecordBattery(domain.BatterySample{At: now, Percent: 90, MilliVolts: 3000})
		healthy = append(healthy, d)
	}
	mesh := domain.BuildMeshTree(canon.MeshTopology{
		SECID: "sec-1", StoreID: "store-1", UpdatedAt: now,
		Nodes: []canon.MeshNode{
			{LabelID: "a", LQI: 220, Router: true}, {LabelID: "b", ParentID: "a", LQI: 210},
		},
	}, 0)
	h := domain.ComputeStoreHealth(p, healthy, []*domain.MeshTree{mesh}, now)
	if h.Score < 95 {
		t.Fatalf("a fully healthy store scored %.1f, want at least 95", h.Score)
	}
	if h.Grade != "healthy" {
		t.Fatalf("grade = %s, want healthy", h.Grade)
	}

	// Take half the fleet offline and orphan half the mesh.
	for i := 0; i < 5; i++ {
		healthy[i].State = domain.StateOffline
	}
	broken := domain.BuildMeshTree(canon.MeshTopology{
		SECID: "sec-1", StoreID: "store-1", UpdatedAt: now,
		Nodes: []canon.MeshNode{
			{LabelID: "a", LQI: 220, Router: true}, {LabelID: "b", ParentID: "ghost", LQI: 210},
		},
	}, 0)
	h2 := domain.ComputeStoreHealth(p, healthy, []*domain.MeshTree{broken}, now)
	if h2.Score >= h.Score {
		t.Fatalf("score did not fall: %.1f then %.1f", h.Score, h2.Score)
	}
	if h2.MeshOrphans != 1 {
		t.Fatalf("mesh orphans = %d, want 1", h2.MeshOrphans)
	}

	// An empty store is unconfigured, not perfect.
	if empty := domain.ComputeStoreHealth(p, nil, nil, now); empty.Score != 0 {
		t.Fatalf("an empty store scored %.1f, want 0", empty.Score)
	}
}

// ---------------------------------------------------------------------------
// Cross-service contract
// ---------------------------------------------------------------------------

func TestLabelAssignedJSONShapeIsFrozen(t *testing.T) {
	t.Parallel()
	// The Label Service builds its fan-out directory from these field names and
	// never queries the registry. Renaming one is a breaking change to a service
	// this package does not own, so the shape is asserted here rather than
	// discovered in production.
	body, err := json.Marshal(domain.LabelAssigned{
		LabelID: "lbl-1", TenantID: "acme", StoreID: "store-1", SECID: "sec-2",
		PreviousSECID: "sec-1", Zone: "aisle-01", SKU: "SKU-1", Facings: 2,
		Template: "promo", Shelf: "A", Rail: "1", Position: 3, Sequence: 7,
		AssignedAt: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"label_id": "lbl-1", "tenant_id": "acme", "store_id": "store-1",
		"sec_id": "sec-2", "previous_sec_id": "sec-1", "zone": "aisle-01",
		"sku": "SKU-1", "facings": float64(2), "display_template": "promo",
		"shelf": "A", "rail": "1", "position": float64(3), "sequence": float64(7),
		"assigned_at": "2026-03-01T09:00:00Z",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("field %q = %#v, want %#v", k, got[k], v)
		}
	}
	if _, present := got["unassigned"]; present {
		t.Fatal("an assignment must not carry the unassigned flag")
	}

	unassigned, err := json.Marshal(domain.LabelAssigned{LabelID: "lbl-1", Unassigned: true, Sequence: 8})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(unassigned, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["unassigned"] != true {
		t.Fatalf("withdrawal payload = %s", unassigned)
	}
}

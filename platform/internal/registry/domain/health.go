package domain

import (
	"sort"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// HealthPolicy is the configurable half of health derivation: the thresholds
// that decide when silence becomes "offline" and when a battery becomes an
// alert.
//
// They are policy rather than constants because the right numbers differ by
// deployment. A hypermarket with 40,000 labels on a congested 2.4 GHz band
// tolerates more missed beacons than a boutique with 300; a chilled-aisle label
// at 2 °C reports a lower voltage than the same cell in ambient and would trip a
// fixed millivolt threshold every winter. The defaults below are the platform's
// starting point and are what the blueprint's numbers describe.
type HealthPolicy struct {
	// BeaconInterval is how often a label is expected to be heard from.
	BeaconInterval time.Duration
	// MissedBeacons is how many consecutive intervals of silence mark a device
	// offline. Three beacons at 30 s is the 90-second budget in the blueprint:
	// long enough that one lost frame in a busy aisle is not an incident, short
	// enough that a genuinely dead controller is visible before a customer
	// notices a stale shelf edge.
	MissedBeacons int
	// HeartbeatWindow is how recently a device must have been heard from to be
	// considered active. Zero derives it as BeaconInterval × MissedBeacons,
	// which is the relationship that should hold; it is settable so that a
	// deployment can widen the active window without also widening the offline
	// one.
	HeartbeatWindow time.Duration
	// BatteryCriticalPct is the percentage at or below which
	// device.battery.critical is raised.
	BatteryCriticalPct int
	// BatteryCriticalMV is the voltage floor, applied in parallel with the
	// percentage. A cell whose firmware percentage estimate has drifted is
	// caught by the voltage; a cell whose voltage is propped up by a cold aisle
	// is caught by the percentage.
	BatteryCriticalMV int
	// BatteryEndOfLifePct is the percentage the runway estimate counts down to.
	// It is above zero because a label that reaches 0% has already stopped
	// refreshing: the useful question is when it stops being able to do its job,
	// not when the chemistry is exhausted.
	BatteryEndOfLifePct int
	// WeakLQI is the link-quality threshold below which a mesh link is weak.
	WeakLQI int
	// DegradedLQI is the link quality at or below which a device that is
	// otherwise reachable is marked degraded.
	DegradedLQI int
}

// DefaultHealthPolicy returns the platform's default thresholds: a 30-second
// beacon, three missed beacons to offline, 10% or 2,400 mV to a critical
// battery, and a 5% end-of-life floor for the runway estimate.
func DefaultHealthPolicy() HealthPolicy {
	return HealthPolicy{
		BeaconInterval:      30 * time.Second,
		MissedBeacons:       3,
		BatteryCriticalPct:  10,
		BatteryCriticalMV:   2400,
		BatteryEndOfLifePct: 5,
		WeakLQI:             DefaultWeakLQI,
		DegradedLQI:         40,
	}
}

// WithDefaults fills unset fields, so a partially configured policy is still
// safe to evaluate.
func (p HealthPolicy) WithDefaults() HealthPolicy {
	d := DefaultHealthPolicy()
	if p.BeaconInterval <= 0 {
		p.BeaconInterval = d.BeaconInterval
	}
	if p.MissedBeacons <= 0 {
		p.MissedBeacons = d.MissedBeacons
	}
	if p.HeartbeatWindow <= 0 {
		p.HeartbeatWindow = p.BeaconInterval * time.Duration(p.MissedBeacons)
	}
	if p.BatteryCriticalPct <= 0 {
		p.BatteryCriticalPct = d.BatteryCriticalPct
	}
	if p.BatteryCriticalMV <= 0 {
		p.BatteryCriticalMV = d.BatteryCriticalMV
	}
	if p.BatteryEndOfLifePct < 0 {
		p.BatteryEndOfLifePct = d.BatteryEndOfLifePct
	}
	if p.WeakLQI <= 0 {
		p.WeakLQI = d.WeakLQI
	}
	if p.DegradedLQI <= 0 {
		p.DegradedLQI = d.DegradedLQI
	}
	return p
}

// OfflineAfter returns the silence budget: beacon interval × missed beacons.
func (p HealthPolicy) OfflineAfter() time.Duration {
	p = p.WithDefaults()
	return p.BeaconInterval * time.Duration(p.MissedBeacons)
}

// DeriveState returns the lifecycle state a device's evidence implies at time
// now, and whether that differs from the state it currently holds.
//
// It only ever proposes the three states that are facts about contact —
// active, degraded and offline. Quarantine, retirement and assignment are
// decisions, and a decision must never be undone by a heartbeat arriving.
// A device in one of those states is returned unchanged.
func (p HealthPolicy) DeriveState(d *Device, now time.Time) (DeviceState, bool) {
	p = p.WithDefaults()
	switch d.State {
	case StateQuarantined, StateRetired, StateManufactured:
		return d.State, false
	}
	if d.LastSeen.IsZero() {
		// Never heard from. A freshly provisioned device is not offline until
		// its first beacon budget has elapsed since provisioning, otherwise
		// every enrolment would emit an immediate offline event.
		if !d.ProvisionedAt.IsZero() && now.Sub(d.ProvisionedAt) <= p.OfflineAfter() {
			return d.State, false
		}
		if d.State == StateOffline {
			return d.State, false
		}
		return StateOffline, CanTransition(d.State, StateOffline)
	}

	// Two windows, not one. A device must have been heard from inside
	// HeartbeatWindow to count as active, and must have gone silent for the
	// whole beacon budget before it is declared offline. With the default
	// policy the two coincide; a deployment that widens the active window
	// without widening the offline one gets a "stale but present" band that
	// shows up as degraded, which is what a store with a congested band
	// actually looks like.
	elapsed := now.Sub(d.LastSeen)
	var want DeviceState
	switch {
	case elapsed > p.OfflineAfter():
		want = StateOffline
	case elapsed > p.HeartbeatWindow, p.degraded(d):
		want = StateDegraded
	default:
		want = StateActive
	}
	if want == d.State {
		return d.State, false
	}
	return want, CanTransition(d.State, want)
}

// degraded reports whether a reachable device is failing a quality criterion.
func (p HealthPolicy) degraded(d *Device) bool {
	if d.LastTelemetry == nil {
		return false
	}
	t := d.LastTelemetry
	if t.TamperFlag {
		return true
	}
	if t.LQI > 0 && t.LQI <= p.DegradedLQI {
		return true
	}
	return p.BatteryCritical(t.BatteryPct, t.BatteryMV)
}

// BatteryCritical reports whether a battery reading is at or below either
// threshold. A reading of zero on one axis is treated as "not reported" rather
// than as the worst possible value, because a firmware that omits the voltage
// must not make every label critical.
func (p HealthPolicy) BatteryCritical(pct, mv int) bool {
	p = p.WithDefaults()
	if pct > 0 && pct <= p.BatteryCriticalPct {
		return true
	}
	if mv > 0 && mv <= p.BatteryCriticalMV {
		return true
	}
	return false
}

// BatteryRunway estimates how many hours of useful life a device's battery has
// left, and whether an estimate could be made at all.
//
// # Why a least-squares fit and not "last minus first"
//
// The number this produces schedules physical work: a technician is dispatched
// to an aisle with a box of cells, and the value of the estimate is that the
// visit happens before the shelf edge goes blank rather than after. Two-point
// extrapolation over a coin cell is useless for that, because the reported
// percentage is a firmware estimate derived from a voltage that swings with
// temperature — a chilled aisle at night and the same aisle at midday differ by
// more than a week of genuine drain. A least-squares slope over the retained
// window averages that swing out and, unlike an exponential filter, does not
// need a tuning constant nobody will ever revisit.
//
// The estimate is refused rather than guessed in three cases: fewer than three
// samples, a window shorter than an hour, and a non-negative slope. The last
// one matters most — a battery that appears to be charging is a sensor fault,
// and reporting "infinite runway" for it would silently remove a failing label
// from the replacement schedule.
func (p HealthPolicy) BatteryRunway(d *Device, now time.Time) (hours float64, ok bool) {
	p = p.WithDefaults()
	samples := d.BatteryHistory
	if len(samples) < 3 {
		return 0, false
	}
	first, last := samples[0], samples[len(samples)-1]
	span := last.At.Sub(first.At).Hours()
	if span < 1 {
		return 0, false
	}

	// Ordinary least squares of percent against hours since the first sample.
	var n, sumX, sumY, sumXY, sumXX float64
	for _, s := range samples {
		x := s.At.Sub(first.At).Hours()
		y := float64(s.Percent)
		n++
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		return 0, false
	}
	slope := (n*sumXY - sumX*sumY) / denom // percent per hour, negative when draining
	if slope >= 0 {
		return 0, false
	}
	drainPerHour := -slope

	remaining := float64(last.Percent - p.BatteryEndOfLifePct)
	if remaining <= 0 {
		return 0, true
	}
	hours = remaining / drainPerHour
	// Charge the estimate for time already elapsed since the last report. The
	// drain does not pause because telemetry did, so a device last heard from
	// six hours ago has six fewer hours of runway than its last sample implies.
	if elapsed := now.Sub(last.At).Hours(); elapsed > 0 {
		hours -= elapsed
	}
	if hours < 0 {
		hours = 0
	}
	return hours, true
}

// StoreHealth is the derived health of one store, the number a regional manager
// sees on a map and an operations team sorts a work queue by.
type StoreHealth struct {
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	// Devices counts every non-retired device on record.
	Devices int `json:"devices"`
	// ByState is the population of each lifecycle state.
	ByState map[DeviceState]int `json:"by_state"`
	// Labels, Controllers and Gateways break the population down by tier.
	Labels      int `json:"labels"`
	Controllers int `json:"controllers"`
	Gateways    int `json:"gateways"`
	// MeshNodes and MeshOrphans summarise every controller's topology.
	MeshNodes   int `json:"mesh_nodes"`
	MeshOrphans int `json:"mesh_orphans"`
	// WeakLinks counts mesh nodes below the weak-LQI threshold.
	WeakLinks int `json:"weak_links"`
	// AverageLQI is the mean link quality across reachable mesh nodes.
	AverageLQI float64 `json:"average_lqi"`
	// BatteryCritical counts labels at or below a battery threshold.
	BatteryCritical int `json:"battery_critical"`
	// MedianBatteryPct is the median reported battery across labels, which is
	// the number that predicts next year's replacement budget.
	MedianBatteryPct int `json:"median_battery_pct"`
	// SoonestRunwayHours is the shortest battery runway in the store, and
	// SoonestRunwayLabel the label it belongs to. Together they answer "when
	// does this store next need a visit".
	SoonestRunwayHours float64       `json:"soonest_runway_hours,omitempty"`
	SoonestRunwayLabel canon.LabelID `json:"soonest_runway_label,omitempty"`
	// Score is 0–100. See ComputeStoreHealth for the weights.
	Score float64 `json:"score"`
	// Grade buckets the score for a status colour.
	Grade      string    `json:"grade"`
	ComputedAt time.Time `json:"computed_at"`
}

// Score weights. They are named constants rather than literals in the formula
// because they are the thing an operator will argue with, and an argument about
// a weight should be a one-line diff with a comment attached.
const (
	// weightAvailability dominates because the platform's product is a correct
	// price on a shelf edge. A store whose labels are all reachable and current
	// is healthy even if its mesh is untidy; a store with 5% of its labels
	// unreachable has 5% of its shelf edge showing yesterday's prices, which is
	// a compliance exposure rather than a maintenance ticket.
	weightAvailability = 0.55
	// weightMesh covers reachability of the radio tree: orphaned nodes are
	// labels that will not receive the next update even though they are alive.
	weightMesh = 0.20
	// weightLink covers link quality, which is the leading indicator of the
	// previous two going wrong.
	weightLink = 0.10
	// weightBattery covers the batteries, which fail slowly and predictably and
	// therefore should never be the reason a store's score is a surprise.
	weightBattery = 0.15
	// lqiReference is the link quality treated as full marks. 200 rather than
	// 255 because a real store never reads 255 anywhere except a metre from the
	// controller, and a scale nobody can reach is a scale nobody trusts.
	lqiReference = 200.0
)

// ComputeStoreHealth derives a store's health from its devices and its
// controllers' mesh topologies.
//
// The score is a weighted mean of four ratios, each already in [0,1]:
// availability (devices answering, out of devices expected to answer), mesh
// reachability (nodes with a path to a controller), link quality (mean LQI
// against a reference of 200), and battery (labels not at a critical
// threshold). A store with no devices scores zero rather than one hundred: an
// empty store is not a healthy store, it is an unconfigured one, and rewarding
// it would put every unopened site at the top of the map.
func ComputeStoreHealth(policy HealthPolicy, devices []*Device, meshes []*MeshTree, now time.Time) StoreHealth {
	policy = policy.WithDefaults()
	h := StoreHealth{
		ByState:    make(map[DeviceState]int, len(AllStates())),
		ComputedAt: now.UTC(),
	}
	for _, s := range AllStates() {
		h.ByState[s] = 0
	}

	var expected, answering int
	batteries := make([]int, 0, len(devices))
	for _, d := range devices {
		if h.TenantID == "" {
			h.TenantID = d.TenantID
		}
		if h.StoreID == "" {
			h.StoreID = d.Placement.StoreID
		}
		h.ByState[d.State]++
		if d.State == StateRetired {
			continue
		}
		h.Devices++
		switch d.Kind {
		case KindLabel:
			h.Labels++
		case KindSEC:
			h.Controllers++
		case KindSGU:
			h.Gateways++
		}
		// A manufactured device has not been deployed yet and a quarantined one
		// has been deliberately taken out of service; neither is evidence about
		// how well the store is running, so both are outside the denominator.
		if d.State != StateManufactured && d.State != StateQuarantined {
			expected++
			if d.State == StateActive || d.State == StateAssigned {
				answering++
			}
		}
		if d.Kind != KindLabel {
			continue
		}
		if pct, ok := d.BatteryPercent(); ok {
			batteries = append(batteries, pct)
			mv := 0
			if d.LastTelemetry != nil {
				mv = d.LastTelemetry.BatteryMV
			}
			if policy.BatteryCritical(pct, mv) {
				h.BatteryCritical++
			}
		}
		if runway, ok := policy.BatteryRunway(d, now); ok {
			if h.SoonestRunwayLabel == "" || runway < h.SoonestRunwayHours {
				h.SoonestRunwayHours = runway
				h.SoonestRunwayLabel = d.LabelID()
			}
		}
	}

	var lqiSum float64
	var lqiNodes int
	for _, m := range meshes {
		if m == nil {
			continue
		}
		h.MeshNodes += m.Size()
		h.MeshOrphans += len(m.Orphans)
		h.WeakLinks += m.WeakLinks
		reachable := m.Size() - len(m.Orphans)
		if reachable > 0 && m.AverageLQI > 0 {
			lqiSum += m.AverageLQI * float64(reachable)
			lqiNodes += reachable
		}
	}
	if lqiNodes > 0 {
		h.AverageLQI = lqiSum / float64(lqiNodes)
	}

	if len(batteries) > 0 {
		sort.Ints(batteries)
		h.MedianBatteryPct = batteries[len(batteries)/2]
	}

	if h.Devices == 0 {
		h.Grade = gradeFor(0)
		return h
	}

	availability := ratio(answering, expected)
	mesh := 1.0
	if h.MeshNodes > 0 {
		mesh = ratio(h.MeshNodes-h.MeshOrphans, h.MeshNodes)
	}
	link := 1.0
	if h.AverageLQI > 0 {
		link = h.AverageLQI / lqiReference
		if link > 1 {
			link = 1
		}
	}
	battery := 1.0
	if h.Labels > 0 {
		battery = ratio(h.Labels-h.BatteryCritical, h.Labels)
	}

	h.Score = 100 * (availability*weightAvailability +
		mesh*weightMesh + link*weightLink + battery*weightBattery)
	if h.Score < 0 {
		h.Score = 0
	}
	if h.Score > 100 {
		h.Score = 100
	}
	h.Grade = gradeFor(h.Score)
	return h
}

// ratio guards the zero denominator that an empty store would otherwise
// produce. An empty numerator and denominator scores zero, not one: see the
// note on unconfigured stores in ComputeStoreHealth.
func ratio(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// gradeFor buckets a score into the four bands the status map colours by.
func gradeFor(score float64) string {
	switch {
	case score >= 95:
		return "healthy"
	case score >= 85:
		return "watch"
	case score >= 60:
		return "degraded"
	default:
		return "critical"
	}
}

// FleetSummary aggregates the whole registry, which is what the platform's own
// operators look at rather than any one retailer.
type FleetSummary struct {
	Devices int `json:"devices"`
	// ByState and ByKind are populations, not percentages: a percentage of 50
	// million hides the thousand devices that matter.
	ByState map[DeviceState]int `json:"by_state"`
	ByKind  map[DeviceKind]int  `json:"by_kind"`
	// ByHardwareTier is what an OTA planner sizes a rollout against.
	ByHardwareTier map[string]int `json:"by_hardware_tier"`
	// ByFirmware is the version spread, the number that says whether the last
	// rollout finished.
	ByFirmware map[string]int `json:"by_firmware_version"`
	Tenants    int            `json:"tenants"`
	Stores     int            `json:"stores"`
	// Assigned counts labels bound to a SKU by a planogram.
	Assigned int `json:"assigned_labels"`
	// BatteryCritical counts labels at or below a battery threshold.
	BatteryCritical int `json:"battery_critical"`
	// Quarantined is called out separately because it is the security number.
	Quarantined int       `json:"quarantined"`
	ComputedAt  time.Time `json:"computed_at"`
}

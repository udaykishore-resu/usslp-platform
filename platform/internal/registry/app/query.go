package app

import (
	"sort"

	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Device returns a copy of one device's registry entry, or nil.
//
// Every query in this file returns copies. The registry's read model is mutated
// by the telemetry path at up to 167,000 readings a second; handing a live
// pointer to an HTTP handler would be a data race with a guaranteed reproducer
// rather than a theoretical one.
func (s *Service) Device(id string) *domain.Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.devices[id].Clone()
}

// DeviceBySerial resolves the serial printed on a unit to its registry entry.
// It is the lookup a technician's scanner performs.
func (s *Service) DeviceBySerial(serial string) *domain.Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.bySerial[serial]
	if !ok {
		return nil
	}
	return s.devices[id].Clone()
}

// StoreDevices returns every device in a store, ordered by identifier so that
// two calls return the same list in the same order.
func (s *Service) StoreDevices(store canon.StoreID) []*domain.Device {
	s.mu.RLock()
	ids := make([]string, 0, len(s.byStore[store]))
	for id := range s.byStore[store] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*domain.Device, 0, len(ids))
	for _, id := range ids {
		if d := s.devices[id]; d != nil {
			out = append(out, d.Clone())
		}
	}
	s.mu.RUnlock()
	return out
}

// StoreMesh returns the assembled topology of every controller in a store,
// ordered by controller identifier.
func (s *Service) StoreMesh(store canon.StoreID) []*domain.MeshTree {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var secs []canon.SECID
	for sec, tree := range s.meshes {
		if tree != nil && tree.StoreID == store {
			secs = append(secs, sec)
		}
	}
	sort.Slice(secs, func(i, j int) bool { return secs[i] < secs[j] })
	out := make([]*domain.MeshTree, 0, len(secs))
	for _, sec := range secs {
		out = append(out, s.meshes[sec])
	}
	return out
}

// Mesh returns one controller's topology, or nil.
func (s *Service) Mesh(sec canon.SECID) *domain.MeshTree {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meshes[sec]
}

// StoreHealth derives a store's health score from its devices and meshes.
func (s *Service) StoreHealth(store canon.StoreID) domain.StoreHealth {
	devices := s.StoreDevices(store)
	meshes := s.StoreMesh(store)
	h := domain.ComputeStoreHealth(s.policy, devices, meshes, s.Now())
	if h.StoreID == "" {
		h.StoreID = store
	}
	return h
}

// DeviceRunway returns a device's battery runway in hours and whether one could
// be fitted.
func (s *Service) DeviceRunway(id string) (float64, bool) {
	d := s.Device(id)
	if d == nil {
		return 0, false
	}
	return s.policy.BatteryRunway(d, s.Now())
}

// RunwayEntry pairs a label with its estimated remaining battery life.
type RunwayEntry struct {
	LabelID canon.LabelID `json:"label_id"`
	StoreID canon.StoreID `json:"store_id"`
	SECID   canon.SECID   `json:"sec_id,omitempty"`
	Zone    string        `json:"zone,omitempty"`
	// Shelf and Rail are carried so the replacement round can be walked in
	// aisle order rather than in label-identifier order, which is the whole
	// operational point of predicting a battery instead of discovering it.
	Shelf       string  `json:"shelf,omitempty"`
	Rail        string  `json:"rail,omitempty"`
	BatteryPct  int     `json:"battery_pct"`
	RunwayHours float64 `json:"runway_hours"`
}

// StoreRunway returns every label in a store for which a runway could be
// fitted, soonest first, so that a replacement round is a sorted list rather
// than a search.
func (s *Service) StoreRunway(store canon.StoreID) []RunwayEntry {
	now := s.Now()
	devices := s.StoreDevices(store)
	out := make([]RunwayEntry, 0, len(devices))
	for _, d := range devices {
		if d.Kind != domain.KindLabel || d.State == domain.StateRetired {
			continue
		}
		hours, ok := s.policy.BatteryRunway(d, now)
		if !ok {
			continue
		}
		pct, _ := d.BatteryPercent()
		e := RunwayEntry{
			LabelID:     d.LabelID(),
			StoreID:     d.Placement.StoreID,
			SECID:       d.Placement.SECID,
			Zone:        d.Placement.Zone,
			BatteryPct:  pct,
			RunwayHours: hours,
		}
		if d.Assignment != nil {
			e.Shelf, e.Rail = d.Assignment.Shelf, d.Assignment.Rail
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RunwayHours != out[j].RunwayHours {
			return out[i].RunwayHours < out[j].RunwayHours
		}
		return out[i].LabelID < out[j].LabelID
	})
	return out
}

// FleetSummary aggregates the whole registry.
func (s *Service) FleetSummary() domain.FleetSummary {
	sum := domain.FleetSummary{
		ByState:        make(map[domain.DeviceState]int, len(domain.AllStates())),
		ByKind:         make(map[domain.DeviceKind]int, 3),
		ByHardwareTier: make(map[string]int),
		ByFirmware:     make(map[string]int),
		ComputedAt:     s.Now(),
	}
	for _, st := range domain.AllStates() {
		sum.ByState[st] = 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.devices {
		sum.Devices++
		sum.ByState[d.State]++
		sum.ByKind[d.Kind]++
		if d.HardwareTier != "" {
			sum.ByHardwareTier[d.HardwareTier]++
		}
		if d.FirmwareVersion != "" {
			sum.ByFirmware[d.FirmwareVersion]++
		}
		if d.Assignment != nil {
			sum.Assigned++
		}
		if d.State == domain.StateQuarantined {
			sum.Quarantined++
		}
		if d.Kind == domain.KindLabel {
			if pct, ok := d.BatteryPercent(); ok {
				mv := 0
				if d.LastTelemetry != nil {
					mv = d.LastTelemetry.BatteryMV
				}
				if s.policy.BatteryCritical(pct, mv) {
					sum.BatteryCritical++
				}
			}
		}
	}
	sum.Tenants = len(s.tenants)
	sum.Stores = len(s.byStore)
	return sum
}

// DevicesForOTA returns every device in a store that an OTA job may address:
// on record, on the right hardware tier, and in a state the platform is allowed
// to talk to.
//
// It exists here rather than in the OTA service because "may we address this
// device" is a registry decision. An OTA controller that filtered on its own
// copy of the rule would eventually disagree with the registry about a
// quarantined label, and the failure mode of that disagreement is a firmware
// download pushed at a device the platform has decided it cannot trust.
func (s *Service) DevicesForOTA(store canon.StoreID, hardwareTier string) []*domain.Device {
	devices := s.StoreDevices(store)
	out := make([]*domain.Device, 0, len(devices))
	for _, d := range devices {
		if hardwareTier != "" && d.HardwareTier != hardwareTier {
			continue
		}
		if !d.State.Addressable() {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Stores returns every store the registry holds devices for, ordered.
func (s *Service) Stores() []canon.StoreID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]canon.StoreID, 0, len(s.byStore))
	for store := range s.byStore {
		out = append(out, store)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

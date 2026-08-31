package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// IngestTelemetry folds a controller's batched telemetry into the health read
// model and fans it out onto the `label-telemetry` stream.
//
// The batching is the controller's, not the registry's, and it is what makes
// the fleet's telemetry affordable: 40,000 labels reporting every five minutes
// is 133 messages per second per store, which across 100,000 stores would be 13
// million messages per second if it were forwarded per label. Batched per
// controller it is 0.08 messages per second per store.
//
// The 13 million multiplies a worst-case store by every store, and does not
// describe the estate: 100,000 x 40,000 is 4 billion labels against a stated
// fleet of 50 million. At the estate's average of 500 labels per store the
// unbatched figure is 167,000 per second — a twentieth of the number quoted
// above, and still ample reason to batch. The inconsistency is catalogued in
// docs/architecture/scalability.md §1 rather than silently corrected here,
// because the blueprint figures are quoted verbatim in several packages and
// picking one of them is not this file's call. The registry unpacks the
// batch here because the *stream* is keyed per label — a consumer computing a
// per-label anomaly baseline must have its partition ordered by label, not by
// whichever controller happened to relay it.
//
// Telemetry is deliberately not written to the event store. It is a
// three-day-retention stream of observations, not a decision anybody has to be
// able to explain in a year, and putting 167,000 appends a second through an
// optimistically-concurrent aggregate store would be a way to make the registry
// the platform's bottleneck.
func (s *Service) IngestTelemetry(ctx context.Context, readings []canon.Telemetry) error {
	if len(readings) == 0 {
		return nil
	}
	now := s.Now()
	type alert struct {
		deviceID string
		payload  domain.BatteryCritical
	}
	var alerts []alert
	var unknown int

	s.mu.Lock()
	for i := range readings {
		t := readings[i]
		if t.LabelID == "" {
			continue
		}
		d := s.devices[string(t.LabelID)]
		if d == nil {
			unknown++
			continue
		}
		if d.State == domain.StateQuarantined || d.State == domain.StateRetired {
			// Telemetry from a device the platform has stopped trusting is
			// recorded nowhere and revives nothing. Accepting it would let a
			// quarantined identity keep its device looking healthy.
			continue
		}
		reported := t.ReportedAt
		if reported.IsZero() {
			reported = now
		}
		if reported.After(d.LastSeen) {
			d.LastSeen = reported.UTC()
		}
		snapshot := t
		d.LastTelemetry = &snapshot
		if t.FirmwareVer != "" {
			d.FirmwareVersion = t.FirmwareVer
		}
		if t.BatteryPct > 0 || t.BatteryMV > 0 {
			d.RecordBattery(domain.BatterySample{
				At: reported.UTC(), MilliVolts: t.BatteryMV, Percent: t.BatteryPct,
			})
		}
		if s.policy.BatteryCritical(t.BatteryPct, t.BatteryMV) && !d.BatteryCriticalRaised {
			runway, _ := s.policy.BatteryRunway(d, now)
			alerts = append(alerts, alert{deviceID: d.ID, payload: domain.BatteryCritical{
				LabelID:     d.LabelID(),
				TenantID:    d.TenantID,
				StoreID:     d.Placement.StoreID,
				SECID:       d.Placement.SECID,
				Zone:        d.Placement.Zone,
				BatteryPct:  t.BatteryPct,
				BatteryMV:   t.BatteryMV,
				RunwayHours: runway,
				At:          now,
			}})
		}
	}
	s.mu.Unlock()

	if s.met != nil {
		s.met.telemetry.With().Add(uint64(len(readings)))
	}
	if unknown > 0 {
		s.log.Debug("telemetry received for devices that are not on record", "readings", unknown)
	}

	if err := s.forwardTelemetry(ctx, readings); err != nil {
		s.log.Warn("registry could not forward telemetry", "error", err, "readings", len(readings))
	}

	for _, a := range alerts {
		if err := s.raiseBatteryCritical(ctx, a.deviceID, a.payload); err != nil {
			s.log.Warn("registry could not raise a battery alert", "device_id", a.deviceID, "error", err)
		}
	}
	return nil
}

// forwardTelemetry publishes one envelope per reading onto the telemetry
// stream, keyed so that each label's history lands on one partition in order.
func (s *Service) forwardTelemetry(ctx context.Context, readings []canon.Telemetry) error {
	if s.cfg.Events == nil {
		return nil
	}
	envs := make([]canon.Envelope, 0, len(readings))
	for i := range readings {
		t := readings[i]
		if t.LabelID == "" {
			continue
		}
		tenant := s.tenantOf(string(t.LabelID))
		if tenant == "" {
			continue
		}
		env, err := s.newEvent(canon.EvtDeviceTelemetry, domain.AggregateDevice,
			string(t.LabelID), tenant, t.StoreID, t)
		if err != nil {
			return err
		}
		envs = append(envs, env)
	}
	if len(envs) == 0 {
		return nil
	}
	return s.cfg.Events.PublishEvents(ctx, canon.StreamTelemetry.Name, envs...)
}

// tenantOf resolves a device's tenant from the read model.
func (s *Service) tenantOf(deviceID string) canon.TenantID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if d := s.devices[deviceID]; d != nil {
		return d.TenantID
	}
	return ""
}

// raiseBatteryCritical records the alert once per battery.
//
// The "once" is the point. A label at 4% reports every five minutes for weeks;
// re-emitting the alert each time would put a hundred thousand events a day on
// the device stream and train every operator to ignore the alert that matters.
// The flag is cleared only when the device is provisioned again, which is what
// happens after a cell is replaced.
func (s *Service) raiseBatteryCritical(ctx context.Context, deviceID string, payload domain.BatteryCritical) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	dev := s.device(deviceID)
	if dev == nil || dev.BatteryCriticalRaised {
		return nil
	}
	env, err := s.newEvent(canon.EvtBatteryCritical, domain.AggregateDevice, deviceID,
		dev.TenantID, dev.Placement.StoreID, payload)
	if err != nil {
		return err
	}
	env.IdempotencyKey = fmt.Sprintf("battery-critical:%s", deviceID)
	if err := s.commit(ctx, deviceStream(deviceID), dev.Version, env); err != nil {
		return err
	}
	s.log.Info("battery critical",
		"label_id", deviceID, "store_id", string(payload.StoreID),
		"battery_pct", payload.BatteryPct, "runway_hours", payload.RunwayHours)
	return nil
}

// IngestMeshReport folds a controller's Zigbee topology report into the mesh
// model.
//
// The report is trusted for what only the controller can know — which node
// named which parent, what the link quality was — and recomputed for everything
// derivable, because a controller mid-re-parent reports depths that contradict
// each other. Orphan detection in particular is the registry's job and not the
// controller's: a controller cannot see that a node's parent has vanished from
// a scan it did not perform.
func (s *Service) IngestMeshReport(ctx context.Context, report canon.MeshTopology) error {
	if report.SECID == "" {
		return fmt.Errorf("%w: mesh report has no controller", domain.ErrInvalid)
	}
	tree := domain.BuildMeshTree(report, s.policy.WeakLQI)
	if tree.UpdatedAt.IsZero() {
		tree.UpdatedAt = s.Now()
	}

	s.mu.Lock()
	previous := s.meshes[report.SECID]
	s.meshes[report.SECID] = tree
	// A node the controller can see is a node that is alive, whatever the
	// heartbeat path thinks. Mesh reports are the second, independent source of
	// liveness and are what keeps a label whose telemetry window is long from
	// being declared offline.
	for _, n := range tree.Nodes {
		if n.Orphaned {
			continue
		}
		if d := s.devices[string(n.LabelID)]; d != nil && tree.UpdatedAt.After(d.LastSeen) {
			d.LastSeen = tree.UpdatedAt
		}
	}
	if d := s.devices[string(report.SECID)]; d != nil && tree.UpdatedAt.After(d.LastSeen) {
		d.LastSeen = tree.UpdatedAt
	}
	s.mu.Unlock()

	// Only a worsening is worth an event. A mesh that re-parents itself back to
	// health should not page anyone, and emitting on every report would put one
	// event per controller per scan on a stream sized for lifecycle changes.
	prevOrphans := 0
	if previous != nil {
		prevOrphans = len(previous.Orphans)
	}
	if len(tree.Orphans) > prevOrphans {
		if err := s.raiseMeshDegraded(ctx, report, tree); err != nil {
			s.log.Warn("registry could not record a mesh degradation",
				"sec_id", string(report.SECID), "error", err)
		}
	}
	return nil
}

// MeshDegraded is the payload of canon.EvtMeshLinkDegraded.
type MeshDegraded struct {
	SECID      canon.SECID     `json:"sec_id"`
	StoreID    canon.StoreID   `json:"store_id"`
	Orphans    []canon.LabelID `json:"orphans"`
	WeakLinks  int             `json:"weak_links"`
	Nodes      int             `json:"nodes"`
	MaxDepth   int             `json:"max_depth"`
	AverageLQI float64         `json:"average_lqi"`
	At         time.Time       `json:"at"`
}

func (s *Service) raiseMeshDegraded(ctx context.Context, report canon.MeshTopology, tree *domain.MeshTree) error {
	tenant := s.tenantOf(string(report.SECID))
	if tenant == "" {
		return nil
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	dev := s.device(string(report.SECID))
	if dev == nil {
		return nil
	}
	env, err := s.newEvent(canon.EvtMeshLinkDegraded, domain.AggregateDevice, string(report.SECID),
		tenant, report.StoreID, MeshDegraded{
			SECID:      tree.SECID,
			StoreID:    tree.StoreID,
			Orphans:    tree.Orphans,
			WeakLinks:  tree.WeakLinks,
			Nodes:      tree.Size(),
			MaxDepth:   tree.MaxDepth,
			AverageLQI: tree.AverageLQI,
			At:         tree.UpdatedAt,
		})
	if err != nil {
		return err
	}
	return s.commit(ctx, deviceStream(string(report.SECID)), dev.Version, env)
}

// RecordHeartbeat marks a controller as heard from. Heartbeats carry no
// payload the registry needs; their value is entirely in having arrived.
func (s *Service) RecordHeartbeat(secID canon.SECID, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.devices[string(secID)]; d != nil && at.After(d.LastSeen) {
		d.LastSeen = at.UTC()
	}
}

// SweepHealth derives every device's state from the evidence and records the
// transitions that follow.
//
// This is where "three missed beacons" actually happens. It is a sweep rather
// than a timer per device because 50 million timers is not a design, and
// because the sweep is what makes the behaviour deterministic in a test: with a
// fake clock, advancing time and calling SweepHealth is the whole experiment.
func (s *Service) SweepHealth(ctx context.Context) (int, error) {
	now := s.Now()
	type pending struct {
		id   string
		want domain.DeviceState
	}
	var todo []pending

	s.mu.RLock()
	for id, d := range s.devices {
		want, changed := s.policy.DeriveState(d, now)
		if changed {
			todo = append(todo, pending{id: id, want: want})
		}
	}
	s.mu.RUnlock()
	sort.Slice(todo, func(i, j int) bool { return todo[i].id < todo[j].id })

	applied := 0
	for _, p := range todo {
		s.cmdMu.Lock()
		dev := s.device(p.id)
		if dev == nil {
			s.cmdMu.Unlock()
			continue
		}
		// Re-derive under the command lock: the device may have been heard from
		// between the read above and now, and marking a live label offline
		// because of a stale read is exactly the false alarm this service exists
		// to avoid.
		want, changed := s.policy.DeriveState(dev, now)
		if !changed {
			s.cmdMu.Unlock()
			continue
		}
		change, err := deviceTransition(dev, want, healthReason(want), now)
		if err != nil {
			s.cmdMu.Unlock()
			return applied, err
		}
		env, err := s.newEvent(stateChangeEventName(want), domain.AggregateDevice, dev.ID,
			dev.TenantID, dev.Placement.StoreID, change)
		if err != nil {
			s.cmdMu.Unlock()
			return applied, err
		}
		err = s.commit(ctx, deviceStream(dev.ID), dev.Version, env)
		s.cmdMu.Unlock()
		if err != nil {
			return applied, err
		}
		if s.met != nil {
			s.met.transitions.With(string(want)).Inc()
		}
		applied++
	}
	return applied, nil
}

// stateChangeEventName picks the canonical event name for a transition.
//
// Going active and going offline have platform-wide names that other services
// already subscribe to, so those transitions carry them; everything else
// carries the registry's generic name. The payload is the same struct either
// way, which means a consumer can subscribe to just `device.state.changed` for
// a complete timeline, or to the two canon names for the two edges that matter
// most, without the registry maintaining two encodings.
func stateChangeEventName(to domain.DeviceState) string {
	switch to {
	case domain.StateActive:
		return canon.EvtDeviceOnline
	case domain.StateOffline:
		return canon.EvtDeviceOffline
	default:
		return domain.EvtDeviceStateChanged
	}
}

func healthReason(to domain.DeviceState) string {
	switch to {
	case domain.StateOffline:
		return "beacon budget exhausted"
	case domain.StateActive:
		return "heard from within the heartbeat window"
	case domain.StateDegraded:
		return "health criterion failed"
	default:
		return "health sweep"
	}
}

// SubscribeDeviceTraffic wires the registry to the upstream MQTT filters it
// owns: batched telemetry, mesh topology reports and controller heartbeats.
//
// The cloud-side filters are the canon.FilterAll* constants, which begin with a
// tenant wildcard: a cloud service is authorised across every tenant, where a
// device's credential is confined to its own.
func (s *Service) SubscribeDeviceTraffic(ctx context.Context) error {
	if s.cfg.Messenger == nil {
		return nil
	}
	if err := s.cfg.Messenger.Subscribe(ctx, canon.FilterAllTelemetry, msgbus.QoS(canon.QoSTelemetry),
		func(ctx context.Context, m msgbus.Message) { s.onTelemetryMessage(ctx, m) }); err != nil {
		return fmt.Errorf("registry: subscribe to telemetry: %w", err)
	}
	if err := s.cfg.Messenger.Subscribe(ctx, canon.FilterAllMesh, msgbus.QoS(canon.QoSTelemetry),
		func(ctx context.Context, m msgbus.Message) { s.onMeshMessage(ctx, m) }); err != nil {
		return fmt.Errorf("registry: subscribe to mesh reports: %w", err)
	}
	if err := s.cfg.Messenger.Subscribe(ctx, canon.FilterAllHeartbeats, msgbus.QoS(canon.QoSTelemetry),
		func(ctx context.Context, m msgbus.Message) { s.onHeartbeatMessage(ctx, m) }); err != nil {
		return fmt.Errorf("registry: subscribe to heartbeats: %w", err)
	}
	return nil
}

// onTelemetryMessage decodes a batched telemetry publication.
//
// The payload is an envelope whose body is an array of readings. A payload that
// does not decode is dropped with a log line rather than retried: it came off a
// QoS 0 topic carrying observations, and a controller shipping malformed
// telemetry needs a fix, not a redelivery.
func (s *Service) onTelemetryMessage(ctx context.Context, m msgbus.Message) {
	var env canon.Envelope
	if err := json.Unmarshal(m.Payload, &env); err != nil {
		s.log.Warn("undecodable telemetry envelope", "topic", m.Topic, "error", err)
		return
	}
	var readings []canon.Telemetry
	if err := env.Decode(&readings); err != nil {
		// A controller with a single label in its zone may send one object
		// rather than an array; accept both rather than losing the reading.
		var single canon.Telemetry
		if err2 := env.Decode(&single); err2 != nil {
			s.log.Warn("undecodable telemetry payload", "topic", m.Topic, "error", err)
			return
		}
		readings = []canon.Telemetry{single}
	}
	if err := s.IngestTelemetry(ctx, readings); err != nil {
		s.log.Warn("telemetry ingest failed", "topic", m.Topic, "error", err)
	}
}

func (s *Service) onMeshMessage(ctx context.Context, m msgbus.Message) {
	var env canon.Envelope
	if err := json.Unmarshal(m.Payload, &env); err != nil {
		s.log.Warn("undecodable mesh envelope", "topic", m.Topic, "error", err)
		return
	}
	var report canon.MeshTopology
	if err := env.Decode(&report); err != nil {
		s.log.Warn("undecodable mesh payload", "topic", m.Topic, "error", err)
		return
	}
	if report.SECID == "" {
		if _, sec, _, ok := canon.ParseSECTopic(m.Topic); ok {
			report.SECID = sec
		}
	}
	if err := s.IngestMeshReport(ctx, report); err != nil {
		s.log.Warn("mesh ingest failed", "topic", m.Topic, "error", err)
	}
}

func (s *Service) onHeartbeatMessage(_ context.Context, m msgbus.Message) {
	_, sec, _, ok := canon.ParseSECTopic(m.Topic)
	if !ok {
		return
	}
	at := m.ReceivedAt
	if at.IsZero() {
		at = s.Now()
	}
	s.RecordHeartbeat(sec, at)
}

// Package app is the Device Registry's application layer: the use cases that
// turn a request from the outside world into durable events, read-model updates
// and messages on the wire.
//
// # The shape of every write
//
// Every command in this package follows the same four steps, in this order:
//
//  1. decide, against the current read model, producing zero or more events;
//  2. append those events to the event store, under optimistic concurrency;
//  3. apply them to the read model;
//  4. publish them to the `device-events` stream and push any MQTT consequence.
//
// The order is the interesting part. The event store is the source of truth, so
// nothing is visible to a reader until it is durable. Publishing comes last and
// is tracked by a durable cursor, so a crash between (2) and (4) republishes on
// the next start rather than silently dropping the event — which matters more
// here than almost anywhere else in the platform, because the Label Service
// builds its entire fan-out directory from these events and never asks the
// registry a question. An assignment event that is lost is a label that is
// never repriced again.
//
// # What is event-sourced and what is not
//
// Device lifecycle is event-sourced: it is low volume, it is what an auditor
// asks about, and its history is the answer to "why is this label showing
// that". The manufacturing manifest and the planogram are stored as documents
// instead. They are bulk reference data — a manifest is a shipment, a planogram
// is a spreadsheet a merchandiser exported — and event-sourcing a 40,000-row
// upload would put a 40,000-row payload in the audit stream to record a change
// of three rows. Their *consequences* are events, which is what downstream
// consumers actually need.
//
// Telemetry is neither: at 167,000 readings a second it is forwarded to the
// `label-telemetry` stream and folded into an in-memory health read model. It
// is deliberately not durable in this service. A registry restart that lost
// three minutes of battery percentages costs nothing; one that made the write
// path 200 times more expensive would cost the fleet.
package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/internal/registry/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/obs"
)

// Config assembles a Service from its ports.
type Config struct {
	// Store is the event store the registry writes device history to. Its
	// underlying kvstore also holds the manifest and planogram documents, so
	// that a manifest ingest and the events it enables land in one write-ahead
	// log rather than two that can disagree after a crash.
	Store *eventstore.Store
	// Events publishes to the platform's durable streams. Nil disables
	// publishing, which is only appropriate in a test that asserts on the event
	// store directly.
	Events ports.EventStreamPublisher
	// Messenger is the MQTT path to the stores. Nil disables device messaging,
	// which is how the service runs in a development environment with no broker.
	Messenger ports.DeviceMessenger
	// Auth verifies device certificates at provisioning. Required.
	Auth ports.DeviceAuthenticator
	// Issuer mints device certificates for the development seeding endpoint.
	// Nil — the production configuration — disables seeding entirely.
	Issuer ports.DeviceIssuer
	// Clock is the time source; nil means ports.SystemClock.
	Clock ports.Clock
	// Health is the health-derivation policy; the zero value takes the
	// platform defaults.
	Health domain.HealthPolicy
	// Region is the geographic shard this instance serves. It becomes the region
	// segment of every MQTT topic the registry publishes to.
	Region canon.Region
	// Log receives operational events; nil is silent.
	Log *obs.Logger
	// Metrics, when non-nil, receives the registry's counters and gauges.
	Metrics *obs.Registry
	// Source names this component in every envelope it produces.
	Source string
}

// Service is the Device Registry application.
//
// It is safe for concurrent use. Two locks are held for different reasons and
// are never held at the same time in the other order: cmdMu serialises the
// decide-persist-apply-publish sequence so that two concurrent provisioning
// requests for the same device cannot both decide against the same stale state,
// and mu guards the read model so that queries and the telemetry path do not
// race with it.
type Service struct {
	cfg    Config
	store  *eventstore.Store
	kv     *kvstore.Store
	clock  ports.Clock
	policy domain.HealthPolicy
	log    *obs.Logger
	met    *metrics

	cmdMu sync.Mutex

	mu         sync.RWMutex
	devices    map[string]*domain.Device
	bySerial   map[string]string
	byEUI      map[string]string
	byStore    map[canon.StoreID]map[string]struct{}
	planograms map[canon.StoreID]*domain.Planogram
	meshes     map[canon.SECID]*domain.MeshTree
	tenants    map[canon.TenantID]struct{}

	// published is the global event-store position up to which events have been
	// handed to the durable stream. It is persisted so that a crash between the
	// append and the publish is repaired on the next start rather than losing a
	// fact the Label Service needs.
	publishMu sync.Mutex
	published int64
}

// metrics holds the registry's instrumentation. It is a struct rather than
// loose fields so that a Service built without a registry carries one nil
// pointer instead of ten.
type metrics struct {
	provisioned  *obs.CounterVec
	rejected     *obs.CounterVec
	transitions  *obs.CounterVec
	telemetry    *obs.CounterVec
	assignments  *obs.CounterVec
	devicesGauge *obs.GaugeVec
}

func newMetrics(r *obs.Registry) *metrics {
	if r == nil {
		return nil
	}
	return &metrics{
		provisioned: r.Counter("registry_provisioned_total",
			"Devices provisioned, by device kind.", "kind"),
		rejected: r.Counter("registry_provision_rejected_total",
			"Provisioning requests refused, by reason.", "reason"),
		transitions: r.Counter("registry_state_transitions_total",
			"Accepted lifecycle transitions, by destination state.", "state"),
		telemetry: r.Counter("registry_telemetry_readings_total",
			"Telemetry readings ingested."),
		assignments: r.Counter("registry_label_assignments_total",
			"Label assignment events emitted, by kind.", "kind"),
		devicesGauge: r.Gauge("registry_devices",
			"Devices on record, by lifecycle state.", "state"),
	}
}

// Errors returned by the application layer.
var (
	// ErrNotConfigured means a required port was not supplied.
	ErrNotConfigured = errors.New("registry: service is not configured")
)

// keyspace prefixes for the documents the registry keeps beside its events in
// the shared kvstore. They begin with an upper-case letter because the event
// store's own keys are all lower-case single letters, so the two namespaces
// cannot collide however either grows.
var (
	manifestPrefix  = []byte("R\x00man\x00")
	planogramPrefix = []byte("R\x00pgm\x00")
	outboxKey       = []byte("R\x00outbox\x00pos")
)

func manifestKey(tenant canon.TenantID, deviceID string) []byte {
	k := append([]byte(nil), manifestPrefix...)
	k = append(k, tenant...)
	k = append(k, 0)
	return append(k, deviceID...)
}

func planogramKey(store canon.StoreID) []byte {
	return append(append([]byte(nil), planogramPrefix...), store...)
}

// Open builds a Service and rebuilds its read model by replaying the event
// store.
//
// Replay is the whole start-up path: there is no separate "load state" step and
// no snapshot format that could drift from the events. A registry that starts
// from an empty directory and one that starts from ten million events run the
// same code, which is what makes the recovery path something the test suite
// exercises on every run rather than something discovered during an incident.
func Open(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("%w: event store is required", ErrNotConfigured)
	}
	if cfg.Auth == nil {
		return nil, fmt.Errorf("%w: device authenticator is required", ErrNotConfigured)
	}
	if cfg.Clock == nil {
		cfg.Clock = ports.SystemClock{}
	}
	if cfg.Log == nil {
		cfg.Log = obs.NopLogger()
	}
	if cfg.Source == "" {
		cfg.Source = "device-registry"
	}
	s := &Service{
		cfg:        cfg,
		store:      cfg.Store,
		kv:         cfg.Store.KV(),
		clock:      cfg.Clock,
		policy:     cfg.Health.WithDefaults(),
		log:        cfg.Log,
		met:        newMetrics(cfg.Metrics),
		devices:    make(map[string]*domain.Device),
		bySerial:   make(map[string]string),
		byEUI:      make(map[string]string),
		byStore:    make(map[canon.StoreID]map[string]struct{}),
		planograms: make(map[canon.StoreID]*domain.Planogram),
		meshes:     make(map[canon.SECID]*domain.MeshTree),
		tenants:    make(map[canon.TenantID]struct{}),
	}
	if err := s.replay(ctx); err != nil {
		return nil, err
	}
	if err := s.loadPlanograms(); err != nil {
		return nil, err
	}
	if err := s.loadOutbox(); err != nil {
		return nil, err
	}
	if err := s.drainOutbox(ctx); err != nil {
		// A stream that is unreachable at start-up must not stop the service
		// coming up: the registry's HTTP surface and its MQTT ingestion are
		// still useful, and the cursor means the backlog is republished on the
		// next attempt rather than lost.
		s.log.Warn("registry could not republish pending events at start-up", "error", err)
	}
	s.refreshGauges()
	return s, nil
}

// replay rebuilds the device read model from the event store.
func (s *Service) replay(ctx context.Context) error {
	const page = 4096
	from := int64(1)
	for {
		recs, err := s.store.ReadAll(ctx, from, page)
		if err != nil {
			return fmt.Errorf("registry: replay from position %d: %w", from, err)
		}
		if len(recs) == 0 {
			return nil
		}
		for _, rec := range recs {
			if err := s.applyLocked(rec.Event, rec.Version); err != nil {
				return fmt.Errorf("registry: replay event %s at position %d: %w",
					rec.Event.EventID, rec.Position, err)
			}
		}
		from = recs[len(recs)-1].Position + 1
	}
}

// loadPlanograms restores the stored planogram documents.
func (s *Service) loadPlanograms() error {
	it := s.kv.Scan(planogramPrefix)
	defer it.Close()
	for it.Next() {
		var pg domain.Planogram
		if err := json.Unmarshal(it.Value(), &pg); err != nil {
			return fmt.Errorf("registry: decode stored planogram: %w", err)
		}
		s.planograms[pg.StoreID] = &pg
	}
	return it.Err()
}

func (s *Service) loadOutbox() error {
	v, err := s.kv.Get(outboxKey)
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("registry: read publish cursor: %w", err)
	}
	if len(v) != 8 {
		return errors.New("registry: publish cursor is corrupt")
	}
	s.published = int64(binary.BigEndian.Uint64(v))
	return nil
}

// drainOutbox republishes every durable event the stream has not been told
// about yet. It is idempotent: the events carry stable identifiers and every
// consumer in the platform is required to be idempotent anyway, so republishing
// a handful after a crash is strictly better than losing one.
func (s *Service) drainOutbox(ctx context.Context) error {
	if s.cfg.Events == nil {
		return nil
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	for {
		recs, err := s.store.ReadAll(ctx, s.published+1, 512)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		envs := make([]canon.Envelope, 0, len(recs))
		for _, r := range recs {
			envs = append(envs, r.Event)
		}
		if err := s.cfg.Events.PublishEvents(ctx, canon.StreamDeviceEvents.Name, envs...); err != nil {
			return err
		}
		s.published = recs[len(recs)-1].Position
		if err := s.persistCursor(s.published); err != nil {
			return err
		}
	}
}

func (s *Service) persistCursor(pos int64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(pos))
	return s.kv.Put(outboxKey, b[:])
}

// Close releases the service. It does not close the event store, which the
// caller owns and may share with another component.
func (s *Service) Close() error { return nil }

// Now returns the service's current time.
func (s *Service) Now() time.Time { return s.clock.Now().UTC() }

// Policy returns the health policy in force.
func (s *Service) Policy() domain.HealthPolicy { return s.policy }

// ---------------------------------------------------------------------------
// Event construction and the write path
// ---------------------------------------------------------------------------

// newEvent builds an envelope in the registry's house style: correct aggregate
// coordinates, tenancy, region and source, so that no call site can produce an
// envelope the audit pipeline cannot route.
func (s *Service) newEvent(eventType, aggregateType, aggregateID string, tenant canon.TenantID, store canon.StoreID, payload any) (canon.Envelope, error) {
	env, err := canon.NewEnvelope(eventType, aggregateType, aggregateID, tenant, payload)
	if err != nil {
		return canon.Envelope{}, err
	}
	env.StoreID = store
	env.Region = s.cfg.Region
	env.Source = s.cfg.Source
	env.CorrelationID = canon.NewCorrelationID()
	now := s.Now()
	env.OccurredAt = now
	env.RecordedAt = now
	return env, nil
}

// commit appends events to a device's stream, applies them to the read model
// and publishes them.
//
// expectedVersion is the aggregate version the caller decided against;
// eventstore.ExpectedNoStream requires the device to be new. A concurrency
// conflict is returned to the caller unchanged, because the right response to
// one is to re-read and re-decide, and this layer cannot know whether that is
// still what the caller wants.
//
// It must be called with cmdMu held.
func (s *Service) commit(ctx context.Context, stream eventstore.StreamID, expectedVersion int64, envs ...canon.Envelope) error {
	if len(envs) == 0 {
		return nil
	}
	res, err := s.store.AppendWithResult(ctx, stream, expectedVersion, envs...)
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, rec := range res.Events {
		if err := s.applyLocked(rec.Event, rec.Version); err != nil {
			s.mu.Unlock()
			// The event is durable and the read model is now behind it. Failing
			// loudly is correct: a restart replays and repairs, whereas
			// continuing would serve queries from a model that has silently
			// diverged from the log.
			return fmt.Errorf("registry: apply committed event %s: %w", rec.Event.EventID, err)
		}
	}
	s.mu.Unlock()
	s.refreshGauges()

	if res.Duplicate {
		return nil
	}
	s.publish(ctx, res)
	return nil
}

// publish hands committed events to the durable stream and advances the cursor.
// A failure is logged rather than returned: the events are already durable, and
// the next drainOutbox — on the next commit or the next start-up — resends
// them. Returning an error here would make a stream blip look like a failed
// provisioning to the device on the shelf, which would retry and provision
// again.
func (s *Service) publish(ctx context.Context, res eventstore.AppendResult) {
	if s.cfg.Events == nil || len(res.Events) == 0 {
		return
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if res.Events[0].Position != s.published+1 {
		// Something is already pending; drain from the cursor so ordering holds.
		if err := s.drainFrom(ctx); err != nil {
			s.log.Warn("registry could not publish device events", "error", err)
		}
		return
	}
	envs := make([]canon.Envelope, 0, len(res.Events))
	for _, r := range res.Events {
		envs = append(envs, r.Event)
	}
	if err := s.cfg.Events.PublishEvents(ctx, canon.StreamDeviceEvents.Name, envs...); err != nil {
		s.log.Warn("registry could not publish device events", "error", err,
			"pending_from", s.published+1)
		return
	}
	s.published = res.Events[len(res.Events)-1].Position
	if err := s.persistCursor(s.published); err != nil {
		s.log.Warn("registry could not persist publish cursor", "error", err)
	}
}

// drainFrom republishes the backlog with publishMu already held.
func (s *Service) drainFrom(ctx context.Context) error {
	for {
		recs, err := s.store.ReadAll(ctx, s.published+1, 512)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		envs := make([]canon.Envelope, 0, len(recs))
		for _, r := range recs {
			envs = append(envs, r.Event)
		}
		if err := s.cfg.Events.PublishEvents(ctx, canon.StreamDeviceEvents.Name, envs...); err != nil {
			return err
		}
		s.published = recs[len(recs)-1].Position
		if err := s.persistCursor(s.published); err != nil {
			return err
		}
	}
}

// deviceStream returns the event-store stream for a device.
func deviceStream(deviceID string) eventstore.StreamID {
	return eventstore.Stream(domain.AggregateDevice, deviceID)
}

// ---------------------------------------------------------------------------
// Read-model projection
// ---------------------------------------------------------------------------

// applyLocked folds one event into the read model. It must be called with mu
// held, or during Open before the service is shared.
//
// Every write in the service goes through this function, whether it came from a
// live command or from replay. That is the property that makes recovery
// trustworthy: there is no second code path that could build a slightly
// different model from the same events.
func (s *Service) applyLocked(env canon.Envelope, version int64) error {
	switch env.EventType {
	// All three provisioning names fold into the same read model. The older
	// name is still handled for every tier because a store's event streams
	// predate the split and replay is the only way this service ever builds its
	// state: a registry that stopped understanding what it wrote last month
	// would come up having forgotten its controllers.
	case canon.EvtLabelProvisioned, canon.EvtSECProvisioned, canon.EvtSGUProvisioned:
		var p DeviceProvisioned
		if err := env.Decode(&p); err != nil {
			return err
		}
		s.applyProvisioned(p, version)

	case canon.EvtDeviceOnline, canon.EvtDeviceOffline, domain.EvtDeviceStateChanged:
		var p domain.DeviceStateChanged
		if err := env.Decode(&p); err != nil {
			return err
		}
		if d := s.devices[p.DeviceID]; d != nil {
			d.State = p.To
			d.StateReason = p.Reason
			d.StateChangedAt = p.ChangedAt
			d.Version = version
		}

	case domain.EvtDeviceQuarantined:
		var p domain.DeviceQuarantined
		if err := env.Decode(&p); err != nil {
			return err
		}
		s.applyQuarantined(p, version)

	case domain.EvtDeviceRetired:
		var p domain.DeviceRetired
		if err := env.Decode(&p); err != nil {
			return err
		}
		if d := s.devices[p.DeviceID]; d != nil {
			d.State = domain.StateRetired
			d.StateReason = p.Reason
			d.StateChangedAt = p.At
			d.Version = version
		}

	case canon.EvtLabelAssigned, domain.EvtLabelUnassigned:
		var p domain.LabelAssigned
		if err := env.Decode(&p); err != nil {
			return err
		}
		s.applyAssignment(p, version)

	case canon.EvtBatteryCritical:
		var p domain.BatteryCritical
		if err := env.Decode(&p); err != nil {
			return err
		}
		if d := s.devices[string(p.LabelID)]; d != nil {
			d.BatteryCriticalRaised = true
			d.Version = version
		}

	default:
		// An unknown event type on a device stream is not an error. The registry
		// must tolerate an event written by a newer build during a rolling
		// upgrade; skipping it is what the envelope contract requires.
	}
	return nil
}

func (s *Service) applyProvisioned(p DeviceProvisioned, version int64) {
	id := string(p.LabelID)
	d := s.devices[id]
	if d == nil {
		d = &domain.Device{ID: id, State: domain.StateManufactured}
		s.devices[id] = d
	}
	if old := d.Placement.StoreID; old != "" && old != p.StoreID {
		if set := s.byStore[old]; set != nil {
			delete(set, id)
		}
	}
	d.Kind = p.DeviceKind()
	d.TenantID = p.TenantID
	d.Serial = p.Serial
	d.EUI64 = p.EUI64
	d.HardwareTier = p.HardwareTier
	d.FirmwareVersion = p.FirmwareVer
	d.CertSerial = p.CertSerial
	d.CertNotAfter = p.CertNotAfter
	d.ProvisionedAt = p.ProvisionedAt
	d.Placement = domain.Placement{StoreID: p.StoreID, SECID: p.SECID, Zone: p.Zone}
	d.State = domain.StateProvisioned
	d.StateChangedAt = p.ProvisionedAt
	d.StateReason = "provisioned"
	d.Version = version
	// Re-provisioning is what happens after a technician replaces the cell, so
	// the battery alert latch is released here. Without this a label that was
	// once flat would never raise the alert again, however many cells it saw.
	d.BatteryCriticalRaised = false

	if p.Serial != "" {
		s.bySerial[p.Serial] = id
	}
	if p.EUI64 != "" {
		s.byEUI[p.EUI64] = id
	}
	if p.StoreID != "" {
		set := s.byStore[p.StoreID]
		if set == nil {
			set = make(map[string]struct{})
			s.byStore[p.StoreID] = set
		}
		set[id] = struct{}{}
	}
	if p.TenantID != "" {
		s.tenants[p.TenantID] = struct{}{}
	}
}

// applyQuarantined records a quarantine, creating the device entry when the
// identity was refused before it was ever enrolled.
//
// Creating it is the point. A certificate that verifies but fails its manifest
// check is exactly the event a security team needs to be able to look up by
// device identifier, and a registry that recorded the alert but not the subject
// would send them to a 404.
func (s *Service) applyQuarantined(p domain.DeviceQuarantined, version int64) {
	d := s.devices[p.DeviceID]
	if d == nil {
		d = &domain.Device{
			ID:       p.DeviceID,
			Kind:     p.Kind,
			TenantID: p.TenantID,
			Serial:   p.Serial,
			EUI64:    p.EUI64,
			State:    domain.StateManufactured,
		}
		if p.StoreID != "" {
			d.Placement.StoreID = p.StoreID
		} else {
			d.Placement.StoreID = p.ObservedStoreID
		}
		s.devices[p.DeviceID] = d
		if d.Serial != "" {
			s.bySerial[d.Serial] = d.ID
		}
		if store := d.Placement.StoreID; store != "" {
			set := s.byStore[store]
			if set == nil {
				set = make(map[string]struct{})
				s.byStore[store] = set
			}
			set[d.ID] = struct{}{}
		}
		if d.TenantID != "" {
			s.tenants[d.TenantID] = struct{}{}
		}
	}
	d.State = domain.StateQuarantined
	d.StateReason = string(p.Reason)
	d.StateChangedAt = p.At
	d.Version = version
}

func (s *Service) applyAssignment(p domain.LabelAssigned, version int64) {
	d := s.devices[string(p.LabelID)]
	if d == nil {
		return
	}
	d.Version = version
	if p.Sequence > d.AssignmentSequence {
		d.AssignmentSequence = p.Sequence
	}
	if p.Unassigned {
		d.Assignment = nil
		return
	}
	if p.SECID != "" {
		d.Placement.SECID = p.SECID
	}
	if p.Zone != "" {
		d.Placement.Zone = p.Zone
	}
	d.Assignment = &domain.Assignment{
		PositionKey: domain.PositionKey{Shelf: p.Shelf, Rail: p.Rail, Position: p.Position},
		SKU:         p.SKU,
		Facings:     p.Facings,
		Template:    p.Template,
		Sequence:    p.Sequence,
		AssignedAt:  p.AssignedAt,
	}
}

// refreshGauges republishes the per-state device populations.
func (s *Service) refreshGauges() {
	if s.met == nil {
		return
	}
	counts := make(map[domain.DeviceState]int, len(domain.AllStates()))
	s.mu.RLock()
	for _, d := range s.devices {
		counts[d.State]++
	}
	s.mu.RUnlock()
	for _, st := range domain.AllStates() {
		s.met.devicesGauge.With(string(st)).Set(float64(counts[st]))
	}
}

// ---------------------------------------------------------------------------
// MQTT helpers
// ---------------------------------------------------------------------------

// scopeFor builds the MQTT topic scope for a device.
func (s *Service) scopeFor(tenant canon.TenantID, store canon.StoreID) canon.TopicScope {
	return canon.TopicScope{Tenant: tenant, Region: s.cfg.Region, Store: store}
}

// pushRetained publishes a retained message, or clears one when payload is nil.
//
// Clearing is the reason this helper exists at all. MQTT's rule is that a
// zero-length retained publish deletes the broker's stored value for a topic,
// and the platform depends on it: a label reassigned to a different controller
// leaves a retained config and price behind on its old zone topic, and a
// controller rebooting after a power cut would otherwise replay them for a
// label that is no longer in its zone. Interface contract §3 makes clearing the
// registry's job, because the registry is the only component that knows the
// reassignment happened.
func (s *Service) pushRetained(ctx context.Context, topic string, payload []byte, qos msgbus.QoS) {
	if s.cfg.Messenger == nil {
		return
	}
	if err := s.cfg.Messenger.Publish(ctx, msgbus.Message{
		Topic:   topic,
		Payload: payload,
		QoS:     qos,
		Retain:  true,
	}); err != nil {
		s.log.Warn("registry could not publish retained message", "topic", topic, "error", err)
	}
}

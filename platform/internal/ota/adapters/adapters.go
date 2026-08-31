// Package adapters implements the OTA service's ports against real
// infrastructure: a content-addressed artifact store on disk, the durable event
// bus, an MQTT broker, and the Device Registry.
package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/internal/ota/ports"
	registryapp "github.com/usslp/usslp/platform/internal/registry/app"
	registrydomain "github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// FileArtifactStore keeps firmware images on disk, addressed by content.
//
// The layout is a two-character fan-out directory holding the full digest:
//
//	<root>/ab/abcdef…
//
// Two hundred firmware versions across a dozen hardware tiers is a few thousand
// files, and a flat directory of a few thousand entries is slow to enumerate on
// every filesystem that matters. The fan-out costs one extra path component and
// removes the problem permanently.
//
// Writes go to a temporary file and are renamed into place, so a process killed
// mid-write leaves a stray temporary file rather than a truncated image under a
// digest that promises the whole one. That distinction is the entire value of
// content addressing: a file that is present is a file that is correct.
type FileArtifactStore struct {
	root string
	mu   sync.RWMutex
}

// NewFileArtifactStore creates the store rooted at dir.
func NewFileArtifactStore(dir string) (*FileArtifactStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("adapters: artifact store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("adapters: create artifact store %s: %w", dir, err)
	}
	return &FileArtifactStore{root: dir}, nil
}

// pathFor returns the on-disk path for a content address, and rejects an
// identifier that is not one.
//
// The validation is not cosmetic: the identifier reaches this function from an
// HTTP request, and an unvalidated one containing path separators would let a
// caller read or write anywhere the process can reach.
func (s *FileArtifactStore) pathFor(artifactID string) (string, error) {
	digest := strings.TrimPrefix(artifactID, "sha256:")
	if len(digest) != 64 {
		return "", fmt.Errorf("%w: artifact id %q is not a sha256 content address", domain.ErrInvalid, artifactID)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("%w: artifact id %q is not hexadecimal", domain.ErrInvalid, artifactID)
	}
	return filepath.Join(s.root, digest[:2], digest), nil
}

// Put stores an image and returns its content address.
func (s *FileArtifactStore) Put(image []byte) (string, error) {
	sum := sha256.Sum256(image)
	id := "sha256:" + hex.EncodeToString(sum[:])
	path, err := s.pathFor(id)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		// Already present. Content addressing makes this unambiguous: the same
		// digest is the same bytes, so there is nothing to overwrite.
		return id, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("adapters: create artifact directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return "", fmt.Errorf("adapters: create temporary artifact file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(image); err != nil {
		tmp.Close()
		return "", fmt.Errorf("adapters: write artifact: %w", err)
	}
	// Firmware is the one payload in this platform that absolutely must survive
	// a power cut intact, so the file is synced before it is named.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("adapters: sync artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("adapters: close artifact: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("adapters: publish artifact: %w", err)
	}
	return id, nil
}

// Get returns an image by content address.
func (s *FileArtifactStore) Get(artifactID string) ([]byte, error) {
	path, err := s.pathFor(artifactID)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", domain.ErrArtifactNotFound, artifactID)
	}
	if err != nil {
		return nil, fmt.Errorf("adapters: read artifact %s: %w", artifactID, err)
	}
	return body, nil
}

// Has reports whether an image is stored.
func (s *FileArtifactStore) Has(artifactID string) bool {
	path, err := s.pathFor(artifactID)
	if err != nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, statErr := os.Stat(path)
	return statErr == nil
}

// MemoryArtifactStore keeps images in memory. It is what a test and a
// single-process development run use; nothing about the rollout logic changes
// between it and the file store, which is the point of the port.
type MemoryArtifactStore struct {
	mu     sync.RWMutex
	images map[string][]byte
}

// NewMemoryArtifactStore returns an empty store.
func NewMemoryArtifactStore() *MemoryArtifactStore {
	return &MemoryArtifactStore{images: make(map[string][]byte)}
}

// Put stores an image and returns its content address.
func (s *MemoryArtifactStore) Put(image []byte) (string, error) {
	id := domain.ArtifactIDFor(image)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.images[id]; !ok {
		s.images[id] = append([]byte(nil), image...)
	}
	return id, nil
}

// Get returns an image by content address.
func (s *MemoryArtifactStore) Get(artifactID string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	img, ok := s.images[artifactID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrArtifactNotFound, artifactID)
	}
	return append([]byte(nil), img...), nil
}

// Has reports whether an image is stored.
func (s *MemoryArtifactStore) Has(artifactID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.images[artifactID]
	return ok
}

// RegistryDirectory reads rollout targets from an in-process Device Registry.
//
// The OTA service and the registry are separate deployables in production and
// this adapter would speak HTTP to it. In a single-process development run and
// in the end-to-end test they share a process, and this adapter calls the
// registry's application layer directly — which is exactly the value of having
// the registry expose "which devices may be addressed" as a method rather than
// leaving each caller to reimplement the predicate.
type RegistryDirectory struct {
	registry *registryapp.Service
	// TimeZones maps a store to its IANA location, so quiet hours are evaluated
	// in the store's own local time. It is configuration rather than registry
	// state because a store's time zone is a property of the retailer's estate
	// record, not of any device in it.
	TimeZones map[canon.StoreID]string
}

// NewRegistryDirectory wraps a registry service.
func NewRegistryDirectory(r *registryapp.Service, zones map[canon.StoreID]string) *RegistryDirectory {
	if zones == nil {
		zones = map[canon.StoreID]string{}
	}
	return &RegistryDirectory{registry: r, TimeZones: zones}
}

// Targets returns every addressable device of a hardware tier.
func (d *RegistryDirectory) Targets(_ context.Context, tenant canon.TenantID, stores []canon.StoreID, hardwareTier string) ([]ports.Target, error) {
	if d == nil || d.registry == nil {
		return nil, nil
	}
	if len(stores) == 0 {
		stores = d.registry.Stores()
	}
	var out []ports.Target
	for _, store := range stores {
		for _, dev := range d.registry.DevicesForOTA(store, hardwareTier) {
			if dev.TenantID != tenant {
				continue
			}
			// Controllers and gateways are updated by a different pipeline: they
			// are mains powered, they hold the store's own broker, and taking one
			// down takes its whole shelf section off the air. This service rolls
			// out to labels.
			if dev.Kind != registrydomain.KindLabel {
				continue
			}
			battery := 0
			if pct, ok := dev.BatteryPercent(); ok {
				battery = pct
			}
			out = append(out, ports.Target{
				DeviceID:        dev.ID,
				StoreID:         dev.Placement.StoreID,
				SECID:           dev.Placement.SECID,
				Zone:            dev.Placement.Zone,
				HardwareTier:    dev.HardwareTier,
				FirmwareVersion: dev.FirmwareVersion,
				BatteryPct:      battery,
				TimeZone:        d.TimeZones[store],
			})
		}
	}
	return out, nil
}

// BusPublisher publishes rollout events onto `ota-commands`.
type BusPublisher struct {
	bus eventbus.Publisher
}

// NewBusPublisher wraps an event bus.
func NewBusPublisher(bus eventbus.Publisher) *BusPublisher { return &BusPublisher{bus: bus} }

// StreamKey returns the partition key an envelope carries on a stream.
//
// Interface contract §2 keys `ota-commands` by `device_id`. The OTA service's
// aggregate is the *job*, so the key has to come from the payload: a device's
// outcomes must be strictly ordered relative to each other, while different
// devices proceed in parallel across 128 partitions. Job-level events — the
// job created, a cohort advanced, a rollback — have no device and are keyed by
// the job, which keeps a rollout's own timeline ordered.
func StreamKey(env canon.Envelope) string {
	var keyed struct {
		DeviceID string `json:"device_id"`
	}
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &keyed); err == nil && keyed.DeviceID != "" {
			return keyed.DeviceID
		}
	}
	if env.AggregateID != "" {
		return env.AggregateID
	}
	return string(env.TenantID)
}

// PublishEvents serialises and publishes a batch.
func (p *BusPublisher) PublishEvents(ctx context.Context, stream string, envs ...canon.Envelope) error {
	if p == nil || p.bus == nil || len(envs) == 0 {
		return nil
	}
	msgs := make([]eventbus.Message, 0, len(envs))
	for _, env := range envs {
		if err := env.Validate(); err != nil {
			return fmt.Errorf("adapters: refusing to publish an invalid envelope: %w", err)
		}
		body, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("adapters: encode envelope %s: %w", env.EventID, err)
		}
		msgs = append(msgs, eventbus.Message{
			Topic: stream,
			Key:   StreamKey(env),
			Value: body,
			Headers: map[string]string{
				eventbus.HeaderEventType:     env.EventType,
				eventbus.HeaderTenantID:      string(env.TenantID),
				eventbus.HeaderStoreID:       string(env.StoreID),
				eventbus.HeaderCorrelationID: string(env.CorrelationID),
				eventbus.HeaderSchemaVersion: "1",
				eventbus.HeaderIdempotency:   env.IdempotencyKey,
			},
			Timestamp: env.RecordedAt,
		})
	}
	return p.bus.Publish(ctx, msgs...)
}

// Messenger adapts an msgbus.Client to the OTA service's port.
type Messenger struct{ client msgbus.Client }

// NewMessenger wraps an MQTT client.
func NewMessenger(c msgbus.Client) *Messenger { return &Messenger{client: c} }

// Publish sends one message.
func (m *Messenger) Publish(ctx context.Context, msg msgbus.Message) error {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client.Publish(ctx, msg)
}

// Subscribe registers a handler for a topic filter.
func (m *Messenger) Subscribe(ctx context.Context, filter string, qos msgbus.QoS, h msgbus.Handler) error {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client.Subscribe(ctx, filter, qos, h)
}

// NopMessenger discards publishes and accepts subscriptions, so the service can
// run with no broker reachable.
type NopMessenger struct{}

// Publish discards the message.
func (NopMessenger) Publish(context.Context, msgbus.Message) error { return nil }

// Subscribe accepts the filter and delivers nothing.
func (NopMessenger) Subscribe(context.Context, string, msgbus.QoS, msgbus.Handler) error { return nil }

// RecordingMessenger keeps every published message, so a test can assert on the
// QoS and retain flags interface contract §3 requires of an OTA trigger.
type RecordingMessenger struct {
	mu       sync.Mutex
	messages []msgbus.Message
	handlers map[string]msgbus.Handler
}

// NewRecordingMessenger returns an empty recorder.
func NewRecordingMessenger() *RecordingMessenger {
	return &RecordingMessenger{handlers: make(map[string]msgbus.Handler)}
}

// Publish records the message.
func (r *RecordingMessenger) Publish(_ context.Context, m msgbus.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m.ReceivedAt = time.Now().UTC()
	r.messages = append(r.messages, m)
	return nil
}

// Subscribe records the handler.
func (r *RecordingMessenger) Subscribe(_ context.Context, filter string, _ msgbus.QoS, h msgbus.Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[filter] = h
	return nil
}

// Deliver hands a message to the handler registered for a filter.
func (r *RecordingMessenger) Deliver(ctx context.Context, filter string, m msgbus.Message) bool {
	r.mu.Lock()
	h := r.handlers[filter]
	r.mu.Unlock()
	if h == nil {
		return false
	}
	h(ctx, m)
	return true
}

// Messages returns a copy of everything published.
func (r *RecordingMessenger) Messages() []msgbus.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]msgbus.Message(nil), r.messages...)
}

// Reset discards the recorded messages.
func (r *RecordingMessenger) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = nil
}

// StaticDirectory is a fixed target list, for tests and for a development run
// with no registry attached.
type StaticDirectory struct {
	mu      sync.RWMutex
	targets []ports.Target
}

// NewStaticDirectory returns a directory over a fixed list.
func NewStaticDirectory(targets ...ports.Target) *StaticDirectory {
	return &StaticDirectory{targets: append([]ports.Target(nil), targets...)}
}

// Set replaces the target list, so a test can model a fleet that changes while
// a rollout is running.
func (d *StaticDirectory) Set(targets []ports.Target) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targets = append([]ports.Target(nil), targets...)
}

// Targets returns the matching devices.
func (d *StaticDirectory) Targets(_ context.Context, _ canon.TenantID, stores []canon.StoreID, hardwareTier string) ([]ports.Target, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	allowed := make(map[canon.StoreID]bool, len(stores))
	for _, s := range stores {
		allowed[s] = true
	}
	out := make([]ports.Target, 0, len(d.targets))
	for _, t := range d.targets {
		if len(allowed) > 0 && !allowed[t.StoreID] {
			continue
		}
		if hardwareTier != "" && t.HardwareTier != hardwareTier {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// Assert that the adapters satisfy the ports they are written against.
var (
	_ ports.ArtifactStore        = (*FileArtifactStore)(nil)
	_ ports.ArtifactStore        = (*MemoryArtifactStore)(nil)
	_ ports.FleetDirectory       = (*RegistryDirectory)(nil)
	_ ports.FleetDirectory       = (*StaticDirectory)(nil)
	_ ports.EventStreamPublisher = (*BusPublisher)(nil)
	_ ports.DeviceMessenger      = (*Messenger)(nil)
	_ ports.DeviceMessenger      = NopMessenger{}
	_ ports.DeviceMessenger      = (*RecordingMessenger)(nil)
)

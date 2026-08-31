package app_test

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/adapters"
	"github.com/usslp/usslp/platform/internal/registry/app"
	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/internal/registry/ports"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// fakeClock is a manually advanced clock. Every time-dependent behaviour in the
// registry — three missed beacons, a battery runway fitted over hours — is
// asserted against one of these rather than against a sleep, because a test
// that sleeps is a test that fails on a loaded build machine and teaches the
// team to re-run it.
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

// capturedPublisher records every envelope handed to the durable stream, which
// is how the tests assert on the cross-service contract the Label Service
// depends on.
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

func (p *capturedPublisher) events(stream string) []canon.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]canon.Envelope(nil), p.streams[stream]...)
}

// teePublisher fans every batch out to two publishers, stopping at the first
// error. It exists so that a test can keep asserting on captured envelopes
// while a second publisher drives real consumers off a real event log.
type teePublisher []ports.EventStreamPublisher

func (t teePublisher) PublishEvents(ctx context.Context, stream string, envs ...canon.Envelope) error {
	for _, p := range t {
		if p == nil {
			continue
		}
		if err := p.PublishEvents(ctx, stream, envs...); err != nil {
			return err
		}
	}
	return nil
}

func (p *capturedPublisher) ofType(stream, eventType string) []canon.Envelope {
	var out []canon.Envelope
	for _, e := range p.events(stream) {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

// harness is a fully wired registry: a real certificate hierarchy, a real event
// store on a temporary directory, and recording adapters for the two ports
// whose effects the tests need to see.
type harness struct {
	t         *testing.T
	svc       *app.Service
	clock     *fakeClock
	hierarchy *pki.Hierarchy
	pub       *capturedPublisher
	mqtt      *adapters.RecordingMessenger
	store     *eventstore.Store
	kv        *kvstore.Store
	dir       string
	tenant    canon.TenantID
	storeID   canon.StoreID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessAt(t, t.TempDir(), time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC))
}

func newHarnessAt(t *testing.T, dir string, at time.Time) *harness {
	t.Helper()
	profile := pki.TestProfile()
	// The hierarchy is bootstrapped at the fake clock's instant so that its
	// certificates are valid in the test's own timeline rather than in the
	// build machine's.
	h, err := pki.Bootstrap(pki.BootstrapConfig{Profile: &profile, Now: at})
	if err != nil {
		t.Fatalf("bootstrap pki hierarchy: %v", err)
	}
	return newHarnessWithPKI(t, dir, at, h)
}

func newHarnessWithPKI(t *testing.T, dir string, at time.Time, h *pki.Hierarchy) *harness {
	t.Helper()
	return newHarnessOn(t, dir, at, h, nil)
}

// newHarnessOn builds the harness with an extra stream publisher wired in
// alongside the recording one, which is how a test drives a real downstream
// consumer group off the registry's real output.
func newHarnessOn(t *testing.T, dir string, at time.Time, h *pki.Hierarchy, extra ports.EventStreamPublisher) *harness {
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
	svc, err := app.Open(context.Background(), app.Config{
		Store:     es,
		Events:    teePublisher{pub, extra},
		Messenger: mqtt,
		Auth:      adapters.NewHierarchyAuthenticator(h),
		Issuer:    adapters.NewHierarchyIssuer(h),
		Clock:     clock,
		Region:    canon.Region("eu-west-1"),
	})
	if err != nil {
		t.Fatalf("open registry service: %v", err)
	}
	hr := &harness{
		t: t, svc: svc, clock: clock, hierarchy: h, pub: pub, mqtt: mqtt,
		store: es, kv: kv, dir: dir,
		tenant: canon.TenantID("acme"), storeID: canon.StoreID("store-0042"),
	}
	t.Cleanup(func() {
		_ = es.Close()
		_ = kv.Close()
	})
	return hr
}

// reopen closes the service's stores and opens a fresh one over the same
// directory, which is how the tests assert that state survives a restart.
func (h *harness) reopen() *harness {
	h.t.Helper()
	if err := h.store.Close(); err != nil {
		h.t.Fatalf("close eventstore: %v", err)
	}
	if err := h.kv.Close(); err != nil {
		h.t.Fatalf("close kvstore: %v", err)
	}
	return newHarnessWithPKI(h.t, h.dir, h.clock.Now(), h.hierarchy)
}

// enrolled is a device the test has manufactured: its identity, its certificate
// chain in the form a controller would forward, and its manifest record.
type enrolled struct {
	deviceID string
	kind     domain.DeviceKind
	eui      string
	serial   string
	chainPEM []byte
	record   domain.ManufacturingRecord
}

// manufacture issues a certificate from the platform's Manufacturing Sub-CA and
// builds the matching manifest record, exactly as a production line would.
func (h *harness) manufacture(deviceID string, kind domain.DeviceKind, euiSuffix uint64) enrolled {
	h.t.Helper()
	return h.manufactureIn(h.hierarchy, h.tenant, h.storeID, deviceID, kind, euiSuffix)
}

func (h *harness) manufactureIn(hier *pki.Hierarchy, tenant canon.TenantID, store canon.StoreID, deviceID string, kind domain.DeviceKind, euiSuffix uint64) enrolled {
	h.t.Helper()
	var idKind pki.IdentityKind
	switch kind {
	case domain.KindLabel:
		idKind = pki.KindLabel
	case domain.KindSEC:
		idKind = pki.KindSEC
	default:
		idKind = pki.KindSGU
	}
	issued, _, err := hier.IssueLeaf(pki.Identity{
		Kind: idKind, TenantID: tenant, StoreID: store, DeviceID: deviceID,
	}, pki.LeafOptions{Now: h.clock.Now()})
	if err != nil {
		h.t.Fatalf("issue certificate for %s: %v", deviceID, err)
	}
	spki, err := x509.MarshalPKIXPublicKey(issued.Certificate.PublicKey)
	if err != nil {
		h.t.Fatalf("marshal public key for %s: %v", deviceID, err)
	}
	eui := strings.ToUpper(fmt.Sprintf("%016x", 0x0011223300000000|euiSuffix))
	return enrolled{
		deviceID: deviceID,
		kind:     kind,
		eui:      eui,
		serial:   canon.DeviceSerial(0x0011223300000000 | euiSuffix),
		chainPEM: issued.ChainPEM(),
		record: domain.ManufacturingRecord{
			Serial:          canon.DeviceSerial(0x0011223300000000 | euiSuffix),
			EUI64:           eui,
			HardwareTier:    "esl-2.9-bw",
			FirmwareVersion: "1.0.0",
			TenantID:        tenant,
			StoreID:         store,
			DeviceID:        deviceID,
			Kind:            kind,
			CertSerial:      issued.Identity.SerialNumber,
			PublicKeySPKI:   spki,
			BatchID:         "batch-001",
			ManufacturedAt:  h.clock.Now(),
		},
	}
}

// ingest stores the manifest records for a set of manufactured devices.
func (h *harness) ingest(devices ...enrolled) {
	h.t.Helper()
	m := &domain.Manifest{TenantID: h.tenant, BatchID: "batch-001"}
	for _, d := range devices {
		m.Records = append(m.Records, d.record)
	}
	if _, err := h.svc.IngestManifest(context.Background(), m); err != nil {
		h.t.Fatalf("ingest manifest: %v", err)
	}
}

// provision enrols a manufactured device through the real provisioning path.
func (h *harness) provision(d enrolled, sec canon.SECID, zone string) *app.ProvisionResult {
	h.t.Helper()
	res, err := h.svc.Provision(context.Background(), app.ProvisionRequest{
		CertificateChainPEM: d.chainPEM,
		EUI64:               d.eui,
		SECID:               sec,
		Zone:                zone,
		FirmwareVersion:     "1.0.0",
		ReportedBy:          sec,
	})
	if err != nil {
		h.t.Fatalf("provision %s: %v", d.deviceID, err)
	}
	return res
}

// mustDevice fetches a device or fails the test.
func (h *harness) mustDevice(id string) *domain.Device {
	h.t.Helper()
	d := h.svc.Device(id)
	if d == nil {
		h.t.Fatalf("device %s is not on record", id)
	}
	return d
}

// retainedFor returns every message published to a topic, in order.
func (h *harness) retainedFor(topic string) [][]byte {
	h.t.Helper()
	var out [][]byte
	for _, m := range h.mqtt.Messages() {
		if m.Topic == topic {
			out = append(out, m.Payload)
		}
	}
	return out
}

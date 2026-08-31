package app_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/app"
	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/pki"
)

func TestProvisionHappyPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)

	res := h.provision(label, "sec-01", "aisle-01")

	if res.Reprovisioned {
		t.Fatal("a first enrolment must not be reported as a re-provisioning")
	}
	dev := h.mustDevice("lbl-0001")
	if dev.State != domain.StateProvisioned {
		t.Fatalf("state = %s, want provisioned", dev.State)
	}
	if dev.Placement.StoreID != h.storeID {
		t.Fatalf("store = %s, want %s (the store must come from the certificate, not the request)",
			dev.Placement.StoreID, h.storeID)
	}
	if dev.Placement.SECID != "sec-01" || dev.Placement.Zone != "aisle-01" {
		t.Fatalf("placement = %+v, want sec-01/aisle-01", dev.Placement)
	}
	if dev.Serial != label.serial || dev.EUI64 != label.eui {
		t.Fatalf("identity = %s/%s, want %s/%s", dev.Serial, dev.EUI64, label.serial, label.eui)
	}
	if dev.CertSerial != label.record.CertSerial {
		t.Fatalf("cert serial = %s, want %s", dev.CertSerial, label.record.CertSerial)
	}

	// The configuration handed back must address the label through its
	// controller's zone namespace, which is what keeps fan-out affordable.
	wantPrice := "usslp/acme/eu-west-1/store-0042/sec/sec-01/labels/lbl-0001/price"
	if res.Config.Topics.Price != wantPrice {
		t.Fatalf("price topic = %s, want %s", res.Config.Topics.Price, wantPrice)
	}
	if res.Config.Topics.OTA != "usslp/acme/eu-west-1/store-0042/sec/sec-01/labels/lbl-0001/ota" {
		t.Fatalf("ota topic = %s", res.Config.Topics.OTA)
	}

	// device.label.provisioned must be on the device-events stream, and its
	// payload must decode into the canonical struct interface contract §2
	// promises.
	events := h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtLabelProvisioned)
	if len(events) != 1 {
		t.Fatalf("published %d provisioning events, want 1", len(events))
	}
	var canonical canon.DeviceProvisioned
	if err := events[0].Decode(&canonical); err != nil {
		t.Fatalf("provisioning payload does not decode as canon.DeviceProvisioned: %v", err)
	}
	if canonical.LabelID != "lbl-0001" || canonical.SECID != "sec-01" || canonical.Serial != label.serial {
		t.Fatalf("canonical payload = %+v", canonical)
	}
	if events[0].StoreID != h.storeID || events[0].TenantID != h.tenant {
		t.Fatalf("envelope tenancy = %s/%s", events[0].TenantID, events[0].StoreID)
	}

	// The configuration is pushed retained, so a controller rebooting after a
	// power cut recovers it from the local broker.
	retained := h.retainedFor(wantPrice)
	if len(retained) != 0 {
		t.Fatalf("provisioning must not publish a price; got %d", len(retained))
	}
	cfgTopic := "usslp/acme/eu-west-1/store-0042/sec/sec-01/labels/lbl-0001/config"
	if got := h.retainedFor(cfgTopic); len(got) != 1 || len(got[0]) == 0 {
		t.Fatalf("expected exactly one non-empty retained configuration on %s, got %d", cfgTopic, len(got))
	}
}

func TestProvisionIsIdempotentForARetriedAnnouncement(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)

	h.provision(label, "sec-01", "aisle-01")
	first := h.mustDevice("lbl-0001").Version

	// A label that does not hear its configuration announces again. The mesh
	// does this routinely and it must not produce a second provisioning event.
	h.provision(label, "sec-01", "aisle-01")
	if got := h.mustDevice("lbl-0001").Version; got != first {
		t.Fatalf("version moved from %d to %d on a retried announcement", first, got)
	}
	if n := len(h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtLabelProvisioned)); n != 1 {
		t.Fatalf("published %d provisioning events for one device, want 1", n)
	}
}

func TestProvisionRejectsAForeignCertificateHierarchy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A complete, internally valid hierarchy that this platform has never heard
	// of. Its certificates verify perfectly against their own root, which is
	// exactly why the registry must anchor verification on its own.
	profile := pki.TestProfile()
	profile.Organization = "Impostor Retail Systems"
	foreign, err := pki.Bootstrap(pki.BootstrapConfig{Profile: &profile, Now: h.clock.Now()})
	if err != nil {
		t.Fatalf("bootstrap foreign hierarchy: %v", err)
	}
	rogue := h.manufactureIn(foreign, h.tenant, h.storeID, "lbl-9999", domain.KindLabel, 99)

	// Even with a manifest record present — the strongest possible position for
	// the attacker — the chain must not verify.
	h.ingest(rogue)

	_, err = h.svc.Provision(context.Background(), app.ProvisionRequest{
		CertificateChainPEM: rogue.chainPEM,
		EUI64:               rogue.eui,
		SECID:               "sec-01",
	})
	if err == nil {
		t.Fatal("a certificate from a foreign hierarchy was accepted")
	}
	if !errors.Is(err, pki.ErrUnknownAuthority) {
		t.Fatalf("error = %v, want pki.ErrUnknownAuthority", err)
	}
	if h.svc.Device("lbl-9999") != nil {
		t.Fatal("a rejected device must not appear in the registry")
	}
}

func TestProvisionRejectsARevokedCertificate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)
	h.provision(label, "sec-01", "aisle-01")

	// The unit is stolen off a shelf and its certificate revoked.
	chain, err := parseChain(label.chainPEM)
	if err != nil {
		t.Fatalf("parse chain: %v", err)
	}
	if err := h.hierarchy.RevokeCertificate(chain[0], pki.ReasonKeyCompromise, h.clock.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err = h.svc.Provision(context.Background(), app.ProvisionRequest{
		CertificateChainPEM: label.chainPEM,
		EUI64:               label.eui,
		SECID:               "sec-02",
	})
	if !errors.Is(err, pki.ErrRevoked) {
		t.Fatalf("error = %v, want pki.ErrRevoked", err)
	}
}

func TestProvisionDetectsACloneInAnotherStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)
	h.provision(label, "sec-01", "aisle-01")

	// The same identity, cloned onto hardware in a different store. The store is
	// bound into the certificate, so the attacker must also present a
	// certificate for the other store — which means a second manufacturing
	// record and a second certificate. The registry catches it on the radio
	// address, which is burned into the transceiver and cannot be chosen.
	clone := h.manufacture("lbl-0002", domain.KindLabel, 2)
	clone.record.EUI64 = label.eui // the cloned unit reports the original's radio
	h.ingest(clone)

	_, err := h.svc.Provision(context.Background(), app.ProvisionRequest{
		CertificateChainPEM: clone.chainPEM,
		EUI64:               label.eui,
		SECID:               "sec-07",
	})
	if !errors.Is(err, app.ErrCloneDetected) {
		t.Fatalf("error = %v, want app.ErrCloneDetected", err)
	}

	dev := h.mustDevice("lbl-0002")
	if dev.State != domain.StateQuarantined {
		t.Fatalf("cloned identity is %s, want quarantined", dev.State)
	}
	alerts := h.pub.ofType(canon.StreamDeviceEvents.Name, domain.EvtDeviceQuarantined)
	if len(alerts) != 1 {
		t.Fatalf("published %d quarantine alerts, want 1", len(alerts))
	}
	var q domain.DeviceQuarantined
	if err := alerts[0].Decode(&q); err != nil {
		t.Fatalf("decode quarantine alert: %v", err)
	}
	if q.Reason != domain.ReasonDuplicateIdentity {
		t.Fatalf("quarantine reason = %s, want duplicate-identity", q.Reason)
	}
	if !strings.Contains(q.Detail, "lbl-0001") {
		t.Fatalf("quarantine detail %q should name the device already holding the radio address", q.Detail)
	}
}

func TestProvisionDetectsADeviceAppearingInTwoStores(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)
	h.provision(label, "sec-01", "aisle-01")

	// The same device identity, certified for a second store. Only an attacker
	// with issuance capability can produce this, and the registry must still
	// refuse it because one physical label cannot be in two buildings.
	moved := h.manufactureIn(h.hierarchy, h.tenant, canon.StoreID("store-0099"), "lbl-0001", domain.KindLabel, 1)
	if _, err := h.svc.IngestManifest(context.Background(), &domain.Manifest{
		TenantID: h.tenant, BatchID: "batch-002", Records: []domain.ManufacturingRecord{moved.record},
	}); err == nil {
		t.Fatal("a manifest record that contradicts a stored one must be refused")
	}

	// Force the conflicting record in under its own store so the anti-cloning
	// check, not the manifest check, is what refuses the enrolment.
	_, err := h.svc.Provision(context.Background(), app.ProvisionRequest{
		CertificateChainPEM: moved.chainPEM,
		EUI64:               moved.eui,
		SECID:               "sec-31",
	})
	if err == nil {
		t.Fatal("a device announcing from a second store was accepted")
	}
	if !errors.Is(err, app.ErrManifestMismatch) && !errors.Is(err, app.ErrCloneDetected) {
		t.Fatalf("error = %v, want a manifest mismatch or a clone detection", err)
	}
	if got := h.mustDevice("lbl-0001").State; got != domain.StateQuarantined {
		t.Fatalf("state = %s, want quarantined", got)
	}
}

func TestProvisionRejectsAManifestMismatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)

	// The factory recorded a different radio address than the unit announces.
	label.record.EUI64 = "00112233AABBCCDD"
	h.ingest(label)

	_, err := h.svc.Provision(context.Background(), app.ProvisionRequest{
		CertificateChainPEM: label.chainPEM,
		EUI64:               label.eui,
		SECID:               "sec-01",
	})
	if !errors.Is(err, app.ErrManifestMismatch) {
		t.Fatalf("error = %v, want app.ErrManifestMismatch", err)
	}
	if got := h.mustDevice("lbl-0001").State; got != domain.StateQuarantined {
		t.Fatalf("state = %s, want quarantined", got)
	}
}

func TestProvisionRefusesADeviceWithNoManufacturingRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)

	_, err := h.svc.Provision(context.Background(), app.ProvisionRequest{
		CertificateChainPEM: label.chainPEM,
		EUI64:               label.eui,
		SECID:               "sec-01",
	})
	if !errors.Is(err, app.ErrUnknownDevice) {
		t.Fatalf("error = %v, want app.ErrUnknownDevice", err)
	}
}

func TestProvisionRefusesARetiredIdentity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)
	h.provision(label, "sec-01", "aisle-01")

	if err := h.svc.Retire(context.Background(), "lbl-0001", "scrapped"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	_, err := h.svc.Provision(context.Background(), app.ProvisionRequest{
		CertificateChainPEM: label.chainPEM,
		EUI64:               label.eui,
		SECID:               "sec-01",
	})
	if !errors.Is(err, app.ErrRetired) {
		t.Fatalf("error = %v, want app.ErrRetired", err)
	}
}

func TestReprovisioningBetweenControllersClearsTheOldRetainedState(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)
	h.provision(label, "sec-01", "aisle-01")
	h.mqtt.Reset()

	h.provision(label, "sec-02", "aisle-02")

	// Interface contract §3: the registry clears the stale retained messages on
	// the controller the label left, with a zero-length retained publish.
	for _, leaf := range []string{"config", "price"} {
		topic := "usslp/acme/eu-west-1/store-0042/sec/sec-01/labels/lbl-0001/" + leaf
		msgs := h.retainedFor(topic)
		if len(msgs) != 1 {
			t.Fatalf("expected one clearing publish on %s, got %d", topic, len(msgs))
		}
		if len(msgs[0]) != 0 {
			t.Fatalf("clearing publish on %s carried %d bytes, want zero", topic, len(msgs[0]))
		}
	}
	for _, m := range h.mqtt.Messages() {
		if strings.Contains(m.Topic, "/sec/sec-01/labels/lbl-0001/") && !m.Retain {
			t.Fatalf("clearing publish on %s was not retained; a non-retained empty message deletes nothing", m.Topic)
		}
	}
	if got := h.mustDevice("lbl-0001").Placement.SECID; got != "sec-02" {
		t.Fatalf("controller = %s, want sec-02", got)
	}
}

func TestManifestIngestIsIdempotentAndRefusesContradictions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)

	m := &domain.Manifest{TenantID: h.tenant, Records: []domain.ManufacturingRecord{label.record}}
	stored, err := h.svc.IngestManifest(context.Background(), m)
	if err != nil || stored != 1 {
		t.Fatalf("first ingest stored %d records, err %v", stored, err)
	}
	m2 := &domain.Manifest{TenantID: h.tenant, Records: []domain.ManufacturingRecord{label.record}}
	stored, err = h.svc.IngestManifest(context.Background(), m2)
	if err != nil || stored != 0 {
		t.Fatalf("re-ingest stored %d records, err %v; a retried upload must be a no-op", stored, err)
	}

	contradiction := label.record
	contradiction.HardwareTier = "esl-4.2-bwr"
	m3 := &domain.Manifest{TenantID: h.tenant, Records: []domain.ManufacturingRecord{contradiction}}
	if _, err := h.svc.IngestManifest(context.Background(), m3); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("error = %v, want domain.ErrAlreadyExists", err)
	}
}

func TestManifestRejectsDuplicateRadioAddressesInOneBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := h.manufacture("lbl-0001", domain.KindLabel, 1)
	b := h.manufacture("lbl-0002", domain.KindLabel, 2)
	b.record.EUI64 = a.record.EUI64

	_, err := h.svc.IngestManifest(context.Background(), &domain.Manifest{
		TenantID: h.tenant, Records: []domain.ManufacturingRecord{a.record, b.record},
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("error = %v, want domain.ErrInvalid", err)
	}
}

func TestQuarantineReleaseReturnsADeviceToProvisioned(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)
	h.provision(label, "sec-01", "aisle-01")

	if err := h.svc.Quarantine(context.Background(), "lbl-0001", "suspected tamper"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if got := h.mustDevice("lbl-0001").State; got != domain.StateQuarantined {
		t.Fatalf("state = %s, want quarantined", got)
	}
	if err := h.svc.Release(context.Background(), "lbl-0001", "inspected, enclosure intact"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := h.mustDevice("lbl-0001").State; got != domain.StateProvisioned {
		t.Fatalf("state = %s, want provisioned", got)
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)
	h.provision(label, "sec-01", "aisle-01")

	h2 := h.reopen()
	dev := h2.mustDevice("lbl-0001")
	if dev.State != domain.StateProvisioned || dev.Placement.SECID != "sec-01" {
		t.Fatalf("device after restart = %+v", dev)
	}
	if _, err := h2.svc.ManifestRecord(h.tenant, "lbl-0001"); err != nil {
		t.Fatalf("manifest record did not survive the restart: %v", err)
	}
}

// parseChain is the test's own PEM decoder, written independently of the
// application's so that a bug in either is visible rather than cancelling out.
func parseChain(pemBytes []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, errors.New("no certificates in chain")
	}
	return out, nil
}

func TestProvisionedEnvelopeCarriesTheRegistrySuperset(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sec := h.manufacture("sec-01", domain.KindSEC, 10)
	h.ingest(sec)
	h.provision(sec, "", "")

	// A controller's enrolment is announced under the controller's own event
	// name, never the label's. See TestProvisioningEventNameCarriesTheDeviceKind.
	events := h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtSECProvisioned)
	if len(events) != 1 {
		t.Fatalf("published %d provisioning events, want 1", len(events))
	}
	var raw map[string]any
	if err := json.Unmarshal(events[0].Payload, &raw); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	for _, field := range []string{"label_id", "serial", "eui64", "store_id", "hardware_tier", "cert_serial", "provisioned_at"} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("payload is missing the canonical field %q", field)
		}
	}
	if raw["kind"] != "sec" {
		t.Fatalf("kind = %v, want sec", raw["kind"])
	}
}

func TestClockIsUsedForProvisioningTimestamps(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 15, 3, 30, 0, 0, time.UTC)
	h := newHarnessAt(t, t.TempDir(), at)
	label := h.manufacture("lbl-0001", domain.KindLabel, 1)
	h.ingest(label)
	h.provision(label, "sec-01", "aisle-01")

	if got := h.mustDevice("lbl-0001").ProvisionedAt; !got.Equal(at) {
		t.Fatalf("provisioned_at = %s, want %s", got, at)
	}
}

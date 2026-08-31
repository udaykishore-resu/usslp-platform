package app

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/msgbus"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// DeviceProvisioned is the payload of the provisioning event family:
// canon.EvtLabelProvisioned, canon.EvtSECProvisioned and
// canon.EvtSGUProvisioned, one per tier of hardware. [canon.ProvisionedEventFor]
// picks the name from the kind, and the registry never emits a name that
// disagrees with the payload's Kind.
//
// It embeds canon.DeviceProvisioned, so the JSON on the wire is a strict
// superset of the platform-wide contract: a consumer that decodes into
// canon.DeviceProvisioned gets exactly the fields §2 promises — Kind among
// them — and ignores the ones the registry adds. Those exist because zone and
// tenant are needed to address a device without a second lookup, and because a
// re-enrolment is worth being able to count.
type DeviceProvisioned struct {
	canon.DeviceProvisioned
	// TenantID is repeated in the payload as well as the envelope so that a
	// consumer reading a payload out of an archive without its envelope can
	// still attribute it.
	TenantID canon.TenantID `json:"tenant_id"`
	// Zone is the controller subdivision the device announced from.
	Zone string `json:"zone,omitempty"`
	// Reprovisioned marks an enrolment of a device the registry already knew:
	// a label moved between shelf sections, or one that lost its configuration
	// and re-announced. It is not an anomaly, but it is worth being able to
	// count.
	Reprovisioned bool `json:"reprovisioned,omitempty"`
}

// DeviceKind returns the payload's kind in the registry's own vocabulary. It
// exists because canon carries the kind as a plain string — canon cannot import
// the registry's domain — and every reader inside the registry wants the typed
// value.
func (d DeviceProvisioned) DeviceKind() domain.DeviceKind {
	return domain.DeviceKind(d.Kind)
}

// ProvisionRequest is a first-power-on announcement, forwarded by the Shelf
// Edge Controller that heard it on the Zigbee mesh.
//
// The controller is a relay here, not an authority. Everything that decides the
// device's identity comes out of the certificate; everything the controller
// supplies is about placement, which is the part no factory could have known.
type ProvisionRequest struct {
	// CertificateChainPEM is the device's certificate followed by its issuing
	// chain, exactly as the device presented it.
	CertificateChainPEM []byte `json:"certificate_chain_pem"`
	// EUI64 is the radio address the device announced, 16 hex characters. It is
	// checked against the manufacturing manifest: a certificate presented by a
	// radio the factory did not pair it with is the signature of a cloned key.
	EUI64 string `json:"eui64"`
	// SECID is the controller that heard the announcement and will own the
	// device's radio traffic.
	SECID canon.SECID `json:"sec_id"`
	// Zone is the controller's subdivision the device was heard in.
	Zone string `json:"zone,omitempty"`
	// FirmwareVersion is what the device reports running.
	FirmwareVersion string `json:"firmware_version,omitempty"`
	// ReportedBy names the controller that relayed the request, for the audit
	// trail. It is normally equal to SECID and differs only when a neighbouring
	// controller relayed for one that is saturated.
	ReportedBy canon.SECID `json:"reported_by,omitempty"`
}

// DeviceConfig is the configuration payload handed back to a freshly
// provisioned device and pushed, retained, to its config topic.
//
// It is deliberately complete rather than minimal. A label that has just joined
// has no state and no way to ask a follow-up question — its next transmission
// window may be five minutes away — so everything it needs to participate has
// to arrive in one message: where to listen, where to acknowledge, how often to
// speak, and which key ring to verify prices against.
type DeviceConfig struct {
	DeviceID     string             `json:"device_id"`
	Kind         domain.DeviceKind  `json:"kind"`
	TenantID     canon.TenantID     `json:"tenant_id"`
	StoreID      canon.StoreID      `json:"store_id"`
	SECID        canon.SECID        `json:"sec_id"`
	Zone         string             `json:"zone,omitempty"`
	Region       canon.Region       `json:"region,omitempty"`
	HardwareTier string             `json:"hardware_tier"`
	Topics       DeviceConfigTopics `json:"topics"`
	// BeaconSeconds is how often the device must make itself heard. Three missed
	// beacons mark it offline, so this value and the registry's health policy
	// are two ends of one contract and are derived from the same number.
	BeaconSeconds int `json:"beacon_seconds"`
	// TelemetrySeconds is the reporting cadence for battery, temperature and
	// link quality.
	TelemetrySeconds int `json:"telemetry_seconds"`
	// MaxFullRefreshesPerDay bounds E-Ink wear and battery drain. A full
	// waveform costs roughly a hundred times a partial one, and a label whose
	// category reprices hourly would exhaust a seven-year cell in months if it
	// took a full refresh every time.
	MaxFullRefreshesPerDay int `json:"max_full_refreshes_per_day"`
	// Sequence is the configuration's monotonic version for this device. A label
	// discards a configuration whose sequence is not greater than the one it
	// holds, exactly as it does for prices.
	Sequence int64     `json:"sequence"`
	IssuedAt time.Time `json:"issued_at"`
}

// DeviceConfigTopics are the four MQTT topics a device needs. They are computed
// by the registry and shipped to the device so that the topic layout can change
// without a firmware release: a label never constructs a topic itself.
type DeviceConfigTopics struct {
	Price     string `json:"price"`
	Config    string `json:"config"`
	OTA       string `json:"ota"`
	ACK       string `json:"ack"`
	ZonePrice string `json:"zone_price"`
}

// ProvisionResult is what a successful provisioning returns.
type ProvisionResult struct {
	Device *domain.Device `json:"device"`
	Config DeviceConfig   `json:"config"`
	// Reprovisioned reports that the device was already on record.
	Reprovisioned bool `json:"reprovisioned"`
}

// Provisioning refusals. Each is a distinct operational story and a distinct
// HTTP status, so each is a distinct sentinel.
var (
	// ErrUnknownDevice means the certificate verified but no manufacturing
	// record matches it. Either the manifest for this shipment has not been
	// ingested yet — the common, benign case on a store opening day — or
	// something is presenting a certificate for a device that was never built.
	ErrUnknownDevice = errors.New("registry: no manufacturing record for this device")
	// ErrManifestMismatch means the certificate verified and a record exists,
	// but they disagree about the device. This is never benign.
	ErrManifestMismatch = errors.New("registry: device does not match its manufacturing record")
	// ErrCloneDetected means the identity is already provisioned somewhere
	// incompatible with this request.
	ErrCloneDetected = errors.New("registry: device identity is already provisioned elsewhere")
	// ErrRetired means the identity was decommissioned and will not be enrolled
	// again.
	ErrRetired = errors.New("registry: device has been retired")
)

// IngestManifest stores a batch of manufacturing records.
//
// Ingest is idempotent per record and additive per batch: re-uploading a
// shipment's manifest after a partial failure is safe, and a record that is
// already present with identical contents is a no-op. A record that is present
// with *different* contents is refused, because the factory does not get to
// change what it built after the fact — a corrected record is a new serial.
func (s *Service) IngestManifest(ctx context.Context, m *domain.Manifest) (int, error) {
	if m == nil {
		return 0, fmt.Errorf("%w: nil manifest", domain.ErrInvalid)
	}
	if err := m.Validate(); err != nil {
		return 0, err
	}
	now := s.Now()
	if m.IngestedAt.IsZero() {
		m.IngestedAt = now
	}
	if m.ManifestID == "" {
		m.ManifestID = canon.NewULID()
	}

	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	batch := s.kv.NewBatch()
	stored := 0
	for _, rec := range m.Records {
		if rec.ManufacturedAt.IsZero() {
			rec.ManufacturedAt = m.IngestedAt
		}
		if rec.BatchID == "" {
			rec.BatchID = m.BatchID
		}
		key := manifestKey(rec.TenantID, rec.DeviceID)
		existing, err := s.kv.Get(key)
		switch {
		case err == nil:
			var prev domain.ManufacturingRecord
			if err := json.Unmarshal(existing, &prev); err != nil {
				return 0, fmt.Errorf("registry: decode stored manifest record %s: %w", rec.DeviceID, err)
			}
			if !sameManufacturingRecord(prev, rec) {
				return 0, fmt.Errorf("%w: manifest record %s already exists with different contents",
					domain.ErrAlreadyExists, rec.DeviceID)
			}
			continue
		case errors.Is(err, kvstore.ErrNotFound):
		default:
			return 0, fmt.Errorf("registry: read manifest record %s: %w", rec.DeviceID, err)
		}
		body, err := json.Marshal(rec)
		if err != nil {
			return 0, fmt.Errorf("registry: encode manifest record %s: %w", rec.DeviceID, err)
		}
		batch.Put(key, body)
		stored++
	}
	if stored == 0 {
		return 0, nil
	}
	if err := batch.Write(); err != nil {
		return 0, fmt.Errorf("registry: store manifest %s: %w", m.ManifestID, err)
	}
	s.log.Info("manufacturing manifest ingested",
		"manifest_id", m.ManifestID, "tenant_id", string(m.TenantID),
		"batch_id", m.BatchID, "records", len(m.Records), "stored", stored)
	return stored, nil
}

// sameManufacturingRecord compares the immutable half of two records. The
// ingest timestamps are excluded on purpose: a re-uploaded manifest is
// legitimately stamped later, and refusing it for that would make retrying an
// interrupted upload impossible.
func sameManufacturingRecord(a, b domain.ManufacturingRecord) bool {
	return a.Serial == b.Serial && a.EUI64 == b.EUI64 &&
		a.HardwareTier == b.HardwareTier && a.TenantID == b.TenantID &&
		a.StoreID == b.StoreID && a.DeviceID == b.DeviceID && a.Kind == b.Kind &&
		a.CertSerial == b.CertSerial &&
		a.PublicKeyFingerprint() == b.PublicKeyFingerprint()
}

// ManifestRecord returns a stored manufacturing record.
func (s *Service) ManifestRecord(tenant canon.TenantID, deviceID string) (domain.ManufacturingRecord, error) {
	raw, err := s.kv.Get(manifestKey(tenant, deviceID))
	if errors.Is(err, kvstore.ErrNotFound) {
		return domain.ManufacturingRecord{}, fmt.Errorf("%w: manufacturing record %s", domain.ErrNotFound, deviceID)
	}
	if err != nil {
		return domain.ManufacturingRecord{}, err
	}
	var rec domain.ManufacturingRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return domain.ManufacturingRecord{}, fmt.Errorf("registry: decode manifest record %s: %w", deviceID, err)
	}
	return rec, nil
}

// Provision runs zero-touch enrolment end to end.
//
// The order of the checks is the security design, not an implementation
// detail:
//
//  1. the certificate chain is verified against the platform's own hierarchy,
//     including revocation, before a single field of the request is read;
//  2. the identity is extracted from the verified certificate, never from the
//     request body, so a controller cannot enrol a label into a store it does
//     not serve;
//  3. the manufacturing record is looked up and compared — public key, cert
//     serial, radio address, hardware tier — so that a certificate which
//     verifies but was never issued for a device that was actually built is
//     refused;
//  4. only then is the device's existing registry state consulted, for the
//     anti-cloning check.
//
// Anything that fails at (3) or (4) quarantines the identity. That is the
// deliberate choice: when two things present the same identity, the platform
// cannot tell which is the genuine label, and continuing to trust either is
// worse than taking both out of service until someone walks the aisle.
func (s *Service) Provision(ctx context.Context, req ProvisionRequest) (*ProvisionResult, error) {
	now := s.Now()

	chain, err := parseCertificateChain(req.CertificateChainPEM)
	if err != nil {
		s.countRejection("malformed-certificate")
		return nil, err
	}
	identity, leaf, err := s.cfg.Auth.Authenticate(chain, now)
	if err != nil {
		s.countRejection("certificate-rejected")
		return nil, fmt.Errorf("registry: provisioning certificate rejected: %w", err)
	}
	if !identity.Kind.IsDevice() {
		s.countRejection("not-a-device")
		return nil, fmt.Errorf("%w: certificate identity %q is not a device", domain.ErrInvalid, identity.Kind)
	}

	kind := deviceKindFor(identity.Kind)
	eui := strings.ToUpper(strings.TrimSpace(req.EUI64))
	secID := req.SECID
	if kind != domain.KindLabel {
		// A controller or gateway is its own radio authority; it is not parented
		// to another controller and must not claim to be.
		secID = ""
	}

	record, err := s.ManifestRecord(identity.TenantID, identity.DeviceID)
	if err != nil {
		s.countRejection("unknown-device")
		return nil, fmt.Errorf("%w: %s", ErrUnknownDevice, identity.DeviceID)
	}
	if detail := manifestMismatch(record, identity, leaf, eui, kind); detail != "" {
		s.countRejection("manifest-mismatch")
		subj := quarantineSubject{
			deviceID: identity.DeviceID, kind: kind, tenant: identity.TenantID,
			store: identity.StoreID, serial: record.Serial, eui: eui,
		}
		if qerr := s.quarantine(ctx, subj, domain.ReasonManifestMismatch, detail, identity.StoreID, now); qerr != nil {
			s.log.Error("registry could not quarantine a manifest mismatch",
				"device_id", identity.DeviceID, "error", qerr)
		}
		s.log.Warn("provisioning refused: manifest mismatch",
			"device_id", identity.DeviceID, "store_id", string(identity.StoreID), "detail", detail)
		return nil, fmt.Errorf("%w: %s", ErrManifestMismatch, detail)
	}

	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	existing := s.device(identity.DeviceID)
	reprovision := existing != nil
	if existing != nil {
		switch existing.State {
		case domain.StateRetired:
			s.countRejection("retired")
			return nil, fmt.Errorf("%w: %s", ErrRetired, identity.DeviceID)
		case domain.StateQuarantined:
			s.countRejection("quarantined")
			return nil, fmt.Errorf("%w: %s", domain.ErrQuarantined, identity.DeviceID)
		}
	}
	if detail := s.cloneEvidence(existing, identity, eui); detail != "" {
		s.countRejection("clone-detected")
		subj := quarantineSubject{
			deviceID: identity.DeviceID, kind: kind, tenant: identity.TenantID,
			store: identity.StoreID, serial: record.Serial, eui: eui,
		}
		if qerr := s.quarantineLocked(ctx, subj, domain.ReasonDuplicateIdentity,
			detail, identity.StoreID, now); qerr != nil {
			s.log.Error("registry could not quarantine a duplicate identity",
				"device_id", identity.DeviceID, "error", qerr)
		}
		s.log.Error("provisioning refused: duplicate device identity",
			"device_id", identity.DeviceID, "serial", record.Serial,
			"claimed_store_id", string(identity.StoreID), "detail", detail)
		return nil, fmt.Errorf("%w: %s", ErrCloneDetected, detail)
	}

	firmware := req.FirmwareVersion
	if firmware == "" {
		firmware = record.FirmwareVersion
	}
	payload := DeviceProvisioned{
		DeviceProvisioned: canon.DeviceProvisioned{
			LabelID:       canon.LabelID(identity.DeviceID),
			Kind:          string(kind),
			Serial:        record.Serial,
			EUI64:         eui,
			StoreID:       identity.StoreID,
			SECID:         secID,
			HardwareTier:  record.HardwareTier,
			FirmwareVer:   firmware,
			CertSerial:    identity.SerialNumber,
			CertNotAfter:  identity.NotAfter,
			ProvisionedAt: now,
		},
		TenantID:      identity.TenantID,
		Zone:          req.Zone,
		Reprovisioned: reprovision,
	}

	// The identifiers below become MQTT topic segments and Kafka keys, and they
	// arrived from a manifest file and a controller's announcement. Validating
	// the assembled device before it is committed is the last point at which a
	// value that would break out of the tenant's namespace can be refused.
	candidate := &domain.Device{
		ID: identity.DeviceID, Kind: kind, TenantID: identity.TenantID,
		Placement: domain.Placement{StoreID: identity.StoreID, SECID: secID, Zone: req.Zone},
		Serial:    record.Serial, EUI64: eui, HardwareTier: record.HardwareTier,
	}
	if err := candidate.Validate(); err != nil {
		s.countRejection("invalid-device")
		return nil, err
	}

	// The event name carries the tier. A Shelf Edge Controller joining the fleet
	// is a genuine fact that the OTA service and monitoring both want, so it is
	// published like any other; it is simply not `device.label.provisioned`,
	// because it is not a label. Naming it separately is what lets the Label
	// Service's directory — which is a directory of labels — skip it on the
	// `usslp-event-type` header rather than decode a payload it will then have
	// to reject. Interface contract §2 keeps all three on `device-events`.
	env, err := s.newEvent(canon.ProvisionedEventFor(string(kind)), domain.AggregateDevice, identity.DeviceID,
		identity.TenantID, identity.StoreID, payload)
	if err != nil {
		return nil, err
	}
	// The idempotency key makes a retried announcement — and the mesh retries,
	// because a label that does not hear its configuration announces again —
	// a no-op rather than a second provisioning event.
	env.IdempotencyKey = fmt.Sprintf("provision:%s:%s:%s", identity.SerialNumber, secID, eui)

	expected := int64(eventstore.ExpectedNoStream)
	previousSEC := canon.SECID("")
	if existing != nil {
		expected = existing.Version
		previousSEC = existing.Placement.SECID
	}
	if err := s.commit(ctx, deviceStream(identity.DeviceID), expected, env); err != nil {
		return nil, err
	}

	// A device that moved between controllers leaves retained configuration on
	// the old zone topic. Clearing it here is interface contract §3: the
	// registry is the only component that knows the move happened.
	if previousSEC != "" && previousSEC != secID {
		scope := s.scopeFor(identity.TenantID, identity.StoreID)
		s.pushRetained(ctx, scope.SECLabelTopic(previousSEC, canon.LabelID(identity.DeviceID), canon.LeafConfig), nil, canon.QoSConfig)
		s.pushRetained(ctx, scope.SECLabelTopic(previousSEC, canon.LabelID(identity.DeviceID), canon.LeafPrice), nil, canon.QoSPrice)
	}

	dev := s.Device(identity.DeviceID)
	if dev == nil {
		return nil, fmt.Errorf("registry: device %s vanished after provisioning", identity.DeviceID)
	}
	cfg := s.buildConfig(dev)
	s.pushConfig(ctx, dev, cfg)

	// A label whose position the store's planogram already declares is bound
	// immediately, so that a replacement label clipped onto a rail is showing
	// the right price before the technician has walked to the end of the aisle.
	if kind == domain.KindLabel {
		if err := s.bindFromPlanogramLocked(ctx, dev, now); err != nil {
			s.log.Warn("registry could not apply the stored planogram to a new label",
				"device_id", dev.ID, "error", err)
		}
	}

	if s.met != nil {
		s.met.provisioned.With(string(kind)).Inc()
	}
	s.log.Info("device provisioned",
		"device_id", dev.ID, "kind", string(kind), "tenant_id", string(dev.TenantID),
		"store_id", string(dev.Placement.StoreID), "sec_id", string(dev.Placement.SECID),
		"serial", dev.Serial, "cert_serial", dev.CertSerial, "reprovisioned", reprovision)

	return &ProvisionResult{
		Device:        s.Device(dev.ID),
		Config:        cfg,
		Reprovisioned: reprovision,
	}, nil
}

// manifestMismatch compares a verified certificate against the manufacturing
// record and returns a description of the first disagreement, or "".
//
// The public-key comparison is the strongest of these checks and the reason the
// manifest records a key at all. A certificate that verifies proves only that
// some authority signed it; comparing the key proves it is the certificate that
// was issued to the unit that came off the line. An attacker who obtained the
// ability to mint certificates still cannot pass this, because they do not have
// the private key sealed in that device's secure element.
func manifestMismatch(rec domain.ManufacturingRecord, id pki.Identity, leaf *x509.Certificate, eui string, kind domain.DeviceKind) string {
	if rec.Kind != kind {
		return fmt.Sprintf("manifest records a %s, certificate identifies a %s", rec.Kind, kind)
	}
	if rec.TenantID != id.TenantID {
		return fmt.Sprintf("manifest tenant %s, certificate tenant %s", rec.TenantID, id.TenantID)
	}
	if rec.StoreID != id.StoreID {
		return fmt.Sprintf("manifest store %s, certificate store %s", rec.StoreID, id.StoreID)
	}
	if rec.CertSerial != id.SerialNumber {
		return fmt.Sprintf("manifest certificate serial %s, presented %s", rec.CertSerial, id.SerialNumber)
	}
	if eui != "" && rec.EUI64 != eui {
		return fmt.Sprintf("manifest radio address %s, announced %s", rec.EUI64, eui)
	}
	if leaf != nil && len(rec.PublicKeySPKI) > 0 {
		spki, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
		if err != nil {
			return "presented certificate carries an unmarshallable public key"
		}
		if !bytesEqual(spki, rec.PublicKeySPKI) {
			return fmt.Sprintf("manifest public key %s does not match the presented certificate",
				rec.PublicKeyFingerprint())
		}
	}
	return ""
}

// cloneEvidence reports why a provisioning request looks like a cloned or
// stolen device, or "" when it does not.
//
// Three signals, each of which can only be produced by two physical things
// sharing one identity:
//
//   - the same device identity announcing from a different store. Store is
//     bound into the certificate, so this can only happen if the certificate
//     and key were copied onto hardware that was shipped somewhere else;
//   - the same identity announcing from a different radio. The EUI-64 is burned
//     into the transceiver and is not something firmware can choose;
//   - a radio address already claimed by a different device identity, which is
//     the same conflict seen from the other side.
//
// Moving between controllers *within* a store is explicitly not evidence: that
// is a shelf reset, and it happens every week.
func (s *Service) cloneEvidence(existing *domain.Device, id pki.Identity, eui string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if owner, ok := s.byEUI[eui]; ok && eui != "" && owner != id.DeviceID {
		return fmt.Sprintf("radio address %s is already registered to device %s", eui, owner)
	}
	if existing == nil {
		return ""
	}
	if existing.Placement.StoreID != "" && existing.Placement.StoreID != id.StoreID {
		return fmt.Sprintf("device is provisioned in store %s and is announcing from store %s",
			existing.Placement.StoreID, id.StoreID)
	}
	if existing.EUI64 != "" && eui != "" && existing.EUI64 != eui {
		return fmt.Sprintf("device is registered with radio address %s and is announcing as %s",
			existing.EUI64, eui)
	}
	return ""
}

// device returns the live device pointer under the read lock. Callers holding
// cmdMu use it to decide; everything outside this package gets a clone.
func (s *Service) device(id string) *domain.Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.devices[id]
}

// quarantineSubject is what the registry knows about the identity it is taking
// out of service. It is passed explicitly rather than looked up because the
// commonest quarantine happens to a device that has no registry entry yet.
type quarantineSubject struct {
	deviceID string
	kind     domain.DeviceKind
	tenant   canon.TenantID
	store    canon.StoreID
	serial   string
	eui      string
}

// quarantine takes cmdMu and stops trusting a device's identity.
func (s *Service) quarantine(ctx context.Context, subj quarantineSubject, reason domain.QuarantineReason, detail string, observedStore canon.StoreID, now time.Time) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	return s.quarantineLocked(ctx, subj, reason, detail, observedStore, now)
}

// quarantineLocked quarantines a device with cmdMu already held.
//
// It emits two events: the lifecycle transition, so the device timeline is
// complete, and the security fact, so a SIEM rule can trigger on an event type
// rather than on a payload field. A device that is not on record at all — a
// certificate that verified for a device the registry has never seen — produces
// only the security event, because there is no aggregate to transition.
func (s *Service) quarantineLocked(ctx context.Context, subj quarantineSubject, reason domain.QuarantineReason, detail string, observedStore canon.StoreID, now time.Time) error {
	deviceID := subj.deviceID
	dev := s.device(deviceID)
	sec := domain.DeviceQuarantined{
		DeviceID:        deviceID,
		Kind:            subj.kind,
		Serial:          subj.serial,
		EUI64:           subj.eui,
		TenantID:        subj.tenant,
		StoreID:         subj.store,
		Reason:          reason,
		Detail:          detail,
		ObservedStoreID: observedStore,
		At:              now,
	}
	if dev != nil {
		if sec.Serial == "" {
			sec.Serial = dev.Serial
		}
		if sec.EUI64 == "" {
			sec.EUI64 = dev.EUI64
		}
		if sec.Kind == "" {
			sec.Kind = dev.Kind
		}
		if dev.TenantID != "" {
			sec.TenantID = dev.TenantID
		}
		if dev.Placement.StoreID != "" {
			sec.StoreID = dev.Placement.StoreID
		}
	}
	tenant, store := sec.TenantID, sec.StoreID
	if tenant == "" {
		// An identity the registry cannot attribute still needs an auditable
		// event, so the security stream carries the platform tenant and the
		// payload names the device.
		tenant = canon.TenantID("platform")
		sec.TenantID = tenant
	}

	var envs []canon.Envelope
	expected := int64(eventstore.ExpectedAny)
	if dev != nil && dev.State != domain.StateQuarantined {
		change, err := deviceTransition(dev, domain.StateQuarantined, string(reason), now)
		if err != nil {
			return err
		}
		env, err := s.newEvent(domain.EvtDeviceStateChanged, domain.AggregateDevice, deviceID, tenant, store, change)
		if err != nil {
			return err
		}
		envs = append(envs, env)
		expected = dev.Version
	}
	env, err := s.newEvent(domain.EvtDeviceQuarantined, domain.AggregateDevice, deviceID, tenant, store, sec)
	if err != nil {
		return err
	}
	envs = append(envs, env)
	if err := s.commit(ctx, deviceStream(deviceID), expected, envs...); err != nil {
		return err
	}
	if s.met != nil {
		s.met.transitions.With(string(domain.StateQuarantined)).Inc()
	}
	return nil
}

// Quarantine takes a device out of service by operator decision.
func (s *Service) Quarantine(ctx context.Context, deviceID, detail string) error {
	dev := s.Device(deviceID)
	if dev == nil {
		return fmt.Errorf("%w: device %s", domain.ErrNotFound, deviceID)
	}
	subj := quarantineSubject{
		deviceID: dev.ID, kind: dev.Kind, tenant: dev.TenantID,
		store: dev.Placement.StoreID, serial: dev.Serial, eui: dev.EUI64,
	}
	return s.quarantine(ctx, subj, domain.ReasonOperator, detail, "", s.Now())
}

// Release returns a quarantined device to the provisioned state, which is where
// its operational life restarts.
//
// It is a deliberate one-way door back to the beginning rather than a
// restoration of whatever the device was doing: an identity that was under
// suspicion has to re-earn its placement through provisioning, and a release
// that silently reinstated an assignment would put a possibly-cloned label back
// in front of a customer with no further check.
func (s *Service) Release(ctx context.Context, deviceID, reason string) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	dev := s.device(deviceID)
	if dev == nil {
		return fmt.Errorf("%w: device %s", domain.ErrNotFound, deviceID)
	}
	if dev.State != domain.StateQuarantined {
		return fmt.Errorf("%w: device %s is %s, not quarantined",
			domain.ErrIllegalTransition, deviceID, dev.State)
	}
	change, err := deviceTransition(dev, domain.StateProvisioned, reason, s.Now())
	if err != nil {
		return err
	}
	env, err := s.newEvent(domain.EvtDeviceStateChanged, domain.AggregateDevice, deviceID,
		dev.TenantID, dev.Placement.StoreID, change)
	if err != nil {
		return err
	}
	return s.commit(ctx, deviceStream(deviceID), dev.Version, env)
}

// Retire decommissions a device permanently.
//
// It clears the retained MQTT state for the device before recording the
// retirement, because a retired label that a broker keeps replaying a price to
// is the one failure mode a decommissioning is supposed to prevent: the unit is
// in a bin, the shelf is empty, and the retained message would still be there
// for whatever controller subscribes to that zone next.
func (s *Service) Retire(ctx context.Context, deviceID, reason string) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	dev := s.device(deviceID)
	if dev == nil {
		return fmt.Errorf("%w: device %s", domain.ErrNotFound, deviceID)
	}
	if dev.State == domain.StateRetired {
		return nil
	}
	now := s.Now()
	var envs []canon.Envelope

	if dev.Kind == domain.KindLabel && dev.Assignment != nil {
		unassign, err := s.unassignEvent(dev, "retired", now)
		if err != nil {
			return err
		}
		envs = append(envs, unassign)
	}
	change, err := deviceTransition(dev, domain.StateRetired, reason, now)
	if err != nil {
		return err
	}
	envChange, err := s.newEvent(domain.EvtDeviceStateChanged, domain.AggregateDevice, deviceID,
		dev.TenantID, dev.Placement.StoreID, change)
	if err != nil {
		return err
	}
	envs = append(envs, envChange)

	envRetire, err := s.newEvent(domain.EvtDeviceRetired, domain.AggregateDevice, deviceID,
		dev.TenantID, dev.Placement.StoreID, domain.DeviceRetired{
			DeviceID: deviceID, Kind: dev.Kind, TenantID: dev.TenantID,
			StoreID: dev.Placement.StoreID, Serial: dev.Serial, Reason: reason, At: now,
		})
	if err != nil {
		return err
	}
	envs = append(envs, envRetire)

	if dev.Placement.SECID != "" && dev.Kind == domain.KindLabel {
		scope := s.scopeFor(dev.TenantID, dev.Placement.StoreID)
		label := dev.LabelID()
		s.pushRetained(ctx, scope.SECLabelTopic(dev.Placement.SECID, label, canon.LeafConfig), nil, canon.QoSConfig)
		s.pushRetained(ctx, scope.SECLabelTopic(dev.Placement.SECID, label, canon.LeafPrice), nil, canon.QoSPrice)
	}
	if err := s.commit(ctx, deviceStream(deviceID), dev.Version, envs...); err != nil {
		return err
	}
	if s.met != nil {
		s.met.transitions.With(string(domain.StateRetired)).Inc()
	}
	s.log.Info("device retired", "device_id", deviceID, "reason", reason)
	return nil
}

// deviceTransition applies a transition to a copy of the device so that the
// decision does not mutate the read model. The read model is only ever changed
// by applyLocked, from an event that is already durable.
func deviceTransition(d *domain.Device, to domain.DeviceState, reason string, at time.Time) (domain.DeviceStateChanged, error) {
	clone := d.Clone()
	return clone.Transition(to, reason, at)
}

// buildConfig assembles the configuration payload for a device.
func (s *Service) buildConfig(d *domain.Device) DeviceConfig {
	scope := s.scopeFor(d.TenantID, d.Placement.StoreID)
	label := d.LabelID()
	sec := d.Placement.SECID
	if d.Kind != domain.KindLabel {
		sec = canon.SECID(d.ID)
	}
	beacon := int(s.policy.BeaconInterval / time.Second)
	return DeviceConfig{
		DeviceID:     d.ID,
		Kind:         d.Kind,
		TenantID:     d.TenantID,
		StoreID:      d.Placement.StoreID,
		SECID:        d.Placement.SECID,
		Zone:         d.Placement.Zone,
		Region:       s.cfg.Region,
		HardwareTier: d.HardwareTier,
		Topics: DeviceConfigTopics{
			Price:     scope.SECLabelTopic(sec, label, canon.LeafPrice),
			Config:    scope.SECLabelTopic(sec, label, canon.LeafConfig),
			OTA:       scope.SECLabelTopic(sec, label, canon.LeafOTA),
			ACK:       scope.SECLabelTopic(sec, label, canon.LeafACK),
			ZonePrice: scope.ZoneTopic(sec, canon.LeafZonePrice),
		},
		BeaconSeconds: beacon,
		// Telemetry at ten beacons keeps the label's radio duty cycle low: a
		// beacon is a few bytes, a telemetry report is a struct, and the
		// blueprint's five-minute cadence at a 30-second beacon is exactly this
		// ratio.
		TelemetrySeconds:       beacon * 10,
		MaxFullRefreshesPerDay: 24,
		Sequence:               d.Version,
		IssuedAt:               s.Now(),
	}
}

// pushConfig publishes a device's configuration, retained, so that a controller
// rebooting after a power cut recovers it from the local broker without a round
// trip to a cloud that may be unreachable.
func (s *Service) pushConfig(ctx context.Context, d *domain.Device, cfg DeviceConfig) {
	if s.cfg.Messenger == nil {
		return
	}
	// This envelope goes to the device's own MQTT config topic, never to
	// `device-events`, and its payload is a DeviceConfig rather than a
	// DeviceProvisioned. It still takes the tier's provisioning name so that no
	// message the registry produces describes a controller as a label; a reader
	// keys off the topic, which is what interface contract §3 defines.
	env, err := s.newEvent(canon.ProvisionedEventFor(string(d.Kind)), domain.AggregateDevice, d.ID,
		d.TenantID, d.Placement.StoreID, cfg)
	if err != nil {
		s.log.Warn("registry could not build a configuration envelope", "device_id", d.ID, "error", err)
		return
	}
	body, err := json.Marshal(env)
	if err != nil {
		s.log.Warn("registry could not encode a configuration envelope", "device_id", d.ID, "error", err)
		return
	}
	s.pushRetained(ctx, cfg.Topics.Config, body, msgbus.QoS(canon.QoSConfig))
}

func (s *Service) countRejection(reason string) {
	if s.met != nil {
		s.met.rejected.With(reason).Inc()
	}
}

// deviceKindFor maps a PKI identity kind onto a registry device kind. The two
// enumerations are separate because the PKI's set includes identities that are
// not hardware at all, and collapsing them would let a service certificate
// enrol as a device.
func deviceKindFor(k pki.IdentityKind) domain.DeviceKind {
	switch k {
	case pki.KindLabel:
		return domain.KindLabel
	case pki.KindSEC:
		return domain.KindSEC
	default:
		return domain.KindSGU
	}
}

// parseCertificateChain decodes a PEM bundle into leaf-first certificate order.
func parseCertificateChain(pemBytes []byte) ([]*x509.Certificate, error) {
	if len(pemBytes) == 0 {
		return nil, fmt.Errorf("%w: no certificate presented", domain.ErrInvalid)
	}
	var out []*x509.Certificate
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: certificate chain: %v", domain.ErrInvalid, err)
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: certificate chain contains no certificates", domain.ErrInvalid)
	}
	return out, nil
}

// bytesEqual is a plain comparison. It is not constant time on purpose: both
// operands here are public keys, and pretending otherwise would suggest the
// comparison protects a secret when it does not.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

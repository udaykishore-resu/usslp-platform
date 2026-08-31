package app

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// ErrSeedingDisabled means the service was built without a certificate issuer,
// which is the correct configuration for production: enrolment authenticates
// hardware a factory already flashed, and a registry that can also mint
// identities is a registry whose HTTP surface can manufacture devices.
var ErrSeedingDisabled = errors.New("registry: seeding is disabled; no device issuer is configured")

// SeedRequest describes a synthetic store to stand up.
type SeedRequest struct {
	TenantID canon.TenantID `json:"tenant_id"`
	StoreID  canon.StoreID  `json:"store_id"`
	// SECs is the number of Shelf Edge Controllers. A real store runs about 25.
	SECs int `json:"secs"`
	// LabelsPerSEC is how many labels each controller owns. A real controller
	// covers an 8 m shelf section, which is 200 to 1,600 labels depending on the
	// category; the default here is small enough to seed in well under a second.
	LabelsPerSEC int `json:"labels_per_sec"`
	// Seed fixes the pseudo-random stream. The same seed produces the same
	// store, which is what lets the end-to-end demo assert on specific labels.
	Seed int64 `json:"seed"`
	// HardwareTier and FirmwareVersion are stamped on every generated device, so
	// that an OTA job created straight after seeding has a cohort to target.
	HardwareTier    string `json:"hardware_tier,omitempty"`
	FirmwareVersion string `json:"firmware_version,omitempty"`
	// WithTelemetry generates one round of telemetry and one mesh report per
	// controller, so the health, mesh and battery-runway views have something to
	// show immediately rather than after the first real reporting interval.
	WithTelemetry bool `json:"with_telemetry"`
}

// SeedResult reports what was created.
type SeedResult struct {
	TenantID          canon.TenantID `json:"tenant_id"`
	StoreID           canon.StoreID  `json:"store_id"`
	SECs              []canon.SECID  `json:"secs"`
	Labels            int            `json:"labels"`
	Positions         int            `json:"positions"`
	PlanogramRevision int64          `json:"planogram_revision"`
	Assigned          int            `json:"assigned"`
	ElapsedMS         int64          `json:"elapsed_ms"`
}

// Seed provisions a complete synthetic store: controllers, labels, a generated
// planogram, and optionally one round of telemetry and mesh reports.
//
// # Why it goes through the real provisioning path
//
// Every device it creates is issued a genuine certificate from the platform's
// own Manufacturing Sub-CA, written into a manufacturing manifest, and then
// enrolled through [Service.Provision] — the same function a label on a shelf
// goes through, with the same chain verification, the same manifest comparison
// and the same anti-cloning check. A faster seeder that wrote registry entries
// directly would produce a demo that proves nothing: the interesting property
// of this platform is that a device cannot join without proving who it is, and
// a seeded store that skipped that would be a store where the property is
// untested.
//
// # Determinism
//
// Given the same seed, every identifier, radio address, SKU, shelf coordinate,
// battery level and mesh parent is identical run to run. The one thing that is
// not reproducible is certificate serial numbers, which carry 128 bits of
// entropy by deliberate design — an unpredictable serial is what denies an
// attacker chosen-prefix control — so a seeded store's certificates differ
// between runs even though the store does not.
func (s *Service) Seed(ctx context.Context, req SeedRequest) (*SeedResult, error) {
	if s.cfg.Issuer == nil {
		return nil, ErrSeedingDisabled
	}
	if !canon.ValidID(string(req.TenantID)) {
		return nil, fmt.Errorf("%w: seed tenant id %q", domain.ErrInvalid, req.TenantID)
	}
	if !canon.ValidID(string(req.StoreID)) {
		return nil, fmt.Errorf("%w: seed store id %q", domain.ErrInvalid, req.StoreID)
	}
	if req.SECs <= 0 {
		req.SECs = 4
	}
	if req.LabelsPerSEC <= 0 {
		req.LabelsPerSEC = 25
	}
	if req.SECs > 64 || req.LabelsPerSEC > 512 {
		return nil, fmt.Errorf("%w: seed would create %d devices; the endpoint is capped at 64 controllers and 512 labels each",
			domain.ErrInvalid, req.SECs*req.LabelsPerSEC)
	}
	if req.HardwareTier == "" {
		req.HardwareTier = "esl-2.9-bw"
	}
	if req.FirmwareVersion == "" {
		req.FirmwareVersion = "1.0.0"
	}
	started := time.Now()
	now := s.Now()
	rng := rand.New(rand.NewSource(req.Seed))

	result := &SeedResult{TenantID: req.TenantID, StoreID: req.StoreID}

	// Radio addresses are drawn from a deterministic stream but must still be
	// unique fleet-wide, so the store identifier is mixed into the high bytes.
	euiBase := euiPrefix(req.StoreID)
	nextEUI := func(n int) string {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], euiBase|uint64(n))
		return strings.ToUpper(hex.EncodeToString(b[:]))
	}

	type plan struct {
		id   string
		kind domain.DeviceKind
		eui  string
		sec  canon.SECID
		zone string
	}
	var plans []plan
	secIDs := make([]canon.SECID, 0, req.SECs)
	index := 0
	for i := 0; i < req.SECs; i++ {
		secID := canon.SECID(fmt.Sprintf("sec-%s-%02d", req.StoreID, i+1))
		secIDs = append(secIDs, secID)
		index++
		plans = append(plans, plan{id: string(secID), kind: domain.KindSEC, eui: nextEUI(index)})
		for j := 0; j < req.LabelsPerSEC; j++ {
			index++
			plans = append(plans, plan{
				id:   fmt.Sprintf("lbl-%s-%02d-%03d", req.StoreID, i+1, j+1),
				kind: domain.KindLabel,
				eui:  nextEUI(index),
				sec:  secID,
				zone: fmt.Sprintf("aisle-%02d", i+1),
			})
		}
	}
	result.SECs = secIDs

	// Issue certificates and build the manifest in one pass, so the manifest
	// records the key that was actually certified rather than one the seeder
	// invented.
	manifest := &domain.Manifest{
		TenantID:   req.TenantID,
		BatchID:    fmt.Sprintf("seed-%s-%d", req.StoreID, req.Seed),
		IngestedAt: now,
		Source:     "dev-seed",
	}
	chains := make(map[string][]byte, len(plans))
	for _, p := range plans {
		id := pki.Identity{
			Kind:     pkiKindFor(p.kind),
			TenantID: req.TenantID,
			StoreID:  req.StoreID,
			DeviceID: p.id,
		}
		issued, err := s.cfg.Issuer.IssueDevice(id, now)
		if err != nil {
			return nil, fmt.Errorf("registry: seed: issue certificate for %s: %w", p.id, err)
		}
		spki, err := x509.MarshalPKIXPublicKey(issued.Certificate.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("registry: seed: marshal public key for %s: %w", p.id, err)
		}
		manifest.Records = append(manifest.Records, domain.ManufacturingRecord{
			Serial:          canon.DeviceSerial(euiValue(p.eui)),
			EUI64:           p.eui,
			HardwareTier:    req.HardwareTier,
			FirmwareVersion: req.FirmwareVersion,
			TenantID:        req.TenantID,
			StoreID:         req.StoreID,
			DeviceID:        p.id,
			Kind:            p.kind,
			CertSerial:      issued.Identity.SerialNumber,
			PublicKeySPKI:   spki,
			BatchID:         manifest.BatchID,
			ManufacturedAt:  now,
		})
		chains[p.id] = issued.ChainPEM()
	}
	if _, err := s.IngestManifest(ctx, manifest); err != nil {
		return nil, fmt.Errorf("registry: seed: ingest manifest: %w", err)
	}

	for _, p := range plans {
		if _, err := s.Provision(ctx, ProvisionRequest{
			CertificateChainPEM: chains[p.id],
			EUI64:               p.eui,
			SECID:               p.sec,
			Zone:                p.zone,
			FirmwareVersion:     req.FirmwareVersion,
			ReportedBy:          p.sec,
		}); err != nil {
			return nil, fmt.Errorf("registry: seed: provision %s: %w", p.id, err)
		}
		if p.kind == domain.KindLabel {
			result.Labels++
		}
	}

	// The planogram walks the store in aisle order: one shelf per controller,
	// ten slots per rail, so that a generated store looks like a shop rather
	// than like a list.
	pg := &domain.Planogram{
		TenantID: req.TenantID,
		StoreID:  req.StoreID,
		Source:   "dev-seed",
	}
	templates := []string{"standard", "standard", "standard", "promo", "unit_price"}
	for i := 0; i < req.SECs; i++ {
		for j := 0; j < req.LabelsPerSEC; j++ {
			pg.Positions = append(pg.Positions, domain.Position{
				PositionKey: domain.PositionKey{
					Shelf:    fmt.Sprintf("shelf-%02d", i+1),
					Rail:     fmt.Sprintf("rail-%d", j/10+1),
					Position: j%10 + 1,
				},
				LabelID:  canon.LabelID(fmt.Sprintf("lbl-%s-%02d-%03d", req.StoreID, i+1, j+1)),
				SKU:      canon.SKU(fmt.Sprintf("SKU-%06d", 100000+i*1000+j)),
				Facings:  1 + rng.Intn(3),
				Template: templates[rng.Intn(len(templates))],
				SECID:    secIDs[i],
				Zone:     fmt.Sprintf("aisle-%02d", i+1),
			})
		}
	}
	applied, err := s.UploadPlanogram(ctx, pg)
	if err != nil {
		return nil, fmt.Errorf("registry: seed: upload planogram: %w", err)
	}
	result.PlanogramRevision = applied.Revision
	result.Assigned = applied.Assigned
	result.Positions = len(pg.Positions)

	if req.WithTelemetry {
		if err := s.seedTelemetry(ctx, req, secIDs, rng, now); err != nil {
			return nil, err
		}
	}

	result.ElapsedMS = time.Since(started).Milliseconds()
	s.log.Info("synthetic store seeded",
		"tenant_id", string(req.TenantID), "store_id", string(req.StoreID),
		"secs", len(secIDs), "labels", result.Labels, "seed", req.Seed,
		"elapsed_ms", result.ElapsedMS)
	return result, nil
}

// seedTelemetry generates one reporting round so that the health, mesh and
// battery views are populated the moment the seed returns.
//
// The mesh it builds is the shape a real Zigbee network settles into: a handful
// of router-capable labels parented directly to the controller, and the rest
// hanging off them at depth two. Battery levels are drawn from a deterministic
// distribution with a long tail, because a real store's replacement schedule is
// driven by the few labels that are nearly flat, not by the median.
func (s *Service) seedTelemetry(ctx context.Context, req SeedRequest, secIDs []canon.SECID, rng *rand.Rand, now time.Time) error {
	for i, sec := range secIDs {
		routers := make([]canon.LabelID, 0, 4)
		nodes := make([]canon.MeshNode, 0, req.LabelsPerSEC)
		readings := make([]canon.Telemetry, 0, req.LabelsPerSEC)
		for j := 0; j < req.LabelsPerSEC; j++ {
			label := canon.LabelID(fmt.Sprintf("lbl-%s-%02d-%03d", req.StoreID, i+1, j+1))
			var parent canon.LabelID
			depth := 1
			isRouter := j < 4
			if isRouter {
				routers = append(routers, label)
			} else if len(routers) > 0 {
				parent = routers[j%len(routers)]
				depth = 2
			}
			lqi := 120 + rng.Intn(120)
			rssi := -40 - rng.Intn(45)
			nodes = append(nodes, canon.MeshNode{
				LabelID: label, ParentID: parent, Depth: depth,
				LQI: lqi, RSSI: rssi, Router: isRouter, Online: true,
			})

			// A long-tailed battery distribution: most cells near full, a few
			// near the end of their life.
			pct := 100 - rng.Intn(15)
			if rng.Intn(20) == 0 {
				pct = 5 + rng.Intn(12)
			}
			readings = append(readings, canon.Telemetry{
				LabelID:       label,
				StoreID:       req.StoreID,
				SECID:         sec,
				ReportedAt:    now,
				BatteryMV:     2200 + pct*8,
				BatteryPct:    pct,
				TemperatureC:  4 + float64(rng.Intn(180))/10,
				RSSI:          rssi,
				LQI:           lqi,
				MeshHops:      depth,
				ParentID:      parent,
				FirmwareVer:   req.FirmwareVersion,
				RefreshCount:  int64(rng.Intn(4000)),
				UptimeSeconds: int64(3600 * (1 + rng.Intn(720))),
			})
		}
		if err := s.IngestMeshReport(ctx, canon.MeshTopology{
			SECID: sec, StoreID: req.StoreID, Nodes: nodes, UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("registry: seed: mesh report for %s: %w", sec, err)
		}
		if err := s.IngestTelemetry(ctx, readings); err != nil {
			return fmt.Errorf("registry: seed: telemetry for %s: %w", sec, err)
		}
		s.RecordHeartbeat(sec, now)
	}
	return nil
}

// euiPrefix derives the high 32 bits of a seeded store's radio addresses from
// its identifier, so that two seeded stores in one registry cannot collide on a
// radio address and trip the anti-cloning check.
func euiPrefix(store canon.StoreID) uint64 {
	var h uint64 = 14695981039346656037 // FNV-1a 64-bit offset basis
	for i := 0; i < len(store); i++ {
		h ^= uint64(store[i])
		h *= 1099511628211
	}
	return (h << 32) & 0xFFFFFFFF00000000
}

// euiValue parses a 16-character hex radio address back into the integer form
// canon.DeviceSerial expects.
func euiValue(eui string) uint64 {
	b, err := hex.DecodeString(eui)
	if err != nil || len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// pkiKindFor maps a registry device kind onto the PKI identity kind that names
// the same hardware.
func pkiKindFor(k domain.DeviceKind) pki.IdentityKind {
	switch k {
	case domain.KindLabel:
		return pki.KindLabel
	case domain.KindSEC:
		return pki.KindSEC
	default:
		return pki.KindSGU
	}
}

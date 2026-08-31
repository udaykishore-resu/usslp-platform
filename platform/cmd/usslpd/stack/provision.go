package stack

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

	regadapters "github.com/usslp/usslp/platform/internal/registry/adapters"
	regapp "github.com/usslp/usslp/platform/internal/registry/app"
	regdomain "github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// InitialFirmware is the version every generated device reports at boot. It is
// named because the OTA tests roll forward from it and roll back to it, and a
// literal in three places is how a rollback assertion silently stops asserting.
const InitialFirmware = "1.4.2"

// HardwareTier is the display and radio generation every generated label
// claims. An OTA job targets a tier, so this and the artifact's tier are one
// contract.
const HardwareTier = "esl-2.9-bw"

// provisionStore enrols every device in a store and applies its planogram.
//
// # Why this is the long way round
//
// It would be a dozen lines to write registry entries directly, and the result
// would be a demonstration that proves nothing. The interesting property of
// this platform is that a device cannot join without proving who it is, so
// every device here is issued a genuine certificate from the platform's own
// Manufacturing or Shelf Controller Sub-CA, written into a manufacturing
// manifest, and then enrolled through registry.Provision — the same function a
// label on a shelf goes through, with the same chain verification, the same
// revocation check, the same manifest comparison (public key, certificate
// serial, radio address, hardware tier) and the same anti-cloning check.
//
// Nothing about the identity comes from the request body. The controller that
// relays a first-power-on announcement supplies placement, which is the part no
// factory could have known; everything else is read out of the verified
// certificate. That is why the certificate has to be real.
//
// # Order
//
// Controllers are provisioned before the labels they own, and the planogram is
// uploaded last. A label whose controller the registry has never heard of would
// be enrolled with a placement pointing at nothing; a planogram naming a label
// that does not exist assigns nothing.
func (s *Stack) provisionStore(ctx context.Context, st *Store) error {
	started := time.Now()
	reg := s.cloudSvcs.registry
	rng := rand.New(rand.NewSource(s.cfg.Seed + int64(len(s.stores))))

	// Radio addresses are drawn deterministically but must be unique
	// fleet-wide, so the store identifier is folded into the high bytes: two
	// stores that collided on a radio address would trip the registry's
	// anti-cloning check and quarantine both devices.
	euiBase := euiPrefixFor(st.ID)
	next := 0
	nextEUI := func() string {
		next++
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], euiBase|uint64(next))
		return strings.ToUpper(hex.EncodeToString(b[:]))
	}

	type unit struct {
		id   string
		kind regdomain.DeviceKind
		eui  string
		sec  canon.SECID
		zone string
		// spare marks a device whose identity is minted now but which does not
		// announce itself until something clips it onto a rail.
		spare bool
	}
	var units []unit
	for i, z := range st.Zones {
		zoneName := fmt.Sprintf("aisle-%02d", i+1)
		units = append(units, unit{id: string(z.SECID), kind: regdomain.KindSEC, eui: nextEUI(), zone: zoneName})
		for _, id := range z.provisioned {
			units = append(units, unit{
				id: string(id), kind: regdomain.KindLabel, eui: nextEUI(),
				sec: z.SECID, zone: zoneName,
			})
		}
		if z.spare != "" {
			units = append(units, unit{
				id: string(z.spare), kind: regdomain.KindLabel, eui: nextEUI(),
				sec: z.SECID, zone: zoneName, spare: true,
			})
		}
	}

	manifest := &regdomain.Manifest{
		TenantID:   st.Tenant,
		BatchID:    fmt.Sprintf("usslpd-%s-%d", st.ID, s.cfg.Seed),
		IngestedAt: time.Now().UTC(),
		Source:     "usslpd",
	}
	issuer := regadapters.NewHierarchyIssuer(s.hierarchy)
	chains := make(map[string][]byte, len(units))
	records := make(map[string]regdomain.ManufacturingRecord, len(units))
	alreadyEnrolled := 0
	for _, u := range units {
		// A restart against an existing data directory finds every device
		// already on record. Re-issuing would mint a second certificate with a
		// different serial, and the registry — correctly — refuses a
		// manufacturing record whose contents changed, because the factory does
		// not get to revise what it built. Skipping is not a shortcut: a device
		// that is already enrolled has nothing to prove again.
		if !u.spare && reg.Device(u.id) != nil {
			if u.kind == regdomain.KindLabel {
				st.labelToSEC[canon.LabelID(u.id)] = u.sec
			}
			alreadyEnrolled++
			continue
		}
		id := pki.Identity{
			Kind: pkiKindOf(u.kind), TenantID: st.Tenant, StoreID: st.ID, DeviceID: u.id,
		}
		issued, err := issuer.IssueDevice(id, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("usslpd: issuing a certificate for %s: %w", u.id, err)
		}
		spki, err := x509.MarshalPKIXPublicKey(issued.Certificate.PublicKey)
		if err != nil {
			return fmt.Errorf("usslpd: marshalling the public key for %s: %w", u.id, err)
		}
		rec := regdomain.ManufacturingRecord{
			Serial:          canon.DeviceSerial(euiValueOf(u.eui)),
			EUI64:           u.eui,
			HardwareTier:    HardwareTier,
			FirmwareVersion: InitialFirmware,
			TenantID:        st.Tenant,
			StoreID:         st.ID,
			DeviceID:        u.id,
			Kind:            u.kind,
			CertSerial:      issued.Identity.SerialNumber,
			PublicKeySPKI:   spki,
			BatchID:         manifest.BatchID,
			ManufacturedAt:  time.Now().UTC(),
		}
		records[u.id] = rec
		chains[u.id] = issued.ChainPEM()
		if u.spare {
			// Held back entirely: no manifest line and no enrolment, so the
			// registry genuinely does not know this device exists until
			// CommissionSpare runs.
			st.spareRecord[canon.LabelID(u.id)] = spareIdentity{record: rec, chain: issued.ChainPEM()}
			continue
		}
		manifest.Records = append(manifest.Records, rec)
	}
	if len(manifest.Records) > 0 {
		if _, err := reg.IngestManifest(ctx, manifest); err != nil {
			return fmt.Errorf("usslpd: ingesting the manufacturing manifest for %s: %w", st.ID, err)
		}
	}

	enrolled := 0
	for _, u := range units {
		if u.spare {
			continue
		}
		if _, minted := chains[u.id]; !minted {
			continue // already enrolled from a previous run
		}
		if _, err := reg.Provision(ctx, regapp.ProvisionRequest{
			CertificateChainPEM: chains[u.id],
			EUI64:               u.eui,
			SECID:               u.sec,
			Zone:                u.zone,
			FirmwareVersion:     InitialFirmware,
			ReportedBy:          u.sec,
		}); err != nil {
			return fmt.Errorf("usslpd: provisioning %s: %w", u.id, err)
		}
		enrolled++
		if u.kind == regdomain.KindLabel {
			st.labelToSEC[canon.LabelID(u.id)] = u.sec
		}
	}

	// The planogram is what turns "a label exists" into "a label shows this
	// product": the Label Service's fan-out resolves store plus SKU to
	// placements, and a label with no assignment is a label no price change
	// will ever reach.
	pg := &regdomain.Planogram{TenantID: st.Tenant, StoreID: st.ID, Source: "usslpd"}
	st.planogram = pg
	templates := []string{"standard", "standard", "standard", "promo", "unit_price"}
	for i, z := range st.Zones {
		for j, labelID := range z.provisioned {
			sku := SKUFor(st.ID, i, j)
			st.skuOf[labelID] = sku
			pg.Positions = append(pg.Positions, regdomain.Position{
				PositionKey: regdomain.PositionKey{
					Shelf:    fmt.Sprintf("shelf-%02d", i+1),
					Rail:     fmt.Sprintf("rail-%d", j/10+1),
					Position: j%10 + 1,
				},
				LabelID:  labelID,
				SKU:      sku,
				Facings:  1 + rng.Intn(3),
				Template: templates[rng.Intn(len(templates))],
				SECID:    z.SECID,
				Zone:     fmt.Sprintf("aisle-%02d", i+1),
			})
		}
	}
	applied, err := reg.UploadPlanogram(ctx, pg)
	if err != nil {
		return fmt.Errorf("usslpd: uploading the planogram for %s: %w", st.ID, err)
	}

	s.bootRT.Log.Info("store provisioned through the zero-touch path",
		"store", string(st.ID), "devices_enrolled", enrolled,
		"already_enrolled", alreadyEnrolled, "spares_held_back", len(st.spareRecord),
		"positions", len(pg.Positions), "assigned", applied.Assigned,
		"planogram_revision", applied.Revision,
		"elapsed_ms", time.Since(started).Milliseconds())
	return nil
}

// ErrNoSpare means a zone's spare label has already been commissioned.
var ErrNoSpare = errors.New("usslpd: this zone has no uncommissioned spare label")

// CommissionSpare enrols a zone's held-back label into a running store, exactly
// as a replacement unit clipped onto a rail would be: a device the registry has
// never seen presents a certificate, is checked against the manufacturing
// record, is enrolled, is assigned a shelf position by the planogram, and gets
// its first price with no human step.
//
// Every zone is built with one label more than it provisions at boot, precisely
// so that this path can be exercised against a running platform. Proving that
// provisioning works during start-up proves only that start-up works.
func (s *Stack) CommissionSpare(ctx context.Context, st *Store, z *Zone, sku canon.SKU) (canon.LabelID, error) {
	if z.spare == "" {
		return "", ErrNoSpare
	}
	labelID := z.spare
	rec, ok := st.spareRecord[labelID]
	if !ok {
		return "", fmt.Errorf("usslpd: no manufacturing record was prepared for %s", labelID)
	}

	reg := s.cloudSvcs.registry
	// The manifest is ingested now rather than at boot, so this really is a
	// device the registry has no record of until the moment it announces —
	// which is the case the manifest comparison exists to catch.
	if _, err := reg.IngestManifest(ctx, &regdomain.Manifest{
		TenantID: st.Tenant, BatchID: "usslpd-replacement-" + string(labelID),
		IngestedAt: time.Now().UTC(), Source: "usslpd",
		Records: []regdomain.ManufacturingRecord{rec.record},
	}); err != nil {
		return "", fmt.Errorf("usslpd: ingesting the replacement manifest: %w", err)
	}
	if _, err := reg.Provision(ctx, regapp.ProvisionRequest{
		CertificateChainPEM: rec.chain, EUI64: rec.record.EUI64,
		SECID: z.SECID, Zone: "aisle-replacement", FirmwareVersion: InitialFirmware,
		ReportedBy: z.SECID,
	}); err != nil {
		return "", fmt.Errorf("usslpd: provisioning %s: %w", labelID, err)
	}

	// The planogram is replaced, not patched: an upload is the whole store's
	// layout, and the registry repositions every affected label against it.
	//
	// It has to be a *copy*. The registry keeps the document it was handed, and
	// diffs the next upload against it; handing it back the same slice with one
	// row appended would have it diff an object against itself, produce an empty
	// diff, and assign nothing — a silent failure whose only symptom is a label
	// that never shows a price.
	next := &regdomain.Planogram{
		TenantID: st.planogram.TenantID, StoreID: st.planogram.StoreID,
		Source:    st.planogram.Source,
		Positions: append([]regdomain.Position(nil), st.planogram.Positions...),
	}
	next.Positions = append(next.Positions, regdomain.Position{
		PositionKey: regdomain.PositionKey{
			Shelf: "shelf-replacement", Rail: "rail-1", Position: len(next.Positions) + 1,
		},
		LabelID: labelID, SKU: sku, Facings: 1, Template: "standard",
		SECID: z.SECID, Zone: "aisle-replacement",
	})
	if _, err := reg.UploadPlanogram(ctx, next); err != nil {
		return "", fmt.Errorf("usslpd: assigning %s a shelf position: %w", labelID, err)
	}
	st.planogram = next

	st.labelToSEC[labelID] = z.SECID
	st.skuOf[labelID] = sku
	z.mu.Lock()
	z.provisioned = append(z.provisioned, labelID)
	z.spare = ""
	z.mu.Unlock()
	return labelID, nil
}

// spareIdentity is a certificate and manufacturing record minted at boot for a
// label that has not yet announced itself.
type spareIdentity struct {
	record regdomain.ManufacturingRecord
	chain  []byte
}

// pkiKindOf maps a registry device kind onto the PKI identity kind naming the
// same hardware, which decides which sub-CA signs it.
func pkiKindOf(k regdomain.DeviceKind) pki.IdentityKind {
	switch k {
	case regdomain.KindLabel:
		return pki.KindLabel
	case regdomain.KindSEC:
		return pki.KindSEC
	default:
		return pki.KindSGU
	}
}

// euiPrefixFor derives the high 32 bits of a store's radio addresses from its
// identifier.
func euiPrefixFor(store canon.StoreID) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(store); i++ {
		h ^= uint64(store[i])
		h *= 1099511628211
	}
	return (h << 32) & 0xFFFFFFFF00000000
}

func euiValueOf(eui string) uint64 {
	b, err := hex.DecodeString(eui)
	if err != nil || len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func hexOf(v uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return hex.EncodeToString(b[:])
}

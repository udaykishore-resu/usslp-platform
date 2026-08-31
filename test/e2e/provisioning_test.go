package e2e

import (
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestZeroTouchProvisioning clips a new label onto a rail in a running store
// and asserts it is trading with no human step.
//
// Every zone in a usslpd store is built with one label more than the platform
// is told about: real hardware on the rail that the Device Registry has never
// heard of. Commissioning it exercises the whole enrolment path against a live
// platform rather than during boot — certificate chain verified against the
// hierarchy, identity read out of the certificate and never out of the request,
// manufacturing record compared, anti-cloning check run, planogram assignment
// applied, first price delivered.
func TestZeroTouchProvisioning(t *testing.T) {
	st := newStack(t, smallStore(2, 6))
	store := st.Stores()[0]
	zone := store.Zones[0]

	spare := zone.Spare()
	if spare == "" {
		t.Fatal("the zone has no uncommissioned spare label to enrol")
	}
	// Before: the platform genuinely does not know this device.
	if _, err := st.Services().Label().Directory().Lookup(t.Context(), spare); err == nil {
		t.Fatalf("%s is already in the label directory; the test would prove nothing", spare)
	}
	if d := st.Services().Registry().Device(string(spare)); d != nil {
		t.Fatalf("%s is already in the device registry", spare)
	}

	deviceEvents := collectStream(t, st, canon.StreamDeviceEvents.Name, "e2e-provisioning")
	sku := canon.SKU("SKU-REPLACEMENT-0001")
	st.BasePrice(store.ID, sku) // no-op; the price book has nothing for a new line yet

	started := time.Now()
	id, err := st.CommissionSpare(t.Context(), store, zone, sku)
	if err != nil {
		t.Fatalf("commissioning %s: %v", spare, err)
	}
	if id != spare {
		t.Fatalf("commissioned %s, expected %s", id, spare)
	}

	// 1. The certificate chain was verified and the device is on record.
	dev := st.Services().Registry().Device(string(id))
	if dev == nil {
		t.Fatal("the device is not in the registry after provisioning")
	}
	if dev.CertSerial == "" {
		t.Error("the registry recorded no certificate serial; the chain was not verified")
	}
	if dev.Placement.StoreID != store.ID || dev.Placement.SECID != zone.SECID {
		t.Errorf("the device was placed at %s/%s, not %s/%s",
			dev.Placement.StoreID, dev.Placement.SECID, store.ID, zone.SECID)
	}
	if dev.TenantID != store.Tenant {
		t.Errorf("the device was enrolled into tenant %q, not %q", dev.TenantID, store.Tenant)
	}
	t.Logf("enrolled %s: cert serial %s, expires %s, state %s",
		id, dev.CertSerial, dev.CertNotAfter.Format(time.DateOnly), dev.State)

	// 2. The enrolment was published for the rest of the platform to project.
	if _, ok := awaitEnvelope(t, deviceEvents, 10*time.Second, func(e canon.Envelope) bool {
		return e.EventType == canon.EvtLabelProvisioned && e.AggregateID == string(id)
	}); !ok {
		t.Error("no device.label.provisioned event was published for the new label")
	}

	// 3. The planogram assignment reached the Label Service's directory.
	eventually(t, 20*time.Second, "the label to appear in the fan-out directory", func() bool {
		p, err := st.Services().Label().Directory().Lookup(t.Context(), id)
		return err == nil && p.SKU == sku && p.SECID == zone.SECID
	})

	// 4. Its first price arrives, through the ordinary POS path, with no human
	//    having touched the device.
	tg := target{Store: store, Zone: zone, Label: id, SKU: sku, Tenant: store.Tenant}
	d, wall := pushPrice(t, st, tg, usd(449))
	if !d.Delivered {
		t.Fatal("the newly provisioned label never confirmed its first price")
	}
	if ok, why := st.GlassMatches(zone, id, usd(449)); !ok {
		t.Errorf("the first price is not on the new label's glass: %s", why)
	}
	verifyAttestation(t, st, tg, usd(449))

	t.Logf("a label that the platform had never seen went from powered on to trading "+
		"in %s, with no human step; its first price landed in %s",
		time.Since(started).Round(time.Millisecond), wall.Round(time.Millisecond))
}

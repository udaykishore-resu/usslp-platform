package labelsim

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/edge/mesh"
	"github.com/usslp/usslp/edge/sim"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// attestFixture is a price authority, the ring a label syncs from it, and a
// helper that builds properly attested type 4 frames.
type attestFixture struct {
	authority *pki.PriceAuthority
	ring      *pki.KeyRing
}

// keyEpoch is when the fixture's price authority key came into force.
//
// It has to precede the simulated clock the labels verify against: a key ring
// is checked against the instant the label believes it is, and a key generated
// "now" is not yet valid in a simulation whose epoch is 2023. That is the key
// ring behaving exactly as intended, and it is worth a comment because a test
// that trips over it looks like a signature failure and is not.
var keyEpoch = time.Unix(1699000000, 0).UTC()

func newAttestFixture(t *testing.T) *attestFixture {
	t.Helper()
	a, err := pki.NewPriceAuthority(pki.PriceAuthorityConfig{Now: keyEpoch})
	if err != nil {
		t.Fatalf("creating a price authority: %v", err)
	}
	ring, err := a.KeyRing()
	if err != nil {
		t.Fatalf("publishing the key ring: %v", err)
	}
	return &attestFixture{authority: a, ring: ring}
}

// frame builds an attested update for a label, signed by the fixture's
// authority, exactly as a Shelf Edge Controller would.
func (f *attestFixture) frame(t *testing.T, id canon.LabelID, seq int64, minor int64, image []byte) AttestedUpdate {
	t.Helper()
	return f.frameSignedBy(t, f.authority, id, seq, minor, image)
}

func (f *attestFixture) frameSignedBy(t *testing.T, by *pki.PriceAuthority,
	id canon.LabelID, seq int64, minor int64, image []byte) AttestedUpdate {
	t.Helper()
	effective := time.Unix(1741944413, 0).UTC()
	in := canon.AttestationInput{
		TenantID: "acme-retail", StoreID: "store-0417", LabelID: id, SKU: "SKU-12345",
		Price:       canon.NewMoney(minor, "GBP"),
		EffectiveAt: effective, Sequence: seq, PromotionID: "",
	}
	att, err := by.Sign(in)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	digest, err := hex.DecodeString(att.Digest)
	if err != nil {
		t.Fatalf("decoding digest: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(att.Signature)
	if err != nil {
		t.Fatalf("decoding signature: %v", err)
	}
	a := AttestedUpdate{
		Update:          Update{Sequence: seq, PriceMinor: minor, Currency: "GBP", Image: image},
		EffectiveAtUnix: effective.Unix(), Alg: AttestAlgEd25519, KeyID: att.KeyID,
		TenantID: in.TenantID, StoreID: in.StoreID, LabelID: id, SKU: in.SKU,
	}
	copy(a.Digest[:], digest)
	copy(a.Signature[:], sig)
	return a
}

func imageOf(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed
	}
	return b
}

// ---------------------------------------------------------------------------
// The wire format
// ---------------------------------------------------------------------------

func TestAttestedFrameMatchesTheFirmwareLayout(t *testing.T) {
	// firmware/src/radio/usslp_wire.h fixes every one of these offsets and its
	// host tests decode frames this encoder produces, so the layout is a
	// contract rather than an implementation detail.
	f := newAttestFixture(t)
	img := imageOf(64, 7)
	a := f.frame(t, "lbl-0001", 42, 249, img)
	a.Flags = FlagRequestPartial
	a.Template = 3
	a.OriginX, a.OriginY = 16, 32
	a.PromotionID = "promo-xyz"
	// Re-sign, because the promotion is part of the signed tuple.
	a = f.frame(t, "lbl-0001", 42, 249, img)
	a.Flags = FlagRequestPartial
	a.Template = 3
	a.OriginX, a.OriginY = 16, 32

	b, err := EncodeAttestedUpdate(a)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	ids := len(a.TenantID) + len(a.StoreID) + len(a.LabelID) + len(a.SKU) + len(a.PromotionID)
	if got, want := len(b), AttestedHeaderBytes+ids+len(img); got != want {
		t.Fatalf("frame is %d bytes, the layout gives %d", got, want)
	}
	if b[0] != WireVersion || b[1] != FrameAttestedUpdate {
		t.Fatalf("frame header is %d/%d, want %d/%d", b[0], b[1], WireVersion, FrameAttestedUpdate)
	}
	// The first 33 bytes must be a type 1 update but for the type byte: the
	// firmware's two decoders share a head, and a controller is meant to be
	// able to truncate one frame into the other.
	legacy, err := EncodeUpdate(a.Update)
	if err != nil {
		t.Fatalf("encoding the legacy form: %v", err)
	}
	if !bytes.Equal(b[2:updateHeaderBytes], legacy[2:updateHeaderBytes]) {
		t.Fatal("the first 33 bytes of a type 4 frame do not match a type 1 frame; the firmware's decoders share this head")
	}
	if b[41] != AttestAlgEd25519 {
		t.Fatalf("algorithm byte at offset 41 is %d, want %d", b[41], AttestAlgEd25519)
	}
	if got := string(b[42 : 42+KeyIDLen]); got != a.KeyID {
		t.Fatalf("key id at offset 42 is %q, want %q", got, a.KeyID)
	}
	if !bytes.Equal(b[70:70+DigestLen], a.Digest[:]) {
		t.Fatal("digest is not at offset 70")
	}
	if !bytes.Equal(b[102:102+SignatureLen], a.Signature[:]) {
		t.Fatal("signature is not at offset 102")
	}
	if int(b[166]) != len(a.TenantID) || int(b[167]) != len(a.StoreID) ||
		int(b[168]) != len(a.LabelID) || int(b[169]) != len(a.SKU) || int(b[170]) != len(a.PromotionID) {
		t.Fatalf("identifier lengths at offsets 166-170 are %v", b[166:171])
	}
	if got := string(b[AttestedHeaderBytes : AttestedHeaderBytes+len(a.TenantID)]); got != string(a.TenantID) {
		t.Fatalf("identifiers do not start at offset %d: found %q", AttestedHeaderBytes, got)
	}
	if !bytes.Equal(b[len(b)-len(img):], img) {
		t.Fatal("the image is not the last field")
	}

	got, err := DecodeAttestedUpdate(b)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Sequence != a.Sequence || got.PriceMinor != a.PriceMinor || got.Currency != a.Currency ||
		got.Flags != a.Flags || got.Template != a.Template || got.OriginX != a.OriginX ||
		got.OriginY != a.OriginY || got.EffectiveAtUnix != a.EffectiveAtUnix ||
		got.KeyID != a.KeyID || got.Digest != a.Digest || got.Signature != a.Signature ||
		got.TenantID != a.TenantID || got.StoreID != a.StoreID || got.LabelID != a.LabelID ||
		got.SKU != a.SKU || got.PromotionID != a.PromotionID || !bytes.Equal(got.Image, img) {
		t.Fatalf("round trip changed the frame:\n got %+v\nwant %+v", got, a)
	}

	// A truncated frame must be refused at every length, which is the firmware's
	// own assertion.
	for n := 0; n < len(b); n++ {
		if _, err := DecodeAttestedUpdate(b[:n]); err == nil {
			t.Fatalf("a %d-byte prefix of a %d-byte frame decoded successfully", n, len(b))
		}
	}
	// And a corrupted image must be caught by the frame's own checksum.
	bad := append([]byte(nil), b...)
	bad[len(bad)-1] ^= 0xFF
	if _, err := DecodeAttestedUpdate(bad); err == nil {
		t.Fatal("a frame with a corrupted image decoded successfully")
	}
}

func TestAttestedFrameRebuildsTheCanonicalTuple(t *testing.T) {
	// The property the whole frame exists for: the identifiers that crossed the
	// air produce the digest the platform signed.
	f := newAttestFixture(t)
	a := f.frame(t, "lbl-0001", 7, 1999, imageOf(32, 1))
	b, err := EncodeAttestedUpdate(a)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	got, err := DecodeAttestedUpdate(b)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	digest := got.AttestationInput().Digest()
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(a.Digest[:]) {
		t.Fatalf("the digest rebuilt from the wire is %x, the signed one is %x", digest, a.Digest)
	}
	if err := f.ring.VerifyAt(got.AttestationInput(), got.Attestation(), time.Now()); err != nil {
		t.Fatalf("a frame the platform signed did not verify from the wire: %v", err)
	}
}

func TestAttestedFrameRefusesIllegalIdentifiers(t *testing.T) {
	// An identifier carrying an MQTT separator is an attempt to address outside
	// a tenant's namespace, and must never be canonicalised as though it were
	// legitimate. The firmware refuses the same set.
	f := newAttestFixture(t)
	a := f.frame(t, "lbl-0001", 1, 100, imageOf(8, 0))
	a.TenantID = "acme/+/#"
	if _, err := EncodeAttestedUpdate(a); err == nil {
		t.Fatal("an identifier carrying MQTT separators was encoded")
	}
	a = f.frame(t, "lbl-0001", 1, 100, imageOf(8, 0))
	a.SKU = ""
	if err := a.ValidateIdentifiers(); err == nil {
		t.Fatal("an empty SKU was accepted; only the promotion may be empty")
	}
	a = f.frame(t, "lbl-0001", 1, 100, imageOf(8, 0))
	a.KeyID = "too-short"
	if _, err := EncodeAttestedUpdate(a); err == nil {
		t.Fatal("a key id that does not fit the fixed-width field was encoded")
	}
}

// ---------------------------------------------------------------------------
// The label
// ---------------------------------------------------------------------------

// attestedZone builds a small zone whose labels verify for themselves.
func attestedZone(t *testing.T, f *attestFixture, mode AttestationMode) (*sim.Engine, *Zone) {
	t.Helper()
	return newZone(t, ZoneSpec{
		SECID: "sec-0042", StoreID: "store-0417", Labels: 4, AisleLengthM: 6,
		KeyRing: f.ring, Attestation: mode,
	})
}

func TestLabelVerifiesEndToEndAndRenders(t *testing.T) {
	f := newAttestFixture(t)
	eng, z := attestedZone(t, f, AttestEndToEnd)
	lbl := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)

	a := f.frame(t, lbl.ID(), 1, 249, imageOf(200, 3))
	payload, err := EncodeAttestedUpdate(a)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)

	s := lbl.Stats()
	if s.Sequence != 1 || s.RefreshCount != 1 {
		t.Fatalf("a valid attested price did not reach the glass: sequence %d, %d refreshes",
			s.Sequence, s.RefreshCount)
	}
	if s.Verifications != 1 {
		t.Fatalf("the label performed %d verifications for one attested frame", s.Verifications)
	}
	if s.AttestationFailures != 0 {
		t.Fatalf("a valid price produced %d attestation failures", s.AttestationFailures)
	}
	if lbl.AttestationMode() != AttestEndToEnd {
		t.Fatal("the label is not in end-to-end mode")
	}
}

func TestLabelRefusesATamperedPriceAndKeepsTheOldImage(t *testing.T) {
	f := newAttestFixture(t)
	eng, z := attestedZone(t, f, AttestEndToEnd)
	lbl := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)

	good := f.frame(t, lbl.ID(), 1, 249, imageOf(200, 3))
	payload, err := EncodeAttestedUpdate(good)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)
	before := lbl.Stats()
	if before.RefreshCount != 1 {
		t.Fatal("the first, valid update did not land")
	}

	var refusals []Event
	lbl.OnEvent(func(e Event) {
		if e.Kind == EventAttestationRefused {
			refusals = append(refusals, e)
		}
	})

	// Rewrite the price after signing: the signature is genuine, the digest is
	// genuine, and neither describes the price about to be rendered. The label
	// recomputes the digest from what it holds, so this cannot pass.
	tampered := f.frame(t, lbl.ID(), 2, 249, imageOf(200, 9))
	tampered.PriceMinor = 1
	payload, err = EncodeAttestedUpdate(tampered)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)

	after := lbl.Stats()
	if after.RefreshCount != before.RefreshCount {
		t.Fatal("a tampered price drove the panel; the previous image must stay on the glass")
	}
	if after.Sequence != 1 {
		t.Fatalf("the label advanced to sequence %d on a price it could not verify", after.Sequence)
	}
	if after.AttestationFailures != 1 {
		t.Fatalf("counted %d attestation failures, want 1", after.AttestationFailures)
	}
	if len(refusals) != 1 || !strings.Contains(refusals[0].Reason, "digest mismatch") {
		t.Fatalf("the refusal does not identify the tampering: %+v", refusals)
	}
	t.Logf("refused and held the previous price: %s", refusals[0].Reason)

	// And a signature that is simply wrong.
	forged := f.frame(t, lbl.ID(), 3, 500, imageOf(200, 11))
	forged.Signature[0] ^= 0xFF
	payload, err = EncodeAttestedUpdate(forged)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)
	if got := lbl.Stats(); got.RefreshCount != before.RefreshCount || got.AttestationFailures != 2 {
		t.Fatalf("a forged signature was accepted: %d refreshes, %d failures",
			got.RefreshCount, got.AttestationFailures)
	}
}

func TestLabelRefusesAnUnknownKeyID(t *testing.T) {
	f := newAttestFixture(t)
	eng, z := attestedZone(t, f, AttestEndToEnd)
	lbl := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)

	// A perfectly valid attestation from an authority this label has never
	// heard of. The signature verifies under its own key; the label does not
	// hold that key, and an update whose signer cannot be identified is never
	// displayed.
	rogue := newAttestFixture(t)
	a := rogue.frame(t, lbl.ID(), 1, 100, imageOf(200, 5))
	payload, err := EncodeAttestedUpdate(a)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	var reason string
	lbl.OnEvent(func(e Event) {
		if e.Kind == EventAttestationRefused {
			reason = e.Reason
		}
	})
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)

	s := lbl.Stats()
	if s.RefreshCount != 0 || s.Sequence != 0 {
		t.Fatal("a price signed by an unknown authority reached the glass")
	}
	if s.AttestationFailures != 1 {
		t.Fatalf("counted %d attestation failures, want 1", s.AttestationFailures)
	}
	if !errors.Is(pki.ErrUnknownKeyID, pki.ErrUnknownKeyID) || !strings.Contains(reason, "unknown price authority key id") {
		t.Fatalf("the refusal does not name the missing key: %q", reason)
	}
	t.Logf("refused: %s", reason)
}

func TestLabelWithNoKeyRingFailsClosed(t *testing.T) {
	// A label that requires verification and holds no ring verifies nothing.
	// Displaying on that basis would make the whole apparatus decorative.
	f := newAttestFixture(t)
	eng := sim.New(time.Unix(1700000000, 0).UTC(), 1)
	z, err := NewZone(eng, ZoneSpec{SECID: "sec-0042", StoreID: "store-0417", Labels: 2,
		AisleLengthM: 6, Attestation: AttestEndToEnd})
	if err != nil {
		t.Fatalf("building the zone: %v", err)
	}
	z.Form(func(time.Duration) {})
	eng.RunUntil(eng.Elapsed() + 2*time.Minute)
	lbl := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)

	payload, err := EncodeAttestedUpdate(f.frame(t, lbl.ID(), 1, 249, imageOf(200, 3)))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)
	if s := lbl.Stats(); s.RefreshCount != 0 || s.AttestationFailures != 1 {
		t.Fatalf("a label with no key ring rendered anyway: %d refreshes, %d failures",
			s.RefreshCount, s.AttestationFailures)
	}
}

func TestUnattestedFrameRefusedByDefaultAndAcceptedInCompatibilityMode(t *testing.T) {
	f := newAttestFixture(t)
	img := imageOf(200, 3)
	legacy, err := EncodeUpdate(Update{Sequence: 1, PriceMinor: 249, Currency: "GBP", Image: img})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	// The shipped posture: a type 1 frame is refused, and the refusal is
	// counted so that a zone running mismatched firmware is visible.
	eng, z := attestedZone(t, f, AttestEndToEnd)
	strict := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)
	z.Net.Send(mesh.TxRequest{Dst: strict.NodeID(), Payload: legacy})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)
	if s := strict.Stats(); s.RefreshCount != 0 || s.UnattestedRefused != 1 {
		t.Fatalf("a label requiring end-to-end attestation took an unattested price: %d refreshes, %d refusals",
			s.RefreshCount, s.UnattestedRefused)
	}

	// Compatibility: a deployment whose controllers predate frame type 4 runs
	// this, trusting the controller to have verified, and the labels work.
	eng2, z2 := attestedZone(t, f, AttestTrustController)
	compat := z2.Labels()[0]
	z2.OpenActiveWindow(10 * time.Minute)
	eng2.RunUntil(eng2.Elapsed() + 40*time.Second)
	z2.Net.Send(mesh.TxRequest{Dst: compat.NodeID(), Payload: legacy})
	eng2.RunUntil(eng2.Elapsed() + 30*time.Second)
	s := compat.Stats()
	if s.RefreshCount != 1 || s.Sequence != 1 {
		t.Fatalf("compatibility mode refused a type 1 frame: %d refreshes, sequence %d",
			s.RefreshCount, s.Sequence)
	}
	if s.Verifications != 0 {
		t.Fatal("compatibility mode verified something; there is nothing in a type 1 frame to verify")
	}
	// And a label in compatibility mode still verifies an attested frame when
	// it gets one: the mode says what it will accept, not what it will skip.
	payload, err := EncodeAttestedUpdate(f.frame(t, compat.ID(), 2, 199, img))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z2.Net.Send(mesh.TxRequest{Dst: compat.NodeID(), Payload: payload})
	eng2.RunUntil(eng2.Elapsed() + 30*time.Second)
	if got := compat.Stats(); got.Verifications != 1 || got.Sequence != 2 {
		t.Fatalf("a compatibility-mode label ignored an attestation it was given: %d verifications, sequence %d",
			got.Verifications, got.Sequence)
	}
}

func TestSequenceRuleRunsBeforeVerification(t *testing.T) {
	// A duplicate is the common case under at-least-once mesh delivery, and an
	// Ed25519 verification is thirteen milliseconds of a coin cell's life.
	// Checking the free invariant first costs nothing in safety, and the
	// firmware orders it the same way.
	f := newAttestFixture(t)
	eng, z := attestedZone(t, f, AttestEndToEnd)
	lbl := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)

	payload, err := EncodeAttestedUpdate(f.frame(t, lbl.ID(), 5, 249, imageOf(200, 3)))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)
	if got := lbl.Stats().Verifications; got != 1 {
		t.Fatalf("%d verifications after one update", got)
	}
	// The same frame again: discarded by sequence, with no signature touched.
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)
	s := lbl.Stats()
	if s.Discarded != 1 {
		t.Fatalf("the duplicate was not discarded: %d discards", s.Discarded)
	}
	if s.Verifications != 1 {
		t.Fatalf("the duplicate cost %d verifications; the sequence rule must run first",
			s.Verifications-1)
	}
}

func TestEndToEndAttestationCosts(t *testing.T) {
	// Both halves of the honest accounting: what the bigger frame costs the
	// zone's shared channel, and what the verification costs the cell.
	f := newAttestFixture(t)
	img := imageOf(320, 3) // a typical windowed partial update
	legacy, err := EncodeUpdate(Update{Sequence: 1, PriceMinor: 249, Currency: "GBP", Image: img})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	attested, err := EncodeAttestedUpdate(f.frame(t, "sec-0042-lbl-00000", 1, 249, img))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	t.Logf("a %d-byte image: type 1 frame %d bytes / %d fragments / %v airtime per hop; "+
		"type 4 frame %d bytes / %d fragments / %v airtime per hop",
		len(img), len(legacy), mesh.Fragments(len(legacy)), mesh.Airtime(len(legacy)).Round(time.Microsecond),
		len(attested), mesh.Fragments(len(attested)), mesh.Airtime(len(attested)).Round(time.Microsecond))

	if len(attested) <= len(legacy) {
		t.Fatal("the attested frame is not larger; the signed tuple has to be somewhere")
	}
	if AttestedOverheadBytes != 138 {
		t.Fatalf("the fixed overhead is %d bytes, the firmware's layout gives 138", AttestedOverheadBytes)
	}

	with := DefaultPower().Project(Tier29BWR, DefaultWorkload())
	without := DefaultWorkload()
	without.EndToEndAttestation = false
	no := DefaultPower().Project(Tier29BWR, without)
	t.Logf("battery with end-to-end attestation: %.3f uA -> %.2f years "+
		"(verification %.4f uA, receiver-on %.3f uA)", with.TotalUA, with.Years, with.VerifyUA, with.DataRXUA)
	t.Logf("battery trusting the controller:     %.3f uA -> %.2f years (receiver-on %.3f uA)",
		no.TotalUA, no.Years, no.DataRXUA)
	t.Logf("end-to-end attestation costs %.3f uA, which is %.2f%% of the budget and %.2f years of life",
		with.TotalUA-no.TotalUA, 100*(with.TotalUA-no.TotalUA)/no.TotalUA, no.Years-with.Years)

	if !with.MeetsTarget {
		t.Errorf("with end-to-end attestation the projection is %.2f years, outside the 7-10 year commitment",
			with.Years)
	}
	if with.VerifyUA > 0.05 {
		t.Fatalf("one Ed25519 verification per update costs %.4f uA; at 13 ms and 3 mA it cannot", with.VerifyUA)
	}
}

// ---------------------------------------------------------------------------
// Refusal status codes and verdicts
// ---------------------------------------------------------------------------

func TestAckStatusAndVerdictRoundTrip(t *testing.T) {
	// The flags byte carries the two display bits and the three verdict bits,
	// and the frame is still twenty bytes. firmware/src/radio/usslp_wire.c
	// decodes exactly these positions.
	statuses := []AckStatus{AckApplied, AckStaleSequence, AckBadFrame,
		AckRefusedAttestation, AckRefusedUnattested}
	verdicts := []AttestVerdict{VerdictOK, VerdictBadAlgorithm, VerdictUnknownKeyID,
		VerdictKeyExpired, VerdictDigestMismatch, VerdictBadSignature,
		VerdictMalformedPrice, VerdictCryptoUnavailable}

	for _, st := range statuses {
		for _, v := range verdicts {
			for _, partial := range []bool{false, true} {
				for _, forced := range []bool{false, true} {
					in := Ack{Sequence: 4242, Status: st, RefreshMS: 1500,
						Partial: partial, ForcedFull: forced, Verdict: v,
						BatteryMV: 3010, BatteryPct: 96, TemperatureCentiC: -1850}
					raw := EncodeAck(in)
					if len(raw) != ackBytes {
						t.Fatalf("ack is %d bytes, the frame is fixed at %d", len(raw), ackBytes)
					}
					got, err := DecodeAck(raw)
					if err != nil {
						t.Fatalf("decoding: %v", err)
					}
					if got != in {
						t.Fatalf("round trip changed the ack:\n got %+v\nwant %+v", got, in)
					}
					// The verdict must live in bits 2-4 and nowhere else.
					if raw[13]>>ackVerdictShift&ackVerdictMask != byte(v) {
						t.Fatalf("verdict %v is not in bits 2-4 of the flags byte (%08b)", v, raw[13])
					}
					if raw[13]>>5 != 0 {
						t.Fatalf("bits 5-7 of the flags byte are %08b, they are reserved and sent as zero", raw[13])
					}
				}
			}
		}
	}
}

func TestAppliedAckIsUnchangedOnTheWire(t *testing.T) {
	// The firmware's compatibility claim rests on this: an applied
	// acknowledgement still encodes byte-identically to what a controller that
	// predates the verdict bits produced, because VerdictOK is zero.
	a := Ack{Sequence: 7, Status: AckApplied, RefreshMS: 300, Partial: true,
		BatteryMV: 3000, BatteryPct: 90, TemperatureCentiC: 2150}
	withVerdict := a
	withVerdict.Verdict = VerdictOK
	if !bytes.Equal(EncodeAck(a), EncodeAck(withVerdict)) {
		t.Fatal("an applied ack encodes differently once a verdict field exists")
	}
	if EncodeAck(a)[13] != ackFlagPartial {
		t.Fatalf("the flags byte of a plain partial-refresh ack is %08b, want just bit 0", EncodeAck(a)[13])
	}
}

func TestUnrecognisedStatusStillReadsAsBadFrame(t *testing.T) {
	// A controller meeting a status code from a future firmware must degrade to
	// today's behaviour rather than print a number at an operator.
	for _, code := range []AckStatus{5, 9, 200, 255} {
		if got := code.String(); got != "bad-frame" {
			t.Fatalf("status %d renders as %q; anything unrecognised must read as bad-frame", code, got)
		}
		if code.Refused() {
			t.Fatalf("status %d reports itself as a deliberate refusal", code)
		}
	}
	if !AckRefusedAttestation.Refused() || !AckRefusedUnattested.Refused() {
		t.Fatal("the two refusal codes do not report themselves as refusals")
	}
	if AckBadFrame.Refused() || AckApplied.Refused() {
		t.Fatal("a transport fault reported itself as a refusal")
	}
}

func TestVerdictClassification(t *testing.T) {
	f := newAttestFixture(t)
	base := f.frame(t, "lbl-0001", 1, 249, imageOf(16, 1))

	tampered := base
	tampered.PriceMinor = 1
	forged := base
	forged.Signature[0] ^= 0xFF
	badAlg := base
	badAlg.Alg = 9
	rogue := newAttestFixture(t).frame(t, "lbl-0001", 1, 249, imageOf(16, 1))
	malformed := base
	malformed.SKU = "SKU/with/separators"

	cases := []struct {
		name string
		err  error
		want AttestVerdict
	}{
		{"a valid attestation", nil, VerdictOK},
		{"a rewritten price", f.ring.VerifyAt(tampered.AttestationInput(), tampered.Attestation(), keyEpoch.Add(time.Hour)), VerdictDigestMismatch},
		{"a forged signature", f.ring.VerifyAt(forged.AttestationInput(), forged.Attestation(), keyEpoch.Add(time.Hour)), VerdictBadSignature},
		{"a key the label does not hold", f.ring.VerifyAt(rogue.AttestationInput(), rogue.Attestation(), keyEpoch.Add(time.Hour)), VerdictUnknownKeyID},
		{"a key outside its window", f.ring.VerifyAt(base.AttestationInput(), base.Attestation(), keyEpoch.Add(-time.Hour)), VerdictKeyExpired},
		{"an identifier that could not be canonicalised", malformed.ValidateIdentifiers(), VerdictMalformedPrice},
		{"no key ring at all", ErrNoKeyRing, VerdictUnknownKeyID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want != VerdictOK && tc.err == nil {
				t.Fatalf("the case produced no error, so there is nothing to classify as %v", tc.want)
			}
			if got := VerdictFor(tc.err); got != tc.want {
				t.Fatalf("classified as %v, want %v (error: %v)", got, tc.want, tc.err)
			}
		})
	}
	if !VerdictDigestMismatch.Tampering() || !VerdictBadSignature.Tampering() {
		t.Fatal("the two verdicts that mean the wire price is not the signed price do not report as tampering")
	}
	if VerdictUnknownKeyID.Tampering() || VerdictKeyExpired.Tampering() {
		t.Fatal("a key-management problem reported itself as tampering; those have different pagers")
	}
}

func TestLabelReportsRefusalStatusAndVerdict(t *testing.T) {
	f := newAttestFixture(t)
	eng, z := attestedZone(t, f, AttestEndToEnd)
	lbl := z.Labels()[0]
	z.OpenActiveWindow(10 * time.Minute)
	eng.RunUntil(eng.Elapsed() + 40*time.Second)

	// Collect what the label sends back to the coordinator.
	var acks []Ack
	if err := z.Net.SetReceiver(z.Coordinator(), func(fr mesh.Frame) {
		if kind, ok := FrameKind(fr.Payload); ok && kind == FrameAck {
			a, err := DecodeAck(fr.Payload)
			if err == nil {
				acks = append(acks, a)
			}
		}
	}); err != nil {
		t.Fatalf("installing the coordinator receiver: %v", err)
	}

	// A rewritten price: status 3, verdict digest-mismatch.
	tampered := f.frame(t, lbl.ID(), 1, 249, imageOf(200, 3))
	tampered.PriceMinor = 1
	payload, err := EncodeAttestedUpdate(tampered)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)

	if len(acks) != 1 {
		t.Fatalf("the label sent %d acknowledgements for one refused frame", len(acks))
	}
	if acks[0].Status != AckRefusedAttestation {
		t.Fatalf("an attestation refusal was reported as %v", acks[0].Status)
	}
	if acks[0].Verdict != VerdictDigestMismatch {
		t.Fatalf("the verdict is %v, want digest-mismatch", acks[0].Verdict)
	}
	if acks[0].Sequence != 1 {
		t.Fatalf("the refusal names sequence %d, want 1", acks[0].Sequence)
	}

	// A legacy frame: status 4, and it must still name the sequence so the
	// controller can correlate and stop retrying that update.
	acks = acks[:0]
	legacy, err := EncodeUpdate(Update{Sequence: 77, PriceMinor: 199, Currency: "GBP", Image: imageOf(200, 4)})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: legacy})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)

	if len(acks) != 1 {
		t.Fatalf("the label sent %d acknowledgements for one legacy frame", len(acks))
	}
	if acks[0].Status != AckRefusedUnattested {
		t.Fatalf("an unattested refusal was reported as %v; it is a configuration fault, not a compliance one",
			acks[0].Status)
	}
	if acks[0].Verdict != VerdictOK {
		t.Fatalf("an unattested refusal carries verdict %v; there was no attestation to judge", acks[0].Verdict)
	}
	if acks[0].Sequence != 77 {
		t.Fatalf("the refusal names sequence %d; the frame is decoded first precisely so it can name 77",
			acks[0].Sequence)
	}

	// An unknown key: status 3, verdict unknown-key-id, which is a stale ring
	// rather than tampering.
	acks = acks[:0]
	rogue := newAttestFixture(t)
	payload, err = EncodeAttestedUpdate(rogue.frame(t, lbl.ID(), 2, 149, imageOf(200, 5)))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	z.Net.Send(mesh.TxRequest{Dst: lbl.NodeID(), Payload: payload})
	eng.RunUntil(eng.Elapsed() + 30*time.Second)
	if len(acks) != 1 || acks[0].Status != AckRefusedAttestation || acks[0].Verdict != VerdictUnknownKeyID {
		t.Fatalf("a stale key ring was not reported as unknown-key-id: %+v", acks)
	}
	if acks[0].Verdict.Tampering() {
		t.Fatal("a missed rotation was reported as tampering")
	}
	t.Logf("the label distinguishes %v from %v without a round trip",
		VerdictDigestMismatch, VerdictUnknownKeyID)
}

package labelsim

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// ---------------------------------------------------------------------------
// The Shelf Edge Controller to label air protocol.
//
// This is the only thing that crosses the Zigbee mesh between Tier 2 and
// Tier 1, and it is deliberately tiny. Everything the cloud sends — the JSON
// envelope, the render spec, the Ed25519 attestation — stops at the controller.
// The controller verifies the attestation (INTERFACE-CONTRACTS section 5),
// renders the framebuffer, compresses it, and puts *that* on the air.
//
// The reason is arithmetic. A canon.PriceUpdated as JSON is around 700 bytes
// and its attestation alone is a 64-byte signature plus a 64-character hex
// digest; sending it to a label would cost eight 802.15.4 fragments per hop and
// buy nothing, because the label has no key ring, no clock it trusts and no
// business deciding whether a price is authorised. What the label needs is a
// sequence number to enforce monotonicity, the price to serve over NFC, and the
// pixels.
//
// The label simulator is the reference implementation of the device side of
// this protocol; the C firmware implements the same layout.
// ---------------------------------------------------------------------------

// WireVersion is the protocol version. A label that receives a frame with a
// version it does not know discards it rather than guessing, because
// misinterpreting a price frame is worse than missing one.
const WireVersion = 1

// Frame type codes.
const (
	// FrameUpdate carries a sequence number, a price and a compressed image.
	FrameUpdate = 1
	// FrameAck is the label's confirmation, carrying the timings the platform's
	// SLO is measured against.
	FrameAck = 2
	// FrameTelemetry is the label's periodic health report.
	FrameTelemetry = 3
	// FrameAttestedUpdate carries the signed tuple end to end so the label can
	// verify for itself, rather than trusting that its controller did. See
	// wire_attested.go for why it exists and what it costs.
	FrameAttestedUpdate = 4
)

// ErrNoKeyRing reports a label asked to verify with no price-authority key
// ring. It fails closed: a label that verifies nothing and displays anyway
// would make the whole apparatus decorative.
var ErrNoKeyRing = errors.New("labelsim: this label holds no price-authority key ring")

// ErrMalformedFrame reports bytes that cannot be a valid frame. A label that
// sees one has either met a corrupted mesh frame that survived its CRC or a
// controller running incompatible firmware; either way the update is dropped
// and the previous price stays on the glass.
var ErrMalformedFrame = errors.New("labelsim: malformed air frame")

// Update flag bits.
const (
	// FlagRequestPartial asks for the shortened waveform. It is a request, not
	// an instruction: the label refuses when its ghosting budget is spent.
	FlagRequestPartial = 1 << 0
	// FlagLEDPulse asks the label to pulse its locator LED, which is how a
	// picker finds one shelf edge among four hundred.
	FlagLEDPulse = 1 << 1
)

// updateHeaderBytes is the fixed portion of an update frame.
const updateHeaderBytes = 33

// Update is a price update on the air.
type Update struct {
	// Sequence is the per-label monotonic counter from
	// INTERFACE-CONTRACTS section 6. The label discards anything not strictly
	// greater than what it is displaying, which is what makes at-least-once
	// mesh delivery safe.
	Sequence int64
	// PriceMinor and Currency are carried so the label can serve the price over
	// NFC without a round trip, and so a field engineer with a reader can see
	// what the device thinks it is showing.
	PriceMinor int64
	Currency   string
	Flags      uint8
	// Template is the render template code, retained for diagnostics: it tells
	// an engineer which layout produced the image without shipping the image
	// back.
	Template uint8
	// ImageCRC is CRC-32 (Castagnoli) over the compressed image. The mesh has
	// its own frame CRC; this one covers reassembly across fragments.
	ImageCRC uint32
	// OriginX and OriginY place the image on the panel. A full refresh carries
	// the whole panel at the origin; a partial refresh carries only the changed
	// window, which is the single largest saving available on a shared 250 kbps
	// channel — a price changing from 2.49 to 1.99 moves one band of pixels, and
	// sending the band costs a fifth of what sending the panel does.
	OriginX, OriginY uint16
	// Image is the run-length-compressed framebuffer for the window.
	Image []byte
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// EncodeUpdate serialises an update for transmission.
func EncodeUpdate(u Update) ([]byte, error) {
	if len(u.Currency) != 3 {
		return nil, fmt.Errorf("labelsim: currency %q must be three characters", u.Currency)
	}
	if len(u.Image) > 0xFFFF {
		return nil, fmt.Errorf("labelsim: image of %d bytes exceeds the 65535-byte frame limit", len(u.Image))
	}
	b := make([]byte, updateHeaderBytes+len(u.Image))
	b[0] = WireVersion
	b[1] = FrameUpdate
	binary.BigEndian.PutUint64(b[2:], uint64(u.Sequence))
	binary.BigEndian.PutUint64(b[10:], uint64(u.PriceMinor))
	copy(b[18:21], u.Currency)
	b[21] = u.Flags
	b[22] = u.Template
	crc := u.ImageCRC
	if crc == 0 {
		crc = crc32.Checksum(u.Image, crcTable)
	}
	binary.BigEndian.PutUint32(b[23:], crc)
	binary.BigEndian.PutUint16(b[27:], u.OriginX)
	binary.BigEndian.PutUint16(b[29:], u.OriginY)
	binary.BigEndian.PutUint16(b[31:], uint16(len(u.Image)))
	copy(b[updateHeaderBytes:], u.Image)
	return b, nil
}

// DecodeUpdate parses an update frame, verifying the image checksum.
func DecodeUpdate(b []byte) (Update, error) {
	if len(b) < updateHeaderBytes {
		return Update{}, fmt.Errorf("%w: %d bytes is shorter than an update header", ErrMalformedFrame, len(b))
	}
	if b[0] != WireVersion {
		return Update{}, fmt.Errorf("%w: protocol version %d, this device speaks %d", ErrMalformedFrame, b[0], WireVersion)
	}
	if b[1] != FrameUpdate {
		return Update{}, fmt.Errorf("%w: frame type %d is not an update", ErrMalformedFrame, b[1])
	}
	n := int(binary.BigEndian.Uint16(b[31:]))
	if len(b) < updateHeaderBytes+n {
		return Update{}, fmt.Errorf("%w: image claims %d bytes, frame holds %d", ErrMalformedFrame, n, len(b)-updateHeaderBytes)
	}
	u := Update{
		Sequence:   int64(binary.BigEndian.Uint64(b[2:])),
		PriceMinor: int64(binary.BigEndian.Uint64(b[10:])),
		Currency:   string(b[18:21]),
		Flags:      b[21],
		Template:   b[22],
		ImageCRC:   binary.BigEndian.Uint32(b[23:]),
		OriginX:    binary.BigEndian.Uint16(b[27:]),
		OriginY:    binary.BigEndian.Uint16(b[29:]),
		Image:      append([]byte(nil), b[updateHeaderBytes:updateHeaderBytes+n]...),
	}
	if got := crc32.Checksum(u.Image, crcTable); got != u.ImageCRC {
		return Update{}, fmt.Errorf("%w: image checksum %08x, frame claims %08x", ErrMalformedFrame, got, u.ImageCRC)
	}
	return u, nil
}

// AckStatus is what the label did with an update.
//
// Codes 3 and 4 exist because without them a label's *refusal to display* is
// indistinguishable on the wire from a corrupted frame, and the two have
// opposite runbooks: a bad frame is a radio problem — check the link, retry —
// while a refusal is either a compliance incident or a fleet-configuration
// fault. Reporting the second as the first forces a controller either to infer
// compliance incidents from transport errors, raising false ones every time a
// frame is genuinely corrupted, or to miss real ones.
//
// The split between 3 and 4 matters as much as their existence. A label that
// requires end-to-end attestation and is sent a legacy frame refuses every
// price in its zone; folding that into the compliance code would bury real
// weights-and-measures incidents under a controller-version mismatch firing once
// per label per update, when the actual fault is that somebody needs to deploy
// a newer controller.
type AckStatus uint8

const (
	// AckApplied means the pixels changed and the price on the glass is the one
	// in the update.
	AckApplied AckStatus = 0
	// AckStaleSequence means the update was discarded because its sequence was
	// not greater than the one already displayed. This is the normal outcome of
	// a duplicated mesh frame and is not an error.
	AckStaleSequence AckStatus = 1
	// AckBadFrame means the frame did not decode, or its image failed the
	// reassembly checksum. A transport fault.
	AckBadFrame AckStatus = 2
	// AckRefusedAttestation means the frame decoded and the label refused to
	// display it because the attestation did not verify. The verdict says which
	// way it failed. This is a compliance incident.
	AckRefusedAttestation AckStatus = 3
	// AckRefusedUnattested means the frame decoded and the label refused it
	// because it requires an end-to-end attestation and was sent a legacy
	// type 1 frame. A fleet configuration fault, not a compliance one, and the
	// controller should stop sending this label unattested frames rather than
	// retry something that cannot ever succeed.
	AckRefusedUnattested AckStatus = 4
)

// String names the status for logs and delivery records.
//
// Anything unrecognised maps to "bad-frame", which is not laziness: the
// firmware's compatibility argument rests on it, because a controller that
// predates a future status code must degrade to today's behaviour rather than
// print a number.
func (s AckStatus) String() string {
	switch s {
	case AckApplied:
		return "applied"
	case AckStaleSequence:
		return "stale-sequence"
	case AckRefusedAttestation:
		return "refused-attestation"
	case AckRefusedUnattested:
		return "refused-unattested"
	default:
		return "bad-frame"
	}
}

// Refused reports whether the status is a deliberate refusal to display rather
// than a transport fault. It is written as a method so that no caller has to
// remember which codes those are.
func (s AckStatus) Refused() bool {
	return s == AckRefusedAttestation || s == AckRefusedUnattested
}

// AttestVerdict is why an attestation failed, carried in bits 2-4 of the ack's
// flags byte.
//
// It is the difference between two faults an operator must not confuse: a
// stale key ring, where the label missed a rotation and the fix is to
// redistribute, and a digest mismatch, where the price on the wire is not the
// price that was signed and somebody rewrote a field in flight. Carrying it in
// the ack means the distinction is available without a round trip to a label
// that may be thirty seconds from being reachable.
type AttestVerdict uint8

// The eight verdicts, matching enum usslp_attest_verdict in
// firmware/src/crypto/usslp_attest.h value for value.
const (
	// VerdictOK is carried by every ack that is not an attestation refusal.
	VerdictOK AttestVerdict = 0
	// VerdictBadAlgorithm means the attestation named an algorithm the device
	// does not implement.
	VerdictBadAlgorithm AttestVerdict = 1
	// VerdictUnknownKeyID means the key identifier is not in the label's ring.
	// Usually a missed rotation, and the fix is to the ring, not to the price.
	VerdictUnknownKeyID AttestVerdict = 2
	// VerdictKeyExpired means the key is in the ring but outside its validity
	// window.
	VerdictKeyExpired AttestVerdict = 3
	// VerdictDigestMismatch means the transmitted digest is not the digest of
	// the price the label holds. This is tampering.
	VerdictDigestMismatch AttestVerdict = 4
	// VerdictBadSignature means the digest is right and the signature over it
	// is not: a forged attestation, or a key ring that names the wrong key.
	VerdictBadSignature AttestVerdict = 5
	// VerdictMalformedPrice means the price tuple could not be canonicalised —
	// an identifier carrying an MQTT separator, say.
	VerdictMalformedPrice AttestVerdict = 6
	// VerdictCryptoUnavailable means the verifier itself could not run. It
	// fails closed.
	VerdictCryptoUnavailable AttestVerdict = 7
	// verdictMax bounds the enumeration for the compile-time width check below.
	verdictMax = VerdictCryptoUnavailable
)

// Ack flags byte layout, fixed by the firmware.
//
//	bit 0     partial refresh ran
//	bit 1     a partial was requested and a full ran anyway
//	bits 2-4  attestation verdict
//	bits 5-7  reserved, sent as zero
const (
	ackFlagPartial    = 1 << 0
	ackFlagForcedFull = 1 << 1
	ackVerdictShift   = 2
	ackVerdictMask    = 0x07
)

// The verdict has to fit in three bits. This constant is negative — and so
// fails to compile as an unsigned value — the moment a ninth verdict is added,
// which is the same protection the firmware gets from its _Static_assert. A
// verdict that silently truncated into another one would tell an operator that
// a stale key ring was tampering, or the reverse.
const _ = uint(ackVerdictMask) - uint(verdictMax)

// String names the verdict for logs, alerts and the compliance record.
func (v AttestVerdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictBadAlgorithm:
		return "unsupported-algorithm"
	case VerdictUnknownKeyID:
		return "unknown-key-id"
	case VerdictKeyExpired:
		return "key-outside-validity-window"
	case VerdictDigestMismatch:
		return "digest-mismatch"
	case VerdictBadSignature:
		return "bad-signature"
	case VerdictMalformedPrice:
		return "malformed-price-tuple"
	case VerdictCryptoUnavailable:
		return "crypto-unavailable"
	default:
		return "unknown-verdict"
	}
}

// Tampering reports whether the verdict means the price on the wire is not the
// price that was signed, as opposed to a key-management problem. It is the
// question an operator asks first and the one that decides whether the pager
// goes off.
func (v AttestVerdict) Tampering() bool {
	return v == VerdictDigestMismatch || v == VerdictBadSignature
}

// ackBytes is the fixed size of an acknowledgement frame.
const ackBytes = 20

// Ack is the label's response, carrying the measurements the platform's SLO is
// written against.
type Ack struct {
	Sequence int64
	Status   AckStatus
	// RefreshMS is how long the waveform actually took, measured by the device.
	// It is the last term of the three-second budget and the only one a
	// retailer can check by looking at a shelf.
	RefreshMS uint16
	Partial   bool
	// ForcedFull records that a partial was requested and a full refresh ran
	// anyway to clear ghosting. A controller seeing many of these is spending
	// five times the energy it budgeted.
	ForcedFull bool
	// Verdict is why an attestation failed. It is VerdictOK on every status
	// except AckRefusedAttestation.
	Verdict    AttestVerdict
	BatteryMV  uint16
	BatteryPct uint8
	// TemperatureCentiC is the on-die temperature in hundredths of a degree,
	// signed: chiller labels report well below zero.
	TemperatureCentiC int16
}

// EncodeAck serialises an acknowledgement.
func EncodeAck(a Ack) []byte {
	b := make([]byte, ackBytes)
	b[0] = WireVersion
	b[1] = FrameAck
	binary.BigEndian.PutUint64(b[2:], uint64(a.Sequence))
	b[10] = byte(a.Status)
	binary.BigEndian.PutUint16(b[11:], a.RefreshMS)
	var flags byte
	if a.Partial {
		flags |= ackFlagPartial
	}
	if a.ForcedFull {
		flags |= ackFlagForcedFull
	}
	flags |= byte(a.Verdict&ackVerdictMask) << ackVerdictShift
	b[13] = flags
	binary.BigEndian.PutUint16(b[14:], a.BatteryMV)
	b[16] = a.BatteryPct
	binary.BigEndian.PutUint16(b[17:], uint16(a.TemperatureCentiC))
	b[19] = FrameAck
	return b
}

// DecodeAck parses an acknowledgement frame.
func DecodeAck(b []byte) (Ack, error) {
	if len(b) < ackBytes || b[0] != WireVersion || b[1] != FrameAck {
		return Ack{}, fmt.Errorf("%w: not an acknowledgement", ErrMalformedFrame)
	}
	flags := b[13]
	return Ack{
		Sequence:          int64(binary.BigEndian.Uint64(b[2:])),
		Status:            AckStatus(b[10]),
		RefreshMS:         binary.BigEndian.Uint16(b[11:]),
		Partial:           flags&ackFlagPartial != 0,
		ForcedFull:        flags&ackFlagForcedFull != 0,
		Verdict:           AttestVerdict(flags >> ackVerdictShift & ackVerdictMask),
		BatteryMV:         binary.BigEndian.Uint16(b[14:]),
		BatteryPct:        b[16],
		TemperatureCentiC: int16(binary.BigEndian.Uint16(b[17:])),
	}, nil
}

// telemetryBytes is the fixed size of a telemetry frame.
const telemetryBytes = 24

// TelemetryFrame is the label's periodic health report, sent uplink on its own
// cadence rather than polled. Polling 40,000 labels would cost the zone's whole
// channel; letting each label speak once every five minutes costs under one per
// cent of it.
type TelemetryFrame struct {
	BatteryMV         uint16
	BatteryPct        uint8
	TemperatureCentiC int16
	// ParentLQI and ParentRSSI are what the label measured on its own uplink,
	// which is the half of the mesh picture the controller cannot see for
	// itself.
	ParentLQI    uint8
	ParentRSSI   int8
	RefreshCount uint32
	NFCTapCount  uint32
	UptimeSec    uint32
	Tamper       bool
}

// EncodeTelemetry serialises a telemetry frame.
func EncodeTelemetry(t TelemetryFrame) []byte {
	b := make([]byte, telemetryBytes)
	b[0] = WireVersion
	b[1] = FrameTelemetry
	binary.BigEndian.PutUint16(b[2:], t.BatteryMV)
	b[4] = t.BatteryPct
	binary.BigEndian.PutUint16(b[5:], uint16(t.TemperatureCentiC))
	b[7] = t.ParentLQI
	b[8] = byte(t.ParentRSSI)
	binary.BigEndian.PutUint32(b[9:], t.RefreshCount)
	binary.BigEndian.PutUint32(b[13:], t.NFCTapCount)
	binary.BigEndian.PutUint32(b[17:], t.UptimeSec)
	if t.Tamper {
		b[21] = 1
	}
	return b
}

// DecodeTelemetry parses a telemetry frame.
func DecodeTelemetry(b []byte) (TelemetryFrame, error) {
	if len(b) < telemetryBytes || b[0] != WireVersion || b[1] != FrameTelemetry {
		return TelemetryFrame{}, fmt.Errorf("%w: not a telemetry report", ErrMalformedFrame)
	}
	return TelemetryFrame{
		BatteryMV:         binary.BigEndian.Uint16(b[2:]),
		BatteryPct:        b[4],
		TemperatureCentiC: int16(binary.BigEndian.Uint16(b[5:])),
		ParentLQI:         b[7],
		ParentRSSI:        int8(b[8]),
		RefreshCount:      binary.BigEndian.Uint32(b[9:]),
		NFCTapCount:       binary.BigEndian.Uint32(b[13:]),
		UptimeSec:         binary.BigEndian.Uint32(b[17:]),
		Tamper:            b[21] != 0,
	}, nil
}

// FrameKind reports the type byte of a frame without decoding it, so a receiver
// can dispatch cheaply.
func FrameKind(b []byte) (uint8, bool) {
	if len(b) < 2 || b[0] != WireVersion {
		return 0, false
	}
	return b[1], true
}

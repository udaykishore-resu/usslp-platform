package domain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// PolicyPack is the Tier-1 rule set for a whole store, in the form the Store
// Gateway Unit embeds.
//
// # Why a bespoke binary format
//
// The SGU is a fanless box with 2 GB of RAM that must hold the rules for every
// SKU in the store and evaluate them with the WAN down. JSON for 40,000 SKUs is
// tens of megabytes and costs a parse on every restart; this encoding is a
// fixed 56-byte record per SKU plus its identifier, memory-mappable in
// principle and decodable in one linear pass with no per-record allocation
// beyond the string. It is versioned and CRC-checked because a policy pack that
// silently truncates on a half-written flash write would leave a store pricing
// against a partial rule set — which is exactly the failure the offline brain
// exists to prevent.
type PolicyPack struct {
	// Tenant and Store scope the pack. The SGU refuses a pack that is not its
	// own; a mis-routed pack would apply another retailer's margins.
	Tenant canon.TenantID
	Store  canon.StoreID
	// Version is the monotonic pack version. The SGU keeps the highest it has
	// seen so that an out-of-order delivery after a reconnect cannot roll the
	// rules backwards.
	Version int64
	// Rules is the per-SKU rule set.
	Rules map[canon.SKU]Constraints
}

// packMagic identifies the format. It is checked before anything else is
// trusted.
var packMagic = [4]byte{'U', 'S', 'P', 'P'}

// packFormatVersion is the on-disk layout version. A reader must refuse a
// version it does not know rather than guess at field offsets.
const packFormatVersion uint16 = 1

// ErrPolicyPack marks a malformed or untrusted pack.
var ErrPolicyPack = errors.New("pricing: malformed policy pack")

// MarshalBinary encodes the pack.
//
// Records are emitted in SKU order so that the encoding is deterministic: two
// encodings of the same pack are byte-identical, which lets the SGU skip a
// reload by comparing checksums and lets the platform prove in an audit that
// the rules a store applied are the rules it was sent.
func (p PolicyPack) MarshalBinary() ([]byte, error) {
	skus := make([]canon.SKU, 0, len(p.Rules))
	for sku := range p.Rules {
		if sku == "" {
			return nil, fmt.Errorf("%w: empty sku", ErrPolicyPack)
		}
		skus = append(skus, sku)
	}
	sort.Slice(skus, func(i, j int) bool { return skus[i] < skus[j] })

	buf := make([]byte, 0, 64+len(skus)*72)
	buf = append(buf, packMagic[:]...)
	buf = binary.LittleEndian.AppendUint16(buf, packFormatVersion)
	buf = appendString(buf, string(p.Tenant))
	buf = appendString(buf, string(p.Store))
	buf = binary.LittleEndian.AppendUint64(buf, uint64(p.Version))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(skus)))

	for _, sku := range skus {
		c := p.Rules[sku]
		buf = appendString(buf, string(sku))
		buf = appendString(buf, c.Currency)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(c.UnitCost))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(c.FloorMinor))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(c.CeilingMinor))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(c.CompetitorMinor))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(c.CurrentMinor))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(c.GranularityMinor))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(c.MinMarginBps))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(c.CompetitorBandBps))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(c.MaxChangeBps))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(c.MaxChangesPerPeriod))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(c.ChangesThisPeriod))
		buf = append(buf, byte(c.Ending))
	}

	sum := crc32.ChecksumIEEE(buf)
	return binary.LittleEndian.AppendUint32(buf, sum), nil
}

// UnmarshalBinary decodes a pack, verifying magic, version and checksum before
// trusting a single field.
func (p *PolicyPack) UnmarshalBinary(b []byte) error {
	if len(b) < 4+2+4 {
		return fmt.Errorf("%w: %d bytes is too short for a header", ErrPolicyPack, len(b))
	}
	if b[0] != packMagic[0] || b[1] != packMagic[1] || b[2] != packMagic[2] || b[3] != packMagic[3] {
		return fmt.Errorf("%w: bad magic", ErrPolicyPack)
	}
	body, want := b[:len(b)-4], binary.LittleEndian.Uint32(b[len(b)-4:])
	if got := crc32.ChecksumIEEE(body); got != want {
		return fmt.Errorf("%w: checksum %08x does not match %08x", ErrPolicyPack, got, want)
	}
	r := &reader{b: body[4:]}
	ver := r.u16()
	if ver != packFormatVersion {
		return fmt.Errorf("%w: format version %d is not supported", ErrPolicyPack, ver)
	}
	p.Tenant = canon.TenantID(r.str())
	p.Store = canon.StoreID(r.str())
	p.Version = int64(r.u64())
	n := int(r.u32())
	if r.err != nil {
		return r.err
	}
	// A count field is attacker-influenced input on the SGU's control channel.
	// Bounding the allocation against the bytes actually present stops a forged
	// header from asking for a terabyte map.
	if n < 0 || n > len(body) {
		return fmt.Errorf("%w: record count %d exceeds the encoded size", ErrPolicyPack, n)
	}
	p.Rules = make(map[canon.SKU]Constraints, n)
	for i := 0; i < n; i++ {
		sku := canon.SKU(r.str())
		var c Constraints
		c.Currency = r.str()
		c.UnitCost = int64(r.u64())
		c.FloorMinor = int64(r.u64())
		c.CeilingMinor = int64(r.u64())
		c.CompetitorMinor = int64(r.u64())
		c.CurrentMinor = int64(r.u64())
		c.GranularityMinor = int64(r.u64())
		c.MinMarginBps = int32(r.u32())
		c.CompetitorBandBps = int32(r.u32())
		c.MaxChangeBps = int32(r.u32())
		c.MaxChangesPerPeriod = int32(r.u32())
		c.ChangesThisPeriod = int32(r.u32())
		c.Ending = Ending(r.u8())
		if r.err != nil {
			return r.err
		}
		p.Rules[sku] = c
	}
	if len(r.b) != 0 {
		return fmt.Errorf("%w: %d trailing bytes", ErrPolicyPack, len(r.b))
	}
	return nil
}

// Evaluate applies the pack's rules for one SKU. A SKU with no rule is
// permitted unchanged: the pack is a constraint list, not an allow-list, and an
// SGU must not refuse to price a product the cloud simply has no rules for.
func (p PolicyPack) Evaluate(sku canon.SKU, requestedMinor int64, fallbackCurrency string) Decision {
	c, ok := p.Rules[sku]
	if !ok {
		return Decision{
			Outcome:        OutcomeAccepted,
			Price:          canon.NewMoney(requestedMinor, fallbackCurrency),
			RequestedMinor: requestedMinor,
			Feasible:       Range{LowMinor: 0, HighMinor: maxPriceMinor},
		}
	}
	return Evaluate(c, requestedMinor)
}

func appendString(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

// reader is a bounds-checked cursor. Every accessor is a no-op once an error
// has been recorded, so a truncated pack produces one clear error instead of a
// panic in whichever field happened to run off the end.
type reader struct {
	b   []byte
	err error
}

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: "+format, append([]any{ErrPolicyPack}, args...)...)
	}
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if len(r.b) < n {
		r.fail("truncated: wanted %d bytes, have %d", n, len(r.b))
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) u8() byte {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *reader) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *reader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *reader) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (r *reader) str() string {
	n := int(r.u16())
	b := r.take(n)
	if b == nil {
		return ""
	}
	return string(b)
}

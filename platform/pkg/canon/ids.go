// Package canon defines the canonical domain vocabulary of the Universal Smart
// Shelf Label Platform (USSLP): identifiers, money, events, and the topic
// namespaces that every tier of the system — cloud, store gateway, shelf
// controller and label — agrees on.
//
// Nothing in this package may import another USSLP package. It is the shared
// kernel: the one contract that the Universal Integration Gateway, the cloud
// microservices, the edge tier and the device simulators all compile against.
package canon

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Typed identifiers
//
// Every identifier in USSLP is a distinct Go type. A StoreID can never be
// silently passed where a LabelID is expected, which removes an entire class of
// bug from a system whose hot path fans one message out to millions of devices.
// ---------------------------------------------------------------------------

type (
	// TenantID identifies a retail customer of the platform. It is the root of
	// every isolation boundary: database rows, Kafka keys, MQTT namespaces and
	// certificate subjects are all tenant-scoped.
	TenantID string
	// StoreID identifies a single physical retail location.
	StoreID string
	// LabelID identifies one smart shelf label (Tier 1 device).
	LabelID string
	// SECID identifies a Shelf Edge Controller (Tier 2 device).
	SECID string
	// SGUID identifies a Store Gateway Unit (Tier 3 device).
	SGUID string
	// SKU identifies a product (stock keeping unit) within a tenant.
	SKU string
	// PromotionID identifies a promotion rule.
	PromotionID string
	// EventID uniquely identifies one immutable event in the event store.
	EventID string
	// CorrelationID ties every event produced while servicing one external
	// request together, from POS webhook to label ACK.
	CorrelationID string
	// Region is a geographic shard: us-east-1, eu-west-1, ap-south-1.
	Region string
)

func (t TenantID) String() string      { return string(t) }
func (s StoreID) String() string       { return string(s) }
func (l LabelID) String() string       { return string(l) }
func (s SECID) String() string         { return string(s) }
func (s SGUID) String() string         { return string(s) }
func (s SKU) String() string           { return string(s) }
func (p PromotionID) String() string   { return string(p) }
func (e EventID) String() string       { return string(e) }
func (c CorrelationID) String() string { return string(c) }
func (r Region) String() string        { return string(r) }

// Valid reports whether the identifier is non-empty and free of the separator
// characters used by the MQTT and Kafka key namespaces. Identifiers arriving
// from a POS system are attacker-adjacent input: a '/' or '#' in a store ID
// would let a tenant subscribe outside its own namespace.
func ValidID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	if strings.ContainsAny(s, "/#+ \t\n\r\x00:") {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Identifier generation
// ---------------------------------------------------------------------------

// NewUUID returns a RFC 4122 version 4 UUID. Used for event IDs and any
// identifier that must be unguessable.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable; a platform that cannot produce
		// unique event IDs must not continue writing to the event store.
		panic("canon: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(b)
}

// NewULID returns a lexicographically sortable, time-prefixed 128-bit
// identifier rendered as a UUID (a "UUIDv7" in all but name). Sortability
// matters for the event store: appending monotonically increasing keys keeps
// B-tree inserts at the right edge of the index instead of scattering them.
func NewULID() string {
	var b [16]byte
	ms := uint64(time.Now().UTC().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		panic("canon: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// NewEventID returns a sortable event identifier.
func NewEventID() EventID { return EventID(NewULID()) }

// NewCorrelationID returns a fresh correlation identifier for an inbound
// request that did not carry one.
func NewCorrelationID() CorrelationID { return CorrelationID(NewULID()) }

// NewTraceID returns a 16-byte W3C trace identifier rendered as 32 hex chars.
func NewTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("canon: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// NewSpanID returns an 8-byte W3C span identifier rendered as 16 hex chars.
func NewSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("canon: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// DeviceSerial derives the deterministic factory serial printed on a label from
// its 64-bit IEEE 802.15.4 extended address. The serial is what a technician
// scans; the EUI-64 is what the Zigbee mesh addresses.
func DeviceSerial(eui64 uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], eui64)
	return fmt.Sprintf("USSLP-%s", strings.ToUpper(hex.EncodeToString(b[:])))
}

// Package mqtt implements the MQTT 3.1.1 control protocol — both a broker and a
// client — over TCP and TLS, from the specification, using only the standard
// library.
//
// USSLP needs its own implementation rather than a driver for an external
// broker because the Store Gateway Unit must keep every shelf label updating
// with the WAN cut: the SGU embeds this Broker so a store is a self-contained
// messaging domain, and the same Client speaks to it locally and to EMQX in the
// cloud. Having one code path for both means the wire behaviour a store depends
// on during an outage is the behaviour exercised by every test in the repo.
//
// The three entry points are Broker (NewBroker, ListenAndServe, Shutdown),
// Client (Dial), which satisfies msgbus.Client, and Authorizer, the hook that
// makes tenant isolation a property of the broker rather than of its callers.
package mqtt

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/usslp/usslp/platform/pkg/msgbus"
)

// Wire-level failures are separated from transport failures so a connection
// handler can tell "this peer is broken" (close it, and say why in the log)
// from "the socket went away" (routine, not worth a warning).
var (
	// ErrMalformedPacket reports bytes that cannot be a valid MQTT packet at
	// all: a bad remaining-length varint, a truncated field, a string that is
	// not valid MQTT UTF-8. A peer that sends one is disconnected immediately,
	// because after a framing error the stream position is unknowable.
	ErrMalformedPacket = errors.New("mqtt: malformed packet")
	// ErrProtocolViolation reports a well-formed packet that breaks a rule of
	// MQTT 3.1.1 — a reserved flag set, QoS 3, a SUBSCRIBE with no filters.
	// Also grounds for disconnection, but the stream itself is still framed.
	ErrProtocolViolation = errors.New("mqtt: protocol violation")
	// ErrPacketTooLarge reports a packet larger than the configured limit. The
	// limit exists because remaining length is decoded before authentication:
	// without it an unauthenticated peer could make the broker allocate 256MB.
	ErrPacketTooLarge = errors.New("mqtt: packet exceeds maximum size")
)

// maxRemainingLength is the largest value the 4-byte remaining-length varint
// can express (2^28-1), and therefore the hard ceiling on any MQTT packet body.
const maxRemainingLength = 268435455

// packetType is the 4-bit MQTT control packet type.
type packetType byte

// The MQTT 3.1.1 control packet types. Values are the wire values, so
// byte(typ)<<4 is the high nibble of the fixed header.
const (
	pktCONNECT     packetType = 1
	pktCONNACK     packetType = 2
	pktPUBLISH     packetType = 3
	pktPUBACK      packetType = 4
	pktPUBREC      packetType = 5
	pktPUBREL      packetType = 6
	pktPUBCOMP     packetType = 7
	pktSUBSCRIBE   packetType = 8
	pktSUBACK      packetType = 9
	pktUNSUBSCRIBE packetType = 10
	pktUNSUBACK    packetType = 11
	pktPINGREQ     packetType = 12
	pktPINGRESP    packetType = 13
	pktDISCONNECT  packetType = 14
)

// String names the packet type for logs and protocol errors. Unknown values
// are rendered rather than hidden, because a reserved type in a log line is the
// evidence that a peer is not speaking MQTT at all.
func (t packetType) String() string {
	switch t {
	case pktCONNECT:
		return "CONNECT"
	case pktCONNACK:
		return "CONNACK"
	case pktPUBLISH:
		return "PUBLISH"
	case pktPUBACK:
		return "PUBACK"
	case pktPUBREC:
		return "PUBREC"
	case pktPUBREL:
		return "PUBREL"
	case pktPUBCOMP:
		return "PUBCOMP"
	case pktSUBSCRIBE:
		return "SUBSCRIBE"
	case pktSUBACK:
		return "SUBACK"
	case pktUNSUBSCRIBE:
		return "UNSUBSCRIBE"
	case pktUNSUBACK:
		return "UNSUBACK"
	case pktPINGREQ:
		return "PINGREQ"
	case pktPINGRESP:
		return "PINGRESP"
	case pktDISCONNECT:
		return "DISCONNECT"
	default:
		return "UNKNOWN(" + strconv.Itoa(int(t)) + ")"
	}
}

// packet is one MQTT control packet in either direction. Encoding is split into
// header flags and body so that writePacket can compute the remaining-length
// varint from the finished body, which is the only way to get it right.
type packet interface {
	pktType() packetType
	// headerFlags returns the low nibble of the fixed header. It is non-zero
	// only for PUBLISH (dup/qos/retain) and for the three packets the spec
	// fixes at 0b0010 (PUBREL, SUBSCRIBE, UNSUBSCRIBE).
	headerFlags() byte
	encodeBody(*bytes.Buffer) error
}

// ---------------------------------------------------------------------------
// Primitive framing
// ---------------------------------------------------------------------------

// encodeVarint writes an MQTT remaining-length varint into dst, returning the
// number of bytes used (1-4). dst must have room for 4.
func encodeVarint(dst []byte, n int) int {
	i := 0
	for {
		digit := byte(n % 128)
		n /= 128
		if n > 0 {
			digit |= 0x80
		}
		dst[i] = digit
		i++
		if n == 0 {
			return i
		}
	}
}

// readVarint decodes a remaining-length varint. A fifth continuation byte is
// malformed rather than merely large: the spec caps the field at four bytes, and
// accepting more would let a peer stall the parser indefinitely.
func readVarint(r io.ByteReader) (int, error) {
	multiplier := 1
	value := 0
	for i := 0; i < 4; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value += int(b&0x7f) * multiplier
		if b&0x80 == 0 {
			return value, nil
		}
		multiplier *= 128
	}
	return 0, fmt.Errorf("%w: remaining length exceeds four bytes", ErrMalformedPacket)
}

// validUTF8 enforces the MQTT string rules: valid UTF-8, and no U+0000. The
// null check matters because topic names cross into log lines and ACL
// comparisons where an embedded null could truncate a string in a C consumer.
func validUTF8(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsRune(s, 0)
}

func writeString(b *bytes.Buffer, s string) error {
	if len(s) > 65535 {
		return fmt.Errorf("%w: string of %d bytes exceeds the 65535-byte field", ErrPacketTooLarge, len(s))
	}
	if !validUTF8(s) {
		return fmt.Errorf("%w: string is not valid MQTT UTF-8", ErrMalformedPacket)
	}
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(s)))
	b.Write(l[:])
	b.WriteString(s)
	return nil
}

func writeBytes(b *bytes.Buffer, p []byte) error {
	if len(p) > 65535 {
		return fmt.Errorf("%w: binary field of %d bytes exceeds 65535", ErrPacketTooLarge, len(p))
	}
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(p)))
	b.Write(l[:])
	b.Write(p)
	return nil
}

func writeUint16(b *bytes.Buffer, v uint16) {
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], v)
	b.Write(l[:])
}

// bodyReader walks an already-framed packet body. Working on a complete []byte
// rather than streaming off the socket means a truncated field is detected as a
// malformed packet instead of a read that blocks until the keepalive fires.
type bodyReader struct {
	b []byte
	i int
}

func (r *bodyReader) remaining() int { return len(r.b) - r.i }

func (r *bodyReader) readByte() (byte, error) {
	if r.remaining() < 1 {
		return 0, fmt.Errorf("%w: truncated byte field", ErrMalformedPacket)
	}
	v := r.b[r.i]
	r.i++
	return v, nil
}

func (r *bodyReader) readUint16() (uint16, error) {
	if r.remaining() < 2 {
		return 0, fmt.Errorf("%w: truncated 16-bit field", ErrMalformedPacket)
	}
	v := binary.BigEndian.Uint16(r.b[r.i:])
	r.i += 2
	return v, nil
}

func (r *bodyReader) readRaw() ([]byte, error) {
	n, err := r.readUint16()
	if err != nil {
		return nil, err
	}
	if r.remaining() < int(n) {
		return nil, fmt.Errorf("%w: length-prefixed field claims %d bytes, %d remain", ErrMalformedPacket, n, r.remaining())
	}
	// Copy: the body buffer is reused by nothing today, but retained messages
	// and offline queues outlive the packet, and aliasing them to a decode
	// buffer is the kind of bug that only shows up under load.
	out := make([]byte, n)
	copy(out, r.b[r.i:r.i+int(n)])
	r.i += int(n)
	return out, nil
}

func (r *bodyReader) readString() (string, error) {
	raw, err := r.readRaw()
	if err != nil {
		return "", err
	}
	s := string(raw)
	if !validUTF8(s) {
		return "", fmt.Errorf("%w: string field is not valid MQTT UTF-8", ErrMalformedPacket)
	}
	return s, nil
}

// rest consumes and returns everything left, which is how PUBLISH payloads are
// delimited: MQTT gives them no length prefix of their own.
func (r *bodyReader) rest() []byte {
	out := make([]byte, r.remaining())
	copy(out, r.b[r.i:])
	r.i = len(r.b)
	return out
}

// ---------------------------------------------------------------------------
// CONNECT / CONNACK
// ---------------------------------------------------------------------------

// ConnectReturnCode is the CONNACK result byte. The values are the wire values
// and are surfaced on ConnectError so a device that is rejected can tell "my
// clock is wrong / my firmware is old" (unacceptable protocol version) from
// "my certificate was revoked" (not authorized) without parsing a log.
type ConnectReturnCode byte

// CONNACK return codes from MQTT 3.1.1 section 3.2.2.3.
const (
	ConnectAccepted           ConnectReturnCode = 0
	ConnectUnacceptableProto  ConnectReturnCode = 1
	ConnectIdentifierRejected ConnectReturnCode = 2
	ConnectServerUnavailable  ConnectReturnCode = 3
	ConnectBadCredentials     ConnectReturnCode = 4
	ConnectNotAuthorized      ConnectReturnCode = 5
)

// String explains the refusal in prose. These strings reach device logs and
// operator dashboards, where "bad username or password" is actionable and
// "0x04" is not.
func (c ConnectReturnCode) String() string {
	switch c {
	case ConnectAccepted:
		return "accepted"
	case ConnectUnacceptableProto:
		return "unacceptable protocol version"
	case ConnectIdentifierRejected:
		return "identifier rejected"
	case ConnectServerUnavailable:
		return "server unavailable"
	case ConnectBadCredentials:
		return "bad username or password"
	case ConnectNotAuthorized:
		return "not authorized"
	default:
		return "unknown return code " + strconv.Itoa(int(c))
	}
}

// protocolName and protocolLevel identify MQTT 3.1.1 on the wire.
const (
	protocolName  = "MQTT"
	protocolLevel = byte(4)
)

type connectPacket struct {
	ProtocolName  string
	ProtocolLevel byte
	CleanSession  bool
	WillFlag      bool
	WillQoS       msgbus.QoS
	WillRetain    bool
	HasUsername   bool
	HasPassword   bool
	KeepAlive     uint16
	ClientID      string
	WillTopic     string
	WillMessage   []byte
	Username      string
	Password      []byte
}

func (p *connectPacket) pktType() packetType { return pktCONNECT }
func (p *connectPacket) headerFlags() byte   { return 0 }

func (p *connectPacket) encodeBody(b *bytes.Buffer) error {
	// The protocol name and level are encoded from the packet, not from the
	// constants: a broker has to be exercised against the CONNECT an old device
	// sends ("MQIsdp", level 3) as well as the one this client sends.
	if err := writeString(b, p.ProtocolName); err != nil {
		return err
	}
	b.WriteByte(p.ProtocolLevel)
	var flags byte
	if p.CleanSession {
		flags |= 0x02
	}
	if p.WillFlag {
		flags |= 0x04
		flags |= byte(p.WillQoS) << 3
		if p.WillRetain {
			flags |= 0x20
		}
	}
	if p.HasPassword {
		flags |= 0x40
	}
	if p.HasUsername {
		flags |= 0x80
	}
	b.WriteByte(flags)
	writeUint16(b, p.KeepAlive)
	if err := writeString(b, p.ClientID); err != nil {
		return err
	}
	if p.WillFlag {
		if err := writeString(b, p.WillTopic); err != nil {
			return err
		}
		if err := writeBytes(b, p.WillMessage); err != nil {
			return err
		}
	}
	if p.HasUsername {
		if err := writeString(b, p.Username); err != nil {
			return err
		}
	}
	if p.HasPassword {
		if err := writeBytes(b, p.Password); err != nil {
			return err
		}
	}
	return nil
}

func decodeConnect(r *bodyReader) (*connectPacket, error) {
	p := &connectPacket{}
	var err error
	if p.ProtocolName, err = r.readString(); err != nil {
		return nil, err
	}
	if p.ProtocolLevel, err = r.readByte(); err != nil {
		return nil, err
	}
	flags, err := r.readByte()
	if err != nil {
		return nil, err
	}
	// Bit 0 is reserved and MUST be zero; a set bit means the peer is speaking
	// something that is not MQTT 3.1.1 and further parsing is guesswork.
	if flags&0x01 != 0 {
		return nil, fmt.Errorf("%w: CONNECT reserved flag set", ErrProtocolViolation)
	}
	p.CleanSession = flags&0x02 != 0
	p.WillFlag = flags&0x04 != 0
	p.WillQoS = msgbus.QoS((flags >> 3) & 0x03)
	p.WillRetain = flags&0x20 != 0
	p.HasPassword = flags&0x40 != 0
	p.HasUsername = flags&0x80 != 0
	if p.WillFlag {
		if p.WillQoS > msgbus.ExactlyOnce {
			return nil, fmt.Errorf("%w: will QoS 3", ErrProtocolViolation)
		}
	} else if p.WillQoS != 0 || p.WillRetain {
		return nil, fmt.Errorf("%w: will QoS/retain set without a will", ErrProtocolViolation)
	}
	if p.HasPassword && !p.HasUsername {
		return nil, fmt.Errorf("%w: password without username", ErrProtocolViolation)
	}
	if p.KeepAlive, err = r.readUint16(); err != nil {
		return nil, err
	}
	if p.ClientID, err = r.readString(); err != nil {
		return nil, err
	}
	if p.WillFlag {
		if p.WillTopic, err = r.readString(); err != nil {
			return nil, err
		}
		if p.WillMessage, err = r.readRaw(); err != nil {
			return nil, err
		}
	}
	if p.HasUsername {
		if p.Username, err = r.readString(); err != nil {
			return nil, err
		}
	}
	if p.HasPassword {
		if p.Password, err = r.readRaw(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

type connackPacket struct {
	// SessionPresent tells a reconnecting client whether the broker still holds
	// its subscriptions and in-flight messages. A SEC that gets false knows it
	// must re-subscribe and re-request its zone's retained state.
	SessionPresent bool
	ReturnCode     ConnectReturnCode
}

func (p *connackPacket) pktType() packetType { return pktCONNACK }
func (p *connackPacket) headerFlags() byte   { return 0 }

func (p *connackPacket) encodeBody(b *bytes.Buffer) error {
	var ack byte
	if p.SessionPresent {
		ack = 1
	}
	b.WriteByte(ack)
	b.WriteByte(byte(p.ReturnCode))
	return nil
}

func decodeConnack(r *bodyReader) (*connackPacket, error) {
	ack, err := r.readByte()
	if err != nil {
		return nil, err
	}
	if ack&0xfe != 0 {
		return nil, fmt.Errorf("%w: CONNACK reserved acknowledge flags set", ErrProtocolViolation)
	}
	code, err := r.readByte()
	if err != nil {
		return nil, err
	}
	return &connackPacket{SessionPresent: ack&0x01 != 0, ReturnCode: ConnectReturnCode(code)}, nil
}

// ---------------------------------------------------------------------------
// PUBLISH and the acknowledgement family
// ---------------------------------------------------------------------------

type publishPacket struct {
	Dup      bool
	QoS      msgbus.QoS
	Retain   bool
	Topic    string
	PacketID uint16
	Payload  []byte
}

func (p *publishPacket) pktType() packetType { return pktPUBLISH }

func (p *publishPacket) headerFlags() byte {
	var f byte
	if p.Dup {
		f |= 0x08
	}
	f |= byte(p.QoS) << 1
	if p.Retain {
		f |= 0x01
	}
	return f
}

func (p *publishPacket) encodeBody(b *bytes.Buffer) error {
	if err := writeString(b, p.Topic); err != nil {
		return err
	}
	if p.QoS > msgbus.AtMostOnce {
		if p.PacketID == 0 {
			return fmt.Errorf("%w: QoS %d PUBLISH with packet identifier 0", ErrProtocolViolation, p.QoS)
		}
		writeUint16(b, p.PacketID)
	}
	b.Write(p.Payload)
	return nil
}

func decodePublish(flags byte, r *bodyReader) (*publishPacket, error) {
	p := &publishPacket{
		Dup:    flags&0x08 != 0,
		QoS:    msgbus.QoS((flags >> 1) & 0x03),
		Retain: flags&0x01 != 0,
	}
	if p.QoS > msgbus.ExactlyOnce {
		return nil, fmt.Errorf("%w: PUBLISH with QoS 3", ErrProtocolViolation)
	}
	if p.QoS == msgbus.AtMostOnce && p.Dup {
		return nil, fmt.Errorf("%w: DUP set on a QoS 0 PUBLISH", ErrProtocolViolation)
	}
	var err error
	if p.Topic, err = r.readString(); err != nil {
		return nil, err
	}
	if p.QoS > msgbus.AtMostOnce {
		if p.PacketID, err = r.readUint16(); err != nil {
			return nil, err
		}
		if p.PacketID == 0 {
			return nil, fmt.Errorf("%w: PUBLISH with packet identifier 0", ErrProtocolViolation)
		}
	}
	p.Payload = r.rest()
	return p, nil
}

// ackPacket carries PUBACK, PUBREC, PUBREL, PUBCOMP and UNSUBACK, which share a
// body of exactly one packet identifier. One type keeps the QoS 2 state machine
// readable instead of spreading four identical structs across the file.
type ackPacket struct {
	Type     packetType
	PacketID uint16
}

func (p *ackPacket) pktType() packetType { return p.Type }

func (p *ackPacket) headerFlags() byte {
	// PUBREL is one of the three packets the spec fixes at 0b0010; brokers are
	// required to reject it otherwise, so encoders must get it right.
	if p.Type == pktPUBREL {
		return 0x02
	}
	return 0
}

func (p *ackPacket) encodeBody(b *bytes.Buffer) error {
	if p.PacketID == 0 {
		return fmt.Errorf("%w: %s with packet identifier 0", ErrProtocolViolation, p.Type)
	}
	writeUint16(b, p.PacketID)
	return nil
}

func decodeAck(typ packetType, r *bodyReader) (*ackPacket, error) {
	id, err := r.readUint16()
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, fmt.Errorf("%w: %s with packet identifier 0", ErrProtocolViolation, typ)
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("%w: %s with %d trailing bytes", ErrMalformedPacket, typ, r.remaining())
	}
	return &ackPacket{Type: typ, PacketID: id}, nil
}

// ---------------------------------------------------------------------------
// SUBSCRIBE / SUBACK / UNSUBSCRIBE / UNSUBACK
// ---------------------------------------------------------------------------

// topicFilter is one entry of a SUBSCRIBE: what to match and the maximum QoS
// the subscriber will accept.
type topicFilter struct {
	Filter string
	QoS    msgbus.QoS
}

type subscribePacket struct {
	PacketID uint16
	Filters  []topicFilter
}

func (p *subscribePacket) pktType() packetType { return pktSUBSCRIBE }
func (p *subscribePacket) headerFlags() byte   { return 0x02 }

func (p *subscribePacket) encodeBody(b *bytes.Buffer) error {
	if len(p.Filters) == 0 {
		return fmt.Errorf("%w: SUBSCRIBE with no filters", ErrProtocolViolation)
	}
	if p.PacketID == 0 {
		return fmt.Errorf("%w: SUBSCRIBE with packet identifier 0", ErrProtocolViolation)
	}
	writeUint16(b, p.PacketID)
	for _, f := range p.Filters {
		if err := writeString(b, f.Filter); err != nil {
			return err
		}
		b.WriteByte(byte(f.QoS))
	}
	return nil
}

func decodeSubscribe(r *bodyReader) (*subscribePacket, error) {
	id, err := r.readUint16()
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, fmt.Errorf("%w: SUBSCRIBE with packet identifier 0", ErrProtocolViolation)
	}
	p := &subscribePacket{PacketID: id}
	for r.remaining() > 0 {
		f, err := r.readString()
		if err != nil {
			return nil, err
		}
		q, err := r.readByte()
		if err != nil {
			return nil, err
		}
		if q > byte(msgbus.ExactlyOnce) {
			return nil, fmt.Errorf("%w: SUBSCRIBE requested QoS %d", ErrProtocolViolation, q)
		}
		p.Filters = append(p.Filters, topicFilter{Filter: f, QoS: msgbus.QoS(q)})
	}
	if len(p.Filters) == 0 {
		return nil, fmt.Errorf("%w: SUBSCRIBE with no filters", ErrProtocolViolation)
	}
	return p, nil
}

// subackFailure is the SUBACK return code for a filter the broker refused —
// how a cross-tenant subscribe is reported without killing the connection.
const subackFailure byte = 0x80

type subackPacket struct {
	PacketID uint16
	// Codes is one byte per requested filter, in request order: the granted QoS
	// (which may be lower than requested) or subackFailure.
	Codes []byte
}

func (p *subackPacket) pktType() packetType { return pktSUBACK }
func (p *subackPacket) headerFlags() byte   { return 0 }

func (p *subackPacket) encodeBody(b *bytes.Buffer) error {
	if p.PacketID == 0 {
		return fmt.Errorf("%w: SUBACK with packet identifier 0", ErrProtocolViolation)
	}
	writeUint16(b, p.PacketID)
	b.Write(p.Codes)
	return nil
}

func decodeSuback(r *bodyReader) (*subackPacket, error) {
	id, err := r.readUint16()
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, fmt.Errorf("%w: SUBACK with packet identifier 0", ErrProtocolViolation)
	}
	codes := r.rest()
	if len(codes) == 0 {
		return nil, fmt.Errorf("%w: SUBACK with no return codes", ErrProtocolViolation)
	}
	for _, c := range codes {
		if c > byte(msgbus.ExactlyOnce) && c != subackFailure {
			return nil, fmt.Errorf("%w: SUBACK return code 0x%02x", ErrProtocolViolation, c)
		}
	}
	return &subackPacket{PacketID: id, Codes: codes}, nil
}

type unsubscribePacket struct {
	PacketID uint16
	Filters  []string
}

func (p *unsubscribePacket) pktType() packetType { return pktUNSUBSCRIBE }
func (p *unsubscribePacket) headerFlags() byte   { return 0x02 }

func (p *unsubscribePacket) encodeBody(b *bytes.Buffer) error {
	if len(p.Filters) == 0 {
		return fmt.Errorf("%w: UNSUBSCRIBE with no filters", ErrProtocolViolation)
	}
	if p.PacketID == 0 {
		return fmt.Errorf("%w: UNSUBSCRIBE with packet identifier 0", ErrProtocolViolation)
	}
	writeUint16(b, p.PacketID)
	for _, f := range p.Filters {
		if err := writeString(b, f); err != nil {
			return err
		}
	}
	return nil
}

func decodeUnsubscribe(r *bodyReader) (*unsubscribePacket, error) {
	id, err := r.readUint16()
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, fmt.Errorf("%w: UNSUBSCRIBE with packet identifier 0", ErrProtocolViolation)
	}
	p := &unsubscribePacket{PacketID: id}
	for r.remaining() > 0 {
		f, err := r.readString()
		if err != nil {
			return nil, err
		}
		p.Filters = append(p.Filters, f)
	}
	if len(p.Filters) == 0 {
		return nil, fmt.Errorf("%w: UNSUBSCRIBE with no filters", ErrProtocolViolation)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Bodyless packets
// ---------------------------------------------------------------------------

// emptyPacket carries PINGREQ, PINGRESP and DISCONNECT, none of which has a
// variable header or payload.
type emptyPacket struct{ Type packetType }

func (p *emptyPacket) pktType() packetType            { return p.Type }
func (p *emptyPacket) headerFlags() byte              { return 0 }
func (p *emptyPacket) encodeBody(*bytes.Buffer) error { return nil }

// ---------------------------------------------------------------------------
// Read and write
// ---------------------------------------------------------------------------

// writePacket frames p onto w. It writes the fixed header and body in one
// buffered pass; callers using a *bufio.Writer must Flush.
func writePacket(w io.Writer, p packet) error {
	var body bytes.Buffer
	if err := p.encodeBody(&body); err != nil {
		return err
	}
	if body.Len() > maxRemainingLength {
		return fmt.Errorf("%w: %s body of %d bytes", ErrPacketTooLarge, p.pktType(), body.Len())
	}
	var hdr [5]byte
	hdr[0] = byte(p.pktType())<<4 | p.headerFlags()
	n := encodeVarint(hdr[1:], body.Len())
	if _, err := w.Write(hdr[:1+n]); err != nil {
		return err
	}
	if body.Len() == 0 {
		return nil
	}
	_, err := w.Write(body.Bytes())
	return err
}

// readPacket reads and decodes one packet. maxSize bounds the remaining length;
// zero means the protocol maximum. A decode error other than an I/O error means
// the peer must be disconnected — see ErrMalformedPacket.
func readPacket(r *bufio.Reader, maxSize int) (packet, error) {
	first, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	typ := packetType(first >> 4)
	flags := first & 0x0f
	length, err := readVarint(r)
	if err != nil {
		return nil, err
	}
	limit := maxSize
	if limit <= 0 || limit > maxRemainingLength {
		limit = maxRemainingLength
	}
	if length > limit {
		return nil, fmt.Errorf("%w: %s of %d bytes exceeds the %d-byte limit", ErrPacketTooLarge, typ, length, limit)
	}
	var body []byte
	if length > 0 {
		body = make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
	}
	return decodePacket(typ, flags, body)
}

// fixedFlags maps the packet types whose header flags the spec pins to a
// constant. PUBLISH is absent because its low nibble carries dup/qos/retain.
var fixedFlags = map[packetType]byte{
	pktCONNECT: 0, pktCONNACK: 0, pktPUBACK: 0, pktPUBREC: 0,
	pktPUBREL: 0x02, pktPUBCOMP: 0, pktSUBSCRIBE: 0x02, pktSUBACK: 0,
	pktUNSUBSCRIBE: 0x02, pktUNSUBACK: 0, pktPINGREQ: 0, pktPINGRESP: 0,
	pktDISCONNECT: 0,
}

func decodePacket(typ packetType, flags byte, body []byte) (packet, error) {
	if want, ok := fixedFlags[typ]; ok && flags != want {
		return nil, fmt.Errorf("%w: %s with header flags 0x%x, want 0x%x", ErrProtocolViolation, typ, flags, want)
	}
	r := &bodyReader{b: body}
	switch typ {
	case pktCONNECT:
		return decodeConnect(r)
	case pktCONNACK:
		return decodeConnack(r)
	case pktPUBLISH:
		return decodePublish(flags, r)
	case pktPUBACK, pktPUBREC, pktPUBREL, pktPUBCOMP, pktUNSUBACK:
		return decodeAck(typ, r)
	case pktSUBSCRIBE:
		return decodeSubscribe(r)
	case pktSUBACK:
		return decodeSuback(r)
	case pktUNSUBSCRIBE:
		return decodeUnsubscribe(r)
	case pktPINGREQ, pktPINGRESP, pktDISCONNECT:
		if len(body) != 0 {
			return nil, fmt.Errorf("%w: %s with a %d-byte body", ErrMalformedPacket, typ, len(body))
		}
		return &emptyPacket{Type: typ}, nil
	default:
		return nil, fmt.Errorf("%w: reserved packet type %d", ErrProtocolViolation, typ)
	}
}

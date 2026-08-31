package mqtt

import (
	"bufio"
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/usslp/usslp/platform/pkg/msgbus"
)

func TestVarintRoundTrip(t *testing.T) {
	// The boundaries are where the encoding gains a byte; a broker that gets
	// them wrong loses stream framing on exactly one payload size, which is the
	// hardest kind of bug to find in production.
	cases := []struct {
		value int
		bytes int
	}{
		{0, 1}, {1, 1}, {127, 1},
		{128, 2}, {16383, 2},
		{16384, 3}, {2097151, 3},
		{2097152, 4}, {maxRemainingLength, 4},
	}
	for _, tc := range cases {
		var buf [4]byte
		n := encodeVarint(buf[:], tc.value)
		if n != tc.bytes {
			t.Errorf("encodeVarint(%d) used %d bytes, want %d", tc.value, n, tc.bytes)
		}
		got, err := readVarint(bytes.NewReader(buf[:n]))
		if err != nil {
			t.Fatalf("readVarint(%d): %v", tc.value, err)
		}
		if got != tc.value {
			t.Errorf("readVarint round trip: got %d, want %d", got, tc.value)
		}
	}
}

func TestVarintRejectsFiveBytes(t *testing.T) {
	_, err := readVarint(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff, 0x7f}))
	if !errors.Is(err, ErrMalformedPacket) {
		t.Fatalf("five-byte varint: got %v, want ErrMalformedPacket", err)
	}
}

// roundTrip encodes p, decodes it back and returns the result.
func roundTrip(t *testing.T, p packet) packet {
	t.Helper()
	var buf bytes.Buffer
	if err := writePacket(&buf, p); err != nil {
		t.Fatalf("encoding %s: %v", p.pktType(), err)
	}
	got, err := readPacket(bufio.NewReader(&buf), defaultMaxPacketSize)
	if err != nil {
		t.Fatalf("decoding %s: %v", p.pktType(), err)
	}
	return got
}

func TestPacketRoundTrip(t *testing.T) {
	packets := []packet{
		&connectPacket{
			ProtocolName: protocolName, ProtocolLevel: protocolLevel,
			CleanSession: true, KeepAlive: 30, ClientID: "sec-0042",
			WillFlag: true, WillTopic: "usslp/acme/eu-west-1/store-7/sec/0042/status",
			WillMessage: []byte("offline"), WillQoS: msgbus.AtLeastOnce, WillRetain: true,
			HasUsername: true, Username: "acme:gateway",
			HasPassword: true, Password: []byte("s3cret"),
		},
		&connackPacket{SessionPresent: true, ReturnCode: ConnectAccepted},
		&publishPacket{QoS: msgbus.ExactlyOnce, Dup: true, Retain: true,
			Topic: "usslp/acme/eu-west-1/store-7/labels/L1/price", PacketID: 4242,
			Payload: []byte(`{"price":399}`)},
		// An empty payload is the retained-clear publication, and it must
		// survive framing as a zero-length payload rather than a missing one.
		&publishPacket{QoS: msgbus.AtMostOnce, Topic: "a/b", Payload: []byte{}},
		&ackPacket{Type: pktPUBACK, PacketID: 1},
		&ackPacket{Type: pktPUBREC, PacketID: 2},
		&ackPacket{Type: pktPUBREL, PacketID: 3},
		&ackPacket{Type: pktPUBCOMP, PacketID: 4},
		&ackPacket{Type: pktUNSUBACK, PacketID: 5},
		&subscribePacket{PacketID: 9, Filters: []topicFilter{
			{Filter: "usslp/acme/+/+/labels/+/price", QoS: msgbus.AtLeastOnce},
			{Filter: "usslp/acme/#", QoS: msgbus.ExactlyOnce},
		}},
		&subackPacket{PacketID: 9, Codes: []byte{1, 2, subackFailure}},
		&unsubscribePacket{PacketID: 11, Filters: []string{"a/#", "b/+"}},
		&emptyPacket{Type: pktPINGREQ},
		&emptyPacket{Type: pktPINGRESP},
		&emptyPacket{Type: pktDISCONNECT},
	}
	for _, want := range packets {
		got := roundTrip(t, want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s did not survive a round trip:\n got %#v\nwant %#v", want.pktType(), got, want)
		}
	}
}

func TestPacketRoundTripLargePayload(t *testing.T) {
	// 300 bytes crosses the two-byte remaining-length boundary; 20000 crosses
	// the three-byte one. A planogram update is comfortably in the second band.
	for _, size := range []int{0, 1, 126, 127, 128, 300, 20000} {
		p := &publishPacket{QoS: msgbus.AtLeastOnce, Topic: "usslp/acme/r/s/store/planogram/update",
			PacketID: 7, Payload: bytes.Repeat([]byte{0xab}, size)}
		got, ok := roundTrip(t, p).(*publishPacket)
		if !ok {
			t.Fatalf("payload of %d bytes did not decode as PUBLISH", size)
		}
		if len(got.Payload) != size {
			t.Errorf("payload of %d bytes decoded as %d", size, len(got.Payload))
		}
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := []struct {
		name  string
		bytes []byte
		want  error
	}{
		{"reserved packet type 0", []byte{0x00, 0x00}, ErrProtocolViolation},
		{"reserved packet type 15", []byte{0xf0, 0x00}, ErrProtocolViolation},
		{"PUBLISH with QoS 3", []byte{0x36, 0x04, 0x00, 0x01, 'a', 0x00}, ErrProtocolViolation},
		{"PUBREL without its fixed flags", []byte{0x60, 0x02, 0x00, 0x01}, ErrProtocolViolation},
		{"SUBSCRIBE without its fixed flags", []byte{0x80, 0x02, 0x00, 0x01}, ErrProtocolViolation},
		{"PUBACK with identifier 0", []byte{0x40, 0x02, 0x00, 0x00}, ErrProtocolViolation},
		{"SUBSCRIBE with no filters", []byte{0x82, 0x02, 0x00, 0x01}, ErrProtocolViolation},
		{"UNSUBSCRIBE with no filters", []byte{0xa2, 0x02, 0x00, 0x01}, ErrProtocolViolation},
		{"truncated string length", []byte{0x30, 0x01, 0x00}, ErrMalformedPacket},
		{"string longer than the body", []byte{0x30, 0x03, 0x00, 0x10, 'a'}, ErrMalformedPacket},
		{"PINGREQ with a body", []byte{0xc0, 0x01, 0x00}, ErrMalformedPacket},
		{"DISCONNECT with a body", []byte{0xe0, 0x01, 0x00}, ErrMalformedPacket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readPacket(bufio.NewReader(bytes.NewReader(tc.bytes)), defaultMaxPacketSize)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecodeRejectsInvalidUTF8Topic(t *testing.T) {
	// 0xff can never appear in valid UTF-8.
	body := []byte{0x30, 0x03, 0x00, 0x01, 0xff}
	_, err := readPacket(bufio.NewReader(bytes.NewReader(body)), defaultMaxPacketSize)
	if !errors.Is(err, ErrMalformedPacket) {
		t.Fatalf("got %v, want ErrMalformedPacket", err)
	}
}

func TestDecodeRejectsNullInString(t *testing.T) {
	body := []byte{0x30, 0x04, 0x00, 0x02, 'a', 0x00}
	_, err := readPacket(bufio.NewReader(bytes.NewReader(body)), defaultMaxPacketSize)
	if !errors.Is(err, ErrMalformedPacket) {
		t.Fatalf("got %v, want ErrMalformedPacket", err)
	}
}

func TestReadPacketEnforcesMaxSize(t *testing.T) {
	// Remaining length 300 with a 16-byte limit: the limit must be applied from
	// the header alone, before the body is allocated.
	body := []byte{0x30, 0xac, 0x02}
	_, err := readPacket(bufio.NewReader(bytes.NewReader(body)), 16)
	if !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("got %v, want ErrPacketTooLarge", err)
	}
}

func TestConnectFlagValidation(t *testing.T) {
	cases := []struct {
		name  string
		flags byte
	}{
		{"reserved bit set", 0x01},
		{"will QoS 3", 0x04 | 0x18},
		{"will QoS without a will", 0x08},
		{"will retain without a will", 0x20},
		{"password without username", 0x40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			if err := writeString(&body, protocolName); err != nil {
				t.Fatal(err)
			}
			body.WriteByte(protocolLevel)
			body.WriteByte(tc.flags)
			writeUint16(&body, 30)
			if err := writeString(&body, "c1"); err != nil {
				t.Fatal(err)
			}
			_, err := decodePacket(pktCONNECT, 0, body.Bytes())
			if !errors.Is(err, ErrProtocolViolation) {
				t.Fatalf("got %v, want ErrProtocolViolation", err)
			}
		})
	}
}

func TestEncodeRejectsOversizedString(t *testing.T) {
	var buf bytes.Buffer
	err := writePacket(&buf, &publishPacket{Topic: strings.Repeat("x", 70000)})
	if !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("got %v, want ErrPacketTooLarge", err)
	}
}

func TestEncodeRejectsZeroPacketID(t *testing.T) {
	var buf bytes.Buffer
	err := writePacket(&buf, &publishPacket{QoS: msgbus.AtLeastOnce, Topic: "a", PacketID: 0})
	if !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("got %v, want ErrProtocolViolation", err)
	}
}

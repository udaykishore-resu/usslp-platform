package eventlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestRecordRoundTrip(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("price"), 4096)
	cases := []struct {
		name string
		rec  record
	}{
		{"minimal", record{timestamp: 1}},
		{"keyed", record{timestamp: 1700000000000000000, key: []byte("store-42:sku-9"), value: []byte(`{"cents":499}`)}},
		{"headers", record{
			timestamp: 7,
			key:       []byte("k"),
			value:     []byte("v"),
			headers:   map[string]string{"usslp-event-type": "label.price.updated", "traceparent": "00-abc-def-01"},
		}},
		{"empty value", record{timestamp: 9, key: []byte("k"), headers: map[string]string{"a": ""}}},
		{"binary key and value", record{timestamp: 11, key: []byte{0x00, 0xff, 0x7f}, value: []byte{0x00, 0x01, 0x02}}},
		{"large value", record{timestamp: 13, key: []byte("bulk"), value: big}},
		{"gap", gapRecord(17)},
		{"many headers", record{timestamp: 19, key: []byte("k"), value: []byte("v"), headers: manyHeaders(64)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enc := encodeRecord(nil, tc.rec)
			got, n, err := readRecord(bytes.NewReader(enc))
			if err != nil {
				t.Fatalf("readRecord: %v", err)
			}
			if n != len(enc) {
				t.Fatalf("consumed %d bytes, encoded %d", n, len(enc))
			}
			if got.timestamp != tc.rec.timestamp {
				t.Errorf("timestamp = %d, want %d", got.timestamp, tc.rec.timestamp)
			}
			if !bytes.Equal(got.key, tc.rec.key) {
				t.Errorf("key = %q, want %q", got.key, tc.rec.key)
			}
			if !bytes.Equal(nilIfEmpty(got.value), nilIfEmpty(tc.rec.value)) {
				t.Errorf("value = %q, want %q", got.value, tc.rec.value)
			}
			if got.gap != tc.rec.gap {
				t.Errorf("gap = %v, want %v", got.gap, tc.rec.gap)
			}
			if len(tc.rec.headers) > 0 && !reflect.DeepEqual(got.headers, tc.rec.headers) {
				t.Errorf("headers = %v, want %v", got.headers, tc.rec.headers)
			}
		})
	}
}

// TestRecordEncodingIsDeterministic pins the property compaction relies on:
// rewriting a record must produce the same bytes it had before.
func TestRecordEncodingIsDeterministic(t *testing.T) {
	t.Parallel()
	rec := record{timestamp: 5, key: []byte("k"), value: []byte("v"),
		headers: map[string]string{"z": "1", "a": "2", "m": "3"}}
	first := encodeRecord(nil, rec)
	for i := 0; i < 32; i++ {
		if got := encodeRecord(nil, rec); !bytes.Equal(got, first) {
			t.Fatalf("encoding differs between runs")
		}
	}
}

func TestReadRecordRejectsDamage(t *testing.T) {
	t.Parallel()
	good := encodeRecord(nil, record{timestamp: 3, key: []byte("store-1:sku-1"), value: []byte("payload")})

	cases := []struct {
		name  string
		input []byte
		want  error
	}{
		{"empty is clean eof", nil, io.EOF},
		{"partial length prefix", good[:2], ErrCorrupt},
		{"truncated body", good[:len(good)-3], ErrCorrupt},
		{"flipped payload byte", flip(good, len(good)-1), ErrCorrupt},
		{"flipped key byte", flip(good, 24), ErrCorrupt},
		{"zero length", []byte{0, 0, 0, 0}, ErrCorrupt},
		{"absurd length", []byte{0xff, 0xff, 0xff, 0xff}, ErrCorrupt},
		{"crc mismatch", corruptCRC(good), ErrCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := readRecord(bytes.NewReader(tc.input))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestDecodeBodyRejectsLyingLengths proves the decoder does not trust a length
// field even when the CRC says the bytes are intact — a writer bug must produce
// an error, not a panic in a consumer.
func TestDecodeBodyRejectsLyingLengths(t *testing.T) {
	t.Parallel()
	bodies := [][]byte{
		mkBody(func(b []byte) []byte { return binary.BigEndian.AppendUint32(b, 0xffffffff) }),
		mkBody(func(b []byte) []byte {
			b = binary.BigEndian.AppendUint32(b, 1)
			b = append(b, 'k')
			return binary.BigEndian.AppendUint32(b, 0xfffffff0) // header count
		}),
		mkBody(func(b []byte) []byte {
			b = binary.BigEndian.AppendUint32(b, 0) // key
			b = binary.BigEndian.AppendUint32(b, 0) // headers
			b = binary.BigEndian.AppendUint32(b, 8) // value length, but no value
			return b
		}),
	}
	for i, body := range bodies {
		framed := frame(body)
		if _, _, err := readRecord(bytes.NewReader(framed)); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("case %d: err = %v, want ErrCorrupt", i, err)
		}
	}
}

func TestOffsetFileNameIsPathSafe(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"label-service":    "label-service.json",
		"../../etc/passwd": "%2E%2E%2F%2E%2E%2Fetc%2Fpasswd.json",
		"a b":              "a%20b.json",
		"":                 ".json",
	}
	for in, want := range cases {
		if got := offsetFileName(in); got != want {
			t.Errorf("offsetFileName(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(offsetFileName(in), `/\`) {
			t.Errorf("offsetFileName(%q) contains a separator", in)
		}
	}
}

func manyHeaders(n int) map[string]string {
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		m[string(rune('a'+i%26))+strings.Repeat("x", i%7)] = strings.Repeat("v", i%5)
	}
	return m
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func flip(b []byte, i int) []byte {
	out := append([]byte(nil), b...)
	out[i] ^= 0xff
	return out
}

func corruptCRC(b []byte) []byte {
	out := append([]byte(nil), b...)
	binary.BigEndian.PutUint32(out[4:], binary.BigEndian.Uint32(out[4:])+1)
	return out
}

// mkBody builds a record body starting with a timestamp.
func mkBody(rest func([]byte) []byte) []byte {
	b := binary.BigEndian.AppendUint64(nil, 42)
	return rest(b)
}

// frame wraps a body in a correct length prefix and CRC, so that only the body
// itself is under test.
func frame(body []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(crcPrefixBytes+len(body)))
	out = binary.BigEndian.AppendUint32(out, crc32.Checksum(body, castagnoli))
	return append(out, body...)
}

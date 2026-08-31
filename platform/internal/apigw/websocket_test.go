package apigw

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Framing
//
// These tests drive the implementation from both ends: a "client" Conn that
// masks its frames as RFC 6455 §5.1 requires of a client, and a "server" Conn
// that does not and refuses anything unmasked.
// ---------------------------------------------------------------------------

// connPair returns two Conns over a real TCP connection.
//
// Real TCP rather than net.Pipe because net.Pipe is synchronous and unbuffered:
// a bufio flush on one side blocks until the other side reads, which turns a
// framing bug into a deadlock instead of a failed assertion.
func connPair(t *testing.T, cfg ConnConfig) (server, client *Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()

	clientRaw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("accept: %v", got.err)
	}

	serverCfg, clientCfg := cfg, cfg
	serverCfg.Mask = false
	clientCfg.Mask = true

	server = newConn(got.conn, bufio.NewReadWriter(bufio.NewReader(got.conn), bufio.NewWriter(got.conn)), serverCfg)
	client = newConn(clientRaw, bufio.NewReadWriter(bufio.NewReader(clientRaw), bufio.NewWriter(clientRaw)), clientCfg)
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return server, client
}

// TestComputeAcceptKey checks the derivation against the worked example in RFC
// 6455 §1.3. Getting this wrong is the difference between a handshake and a
// browser that refuses to connect with no useful diagnostic.
func TestComputeAcceptKey(t *testing.T) {
	t.Parallel()
	const clientKey = "dGhlIHNhbXBsZSBub25jZQ=="
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := ComputeAcceptKey(clientKey); got != want {
		t.Fatalf("ComputeAcceptKey(%q) = %q, want %q", clientKey, got, want)
	}
}

func TestUpgradeRefusesMalformedHandshakes(t *testing.T) {
	t.Parallel()
	base := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/stream", nil)
		r.Header.Set("Connection", "keep-alive, Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		return r
	}

	tests := []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"missing Connection", func(r *http.Request) { r.Header.Del("Connection") }, http.StatusBadRequest},
		{"wrong Upgrade", func(r *http.Request) { r.Header.Set("Upgrade", "h2c") }, http.StatusBadRequest},
		{"unsupported version", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") }, http.StatusBadRequest},
		{"missing key", func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") }, http.StatusBadRequest},
		{"short key", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "c2hvcnQ=") }, http.StatusBadRequest},
		{"not a GET", func(r *http.Request) { r.Method = http.MethodPost }, http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(r)
			w := httptest.NewRecorder()
			_, err := Upgrade(w, r, ConnConfig{}, "")
			if err == nil {
				t.Fatal("the handshake was accepted")
			}
			var ae *apiError
			if !errors.As(err, &ae) || ae.status != tc.status {
				t.Fatalf("error %v, want status %d", err, tc.status)
			}
		})
	}

	// A well-formed handshake still fails on a ResponseWriter that cannot be
	// hijacked, and says so rather than panicking.
	w := httptest.NewRecorder()
	if _, err := Upgrade(w, base(), ConnConfig{}, ""); err == nil {
		t.Fatal("Upgrade succeeded on a non-hijackable ResponseWriter")
	}
}

func TestTextAndBinaryFramesRoundTrip(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{})

	cases := []struct {
		name    string
		op      Opcode
		payload []byte
	}{
		{"empty text", OpText, []byte("")},
		{"short text", OpText, []byte("hello, shelf")},
		{"utf-8 text", OpText, []byte("£2.99 · 2 für 3 · ¥300")},
		{"binary", OpBinary, []byte{0x00, 0xff, 0x7f, 0x80, 0x01}},
		// The three payload-length encodings: 7-bit, 16-bit and 64-bit.
		{"125 bytes", OpBinary, bytes.Repeat([]byte{0xAB}, 125)},
		{"126 bytes", OpBinary, bytes.Repeat([]byte{0xCD}, 126)},
		{"65535 bytes", OpBinary, bytes.Repeat([]byte{0xEF}, 65535)},
		{"65536 bytes", OpBinary, bytes.Repeat([]byte{0x12}, 65536)},
	}

	for _, tc := range cases {
		t.Run(tc.name+" client to server", func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- client.WriteMessage(tc.op, tc.payload) }()
			op, got, err := server.ReadMessage()
			if err != nil {
				t.Fatalf("server read: %v", err)
			}
			if werr := <-done; werr != nil {
				t.Fatalf("client write: %v", werr)
			}
			if op != tc.op {
				t.Fatalf("opcode %s, want %s", op, tc.op)
			}
			if !bytes.Equal(got, tc.payload) {
				t.Fatalf("payload of %d bytes did not survive the round trip", len(tc.payload))
			}
		})
		t.Run(tc.name+" server to client", func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- server.WriteMessage(tc.op, tc.payload) }()
			op, got, err := client.ReadMessage()
			if err != nil {
				t.Fatalf("client read: %v", err)
			}
			if werr := <-done; werr != nil {
				t.Fatalf("server write: %v", werr)
			}
			if op != tc.op || !bytes.Equal(got, tc.payload) {
				t.Fatalf("server-to-client %s round trip failed", tc.name)
			}
		})
	}
}

func TestFragmentedMessagesAreReassembled(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{})

	chunks := [][]byte{[]byte("the "), []byte("price "), []byte("of "), []byte("everything")}
	want := []byte("the price of everything")

	done := make(chan error, 1)
	go func() { done <- client.WriteFragments(OpText, chunks) }()
	op, got, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}
	if op != OpText {
		t.Fatalf("opcode %s, want text — the opcode belongs to the first frame only", op)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("reassembled %q, want %q", got, want)
	}
}

func TestControlFramesMayInterleaveWithFragments(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{})

	// RFC 6455 §5.4: a control frame may appear between the fragments of a
	// message. An implementation that treats the ping as a continuation
	// corrupts the message.
	go func() {
		_ = client.writeFrame(false, OpText, []byte("half "))
		_ = client.WritePing([]byte("keepalive"))
		_ = client.writeFrame(true, OpContinuation, []byte("a message"))
	}()

	op, got, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != OpText || string(got) != "half a message" {
		t.Fatalf("got %s %q, want text \"half a message\"", op, got)
	}

	// And the server answered the interleaved ping rather than swallowing it.
	op, payload, err := readRawFrame(t, client)
	if err != nil {
		t.Fatalf("reading the pong: %v", err)
	}
	if op != OpPong || string(payload) != "keepalive" {
		t.Fatalf("got %s %q, want a pong echoing \"keepalive\"", op, payload)
	}
}

func TestServerRefusesUnmaskedClientFrames(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{})

	// A client Conn that does not mask — exactly the bug RFC 6455 §5.1 makes
	// illegal, because an unmasked client frame is what lets an attacker
	// choose the bytes an intermediary cache sees.
	raw := client.UnderlyingConn()
	go func() {
		payload := []byte("unmasked")
		frame := []byte{0x81, byte(len(payload))}
		frame = append(frame, payload...)
		_, _ = raw.Write(frame)
	}()

	_, _, err := server.ReadMessage()
	if !IsCloseError(err, CloseProtocolError) {
		t.Fatalf("error %v, want a 1002 protocol error", err)
	}

	// And the peer is told why before the connection goes away.
	op, payload, rerr := readRawFrame(t, client)
	if rerr != nil {
		t.Fatalf("reading the server's close frame: %v", rerr)
	}
	if op != OpClose {
		t.Fatalf("server sent %s, want a close frame", op)
	}
	if code := CloseCode(binary.BigEndian.Uint16(payload[:2])); code != CloseProtocolError {
		t.Fatalf("close code %d, want %d", code, CloseProtocolError)
	}
}

func TestProtocolViolationsAreRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		frame []byte
		want  CloseCode
	}{
		{
			// RSV1 set with no negotiated extension.
			name:  "reserved bit set",
			frame: maskedFrame(0xC1, []byte("x")),
			want:  CloseProtocolError,
		},
		{
			name:  "reserved opcode",
			frame: maskedFrame(0x83, []byte("x")),
			want:  CloseProtocolError,
		},
		{
			// A fragmented control frame: FIN clear on a ping.
			name:  "fragmented control frame",
			frame: maskedFrame(0x09, []byte("x")),
			want:  CloseProtocolError,
		},
		{
			name:  "continuation with nothing in progress",
			frame: maskedFrame(0x80, []byte("orphan")),
			want:  CloseProtocolError,
		},
		{
			name:  "invalid utf-8 in a text frame",
			frame: maskedFrame(0x81, []byte{0xff, 0xfe, 0xfd}),
			want:  CloseInvalidPayload,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, client := connPair(t, ConnConfig{})
			frame := tc.frame
			if tc.name == "fragmented control frame" {
				frame[0] &^= 0x80 // clear FIN on the ping
			}
			go func() { _, _ = client.UnderlyingConn().Write(frame) }()
			_, _, err := server.ReadMessage()
			if !IsCloseError(err, tc.want) {
				t.Fatalf("error %v, want close code %d", err, tc.want)
			}
		})
	}
}

func TestOversizedMessageIsRefused(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{MaxMessageBytes: 64})

	go func() { _ = client.WriteMessage(OpBinary, bytes.Repeat([]byte{1}, 256)) }()
	_, _, err := server.ReadMessage()
	if !IsCloseError(err, CloseMessageTooBig) {
		t.Fatalf("error %v, want 1009 message too big", err)
	}
}

func TestFragmentedMessageCannotExceedTheLimitInAggregate(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{MaxMessageBytes: 100})

	// Each fragment is inside the limit; the reassembled message is not. An
	// implementation that only checks per frame has an unbounded allocation
	// controlled entirely by the peer.
	go func() {
		for i := 0; i < 5; i++ {
			op := OpContinuation
			if i == 0 {
				op = OpBinary
			}
			if err := client.writeFrame(false, op, bytes.Repeat([]byte{9}, 40)); err != nil {
				return
			}
		}
	}()
	_, _, err := server.ReadMessage()
	if !IsCloseError(err, CloseMessageTooBig) {
		t.Fatalf("error %v, want 1009 message too big", err)
	}
}

func TestPingIsAnsweredWithAPong(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{})

	// The server answers from inside its read loop, so a busy application
	// goroutine cannot make a healthy connection look dead to its peer.
	go func() {
		_, _, _ = server.ReadMessage()
	}()
	if err := client.WritePing([]byte("are you there")); err != nil {
		t.Fatalf("ping: %v", err)
	}
	op, payload, err := readRawFrame(t, client)
	if err != nil {
		t.Fatalf("reading the pong: %v", err)
	}
	if op != OpPong {
		t.Fatalf("got %s, want a pong", op)
	}
	if string(payload) != "are you there" {
		t.Fatalf("pong payload %q; RFC 6455 requires the ping's body to be echoed", payload)
	}
}

func TestPongsAreDeliveredToTheKeepalive(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{})

	go func() { _, _, _ = server.ReadMessage() }()
	if err := client.WritePong([]byte("unsolicited")); err != nil {
		t.Fatalf("pong: %v", err)
	}
	select {
	case got := <-server.Pongs():
		if string(got) != "unsolicited" {
			t.Fatalf("pong payload %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pong was not delivered to Pongs()")
	}
}

func TestCloseHandshakeIsCompletedInBothDirections(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{})

	// Client closes; the server echoes the code and reports it.
	go func() { _ = client.WriteClose(CloseNormal, "done") }()
	_, _, err := server.ReadMessage()
	var ce *CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v, want a CloseError", err)
	}
	if ce.Code != CloseNormal || ce.Reason != "done" {
		t.Fatalf("close %d %q, want 1000 \"done\"", ce.Code, ce.Reason)
	}
	op, payload, rerr := readRawFrame(t, client)
	if rerr != nil {
		t.Fatalf("reading the echoed close: %v", rerr)
	}
	if op != OpClose {
		t.Fatalf("server replied with %s, want a close frame", op)
	}
	if code := CloseCode(binary.BigEndian.Uint16(payload[:2])); code != CloseNormal {
		t.Fatalf("echoed close code %d, want 1000", code)
	}
}

func TestCloseWithHandshakeWaitsForThePeer(t *testing.T) {
	t.Parallel()
	server, client := connPair(t, ConnConfig{})

	replied := make(chan struct{})
	go func() {
		// The peer reads the close and replies, which is what the initiator
		// is waiting for; without the wait the TCP reset overtakes the close
		// frame and the peer reports a connection error instead of a clean
		// shutdown.
		_, _, _ = client.ReadMessage()
		close(replied)
	}()
	if err := server.CloseWithHandshake(CloseGoingAway, "draining", 2*time.Second); err != nil {
		t.Fatalf("close handshake: %v", err)
	}
	select {
	case <-replied:
	case <-time.After(2 * time.Second):
		t.Fatal("the peer never saw the close frame")
	}
}

func TestCloseFrameValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload []byte
		wantErr bool
	}{
		{"empty is a valid close", nil, false},
		{"one byte cannot hold a code", []byte{0x03}, true},
		{"1000 normal", closePayload(1000, ""), false},
		{"1013 try again later", closePayload(1013, "behind"), false},
		{"3000 application range", closePayload(3000, ""), false},
		{"1004 is reserved", closePayload(1004, ""), true},
		{"1005 must never appear on the wire", closePayload(1005, ""), true},
		{"1006 must never appear on the wire", closePayload(1006, ""), true},
		{"999 is below the valid range", closePayload(999, ""), true},
		{"5000 is above the valid range", closePayload(5000, ""), true},
		{"invalid utf-8 reason", append(closePayload(1000, ""), 0xff, 0xfe), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseClosePayload(tc.payload)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseClosePayload(%v) error = %v, wantErr %v", tc.payload, err, tc.wantErr)
			}
		})
	}
}

func TestControlFramePayloadIsBounded(t *testing.T) {
	t.Parallel()
	server, _ := connPair(t, ConnConfig{})
	// RFC 6455 §5.5 caps a control payload at 125 bytes; the writer refuses
	// rather than emitting a frame no conforming peer will accept.
	if err := server.WritePing(bytes.Repeat([]byte{1}, 200)); err == nil {
		t.Fatal("a 200-byte ping was written")
	}
}

// ---------------------------------------------------------------------------
// Raw frame helpers used by the tests above
// ---------------------------------------------------------------------------

// readRawFrame reads one frame from a Conn without the message-assembly layer,
// so a test can assert on control frames the reader would otherwise consume.
func readRawFrame(t *testing.T, c *Conn) (Opcode, []byte, error) {
	t.Helper()
	_ = c.UnderlyingConn().SetReadDeadline(time.Now().Add(3 * time.Second))
	f, err := c.readFrame()
	if err != nil {
		return 0, nil, err
	}
	return f.opcode, f.payload, nil
}

// maskedFrame builds a single masked frame with the given first byte, as a
// conforming client would send it.
func maskedFrame(first byte, payload []byte) []byte {
	mask := []byte{0xA1, 0xB2, 0xC3, 0xD4}
	out := []byte{first, byte(0x80 | len(payload))}
	out = append(out, mask...)
	for i, b := range payload {
		out = append(out, b^mask[i%4])
	}
	return out
}

// closePayload builds a close frame body.
func closePayload(code uint16, reason string) []byte {
	out := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(out[:2], code)
	copy(out[2:], reason)
	return out
}

// ---------------------------------------------------------------------------
// A hand-written client for the end-to-end stream tests
// ---------------------------------------------------------------------------

// wsClient is a minimal RFC 6455 client. It performs the opening handshake by
// writing the HTTP request itself and verifying Sec-WebSocket-Accept, so the
// gateway's handshake is tested rather than assumed.
type wsClient struct {
	conn *Conn
	raw  net.Conn
	// Subprotocol is what the server selected.
	Subprotocol string
}

// dialWebSocket opens a stream connection against a test server.
func dialWebSocket(t *testing.T, serverURL, path string, protocols []string, extra http.Header) (*wsClient, *http.Response, error) {
	t.Helper()
	host := strings.TrimPrefix(serverURL, "http://")
	raw, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		return nil, nil, err
	}

	// A fixed nonce: the accept value is then a constant this test can check
	// against ComputeAcceptKey, which is itself checked against the RFC's
	// worked example.
	const nonce = "dGhlIHNhbXBsZSBub25jZQ=="
	var req strings.Builder
	req.WriteString("GET " + path + " HTTP/1.1\r\n")
	req.WriteString("Host: " + host + "\r\n")
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: keep-alive, Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	req.WriteString("Sec-WebSocket-Key: " + nonce + "\r\n")
	if len(protocols) > 0 {
		req.WriteString("Sec-WebSocket-Protocol: " + strings.Join(protocols, ", ") + "\r\n")
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.WriteString(k + ": " + v + "\r\n")
		}
	}
	req.WriteString("\r\n")

	_ = raw.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(raw, req.String()); err != nil {
		_ = raw.Close()
		return nil, nil, err
	}

	br := bufio.NewReader(raw)
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	httpReq, _ := http.NewRequest(http.MethodGet, serverURL+path, nil)
	res, err := http.ReadResponse(br, httpReq)
	if err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		return nil, res, nil
	}
	if got := res.Header.Get("Sec-WebSocket-Accept"); got != ComputeAcceptKey(nonce) {
		_ = raw.Close()
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, ComputeAcceptKey(nonce))
	}
	if !strings.EqualFold(res.Header.Get("Upgrade"), "websocket") {
		t.Fatalf("Upgrade header = %q", res.Header.Get("Upgrade"))
	}
	_ = raw.SetReadDeadline(time.Time{})

	c := newConn(raw, bufio.NewReadWriter(br, bufio.NewWriter(raw)),
		ConnConfig{Mask: true, ReadTimeout: 5 * time.Second})
	client := &wsClient{conn: c, raw: raw, Subprotocol: res.Header.Get("Sec-WebSocket-Protocol")}
	t.Cleanup(func() { _ = c.Close() })
	return client, res, nil
}

// readJSON reads one text message and decodes it.
func (c *wsClient) readJSON(t *testing.T, dst any) error {
	t.Helper()
	op, payload, err := c.conn.ReadMessage()
	if err != nil {
		return err
	}
	if op != OpText {
		t.Fatalf("expected a text frame, got %s", op)
	}
	return jsonDecode(payload, dst)
}

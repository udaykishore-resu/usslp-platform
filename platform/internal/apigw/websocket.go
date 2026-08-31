package apigw

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// RFC 6455, by hand
//
// USSLP builds with the standard library only, so the WebSocket protocol is
// implemented here directly over a hijacked net.Conn. This is a complete
// server implementation of the framing layer — handshake, fragmentation,
// masking, control frames, the close handshake and UTF-8 validation of text
// payloads — and a client half used by the tests to drive it.
//
// The protocol errors are enforced rather than tolerated. A server that
// silently accepts an unmasked client frame or a fragmented control frame is a
// server whose peers will develop dependencies on those bugs, and at the point
// where a Shelf Edge Controller's diagnostic tooling and a browser console are
// both speaking to this endpoint, "be liberal in what you accept" produces two
// incompatible dialects rather than one protocol.
// ---------------------------------------------------------------------------

// Opcode is a WebSocket frame opcode (RFC 6455 §5.2).
type Opcode byte

// The opcodes the platform uses. The reserved ranges (0x3–0x7 non-control,
// 0xB–0xF control) are rejected: they can only appear if an extension was
// negotiated, and the gateway negotiates none.
const (
	// OpContinuation continues a fragmented message.
	OpContinuation Opcode = 0x0
	// OpText carries UTF-8. Every gateway-originated event is a text frame of
	// JSON.
	OpText Opcode = 0x1
	// OpBinary carries opaque bytes.
	OpBinary Opcode = 0x2
	// OpClose starts or completes the close handshake.
	OpClose Opcode = 0x8
	// OpPing and OpPong are the keepalive pair.
	OpPing Opcode = 0x9
	OpPong Opcode = 0xA
)

func (o Opcode) isControl() bool { return o&0x8 != 0 }

// String renders an opcode for logs and test failures.
func (o Opcode) String() string {
	switch o {
	case OpContinuation:
		return "continuation"
	case OpText:
		return "text"
	case OpBinary:
		return "binary"
	case OpClose:
		return "close"
	case OpPing:
		return "ping"
	case OpPong:
		return "pong"
	default:
		return fmt.Sprintf("opcode(0x%x)", byte(o))
	}
}

// CloseCode is a WebSocket close status (RFC 6455 §7.4).
type CloseCode uint16

// The close codes the gateway sends.
const (
	// CloseNormal is an orderly shutdown of a finished conversation.
	CloseNormal CloseCode = 1000
	// CloseGoingAway is sent when the gateway is draining for a deployment. A
	// client seeing it should reconnect immediately; a client seeing 1000
	// should not.
	CloseGoingAway CloseCode = 1001
	// CloseProtocolError is a framing violation.
	CloseProtocolError CloseCode = 1002
	// CloseUnsupportedData is a frame type the endpoint cannot accept.
	CloseUnsupportedData CloseCode = 1003
	// CloseInvalidPayload is a text frame that is not valid UTF-8.
	CloseInvalidPayload CloseCode = 1007
	// ClosePolicyViolation covers a request the endpoint refuses on policy.
	ClosePolicyViolation CloseCode = 1008
	// CloseMessageTooBig is a message beyond the configured limit.
	CloseMessageTooBig CloseCode = 1009
	// CloseInternalError is an endpoint-side failure.
	CloseInternalError CloseCode = 1011
	// CloseTryAgainLater is the IANA-registered code for "the server is
	// temporarily unable to keep serving this client". It is what a subscriber
	// that has fallen too far behind receives: the connection is being closed
	// because of a condition on the client's side of the pipe, and telling it
	// so — rather than sending 1011, which says the server broke — is what
	// makes the client's reconnect-and-resync the obviously correct response.
	CloseTryAgainLater CloseCode = 1013
)

// CloseError is returned by ReadMessage when the peer closes.
type CloseError struct {
	Code   CloseCode
	Reason string
}

// Error implements error.
func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket: peer closed with %d %q", e.Code, e.Reason)
}

// IsCloseError reports whether err is a peer close, optionally matching one of
// the given codes.
func IsCloseError(err error, codes ...CloseCode) bool {
	var ce *CloseError
	if !errors.As(err, &ce) {
		return false
	}
	if len(codes) == 0 {
		return true
	}
	for _, c := range codes {
		if ce.Code == c {
			return true
		}
	}
	return false
}

// websocketGUID is the RFC 6455 §1.3 magic value. It exists so that a cached
// HTTP proxy cannot be tricked into completing a handshake by replaying a
// response it saw earlier: the accept value is only derivable by an endpoint
// that actually understands the protocol.
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ComputeAcceptKey derives the Sec-WebSocket-Accept value for a client key.
func ComputeAcceptKey(clientKey string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, clientKey)
	_, _ = io.WriteString(h, websocketGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Frame limits.
const (
	// maxControlPayload is fixed by RFC 6455 §5.5.
	maxControlPayload = 125
	// DefaultMaxMessageBytes bounds a reassembled inbound message. Clients
	// send subscription changes and pongs; a megabyte is three orders of
	// magnitude more than any of them needs, and without a limit a fragmented
	// message is an unbounded allocation controlled entirely by the peer.
	DefaultMaxMessageBytes = 1 << 20
)

// ConnConfig tunes one connection.
type ConnConfig struct {
	// MaxMessageBytes bounds a reassembled inbound message.
	MaxMessageBytes int64
	// ReadTimeout is the idle deadline for inbound frames. The keepalive is
	// what keeps this from firing on a healthy but quiet connection.
	ReadTimeout time.Duration
	// WriteTimeout bounds a single frame write. It must be finite: a TCP
	// receiver that stops reading otherwise blocks a gateway goroutine for as
	// long as the kernel's retransmit schedule takes, which is minutes.
	WriteTimeout time.Duration
	// Mask makes this endpoint mask its outbound frames, which RFC 6455
	// requires of a client and forbids of a server.
	Mask bool
}

func (c ConnConfig) withDefaults() ConnConfig {
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 90 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	return c
}

// Conn is one WebSocket connection.
//
// One goroutine may read and one may write concurrently, which is the standard
// contract for this kind of type. Writes are additionally serialised by a
// mutex because the reader must be able to answer a ping with a pong while the
// writer is mid-message — interleaving two frames' bytes on the wire would
// corrupt the stream irrecoverably.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	cfg  ConnConfig

	wmu sync.Mutex

	// closeMu guards the close bookkeeping so that a close from the reader and
	// one from the writer cannot both send a close frame.
	closeMu   sync.Mutex
	closeSent bool
	closed    bool

	// pongs receives the payloads of inbound pongs, so a keepalive loop can
	// verify liveness without owning the read loop.
	pongs chan []byte
}

// newConn wraps a hijacked connection.
func newConn(c net.Conn, brw *bufio.ReadWriter, cfg ConnConfig) *Conn {
	cfg = cfg.withDefaults()
	br := brw.Reader
	if br == nil {
		br = bufio.NewReader(c)
	}
	bw := brw.Writer
	if bw == nil {
		bw = bufio.NewWriter(c)
	}
	return &Conn{conn: c, br: br, bw: bw, cfg: cfg, pongs: make(chan []byte, 1)}
}

// UnderlyingConn exposes the socket, for deadlines the stream layer sets.
func (c *Conn) UnderlyingConn() net.Conn { return c.conn }

// Pongs returns the channel inbound pong payloads are delivered on. It is
// buffered and lossy by design: a keepalive only needs to know that *a* pong
// arrived recently, and blocking the read loop to deliver the third redundant
// one would let a peer stall the connection by spamming pongs.
func (c *Conn) Pongs() <-chan []byte { return c.pongs }

// ---------------------------------------------------------------------------
// Handshake
// ---------------------------------------------------------------------------

// Upgrade completes the RFC 6455 opening handshake and takes the connection.
//
// Everything it checks is a MUST in §4.2.1. The failures are ordinary HTTP
// responses because at this point the connection is still HTTP and the client
// is still an HTTP client — a 400 with a readable body is far more use to
// whoever is debugging than a socket that closes.
func Upgrade(w http.ResponseWriter, r *http.Request, cfg ConnConfig, subprotocol string) (*Conn, error) {
	if r.Method != http.MethodGet {
		return nil, statusError(http.StatusMethodNotAllowed, "invalid_argument",
			"a websocket handshake must be a GET")
	}
	if !headerContainsToken(r.Header, "Connection", "upgrade") {
		return nil, errBadRequest("websocket handshake requires 'Connection: Upgrade'")
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return nil, errBadRequest("websocket handshake requires 'Upgrade: websocket'")
	}
	if v := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); v != "13" {
		// §4.4 says an unsupported version must be answered with the versions
		// this endpoint does support, so a client can downgrade rather than
		// guess.
		w.Header().Set("Sec-WebSocket-Version", "13")
		return nil, errBadRequest("websocket version %q is not supported; this endpoint speaks 13", v)
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		return nil, errBadRequest("Sec-WebSocket-Key must be 16 base64-encoded bytes")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errInternal("this server does not support connection hijacking")
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, errInternal("hijacking the connection: %v", err)
	}

	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: " + ComputeAcceptKey(key) + "\r\n")
	if subprotocol != "" {
		b.WriteString("Sec-WebSocket-Protocol: " + subprotocol + "\r\n")
	}
	b.WriteString("\r\n")

	// A deadline on the handshake write: a client that completes the TCP
	// handshake and then never reads would otherwise hold a goroutine here.
	_ = netConn.SetWriteDeadline(time.Now().Add(cfg.withDefaults().WriteTimeout))
	if _, err := io.WriteString(netConn, b.String()); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("apigw: writing the websocket handshake: %w", err)
	}
	_ = netConn.SetWriteDeadline(time.Time{})
	return newConn(netConn, brw, cfg), nil
}

// headerContainsToken reports whether a comma-separated header contains a
// token, case-insensitively. "Connection: keep-alive, Upgrade" is legal and a
// plain string comparison misses it.
func headerContainsToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// frame is one parsed WebSocket frame.
type frame struct {
	fin     bool
	opcode  Opcode
	payload []byte
}

// readFrame parses one frame, unmasking the payload in place.
func (c *Conn) readFrame() (frame, error) {
	if c.cfg.ReadTimeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout)); err != nil {
			return frame{}, err
		}
	}
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return frame{}, err
	}
	fin := head[0]&0x80 != 0
	rsv := head[0] & 0x70
	opcode := Opcode(head[0] & 0x0f)
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7f)

	if rsv != 0 {
		// No extension was negotiated, so a set reserved bit means the peer is
		// speaking a protocol this endpoint did not agree to.
		return frame{}, protocolError("reserved bits set with no negotiated extension")
	}
	switch opcode {
	case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:
	default:
		return frame{}, protocolError("reserved opcode 0x%x", byte(opcode))
	}
	if opcode.isControl() {
		if !fin {
			return frame{}, protocolError("control frame 0x%x is fragmented", byte(opcode))
		}
		if length > maxControlPayload {
			return frame{}, protocolError("control frame payload is %d bytes, the maximum is %d",
				length, maxControlPayload)
		}
	}

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return frame{}, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return frame{}, err
		}
		v := binary.BigEndian.Uint64(ext[:])
		// §5.2: the most significant bit of a 64-bit length must be 0.
		if v&(1<<63) != 0 {
			return frame{}, protocolError("64-bit payload length has the high bit set")
		}
		length = int64(v)
	}
	if length > c.cfg.MaxMessageBytes {
		return frame{}, &CloseError{Code: CloseMessageTooBig,
			Reason: fmt.Sprintf("frame of %d bytes exceeds the %d byte limit", length, c.cfg.MaxMessageBytes)}
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
			return frame{}, err
		}
	} else if !c.cfg.Mask {
		// This endpoint is the server, so the peer is a client, and §5.1
		// requires every client frame to be masked. Accepting an unmasked one
		// is the cache-poisoning hole masking exists to close.
		return frame{}, protocolError("client frame is not masked")
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return frame{}, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return frame{fin: fin, opcode: opcode, payload: payload}, nil
}

func protocolError(format string, args ...any) *CloseError {
	return &CloseError{Code: CloseProtocolError, Reason: fmt.Sprintf(format, args...)}
}

// ReadMessage reads one complete application message, reassembling fragments
// and handling control frames along the way.
//
// A ping is answered with a pong immediately, from inside the read loop: RFC
// 6455 says "as soon as is practical", and deferring it to an application
// goroutine means a peer whose application is busy looks dead to its peer's
// keepalive. A pong is offered to [Conn.Pongs] and dropped if nobody is
// listening. A close frame ends the read with a [CloseError] after the closing
// frame has been echoed, which is the server's half of the close handshake.
func (c *Conn) ReadMessage() (Opcode, []byte, error) {
	var (
		msgOp   Opcode
		buf     []byte
		started bool
	)
	for {
		f, err := c.readFrame()
		if err != nil {
			var ce *CloseError
			if errors.As(err, &ce) {
				// A protocol violation is reported to the peer before the
				// connection goes away, so the other side's logs say why.
				_ = c.writeClose(ce.Code, ce.Reason)
			}
			return 0, nil, err
		}

		if f.opcode.isControl() {
			switch f.opcode {
			case OpPing:
				if err := c.WritePong(f.payload); err != nil {
					return 0, nil, err
				}
			case OpPong:
				select {
				case c.pongs <- f.payload:
				default:
				}
			case OpClose:
				code, reason, perr := parseClosePayload(f.payload)
				if perr != nil {
					_ = c.writeClose(CloseProtocolError, perr.Error())
					return 0, nil, perr
				}
				// Echo the code back, completing the handshake, then stop.
				_ = c.writeClose(code, "")
				return 0, nil, &CloseError{Code: code, Reason: reason}
			}
			continue
		}

		switch {
		case f.opcode == OpContinuation && !started:
			err := protocolError("continuation frame with no message in progress")
			_ = c.writeClose(err.Code, err.Reason)
			return 0, nil, err
		case f.opcode != OpContinuation && started:
			err := protocolError("new %s frame while a message is still fragmented", f.opcode)
			_ = c.writeClose(err.Code, err.Reason)
			return 0, nil, err
		case f.opcode != OpContinuation:
			msgOp = f.opcode
			started = true
		}

		if int64(len(buf))+int64(len(f.payload)) > c.cfg.MaxMessageBytes {
			err := &CloseError{Code: CloseMessageTooBig,
				Reason: fmt.Sprintf("reassembled message exceeds the %d byte limit", c.cfg.MaxMessageBytes)}
			_ = c.writeClose(err.Code, err.Reason)
			return 0, nil, err
		}
		buf = append(buf, f.payload...)

		if f.fin {
			if msgOp == OpText && !utf8.Valid(buf) {
				err := &CloseError{Code: CloseInvalidPayload, Reason: "text message is not valid UTF-8"}
				_ = c.writeClose(err.Code, err.Reason)
				return 0, nil, err
			}
			return msgOp, buf, nil
		}
	}
}

// parseClosePayload decodes a close frame body.
func parseClosePayload(payload []byte) (CloseCode, string, error) {
	switch {
	case len(payload) == 0:
		// §7.1.5: a close with no body means "no status received"; 1000 is the
		// right thing to echo.
		return CloseNormal, "", nil
	case len(payload) == 1:
		return 0, "", protocolError("close payload of 1 byte cannot hold a status code")
	}
	code := CloseCode(binary.BigEndian.Uint16(payload[:2]))
	reason := string(payload[2:])
	if !utf8.ValidString(reason) {
		return 0, "", &CloseError{Code: CloseInvalidPayload, Reason: "close reason is not valid UTF-8"}
	}
	if !validCloseCode(code) {
		return 0, "", protocolError("close code %d is not one an endpoint may send", code)
	}
	return code, reason, nil
}

// validCloseCode implements §7.4.2: 1000–1003, 1007–1011 and 1012–1014 are
// defined or registered; 1004, 1005, 1006 and 1015 must never appear on the
// wire; 3000–4999 are for applications and libraries.
func validCloseCode(c CloseCode) bool {
	switch {
	case c >= 3000 && c <= 4999:
		return true
	case c >= 1000 && c <= 1003:
		return true
	case c >= 1007 && c <= 1014:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// WriteMessage writes one unfragmented message.
func (c *Conn) WriteMessage(op Opcode, payload []byte) error {
	return c.writeFrame(true, op, payload)
}

// WriteFragments writes one message split across frames.
//
// The gateway itself never fragments — its events are small JSON objects — but
// the capability is here because the protocol requires an endpoint to be able
// to produce what it must be able to consume, and the tests exercise both
// directions with it.
func (c *Conn) WriteFragments(op Opcode, chunks [][]byte) error {
	if len(chunks) == 0 {
		return c.writeFrame(true, op, nil)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	for i, chunk := range chunks {
		frameOp := OpContinuation
		if i == 0 {
			frameOp = op
		}
		if err := c.writeFrameLocked(i == len(chunks)-1, frameOp, chunk); err != nil {
			return err
		}
	}
	return nil
}

// WritePing sends a ping.
func (c *Conn) WritePing(payload []byte) error { return c.writeFrame(true, OpPing, payload) }

// WritePong sends a pong.
func (c *Conn) WritePong(payload []byte) error { return c.writeFrame(true, OpPong, payload) }

func (c *Conn) writeFrame(fin bool, op Opcode, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrameLocked(fin, op, payload)
}

func (c *Conn) writeFrameLocked(fin bool, op Opcode, payload []byte) error {
	if op.isControl() && len(payload) > maxControlPayload {
		return fmt.Errorf("websocket: %s payload of %d bytes exceeds the %d byte control limit",
			op, len(payload), maxControlPayload)
	}
	if c.cfg.WriteTimeout > 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout)); err != nil {
			return err
		}
	}

	var head [14]byte
	n := 2
	head[0] = byte(op)
	if fin {
		head[0] |= 0x80
	}
	length := len(payload)
	switch {
	case length <= 125:
		head[1] = byte(length)
	case length <= 0xffff:
		head[1] = 126
		binary.BigEndian.PutUint16(head[2:4], uint16(length))
		n = 4
	default:
		head[1] = 127
		binary.BigEndian.PutUint64(head[2:10], uint64(length))
		n = 10
	}

	var maskKey [4]byte
	if c.cfg.Mask {
		head[1] |= 0x80
		if _, err := rand.Read(maskKey[:]); err != nil {
			return fmt.Errorf("websocket: generating a mask key: %w", err)
		}
		copy(head[n:n+4], maskKey[:])
		n += 4
	}
	if _, err := c.bw.Write(head[:n]); err != nil {
		return err
	}
	if length > 0 {
		if c.cfg.Mask {
			// Mask into a copy: the caller's buffer may be a shared,
			// pre-serialised event being written to several connections.
			masked := make([]byte, length)
			for i := range payload {
				masked[i] = payload[i] ^ maskKey[i%4]
			}
			if _, err := c.bw.Write(masked); err != nil {
				return err
			}
		} else if _, err := c.bw.Write(payload); err != nil {
			return err
		}
	}
	return c.bw.Flush()
}

// writeClose sends a close frame at most once per connection.
func (c *Conn) writeClose(code CloseCode, reason string) error {
	c.closeMu.Lock()
	if c.closeSent || c.closed {
		c.closeMu.Unlock()
		return nil
	}
	c.closeSent = true
	c.closeMu.Unlock()

	if len(reason) > maxControlPayload-2 {
		reason = reason[:maxControlPayload-2]
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)
	return c.writeFrame(true, OpClose, payload)
}

// WriteClose sends a close frame without waiting for the peer's reply.
func (c *Conn) WriteClose(code CloseCode, reason string) error { return c.writeClose(code, reason) }

// CloseWithHandshake performs the closing handshake and then closes the socket.
//
// It sends a close frame, then drains inbound frames until the peer's close
// arrives or the deadline passes, and only then closes the TCP connection.
// Skipping the drain is the common shortcut and it is why so many WebSocket
// clients report "connection reset" on an orderly server shutdown: the
// server's own close frame is still in the socket buffer when the RST
// overtakes it.
//
// Because it reads, the caller must be the connection's only reader. Where a
// separate goroutine owns reading — as it does for the live stream — that
// goroutine performs the drain instead; see [Gateway.closeStream].
func (c *Conn) CloseWithHandshake(code CloseCode, reason string, wait time.Duration) error {
	writeErr := c.writeClose(code, reason)
	deadline := time.Now().Add(wait)
	_ = c.conn.SetReadDeadline(deadline)
	for time.Now().Before(deadline) {
		f, err := c.readFrame()
		if err != nil {
			break
		}
		if f.opcode == OpClose {
			break
		}
	}
	if err := c.Close(); err != nil && writeErr == nil {
		writeErr = err
	}
	return writeErr
}

// Close closes the underlying connection. It is safe to call more than once.
func (c *Conn) Close() error {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil
	}
	c.closed = true
	c.closeMu.Unlock()
	return c.conn.Close()
}

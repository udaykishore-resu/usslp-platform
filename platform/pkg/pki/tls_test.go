package pki

import (
	"crypto"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// handshake runs a real TLS exchange over a loopback listener and reports what
// each side saw. Both sides exchange a few bytes after the handshake because in
// TLS 1.3 the client finishes its handshake before the server has seen the
// client certificate: a rejection surfaces on the client's next read, not on
// Handshake, and a test that only called Handshake would pass regardless.
type handshakeOutcome struct {
	serverErr  error
	clientErr  error
	serverPeer Identity
	version    uint16
}

func handshake(t *testing.T, serverCfg, clientCfg *tls.Config) handshakeOutcome {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type serverResult struct {
		err     error
		peer    Identity
		version uint16
	}
	done := make(chan serverResult, 1)

	go func() {
		raw, err := ln.Accept()
		if err != nil {
			done <- serverResult{err: err}
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
		conn := tls.Server(raw, serverCfg)
		if err := conn.Handshake(); err != nil {
			done <- serverResult{err: err}
			return
		}
		state := conn.ConnectionState()
		peer, idErr := PeerIdentity(&state)
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			done <- serverResult{err: err, peer: peer, version: state.Version}
			return
		}
		if _, err := conn.Write([]byte("pong")); err != nil {
			done <- serverResult{err: err, peer: peer, version: state.Version}
			return
		}
		done <- serverResult{err: idErr, peer: peer, version: state.Version}
	}()

	var clientErr error
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn := tls.Client(raw, clientCfg)
	if clientErr = conn.Handshake(); clientErr == nil {
		if _, clientErr = conn.Write([]byte("ping")); clientErr == nil {
			buf := make([]byte, 4)
			_, clientErr = io.ReadFull(conn, buf)
		}
	}

	res := <-done
	return handshakeOutcome{serverErr: res.err, clientErr: clientErr, serverPeer: res.peer, version: res.version}
}

// serverTLS builds a server config presenting a service certificate.
func serverTLS(t *testing.T, h *Hierarchy, opts TLSOptions) (*tls.Config, string) {
	t.Helper()
	issued, key, err := h.IssueService("usslp-prod", "label-service")
	if err != nil {
		t.Fatalf("issue service certificate: %v", err)
	}
	cert, err := issued.TLSCertificate(key)
	if err != nil {
		t.Fatalf("assemble server tls certificate: %v", err)
	}
	cfg, err := h.ServerTLSConfig(cert, opts)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	return cfg, "label-service.usslp-prod.svc." + h.TrustDomain()
}

// clientTLS builds a client config presenting an already-issued certificate.
func clientTLS(t *testing.T, h *Hierarchy, issued *Issued, key crypto.Signer, opts TLSOptions) *tls.Config {
	t.Helper()
	cert, err := issued.TLSCertificate(key)
	if err != nil {
		t.Fatalf("assemble client tls certificate: %v", err)
	}
	cfg, err := h.ClientTLSConfig(cert, opts)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	return cfg
}

// TestMutualTLSHandshakeSucceeds is the happy path: two USSLP peers from the
// same hierarchy, each verifying the other.
func TestMutualTLSHandshakeSucceeds(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	serverCfg, serverName := serverTLS(t, h, TLSOptions{Predicate: RequireTenant(tenantA)})
	if serverCfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#04x, want TLS 1.3", serverCfg.MinVersion)
	}
	if serverCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", serverCfg.ClientAuth)
	}
	if !serverCfg.SessionTicketsDisabled {
		t.Error("session tickets are enabled; resumption would skip revocation checking")
	}

	sec, secKey, err := h.IssueSEC(tenantA, store1, "sec-handshake")
	if err != nil {
		t.Fatalf("issue sec: %v", err)
	}
	clientCfg := clientTLS(t, h, sec, secKey, TLSOptions{
		ServerName: serverName,
		Predicate:  RequireService("usslp-prod", "label-service"),
	})

	out := handshake(t, serverCfg, clientCfg)
	if out.serverErr != nil {
		t.Fatalf("server: %v", out.serverErr)
	}
	if out.clientErr != nil {
		t.Fatalf("client: %v", out.clientErr)
	}
	if out.version != tls.VersionTLS13 {
		t.Errorf("negotiated version %#04x, want TLS 1.3", out.version)
	}
	if out.serverPeer.Kind != KindSEC {
		t.Errorf("server saw peer kind %q, want sec", out.serverPeer.Kind)
	}
	if out.serverPeer.TenantID != tenantA || out.serverPeer.StoreID != store1 {
		t.Errorf("server saw %s/%s, want %s/%s",
			out.serverPeer.TenantID, out.serverPeer.StoreID, tenantA, store1)
	}
	if out.serverPeer.DeviceID != "sec-handshake" {
		t.Errorf("server saw device %q, want sec-handshake", out.serverPeer.DeviceID)
	}
}

// TestMutualTLSRejectsForeignHierarchy proves a shared certificate shape is not
// shared trust: a perfectly valid certificate from another PKI does not get in.
func TestMutualTLSRejectsForeignHierarchy(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)
	foreign := foreignHierarchy(t)

	serverCfg, serverName := serverTLS(t, h, TLSOptions{})

	intruder, intruderKey, err := foreign.IssueSEC(tenantA, store1, "sec-intruder")
	if err != nil {
		t.Fatalf("issue foreign sec: %v", err)
	}
	// The client still trusts this platform's root — only its own certificate
	// comes from elsewhere — so the failure is unambiguously about the client
	// credential rather than about the server's.
	clientCfg := clientTLS(t, h, intruder, intruderKey, TLSOptions{ServerName: serverName})
	// A well-behaved Go client notices that its certificate does not chain to
	// any authority the server named and declines to send it at all, which
	// would test the client's manners rather than the server's checks. An
	// attacker has no such manners, so the certificate is offered regardless.
	offered := clientCfg.Certificates[0]
	clientCfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &offered, nil
	}

	out := handshake(t, serverCfg, clientCfg)
	if out.serverErr == nil {
		t.Fatal("server accepted a client certificate from a different hierarchy")
	}
	if !strings.Contains(out.serverErr.Error(), "unknown authority") &&
		!errors.Is(out.serverErr, ErrUnknownAuthority) {
		t.Errorf("server error = %v, want an unknown-authority rejection", out.serverErr)
	}
	if out.clientErr == nil {
		t.Error("client believed the connection succeeded after the server rejected it")
	}
}

// TestMutualTLSRejectsCrossTenantClient is tenant isolation at the transport
// layer: both peers are genuine USSLP devices under the same root, and the
// connection is still refused.
func TestMutualTLSRejectsCrossTenantClient(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	serverCfg, serverName := serverTLS(t, h, TLSOptions{
		Predicate: AllPredicates(RequireTenant(tenantA), RequireKind(KindSEC, KindSGU)),
	})

	other, otherKey, err := h.IssueSEC(tenantB, store2, "sec-neighbour")
	if err != nil {
		t.Fatalf("issue sec: %v", err)
	}
	clientCfg := clientTLS(t, h, other, otherKey, TLSOptions{ServerName: serverName})

	out := handshake(t, serverCfg, clientCfg)
	if !errors.Is(out.serverErr, ErrIdentityRejected) {
		t.Fatalf("server error = %v, want ErrIdentityRejected", out.serverErr)
	}
	if out.clientErr == nil {
		t.Error("client believed the connection succeeded after the server rejected it")
	}

	// The same client is accepted by an endpoint that serves its own tenant,
	// so the rejection is about the predicate and not about the certificate.
	ownServerCfg, ownServerName := serverTLS(t, h, TLSOptions{Predicate: RequireTenant(tenantB)})
	ownClientCfg := clientTLS(t, h, other, otherKey, TLSOptions{ServerName: ownServerName})
	if out := handshake(t, ownServerCfg, ownClientCfg); out.serverErr != nil || out.clientErr != nil {
		t.Fatalf("same client rejected by its own tenant's endpoint: server=%v client=%v",
			out.serverErr, out.clientErr)
	}
}

// TestMutualTLSRejectsRevokedClient closes the loop between the revocation
// registry and a live connection.
func TestMutualTLSRejectsRevokedClient(t *testing.T) {
	t.Parallel()

	p := TestProfile()
	h, err := Bootstrap(BootstrapConfig{Profile: &p})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	serverCfg, serverName := serverTLS(t, h, TLSOptions{})

	sgu, sguKey, err := h.IssueSGU(tenantA, store1, "sgu-revoked")
	if err != nil {
		t.Fatalf("issue sgu: %v", err)
	}
	clientCfg := clientTLS(t, h, sgu, sguKey, TLSOptions{ServerName: serverName})

	if out := handshake(t, serverCfg, clientCfg); out.serverErr != nil {
		t.Fatalf("handshake failed before revocation: %v", out.serverErr)
	}
	if err := h.RevokeCertificate(sgu.Certificate, ReasonKeyCompromise, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	out := handshake(t, serverCfg, clientCfg)
	if !errors.Is(out.serverErr, ErrRevoked) {
		t.Fatalf("server error = %v, want ErrRevoked", out.serverErr)
	}
}

// TestMutualTLSRejectsExpiredClient covers the temporal case over a live
// connection rather than through VerifyChain alone.
func TestMutualTLSRejectsExpiredClient(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	serverCfg, serverName := serverTLS(t, h, TLSOptions{})
	now := time.Now()
	expired, key, err := h.IssueLeaf(NewSECIdentity(tenantA, store1, "sec-stale"), LeafOptions{
		NotBefore: now.Add(-2 * time.Hour),
		NotAfter:  now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("issue expired sec: %v", err)
	}
	clientCfg := clientTLS(t, h, expired, key, TLSOptions{ServerName: serverName})

	out := handshake(t, serverCfg, clientCfg)
	if out.serverErr == nil {
		t.Fatal("server accepted an expired client certificate")
	}
}

// TestClientTLSConfigRequiresServerName refuses the configuration in which a
// client cannot tell which server it reached.
func TestClientTLSConfigRequiresServerName(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	issued, key, err := h.IssueSEC(tenantA, store1, "sec-noservername")
	if err != nil {
		t.Fatalf("issue sec: %v", err)
	}
	cert, err := issued.TLSCertificate(key)
	if err != nil {
		t.Fatalf("tls certificate: %v", err)
	}
	if _, err := h.ClientTLSConfig(cert, TLSOptions{}); err == nil {
		t.Fatal("ClientTLSConfig accepted an empty ServerName")
	}
	if _, err := h.ServerTLSConfig(tls.Certificate{}, TLSOptions{}); err == nil {
		t.Fatal("ServerTLSConfig accepted an empty certificate")
	}
}

// TestTLSCertificateRejectsMismatchedKey catches the pairing mistake that would
// otherwise surface as an opaque handshake failure in production.
func TestTLSCertificateRejectsMismatchedKey(t *testing.T) {
	t.Parallel()
	h := testHierarchy(t)

	a, _, err := h.IssueSEC(tenantA, store1, "sec-key-a")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	_, keyB, err := h.IssueSEC(tenantA, store1, "sec-key-b")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := a.TLSCertificate(keyB); err == nil {
		t.Fatal("assembled a tls.Certificate from a mismatched key and certificate")
	}
}

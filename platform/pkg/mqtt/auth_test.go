package mqtt

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/msgbus"
)

func acmeAuthorizer() *TenantAuthorizer {
	return &TenantAuthorizer{Credentials: map[string]string{
		"acme":         "acme-secret",
		"acme:gateway": "gateway-secret",
		"globex":       "globex-secret",
	}}
}

func TestTenantAuthorizerAuthenticate(t *testing.T) {
	a := acmeAuthorizer()
	cases := []struct {
		name     string
		username string
		password string
		wantErr  bool
	}{
		{"known tenant", "acme", "acme-secret", false},
		{"tenant sub-account", "acme:gateway", "gateway-secret", false},
		{"wrong password", "acme", "hunter2", true},
		{"unknown username", "nobody", "", true},
		{"anonymous", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := a.Authenticate("c1", tc.username, []byte(tc.password))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Authenticate(%q) = %v, wantErr %v", tc.username, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrNotAuthenticated) {
				t.Errorf("error %v does not wrap ErrNotAuthenticated", err)
			}
		})
	}
}

func TestTenantAuthorizerConfinesToItsNamespace(t *testing.T) {
	a := acmeAuthorizer()
	cases := []struct {
		name   string
		topic  string
		action Action
		allow  bool
	}{
		{"own label topic", "usslp/acme/eu-west-1/store-7/labels/L1/price", ActionPublish, true},
		{"own zone filter", "usslp/acme/eu-west-1/store-7/labels/+/price", ActionSubscribe, true},
		{"own tenant wildcard", "usslp/acme/#", ActionSubscribe, true},
		{"own tenant root", "usslp/acme", ActionSubscribe, true},
		{"another tenant", "usslp/globex/eu-west-1/store-7/labels/L1/price", ActionPublish, false},
		{"another tenant filter", "usslp/globex/#", ActionSubscribe, false},
		// The filters that look scoped but are not: a wildcard at the tenant
		// level matches every tenant on the broker.
		{"wildcard tenant level", "usslp/+/eu-west-1/#", ActionSubscribe, false},
		{"multi-level at the root", "#", ActionSubscribe, false},
		{"multi-level below the root", "usslp/#", ActionSubscribe, false},
		{"prefix collision", "usslp/acme-evil/x", ActionPublish, false},
		{"outside the namespace", "internal/metrics", ActionPublish, false},
		{"bare topic", "usslp", ActionPublish, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := a.Authorize("sec-1", "acme", tc.topic, tc.action)
			if tc.allow && err != nil {
				t.Fatalf("Authorize(%q) = %v, want allowed", tc.topic, err)
			}
			if !tc.allow {
				if err == nil {
					t.Fatalf("Authorize(%q) allowed a cross-tenant %s", tc.topic, tc.action)
				}
				if !errors.Is(err, ErrNotAuthorized) {
					t.Errorf("error %v does not wrap ErrNotAuthorized", err)
				}
			}
		})
	}
}

func TestTenantAuthorizerSubAccountKeepsTenantScope(t *testing.T) {
	a := acmeAuthorizer()
	if err := a.Authorize("gw-1", "acme:gateway", "usslp/acme/r/s/store/planogram/update", ActionPublish); err != nil {
		t.Errorf("a tenant sub-account was denied its own namespace: %v", err)
	}
	if err := a.Authorize("gw-1", "acme:gateway", "usslp/globex/r/s/store/planogram/update", ActionPublish); err == nil {
		t.Error("a tenant sub-account reached another tenant's namespace")
	}
}

// TestBrokerDeniesCrossTenantSubscribe checks the SUBACK failure path: a
// refused filter is reported per-filter, and the connection survives so the
// filters the client may have still work.
func TestBrokerDeniesCrossTenantSubscribe(t *testing.T) {
	_, addr := startBroker(t, Options{Authorizer: acmeAuthorizer()})

	c := dialRaw(t, addr)
	c.connect(&connectPacket{ClientID: "sec-1", CleanSession: true,
		HasUsername: true, Username: "acme", HasPassword: true, Password: []byte("acme-secret")})

	c.write(&subscribePacket{PacketID: 1, Filters: []topicFilter{
		{Filter: "usslp/globex/#", QoS: msgbus.AtLeastOnce},
		{Filter: "usslp/acme/#", QoS: msgbus.AtLeastOnce},
	}})
	sa, ok := c.mustRead().(*subackPacket)
	if !ok {
		t.Fatal("expected SUBACK")
	}
	if len(sa.Codes) != 2 {
		t.Fatalf("SUBACK carried %d codes, want 2", len(sa.Codes))
	}
	if sa.Codes[0] != subackFailure {
		t.Errorf("cross-tenant filter granted 0x%02x, want 0x80", sa.Codes[0])
	}
	if sa.Codes[1] != byte(msgbus.AtLeastOnce) {
		t.Errorf("own-tenant filter granted 0x%02x, want QoS 1", sa.Codes[1])
	}

	// The permitted filter really works, and the refused one really does not.
	other := dialRaw(t, addr)
	other.connect(&connectPacket{ClientID: "globex-1", CleanSession: true,
		HasUsername: true, Username: "globex", HasPassword: true, Password: []byte("globex-secret")})
	other.write(&publishPacket{QoS: msgbus.AtMostOnce,
		Topic: "usslp/globex/r/s/labels/L1/price", Payload: []byte("999")})

	if _, err := c.read(300 * time.Millisecond); err == nil {
		t.Error("a refused subscription still delivered another tenant's traffic")
	}
}

// TestBrokerDisconnectsCrossTenantPublish covers the other half: MQTT 3.1.1 has
// no in-band way to refuse a PUBLISH, so the broker closes the connection.
func TestBrokerDisconnectsCrossTenantPublish(t *testing.T) {
	_, addr := startBroker(t, Options{Authorizer: acmeAuthorizer()})

	c := dialRaw(t, addr)
	c.connect(&connectPacket{ClientID: "sec-1", CleanSession: true,
		HasUsername: true, Username: "acme", HasPassword: true, Password: []byte("acme-secret")})

	// A legal publish first, so the failure below cannot be blamed on the setup.
	c.write(&publishPacket{QoS: msgbus.AtLeastOnce, PacketID: 1,
		Topic: "usslp/acme/r/s/labels/L1/price", Payload: []byte("399")})
	if p := c.mustRead(); p.(*ackPacket).Type != pktPUBACK {
		t.Fatalf("own-tenant publish was not acknowledged: got %s", p.pktType())
	}

	c.write(&publishPacket{QoS: msgbus.AtLeastOnce, PacketID: 2,
		Topic: "usslp/globex/r/s/labels/L1/price", Payload: []byte("999")})
	if _, err := c.read(testTimeout); err == nil {
		t.Fatal("a cross-tenant publish left the connection open")
	}
}

func TestBrokerRejectsBadCredentials(t *testing.T) {
	_, addr := startBroker(t, Options{Authorizer: acmeAuthorizer()})
	c := dialRaw(t, addr)
	c.write(&connectPacket{ProtocolName: protocolName, ProtocolLevel: protocolLevel,
		ClientID: "sec-1", CleanSession: true,
		HasUsername: true, Username: "acme", HasPassword: true, Password: []byte("wrong")})
	p, err := c.read(testTimeout)
	if err != nil {
		t.Fatalf("reading CONNACK: %v", err)
	}
	ack, ok := p.(*connackPacket)
	if !ok {
		t.Fatalf("got %s, want CONNACK", p.pktType())
	}
	if ack.ReturnCode != ConnectBadCredentials {
		t.Errorf("return code %s, want %s", ack.ReturnCode, ConnectBadCredentials)
	}

	// The typed client surfaces the same refusal to its caller.
	cfg := testConfig(addr, "sec-1")
	cfg.Username = "acme"
	cfg.Password = "wrong"
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_, dialErr := Dial(ctx, cfg)
	var ce *ConnectError
	if !errors.As(dialErr, &ce) {
		t.Fatalf("Dial returned %v, want a *ConnectError", dialErr)
	}
	if ce.Code != ConnectBadCredentials {
		t.Errorf("ConnectError carried %s, want %s", ce.Code, ConnectBadCredentials)
	}
}

func TestBrokerRejectsUnauthorizedWillTopic(t *testing.T) {
	_, addr := startBroker(t, Options{Authorizer: acmeAuthorizer()})
	c := dialRaw(t, addr)
	c.write(&connectPacket{ProtocolName: protocolName, ProtocolLevel: protocolLevel,
		ClientID: "sec-1", CleanSession: true,
		HasUsername: true, Username: "acme", HasPassword: true, Password: []byte("acme-secret"),
		WillFlag: true, WillTopic: "usslp/globex/r/s/sec/S1/status", WillMessage: []byte("offline")})
	p, err := c.read(testTimeout)
	if err != nil {
		t.Fatalf("reading CONNACK: %v", err)
	}
	if ack := p.(*connackPacket); ack.ReturnCode != ConnectNotAuthorized {
		t.Errorf("return code %s, want %s", ack.ReturnCode, ConnectNotAuthorized)
	}
}

func TestClientSubscribeReportsRefusal(t *testing.T) {
	_, addr := startBroker(t, Options{Authorizer: acmeAuthorizer()})
	cfg := testConfig(addr, "sec-1")
	cfg.Username = "acme"
	cfg.Password = "acme-secret"
	c := dialClient(t, cfg)

	err := c.Subscribe(context.Background(), "usslp/globex/#", msgbus.AtLeastOnce,
		func(context.Context, msgbus.Message) {})
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("Subscribe to another tenant returned %v, want ErrNotAuthorized", err)
	}
	// A refused subscription must not leave a handler registered behind it.
	c.mu.Lock()
	_, leaked := c.subs["usslp/globex/#"]
	c.mu.Unlock()
	if leaked {
		t.Error("the client kept a handler for a filter the broker refused")
	}
}

// ---------------------------------------------------------------------------
// mTLS
// ---------------------------------------------------------------------------

// testPKI is a throwaway certificate authority: enough of one to prove that the
// broker exposes the peer certificate to the Authorizer and that a device
// certificate can therefore be the tenant identity.
type testPKI struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	pool   *x509.CertPool
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "usslp-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testPKI{caCert: cert, caKey: key, pool: pool}
}

// issue mints a leaf certificate. org becomes the certificate's Organization,
// which is where DefaultTenantOf looks for the tenant.
func (p *testPKI) issue(t *testing.T, commonName, org string, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if org != "" {
		tmpl.Subject.Organization = []string{org}
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.DNSNames = []string{"localhost"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatalf("issuing certificate for %s: %v", commonName, err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestMutualTLSCertificateIsTheTenantIdentity is the production posture: no
// passwords, a device certificate per SEC, and the tenant taken from the
// certificate's subject rather than from anything the device claims.
func TestMutualTLSCertificateIsTheTenantIdentity(t *testing.T) {
	pki := newTestPKI(t)
	serverCert := pki.issue(t, "sgu-17.store-17.acme", "acme", true)

	b := NewBroker(Options{
		Addr:       "127.0.0.1:0",
		Authorizer: &TenantAuthorizer{},
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pki.pool,
			MinVersion:   tls.VersionTLS12,
		},
	})
	addr, err := b.Start()
	if err != nil {
		t.Fatalf("starting TLS broker: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		b.Shutdown(ctx)
	})

	clientTLS := func(cert tls.Certificate) *tls.Config {
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pki.pool,
			ServerName:   "localhost",
			MinVersion:   tls.VersionTLS12,
		}
	}

	cfg := msgbus.Config{
		BrokerURL:      "tls://" + addr.String(),
		ClientID:       "sec-0042",
		CleanSession:   true,
		ConnectTimeout: testTimeout,
		AckTimeout:     2 * time.Second,
		TLSConfig:      clientTLS(pki.issue(t, "sec-0042.store-17.acme", "acme", false)),
	}
	sub := dialClient(t, cfg)

	got := newCollector()
	ctx := context.Background()
	if err := sub.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, got.handle); err != nil {
		t.Fatalf("subscribing over mTLS: %v", err)
	}
	if err := sub.Publish(ctx, msgbus.Message{Topic: priceTopic, Payload: []byte("399"),
		QoS: msgbus.AtLeastOnce}); err != nil {
		t.Fatalf("publishing over mTLS: %v", err)
	}
	if m := got.next(t); string(m.Payload) != "399" {
		t.Errorf("received %q, want 399", m.Payload)
	}

	// A certificate from another tenant is authenticated by the same CA and is
	// still confined to its own namespace.
	globexCfg := cfg
	globexCfg.ClientID = "sec-9001"
	globexCfg.TLSConfig = clientTLS(pki.issue(t, "sec-9001.store-3.globex", "globex", false))
	globex := dialClient(t, globexCfg)
	err = globex.Subscribe(ctx, zoneFilter, msgbus.AtLeastOnce, func(context.Context, msgbus.Message) {})
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a globex certificate subscribed to an acme filter: %v", err)
	}
}

func TestDefaultTenantOfPrefersTheCertificate(t *testing.T) {
	pki := newTestPKI(t)
	cert := pki.issue(t, "sec-0042.store-17.acme", "acme", false)
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parsing leaf: %v", err)
	}
	// The username claims another tenant; the certificate must win.
	got, err := DefaultTenantOf(ConnInfo{Username: "globex", PeerCertificate: leaf})
	if err != nil {
		t.Fatalf("DefaultTenantOf: %v", err)
	}
	if got != canon.TenantID("acme") {
		t.Errorf("tenant %q, want acme: a username must not override a certificate", got)
	}

	if _, err := DefaultTenantOf(ConnInfo{Username: "bad/tenant"}); err == nil {
		t.Error("a username containing a topic separator was accepted as a tenant")
	}
}

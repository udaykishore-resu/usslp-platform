package apigw

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/pki"
)

func TestAPIKeyFormatIsGreppableAndSelfIdentifying(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})

	if !strings.HasPrefix(key, KeyPrefixLive) {
		t.Fatalf("key %q does not carry the %s prefix; a leaked key must be findable by a secret scanner",
			key, KeyPrefixLive)
	}
	keyID, secret, err := splitKey(key)
	if err != nil {
		t.Fatalf("the issuer produced a key its own parser rejects: %v", err)
	}
	if len(keyID) != keyIDBytes*2 {
		t.Fatalf("key id is %d characters, want %d", len(keyID), keyIDBytes*2)
	}
	if len(secret) < 30 {
		t.Fatalf("secret half is only %d characters; it must carry the full %d bytes of entropy",
			len(secret), keySecretBytes)
	}
	rec, err := h.store.Lookup(context.Background(), keyID)
	if err != nil {
		t.Fatalf("the key id in the credential does not resolve to a record: %v", err)
	}
	if strings.Contains(string(rec.Hash), secret) || string(rec.Hash) == secret {
		t.Fatal("the stored verifier contains the secret")
	}
	if rec.KDF != KDFName {
		t.Fatalf("record kdf is %q, want %q", rec.KDF, KDFName)
	}
	if rec.ExpiresAt.IsZero() {
		t.Fatal("a key was issued with no expiry")
	}
}

func TestAPIKeyVerification(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	valid := h.issueKey("acme", []Role{RoleReadOnly})
	keyID, secret, _ := splitKey(valid)

	tests := []struct {
		name       string
		credential string
		wantStatus int
	}{
		{"valid key", valid, http.StatusOK},
		{"no credential", "", http.StatusUnauthorized},
		{"wrong secret", KeyPrefixLive + keyID + "_" + strings.Repeat("A", len(secret)), http.StatusUnauthorized},
		{"unknown key id", KeyPrefixLive + "00112233445566778_" + secret, http.StatusUnauthorized},
		{"missing prefix", keyID + "_" + secret, http.StatusUnauthorized},
		{"no secret segment", KeyPrefixLive + keyID, http.StatusUnauthorized},
		{"empty secret", KeyPrefixLive + keyID + "_", http.StatusUnauthorized},
		{"short key id", KeyPrefixLive + "abcd_" + secret, http.StatusUnauthorized},
		{"non-hex key id", KeyPrefixLive + strings.Repeat("z", 16) + "_" + secret, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := h.do(http.MethodGet, "/v1/me", tc.credential, nil)
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("got %d, want %d (body %s)", res.StatusCode, tc.wantStatus, bodyString(t, res))
			}
			if tc.wantStatus == http.StatusUnauthorized {
				if res.Header.Get("WWW-Authenticate") == "" {
					t.Error("a 401 must say what would satisfy it (RFC 7235)")
				}
				var body ErrorBody
				decodeBody(t, res, &body)
				// Every rejection must look identical from outside: an error
				// that distinguishes "unknown key" from "wrong secret" tells
				// an attacker which half of a leaked credential is real.
				if body.Code != "unauthenticated" {
					t.Errorf("error code %q leaks which check failed", body.Code)
				}
			}
		})
	}
}

func TestAPIKeyExpiryAndRevocation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	expiring := h.issueKey("acme", []Role{RoleReadOnly})
	revoked := h.issueKey("acme", []Role{RoleReadOnly})
	revokedID, _, _ := splitKey(revoked)

	if got := h.do(http.MethodGet, "/v1/me", expiring, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("a fresh key was refused: %d", got)
	}

	h.clock.Advance(DefaultKeyTTL + time.Minute)
	if got := h.do(http.MethodGet, "/v1/me", expiring, nil).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("an expired key was accepted: %d", got)
	}

	// The revoked key was issued at the same instant, so roll the clock back
	// far enough that expiry is not what refuses it.
	h.clock.Advance(-DefaultKeyTTL)
	if got := h.do(http.MethodGet, "/v1/me", revoked, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("key refused before revocation: %d", got)
	}
	if err := h.store.Revoke(context.Background(), "acme", revokedID, h.clock.Now()); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if got := h.do(http.MethodGet, "/v1/me", revoked, nil).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("a revoked key was accepted: %d", got)
	}
}

func TestAPIKeyRevocationIsTenantScoped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	victim := h.issueKey("beta", []Role{RoleOwner})
	victimID, _, _ := splitKey(victim)

	// Knowing another tenant's key id — from a log line, say — must not
	// confer the ability to revoke it.
	err := h.store.Revoke(context.Background(), "acme", victimID, h.clock.Now())
	if !errors.Is(err, ErrKeyUnknown) {
		t.Fatalf("cross-tenant revoke returned %v, want ErrKeyUnknown", err)
	}
	if got := h.do(http.MethodGet, "/v1/me", victim, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("the key was revoked across the tenant boundary: %d", got)
	}
}

// TestAPIKeyProductionKDF exercises the real work factor once, so a change to
// DefaultKDFIterations that breaks verification cannot pass on the cheap
// setting the rest of the suite uses.
func TestAPIKeyProductionKDF(t *testing.T) {
	t.Parallel()
	clock := newClock()
	issuer, err := NewKeyIssuer(KeyIssuerConfig{Store: NewMemoryKeyStore(), Now: clock.Now})
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	rec, plaintext, err := issuer.Issue(context.Background(), IssueRequest{
		TenantID: "acme", Name: "prod", Roles: []Role{RoleIntegration},
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if rec.Iterations != DefaultKDFIterations {
		t.Fatalf("record records %d iterations, want %d", rec.Iterations, DefaultKDFIterations)
	}
	got, err := issuer.Verify(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("verifying a key issued with the production KDF: %v", err)
	}
	if got.KeyID != rec.KeyID {
		t.Fatalf("verified %s, want %s", got.KeyID, rec.KeyID)
	}
}

// ---------------------------------------------------------------------------
// JWT
// ---------------------------------------------------------------------------

func TestJWTAuthenticationAndKeyRotation(t *testing.T) {
	t.Parallel()
	profile := pki.TestProfile()
	hierarchy, err := pki.Bootstrap(pki.BootstrapConfig{Profile: &profile})
	if err != nil {
		t.Fatalf("bootstrapping the pki: %v", err)
	}
	oldSigner, err := hierarchy.NewJWTSigner("usslp-tokens")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	newSigner, err := hierarchy.NewJWTSigner("usslp-tokens")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	keySet := pki.NewJWTKeySet()
	oldKID, err := keySet.AddSigner(oldSigner)
	if err != nil {
		t.Fatalf("adding a key: %v", err)
	}
	if _, err := keySet.AddSigner(newSigner); err != nil {
		t.Fatalf("adding a key: %v", err)
	}

	h := newHarness(t, func(o *harnessOptions) {
		o.auth = AuthConfig{JWTKeys: keySet, JWTIssuer: "usslp-tokens"}
	})

	mint := func(signer *pki.JWTSigner) string {
		token, err := signer.Sign(pki.JWTClaims{
			Subject:  "alice@acme.example",
			TenantID: "acme",
			Scopes:   []string{scopeRolePrefix + string(RoleStoreManager), scopeStorePrefix + "store-1", "custom:thing"},
		}, h.clock.Now())
		if err != nil {
			t.Fatalf("signing: %v", err)
		}
		return token
	}

	oldToken, newToken := mint(oldSigner), mint(newSigner)

	// Both keys are in the set: both tokens work. This is the overlap window
	// that makes rotation possible without invalidating live sessions.
	for name, token := range map[string]string{"old key": oldToken, "new key": newToken} {
		res := h.do(http.MethodGet, "/v1/me", token, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d, want 200 (%s)", name, res.StatusCode, bodyString(t, res))
		}
		var me MeResponse
		decodeBody(t, res, &me)
		if me.TenantID != "acme" {
			t.Fatalf("%s: tenant %q, want acme", name, me.TenantID)
		}
		if me.AuthMethod != CredJWT {
			t.Fatalf("%s: auth method %q, want jwt", name, me.AuthMethod)
		}
		if len(me.Roles) != 1 || me.Roles[0] != RoleStoreManager {
			t.Fatalf("%s: roles %v, want [store-manager] from the role: scope prefix", name, me.Roles)
		}
		if len(me.Stores) != 1 || me.Stores[0] != "store-1" {
			t.Fatalf("%s: stores %v, want [store-1] from the store: scope prefix", name, me.Stores)
		}
		if len(me.Scopes) != 1 || me.Scopes[0] != "custom:thing" {
			t.Fatalf("%s: scopes %v, want the unreserved scope preserved", name, me.Scopes)
		}
	}

	// Rotate: retire the old key. Tokens it signed stop working at once,
	// which is what makes removal from the key set a usable revocation.
	keySet.Remove(oldKID)
	if got := h.do(http.MethodGet, "/v1/me", oldToken, nil).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("a token signed by a retired key was accepted: %d", got)
	}
	if got := h.do(http.MethodGet, "/v1/me", newToken, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("rotation broke the surviving key: %d", got)
	}
}

func TestJWTExpiryAndIssuerAreEnforced(t *testing.T) {
	t.Parallel()
	profile := pki.TestProfile()
	hierarchy, err := pki.Bootstrap(pki.BootstrapConfig{Profile: &profile})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	good, err := hierarchy.NewJWTSigner("usslp-tokens")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	imposter, err := hierarchy.NewJWTSigner("someone-elses-idp")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	set := pki.NewJWTKeySet()
	if _, err := set.AddSigner(good); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := set.AddSigner(imposter); err != nil {
		t.Fatalf("add: %v", err)
	}

	h := newHarness(t, func(o *harnessOptions) {
		o.auth = AuthConfig{JWTKeys: set, JWTIssuer: "usslp-tokens", JWTLeeway: time.Second}
	})

	token, err := good.Sign(pki.JWTClaims{Subject: "bob", TenantID: "acme",
		Scopes: []string{scopeRolePrefix + string(RoleReadOnly)}}, h.clock.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if got := h.do(http.MethodGet, "/v1/me", token, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("fresh token refused: %d", got)
	}
	h.clock.Advance(pki.DefaultTokenTTL + time.Minute)
	if got := h.do(http.MethodGet, "/v1/me", token, nil).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("an expired token was accepted: %d", got)
	}

	h.clock.Advance(-(pki.DefaultTokenTTL + time.Minute))
	wrongIssuer, err := imposter.Sign(pki.JWTClaims{Subject: "mallory", TenantID: "acme",
		Scopes: []string{scopeRolePrefix + string(RoleOwner)}}, h.clock.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// The signature verifies — the key is in the set — and the token is still
	// refused, because the issuer is not the one this gateway trusts.
	if got := h.do(http.MethodGet, "/v1/me", wrongIssuer, nil).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("a validly signed token from the wrong issuer was accepted: %d", got)
	}
}

func TestJWTWithoutAConfiguredKeySetIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// A JWT-shaped credential presented to a gateway with no key set must be
	// refused rather than fall through to some other scheme.
	res := h.do(http.MethodGet, "/v1/me", "eyJhbGciOiJFUzI1NiJ9.e30.AAAA", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", res.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// mTLS
// ---------------------------------------------------------------------------

func issueTenantClient(t *testing.T, tenant canon.TenantID, client string) *x509.Certificate {
	t.Helper()
	profile := pki.TestProfile()
	hierarchy, err := pki.Bootstrap(pki.BootstrapConfig{Profile: &profile})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	issued, _, err := hierarchy.IssueTenantClient(tenant, client)
	if err != nil {
		t.Fatalf("issuing a tenant client certificate: %v", err)
	}
	return issued.Certificate
}

func TestMTLSTenantIdentityBecomesThePrincipal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cert := issueTenantClient(t, "acme", "pos-adapter")

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	// crypto/tls has already chain-verified the peer by the time a handler
	// runs; the gateway derives identity from the verified leaf.
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	rec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var me MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if me.TenantID != "acme" {
		t.Fatalf("tenant %q, want acme — it must come from the certificate", me.TenantID)
	}
	if me.AuthMethod != CredMTLS {
		t.Fatalf("auth method %q, want mtls", me.AuthMethod)
	}
	if len(me.Roles) != 1 || me.Roles[0] != RoleIntegration {
		t.Fatalf("roles %v, want [integration]", me.Roles)
	}
	if me.CredentialID == "" {
		t.Fatal("the certificate serial must be recorded so the credential can be revoked")
	}
}

func TestMTLSCertificateTakesPrecedenceOverABearerToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cert := issueTenantClient(t, "acme", "pos-adapter")
	otherTenantKey := h.issueKey("beta", []Role{RoleOwner})

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	req.Header.Set("Authorization", "Bearer "+otherTenantKey)
	rec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(rec, req)

	var me MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	if me.TenantID != "acme" {
		t.Fatalf("tenant %q: a bearer token on a mutually authenticated connection "+
			"must not override the certificate", me.TenantID)
	}
}

func TestDefaultCertPrincipalRefusesNonTenantIdentities(t *testing.T) {
	t.Parallel()
	profile := pki.TestProfile()
	hierarchy, err := pki.Bootstrap(pki.BootstrapConfig{Profile: &profile})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	svc, _, err := hierarchy.IssueService("usslp-prod", "label-service")
	if err != nil {
		t.Fatalf("issuing a service cert: %v", err)
	}
	label, _, err := hierarchy.IssueLabel("acme", "store-1", "lbl-1")
	if err != nil {
		t.Fatalf("issuing a label cert: %v", err)
	}
	for name, cert := range map[string]*x509.Certificate{
		"service": svc.Certificate,
		"label":   label.Certificate,
	} {
		id, err := pki.IdentityFromCertificate(cert)
		if err != nil {
			t.Fatalf("%s: extracting identity: %v", name, err)
		}
		if _, err := DefaultCertPrincipal(id); !errors.Is(err, pki.ErrIdentityRejected) {
			t.Fatalf("%s identity was admitted at the public gateway: %v", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// The tenant boundary
// ---------------------------------------------------------------------------

// TestCrossTenantLabelAccessIsNotFound is the isolation test the whole package
// exists to make true: tenant A, fully authenticated and holding every
// permission there is, asks for a label that belongs to tenant B and is told
// the label does not exist.
func TestCrossTenantLabelAccessIsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	labels := newTenantLabelStore()
	labels.put("beta", "lbl-beta-1", map[string]any{"label_id": "lbl-beta-1", "tenant_id": "beta"})
	labels.put("acme", "lbl-acme-1", map[string]any{"label_id": "lbl-acme-1", "tenant_id": "acme"})
	labels.install(h.stubs[UpstreamLabel])

	acme := h.issueKey("acme", []Role{RoleOwner})

	// Sanity: the tenant's own label is visible, so a 404 below is about
	// tenancy and not about the stub being empty.
	if got := h.do(http.MethodGet, "/v1/labels/lbl-acme-1", acme, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("own label: got %d, want 200", got)
	}

	res := h.do(http.MethodGet, "/v1/labels/lbl-beta-1", acme, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant label: got %d, want 404 — 403 would confirm the id exists",
			res.StatusCode)
	}
	var body ErrorBody
	decodeBody(t, res, &body)
	if body.Code != "not_found" {
		t.Fatalf("error code %q, want not_found", body.Code)
	}

	// And the mechanism: the upstream was asked on behalf of acme, so it could
	// not have found beta's label even if it wanted to.
	calls := h.stubs[UpstreamLabel].calls()
	last := calls[len(calls)-1]
	if got := last.Header.Get(HeaderTenant); got != "acme" {
		t.Fatalf("the upstream was called with tenant %q, want acme", got)
	}
}

func TestClientSuppliedTenantHeadersAreStripped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	acme := h.issueKey("acme", []Role{RoleOwner})

	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/v1/labels/lbl-1", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+acme)
	// Every header a caller might use to claim someone else's identity.
	req.Header.Set(HeaderTenant, "beta")
	req.Header.Set(HeaderSubject, "root")
	req.Header.Set(HeaderRoles, "owner")
	req.Header.Set(HeaderStores, "store-99")
	req.Header.Set(HeaderUpstream, "analytics-service")

	res, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()

	calls := h.stubs[UpstreamLabel].calls()
	if len(calls) == 0 {
		t.Fatal("the request never reached an upstream")
	}
	got := calls[len(calls)-1]
	if v := got.Header.Get(HeaderTenant); v != "acme" {
		t.Fatalf("upstream saw tenant %q; the client's claim of %q must not survive", v, "beta")
	}
	if v := got.Header.Get(HeaderSubject); !strings.HasPrefix(v, "key:") {
		t.Fatalf("upstream saw subject %q, want the credential's own subject", v)
	}
	if v := got.Header.Get(HeaderStores); v != "" {
		t.Fatalf("upstream saw a store scope of %q for an unscoped credential", v)
	}
	if v := got.Header.Get("Authorization"); v != "" {
		t.Fatal("the caller's credential was forwarded to an upstream")
	}
}

func TestPublicRoutesNeedNoCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, rt := range Routes() {
		if !rt.Public {
			continue
		}
		res := h.do(rt.Method, rt.Pattern, "", nil)
		if res.StatusCode >= http.StatusBadRequest {
			t.Errorf("%s: got %d without a credential, want a success", rt.Key(), res.StatusCode)
		}
	}
}

func TestAuthenticatedRoutesRefuseAnonymousCallers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, rt := range Routes() {
		if rt.Public || strings.Contains(rt.Pattern, "{") {
			continue
		}
		res := h.do(rt.Method, rt.Pattern, "", nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: got %d without a credential, want 401", rt.Key(), res.StatusCode)
		}
	}
}

func TestAuthorizationSchemeMustBeBearer(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/v1/me", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Basic YWRtaW46YWRtaW4=")
	res, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d for a Basic credential, want 401", res.StatusCode)
	}
}

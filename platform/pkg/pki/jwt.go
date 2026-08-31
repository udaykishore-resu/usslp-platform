package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ---------------------------------------------------------------------------
// Tenant JWT signing keys
//
// The Tenant Management authority issues two kinds of credential. A tenant's
// own systems authenticate to the Universal Integration Gateway with a client
// certificate (see Hierarchy.IssueTenantCert); the tokens the platform then
// mints for that tenant's users and sessions are signed with the ECDSA keys
// here and verified by every downstream service against a published JWKS.
//
// The implementation is deliberately the smallest thing that is correct, and it
// refuses rather than negotiates. ES256 is the only algorithm; the "alg" field
// in a presented token is checked against that constant and is never used to
// select a verification strategy, which is the root of the alg-confusion class
// of JWT vulnerabilities. Nothing is ever read out of a token before its
// signature has been checked — not the issuer, not the tenant, not the
// expiry — and the key is resolved by "kid" from a key set the verifier already
// holds, never from anything the token itself carries.
// ---------------------------------------------------------------------------

// JWT verification failures.
var (
	// ErrTokenMalformed means the token is not a well-formed compact JWS.
	ErrTokenMalformed = errors.New("pki: token is malformed")
	// ErrTokenSignature means the signature does not verify under the key the
	// token names.
	ErrTokenSignature = errors.New("pki: token signature does not verify")
	// ErrTokenExpired means the token is outside its validity window.
	ErrTokenExpired = errors.New("pki: token is expired or not yet valid")
	// ErrTokenClaims means the token verified but its claims do not satisfy the
	// caller's requirements.
	ErrTokenClaims = errors.New("pki: token claims rejected")
)

// JWTAlgorithm is the only signature algorithm the platform issues or accepts.
// ECDSA P-256 with SHA-256: small tokens, ubiquitous library support, and
// public verification keys that can be published without giving anyone the
// ability to mint tokens — which a shared HMAC secret would not.
const JWTAlgorithm = "ES256"

// JWTClaims is the platform's token payload.
//
// It is a struct rather than a map because every consumer of a USSLP token
// authorises on the same three things — who, which tenant, what scopes — and a
// map invites each service to invent its own spelling of "tenant".
type JWTClaims struct {
	// Issuer identifies the service that minted the token.
	Issuer string `json:"iss"`
	// Subject is the user or machine principal.
	Subject string `json:"sub"`
	// Audience is the service the token may be presented to.
	Audience string `json:"aud,omitempty"`
	// TenantID scopes every authorisation decision made with this token.
	TenantID canon.TenantID `json:"tenant_id,omitempty"`
	// Scopes are the permissions granted.
	Scopes []string `json:"scope,omitempty"`
	// IssuedAt, NotBefore and ExpiresAt are Unix seconds.
	IssuedAt  int64 `json:"iat"`
	NotBefore int64 `json:"nbf"`
	ExpiresAt int64 `json:"exp"`
	// TokenID is a unique identifier, so a token can be individually revoked
	// and so the audit stream can join a request to the credential that made
	// it.
	TokenID string `json:"jti"`
}

// HasScope reports whether the token grants a permission.
func (c JWTClaims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// jwtHeader is the JOSE header. There is no "jku", "jwk" or "x5u": a token must
// never be able to tell the verifier where to find the key that validates it.
type jwtHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

// JWTSigner mints tokens for a tenant with an ECDSA P-256 key held by the
// Tenant Management authority.
type JWTSigner struct {
	mu     sync.RWMutex
	kid    string
	key    *ecdsa.PrivateKey
	issuer string
	ttl    time.Duration
}

// DefaultTokenTTL is how long a minted token is valid.
//
// Fifteen minutes is short enough that a leaked token is worth little and long
// enough that a tenant's batch price import does not have to re-authenticate
// mid-run. Sessions outlive it by refreshing, which is a decision the token
// service makes, not this package.
const DefaultTokenTTL = 15 * time.Minute

// NewJWTSigner generates a token signing key under the Tenant Management
// authority.
//
// The key is not certified by the authority — a JWT verifier resolves keys by
// "kid" from a published JWKS, not by chain building — but it is generated and
// held by the same component, so that the blast radius of a Tenant Management
// compromise is one clearly delimited set of credentials rather than something
// nobody can enumerate.
func (h *Hierarchy) NewJWTSigner(issuer string) (*JWTSigner, error) {
	if issuer == "" {
		return nil, errors.New("pki: jwt signer: issuer is required")
	}
	if _, err := h.CA(RoleTenantManagement); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: generate jwt signing key: %w", err)
	}
	kid, err := jwkThumbprint(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	h.logger().Info("pki jwt signing key created", "kid", kid, "issuer", issuer)
	return &JWTSigner{kid: kid, key: key, issuer: issuer, ttl: DefaultTokenTTL}, nil
}

// KeyID returns the RFC 7638 thumbprint identifying the signing key. Like the
// price authority's key identifiers, it is derived from the key itself, so a
// published key set entry cannot claim an identifier that belongs to a
// different key.
func (s *JWTSigner) KeyID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.kid
}

// Issuer returns the issuer claim this signer stamps on every token.
func (s *JWTSigner) Issuer() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.issuer
}

// SetTTL changes the lifetime of newly minted tokens.
func (s *JWTSigner) SetTTL(d time.Duration) {
	if d <= 0 {
		d = DefaultTokenTTL
	}
	s.mu.Lock()
	s.ttl = d
	s.mu.Unlock()
}

// PublicKey returns the verification key.
func (s *JWTSigner) PublicKey() *ecdsa.PublicKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &s.key.PublicKey
}

// Sign mints a compact-serialisation JWS token.
//
// Issuer, issued-at, not-before, expiry and token ID are filled in by the
// signer, overriding whatever the caller supplied: a caller that could choose
// its own expiry could mint an immortal token.
func (s *JWTSigner) Sign(claims JWTClaims, at time.Time) (string, error) {
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()

	s.mu.RLock()
	kid, key, issuer, ttl := s.kid, s.key, s.issuer, s.ttl
	s.mu.RUnlock()
	if key == nil {
		return "", errors.New("pki: jwt signer has no key")
	}

	claims.Issuer = issuer
	claims.IssuedAt = at.Unix()
	claims.NotBefore = at.Unix()
	claims.ExpiresAt = at.Add(ttl).Unix()
	claims.TokenID = canon.NewULID()

	headerJSON, err := json.Marshal(jwtHeader{Algorithm: JWTAlgorithm, Type: "JWT", KeyID: kid})
	if err != nil {
		return "", fmt.Errorf("pki: encode token header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("pki: encode token claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	r, sig, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("pki: sign token: %w", err)
	}
	// JWS ES256 signatures are the fixed-width concatenation R||S, not the
	// ASN.1 form crypto/ecdsa's SignASN1 produces.
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	sig.FillBytes(raw[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(raw), nil
}

// JWTKeySet is the set of public keys a verifier accepts tokens from,
// published as a JWKS and rotated the same way the price authority ring is.
type JWTKeySet struct {
	mu   sync.RWMutex
	keys map[string]*ecdsa.PublicKey
}

// NewJWTKeySet returns an empty key set.
func NewJWTKeySet() *JWTKeySet {
	return &JWTKeySet{keys: make(map[string]*ecdsa.PublicKey)}
}

// Add inserts a public key, keyed by its own thumbprint.
func (s *JWTKeySet) Add(pub *ecdsa.PublicKey) (string, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return "", fmt.Errorf("pki: jwt key set: only ECDSA P-256 keys are accepted")
	}
	kid, err := jwkThumbprint(pub)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = make(map[string]*ecdsa.PublicKey)
	}
	s.keys[kid] = pub
	return kid, nil
}

// AddSigner registers a signer's public key.
func (s *JWTKeySet) AddSigner(signer *JWTSigner) (string, error) {
	if signer == nil {
		return "", errors.New("pki: jwt key set: signer is nil")
	}
	return s.Add(signer.PublicKey())
}

// Remove drops a key, which is how a compromised signing key is taken out of
// service everywhere at once.
func (s *JWTKeySet) Remove(kid string) {
	s.mu.Lock()
	delete(s.keys, kid)
	s.mu.Unlock()
}

// Len returns the number of keys in the set.
func (s *JWTKeySet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// JWTVerifyOptions constrains which tokens are accepted.
type JWTVerifyOptions struct {
	// At is the instant to check the validity window against; zero means now.
	At time.Time
	// Issuer, when set, must match the token's iss claim.
	Issuer string
	// Audience, when set, must match the token's aud claim.
	Audience string
	// Leeway absorbs clock skew between the token service and the verifier.
	Leeway time.Duration
}

// Verify checks a token's signature and validity window and returns its claims.
//
// The order is the security-relevant part: the signature is checked before any
// claim is believed, and the key is chosen by the "kid" header only as a lookup
// into this set — a token naming an unknown key is rejected outright rather
// than falling back to trying every key, which would turn the set into an
// oracle.
func (s *JWTKeySet) Verify(token string, opts JWTVerifyOptions) (JWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return JWTClaims{}, fmt.Errorf("%w: expected 3 dot-separated segments, got %d", ErrTokenMalformed, len(parts))
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return JWTClaims{}, fmt.Errorf("%w: header is not base64url: %v", ErrTokenMalformed, err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return JWTClaims{}, fmt.Errorf("%w: header is not JSON: %v", ErrTokenMalformed, err)
	}
	if header.Algorithm != JWTAlgorithm {
		return JWTClaims{}, fmt.Errorf("%w: algorithm %q is not %s", ErrTokenMalformed, header.Algorithm, JWTAlgorithm)
	}
	if header.KeyID == "" {
		return JWTClaims{}, fmt.Errorf("%w: header carries no kid", ErrTokenMalformed)
	}

	s.mu.RLock()
	pub, ok := s.keys[header.KeyID]
	s.mu.RUnlock()
	if !ok {
		return JWTClaims{}, fmt.Errorf("%w: key %q is not in this verifier's key set", ErrTokenSignature, header.KeyID)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return JWTClaims{}, fmt.Errorf("%w: signature is not base64url: %v", ErrTokenMalformed, err)
	}
	if len(sig) != 64 {
		return JWTClaims{}, fmt.Errorf("%w: ES256 signature must be 64 bytes, got %d", ErrTokenMalformed, len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	sv := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, sv) {
		return JWTClaims{}, fmt.Errorf("%w: under key %q", ErrTokenSignature, header.KeyID)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return JWTClaims{}, fmt.Errorf("%w: claims are not base64url: %v", ErrTokenMalformed, err)
	}
	var claims JWTClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return JWTClaims{}, fmt.Errorf("%w: claims are not JSON: %v", ErrTokenMalformed, err)
	}

	at := opts.At
	if at.IsZero() {
		at = time.Now()
	}
	now := at.UTC().Unix()
	leeway := int64(opts.Leeway / time.Second)
	if claims.ExpiresAt != 0 && now > claims.ExpiresAt+leeway {
		return JWTClaims{}, fmt.Errorf("%w: expired at %s",
			ErrTokenExpired, time.Unix(claims.ExpiresAt, 0).UTC().Format(time.RFC3339))
	}
	if claims.NotBefore != 0 && now < claims.NotBefore-leeway {
		return JWTClaims{}, fmt.Errorf("%w: not valid until %s",
			ErrTokenExpired, time.Unix(claims.NotBefore, 0).UTC().Format(time.RFC3339))
	}
	if opts.Issuer != "" && claims.Issuer != opts.Issuer {
		return JWTClaims{}, fmt.Errorf("%w: issuer %q, want %q", ErrTokenClaims, claims.Issuer, opts.Issuer)
	}
	if opts.Audience != "" && claims.Audience != opts.Audience {
		return JWTClaims{}, fmt.Errorf("%w: audience %q, want %q", ErrTokenClaims, claims.Audience, opts.Audience)
	}
	return claims, nil
}

// jwksDocument is the published key set.
type jwksDocument struct {
	Keys []jwksEntry `json:"keys"`
}

type jwksEntry struct {
	KeyType   string `json:"kty"`
	Curve     string `json:"crv"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	X         string `json:"x"`
	Y         string `json:"y"`
}

// PublishJWKS renders the key set as an RFC 7517 JWKS document for distribution
// to every service that verifies tenant tokens.
func (s *JWTKeySet) PublishJWKS() ([]byte, error) {
	s.mu.RLock()
	kids := make([]string, 0, len(s.keys))
	for kid := range s.keys {
		kids = append(kids, kid)
	}
	doc := jwksDocument{Keys: make([]jwksEntry, 0, len(kids))}
	sort.Strings(kids)
	for _, kid := range kids {
		pub := s.keys[kid]
		doc.Keys = append(doc.Keys, jwksEntry{
			KeyType:   "EC",
			Curve:     "P-256",
			Use:       "sig",
			Algorithm: JWTAlgorithm,
			KeyID:     kid,
			X:         base64.RawURLEncoding.EncodeToString(coordinate(pub.X)),
			Y:         base64.RawURLEncoding.EncodeToString(coordinate(pub.Y)),
		})
	}
	s.mu.RUnlock()

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("pki: publish jwks: %w", err)
	}
	return append(out, '\n'), nil
}

// ParseJWKS decodes a published key set.
func ParseJWKS(data []byte) (*JWTKeySet, error) {
	var doc jwksDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("pki: parse jwks: %w", err)
	}
	set := NewJWTKeySet()
	for _, entry := range doc.Keys {
		if entry.KeyType != "EC" || entry.Curve != "P-256" {
			return nil, fmt.Errorf("pki: parse jwks: key %q is %s/%s, only EC/P-256 is accepted",
				entry.KeyID, entry.KeyType, entry.Curve)
		}
		x, err := base64.RawURLEncoding.DecodeString(entry.X)
		if err != nil {
			return nil, fmt.Errorf("pki: parse jwks: key %q x: %w", entry.KeyID, err)
		}
		y, err := base64.RawURLEncoding.DecodeString(entry.Y)
		if err != nil {
			return nil, fmt.Errorf("pki: parse jwks: key %q y: %w", entry.KeyID, err)
		}
		pub := &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			return nil, fmt.Errorf("pki: parse jwks: key %q is not a point on P-256", entry.KeyID)
		}
		kid, err := set.Add(pub)
		if err != nil {
			return nil, err
		}
		if entry.KeyID != "" && entry.KeyID != kid {
			return nil, fmt.Errorf("%w: jwks entry claims %q but derives %q", ErrKeyIDMismatch, entry.KeyID, kid)
		}
	}
	return set, nil
}

// jwkThumbprint computes the RFC 7638 thumbprint of an EC public key: the
// base64url SHA-256 of the key's canonical JWK, whose members are the required
// ones in lexicographic order. Building the JSON by hand rather than through
// encoding/json is what guarantees that ordering, and therefore that two
// implementations derive the same identifier for the same key.
func jwkThumbprint(pub *ecdsa.PublicKey) (string, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return "", errors.New("pki: thumbprint: only ECDSA P-256 keys are supported")
	}
	canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`,
		base64.RawURLEncoding.EncodeToString(coordinate(pub.X)),
		base64.RawURLEncoding.EncodeToString(coordinate(pub.Y)))
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// coordinate renders an EC coordinate as the fixed 32-byte big-endian form RFC
// 7518 requires. Trimming leading zeros — which big.Int.Bytes does — would
// produce a different thumbprint for one key in every 256.
func coordinate(v *big.Int) []byte {
	b := make([]byte, 32)
	v.FillBytes(b)
	return b
}

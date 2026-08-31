package apigw

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ---------------------------------------------------------------------------
// API keys
//
// The API key is the credential that most often leaks. It ends up in a git
// repository, a CI log, a Postman collection shared in Slack, a screenshot in
// a support ticket. Every decision below follows from taking that as given
// rather than as a failure mode:
//
//   - the key is prefix-identifiable, so a secret scanner (GitHub's, or a
//     `git grep usslp_live_` in an incident) finds it without knowing anything
//     about USSLP;
//   - the key carries its own record identifier, so revocation is an O(1)
//     lookup and a leaked key can be attributed to an issuer, a tenant and a
//     creation date instantly;
//   - only a KDF output is stored, so a database disclosure does not hand the
//     attacker working credentials;
//   - the key expires, because a credential with no end date is a credential
//     that outlives the person who created it.
// ---------------------------------------------------------------------------

// Key prefixes. The environment is in the prefix so that a key pasted into the
// wrong deployment fails immediately and legibly, instead of authenticating
// against a staging tenant that happens to share an identifier.
const (
	// KeyPrefixLive identifies a production credential.
	KeyPrefixLive = "usslp_live_"
	// KeyPrefixTest identifies a non-production credential.
	KeyPrefixTest = "usslp_test_"
)

// Key material sizes.
const (
	// keyIDBytes is the size of the public, non-secret record identifier. Eight
	// bytes is far more than enough to be collision-free across every key the
	// platform will ever issue, and it is short enough to read aloud on a
	// support call.
	keyIDBytes = 8
	// keySecretBytes is the size of the secret half. Twenty-four bytes is 192
	// bits from crypto/rand; guessing it is not a threat model.
	keySecretBytes = 24
	// keySaltBytes is the per-record KDF salt.
	keySaltBytes = 16
	// keyHashBytes is the KDF output length.
	keyHashBytes = 32
)

// KDFName is the key derivation function used for keys at rest, recorded on
// every record so that the iteration count can be raised, or the function
// replaced, without invalidating credentials already in the field.
const KDFName = "pbkdf2-hmac-sha256"

// DefaultKDFIterations is the PBKDF2 work factor.
//
// The number is deliberately far below the six-figure counts recommended for
// *passwords*, and the reason is worth stating because it looks like a
// mistake. Those recommendations exist to make a dictionary attack expensive.
// There is no dictionary here: the secret half of a USSLP key is 192 bits
// straight from crypto/rand, so an offline attacker's cheapest path is
// exhaustive search of a space no iteration count meaningfully changes. What
// the KDF actually buys is that a leaked datastore contains no usable
// credential and no length-extension surface, plus a versioned, tunable
// primitive for the day the platform ever accepts a caller-chosen secret.
// Meanwhile the cost is paid on every single request through the front door of
// a system with a three-second end-to-end budget, so it is kept to roughly a
// millisecond rather than a hundred.
const DefaultKDFIterations = 4096

// API key errors. They are deliberately indistinguishable to the caller — the
// middleware collapses all of them to one 401 — but distinct here so that the
// access log records which one actually happened.
var (
	// ErrKeyMalformed means the presented string is not a USSLP key at all.
	ErrKeyMalformed = errors.New("apigw: api key is malformed")
	// ErrKeyUnknown means no record matches the key's identifier.
	ErrKeyUnknown = errors.New("apigw: api key is not recognised")
	// ErrKeySecret means the record exists but the secret does not verify.
	ErrKeySecret = errors.New("apigw: api key secret does not verify")
	// ErrKeyExpired means the record's expiry has passed.
	ErrKeyExpired = errors.New("apigw: api key has expired")
	// ErrKeyRevoked means the record was revoked.
	ErrKeyRevoked = errors.New("apigw: api key has been revoked")
)

// APIKey is a stored key record. It contains no secret: the only thing that
// can be recovered from it is the ability to check a secret someone else
// presents.
type APIKey struct {
	// KeyID is the public identifier, 16 lower-case hex characters. It appears
	// in the key itself, in access logs and in the console.
	KeyID string `json:"key_id"`
	// Prefix is the environment prefix the key was issued with.
	Prefix string `json:"prefix"`
	// TenantID is the isolation boundary this credential is bound to. It is
	// fixed at issuance and there is no operation that changes it.
	TenantID canon.TenantID `json:"tenant_id"`
	// Name is the human label: "nightly-price-import", "grafana".
	Name string `json:"name"`
	// Roles grant permissions.
	Roles []Role `json:"roles"`
	// Stores restricts the key to a subset of the tenant's stores.
	Stores []canon.StoreID `json:"stores,omitempty"`
	// Scopes are tenant-defined free-form labels.
	Scopes []string `json:"scopes,omitempty"`
	// KDF, Iterations, Salt and Hash are the verifier.
	KDF        string `json:"kdf"`
	Iterations int    `json:"kdf_iterations"`
	Salt       []byte `json:"-"`
	Hash       []byte `json:"-"`
	// CreatedBy records the principal that issued this key, so a credential
	// created by a compromised owner account can be found and revoked as a
	// group.
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is mandatory. A key store that permits non-expiring keys grows
	// a tail of credentials nobody remembers issuing.
	ExpiresAt time.Time `json:"expires_at"`
	// RevokedAt is set when the key is withdrawn. Records are kept rather than
	// deleted so that a request that used a revoked key can still be attributed
	// during an investigation.
	RevokedAt time.Time `json:"revoked_at,omitempty"`
	// LastUsedAt is best-effort, updated on successful verification. It is the
	// field that tells an operator which of forty keys can safely be revoked.
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

// Active reports whether the key may authenticate at a given instant.
func (k APIKey) Active(now time.Time) error {
	if !k.RevokedAt.IsZero() {
		return ErrKeyRevoked
	}
	if !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt) {
		return ErrKeyExpired
	}
	return nil
}

// KeyStore holds API key records.
//
// It is an interface because the production implementation is a Postgres table
// replicated per region, while `make dev`, the test suite and a single-store
// deployment run the in-memory one below. Nothing above this port knows which.
type KeyStore interface {
	// Lookup returns the record for a key identifier.
	Lookup(ctx context.Context, keyID string) (APIKey, error)
	// Put stores a record, replacing any with the same identifier.
	Put(ctx context.Context, rec APIKey) error
	// List returns a tenant's records, newest first.
	List(ctx context.Context, tenant canon.TenantID) ([]APIKey, error)
	// Revoke marks a key withdrawn. It must be scoped by tenant so that
	// knowing another tenant's key identifier does not confer the ability to
	// revoke it — a denial-of-service that would otherwise need only a leaked
	// log line.
	Revoke(ctx context.Context, tenant canon.TenantID, keyID string, at time.Time) error
	// Touch records a successful use. Implementations may coalesce or drop
	// these writes: the field is an operational hint, not an audit record, and
	// the audit record is the access log.
	Touch(ctx context.Context, keyID string, at time.Time) error
}

// MemoryKeyStore is an in-process KeyStore.
type MemoryKeyStore struct {
	mu   sync.RWMutex
	byID map[string]APIKey
}

// NewMemoryKeyStore returns an empty store.
func NewMemoryKeyStore() *MemoryKeyStore {
	return &MemoryKeyStore{byID: make(map[string]APIKey)}
}

// Lookup implements KeyStore.
func (s *MemoryKeyStore) Lookup(_ context.Context, keyID string) (APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byID[keyID]
	if !ok {
		return APIKey{}, fmt.Errorf("%w: %s", ErrKeyUnknown, keyID)
	}
	return rec, nil
}

// Put implements KeyStore.
func (s *MemoryKeyStore) Put(_ context.Context, rec APIKey) error {
	if rec.KeyID == "" {
		return errors.New("apigw: key record has no id")
	}
	s.mu.Lock()
	s.byID[rec.KeyID] = rec
	s.mu.Unlock()
	return nil
}

// List implements KeyStore.
func (s *MemoryKeyStore) List(_ context.Context, tenant canon.TenantID) ([]APIKey, error) {
	s.mu.RLock()
	out := make([]APIKey, 0, len(s.byID))
	for _, rec := range s.byID {
		if rec.TenantID == tenant {
			out = append(out, rec)
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Revoke implements KeyStore.
func (s *MemoryKeyStore) Revoke(_ context.Context, tenant canon.TenantID, keyID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[keyID]
	// The tenant mismatch is reported as "unknown", not "forbidden": telling a
	// caller that a key id exists but belongs to someone else is the same leak
	// as telling them a label id does.
	if !ok || rec.TenantID != tenant {
		return fmt.Errorf("%w: %s", ErrKeyUnknown, keyID)
	}
	if rec.RevokedAt.IsZero() {
		rec.RevokedAt = at.UTC()
		s.byID[keyID] = rec
	}
	return nil
}

// Touch implements KeyStore.
func (s *MemoryKeyStore) Touch(_ context.Context, keyID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.byID[keyID]; ok {
		rec.LastUsedAt = at.UTC()
		s.byID[keyID] = rec
	}
	return nil
}

// KeyIssuerConfig configures credential minting.
type KeyIssuerConfig struct {
	// Store is where records are written.
	Store KeyStore
	// Prefix is the environment prefix, defaulting to KeyPrefixLive.
	Prefix string
	// Iterations overrides the KDF work factor.
	Iterations int
	// MaxTTL caps the lifetime a caller may request.
	MaxTTL time.Duration
	// DefaultTTL is used when the caller does not ask for one.
	DefaultTTL time.Duration
	// Now supplies the clock, injected so that expiry is testable without
	// sleeping.
	Now func() time.Time
}

// Default key lifetimes. Ninety days is short enough that a forgotten
// credential eventually stops working and long enough that rotation is a
// quarterly chore rather than a weekly interruption; the one-year cap exists
// because a caller asking for ten years is asking for a key that will outlive
// its own documentation.
const (
	DefaultKeyTTL = 90 * 24 * time.Hour
	MaxKeyTTL     = 365 * 24 * time.Hour
)

// KeyIssuer mints and verifies API keys.
type KeyIssuer struct {
	store      KeyStore
	prefix     string
	iterations int
	maxTTL     time.Duration
	defTTL     time.Duration
	now        func() time.Time
}

// NewKeyIssuer builds an issuer over a store.
func NewKeyIssuer(cfg KeyIssuerConfig) (*KeyIssuer, error) {
	if cfg.Store == nil {
		return nil, errors.New("apigw: key issuer requires a store")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = KeyPrefixLive
	}
	if !strings.HasSuffix(cfg.Prefix, "_") {
		return nil, fmt.Errorf("apigw: key prefix %q must end in an underscore so it is greppable", cfg.Prefix)
	}
	if cfg.Iterations <= 0 {
		cfg.Iterations = DefaultKDFIterations
	}
	if cfg.MaxTTL <= 0 {
		cfg.MaxTTL = MaxKeyTTL
	}
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = DefaultKeyTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &KeyIssuer{
		store: cfg.Store, prefix: cfg.Prefix, iterations: cfg.Iterations,
		maxTTL: cfg.MaxTTL, defTTL: cfg.DefaultTTL, now: cfg.Now,
	}, nil
}

// IssueRequest describes a credential to mint.
type IssueRequest struct {
	TenantID  canon.TenantID
	Name      string
	Roles     []Role
	Stores    []canon.StoreID
	Scopes    []string
	TTL       time.Duration
	CreatedBy string
}

// Issue mints a key, returning the record and the one and only copy of the
// plaintext credential. The plaintext is never stored, never logged and never
// recoverable: a caller that loses it issues another one.
func (i *KeyIssuer) Issue(ctx context.Context, req IssueRequest) (APIKey, string, error) {
	if req.TenantID == "" {
		return APIKey{}, "", errors.New("apigw: issuing a key requires a tenant")
	}
	if !canon.ValidID(string(req.TenantID)) {
		return APIKey{}, "", fmt.Errorf("apigw: tenant %q contains reserved characters", req.TenantID)
	}
	if strings.TrimSpace(req.Name) == "" {
		return APIKey{}, "", errors.New("apigw: issuing a key requires a name")
	}
	if len(req.Roles) == 0 {
		return APIKey{}, "", errors.New("apigw: issuing a key requires at least one role")
	}
	for _, r := range req.Roles {
		if !r.Valid() {
			return APIKey{}, "", fmt.Errorf("apigw: unknown role %q", r)
		}
	}
	for _, s := range req.Stores {
		if !canon.ValidID(string(s)) {
			return APIKey{}, "", fmt.Errorf("apigw: store %q contains reserved characters", s)
		}
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = i.defTTL
	}
	if ttl > i.maxTTL {
		return APIKey{}, "", fmt.Errorf("apigw: requested lifetime %s exceeds the %s maximum", ttl, i.maxTTL)
	}

	idBytes := make([]byte, keyIDBytes)
	secret := make([]byte, keySecretBytes)
	salt := make([]byte, keySaltBytes)
	for _, b := range [][]byte{idBytes, secret, salt} {
		if _, err := rand.Read(b); err != nil {
			return APIKey{}, "", fmt.Errorf("apigw: generating key material: %w", err)
		}
	}
	keyID := hex.EncodeToString(idBytes)
	secretText := base64.RawURLEncoding.EncodeToString(secret)
	hash, err := deriveKeyHash(secretText, salt, i.iterations)
	if err != nil {
		return APIKey{}, "", err
	}

	now := i.now().UTC()
	rec := APIKey{
		KeyID: keyID, Prefix: i.prefix, TenantID: req.TenantID, Name: req.Name,
		Roles: append([]Role(nil), req.Roles...),
		KDF:   KDFName, Iterations: i.iterations, Salt: salt, Hash: hash,
		CreatedBy: req.CreatedBy, CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if len(req.Stores) > 0 {
		rec.Stores = append([]canon.StoreID(nil), req.Stores...)
	}
	if len(req.Scopes) > 0 {
		rec.Scopes = append([]string(nil), req.Scopes...)
	}
	if err := i.store.Put(ctx, rec); err != nil {
		return APIKey{}, "", err
	}
	return rec, i.prefix + keyID + "_" + secretText, nil
}

// Verify checks a presented key and returns its record.
//
// The order is chosen so that no failure mode is faster than another in a way
// an attacker could measure usefully: the record lookup necessarily short-
// circuits on an unknown identifier, but the identifier is not a secret, and
// everything after it runs the full KDF and a constant-time comparison before
// any expiry or revocation decision is made.
func (i *KeyIssuer) Verify(ctx context.Context, presented string) (APIKey, error) {
	keyID, secret, err := splitKey(presented)
	if err != nil {
		return APIKey{}, err
	}
	rec, err := i.store.Lookup(ctx, keyID)
	if err != nil {
		return APIKey{}, err
	}
	iterations := rec.Iterations
	if iterations <= 0 {
		iterations = DefaultKDFIterations
	}
	if rec.KDF != "" && rec.KDF != KDFName {
		return APIKey{}, fmt.Errorf("apigw: key %s uses unsupported kdf %q", keyID, rec.KDF)
	}
	candidate, err := deriveKeyHash(secret, rec.Salt, iterations)
	if err != nil {
		return APIKey{}, err
	}
	if subtle.ConstantTimeCompare(candidate, rec.Hash) != 1 {
		return APIKey{}, fmt.Errorf("%w: key %s", ErrKeySecret, keyID)
	}
	if err := rec.Active(i.now()); err != nil {
		return APIKey{}, fmt.Errorf("%w: key %s", err, keyID)
	}
	// Best effort: a failure to record last-use must not fail a request that
	// has already authenticated.
	_ = i.store.Touch(ctx, keyID, i.now())
	return rec, nil
}

// Store exposes the underlying record store for the /v1/keys handlers.
func (i *KeyIssuer) Store() KeyStore { return i.store }

// splitKey parses "usslp_live_<keyid>_<secret>".
//
// It accepts either environment prefix rather than only the issuer's own, so
// that a test key presented to production is rejected as an unknown *record*
// with the same 401 as everything else, instead of being distinguishable by
// its error.
func splitKey(presented string) (keyID, secret string, err error) {
	presented = strings.TrimSpace(presented)
	var rest string
	switch {
	case strings.HasPrefix(presented, KeyPrefixLive):
		rest = presented[len(KeyPrefixLive):]
	case strings.HasPrefix(presented, KeyPrefixTest):
		rest = presented[len(KeyPrefixTest):]
	default:
		return "", "", fmt.Errorf("%w: missing %s prefix", ErrKeyMalformed, KeyPrefixLive)
	}
	id, sec, ok := strings.Cut(rest, "_")
	if !ok {
		return "", "", fmt.Errorf("%w: no secret segment", ErrKeyMalformed)
	}
	if len(id) != keyIDBytes*2 {
		return "", "", fmt.Errorf("%w: identifier is %d characters, want %d", ErrKeyMalformed, len(id), keyIDBytes*2)
	}
	if _, decErr := hex.DecodeString(id); decErr != nil {
		return "", "", fmt.Errorf("%w: identifier is not hexadecimal", ErrKeyMalformed)
	}
	if sec == "" {
		return "", "", fmt.Errorf("%w: empty secret", ErrKeyMalformed)
	}
	return id, sec, nil
}

// deriveKeyHash runs the KDF.
func deriveKeyHash(secret string, salt []byte, iterations int) ([]byte, error) {
	if len(salt) == 0 {
		return nil, errors.New("apigw: key record has no salt")
	}
	h, err := pbkdf2.Key(sha256.New, secret, salt, iterations, keyHashBytes)
	if err != nil {
		return nil, fmt.Errorf("apigw: deriving key hash: %w", err)
	}
	return h, nil
}

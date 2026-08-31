package pki

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/usslp/usslp/platform/pkg/obs"
)

// File modes used throughout persistence.
const (
	// keyMode is owner read/write only. Anything looser on a CA key is a
	// finding in every audit the platform is subject to.
	keyMode fs.FileMode = 0o600
	// publicMode is world-readable: certificates and the published key ring are
	// public by design and are served to devices.
	publicMode fs.FileMode = 0o644
	// dirMode keeps CA material directories owner-only. The keys inside are
	// already 0600; an owner-only directory additionally hides which
	// authorities exist from a process running as another user on the same
	// host, which is one fewer thing for an attacker to enumerate.
	dirMode fs.FileMode = 0o700
)

// Filenames within a persisted hierarchy.
const (
	caCertFile      = "ca.crt.pem"
	caKeyFile       = "ca.key.pem"
	hierarchyFile   = "hierarchy.json"
	revocationsFile = "revocations.json"
	hierarchyFormat = "usslp.pki.hierarchy.v1"
)

// ErrInsecureKeyPermissions means a private key file on disk is readable by
// someone other than its owner.
//
// Loading is refused rather than warned about. A CA key that any process on the
// host can read is not a private key, and the failure mode of continuing —
// a platform that appears to be working while its issuing key is world-readable
// — is far worse than a service that will not start. The remedy is one chmod
// and it is named in the error.
var ErrInsecureKeyPermissions = errors.New("pki: private key file permissions are too permissive")

// ---------------------------------------------------------------------------
// PEM helpers
// ---------------------------------------------------------------------------

// encodeCertificatePEM renders certificates as concatenated PEM blocks.
func encodeCertificatePEM(certs ...*x509.Certificate) []byte {
	var buf bytes.Buffer
	for _, c := range certs {
		if c == nil {
			continue
		}
		// pem.Encode to a bytes.Buffer cannot fail.
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	return buf.Bytes()
}

// ParseCertificatesPEM decodes one or more concatenated PEM certificates. It is
// exported because every service that loads a certificate bundle from a config
// map or a secret needs it, and each rewriting it is how one of them ends up
// silently accepting a bundle with a trailing garbage block.
func ParseCertificatesPEM(data []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("pki: unexpected PEM block %q where a certificate was required", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("pki: parse certificate: %w", err)
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, errors.New("pki: no certificate found in PEM data")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("pki: trailing data after the last PEM certificate")
	}
	return out, nil
}

// EncodePrivateKeyPEM renders a private key as an unencrypted PKCS#8 PEM block.
//
// PKCS#8 rather than the algorithm-specific formats because it is the one
// encoding that carries RSA, ECDSA and Ed25519 keys identically — the platform
// uses all three, and a loader that has to sniff which of three block types it
// is looking at is a loader that eventually guesses wrong.
//
// The key is not encrypted. Passphrase-encrypted PEM is security theatre for a
// service that must start unattended: the passphrase ends up in the same
// environment the key is in. Real protection comes from file permissions here,
// and from an HSM or a KMS in a deployment that needs more.
func EncodePrivateKeyPEM(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("pki: marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePrivateKeyPEM decodes a PKCS#8 PEM private key.
func ParsePrivateKeyPEM(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("pki: no PEM block found in private key data")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("pki: PEM block is %q, want an unencrypted PKCS#8 \"PRIVATE KEY\"", block.Type)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse private key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("pki: private key of type %T cannot sign", key)
	}
	return signer, nil
}

// writeFileMode writes data and forces the mode, because os.WriteFile's
// permission argument is masked by the process umask and a 0600 key written
// under a permissive umask would silently land as 0644.
func writeFileMode(path string, data []byte, mode fs.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("pki: set mode on %s: %w", path, err)
	}
	return nil
}

func writePrivateKeyPEM(path string, key crypto.Signer) error {
	data, err := EncodePrivateKeyPEM(key)
	if err != nil {
		return err
	}
	return writeFileMode(path, data, keyMode)
}

// CheckKeyFilePermissions verifies that a private key file is accessible only
// to its owner, and that it is a regular file rather than a symlink into
// somewhere an attacker controls.
//
// It is exported so that services loading keys this package did not write —
// a key mounted from a secret store, say — can apply the same rule.
func CheckKeyFilePermissions(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("pki: stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symbolic link; private keys must be regular files "+
			"so that what is checked is what is read", ErrInsecureKeyPermissions, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInsecureKeyPermissions, path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s is mode %04o, which grants access to group or world; "+
			"run: chmod 600 %s", ErrInsecureKeyPermissions, path, uint32(perm), path)
	}
	return nil
}

func readPrivateKeyPEM(path string) (crypto.Signer, error) {
	if err := CheckKeyFilePermissions(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pki: read %s: %w", path, err)
	}
	key, err := ParsePrivateKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", path, err)
	}
	return key, nil
}

func readCertificatesPEM(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pki: read %s: %w", path, err)
	}
	certs, err := ParseCertificatesPEM(data)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", path, err)
	}
	return certs, nil
}

// ---------------------------------------------------------------------------
// Hierarchy persistence
// ---------------------------------------------------------------------------

type hierarchyDocument struct {
	Format      string              `json:"usslp_pki_format"`
	CreatedAt   time.Time           `json:"created_at"`
	Profile     Profile             `json:"profile"`
	Authorities []authorityDocument `json:"authorities"`
}

type authorityDocument struct {
	Role       CARole    `json:"role"`
	KeySpec    KeySpec   `json:"key_spec"`
	CommonName string    `json:"common_name"`
	Serial     string    `json:"serial"`
	NotAfter   time.Time `json:"not_after"`
	// CRLNumber is the highest CRL number this authority has published, so a
	// reloaded hierarchy continues the sequence instead of restarting it and
	// publishing a list that relying parties treat as stale.
	CRLNumber string `json:"crl_number,omitempty"`
}

type revocationDocument struct {
	Format  string                    `json:"usslp_pki_format"`
	Entries []revocationEntryDocument `json:"entries"`
}

type revocationEntryDocument struct {
	Serial      string    `json:"serial_hex"`
	Reason      int       `json:"reason"`
	ReasonName  string    `json:"reason_name"`
	RevokedAt   time.Time `json:"revoked_at"`
	IssuerKeyID string    `json:"issuer_key_id,omitempty"`
}

// Save writes the entire hierarchy to a directory.
//
// Certificates are written world-readable and private keys owner-only, and the
// two are separate files per authority so that a deployment can ship the
// certificates everywhere and mount only the one key each service needs. The
// root key is written if it is loaded — the key ceremony has to put it
// somewhere — but a production ceremony writes it to removable media and then
// removes it from the running hierarchy with [Hierarchy.DropRootKey].
func (h *Hierarchy) Save(dir string) error {
	if dir == "" {
		return errors.New("pki: save: directory is required")
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("pki: save: %w", err)
	}

	doc := hierarchyDocument{
		Format:    hierarchyFormat,
		CreatedAt: h.createdAt,
		Profile:   h.profile,
	}
	for _, ca := range h.Authorities() {
		roleDir := filepath.Join(dir, string(ca.Role))
		if err := os.MkdirAll(roleDir, dirMode); err != nil {
			return fmt.Errorf("pki: save %s: %w", ca.Role, err)
		}
		if err := writeFileMode(filepath.Join(roleDir, caCertFile),
			encodeCertificatePEM(ca.Chain()...), publicMode); err != nil {
			return err
		}
		if key, err := ca.Signer(); err == nil {
			if err := writePrivateKeyPEM(filepath.Join(roleDir, caKeyFile), key); err != nil {
				return err
			}
		} else if !errors.Is(err, ErrNoSigningKey) {
			return err
		}

		entry := authorityDocument{
			Role:       ca.Role,
			KeySpec:    ca.KeySpec,
			CommonName: ca.Certificate.Subject.CommonName,
			Serial:     serialHex(ca.Certificate.SerialNumber),
			NotAfter:   ca.Certificate.NotAfter,
		}
		ca.mu.RLock()
		if ca.crlNumber != nil {
			entry.CRLNumber = ca.crlNumber.String()
		}
		ca.mu.RUnlock()
		doc.Authorities = append(doc.Authorities, entry)
	}

	meta, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("pki: save: encode metadata: %w", err)
	}
	if err := writeFileMode(filepath.Join(dir, hierarchyFile), append(meta, '\n'), publicMode); err != nil {
		return err
	}
	return h.saveRevocations(dir)
}

func (h *Hierarchy) saveRevocations(dir string) error {
	doc := revocationDocument{Format: hierarchyFormat}
	for _, e := range h.revocations.Entries() {
		doc.Entries = append(doc.Entries, revocationEntryDocument{
			Serial:      serialHex(e.SerialNumber),
			Reason:      int(e.Reason),
			ReasonName:  e.Reason.String(),
			RevokedAt:   e.RevokedAt,
			IssuerKeyID: e.IssuerKeyID,
		})
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("pki: save revocations: %w", err)
	}
	return writeFileMode(filepath.Join(dir, revocationsFile), append(data, '\n'), publicMode)
}

// LoadOptions tunes loading a hierarchy from disk.
type LoadOptions struct {
	// IncludeRootKey loads the root private key if it is present on disk.
	//
	// It defaults to false, which is the important half of the design: a
	// service that only issues device and service certificates has no use for
	// the root key, and a service that cannot load it cannot leak it. Set it
	// true only in the tooling that runs a key ceremony.
	IncludeRootKey bool
	// Logger receives the load audit line. Nil means silent.
	Logger *obs.Logger
}

// Load reads a hierarchy previously written by [Hierarchy.Save].
//
// Two checks happen here that a naive loader would skip. Every private key file
// is refused unless it is owner-only (see [ErrInsecureKeyPermissions]), and
// every authority certificate is re-verified against its parent's signature, so
// a store in which one certificate has been swapped fails to load instead of
// producing a hierarchy that issues certificates nobody can verify.
func Load(dir string, opts LoadOptions) (*Hierarchy, error) {
	metaRaw, err := os.ReadFile(filepath.Join(dir, hierarchyFile))
	if err != nil {
		return nil, fmt.Errorf("pki: load: %w", err)
	}
	var doc hierarchyDocument
	if err := json.Unmarshal(metaRaw, &doc); err != nil {
		return nil, fmt.Errorf("pki: load: decode metadata: %w", err)
	}
	if doc.Format != hierarchyFormat {
		return nil, fmt.Errorf("pki: load: metadata format %q, this build understands %q",
			doc.Format, hierarchyFormat)
	}
	if err := doc.Profile.validate(); err != nil {
		return nil, fmt.Errorf("pki: load: %w", err)
	}

	log := opts.Logger
	if log == nil {
		log = obs.NopLogger()
	}
	h := &Hierarchy{
		profile:     doc.Profile,
		createdAt:   doc.CreatedAt,
		byRole:      make(map[CARole]*CA, len(AllRoles())),
		revocations: NewRevocationChecker(),
	}
	h.log.Store(log)
	specs := make(map[CARole]authorityDocument, len(doc.Authorities))
	for _, a := range doc.Authorities {
		specs[a.Role] = a
	}

	// Roles are loaded in hierarchy order so a parent is always in place before
	// its child needs it for the signature check.
	for _, role := range AllRoles() {
		spec, ok := specs[role]
		if !ok {
			return nil, fmt.Errorf("pki: load: metadata does not describe the %s authority", role)
		}
		roleDir := filepath.Join(dir, string(role))
		certs, err := readCertificatesPEM(filepath.Join(roleDir, caCertFile))
		if err != nil {
			return nil, err
		}
		cert := certs[0]
		if !cert.IsCA {
			return nil, fmt.Errorf("pki: load: %s certificate is not a CA certificate", role)
		}

		var parent *CA
		if parentRole := parentRoleOf(role); parentRole != "" {
			parent = h.byRole[parentRole]
			if parent == nil {
				return nil, fmt.Errorf("pki: load: %s has no loaded parent %s", role, parentRole)
			}
			if err := cert.CheckSignatureFrom(parent.Certificate); err != nil {
				return nil, fmt.Errorf("pki: load: %s certificate is not signed by %s: %w",
					role, parentRole, err)
			}
		} else if err := cert.CheckSignatureFrom(cert); err != nil {
			return nil, fmt.Errorf("pki: load: root certificate is not self-signed: %w", err)
		}

		ca := &CA{Role: role, Certificate: cert, Parent: parent, KeySpec: spec.KeySpec}
		if spec.CRLNumber != "" {
			if n, ok := new(big.Int).SetString(spec.CRLNumber, 10); ok {
				ca.observeCRLNumber(n)
			}
		}

		keyPath := filepath.Join(roleDir, caKeyFile)
		wantKey := role != RoleRoot || opts.IncludeRootKey
		if _, statErr := os.Lstat(keyPath); statErr == nil && wantKey {
			key, err := readPrivateKeyPEM(keyPath)
			if err != nil {
				return nil, err
			}
			if !publicKeysEqual(key.Public(), cert.PublicKey) {
				return nil, fmt.Errorf("pki: load: %s private key does not match its certificate", role)
			}
			ca.key = key
		} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("pki: load: stat %s: %w", keyPath, statErr)
		} else if statErr != nil && role != RoleRoot {
			return nil, fmt.Errorf("pki: load: %s has no private key at %s; an issuing authority "+
				"without its key cannot sign anything", role, keyPath)
		}

		h.byRole[role] = ca
		switch role {
		case RoleRoot:
			h.Root = ca
		case RoleDeviceIssuance:
			h.DeviceIssuance = ca
		case RoleManufacturing:
			h.Manufacturing = ca
		case RoleShelfController:
			h.ShelfController = ca
		case RoleServices:
			h.Services = ca
		case RoleTenantManagement:
			h.TenantManagement = ca
		}
	}

	if err := h.loadRevocations(dir); err != nil {
		return nil, err
	}
	h.buildPools()
	log.Info("pki hierarchy loaded",
		"dir", dir, "authorities", len(h.byRole),
		"root_key_loaded", h.Root.HasKey(), "revocations", h.revocations.Len())
	return h, nil
}

func (h *Hierarchy) loadRevocations(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, revocationsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pki: load revocations: %w", err)
	}
	var doc revocationDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("pki: load revocations: %w", err)
	}
	for _, e := range doc.Entries {
		serial, ok := new(big.Int).SetString(e.Serial, 16)
		if !ok {
			return fmt.Errorf("pki: load revocations: %q is not a hex serial", e.Serial)
		}
		if err := h.revocations.Add(RevocationEntry{
			SerialNumber: serial,
			Reason:       RevocationReason(e.Reason),
			RevokedAt:    e.RevokedAt,
			IssuerKeyID:  e.IssuerKeyID,
		}); err != nil {
			return fmt.Errorf("pki: load revocations: %w", err)
		}
	}
	return nil
}

// parentRoleOf returns the role that issues a given role's certificate. The
// shape of the hierarchy is code, not configuration: a deployment cannot
// reparent the Manufacturing Sub-CA under the Services intermediate by editing
// a JSON file.
func parentRoleOf(role CARole) CARole {
	switch role {
	case RoleRoot:
		return ""
	case RoleManufacturing, RoleShelfController:
		return RoleDeviceIssuance
	default:
		return RoleRoot
	}
}

// ---------------------------------------------------------------------------
// Identity persistence
// ---------------------------------------------------------------------------

// StoredIdentity is a leaf certificate, its chain and its private key as loaded
// from disk.
type StoredIdentity struct {
	// Certificate is the end-entity certificate.
	Certificate *x509.Certificate
	// Chain is the issuing chain that accompanied it.
	Chain []*x509.Certificate
	// PrivateKey is the matching key.
	PrivateKey crypto.Signer
	// Identity is the identity the certificate asserts.
	Identity Identity
}

// TLSCertificate assembles the tls.Certificate to present in a handshake.
func (s *StoredIdentity) TLSCertificate() (tls.Certificate, error) {
	out := tls.Certificate{
		Certificate: make([][]byte, 0, 1+len(s.Chain)),
		PrivateKey:  s.PrivateKey,
		Leaf:        s.Certificate,
	}
	out.Certificate = append(out.Certificate, s.Certificate.Raw)
	for _, c := range s.Chain {
		out.Certificate = append(out.Certificate, c.Raw)
	}
	return out, nil
}

// SaveIdentity writes an issued certificate, its chain and its private key as
// {name}.crt.pem and {name}.key.pem.
//
// This is what the factory-provisioning simulator and the local development
// stack write, and what a service reads at start-up. The chain is written into
// the certificate file rather than a third file because a TLS peer must present
// them together and the commonest deployment mistake in mutual TLS is shipping
// a leaf without its intermediates.
func SaveIdentity(dir, name string, issued *Issued, key crypto.Signer) error {
	if issued == nil {
		return errors.New("pki: save identity: nothing to save")
	}
	if name == "" {
		return errors.New("pki: save identity: name is required")
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("pki: save identity: %w", err)
	}
	if err := writeFileMode(filepath.Join(dir, name+".crt.pem"), issued.ChainPEM(), publicMode); err != nil {
		return err
	}
	if key != nil {
		if !publicKeysEqual(key.Public(), issued.Certificate.PublicKey) {
			return errors.New("pki: save identity: private key does not match the certificate")
		}
		if err := writePrivateKeyPEM(filepath.Join(dir, name+".key.pem"), key); err != nil {
			return err
		}
	}
	return nil
}

// LoadIdentity reads a certificate, chain and key written by [SaveIdentity],
// refusing a private key whose permissions allow more than owner access.
func LoadIdentity(dir, name string) (*StoredIdentity, error) {
	certs, err := readCertificatesPEM(filepath.Join(dir, name+".crt.pem"))
	if err != nil {
		return nil, err
	}
	key, err := readPrivateKeyPEM(filepath.Join(dir, name+".key.pem"))
	if err != nil {
		return nil, err
	}
	if !publicKeysEqual(key.Public(), certs[0].PublicKey) {
		return nil, fmt.Errorf("pki: load identity %q: private key does not match the certificate", name)
	}
	id, err := IdentityFromCertificate(certs[0])
	if err != nil {
		return nil, err
	}
	return &StoredIdentity{
		Certificate: certs[0],
		Chain:       certs[1:],
		PrivateKey:  key,
		Identity:    id,
	}, nil
}

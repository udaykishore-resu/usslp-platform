package domain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Errors returned by artifact handling. Each maps to a different operational
// response, which is why they are distinct sentinels rather than one
// "bad artifact".
var (
	// ErrUnsigned means no signature was supplied. An unsigned image is not a
	// degraded case to warn about; it is one the pipeline has no way to
	// evaluate, and it is refused at upload rather than at rollout.
	ErrUnsigned = errors.New("ota: firmware artifact carries no signature")
	// ErrBadSignature means the signature does not verify against any trusted
	// firmware signing key. This is a security event.
	ErrBadSignature = errors.New("ota: firmware signature does not verify")
	// ErrDigestMismatch means the uploaded bytes do not hash to the digest the
	// upload declared — a corrupted transfer, or an attempt to attach a valid
	// signature to different content.
	ErrDigestMismatch = errors.New("ota: firmware digest does not match its content")
	// ErrArtifactNotFound means no artifact is stored under that identifier.
	ErrArtifactNotFound = errors.New("ota: firmware artifact not found")
	// ErrInvalid marks a structurally unusable argument.
	ErrInvalid = errors.New("ota: invalid argument")
)

// Version is a firmware version in major.minor.patch form.
//
// It is a distinct type because rollouts compare versions constantly — is this
// an upgrade, is this the version we are rolling back to, is this device
// already done — and string comparison gets "1.10.0" and "1.9.0" backwards,
// which on an OTA pipeline means re-flashing a fleet with older firmware.
type Version string

// Parse splits a version into its three numeric components.
func (v Version) Parse() (major, minor, patch int, err error) {
	parts := strings.Split(string(v), ".")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("%w: version %q is not major.minor.patch", ErrInvalid, v)
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 0 {
			return 0, 0, 0, fmt.Errorf("%w: version %q component %q", ErrInvalid, v, p)
		}
		out[i] = n
	}
	return out[0], out[1], out[2], nil
}

// Valid reports whether the version parses.
func (v Version) Valid() bool {
	_, _, _, err := v.Parse()
	return err == nil
}

// Compare returns -1, 0 or 1 as v sorts before, with or after o. An unparseable
// version sorts before every parseable one, so a malformed value can never be
// mistaken for the newest.
func (v Version) Compare(o Version) int {
	aMaj, aMin, aPat, aErr := v.Parse()
	bMaj, bMin, bPat, bErr := o.Parse()
	switch {
	case aErr != nil && bErr != nil:
		return strings.Compare(string(v), string(o))
	case aErr != nil:
		return -1
	case bErr != nil:
		return 1
	}
	for _, pair := range [][2]int{{aMaj, bMaj}, {aMin, bMin}, {aPat, bPat}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// String renders the version.
func (v Version) String() string { return string(v) }

// Artifact is one firmware image the platform is prepared to install.
type Artifact struct {
	// ArtifactID is the content address: "sha256:<hex>". Two uploads of
	// identical bytes are the same artifact, which is what makes a re-upload
	// after a failed build idempotent instead of a duplicate.
	ArtifactID string `json:"artifact_id"`
	// Version is the firmware version this image contains.
	Version Version `json:"version"`
	// HardwareTier is the display and radio generation this image is built for.
	// It is bound into the signed manifest, so an image signed for one tier
	// cannot be rolled out as another.
	HardwareTier string `json:"hardware_tier"`
	// SHA256 is the hex digest of the image bytes.
	SHA256 string `json:"sha256"`
	// Size is the image length in bytes.
	Size int64 `json:"size"`
	// Signature is the base64 Ed25519 signature over the canonical manifest
	// string; see [SigningManifest].
	Signature string `json:"signature"`
	// SigningKeyID names the key that signed it, so a key rotation can be
	// audited and a compromised key's artifacts found.
	SigningKeyID string `json:"signing_key_id"`
	// ReleaseNotes and UploadedBy are for humans reconstructing a rollout.
	ReleaseNotes string    `json:"release_notes,omitempty"`
	UploadedBy   string    `json:"uploaded_by,omitempty"`
	UploadedAt   time.Time `json:"uploaded_at"`
}

// ArtifactIDFor returns the content address of an image.
func ArtifactIDFor(image []byte) string {
	sum := sha256.Sum256(image)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SigningManifest returns the exact byte string a firmware signature is
// computed over.
//
// # Why the signature covers a manifest and not the image
//
// Signing the raw image would prove only that some authorised party once built
// those bytes. It would say nothing about what they are *for*, and the OTA
// pipeline's most dangerous mistake is not an unsigned image — it is a
// perfectly legitimate, properly signed image installed on the wrong hardware.
// A 4.2-inch three-colour panel driven by a 2.9-inch monochrome waveform is a
// brick, and a brick is a person walking an aisle with a screwdriver.
//
// Binding the version and the hardware tier into the signed string makes that
// impossible to do by accident or on purpose: an image cannot be re-declared as
// a different tier without invalidating its signature. The digest is included
// rather than the image itself so that verification is constant-cost regardless
// of image size, and the fields are separated by a byte that cannot occur in
// any of them so that no two different field splits produce the same string.
func SigningManifest(version Version, hardwareTier, sha256hex string) []byte {
	return []byte(strings.Join([]string{
		"usslp-firmware-v1",
		string(version),
		hardwareTier,
		strings.ToLower(sha256hex),
	}, "\n"))
}

// SignArtifact produces the signature for an image. It is the counterpart of
// [VerifyArtifact] and is used by the release pipeline and by tests; the
// service itself only ever verifies.
func SignArtifact(key ed25519.PrivateKey, version Version, hardwareTier string, image []byte) string {
	sum := sha256.Sum256(image)
	manifest := SigningManifest(version, hardwareTier, hex.EncodeToString(sum[:]))
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, manifest))
}

// KeyRing is the set of firmware signing keys the platform trusts, by key
// identifier.
//
// It is a set rather than a single key because signing keys rotate and both the
// old and the new must verify during the overlap — a fleet of 50 million
// devices is never all on one side of a rotation at one instant.
type KeyRing map[string]ed25519.PublicKey

// Verify checks a base64 signature over a manifest against every key in the
// ring, returning the identifier of the key that verified it.
//
// Every key is tried rather than only the one the upload names, because the key
// identifier travels with the signature and is therefore attacker-controlled.
// Trusting it to select the key would let an attacker point verification at
// whichever key they had a signature for.
func (r KeyRing) Verify(manifest []byte, signature string) (string, error) {
	if len(r) == 0 {
		return "", fmt.Errorf("%w: no firmware signing keys are configured", ErrBadSignature)
	}
	if signature == "" {
		return "", ErrUnsigned
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return "", fmt.Errorf("%w: signature is not valid base64: %v", ErrBadSignature, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: signature is %d bytes, Ed25519 signatures are %d",
			ErrBadSignature, len(sig), ed25519.SignatureSize)
	}
	// Deterministic order so that two identical uploads attribute to the same
	// key when a ring holds two keys that would both verify — which happens
	// only if the same key material was added twice under two identifiers, and
	// is worth being consistent about.
	ids := make([]string, 0, len(r))
	for id := range r {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for _, id := range ids {
		if ed25519.Verify(r[id], manifest, sig) {
			return id, nil
		}
	}
	return "", ErrBadSignature
}

// VerifyArtifact checks that an image matches its declared digest and that its
// signature verifies against the ring, returning the completed artifact record.
//
// This is the gate the whole OTA pipeline hangs on: nothing downstream — not
// job creation, not cohort selection, not a device trigger — accepts an
// artifact that has not been through here, so an unsigned or mis-signed image
// cannot become a rollout.
func VerifyArtifact(ring KeyRing, a Artifact, image []byte) (Artifact, error) {
	if !a.Version.Valid() {
		return Artifact{}, fmt.Errorf("%w: firmware version %q", ErrInvalid, a.Version)
	}
	if a.HardwareTier == "" {
		return Artifact{}, fmt.Errorf("%w: hardware tier is required", ErrInvalid)
	}
	if len(image) == 0 {
		return Artifact{}, fmt.Errorf("%w: image is empty", ErrInvalid)
	}
	sum := sha256.Sum256(image)
	digest := hex.EncodeToString(sum[:])
	if a.SHA256 != "" && !strings.EqualFold(a.SHA256, digest) {
		return Artifact{}, fmt.Errorf("%w: declared %s, computed %s", ErrDigestMismatch, a.SHA256, digest)
	}
	a.SHA256 = digest
	a.Size = int64(len(image))
	a.ArtifactID = "sha256:" + digest

	keyID, err := ring.Verify(SigningManifest(a.Version, a.HardwareTier, digest), a.Signature)
	if err != nil {
		return Artifact{}, err
	}
	a.SigningKeyID = keyID
	if a.UploadedAt.IsZero() {
		a.UploadedAt = time.Now().UTC()
	}
	a.UploadedAt = a.UploadedAt.UTC()
	return a, nil
}

// sortStrings is an insertion sort over the handful of key identifiers a ring
// holds. It avoids pulling the sort package into a file that is otherwise pure
// crypto, and at ring sizes measured in single digits it is faster anyway.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

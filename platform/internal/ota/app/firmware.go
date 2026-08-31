package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/usslp/usslp/platform/internal/ota/domain"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

// Keyspace prefixes for the documents the OTA service keeps beside its events
// in the shared kvstore. They begin with an upper-case letter because the event
// store's own keys are all lower-case single letters, so the two namespaces
// cannot collide however either grows.
var (
	artifactPrefix = []byte("A\x00rec\x00")
	versionPrefix  = []byte("A\x00ver\x00")
)

func artifactKey(id string) []byte {
	return append(append([]byte(nil), artifactPrefix...), id...)
}

// versionKey indexes an artifact by the pair that identifies it operationally:
// which hardware it is for and which version it contains. Two images cannot
// occupy one (tier, version) slot, which is what stops a rollout from
// installing "1.4.0" and a later one from installing a different "1.4.0".
func versionKey(tier string, v domain.Version) []byte {
	k := append(append([]byte(nil), versionPrefix...), tier...)
	k = append(k, 0)
	return append(k, v...)
}

// UploadFirmware verifies and stores a firmware image.
//
// Verification happens here, once, at the only point where the bytes and the
// claim about them are both present. Everything downstream — job creation,
// cohort selection, the trigger a device receives — works from the stored
// record, so there is no path by which an unverified image can become a
// rollout. That is deliberate: a signature check that happens "somewhere before
// deployment" is a check that eventually gets skipped by a code path nobody
// remembered.
func (c *Controller) UploadFirmware(ctx context.Context, a domain.Artifact, image []byte) (domain.Artifact, error) {
	verified, err := domain.VerifyArtifact(c.cfg.Keys, a, image)
	if err != nil {
		c.countRejection(rejectionReason(err))
		c.log.Warn("firmware upload rejected",
			"version", string(a.Version), "hardware_tier", a.HardwareTier,
			"size", len(image), "error", err)
		return domain.Artifact{}, err
	}
	if verified.UploadedAt.IsZero() {
		verified.UploadedAt = c.Now()
	}

	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	// A re-upload of identical bytes is a no-op. Build pipelines retry, and a
	// retried upload must not create a second artifact that a rollout could
	// pick by accident.
	if existing, err := c.artifactRecord(verified.ArtifactID); err == nil {
		return existing, nil
	} else if !errors.Is(err, domain.ErrArtifactNotFound) {
		return domain.Artifact{}, err
	}

	// A different image claiming a (tier, version) slot that is already taken is
	// refused. Allowing it would mean "1.4.0" named two different images, and
	// the rollout that discovers this is the one where half a fleet is on one
	// and half on the other with nothing to tell them apart.
	if id, err := c.artifactIDForVersion(verified.HardwareTier, verified.Version); err == nil && id != verified.ArtifactID {
		return domain.Artifact{}, fmt.Errorf("%w: %s %s is already published as artifact %s",
			domain.ErrInvalid, verified.HardwareTier, verified.Version, id)
	}

	if _, err := c.cfg.Artifacts.Put(image); err != nil {
		return domain.Artifact{}, fmt.Errorf("ota: store firmware image: %w", err)
	}
	body, err := json.Marshal(verified)
	if err != nil {
		return domain.Artifact{}, fmt.Errorf("ota: encode artifact record: %w", err)
	}
	batch := c.kv.NewBatch()
	batch.Put(artifactKey(verified.ArtifactID), body)
	batch.Put(versionKey(verified.HardwareTier, verified.Version), []byte(verified.ArtifactID))
	if err := batch.Write(); err != nil {
		return domain.Artifact{}, fmt.Errorf("ota: record artifact: %w", err)
	}

	if c.met != nil {
		c.met.artifacts.With(verified.HardwareTier).Inc()
	}
	c.log.Info("firmware artifact accepted",
		"artifact_id", verified.ArtifactID, "version", string(verified.Version),
		"hardware_tier", verified.HardwareTier, "size", verified.Size,
		"signing_key_id", verified.SigningKeyID)
	return verified, nil
}

// Artifact returns a stored artifact record.
func (c *Controller) Artifact(id string) (domain.Artifact, error) {
	return c.artifactRecord(id)
}

// ArtifactImage returns the bytes of a stored artifact.
func (c *Controller) ArtifactImage(id string) ([]byte, error) {
	return c.cfg.Artifacts.Get(id)
}

// Artifacts lists every stored artifact, newest version first within each
// hardware tier, so the listing reads the way a release manager thinks.
func (c *Controller) Artifacts() ([]domain.Artifact, error) {
	it := c.kv.Scan(artifactPrefix)
	defer it.Close()
	var out []domain.Artifact
	for it.Next() {
		var a domain.Artifact
		if err := json.Unmarshal(it.Value(), &a); err != nil {
			return nil, fmt.Errorf("ota: decode stored artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HardwareTier != out[j].HardwareTier {
			return out[i].HardwareTier < out[j].HardwareTier
		}
		if cmp := out[j].Version.Compare(out[i].Version); cmp != 0 {
			return cmp < 0
		}
		return out[i].ArtifactID < out[j].ArtifactID
	})
	return out, nil
}

// artifactRecord loads one artifact's metadata.
func (c *Controller) artifactRecord(id string) (domain.Artifact, error) {
	raw, err := c.kv.Get(artifactKey(id))
	if errors.Is(err, kvstore.ErrNotFound) {
		return domain.Artifact{}, fmt.Errorf("%w: %s", domain.ErrArtifactNotFound, id)
	}
	if err != nil {
		return domain.Artifact{}, err
	}
	var a domain.Artifact
	if err := json.Unmarshal(raw, &a); err != nil {
		return domain.Artifact{}, fmt.Errorf("ota: decode artifact %s: %w", id, err)
	}
	return a, nil
}

// artifactIDForVersion resolves a (hardware tier, version) pair to its content
// address.
func (c *Controller) artifactIDForVersion(tier string, v domain.Version) (string, error) {
	raw, err := c.kv.Get(versionKey(tier, v))
	if errors.Is(err, kvstore.ErrNotFound) {
		return "", fmt.Errorf("%w: %s %s", domain.ErrArtifactNotFound, tier, v)
	}
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// DeltaPlan is the outcome of asking whether a rollout should ship a patch.
type DeltaPlan struct {
	// Use reports whether the patch is smaller than the compressed image and
	// should therefore be shipped.
	Use bool `json:"use"`
	// ArtifactID is the content address of the stored patch, empty when no
	// patch was produced.
	ArtifactID string `json:"artifact_id,omitempty"`
	// FromVersion is the base the patch applies to.
	FromVersion domain.Version `json:"from_version,omitempty"`
	// DeltaBytes and FullBytes are the two sizes that were compared. FullBytes
	// is the *compressed* image, because comparing a compressed patch with an
	// uncompressed image would make every patch look like a win.
	DeltaBytes int `json:"delta_bytes"`
	FullBytes  int `json:"full_bytes"`
	// SHA256 is the digest of the patch itself. The device checks it before
	// applying, and checks the reconstructed image's own digest afterwards.
	SHA256 string `json:"sha256,omitempty"`
	// Savings is the fraction of payload the patch avoids sending.
	Savings float64 `json:"savings"`
	// Reason explains a refusal, e.g. no base artifact on file.
	Reason string `json:"reason,omitempty"`
}

// PlanDelta computes and stores a patch from one version to another, if one is
// worth shipping.
//
// It refuses rather than guesses in every case where the answer is not clearly
// yes: no stored base image, a patch that is not smaller than the compressed
// full image, or a base and target that do not share a hardware tier. A delta
// that saves nothing costs a rollout the extra failure mode of a patch that
// might not apply, and a battery-powered fleet gets nothing back for it.
func (c *Controller) PlanDelta(from, to domain.Version, tier string) (DeltaPlan, error) {
	plan := DeltaPlan{FromVersion: from}
	if from == "" || from == to {
		plan.Reason = "no base version to patch from"
		return plan, nil
	}
	baseID, err := c.artifactIDForVersion(tier, from)
	if err != nil {
		plan.Reason = fmt.Sprintf("no stored image for %s %s", tier, from)
		return plan, nil
	}
	targetID, err := c.artifactIDForVersion(tier, to)
	if err != nil {
		return plan, err
	}
	base, err := c.cfg.Artifacts.Get(baseID)
	if err != nil {
		return plan, err
	}
	target, err := c.cfg.Artifacts.Get(targetID)
	if err != nil {
		return plan, err
	}

	delta, err := domain.Diff(base, target)
	if err != nil {
		return plan, err
	}
	ship, deltaBytes, fullBytes := domain.ShouldShipDelta(delta, target)
	plan.DeltaBytes, plan.FullBytes = deltaBytes, fullBytes
	if !ship {
		plan.Reason = fmt.Sprintf("patch is %d bytes against a %d-byte compressed image; shipping the image",
			deltaBytes, fullBytes)
		return plan, nil
	}
	id, err := c.cfg.Artifacts.Put(delta.Bytes)
	if err != nil {
		return plan, fmt.Errorf("ota: store delta: %w", err)
	}
	plan.Use = true
	plan.ArtifactID = id
	plan.SHA256 = domain.ArtifactIDFor(delta.Bytes)[len("sha256:"):]
	plan.Savings = 1 - float64(deltaBytes)/float64(fullBytes)
	c.log.Info("firmware delta prepared",
		"from", string(from), "to", string(to), "hardware_tier", tier,
		"delta_bytes", deltaBytes, "full_bytes", fullBytes,
		"savings_pct", fmt.Sprintf("%.1f", plan.Savings*100))
	return plan, nil
}

// rejectionReason maps an artifact error onto a metric label.
func rejectionReason(err error) string {
	switch {
	case errors.Is(err, domain.ErrUnsigned):
		return "unsigned"
	case errors.Is(err, domain.ErrBadSignature):
		return "bad-signature"
	case errors.Is(err, domain.ErrDigestMismatch):
		return "digest-mismatch"
	default:
		return "invalid"
	}
}

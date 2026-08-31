package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// DefaultCohorts is the platform's standard staged rollout: 1%, then 5%, then
// 25%, then everything.
//
// The percentages are cumulative, not incremental — wave 1 finishes with 5% of
// the fleet updated, not 6%. Cumulative is the form an operator reasons in
// ("we're at 25%") and the form that makes a wave's membership stable when a
// later wave's size is changed mid-rollout.
//
// The shape is a compromise between two costs that pull in opposite directions.
// A first wave of 1% of a hundred-thousand-store fleet is still tens of
// thousands of labels, which is a large enough sample that a 2% failure rate is
// unmistakable rather than noise. But it is small enough that if the image
// bricks every device it touches, the recovery is a manageable number of
// service visits rather than a national incident.
var DefaultCohorts = []int{1, 5, 25, 100}

// bucketSpace is the resolution of cohort assignment: ten thousand buckets, so
// a cohort boundary can be expressed to a hundredth of a percent. Finer than a
// percent matters because 1% of a 50-million-device fleet is 500,000 devices,
// and a rollout manager sometimes wants a first wave an order of magnitude
// smaller than that.
const bucketSpace = 10000

// Bucket returns a device's stable position in [0, 10000) for one job.
//
// # Why a hash and not a list
//
// Cohort membership has to survive a service restart, a controller failover, a
// job being paused for a week and resumed, and the rollout being re-run against
// a fleet that has gained and lost devices in the meantime. A stored membership
// list survives none of that cheaply: it is 50 million rows that have to be
// written transactionally with the job, reconciled every time a device is
// provisioned or retired, and replayed identically after a crash.
//
// Hashing (job, device) makes membership a pure function. There is nothing to
// store, nothing to reconcile, and nothing that can drift. A device that was in
// the first wave before the restart is in the first wave after it, and a device
// provisioned into the store halfway through the rollout lands in whichever
// wave it would always have been in — which is the behaviour an operator
// expects, because the alternative is a new device jumping straight to 100%.
//
// The job identifier is mixed in so that two consecutive rollouts do not pick
// the same unlucky 1% twice. If they did, one store's shelf would be the canary
// for every firmware release the platform ever ships.
func Bucket(jobID, deviceID string) uint32 {
	h := sha256.New()
	h.Write([]byte(jobID))
	// A separator that cannot occur in an identifier — canon.ValidID rejects
	// NUL — so that ("ab", "c") and ("a", "bc") cannot hash alike.
	h.Write([]byte{0})
	h.Write([]byte(deviceID))
	sum := h.Sum(nil)
	return uint32(binary.BigEndian.Uint64(sum[:8]) % bucketSpace)
}

// WaveFor returns the index of the first cohort that includes a device, or -1
// when the cohort schedule does not reach it.
//
// The schedule is cumulative and must be non-decreasing; [ValidateCohorts]
// enforces that at job creation so this function never has to decide what a
// decreasing schedule means.
func WaveFor(jobID, deviceID string, cohorts []int) int {
	bucket := Bucket(jobID, deviceID)
	for i, pct := range cohorts {
		if bucket < uint32(pct)*bucketSpace/100 {
			return i
		}
	}
	return -1
}

// InWave reports whether a device belongs to cohort wave — that is, whether it
// is inside the cumulative boundary of that wave and outside the previous one.
func InWave(jobID, deviceID string, cohorts []int, wave int) bool {
	return WaveFor(jobID, deviceID, cohorts) == wave
}

// InOrBefore reports whether a device is inside the cumulative boundary of a
// wave. It is the predicate that answers "should this device be running the new
// firmware by now", which is what a rollback and a progress report both need.
func InOrBefore(jobID, deviceID string, cohorts []int, wave int) bool {
	w := WaveFor(jobID, deviceID, cohorts)
	return w >= 0 && w <= wave
}

// ValidateCohorts rejects a schedule that cannot be executed.
//
// A schedule that does not end at 100 is refused because a rollout that stops
// at 90% leaves a permanent minority of the fleet on old firmware with nothing
// to say so — the job reports "completed" and ten percent of the shelves are
// running the version the release was meant to replace.
func ValidateCohorts(cohorts []int) error {
	if len(cohorts) == 0 {
		return fmt.Errorf("%w: a rollout needs at least one cohort", ErrInvalid)
	}
	prev := 0
	for i, pct := range cohorts {
		if pct <= 0 || pct > 100 {
			return fmt.Errorf("%w: cohort %d is %d%%, must be between 1 and 100", ErrInvalid, i, pct)
		}
		if pct <= prev {
			return fmt.Errorf("%w: cohort %d is %d%%, which does not advance on the previous %d%%; "+
				"cohort percentages are cumulative", ErrInvalid, i, pct, prev)
		}
		prev = pct
	}
	if prev != 100 {
		return fmt.Errorf("%w: the last cohort is %d%%; a rollout must finish at 100%% or part of the fleet "+
			"stays on the old firmware with the job reporting success", ErrInvalid, prev)
	}
	return nil
}

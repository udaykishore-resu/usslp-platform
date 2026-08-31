package sgu

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Conflict-free merge
//
// While the WAN is down both sides keep changing. The cloud accepts price
// changes from head office; the store accepts them from its own point of sale,
// activates scheduled promotions and counts its own stock. When the link
// returns, some keys have moved on both sides and something has to decide.
//
// The mechanism is a last-writer-wins register over a hybrid logical clock,
// with two deliberate departures from pure last-writer-wins, because pure
// last-writer-wins is wrong for this domain in two specific ways:
//
//  1. A key that only one side touched is not a conflict at all, whatever the
//     timestamps say. Pure LWW would let a cloud value that predates the outage
//     overwrite a local change made during it, purely because the cloud's clock
//     had ticked more recently on some unrelated field. Divergence has to be
//     tracked from the moment the link broke.
//
//  2. Where both sides did change, the winner is decided by what the value
//     *is*, not by when it was written. Price is the cloud's to decide: head
//     office owns pricing, the store's local override was an emergency measure,
//     and a promotion the merchandising team launched must not be silently
//     reverted because a till in one store was a second later. Inventory is the
//     store's: the cloud's stock figure is a projection of events, and the
//     store's is a count of things on a shelf. Neither of these follows from a
//     timestamp, and neither should be left to one.
//
// Everything a policy does not cover falls through to the hybrid clock, which
// is where last-writer-wins is actually appropriate.
// ---------------------------------------------------------------------------

// Origin identifies which side of the link wrote a value.
type Origin string

// The two writers.
const (
	// OriginCloud is the platform: head office pricing, promotions, planograms.
	OriginCloud Origin = "cloud"
	// OriginLocal is the store: its point of sale, its schedule activating on
	// local time, its own stock counts.
	OriginLocal Origin = "local"
)

// Kind selects the conflict policy for a key.
type Kind string

// The kinds with an explicit policy. Anything else falls through to the clock.
const (
	// KindPricing is a displayed price. The cloud is the system of record.
	KindPricing Kind = "pricing"
	// KindInventory is a stock level. The store is the system of record.
	KindInventory Kind = "inventory"
	// KindOther has no domain policy and is resolved by the hybrid clock.
	KindOther Kind = "other"
)

// Register is one last-writer-wins value.
type Register struct {
	Key   string          `json:"key"`
	Kind  Kind            `json:"kind"`
	Value json.RawMessage `json:"value"`
	// TS is the hybrid logical clock timestamp of the write.
	TS     HLC    `json:"ts"`
	Origin Origin `json:"origin"`
	// WrittenAt is the physical instant, kept for the audit trail. It is never
	// used to order anything.
	WrittenAt time.Time `json:"written_at"`
}

// Exists reports whether the register holds a value.
func (r Register) Exists() bool { return r.Key != "" && !r.TS.IsZero() }

// Resolution names how a merge was decided. The strings appear in the
// reconciliation report an operator reads after an outage, so they are written
// for that reader.
type Resolution string

// The possible outcomes of a merge.
const (
	// ResolutionOnlyLocal means only the store changed this key.
	ResolutionOnlyLocal Resolution = "local-only change"
	// ResolutionOnlyCloud means only the cloud changed it.
	ResolutionOnlyCloud Resolution = "cloud-only change"
	// ResolutionIdentical means both sides hold the same bytes.
	ResolutionIdentical Resolution = "identical on both sides"
	// ResolutionPolicyCloudPricing means both changed and the pricing policy
	// gave it to the cloud.
	ResolutionPolicyCloudPricing Resolution = "conflict: cloud wins for pricing"
	// ResolutionPolicyLocalInventory means both changed and the inventory policy
	// gave it to the store.
	ResolutionPolicyLocalInventory Resolution = "conflict: local wins for inventory"
	// ResolutionClock means both changed and the hybrid clock decided.
	ResolutionClock Resolution = "conflict: resolved by hybrid logical clock"
)

// MergeResult is the outcome of merging one key.
type MergeResult struct {
	Winner Register `json:"winner"`
	// Loser is the value that was discarded, present only on a real conflict.
	// It is retained because a store manager whose local price was overridden is
	// entitled to see what it was.
	Loser *Register `json:"loser,omitempty"`
	// Conflict is true only when both sides changed the key after divergence.
	// It is what StoreModeChanged.Conflicts counts.
	Conflict   bool       `json:"conflict"`
	Resolution Resolution `json:"resolution"`
}

// Merge reconciles one key's two versions.
//
// divergedAt is the hybrid clock timestamp at the moment the link broke.
// Everything at or before it is common history that both sides agree on;
// everything after it happened while they could not see each other. Passing a
// zero divergedAt makes every write look like a divergent one, which is the
// safe direction to be wrong in: it produces conflicts to review rather than
// silent overwrites.
func Merge(local, cloud Register, divergedAt HLC) MergeResult {
	localChanged := local.Exists() && local.TS.After(divergedAt)
	cloudChanged := cloud.Exists() && cloud.TS.After(divergedAt)

	switch {
	case !local.Exists() && !cloud.Exists():
		return MergeResult{Resolution: ResolutionIdentical}
	case !cloud.Exists():
		return MergeResult{Winner: local, Resolution: ResolutionOnlyLocal}
	case !local.Exists():
		return MergeResult{Winner: cloud, Resolution: ResolutionOnlyCloud}
	}

	if string(local.Value) == string(cloud.Value) {
		// Identical bytes are not a conflict however the timestamps compare.
		// Keeping the later one preserves causality for the next merge.
		winner := cloud
		if local.TS.After(cloud.TS) {
			winner = local
		}
		return MergeResult{Winner: winner, Resolution: ResolutionIdentical}
	}

	switch {
	case localChanged && !cloudChanged:
		// The cloud's value is pre-outage history the store has moved past.
		return MergeResult{Winner: local, Resolution: ResolutionOnlyLocal}
	case cloudChanged && !localChanged:
		return MergeResult{Winner: cloud, Resolution: ResolutionOnlyCloud}
	case !cloudChanged && !localChanged:
		// Neither side wrote after divergence but the values differ, which means
		// they diverged before the outage and one side never caught up. The
		// clock is the right arbiter: there is no domain question here, only a
		// missed replication.
		winner, loser := cloud, local
		if local.TS.After(cloud.TS) {
			winner, loser = local, cloud
		}
		return MergeResult{Winner: winner, Loser: &loser, Resolution: ResolutionClock}
	}

	// Both sides changed the same key while they could not see each other.
	switch local.Kind {
	case KindPricing:
		loser := local
		return MergeResult{Winner: cloud, Loser: &loser, Conflict: true,
			Resolution: ResolutionPolicyCloudPricing}
	case KindInventory:
		loser := cloud
		return MergeResult{Winner: local, Loser: &loser, Conflict: true,
			Resolution: ResolutionPolicyLocalInventory}
	default:
		winner, loser := cloud, local
		if local.TS.After(cloud.TS) {
			winner, loser = local, cloud
		}
		return MergeResult{Winner: winner, Loser: &loser, Conflict: true,
			Resolution: ResolutionClock}
	}
}

// ConflictRecord is one resolved conflict, as it appears in the reconciliation
// report.
type ConflictRecord struct {
	Key        string     `json:"key"`
	Kind       Kind       `json:"kind"`
	Resolution Resolution `json:"resolution"`
	WinnerTS   string     `json:"winner_ts"`
	LoserTS    string     `json:"loser_ts"`
	WinnerFrom Origin     `json:"winner_from"`
	// Discarded is the value that lost, so a store manager can see what their
	// local change was before it was overridden.
	Discarded json.RawMessage `json:"discarded_value,omitempty"`
}

// ReconciliationReport is the whole outcome of rejoining the cloud.
//
// It is a first-class artefact rather than a log line because it is what an
// operations team looks at after every outage, and because
// canon.StoreModeChanged carries only the summary counts: the detail has to
// live somewhere a human can reach.
type ReconciliationReport struct {
	StoreID string `json:"store_id"`
	SGUID   string `json:"sgu_id"`
	// StartedAt and FinishedAt bracket the reconciliation itself.
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// OutageSeconds is how long the store was autonomous.
	OutageSeconds int64 `json:"outage_seconds"`
	// DivergedAt is the hybrid clock timestamp the merge treated as the last
	// point of agreement.
	DivergedAt string `json:"diverged_at"`
	// Flushed, Deduplicated and Dropped account for every message that was in
	// the buffer when the link came back.
	Flushed        int `json:"flushed"`
	Deduplicated   int `json:"deduplicated"`
	Dropped        int `json:"dropped_on_overflow"`
	FlushFailed    int `json:"flush_failed"`
	KeysCompared   int `json:"keys_compared"`
	KeysChanged    int `json:"keys_changed"`
	Conflicts      int `json:"conflicts_resolved"`
	ClockSkew      SkewReport
	ConflictDetail []ConflictRecord `json:"conflicts,omitempty"`
	// Lossy is true when the buffer overflowed during the outage, so the cloud's
	// view of what happened in this store is incomplete and somebody has to be
	// told rather than left to infer it from a counter.
	Lossy bool `json:"lossy"`
}

// Summary renders the report as the single line an operator reads first.
func (r ReconciliationReport) Summary() string {
	lossy := ""
	if r.Lossy {
		lossy = " (BUFFER OVERFLOWED: the cloud's record of this outage is incomplete)"
	}
	return fmt.Sprintf(
		"store %s reconciled after %s autonomous: %d messages flushed, %d deduplicated, %d dropped, "+
			"%d of %d keys changed, %d conflicts resolved, clock skew %v%s",
		r.StoreID, (time.Duration(r.OutageSeconds) * time.Second).String(),
		r.Flushed, r.Deduplicated, r.Dropped, r.KeysChanged, r.KeysCompared, r.Conflicts,
		r.ClockSkew.Last.Round(time.Millisecond), lossy)
}

// sortConflicts orders the detail deterministically so two readings of the same
// report are identical.
func sortConflicts(cs []ConflictRecord) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].Key < cs[j].Key })
}

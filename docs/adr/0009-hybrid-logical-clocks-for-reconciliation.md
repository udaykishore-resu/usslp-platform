# 0009 — Hybrid logical clocks and a typed conflict policy, not wall-clock last-writer-wins

**Status:** Accepted

---

## Context

[0003](0003-edge-first-architecture.md) makes a store keep trading through a WAN
outage. That means both sides go on changing state while they cannot see each
other: the cloud accepts price changes from head office, and the store accepts
them from its own point of sale, activates scheduled promotions and counts its own
stock. When the link returns, some keys have moved on both sides and something has
to decide.

The default answer is last-writer-wins on a wall-clock timestamp. It is wrong here
in two independent ways.

**The clock is the least trustworthy thing in the building.** The whole premise
of an autonomous store is that it has been out of contact, which is precisely
when its NTP discipline has been unavailable longest. A store whose real-time
clock has drifted four minutes fast wins every last-writer-wins merge for as long
as the drift lasts, and does so silently.

**Recency is the wrong criterion anyway.** Whether the cloud's price or the
store's price should win is not a question about time. It is a question about
which side is the system of record for that kind of value, and the answer differs
by kind.

## Decision

Two mechanisms, layered.

### A hybrid logical clock for order

`edge/sgu/hlc.go` implements an HLC: a physical timestamp plus a logical counter.
It keeps the useful property of a wall clock — the timestamps are approximately
real times, so a human can read them and an auditor can correlate them — while
the counter enforces causality regardless of drift. Two events the gateway knows
the order of are ordered correctly whatever the RTC says, and **the drift becomes
a measurable quantity rather than an invisible bias**.

A remote timestamp far enough ahead of local physical time that adopting it would
corrupt the local clock is refused with `ErrClockSkew` rather than swallowed,
because it means somebody's NTP is broken and a human should know.

The package comment is explicit that the clock is not the whole answer and does
not claim to be: for pricing, the conflict policy overrides it outright.

### A typed conflict policy for the decision

`edge/sgu/crdt.go` is a last-writer-wins register over the HLC with two
deliberate departures, both stated in the file:

**1. A key only one side touched is not a conflict, whatever the timestamps
say.** Pure LWW would let a cloud value that predates the outage overwrite a
local change made during it, purely because the cloud's clock ticked more
recently on some unrelated field. Divergence is therefore tracked from the moment
the link broke: `onModeChange` records `divergedAt` from the HLC the instant the
store goes autonomous, and anything stamped after that happened while the two
sides could not see each other.

**2. Where both sides changed, the winner is decided by what the value *is*.**

| Kind | System of record | Why |
|---|---|---|
| `pricing` | **Cloud** | Head office owns pricing. A local override was an emergency measure, and a promotion the merchandising team launched must not be silently reverted because a till in one store was a second later. |
| `inventory` | **Store** | The cloud's stock figure is a projection of events; the store's is a count of things on a shelf. |
| anything else | The hybrid clock | Where last-writer-wins is genuinely appropriate. |

Neither of the first two follows from a timestamp, and neither should be left to
one.

### Collection is armed for the whole outage, not just at recovery

A subtlety in `onModeChange` worth recording because it is easy to get wrong: the
MQTT client reconnects and restores its subscriptions on its own backoff, so the
cloud's retained state can arrive milliseconds after the link returns and several
seconds *before* the WAN detector's hysteresis admits the store is back. Anything
downstream arriving while the store still considers itself autonomous is, by
definition, the cloud's view crossing a link that has just come back — exactly
what the merge needs, and exactly what must not be applied blindly over what the
store decided while it was alone. So `collecting` is set on entry to autonomy,
not on exit.

## Consequences

**Measured.** `TestStoreSurvivesWANOutage` on a 2-core container: a 7-second
outage, 6 messages flushed in order, 0 deduplicated, 0 dropped, 16 keys compared,
1 changed, 0 conflicts, `Lossy: false`. Clock skew: last −6 ms, worst 6 ms, 35
timestamps adopted, 0 rejected, against a configured `MaxSkew` of 10 minutes.

**The reconciliation report is a first-class artefact, not a log line.** It
carries the outage duration, the divergence point, flush and drop counts, keys
compared and changed, the conflicts resolved with their detail, the measured
clock skew, and a `Lossy` flag set when a `critical`-class message had to be
dropped from the upstream buffer. It is available on the gateway's diagnostics
surface. `USSLPMergeConflictsHigh` alerts on `sgu_merge_conflicts_total`.

**The clock skew is published rather than hidden**, which is the property that
makes "the promotion started four minutes early" findable rather than deducible
from till receipts.

**The conflict policy is a policy, and it can be wrong for a retailer.** "The
cloud owns pricing" is right for a chain with central merchandising and wrong for
a franchise operator whose stores set their own prices. The policy is per-kind
and configurable rather than hard-coded, but the *default* encodes an assumption
about how a retailer is organised, and a deployment that disagrees has to say so.

**This is not a general CRDT.** It is a last-writer-wins register with domain
overrides. It converges — every replica applying the same policy to the same
inputs reaches the same state — but it does not preserve both sides' intent the
way a genuine multi-value register or an operational-transform structure would.
For a price, discarding the loser is the correct behaviour: a shelf shows one
number.

**Two nodes, not N.** The merge is between one store and the cloud. A design for
store-to-store reconciliation would need a different structure; nothing here
generalises to it and nothing claims to.

## Alternatives considered

**Wall-clock last-writer-wins.** Rejected on both grounds in *Context*. The clock
argument alone is decisive: the merge's correctness would depend on the accuracy
of the least-disciplined clock in the estate at the moment it was least
disciplined.

**Vector clocks.** Correct causality without any dependence on physical time.
Rejected because they carry no human-readable time at all, and an auditor asking
"when did this store activate that promotion" needs an answer in minutes past
eight, not a version vector. An HLC gives both.

**Lamport clocks.** Same objection, plus they lose the ability to bound skew or
detect a broken RTC — which is a diagnostic the platform actively wants.

**Refusing local writes during an outage.** Would eliminate divergence entirely.
Rejected: it is the same thing as not having store autonomy, and it is precisely
the case a store manager standing next to a mispriced item needs.

**Making the cloud replay and win unconditionally on reconnect.** Simple, and
wrong: it would silently discard everything the store did while it was alone,
including price changes customers have already been charged against.

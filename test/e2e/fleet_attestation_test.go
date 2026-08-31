package e2e

import (
	"fmt"
	"sort"
	"testing"

	"github.com/usslp/usslp/edge/labelsim"
	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
)

// The two tests in this file are a fleet audit rather than a behaviour test.
//
// They exist because of a specific way this platform can break, and the way it
// broke was instructive: the edge tier gained end-to-end attestation — the
// controller forwards the signed tuple to the label, and the label rebuilds the
// canonical string and checks the Ed25519 signature against its own key ring
// before driving a pixel — and `usslpd` wired the ring into the controllers but
// not into the labels. Every label then failed closed, which is the correct
// behaviour for a label that cannot verify and a total outage for a store.
//
// What makes that worth guarding is the shape of the two available fixes. One
// is to give the labels the ring. The other is to set the fleet to
// `AttestTrustController`, which makes the symptom disappear, leaves every
// price flowing, and silently deletes the security property the compliance
// story rests on. Nothing in a latency test or a delivery count can tell those
// two apart: a fleet that verifies nothing is indistinguishable, by every other
// measure in this suite, from a fleet that verifies everything.
//
// So these assert the property directly. One says the fleet is in a mode where
// it verifies; the other says it actually did, and refused nothing, over a
// complete boot.

// TestEveryLabelVerifiesForItself fails if any label in a booted fleet would
// display a price it had not checked.
//
// It audits both fleets the suite builds — the shared four-controller store and
// a freshly booted one — because the defect this guards against lives in the
// assembly code, and a shape that is only ever built once is a shape that is
// only ever tested once.
func TestEveryLabelVerifiesForItself(t *testing.T) {
	audit := func(t *testing.T, st *stack.Stack, what string) {
		t.Helper()
		checked, byMode := 0, map[string]int{}
		var offenders []string
		for _, store := range st.Stores() {
			for _, z := range store.Zones {
				// Every label the zone holds, not only the commissioned ones:
				// the held-back spare is enrolled later against a running
				// platform, and a spare built without a ring would fail the
				// moment it was commissioned rather than at boot.
				for _, l := range z.Sim.Labels() {
					checked++
					mode := l.AttestationMode()
					byMode[mode.String()]++
					if mode != labelsim.AttestEndToEnd {
						offenders = append(offenders,
							fmt.Sprintf("%s (%s)", l.ID(), mode))
					}
				}
			}
		}
		if checked == 0 {
			t.Fatalf("%s: the audit found no labels at all, so it asserted nothing", what)
		}
		modes := make([]string, 0, len(byMode))
		for m, n := range byMode {
			modes = append(modes, fmt.Sprintf("%s=%d", m, n))
		}
		sort.Strings(modes)
		t.Logf("%s: %d label(s) audited: %v", what, checked, modes)
		if len(offenders) > 0 {
			shown := offenders
			if len(shown) > 5 {
				shown = shown[:5]
			}
			t.Errorf("%s: %d of %d label(s) are not in end-to-end attestation mode "+
				"and would display a price they had not verified (for example %v). "+
				"AttestTrustController is a compatibility mode for firmware that "+
				"predates frame type 4, not a way to make a wiring fault go away.",
				what, len(offenders), checked, shown)
		}
	}

	audit(t, shared, "the shared store")
	audit(t, newStack(t, smallStore(2, 6)), "a freshly booted store")
}

// TestFleetBootsWithNoAttestationRefusal fails if a fleet reaches the end of
// start-up having refused anything it could not verify.
//
// The runtime does not declare a store open until every label is showing a
// price, so by the time Start returns, every label has taken and verified at
// least one attested frame. That gives the test two halves, and it needs both:
// no refusals, and a non-zero count of successful verifications. Refusals alone
// would pass on a fleet that never received anything, which is exactly the
// state a broken key ring would produce if the runtime's own start-up gate had
// not caught it first.
//
// It boots its own store rather than using the shared one so that "at the end
// of boot" is literally what is being measured, independent of what any other
// test in this package has done.
func TestFleetBootsWithNoAttestationRefusal(t *testing.T) {
	st := newStack(t, smallStore(2, 6))

	var (
		labels        int
		verifications int64
		attFailures   int64
		unattested    int64
		badFrames     int64
	)
	type refusal struct {
		id                              string
		failures, unattested, badFrames int64
	}
	var refusals []refusal

	for _, store := range st.Stores() {
		for _, z := range store.Zones {
			for _, id := range z.Labels() {
				l, ok := z.Sim.Label(id)
				if !ok {
					t.Fatalf("%s is commissioned but the zone has no such panel", id)
				}
				labels++
				ls := l.Stats()
				verifications += ls.Verifications
				attFailures += ls.AttestationFailures
				unattested += ls.UnattestedRefused
				badFrames += ls.BadFrames
				if ls.AttestationFailures > 0 || ls.UnattestedRefused > 0 || ls.BadFrames > 0 {
					refusals = append(refusals, refusal{
						id: string(id), failures: ls.AttestationFailures,
						unattested: ls.UnattestedRefused, badFrames: ls.BadFrames,
					})
				}
			}
		}
	}

	t.Logf("%d label(s) after boot: %d verification(s), %d attestation failure(s), "+
		"%d unattested frame(s) refused, %d bad frame(s)",
		labels, verifications, attFailures, unattested, badFrames)

	if labels == 0 {
		t.Fatal("the fleet has no commissioned labels, so this asserted nothing")
	}
	if verifications < int64(labels) {
		t.Errorf("the fleet recorded %d successful verification(s) across %d label(s); "+
			"every label was priced during boot, so each should have verified at least "+
			"once. A lower number means prices are reaching the glass without being "+
			"checked, which is the failure this test exists to catch",
			verifications, labels)
	}
	if len(refusals) > 0 {
		shown := refusals
		if len(shown) > 5 {
			shown = shown[:5]
		}
		t.Errorf("%d of %d label(s) refused a price during boot (for example %+v). "+
			"A label refuses when it cannot verify: the usual cause is the "+
			"price-authority key ring not reaching the fleet, which leaves every "+
			"shelf blank and every panel correct to have kept it that way",
			len(refusals), labels, shown)
	}
}

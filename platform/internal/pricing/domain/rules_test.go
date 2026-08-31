package domain

import (
	"strings"
	"testing"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// baseConstraints is a plausible grocery SKU: 89 cents cost, 25% minimum
// margin, currently on the shelf at $1.49.
func baseConstraints() Constraints {
	return Constraints{
		Currency:     "USD",
		UnitCost:     89,
		MinMarginBps: 2500,
		CurrentMinor: 149,
	}
}

func TestEvaluateConstraintEnforcement(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*Constraints)
		requested   int64
		wantOutcome Outcome
		wantPrice   int64
		wantKinds   []ConstraintKind
	}{
		{
			name:        "compliant price is accepted unchanged",
			requested:   149,
			wantOutcome: OutcomeAccepted,
			wantPrice:   149,
		},
		{
			// cost 89 at 25% margin needs 89/0.75 = 118.67 -> 119.
			name:        "below minimum margin is raised to the margin floor",
			requested:   100,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   119,
			wantKinds:   []ConstraintKind{KindMinMargin},
		},
		{
			name:        "regulatory floor binds above the margin floor",
			mutate:      func(c *Constraints) { c.FloorMinor = 130 },
			requested:   120,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   130,
			wantKinds:   []ConstraintKind{KindRegulatoryFloor},
		},
		{
			name:        "regulatory ceiling caps the price",
			mutate:      func(c *Constraints) { c.CeilingMinor = 160 },
			requested:   200,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   160,
			wantKinds:   []ConstraintKind{KindRegulatoryCeiling},
		},
		{
			// competitor 150 with a 5% band permits [143, 158] (rounded).
			name: "competitor parity bounds the price",
			mutate: func(c *Constraints) {
				c.CompetitorMinor = 150
				c.CompetitorBandBps = 500
			},
			requested:   200,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   158,
			wantKinds:   []ConstraintKind{KindCompetitorParity},
		},
		{
			// current 149 with a 10% max change permits [134, 164].
			name:        "max change per period bounds movement",
			mutate:      func(c *Constraints) { c.MaxChangeBps = 1000 },
			requested:   250,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   164,
			wantKinds:   []ConstraintKind{KindMaxChange},
		},
		{
			name: "exhausted change budget pins the price to the shelf price",
			mutate: func(c *Constraints) {
				c.MaxChangesPerPeriod = 2
				c.ChangesThisPeriod = 2
			},
			requested:   139,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   149,
			wantKinds:   []ConstraintKind{KindChangeFrequency},
		},
		{
			name: "exhausted change budget still permits re-asserting the current price",
			mutate: func(c *Constraints) {
				c.MaxChangesPerPeriod = 2
				c.ChangesThisPeriod = 5
			},
			requested:   149,
			wantOutcome: OutcomeAccepted,
			wantPrice:   149,
		},
		{
			name:        "charm ending snaps to .99",
			mutate:      func(c *Constraints) { c.Ending = EndingCharm },
			requested:   150,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   199,
			wantKinds:   []ConstraintKind{KindEndingPolicy},
		},
		{
			name: "charm ending is waived when no .99 price is feasible",
			mutate: func(c *Constraints) {
				c.Ending = EndingCharm
				c.FloorMinor = 150
				c.CeilingMinor = 160
			},
			requested:   155,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   155,
			wantKinds:   []ConstraintKind{KindEndingPolicy},
		},
		{
			name:        "nickel ending snaps to a multiple of five",
			mutate:      func(c *Constraints) { c.Ending = EndingNickel },
			requested:   152,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   150,
			wantKinds:   []ConstraintKind{KindEndingPolicy},
		},
		{
			name:        "granularity snaps to the lattice",
			mutate:      func(c *Constraints) { c.GranularityMinor = 10 },
			requested:   153,
			wantOutcome: OutcomeAdjusted,
			wantPrice:   150,
			wantKinds:   []ConstraintKind{KindEndingPolicy},
		},
		{
			name: "a floor above a ceiling is infeasible, not arbitrary",
			mutate: func(c *Constraints) {
				c.FloorMinor = 300
				c.CeilingMinor = 200
			},
			requested:   250,
			wantOutcome: OutcomeInfeasible,
			wantKinds:   []ConstraintKind{KindRegulatoryCeiling, KindRegulatoryFloor},
		},
		{
			name: "a margin floor above a competitor ceiling is infeasible",
			mutate: func(c *Constraints) {
				c.UnitCost = 200
				c.MinMarginBps = 3000 // needs 286
				c.CompetitorMinor = 220
				c.CompetitorBandBps = 200 // permits [216, 224]
			},
			requested:   250,
			wantOutcome: OutcomeInfeasible,
			wantKinds:   []ConstraintKind{KindCompetitorParity, KindMinMargin},
		},
		{
			name: "a max-change bound that excludes the margin floor is infeasible",
			mutate: func(c *Constraints) {
				c.UnitCost = 500
				c.MinMarginBps = 2000 // needs 625
				c.CurrentMinor = 300
				c.MaxChangeBps = 500 // permits [285, 315]
			},
			requested:   320,
			wantOutcome: OutcomeInfeasible,
			wantKinds:   []ConstraintKind{KindMaxChange, KindMinMargin},
		},
		{
			name:        "a negative price is invalid, not merely non-compliant",
			requested:   -1,
			wantOutcome: OutcomeInvalid,
		},
		{
			name:        "a bad currency is invalid",
			mutate:      func(c *Constraints) { c.Currency = "DOLLARS" },
			requested:   149,
			wantOutcome: OutcomeInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := baseConstraints()
			if tt.mutate != nil {
				tt.mutate(&c)
			}
			d := Evaluate(c, tt.requested)
			if d.Outcome != tt.wantOutcome {
				t.Fatalf("outcome = %s, want %s (violations %+v)", d.Outcome, tt.wantOutcome, d.Violations)
			}
			if tt.wantOutcome == OutcomeAccepted || tt.wantOutcome == OutcomeAdjusted {
				if d.Price.Amount != tt.wantPrice {
					t.Errorf("price = %d, want %d (violations %+v)", d.Price.Amount, tt.wantPrice, d.Violations)
				}
				if d.Price.Currency != c.Currency {
					t.Errorf("currency = %q, want %q", d.Price.Currency, c.Currency)
				}
			}
			for _, want := range tt.wantKinds {
				found := false
				for _, v := range d.Violations {
					if v.Kind == want {
						found = true
					}
				}
				if !found {
					t.Errorf("violation %s not reported; got %+v", want, d.Violations)
				}
			}
		})
	}
}

// TestInfeasibilityNamesEveryConflictingRule is the property that separates a
// useful infeasible result from a useless one: an operator must be able to see
// which pair of rules disagrees, not merely that something does.
func TestInfeasibilityNamesEveryConflictingRule(t *testing.T) {
	c := baseConstraints()
	c.UnitCost = 400
	c.MinMarginBps = 4000 // needs 667
	c.CeilingMinor = 500
	c.CompetitorMinor = 450
	c.CompetitorBandBps = 100 // permits [446, 455]

	d := Evaluate(c, 500)
	if d.Outcome != OutcomeInfeasible {
		t.Fatalf("outcome = %s, want infeasible", d.Outcome)
	}
	if !d.Feasible.Empty {
		t.Error("feasible range should be marked empty")
	}
	kinds := map[ConstraintKind]bool{}
	for _, v := range d.Violations {
		kinds[v.Kind] = true
		if v.Detail == "" {
			t.Errorf("violation %s has no detail", v.Kind)
		}
	}
	// The margin floor at 667 is above the ceiling at 500 and above the
	// competitor band's top at 455; all three participate.
	for _, want := range []ConstraintKind{KindMinMargin, KindRegulatoryCeiling, KindCompetitorParity} {
		if !kinds[want] {
			t.Errorf("infeasibility report omits %s: %+v", want, d.Violations)
		}
	}
}

// TestEvaluateIsDeterministic pins the property the SGU depends on: the same
// inputs give a byte-identical decision, so an offline store and the cloud
// cannot disagree.
func TestEvaluateIsDeterministic(t *testing.T) {
	c := baseConstraints()
	c.CompetitorMinor = 155
	c.CompetitorBandBps = 700
	c.Ending = EndingCharm
	first := Evaluate(c, 172)
	for i := 0; i < 100; i++ {
		got := Evaluate(c, 172)
		if got.Price != first.Price || got.Outcome != first.Outcome || len(got.Violations) != len(first.Violations) {
			t.Fatalf("iteration %d differs: %+v vs %+v", i, got, first)
		}
		for j := range got.Violations {
			if got.Violations[j] != first.Violations[j] {
				t.Fatalf("iteration %d violation %d differs", i, j)
			}
		}
	}
}

func TestEvaluateDoesNotAllocateOnTheAcceptedPath(t *testing.T) {
	c := baseConstraints()
	allocs := testing.AllocsPerRun(200, func() {
		d := Evaluate(c, 149)
		if d.Outcome != OutcomeAccepted {
			t.Fatalf("outcome = %s", d.Outcome)
		}
	})
	// canon.NewMoney does not allocate, and no violation slice is built, so
	// the accepted path should be allocation-free. This is a hot-path budget
	// assertion, not a micro-optimisation: Tier 1 runs on every one of 52,000
	// price updates a second.
	if allocs > 0 {
		t.Errorf("accepted path allocated %.1f times per run, want 0", allocs)
	}
}

func TestCandidatesRespectEndingAndBounds(t *testing.T) {
	c := baseConstraints()
	c.FloorMinor = 150
	c.CeilingMinor = 400
	c.Ending = EndingCharm

	got := c.Candidates(0)
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	for _, p := range got {
		if p < 150 || p > 400 {
			t.Errorf("candidate %d is outside [150, 400]", p)
		}
		if p%100 != 99 {
			t.Errorf("candidate %d does not end in .99", p)
		}
		if d := Evaluate(c, p); d.Outcome != OutcomeAccepted {
			t.Errorf("candidate %d is not accepted by Tier 1: %s %+v", p, d.Outcome, d.Violations)
		}
	}
	if len(got) != 3 { // 199, 299, 399
		t.Errorf("got %d candidates %v, want 3", len(got), got)
	}
}

func TestCandidatesEmptyWhenInfeasible(t *testing.T) {
	c := baseConstraints()
	c.FloorMinor = 500
	c.CeilingMinor = 100
	if got := c.Candidates(0); len(got) != 0 {
		t.Errorf("infeasible constraints produced %d candidates", len(got))
	}
}

func TestPolicyPackRoundTrip(t *testing.T) {
	pack := PolicyPack{
		Tenant: "acme", Store: "store-001", Version: 42,
		Rules: map[canon.SKU]Constraints{
			"sku-a": baseConstraints(),
			"sku-b": {Currency: "GBP", UnitCost: 250, MinMarginBps: 3000, CurrentMinor: 399, Ending: EndingCharm},
		},
	}
	b, err := pack.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back PolicyPack
	if err := back.UnmarshalBinary(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Tenant != pack.Tenant || back.Store != pack.Store || back.Version != pack.Version {
		t.Errorf("header mismatch: %+v", back)
	}
	if len(back.Rules) != len(pack.Rules) {
		t.Fatalf("got %d rules, want %d", len(back.Rules), len(pack.Rules))
	}
	for sku, want := range pack.Rules {
		if got := back.Rules[sku]; got != want {
			t.Errorf("rule %s: got %+v, want %+v", sku, got, want)
		}
	}

	// The encoding must be deterministic, which is what lets the SGU skip a
	// reload by comparing checksums.
	b2, err := pack.MarshalBinary()
	if err != nil {
		t.Fatalf("second marshal: %v", err)
	}
	if string(b) != string(b2) {
		t.Error("encoding is not deterministic")
	}
}

func TestPolicyPackRejectsCorruption(t *testing.T) {
	pack := PolicyPack{Tenant: "acme", Store: "s1", Version: 1,
		Rules: map[canon.SKU]Constraints{"sku-a": baseConstraints()}}
	b, err := pack.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	t.Run("flipped byte", func(t *testing.T) {
		bad := append([]byte(nil), b...)
		bad[len(bad)/2] ^= 0xff
		var p PolicyPack
		if err := p.UnmarshalBinary(bad); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Errorf("err = %v, want a checksum failure", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		var p PolicyPack
		if err := p.UnmarshalBinary(b[:len(b)-8]); err == nil {
			t.Error("truncated pack decoded without error")
		}
	})
	t.Run("bad magic", func(t *testing.T) {
		bad := append([]byte(nil), b...)
		bad[0] = 'X'
		var p PolicyPack
		if err := p.UnmarshalBinary(bad); err == nil || !strings.Contains(err.Error(), "magic") {
			t.Errorf("err = %v, want a magic failure", err)
		}
	})
}

func TestPolicyPackEvaluateFallsThroughForUnknownSKU(t *testing.T) {
	pack := PolicyPack{Tenant: "acme", Store: "s1", Rules: map[canon.SKU]Constraints{}}
	d := pack.Evaluate("unknown", 999, "USD")
	if d.Outcome != OutcomeAccepted || d.Price.Amount != 999 {
		t.Errorf("unknown SKU: got %s at %d, want accepted at 999", d.Outcome, d.Price.Amount)
	}
}

func TestSeasonMapping(t *testing.T) {
	// Meteorological seasons: December, January and February are index 0.
	cases := []struct {
		month int
		north int
		south int
	}{{1, 0, 2}, {3, 1, 3}, {7, 2, 0}, {10, 3, 1}, {12, 0, 2}}
	for _, c := range cases {
		local := timeIn(c.month)
		if got := Season(local, false); got != c.north {
			t.Errorf("month %d northern season = %d, want %d", c.month, got, c.north)
		}
		if got := Season(local, true); got != c.south {
			t.Errorf("month %d southern season = %d, want %d", c.month, got, c.south)
		}
	}
}

package domain

import (
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// Default policy values. They are named constants rather than magic numbers in
// the decision functions because every one of them is a business rule that a
// retailer will eventually ask to see, argue with, and have changed.
const (
	// DefaultEffectiveGrace is how far into the past an effective_at may sit
	// and still be applied.
	//
	// Two hours, not zero: a store gateway that has been offline through a WAN
	// outage floods the platform with genuinely-wanted updates whose
	// effective_at is minutes or hours old, and rejecting those would mean a
	// store comes back from an outage showing yesterday's prices. Two hours
	// covers the realistic outage and the realistic clock skew of a POS that
	// nobody has run NTP against, while a nightly batch that a POS replays a
	// week late is still refused.
	DefaultEffectiveGrace = 2 * time.Hour

	// DefaultScheduleHorizon caps how far ahead a price change may be
	// scheduled. Beyond it the platform is being used as a calendar, and a
	// year-out price sitting in an aggregate is a price nobody will remember
	// authorising.
	DefaultScheduleHorizon = 90 * 24 * time.Hour

	// DefaultGuardrailFactor is the multiple beyond which a price change is
	// treated as a corrupt feed rather than a decision. Five is deliberately
	// generous — clearance markdowns of 80% (a 5x drop) are routine and must
	// pass — while the failure this exists to stop, a decimal point lost
	// somewhere between an ERP and a CSV, moves a price by 10x or 100x.
	DefaultGuardrailFactor = 5.0

	// DefaultGuardrailFloorMinor is the amount below which the ratio guard is
	// not applied, in minor units.
	//
	// Ratios are meaningless at the bottom of the range: a loose-leaf herb
	// going from 5c to 50c is a 10x move and a perfectly ordinary one. The
	// guard therefore only engages once the larger of the two prices is at
	// least this much, which is where an order-of-magnitude error starts to
	// cost a shopper real money.
	DefaultGuardrailFloorMinor = 100

	// DefaultFullRefreshEvery is how many consecutive partial refreshes a label
	// may take before a full waveform is forced. See DecideRender for the
	// ghosting reasoning.
	DefaultFullRefreshEvery = 8
)

// Policy is the tenant-configurable rule set the aggregate decides against.
//
// It is a value, not an interface, and it is passed into every decision rather
// than read from ambient state, so that a command's outcome is a pure function
// of (aggregate, command, policy) and a rejection can be reproduced exactly
// from an audit record months later.
type Policy struct {
	// EffectiveGrace is how stale an effective_at may be.
	EffectiveGrace time.Duration
	// ScheduleHorizon caps forward scheduling.
	ScheduleHorizon time.Duration
	// GuardrailFactor is the maximum multiple, up or down, between the
	// displayed price and the new one.
	GuardrailFactor float64
	// GuardrailFloorMinor is the amount below which the ratio guard is skipped.
	GuardrailFloorMinor int64
	// FullRefreshEvery forces a full E-Ink waveform after this many partials.
	FullRefreshEvery int
}

// DefaultPolicy returns the platform defaults.
func DefaultPolicy() Policy {
	return Policy{
		EffectiveGrace:      DefaultEffectiveGrace,
		ScheduleHorizon:     DefaultScheduleHorizon,
		GuardrailFactor:     DefaultGuardrailFactor,
		GuardrailFloorMinor: DefaultGuardrailFloorMinor,
		FullRefreshEvery:    DefaultFullRefreshEvery,
	}
}

// WithDefaults fills unset fields from the platform defaults, so a tenant
// override can specify only the one knob it cares about.
func (p Policy) WithDefaults() Policy {
	d := DefaultPolicy()
	if p.EffectiveGrace <= 0 {
		p.EffectiveGrace = d.EffectiveGrace
	}
	if p.ScheduleHorizon <= 0 {
		p.ScheduleHorizon = d.ScheduleHorizon
	}
	if p.GuardrailFactor <= 1 {
		p.GuardrailFactor = d.GuardrailFactor
	}
	if p.GuardrailFloorMinor < 0 {
		p.GuardrailFloorMinor = d.GuardrailFloorMinor
	}
	if p.FullRefreshEvery <= 0 {
		p.FullRefreshEvery = d.FullRefreshEvery
	}
	return p
}

// PolicySet resolves the policy for a tenant.
//
// Guard rails are per tenant because the tolerable blast radius differs by
// business: a grocer repricing 40,000 lines a night wants a tight factor and
// will accept a handful of false rejections, while a jeweller whose catalogue
// genuinely spans 20x on a single SKU family cannot use one at all. The zero
// value is usable and yields the platform defaults for every tenant.
type PolicySet struct {
	// Default applies to any tenant without an override.
	Default Policy
	// ByTenant holds per-tenant overrides.
	ByTenant map[canon.TenantID]Policy
}

// NewPolicySet builds a policy set with the platform defaults.
func NewPolicySet() *PolicySet {
	return &PolicySet{Default: DefaultPolicy(), ByTenant: map[canon.TenantID]Policy{}}
}

// Set installs an override for one tenant, filling unset fields from the
// platform defaults.
func (s *PolicySet) Set(tenant canon.TenantID, p Policy) {
	if s.ByTenant == nil {
		s.ByTenant = map[canon.TenantID]Policy{}
	}
	s.ByTenant[tenant] = p.WithDefaults()
}

// For returns the policy governing a tenant.
func (s *PolicySet) For(tenant canon.TenantID) Policy {
	if s == nil {
		return DefaultPolicy()
	}
	if p, ok := s.ByTenant[tenant]; ok {
		return p.WithDefaults()
	}
	return s.Default.WithDefaults()
}

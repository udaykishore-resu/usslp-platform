// Package ports declares the interfaces the pricing service's application layer
// depends on.
//
// There are deliberately few. The pricing engine owns its models, its feature
// store and its rules; the only things it does not own are where the Tier-1
// constraints come from (the merchandising system, via whatever the tenant has
// integrated) and what time it is.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// ErrNoConstraints is returned when a (store, SKU) has no configured rules.
//
// It is an error rather than an empty Constraints value on purpose. An empty
// Constraints permits every price, and "we have no margin floor configured for
// this product" must not be silently indistinguishable from "this product has
// no margin floor" — the first is a data gap that should surface, the second is
// a decision someone made.
var ErrNoConstraints = errors.New("pricing: no constraints configured")

// ConstraintSource resolves the Tier-1 rules for a product in a store.
type ConstraintSource interface {
	// Constraints returns the rules for one (store, SKU).
	Constraints(ctx context.Context, tenant canon.TenantID, store canon.StoreID, sku canon.SKU) (domain.Constraints, error)
}

// Clock is the injected time source.
//
// Point-in-time correctness is the whole premise of the feature store, and a
// test that cannot control "now" cannot demonstrate it. Every time this service
// reads goes through here.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock.
type SystemClock struct{}

// Now returns the current UTC instant.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock is a test clock.
type FixedClock struct{ T time.Time }

// Now returns the fixed instant.
func (f FixedClock) Now() time.Time { return f.T }

// StaticConstraints is a ConstraintSource backed by an in-memory table.
//
// It is the adapter a single-store deployment and the test suite both use, and
// it is the shape a real adapter takes: the merchandising system's rules are
// pushed into the pricing service as a table, not pulled per decision, because
// a Tier-1 evaluation has a sub-10-millisecond budget that a network round trip
// to an ERP does not fit inside.
type StaticConstraints struct {
	// Default applies to any SKU without a specific entry. Its zero value
	// permits every price, so a deployment that wants a global margin floor
	// sets it here.
	Default domain.Constraints
	// ByKey is keyed by tenant/store/sku.
	ByKey map[string]domain.Constraints
	// UseDefault controls whether a missing entry falls back to Default or
	// returns ErrNoConstraints.
	UseDefault bool
}

// ConstraintKey builds the map key.
func ConstraintKey(tenant canon.TenantID, store canon.StoreID, sku canon.SKU) string {
	return string(tenant) + "\x00" + string(store) + "\x00" + string(sku)
}

// Constraints implements ConstraintSource.
func (s *StaticConstraints) Constraints(_ context.Context, tenant canon.TenantID, store canon.StoreID, sku canon.SKU) (domain.Constraints, error) {
	if c, ok := s.ByKey[ConstraintKey(tenant, store, sku)]; ok {
		return c, nil
	}
	if s.UseDefault {
		return s.Default, nil
	}
	return domain.Constraints{}, ErrNoConstraints
}

// PolicyPackSource is the optional capability of enumerating a store's whole
// rule table, so that the Store Gateway Unit can be handed a compact policy
// pack instead of querying per SKU.
//
// It is a separate interface rather than a method on ConstraintSource because
// not every source can do it: an adapter that proxies a merchandising API can
// answer one SKU at a time but cannot list forty thousand, and forcing it to
// implement a method it would have to fail would make the failure invisible
// until a gateway asked.
type PolicyPackSource interface {
	PolicyPack(ctx context.Context, tenant canon.TenantID, store canon.StoreID) (domain.PolicyPack, error)
}

// PolicyPack implements PolicyPackSource over the static table.
func (s *StaticConstraints) PolicyPack(_ context.Context, tenant canon.TenantID, store canon.StoreID) (domain.PolicyPack, error) {
	prefix := string(tenant) + "\x00" + string(store) + "\x00"
	pack := domain.PolicyPack{Tenant: tenant, Store: store, Version: 1, Rules: map[canon.SKU]domain.Constraints{}}
	for k, c := range s.ByKey {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			pack.Rules[canon.SKU(k[len(prefix):])] = c
		}
	}
	return pack, nil
}

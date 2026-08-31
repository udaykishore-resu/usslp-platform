// Package ports declares the interfaces the promotion service's application
// layer depends on.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/usslp/usslp/platform/internal/promotion/app"
	"github.com/usslp/usslp/platform/internal/promotion/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Errors the ports define.
var (
	// ErrNotFound is returned for an unknown promotion.
	ErrNotFound = errors.New("promotion: not found")
	// ErrConcurrency is returned when an edit lost a race. The caller must
	// re-read and retry: promotions are edited by people, and two people
	// editing the same one is a merge conflict, not a last-write-wins.
	ErrConcurrency = errors.New("promotion: version conflict")
)

// Catalogue is the product data the promotion service evaluates against.
//
// It is an interface because the data belongs to the merchandising system and
// arrives by a route each tenant chooses — a nightly extract, the UIG's
// inventory stream, or a live API. The service needs to iterate it, not to own
// it.
type Catalogue interface {
	// Products returns the (store, SKU) population for a tenant, optionally
	// narrowed to a set of stores.
	Products(ctx context.Context, tenant canon.TenantID, stores []canon.StoreID) ([]domain.Product, error)
}

// StoreDirectory resolves a store's time zone and cluster membership.
//
// The zone is what makes "starts Monday" mean local Monday, so it is a hard
// dependency of activation rather than a decoration: a store whose zone the
// platform does not know cannot have a wall-clock promotion activated
// correctly, and the service says so rather than guessing UTC.
type StoreDirectory interface {
	// Zone returns a store's IANA location name.
	Zone(ctx context.Context, tenant canon.TenantID, store canon.StoreID) (string, error)
	// Zones returns the locations of every store a tenant has.
	Zones(ctx context.Context, tenant canon.TenantID) (domain.StoreZones, error)
	// Cluster returns a store's cluster name, for lift reporting. An empty
	// string is legitimate and means "unclustered".
	Cluster(ctx context.Context, tenant canon.TenantID, store canon.StoreID) (string, error)
}

// SalesSource supplies the daily trading history a lift measurement needs. In
// production it is the analytics service; in a test it is a slice.
type SalesSource interface {
	Sales(ctx context.Context, tenant canon.TenantID, promotion canon.PromotionID,
		from, to time.Time) (test []app.SalesPoint, control []app.SalesPoint, controlStores int, err error)
}

// Clock is the injected time source, so that activation tests can control
// "now" without sleeping.
type Clock interface{ Now() time.Time }

// SystemClock is the production clock.
type SystemClock struct{}

// Now returns the current UTC instant.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock is a test clock.
type FixedClock struct{ T time.Time }

// Now returns the fixed instant.
func (f FixedClock) Now() time.Time { return f.T }

// StaticDirectory is an in-memory StoreDirectory.
type StaticDirectory struct {
	// ZoneOf maps a store to an IANA location.
	ZoneOf map[canon.StoreID]string
	// ClusterOf maps a store to a cluster name.
	ClusterOf map[canon.StoreID]string
	// DefaultZone applies to a store with no entry. It is empty by default,
	// which resolves to UTC — a deliberate, visible fallback rather than the
	// process's local zone.
	DefaultZone string
}

// Zone implements StoreDirectory.
func (d *StaticDirectory) Zone(_ context.Context, _ canon.TenantID, store canon.StoreID) (string, error) {
	if z, ok := d.ZoneOf[store]; ok {
		return z, nil
	}
	return d.DefaultZone, nil
}

// Zones implements StoreDirectory.
func (d *StaticDirectory) Zones(_ context.Context, _ canon.TenantID) (domain.StoreZones, error) {
	out := make(domain.StoreZones, len(d.ZoneOf))
	for store, zone := range d.ZoneOf {
		out[string(store)] = zone
	}
	return out, nil
}

// Cluster implements StoreDirectory.
func (d *StaticDirectory) Cluster(_ context.Context, _ canon.TenantID, store canon.StoreID) (string, error) {
	return d.ClusterOf[store], nil
}

// StaticCatalogue is an in-memory Catalogue.
type StaticCatalogue struct {
	// ByTenant holds the product population per tenant.
	ByTenant map[canon.TenantID][]domain.Product
}

// Products implements Catalogue.
func (c *StaticCatalogue) Products(_ context.Context, tenant canon.TenantID, stores []canon.StoreID) ([]domain.Product, error) {
	all := c.ByTenant[tenant]
	if len(stores) == 0 {
		return all, nil
	}
	want := make(map[canon.StoreID]struct{}, len(stores))
	for _, s := range stores {
		want[s] = struct{}{}
	}
	out := make([]domain.Product, 0, len(all))
	for _, p := range all {
		if _, ok := want[p.StoreID]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// StaticSales is an in-memory SalesSource.
type StaticSales struct {
	Test          []app.SalesPoint
	Control       []app.SalesPoint
	ControlStores int
}

// Sales implements SalesSource.
func (s *StaticSales) Sales(_ context.Context, _ canon.TenantID, _ canon.PromotionID,
	from, to time.Time) ([]app.SalesPoint, []app.SalesPoint, int, error) {
	filter := func(in []app.SalesPoint) []app.SalesPoint {
		out := make([]app.SalesPoint, 0, len(in))
		for _, p := range in {
			if p.Day.Before(from) || p.Day.After(to) {
				continue
			}
			out = append(out, p)
		}
		return out
	}
	return filter(s.Test), filter(s.Control), s.ControlStores, nil
}

package apigw

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Permissions
//
// A permission is a (resource, action) pair and nothing else. Resources are
// coarse — the nine nouns the public API exposes — because a permission model
// finer than the API it guards is a model nobody can reason about, and the
// place to express "this key may only touch store 42" is the principal's store
// scope, not a combinatorial explosion of permission names.
// ---------------------------------------------------------------------------

// Resource is the noun a permission applies to.
type Resource string

// The resources the public API exposes.
const (
	// ResPrices is price mutation: single updates and batches.
	ResPrices Resource = "prices"
	// ResLabels is per-label state and history.
	ResLabels Resource = "labels"
	// ResStores is store rosters, health, mesh and planograms.
	ResStores Resource = "stores"
	// ResPromotions is the promotion catalogue and activation.
	ResPromotions Resource = "promotions"
	// ResPricing is the rules engine and its simulator.
	ResPricing Resource = "pricing"
	// ResAnalytics is the query and reporting surface.
	ResAnalytics Resource = "analytics"
	// ResOTA is firmware rollout control.
	ResOTA Resource = "ota"
	// ResDevices is the device fleet: provisioning, retirement, quarantine.
	ResDevices Resource = "devices"
	// ResKeys is API key issuance and revocation. It is deliberately its own
	// resource: the ability to mint credentials is the ability to escalate to
	// anything, so it is never folded into a general "admin" grant.
	ResKeys Resource = "keys"
	// ResStream is the live event feed.
	ResStream Resource = "stream"
	// ResSelf is the caller's own identity. Every authenticated principal has
	// it; it exists as a resource so that /v1/me goes through exactly the same
	// authorisation path as everything else rather than being a special case.
	ResSelf Resource = "self"
	// ResPOS is Universal Integration Gateway state: bindings and deliveries.
	ResPOS Resource = "pos"
)

// Action is the verb a permission applies to.
type Action string

// The three actions. There is no "delete": in an event-sourced platform
// nothing is deleted, it is retired, revoked or expired, and each of those is
// a write.
const (
	// ActRead observes state.
	ActRead Action = "read"
	// ActWrite changes state.
	ActWrite Action = "write"
	// ActAdmin performs operations whose blast radius is the whole tenant:
	// issuing credentials, aborting a fleet-wide rollout, uploading a
	// planogram that repositions every label in a store.
	ActAdmin Action = "admin"
)

// Permission is a resource/action pair.
type Permission struct {
	Resource Resource
	Action   Action
}

// String renders a permission as "resource:action".
func (p Permission) String() string { return string(p.Resource) + ":" + string(p.Action) }

// Zero reports whether the permission is unset, which marks a route as
// requiring authentication but no particular grant.
func (p Permission) Zero() bool { return p.Resource == "" && p.Action == "" }

// Read, Write and Admin build permissions readably at route-table call sites.
func Read(r Resource) Permission  { return Permission{r, ActRead} }
func Write(r Resource) Permission { return Permission{r, ActWrite} }
func Admin(r Resource) Permission { return Permission{r, ActAdmin} }

// ---------------------------------------------------------------------------
// Roles
// ---------------------------------------------------------------------------

// Role is a named bundle of permissions.
type Role string

// The six roles USSLP ships. They are the job titles that actually exist in a
// retail organisation, which is what makes them assignable without a training
// course; anything more specific belongs in a store scope.
const (
	// RoleOwner is the tenant administrator: everything, including minting
	// credentials for everyone else.
	RoleOwner Role = "owner"
	// RoleStoreManager runs one or more stores. Full operational control
	// inside their store scope, read-only outside their own remit, and no
	// ability to touch firmware or credentials.
	RoleStoreManager Role = "store-manager"
	// RolePricingAnalyst sets prices and promotions across the estate but has
	// no authority over hardware.
	RolePricingAnalyst Role = "pricing-analyst"
	// RoleFieldTechnician installs, retires and updates hardware, and can see
	// enough of a store to do it. It cannot change a price: a technician
	// holding a ladder is not the person who should be able to reprice a
	// shelf, and separating those two is the whole reason the role exists.
	RoleFieldTechnician Role = "field-technician"
	// RoleReadOnly observes. Auditors, dashboards, support.
	RoleReadOnly Role = "read-only"
	// RoleIntegration is the machine role: a retailer's own systems pushing
	// prices and consuming the event stream. It has no administrative
	// capability at all, so a leaked integration credential cannot mint
	// another one.
	RoleIntegration Role = "integration"
)

// rolePermissions is the authorisation matrix. It is a package-level table
// rather than a database because these six roles are a product decision, not a
// tenant configuration, and a matrix that can drift per tenant is a matrix no
// one can audit.
var rolePermissions = map[Role]map[Permission]bool{
	RoleOwner: permSet(
		Read(ResPrices), Write(ResPrices),
		Read(ResLabels), Write(ResLabels),
		Read(ResStores), Write(ResStores), Admin(ResStores),
		Read(ResPromotions), Write(ResPromotions),
		Read(ResPricing), Write(ResPricing),
		Read(ResAnalytics), Write(ResAnalytics),
		Read(ResOTA), Write(ResOTA), Admin(ResOTA),
		Read(ResDevices), Write(ResDevices), Admin(ResDevices),
		Read(ResKeys), Write(ResKeys), Admin(ResKeys),
		Read(ResStream), Read(ResSelf), Read(ResPOS),
	),
	RoleStoreManager: permSet(
		Read(ResPrices), Write(ResPrices),
		Read(ResLabels), Write(ResLabels),
		Read(ResStores), Write(ResStores),
		Read(ResPromotions), Write(ResPromotions),
		Read(ResPricing),
		Read(ResAnalytics),
		Read(ResOTA),
		Read(ResDevices),
		Read(ResStream), Read(ResSelf), Read(ResPOS),
	),
	RolePricingAnalyst: permSet(
		Read(ResPrices), Write(ResPrices),
		Read(ResLabels),
		Read(ResStores),
		Read(ResPromotions), Write(ResPromotions),
		Read(ResPricing), Write(ResPricing),
		Read(ResAnalytics), Write(ResAnalytics),
		Read(ResStream), Read(ResSelf),
	),
	RoleFieldTechnician: permSet(
		Read(ResLabels),
		Read(ResStores),
		Read(ResOTA), Write(ResOTA),
		Read(ResDevices), Write(ResDevices),
		Read(ResStream), Read(ResSelf),
	),
	RoleReadOnly: permSet(
		Read(ResPrices), Read(ResLabels), Read(ResStores),
		Read(ResPromotions), Read(ResPricing), Read(ResAnalytics),
		Read(ResOTA), Read(ResDevices),
		Read(ResStream), Read(ResSelf), Read(ResPOS),
	),
	RoleIntegration: permSet(
		Read(ResPrices), Write(ResPrices),
		Read(ResLabels), Write(ResLabels),
		Read(ResStores),
		Read(ResPromotions), Write(ResPromotions),
		Read(ResPricing),
		Read(ResAnalytics),
		Read(ResDevices),
		Read(ResStream), Read(ResSelf), Read(ResPOS),
	),
}

func permSet(perms ...Permission) map[Permission]bool {
	m := make(map[Permission]bool, len(perms))
	for _, p := range perms {
		m[p] = true
	}
	return m
}

// Grants reports whether the role carries a permission.
//
// An admin grant implies write and write implies read, so the matrix above
// only has to state the highest action per resource it actually intends —
// except that it states them anyway, because an implication that is only in
// code is an implication nobody reviewing the table can see.
func (r Role) Grants(perm Permission) bool {
	set, ok := rolePermissions[r]
	if !ok {
		return false
	}
	if set[perm] {
		return true
	}
	switch perm.Action {
	case ActRead:
		return set[Permission{perm.Resource, ActWrite}] || set[Permission{perm.Resource, ActAdmin}]
	case ActWrite:
		return set[Permission{perm.Resource, ActAdmin}]
	}
	return false
}

// Valid reports whether the role is one the platform defines. Unknown roles
// are rejected at credential-issuance time rather than silently granting
// nothing, so a typo in an automation script fails loudly instead of producing
// a credential that mysteriously cannot do anything.
func (r Role) Valid() bool {
	_, ok := rolePermissions[r]
	return ok
}

// AllRoles returns the defined roles in a stable order.
func AllRoles() []Role {
	out := make([]Role, 0, len(rolePermissions))
	for r := range rolePermissions {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Permissions returns the role's grants in a stable order, for /v1/me and for
// the console's "what can I do here" affordances.
func (r Role) Permissions() []string {
	set := rolePermissions[r]
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p.String())
	}
	sort.Strings(out)
	return out
}

// ParseRoles validates and de-duplicates a list of role names.
func ParseRoles(names []string) ([]Role, error) {
	seen := make(map[Role]bool, len(names))
	out := make([]Role, 0, len(names))
	for _, n := range names {
		r := Role(strings.TrimSpace(n))
		if !r.Valid() {
			return nil, fmt.Errorf("unknown role %q (valid: %s)", n, joinRoles(AllRoles()))
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out, nil
}

func joinRoles(rs []Role) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = string(r)
	}
	return strings.Join(parts, ", ")
}

package apigw

import (
	"net/http"
	"testing"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestRoleMatrix is the authorisation contract, written out.
//
// It is a table rather than a loop over rolePermissions because a test derived
// from the implementation proves only that the implementation is
// self-consistent. These are the answers a security review agreed to, and the
// separations that matter are visible in it: a field technician cannot change
// a price, a pricing analyst cannot touch hardware, and only an owner can mint
// a credential.
func TestRoleMatrix(t *testing.T) {
	t.Parallel()

	resources := []Resource{
		ResPrices, ResLabels, ResStores, ResPromotions, ResPricing,
		ResAnalytics, ResOTA, ResDevices, ResKeys, ResStream, ResSelf, ResPOS,
	}
	actions := []Action{ActRead, ActWrite, ActAdmin}

	// "rwa" per resource, in the order of `resources` above.
	expected := map[Role][]string{
		RoleOwner: {
			"rw-", "rw-", "rwa", "rw-", "rw-",
			"rw-", "rwa", "rwa", "rwa", "r--", "r--", "r--",
		},
		RoleStoreManager: {
			"rw-", "rw-", "rw-", "rw-", "r--",
			"r--", "r--", "r--", "---", "r--", "r--", "r--",
		},
		RolePricingAnalyst: {
			"rw-", "r--", "r--", "rw-", "rw-",
			"rw-", "---", "---", "---", "r--", "r--", "---",
		},
		RoleFieldTechnician: {
			"---", "r--", "r--", "---", "---",
			"---", "rw-", "rw-", "---", "r--", "r--", "---",
		},
		RoleReadOnly: {
			"r--", "r--", "r--", "r--", "r--",
			"r--", "r--", "r--", "---", "r--", "r--", "r--",
		},
		RoleIntegration: {
			"rw-", "rw-", "r--", "rw-", "r--",
			"r--", "---", "r--", "---", "r--", "r--", "r--",
		},
	}

	if len(expected) != len(AllRoles()) {
		t.Fatalf("the matrix covers %d roles but the platform defines %d", len(expected), len(AllRoles()))
	}
	for role, rows := range expected {
		if len(rows) != len(resources) {
			t.Fatalf("%s: matrix row covers %d resources, want %d", role, len(rows), len(resources))
		}
		for i, res := range resources {
			for j, act := range actions {
				want := rows[i][j] != '-'
				got := role.Grants(Permission{res, act})
				if got != want {
					t.Errorf("%s on %s:%s = %v, want %v", role, res, act, got, want)
				}
			}
		}
	}
}

func TestUnknownRolesGrantNothing(t *testing.T) {
	t.Parallel()
	ghost := Role("super-admin")
	if ghost.Valid() {
		t.Fatal("an undefined role reports itself as valid")
	}
	if ghost.Grants(Read(ResSelf)) {
		t.Fatal("an undefined role granted a permission")
	}
	if _, err := ParseRoles([]string{"owner", "super-admin"}); err == nil {
		t.Fatal("ParseRoles accepted an undefined role; a typo in an automation script must fail loudly")
	}
}

func TestAdminImpliesWriteImpliesRead(t *testing.T) {
	t.Parallel()
	// The matrix states the implications explicitly, but the implication
	// itself must hold: a role granted admin on a resource can read it.
	if !RoleOwner.Grants(Read(ResKeys)) || !RoleOwner.Grants(Write(ResKeys)) {
		t.Fatal("keys:admin does not imply keys:write and keys:read")
	}
}

// TestRoutePermissionsAreEnforced walks the real route table with one
// credential per role and checks that every route is either reachable or 403,
// exactly as the matrix says. It is the proof that authorisation lives in the
// middleware rather than in individual handlers: no handler is consulted.
func TestRoutePermissionsAreEnforced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	for _, role := range AllRoles() {
		role := role
		key := h.issueKey("acme", []Role{role})
		for _, rt := range Routes() {
			if rt.Public || rt.Streaming {
				continue
			}
			// Only routes whose permission the role lacks are asserted on:
			// a permitted route's status depends on the stub upstream, which
			// this test is not about.
			if role.Grants(rt.Permission) {
				continue
			}
			path := concretePath(rt.Pattern)
			res := h.do(rt.Method, path, key, map[string]any{"probe": true})
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("%s as %s: got %d, want 403 (it does not grant %s)",
					rt.Key(), role, res.StatusCode, rt.Permission)
			}
		}
	}
}

func TestStoreScopedCredentialCannotReachAnotherStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	scoped := h.issueKey("acme", []Role{RoleStoreManager}, "store-1")

	if got := h.do(http.MethodGet, "/v1/stores/store-1/labels", scoped, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("own store: got %d, want 200", got)
	}
	res := h.do(http.MethodGet, "/v1/stores/store-2/labels", scoped, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("out-of-scope store: got %d, want 404 — a 403 would let a manager "+
			"enumerate the estate", res.StatusCode)
	}
	// The upstream must never have been asked about store-2.
	for _, call := range h.stubs[UpstreamLabel].calls() {
		if call.Path == "/v1/stores/store-2/labels" {
			t.Fatal("the out-of-scope request reached the upstream")
		}
	}
}

func TestStoreScopeIsEnforcedOnEveryStoreRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	scoped := h.issueKey("acme", []Role{RoleOwner}, "store-1")

	checked := 0
	for _, rt := range Routes() {
		if rt.StorePathValue == "" {
			continue
		}
		checked++
		path := substitute(rt.Pattern, map[string]string{rt.StorePathValue: "store-9"})
		path = concretePath(path)
		res := h.do(rt.Method, path, scoped, map[string]any{"probe": true})
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: got %d for an out-of-scope store, want 404", rt.Key(), res.StatusCode)
		}
	}
	if checked == 0 {
		t.Fatal("no store-scoped routes were exercised; the route table lost its StorePathValue markers")
	}
}

func TestReservedCharactersInAStoreIDAreRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	key := h.issueKey("acme", []Role{RoleOwner})
	// canon.ValidID forbids the separators the MQTT and Kafka namespaces use;
	// a store id carrying one would let a caller address outside its namespace.
	res := h.do(http.MethodGet, "/v1/stores/store%3A1/labels", key, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d for a store id containing a reserved character, want 400", res.StatusCode)
	}
}

func TestIssuingAKeyCannotEscalatePrivilege(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A store manager cannot mint anything at all: it has no keys permission.
	manager := h.issueKey("acme", []Role{RoleStoreManager})
	res := h.do(http.MethodPost, "/v1/keys", manager, IssueKeyRequest{
		Name: "escalation", Roles: []string{string(RoleOwner)},
	})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("store-manager minting an owner key: got %d, want 403", res.StatusCode)
	}

	// An owner scoped to one store may not mint a key for another one.
	scopedOwner := h.issueKey("acme", []Role{RoleOwner}, "store-1")
	res = h.do(http.MethodPost, "/v1/keys", scopedOwner, IssueKeyRequest{
		Name: "wider", Roles: []string{string(RoleStoreManager)},
		Stores: []canon.StoreID{"store-2"},
	})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("scoped owner widening a store scope: got %d, want 403", res.StatusCode)
	}

	// And a key minted without an explicit scope inherits the issuer's,
	// rather than silently becoming unscoped.
	res = h.do(http.MethodPost, "/v1/keys", scopedOwner, IssueKeyRequest{
		Name: "inherited", Roles: []string{string(RoleStoreManager)},
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("issuing a key: got %d (%s)", res.StatusCode, bodyString(t, res))
	}
	var issued IssueKeyResponse
	decodeBody(t, res, &issued)
	if len(issued.Record.Stores) != 1 || issued.Record.Stores[0] != "store-1" {
		t.Fatalf("issued key scope %v, want the issuer's [store-1]", issued.Record.Stores)
	}

	// The new key really is limited.
	if got := h.do(http.MethodGet, "/v1/stores/store-2/labels", issued.Key, nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("the inherited scope is not enforced: got %d, want 404", got)
	}
}

func TestIssuedKeyRoundTripsThroughTheAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	owner := h.issueKey("acme", []Role{RoleOwner})

	res := h.do(http.MethodPost, "/v1/keys", owner, IssueKeyRequest{
		Name: "nightly-import", Roles: []string{string(RoleIntegration)},
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("issue: got %d (%s)", res.StatusCode, bodyString(t, res))
	}
	var issued IssueKeyResponse
	decodeBody(t, res, &issued)
	if issued.Key == "" {
		t.Fatal("no key material was returned")
	}

	res = h.do(http.MethodGet, "/v1/me", issued.Key, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the issued key does not authenticate: %d", res.StatusCode)
	}

	// The listing must never carry key material.
	res = h.do(http.MethodGet, "/v1/keys", owner, nil)
	if body := bodyString(t, res); containsSecret(body, issued.Key) {
		t.Fatal("the key listing echoed key material")
	}

	res = h.do(http.MethodDelete, "/v1/keys/"+issued.Record.KeyID, owner, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: got %d, want 204", res.StatusCode)
	}
	if got := h.do(http.MethodGet, "/v1/me", issued.Key, nil).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("a revoked key still authenticates: %d", got)
	}
	// Revoking a key that belongs to nobody visible is a 404, not a 403.
	if got := h.do(http.MethodDelete, "/v1/keys/00000000deadbeef", owner, nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("revoking an unknown key: got %d, want 404", got)
	}
}

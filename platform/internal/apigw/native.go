package apigw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ---------------------------------------------------------------------------
// Endpoints the gateway answers itself
//
// Two kinds live here. Identity endpoints (/v1/me, /v1/keys) are about the
// credential and have no upstream to ask. Composed endpoints (/v1/prices,
// /v1/stores/{id}/overview) exist because the shape a caller wants and the
// shape the internal services expose are genuinely different, and the
// alternative to composing here is every client composing it for themselves.
//
// Composition goes through [Proxy.Do] like any other route, so a composed call
// is subject to the same timeouts, retries, breaker and size limits as a
// pass-through one. A composed endpoint that dialled an upstream directly
// would be the one caller that keeps hammering a service the breaker has
// already given up on.
// ---------------------------------------------------------------------------

// MeResponse describes the calling credential.
type MeResponse struct {
	// TenantID is the isolation boundary every call this credential makes is
	// confined to.
	TenantID canon.TenantID `json:"tenant_id"`
	// Subject and CredentialID identify the caller and the specific credential.
	Subject      string `json:"subject"`
	CredentialID string `json:"credential_id,omitempty"`
	// AuthMethod is api_key, jwt or mtls.
	AuthMethod CredentialKind `json:"auth_method"`
	// Roles and Permissions are what this credential may do. Permissions are
	// expanded from the roles rather than left for the client to derive: a
	// console that hides buttons the caller cannot use should not have to ship
	// its own copy of the authorisation matrix.
	Roles       []Role   `json:"roles"`
	Permissions []string `json:"permissions"`
	// Stores is the store scope, absent when unrestricted within the tenant.
	Stores []canon.StoreID `json:"stores,omitempty"`
	// Scopes are the credential's free-form labels.
	Scopes []string `json:"scopes,omitempty"`
	// ExpiresAt is when this credential stops working.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (g *Gateway) handleMe(w http.ResponseWriter, r *http.Request) {
	p, err := principalOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	seen := map[string]bool{}
	perms := make([]string, 0, 16)
	for _, role := range p.Roles {
		for _, perm := range role.Permissions() {
			if !seen[perm] {
				seen[perm] = true
				perms = append(perms, perm)
			}
		}
	}
	sortStrings(perms)
	resp := MeResponse{
		TenantID: p.TenantID, Subject: p.Subject, CredentialID: p.CredentialID,
		AuthMethod: p.Kind, Roles: p.Roles, Permissions: perms,
		Stores: p.Stores, Scopes: p.Scopes,
	}
	if !p.ExpiresAt.IsZero() {
		exp := p.ExpiresAt.UTC()
		resp.ExpiresAt = &exp
	}
	writeJSON(w, http.StatusOK, resp)
}

// IssueKeyRequest is the body of POST /v1/keys.
type IssueKeyRequest struct {
	// Name is the human label. Required, because a key store full of keys
	// called "key" cannot be audited.
	Name string `json:"name"`
	// Roles are the roles to grant. A caller may not grant a role it does not
	// itself hold.
	Roles []string `json:"roles"`
	// Stores restricts the new key; it must be within the caller's own scope.
	Stores []canon.StoreID `json:"stores,omitempty"`
	// Scopes are free-form tenant labels.
	Scopes []string `json:"scopes,omitempty"`
	// TTLSeconds is the requested lifetime.
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// IssueKeyResponse is the one and only time the key material is visible.
type IssueKeyResponse struct {
	// Key is the plaintext credential. It is not stored and cannot be
	// recovered.
	Key string `json:"key"`
	// Record is the stored metadata.
	Record APIKey `json:"record"`
	// Warning states plainly that this value will not be shown again.
	Warning string `json:"warning"`
}

func (g *Gateway) handleIssueKey(w http.ResponseWriter, r *http.Request) {
	p, err := principalOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if g.keys == nil {
		writeError(w, r, errUpstream(http.StatusNotImplemented, "unsupported",
			"this deployment does not issue API keys"))
		return
	}
	var req IssueKeyRequest
	if err := decodeJSONBody(r, &req, 64<<10); err != nil {
		writeError(w, r, err)
		return
	}
	roles, perr := ParseRoles(req.Roles)
	if perr != nil {
		writeError(w, r, errBadRequest("%v", perr))
		return
	}
	// Privilege escalation is the failure mode that matters here: without this
	// check a store-manager could mint an owner key and become one. A caller
	// may only delegate what it already holds.
	for _, role := range roles {
		for _, perm := range role.Permissions() {
			res, act, _ := strings.Cut(perm, ":")
			if !p.Can(Permission{Resource(res), Action(act)}) {
				writeError(w, r, errForbidden(
					"cannot issue a key with role %q: it grants %s, which this credential does not hold",
					role, perm))
				return
			}
		}
	}
	// A store scope may only be narrowed, never widened.
	for _, s := range req.Stores {
		if !p.AllowsStore(s) {
			writeError(w, r, errForbidden("cannot issue a key scoped to store %s: it is outside this credential's scope", s))
			return
		}
	}
	stores := req.Stores
	if len(stores) == 0 && len(p.Stores) > 0 {
		// An unscoped request from a scoped issuer inherits the issuer's
		// scope. Silently producing an unscoped key would be an escalation
		// dressed as a default.
		stores = append([]canon.StoreID(nil), p.Stores...)
	}

	rec, plaintext, err := g.keys.Issue(r.Context(), IssueRequest{
		TenantID: p.TenantID, Name: req.Name, Roles: roles, Stores: stores,
		Scopes: req.Scopes, TTL: time.Duration(req.TTLSeconds) * time.Second,
		CreatedBy: p.Subject,
	})
	if err != nil {
		writeError(w, r, errBadRequest("%v", err))
		return
	}
	g.log.FromContext(r.Context()).Info("api key issued",
		"tenant_id", string(p.TenantID), "key_id", rec.KeyID, "name", rec.Name,
		"roles", req.Roles, "issued_by", p.Subject, "expires_at", rec.ExpiresAt)
	writeJSON(w, http.StatusCreated, IssueKeyResponse{
		Key: plaintext, Record: rec,
		Warning: "store this value now; it is hashed at rest and cannot be shown again",
	})
}

// ListKeysResponse is the tenant's credential inventory.
type ListKeysResponse struct {
	Keys []APIKey `json:"keys"`
}

func (g *Gateway) handleListKeys(w http.ResponseWriter, r *http.Request) {
	p, err := principalOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if g.keys == nil {
		writeJSON(w, http.StatusOK, ListKeysResponse{Keys: []APIKey{}})
		return
	}
	// Scoped by tenant at the store, not filtered afterwards: a listing that
	// fetched everything and then filtered is one refactor away from being a
	// listing that forgets to.
	recs, err := g.keys.Store().List(r.Context(), p.TenantID)
	if err != nil {
		writeError(w, r, errInternal("listing keys: %v", err))
		return
	}
	if recs == nil {
		recs = []APIKey{}
	}
	writeJSON(w, http.StatusOK, ListKeysResponse{Keys: recs})
}

func (g *Gateway) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	p, err := principalOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if g.keys == nil {
		writeError(w, r, errNotFound("no such key"))
		return
	}
	keyID := r.PathValue("keyId")
	if err := g.keys.Store().Revoke(r.Context(), p.TenantID, keyID, g.now()); err != nil {
		if errors.Is(err, ErrKeyUnknown) {
			writeError(w, r, errNotFound("no such key"))
			return
		}
		writeError(w, r, errInternal("revoking key: %v", err))
		return
	}
	g.log.FromContext(r.Context()).Info("api key revoked",
		"tenant_id", string(p.TenantID), "key_id", keyID, "revoked_by", p.Subject)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Composed: POST /v1/prices
// ---------------------------------------------------------------------------

// PriceChangeRequest is the single-price form of a change.
//
// It exists because the natural thing a caller wants to say — "this SKU in
// this store now costs this" — is a one-item batch to label-service, and
// making every integration construct a batch envelope for one price is the
// kind of friction that ends with people writing their own wrappers and
// getting the retry semantics wrong.
type PriceChangeRequest struct {
	StoreID     canon.StoreID     `json:"store_id"`
	SKU         canon.SKU         `json:"sku"`
	LabelID     canon.LabelID     `json:"label_id,omitempty"`
	Price       canon.Money       `json:"price"`
	WasPrice    *canon.Money      `json:"was_price,omitempty"`
	UnitPrice   *canon.Money      `json:"unit_price,omitempty"`
	UnitMeasure string            `json:"unit_measure,omitempty"`
	EffectiveAt *time.Time        `json:"effective_at,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	PromotionID canon.PromotionID `json:"promotion_id,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	// IdempotencyKey makes a retry a no-op all the way down to the event
	// store.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// batchEnvelope and batchItem mirror label-service's wire contract.
//
// They are declared here rather than imported from that service's Go packages
// on purpose. A gateway that shares structs with the service behind it cannot
// be deployed independently of it: a field added upstream becomes a
// compile-time coupling, and the gateway's job is to be a stable public
// contract in front of services that change. What is shared is the JSON, and
// the agreement is tested at the seam, not asserted by the type system.
type batchEnvelope struct {
	Items       []batchItem `json:"items"`
	InitiatedBy string      `json:"initiated_by,omitempty"`
}

type batchItem struct {
	StoreID        canon.StoreID     `json:"store_id"`
	SKU            canon.SKU         `json:"sku"`
	LabelID        canon.LabelID     `json:"label_id,omitempty"`
	Price          canon.Money       `json:"price"`
	WasPrice       *canon.Money      `json:"was_price,omitempty"`
	UnitPrice      *canon.Money      `json:"unit_price,omitempty"`
	UnitMeasure    string            `json:"unit_measure,omitempty"`
	EffectiveAt    time.Time         `json:"effective_at"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	PromotionID    canon.PromotionID `json:"promotion_id,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	Attributes     map[string]string `json:"attributes,omitempty"`
	InitiatedBy    string            `json:"initiated_by,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

func (g *Gateway) handleUpdatePrice(w http.ResponseWriter, r *http.Request) {
	p, err := principalOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	route, _ := RouteFrom(r.Context())
	up, ok := g.proxy.Upstream(UpstreamLabel)
	if !ok {
		writeError(w, r, errInternal("label-service is not configured"))
		return
	}

	var req PriceChangeRequest
	if err := decodeJSONBody(r, &req, 256<<10); err != nil {
		writeError(w, r, err)
		return
	}
	switch {
	case req.StoreID == "" || !canon.ValidID(string(req.StoreID)):
		writeError(w, r, errBadRequest("store_id is required and must not contain reserved characters"))
		return
	case req.SKU == "" && req.LabelID == "":
		writeError(w, r, errBadRequest("one of sku or label_id is required"))
		return
	case !req.Price.Valid():
		writeError(w, r, errBadRequest("price currency %q is not an ISO 4217 code", req.Price.Currency))
		return
	}
	if !p.AllowsStore(req.StoreID) {
		writeError(w, r, errNotFound("store %s not found", req.StoreID))
		return
	}

	effective := g.now().UTC()
	if req.EffectiveAt != nil {
		effective = *req.EffectiveAt
	}
	body, err := json.Marshal(batchEnvelope{
		InitiatedBy: p.Subject,
		Items: []batchItem{{
			StoreID: req.StoreID, SKU: req.SKU, LabelID: req.LabelID,
			Price: req.Price, WasPrice: req.WasPrice, UnitPrice: req.UnitPrice,
			UnitMeasure: req.UnitMeasure, EffectiveAt: effective, ExpiresAt: req.ExpiresAt,
			PromotionID: req.PromotionID, Reason: req.Reason, Attributes: req.Attributes,
			InitiatedBy: p.Subject, IdempotencyKey: req.IdempotencyKey,
		}},
	})
	if err != nil {
		writeError(w, r, errInternal("encoding the upstream request: %v", err))
		return
	}

	header := forwardableHeader(r)
	header.Set("Content-Type", "application/json")
	res, err := g.proxy.Do(r.Context(), up, route, http.MethodPost, "/v1/prices:batch", "",
		header, body, p, RequestIDFrom(r.Context()))
	if err != nil {
		writeError(w, r, err)
		return
	}
	// The upstream's report is passed through unchanged. Re-shaping a
	// multi-status batch report into a single-item response would have to
	// invent a story for the partial case, and "one of the one items failed"
	// is a story the report already tells accurately.
	writeProxyResult(w, route, up, res)
}

// ---------------------------------------------------------------------------
// Composed: GET /v1/stores/{storeId}/overview
// ---------------------------------------------------------------------------

// StoreOverview is everything the console's header needs, in one call.
//
// The console would otherwise open four connections and paint in four stages,
// which on a store network is four chances to look broken. Fanning out here
// costs the gateway one goroutine per section and gives the browser one round
// trip.
type StoreOverview struct {
	StoreID canon.StoreID `json:"store_id"`
	// Health, Mesh, SLO and OTA are the upstream documents verbatim. They are
	// json.RawMessage rather than parsed structs so that a field added to the
	// device registry reaches the console without a gateway release.
	Health json.RawMessage `json:"health,omitempty"`
	Mesh   json.RawMessage `json:"mesh,omitempty"`
	SLO    json.RawMessage `json:"slo,omitempty"`
	OTA    json.RawMessage `json:"ota,omitempty"`
	// Degraded names the sections that could not be fetched. The overview
	// returns 200 with the sections it has: a console that shows mesh health
	// and says "SLO unavailable" is far more use during an incident than one
	// that shows a 502, and the incident is exactly when a dependency is down.
	Degraded []string `json:"degraded,omitempty"`
	// FetchedAt is when the fan-out completed.
	FetchedAt time.Time `json:"fetched_at"`
}

func (g *Gateway) handleStoreOverview(w http.ResponseWriter, r *http.Request) {
	p, err := principalOf(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	route, _ := RouteFrom(r.Context())
	store := r.PathValue("storeId")

	type section struct {
		name     string
		upstream string
		path     string
		query    string
		dst      *json.RawMessage
	}
	out := StoreOverview{StoreID: canon.StoreID(store)}
	sections := []section{
		{"health", UpstreamRegistry, "/v1/stores/" + store + "/health", "", &out.Health},
		{"mesh", UpstreamRegistry, "/v1/stores/" + store + "/mesh", "", &out.Mesh},
		{"slo", UpstreamLabel, "/v1/stores/" + store + "/slo", "", &out.SLO},
		{"ota", UpstreamOTA, "/v1/ota/jobs", "store_id=" + store, &out.OTA},
	}

	header := forwardableHeader(r)
	requestID := RequestIDFrom(r.Context())
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, s := range sections {
		up, ok := g.proxy.Upstream(s.upstream)
		if !ok {
			out.Degraded = append(out.Degraded, s.name)
			continue
		}
		wg.Add(1)
		go func(s section, up *Upstream) {
			defer wg.Done()
			res, err := g.proxy.Do(r.Context(), up, route, http.MethodGet, s.path, s.query,
				header.Clone(), nil, p, requestID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || res.status != http.StatusOK || !json.Valid(res.body) {
				out.Degraded = append(out.Degraded, s.name)
				return
			}
			*s.dst = json.RawMessage(res.body)
		}(s, up)
	}
	wg.Wait()
	sortStrings(out.Degraded)
	out.FetchedAt = g.now().UTC()
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// healthResponse is the body of /healthz and /readyz.
type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
	Build  map[string]string `json:"build,omitempty"`
}

// handleHealth answers liveness: the process is scheduling goroutines.
//
// Per §7 of the interface contracts, it registers no dependency checks. A
// broker blip must remove a pod from the load balancer, never restart it.
func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status: "alive",
		Build:  map[string]string{"service": g.service, "version": g.version},
	})
}

// handleReady answers readiness: start-up finished and every dependency check
// passes.
func (g *Gateway) handleReady(w http.ResponseWriter, r *http.Request) {
	ok, results := g.health.Status(r.Context())
	status, text := http.StatusOK, "ready"
	if !ok {
		status, text = http.StatusServiceUnavailable, "not ready"
	}
	writeJSON(w, status, healthResponse{Status: text, Checks: results})
}

// upstreamReadiness builds the readiness check for one upstream.
//
// It reports the circuit breaker's state rather than making a probe call.
// Probing on every readiness poll would add a request per upstream per second
// per replica to services that are already struggling if the answer is ever
// interesting, and the breaker already holds a continuously updated verdict
// derived from real traffic.
func upstreamReadiness(up *Upstream) func(context.Context) error {
	return func(context.Context) error {
		if state := up.Breaker.State(); state == BreakerOpen {
			return errors.New("circuit breaker is open for " + up.Name)
		}
		return nil
	}
}

// decodeJSONBody reads and decodes a bounded JSON body.
func decodeJSONBody(r *http.Request, dst any, limit int64) error {
	buf, err := readLimitedBody(r, limit)
	if err != nil {
		return err
	}
	if len(buf) == 0 {
		return errBadRequest("a request body is required")
	}
	dec := json.NewDecoder(strings.NewReader(string(buf)))
	// Unknown fields are refused. A caller that misspells "effective_at" and
	// gets a 200 has scheduled nothing and been told it worked.
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errBadRequest("malformed request body: %v", err)
	}
	return nil
}

// sortStrings is an insertion sort over the tiny slices this package sorts.
// It avoids pulling sort into the hot request path for lists of four.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

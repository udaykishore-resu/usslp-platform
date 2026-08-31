package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ErrNoBinding is returned when a tenant has no binding by that id. It is a
// 404, not a 401: telling an unauthenticated caller which binding ids exist
// would be an enumeration oracle, so the gateway answers the same way for a
// wrong tenant and a wrong binding.
var ErrNoBinding = errors.New("uig/adapter: no such binding")

// ErrBindingInvalid marks a binding that cannot be installed.
var ErrBindingInvalid = errors.New("uig/adapter: invalid binding")

// Secrets are the credentials one binding uses to authenticate its POS.
//
// The type marshals to redacted placeholders unconditionally. Bindings are
// returned by an operator API, written to debug logs, and included in support
// bundles; making the safe rendering the only rendering means no future caller
// can leak a webhook signing key by forgetting to strip it. Configuration is
// still loaded normally, because only the marshalling side is redacted.
type Secrets struct {
	// HMACKey is the shared signing key for HMAC-based webhooks (Shopify,
	// Square, NCR, and any mapping-driven source that signs its bodies).
	HMACKey string `json:"hmac_key,omitempty"`
	// APIKeyID and APIKey are NCR's access-key pair: the id travels in the
	// clear inside the Authorization header, the key never leaves this struct.
	APIKeyID string `json:"api_key_id,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	// SharedToken is a bare secret compared constant-time against a header —
	// Clover's X-Clover-Auth and the WS-Security password Oracle Retail sends.
	SharedToken string `json:"shared_token,omitempty"`
	// Username pairs with SharedToken for WS-Security UsernameToken.
	Username string `json:"username,omitempty"`
	// NotificationURL is the exact URL Square was configured with. Square signs
	// the concatenation of that URL and the body, so a mismatch between what
	// Square thinks the endpoint is and what the load balancer forwarded is a
	// signature failure — which is why it is configuration rather than being
	// reconstructed from the request.
	NotificationURL string `json:"notification_url,omitempty"`
	// BearerToken authenticates the UIG's *outbound* calls, for sources like
	// Clover whose webhook carries only object ids and must be called back.
	BearerToken string `json:"bearer_token,omitempty"`
	// PeerCommonNames are the mTLS subject common names accepted for this
	// binding. A non-empty list makes a verified client certificate sufficient
	// authentication on its own, which is how retailers on private
	// interconnects prefer to run.
	PeerCommonNames []string `json:"peer_common_names,omitempty"`
}

const redacted = "***redacted***"

// MarshalJSON renders the secrets with every value replaced, preserving which
// credentials are configured so an operator can see that a binding has an HMAC
// key without seeing the key.
func (s Secrets) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	if s.HMACKey != "" {
		out["hmac_key"] = redacted
	}
	if s.APIKeyID != "" {
		out["api_key_id"] = s.APIKeyID
	}
	if s.APIKey != "" {
		out["api_key"] = redacted
	}
	if s.SharedToken != "" {
		out["shared_token"] = redacted
	}
	if s.Username != "" {
		out["username"] = s.Username
	}
	if s.NotificationURL != "" {
		out["notification_url"] = s.NotificationURL
	}
	if s.BearerToken != "" {
		out["bearer_token"] = redacted
	}
	if len(s.PeerCommonNames) > 0 {
		out["peer_common_names"] = s.PeerCommonNames
	}
	return json.Marshal(out)
}

// RateLimitSpec is the per-binding ingress budget.
type RateLimitSpec struct {
	// RatePerSecond is the sustained rate. Zero means the pipeline's default.
	RatePerSecond float64 `json:"rate_per_second,omitempty"`
	// Burst is the bucket depth. POS systems deliver in bursts by nature — a
	// price book publish is thousands of webhooks in a few seconds — so the
	// burst is deliberately generous relative to the rate.
	Burst int `json:"burst,omitempty"`
}

// Binding is one tenant's configured integration with one POS instance.
//
// The binding, not the adapter, is where tenancy lives. One Shopify adapter
// instance serves every Shopify retailer on the platform; the shop domain, the
// signing secret, the store mapping and the currency default are per binding.
// That is what makes onboarding a new retailer a configuration change rather
// than a deployment.
type Binding struct {
	// ID is unique within the tenant and appears in the ingest URL.
	ID string `json:"id"`
	// TenantID owns the binding.
	TenantID canon.TenantID `json:"tenant_id"`
	// Adapter names the registered adapter this binding routes to.
	Adapter string `json:"adapter"`
	// POSInstance identifies which of the tenant's POS deployments this is —
	// "eu-prod-sap", "shopify-uk". A tenant frequently has several of the same
	// kind and support needs to tell them apart.
	POSInstance string `json:"pos_instance,omitempty"`
	// Description is free text for the operator UI.
	Description string `json:"description,omitempty"`
	// Disabled stops ingest without deleting configuration, which is what an
	// operator reaches for when a partner's integration is misbehaving at 2am.
	Disabled bool `json:"disabled,omitempty"`

	// Secrets holds the credentials.
	Secrets Secrets `json:"secrets,omitempty"`

	// DefaultStore is used when the source identifies no store, which is the
	// normal case for a single-site retailer.
	DefaultStore canon.StoreID `json:"default_store,omitempty"`
	// StoreMap translates the source system's store codes to USSLP store ids.
	// Retailers do not renumber their estate to suit a vendor, so the
	// translation has to live somewhere; here it is one table per integration
	// rather than one per mapping document.
	StoreMap map[string]canon.StoreID `json:"store_map,omitempty"`
	// AllowUnmappedStores passes an unrecognised source store code through as a
	// USSLP store id. Off by default: silently inventing a store is how a price
	// change goes to a shelf in the wrong building.
	AllowUnmappedStores bool `json:"allow_unmapped_stores,omitempty"`

	// DefaultCurrency is applied when the source omits one — which Shopify's
	// product webhooks and most flat-file exports do.
	DefaultCurrency string `json:"default_currency,omitempty"`
	// AllowedCurrencies restricts what this binding may emit. A feed that
	// suddenly starts sending a currency the retailer does not trade in is a
	// mis-routed integration, and catching it here stops it reaching a shelf.
	AllowedCurrencies []string `json:"allowed_currencies,omitempty"`

	// Options is adapter-specific configuration, compiled once at install time
	// by adapters implementing Configurable.
	Options json.RawMessage `json:"options,omitempty"`

	// RateLimit is this binding's ingress budget.
	RateLimit RateLimitSpec `json:"rate_limit,omitempty"`

	// RetainRaw stores the raw body of *successful* deliveries too, not just
	// failed ones, so they can be replayed after a mapping fix. It is off by
	// default because at the platform's peak the raw bodies of successful
	// deliveries are terabytes a day; it is switched on per binding while an
	// integration is being commissioned.
	RetainRaw bool `json:"retain_raw,omitempty"`

	// InitiatedBy is written to every emitted change's InitiatedBy field, so an
	// auditor asking "who changed this price" gets the integration, not a
	// generic "system".
	InitiatedBy string `json:"initiated_by,omitempty"`

	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	// options is the compiled form of Options.
	options any
}

// CompiledOptions returns the adapter-specific compiled configuration.
func (b *Binding) CompiledOptions() any { return b.options }

// Currency returns the currency to use when a payload does not carry one.
func (b *Binding) Currency() string {
	return strings.ToUpper(strings.TrimSpace(b.DefaultCurrency))
}

// CurrencyAllowed reports whether this binding may emit a currency.
func (b *Binding) CurrencyAllowed(code string) bool {
	if len(b.AllowedCurrencies) == 0 {
		return true
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, c := range b.AllowedCurrencies {
		if strings.ToUpper(strings.TrimSpace(c)) == code {
			return true
		}
	}
	return false
}

// Validate checks the fields the pipeline relies on. It is called on install so
// that a malformed binding never reaches the price path.
func (b *Binding) Validate() error {
	switch {
	case b.ID == "" || !canon.ValidID(b.ID):
		return fmt.Errorf("%w: id %q", ErrBindingInvalid, b.ID)
	case b.TenantID == "" || !canon.ValidID(string(b.TenantID)):
		return fmt.Errorf("%w: tenant_id %q", ErrBindingInvalid, b.TenantID)
	case b.Adapter == "":
		return fmt.Errorf("%w: missing adapter", ErrBindingInvalid)
	case b.DefaultStore != "" && !canon.ValidID(string(b.DefaultStore)):
		return fmt.Errorf("%w: default_store %q", ErrBindingInvalid, b.DefaultStore)
	}
	for src, dst := range b.StoreMap {
		if dst == "" || !canon.ValidID(string(dst)) {
			return fmt.Errorf("%w: store_map[%q] = %q is not a usable store id", ErrBindingInvalid, src, dst)
		}
	}
	if c := b.Currency(); c != "" && !(canon.Money{Currency: c}).Valid() {
		return fmt.Errorf("%w: default_currency %q", ErrBindingInvalid, c)
	}
	for _, c := range b.AllowedCurrencies {
		if !(canon.Money{Currency: strings.ToUpper(c)}).Valid() {
			return fmt.Errorf("%w: allowed_currencies contains %q", ErrBindingInvalid, c)
		}
	}
	if b.RateLimit.RatePerSecond < 0 || b.RateLimit.Burst < 0 {
		return fmt.Errorf("%w: negative rate limit", ErrBindingInvalid)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Binding store
// ---------------------------------------------------------------------------

// BindingStore holds the installed bindings, indexed by tenant.
//
// It is an in-memory store fed from configuration at start-up and by the
// operator API afterwards, because binding lookup is on the hot path of every
// single delivery and a database round trip there would spend a fifth of the
// gateway's entire 50ms latency slice before any work had been done.
type BindingStore struct {
	mu       sync.RWMutex
	byTenant map[canon.TenantID]map[string]*Binding
	registry *Registry
}

// NewBindingStore creates a store that compiles adapter options against reg.
func NewBindingStore(reg *Registry) *BindingStore {
	return &BindingStore{
		byTenant: make(map[canon.TenantID]map[string]*Binding),
		registry: reg,
	}
}

// Put installs or replaces a binding, validating it and compiling any
// adapter-specific options.
//
// The binding is copied, so a caller mutating its struct afterwards cannot
// change the configuration a concurrent delivery is being processed under.
func (s *BindingStore) Put(b *Binding) error {
	if b == nil {
		return fmt.Errorf("%w: nil binding", ErrBindingInvalid)
	}
	cp := *b
	if err := cp.Validate(); err != nil {
		return err
	}
	if s.registry == nil {
		return fmt.Errorf("%w: binding store has no adapter registry", ErrBindingInvalid)
	}
	a, err := s.registry.Get(cp.Adapter)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBindingInvalid, err)
	}
	if c, ok := a.(Configurable); ok {
		opts, err := c.CompileOptions(cp.Options)
		if err != nil {
			return fmt.Errorf("%w: adapter %s options: %v", ErrBindingInvalid, cp.Adapter, err)
		}
		cp.options = opts
	} else if len(cp.Options) > 0 {
		return fmt.Errorf("%w: adapter %s takes no options", ErrBindingInvalid, cp.Adapter)
	}
	now := time.Now().UTC()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.byTenant[cp.TenantID]
	if m == nil {
		m = make(map[string]*Binding)
		s.byTenant[cp.TenantID] = m
	}
	if prev, ok := m[cp.ID]; ok {
		cp.CreatedAt = prev.CreatedAt
	}
	m[cp.ID] = &cp
	return nil
}

// Get returns a binding.
func (s *BindingStore) Get(tenant canon.TenantID, id string) (*Binding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.byTenant[tenant]; ok {
		if b, ok := m[id]; ok {
			return b, nil
		}
	}
	return nil, fmt.Errorf("%w: %s/%s", ErrNoBinding, tenant, id)
}

// List returns a tenant's bindings, ordered by id so an operator API is stable
// between calls.
func (s *BindingStore) List(tenant canon.TenantID) []*Binding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.byTenant[tenant]
	out := make([]*Binding, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Delete removes a binding, reporting whether it existed.
func (s *BindingStore) Delete(tenant canon.TenantID, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.byTenant[tenant]
	if m == nil {
		return false
	}
	_, ok := m[id]
	delete(m, id)
	return ok
}

// Count returns the number of installed bindings across all tenants, which the
// readiness check uses: a gateway with no bindings has almost certainly failed
// to load its configuration and must not take traffic.
func (s *BindingStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.byTenant {
		n += len(m)
	}
	return n
}

// LoadJSON installs a JSON array of bindings, reporting every failure at once
// rather than one per restart.
func (s *BindingStore) LoadJSON(raw []byte) error {
	var list []*Binding
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("%w: %v", ErrBindingInvalid, err)
	}
	var problems []string
	for _, b := range list {
		if err := s.Put(b); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrBindingInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Store resolution
// ---------------------------------------------------------------------------

// StoreResolver turns a source system's store code into a USSLP store id.
//
// It is an interface because the mapping table in a binding is the simple case.
// A chain with 4,000 sites keeps that mapping in its own store directory, and
// the UIG resolves against it — but the resolution still has to happen in the
// enrichment step, before publication, so that everything downstream of
// price-updates only ever sees canonical ids.
type StoreResolver interface {
	// Resolve maps external (possibly empty) to a canonical store id.
	Resolve(ctx context.Context, b *Binding, external string) (canon.StoreID, error)
}

// BindingResolver resolves stores from the binding's own map and default.
type BindingResolver struct{}

// Resolve implements StoreResolver.
func (BindingResolver) Resolve(_ context.Context, b *Binding, external string) (canon.StoreID, error) {
	external = strings.TrimSpace(external)
	if external == "" {
		if b.DefaultStore == "" {
			return "", Invalid("store_unresolved",
				"the source identified no store and the binding has no default store", nil)
		}
		return b.DefaultStore, nil
	}
	if id, ok := b.StoreMap[external]; ok {
		return id, nil
	}
	// Case-insensitive second pass: source systems are inconsistent about the
	// case of site codes between their catalogue and their event feeds, and a
	// price that fails to land because of it is a support ticket rather than a
	// safety property.
	for src, id := range b.StoreMap {
		if strings.EqualFold(src, external) {
			return id, nil
		}
	}
	if b.AllowUnmappedStores {
		if !canon.ValidID(external) {
			return "", Invalid("store_invalid",
				fmt.Sprintf("source store code %q cannot be a USSLP store id", external), nil)
		}
		return canon.StoreID(external), nil
	}
	if b.DefaultStore != "" && len(b.StoreMap) == 0 {
		return b.DefaultStore, nil
	}
	return "", Invalid("store_unmapped",
		fmt.Sprintf("source store code %q is not in the binding's store map", external), nil)
}

// Tenants returns every tenant with at least one installed binding.
//
// It exists for start-up only — enumerating tenants is exactly what the request
// path must never do, since a lookup that walks tenants is a lookup whose cost
// grows with the customer base. Start-up wants it once, to find the bindings
// that need a file watcher.
func (s *BindingStore) Tenants() []canon.TenantID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]canon.TenantID, 0, len(s.byTenant))
	for t, m := range s.byTenant {
		if len(m) == 0 {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

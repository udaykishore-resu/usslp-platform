package apigw

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The route table
//
// This slice is the gateway's public surface. It is data, not code: every
// property that governs a request — which upstream answers it, what permission
// it needs, whether it is store-scoped, how long it may take, whether it draws
// on the expensive rate-limit bucket — is stated here, in one place, next to
// the path.
//
// Two things follow. First, the middleware chain is assembled uniformly from
// the table (see [Gateway.buildMux]), so there is no route that can be
// registered without authentication, without RBAC, or without rate limiting;
// forgetting is not an available mistake. Second, the OpenAPI document is
// checkable against the table mechanically, which
// [TestOpenAPIMatchesRouteTable] does — a route added without documentation,
// or documentation left behind after a route moves, fails the build.
// ---------------------------------------------------------------------------

// Route describes one endpoint.
type Route struct {
	// Method and Pattern form the http.ServeMux pattern. Pattern uses the
	// standard library's {name} wildcards and is also, verbatim, the OpenAPI
	// path template.
	Method  string
	Pattern string
	// Operation is the stable operationId. It names the metric series, the
	// span, the access log line and the OpenAPI operation, so all four join.
	Operation string
	// Summary and Tags feed the OpenAPI document.
	Summary string
	Tags    []string

	// Upstream is the internal service that answers, or "" when the gateway
	// answers it itself.
	Upstream string
	// Rewrite, when set, is the upstream path template. "{tenant}" is filled
	// from the authenticated principal — never from the URL — which is how a
	// tenant-in-path upstream like the Universal Integration Gateway is
	// addressed without ever letting a caller name the tenant. Any other
	// "{name}" is filled from the matched path wildcard.
	Rewrite string
	// Native names the gateway's own handler for a non-proxied route.
	Native string

	// Permission is required to call this route. The zero value means
	// authentication only.
	Permission Permission
	// StorePathValue is the wildcard holding a store id. When set, the
	// authorisation middleware refuses a principal whose store scope excludes
	// it — with a 404, so an out-of-scope store cannot be probed for existence.
	StorePathValue string
	// Public routes need no credential at all: health, the console shell, the
	// OpenAPI document and its viewer.
	Public bool
	// Expensive routes draw on the tighter rate-limit bucket.
	Expensive bool
	// Timeout overrides the upstream default for this route.
	Timeout time.Duration
	// Streaming marks the WebSocket route, which is excluded from the response
	// buffering and the request-latency histogram.
	Streaming bool
	// NoBody documents that the route takes no request body, used to generate
	// an accurate OpenAPI operation and checked by the agreement test.
	NoBody bool
}

// Timeouts chosen per class of work rather than one number for everything.
const (
	// fastTimeout is for read paths that hit a warm read model.
	fastTimeout = 2 * time.Second
	// writeTimeout is for a single price update: it must fit inside the
	// 3-second end-to-end budget with the edge tier's share left over.
	writeTimeout = 3 * time.Second
	// batchTimeout is for a store-wide repricing, which legitimately takes
	// tens of seconds at 40,000 labels.
	batchTimeout = 90 * time.Second
	// analyticsTimeout is for a query over a warehouse.
	analyticsTimeout = 30 * time.Second
)

// routes is the table. Order is irrelevant to routing — http.ServeMux matches
// by specificity — and is chosen for readability.
var routes = []Route{
	// --- unauthenticated -----------------------------------------------
	{Method: "GET", Pattern: "/healthz", Operation: "getHealth", Public: true, NoBody: true,
		Summary: "Liveness. Answers as long as the process is scheduling goroutines.",
		Tags:    []string{"operations"}, Native: "health"},
	{Method: "GET", Pattern: "/readyz", Operation: "getReadiness", Public: true, NoBody: true,
		Summary: "Readiness. Fails while start-up is incomplete or a dependency is unreachable.",
		Tags:    []string{"operations"}, Native: "ready"},
	{Method: "GET", Pattern: "/openapi.json", Operation: "getOpenAPI", Public: true, NoBody: true,
		Summary: "This document.", Tags: []string{"operations"}, Native: "openapi"},
	{Method: "GET", Pattern: "/docs", Operation: "getDocs", Public: true, NoBody: true,
		Summary: "Human-readable API reference.", Tags: []string{"operations"}, Native: "docs"},
	{Method: "GET", Pattern: "/console", Operation: "getConsole", Public: true, NoBody: true,
		Summary: "The store operations console.", Tags: []string{"operations"}, Native: "console"},

	// --- gateway-native ------------------------------------------------
	{Method: "GET", Pattern: "/v1/me", Operation: "getMe", Permission: Read(ResSelf), NoBody: true,
		Summary: "The calling credential: tenant, roles, store scope, expiry.",
		Tags:    []string{"identity"}, Native: "me"},
	{Method: "GET", Pattern: "/v1/keys", Operation: "listAPIKeys", Permission: Read(ResKeys), NoBody: true,
		Summary: "The tenant's API keys. Never returns key material.",
		Tags:    []string{"identity"}, Native: "listKeys"},
	{Method: "POST", Pattern: "/v1/keys", Operation: "issueAPIKey", Permission: Admin(ResKeys),
		Summary: "Mint an API key. The plaintext is returned once and never again.",
		Tags:    []string{"identity"}, Native: "issueKey"},
	{Method: "DELETE", Pattern: "/v1/keys/{keyId}", Operation: "revokeAPIKey", Permission: Admin(ResKeys), NoBody: true,
		Summary: "Revoke an API key immediately.", Tags: []string{"identity"}, Native: "revokeKey"},
	{Method: "GET", Pattern: "/v1/stream", Operation: "streamEvents", Permission: Read(ResStream),
		Streaming: true, NoBody: true,
		Summary: "WebSocket feed of platform events, filtered to the caller's tenant.",
		Tags:    []string{"streaming"}, Native: "stream"},
	{Method: "POST", Pattern: "/v1/prices", Operation: "updatePrice", Permission: Write(ResPrices),
		Timeout: writeTimeout,
		Summary: "Change one product's price in one store.",
		Tags:    []string{"pricing"}, Native: "updatePrice"},
	{Method: "GET", Pattern: "/v1/stores/{storeId}/overview", Operation: "getStoreOverview",
		Permission: Read(ResStores), StorePathValue: "storeId", Timeout: fastTimeout, NoBody: true,
		Summary: "Composed store dashboard: health, mesh, SLO and rollouts in one call.",
		Tags:    []string{"stores"}, Native: "storeOverview"},

	// --- label-service --------------------------------------------------
	{Method: "POST", Pattern: "/v1/prices:batch", Operation: "batchUpdatePrices",
		Upstream: UpstreamLabel, Permission: Write(ResPrices), Expensive: true, Timeout: batchTimeout,
		Summary: "Change many prices at once.", Tags: []string{"pricing"}},
	{Method: "POST", Pattern: "/v1/labels/{labelId}/price", Operation: "updateLabelPrice",
		Upstream: UpstreamLabel, Permission: Write(ResPrices), Timeout: writeTimeout,
		Summary: "Change the price shown on one specific label.", Tags: []string{"pricing"}},
	{Method: "GET", Pattern: "/v1/labels/{labelId}", Operation: "getLabel",
		Upstream: UpstreamLabel, Permission: Read(ResLabels), Timeout: fastTimeout, NoBody: true,
		Summary: "One label's current state.", Tags: []string{"labels"}},
	{Method: "GET", Pattern: "/v1/labels/{labelId}/history", Operation: "getLabelHistory",
		Upstream: UpstreamLabel, Permission: Read(ResLabels), Timeout: fastTimeout, NoBody: true,
		Summary: "A label's price history, newest first.", Tags: []string{"labels"}},
	{Method: "GET", Pattern: "/v1/stores/{storeId}/labels", Operation: "listStoreLabels",
		Upstream: UpstreamLabel, Permission: Read(ResLabels), StorePathValue: "storeId",
		Timeout: fastTimeout, NoBody: true,
		Summary: "Every label in a store with its health.", Tags: []string{"labels"}},
	{Method: "GET", Pattern: "/v1/stores/{storeId}/slo", Operation: "getStoreSLO",
		Upstream: UpstreamLabel, Permission: Read(ResStores), StorePathValue: "storeId",
		Timeout: fastTimeout, NoBody: true,
		Summary: "Measured end-to-end price propagation latency against the 3s SLO.",
		Tags:    []string{"stores"}},

	// --- device-registry -------------------------------------------------
	{Method: "GET", Pattern: "/v1/stores/{storeId}/devices", Operation: "listStoreDevices",
		Upstream: UpstreamRegistry, Permission: Read(ResDevices), StorePathValue: "storeId",
		Timeout: fastTimeout, NoBody: true,
		Summary: "The devices installed in a store.", Tags: []string{"devices"}},
	{Method: "GET", Pattern: "/v1/stores/{storeId}/mesh", Operation: "getStoreMesh",
		Upstream: UpstreamRegistry, Permission: Read(ResStores), StorePathValue: "storeId",
		Timeout: fastTimeout, NoBody: true,
		Summary: "Zigbee mesh topology and predicted link risk.", Tags: []string{"stores"}},
	{Method: "GET", Pattern: "/v1/stores/{storeId}/health", Operation: "getStoreHealth",
		Upstream: UpstreamRegistry, Permission: Read(ResStores), StorePathValue: "storeId",
		Timeout: fastTimeout, NoBody: true,
		Summary: "Labels online, battery warnings and mesh health for one store.",
		Tags:    []string{"stores"}},
	{Method: "GET", Pattern: "/v1/stores/{storeId}/runway", Operation: "getStoreRunway",
		Upstream: UpstreamRegistry, Permission: Read(ResStores), StorePathValue: "storeId",
		Timeout: fastTimeout, NoBody: true,
		Summary: "Projected battery runway for a store's fleet.", Tags: []string{"stores"}},
	{Method: "GET", Pattern: "/v1/stores/{storeId}/planogram", Operation: "getPlanogram",
		Upstream: UpstreamRegistry, Permission: Read(ResStores), StorePathValue: "storeId",
		Timeout: fastTimeout, NoBody: true,
		Summary: "The store's current planogram.", Tags: []string{"stores"}},
	{Method: "POST", Pattern: "/v1/stores/{storeId}/planogram", Operation: "uploadPlanogram",
		Upstream: UpstreamRegistry, Permission: Admin(ResStores), StorePathValue: "storeId",
		Expensive: true, Timeout: batchTimeout,
		Summary: "Replace a store's planogram, repositioning every affected label.",
		Tags:    []string{"stores"}},
	{Method: "GET", Pattern: "/v1/devices/{deviceId}", Operation: "getDevice",
		Upstream: UpstreamRegistry, Permission: Read(ResDevices), Timeout: fastTimeout, NoBody: true,
		Summary: "One device's registry record.", Tags: []string{"devices"}},
	{Method: "POST", Pattern: "/v1/devices:provision", Operation: "provisionDevice",
		Upstream: UpstreamRegistry, Rewrite: "/v1/provision", Permission: Write(ResDevices),
		Timeout: writeTimeout,
		Summary: "Provision a device and issue its certificate.", Tags: []string{"devices"}},
	{Method: "POST", Pattern: "/v1/devices/{deviceId}/retire", Operation: "retireDevice",
		Upstream: UpstreamRegistry, Permission: Write(ResDevices), Timeout: writeTimeout,
		Summary: "Retire a device and revoke its certificate.", Tags: []string{"devices"}},
	{Method: "POST", Pattern: "/v1/devices/{deviceId}/quarantine", Operation: "quarantineDevice",
		Upstream: UpstreamRegistry, Permission: Write(ResDevices), Timeout: writeTimeout,
		Summary: "Quarantine a suspect device.", Tags: []string{"devices"}},
	{Method: "POST", Pattern: "/v1/devices/{deviceId}/release", Operation: "releaseDevice",
		Upstream: UpstreamRegistry, Permission: Write(ResDevices), Timeout: writeTimeout,
		Summary: "Release a device from quarantine.", Tags: []string{"devices"}},
	{Method: "GET", Pattern: "/v1/fleet/summary", Operation: "getFleetSummary",
		Upstream: UpstreamRegistry, Permission: Read(ResDevices), Timeout: fastTimeout, NoBody: true,
		Summary: "Fleet-wide device counts and health.", Tags: []string{"devices"}},

	// --- ota-service -----------------------------------------------------
	{Method: "GET", Pattern: "/v1/ota/jobs", Operation: "listOTAJobs",
		Upstream: UpstreamOTA, Permission: Read(ResOTA), Timeout: fastTimeout, NoBody: true,
		Summary: "Firmware rollout jobs.", Tags: []string{"ota"}},
	{Method: "POST", Pattern: "/v1/ota/jobs", Operation: "createOTAJob",
		Upstream: UpstreamOTA, Permission: Admin(ResOTA), Timeout: writeTimeout,
		Summary: "Start a staged firmware rollout.", Tags: []string{"ota"}},
	{Method: "GET", Pattern: "/v1/ota/jobs/{jobId}", Operation: "getOTAJob",
		Upstream: UpstreamOTA, Permission: Read(ResOTA), Timeout: fastTimeout, NoBody: true,
		Summary: "One rollout's progress.", Tags: []string{"ota"}},
	{Method: "GET", Pattern: "/v1/ota/jobs/{jobId}/devices", Operation: "listOTAJobDevices",
		Upstream: UpstreamOTA, Permission: Read(ResOTA), Timeout: fastTimeout, NoBody: true,
		Summary: "Per-device outcomes within a rollout.", Tags: []string{"ota"}},
	{Method: "POST", Pattern: "/v1/ota/jobs/{jobId}/pause", Operation: "pauseOTAJob", NoBody: true,
		Upstream: UpstreamOTA, Permission: Write(ResOTA), Timeout: writeTimeout,
		Summary: "Pause a rollout.", Tags: []string{"ota"}},
	{Method: "POST", Pattern: "/v1/ota/jobs/{jobId}/resume", Operation: "resumeOTAJob", NoBody: true,
		Upstream: UpstreamOTA, Permission: Write(ResOTA), Timeout: writeTimeout,
		Summary: "Resume a paused rollout.", Tags: []string{"ota"}},
	{Method: "POST", Pattern: "/v1/ota/jobs/{jobId}/abort", Operation: "abortOTAJob", NoBody: true,
		Upstream: UpstreamOTA, Permission: Admin(ResOTA), Timeout: writeTimeout,
		Summary: "Abort a rollout.", Tags: []string{"ota"}},
	{Method: "POST", Pattern: "/v1/ota/jobs/{jobId}/rollback", Operation: "rollbackOTAJob",
		Upstream: UpstreamOTA, Permission: Admin(ResOTA), Timeout: writeTimeout,
		Summary: "Roll a fleet back to the previous firmware.", Tags: []string{"ota"}},

	// --- pricing-service --------------------------------------------------
	{Method: "GET", Pattern: "/v1/pricing/rules", Operation: "listPricingRules",
		Upstream: UpstreamPricing, Permission: Read(ResPricing), Timeout: fastTimeout, NoBody: true,
		Summary: "The tenant's pricing rules.", Tags: []string{"pricing"}},
	{Method: "POST", Pattern: "/v1/pricing/rules", Operation: "createPricingRule",
		Upstream: UpstreamPricing, Permission: Write(ResPricing), Timeout: writeTimeout,
		Summary: "Create a pricing rule.", Tags: []string{"pricing"}},
	{Method: "POST", Pattern: "/v1/pricing/simulate", Operation: "simulatePricing",
		Upstream: UpstreamPricing, Permission: Write(ResPricing), Expensive: true, Timeout: analyticsTimeout,
		Summary: "Dry-run a rule set across an estate without changing a shelf.",
		Tags:    []string{"pricing"}},

	// --- promotion-service ------------------------------------------------
	{Method: "GET", Pattern: "/v1/promotions", Operation: "listPromotions",
		Upstream: UpstreamPromotion, Permission: Read(ResPromotions), Timeout: fastTimeout, NoBody: true,
		Summary: "The tenant's promotions.", Tags: []string{"promotions"}},
	{Method: "POST", Pattern: "/v1/promotions", Operation: "createPromotion",
		Upstream: UpstreamPromotion, Permission: Write(ResPromotions), Timeout: writeTimeout,
		Summary: "Create a promotion.", Tags: []string{"promotions"}},
	{Method: "GET", Pattern: "/v1/promotions/{promotionId}", Operation: "getPromotion",
		Upstream: UpstreamPromotion, Permission: Read(ResPromotions), Timeout: fastTimeout, NoBody: true,
		Summary: "One promotion.", Tags: []string{"promotions"}},
	{Method: "POST", Pattern: "/v1/promotions/{promotionId}/activate", Operation: "activatePromotion",
		Upstream: UpstreamPromotion, Permission: Write(ResPromotions), Expensive: true, Timeout: batchTimeout,
		Summary: "Activate a promotion across every affected label.", Tags: []string{"promotions"}},

	// --- analytics-service ------------------------------------------------
	{Method: "POST", Pattern: "/v1/analytics/query", Operation: "runAnalyticsQuery",
		Upstream: UpstreamAnalytics, Permission: Read(ResAnalytics), Expensive: true, Timeout: analyticsTimeout,
		Summary: "Run an analytics query over the tenant's estate.", Tags: []string{"analytics"}},
	{Method: "GET", Pattern: "/v1/analytics/slo", Operation: "getAnalyticsSLO",
		Upstream: UpstreamAnalytics, Permission: Read(ResAnalytics), Timeout: fastTimeout, NoBody: true,
		Summary: "Fleet-wide SLO attainment and error budget.", Tags: []string{"analytics"}},
	{Method: "GET", Pattern: "/v1/analytics/reports/{reportId}", Operation: "getAnalyticsReport",
		Upstream: UpstreamAnalytics, Permission: Read(ResAnalytics), Timeout: analyticsTimeout, NoBody: true,
		Summary: "A previously generated report.", Tags: []string{"analytics"}},

	// --- universal integration gateway ------------------------------------
	{Method: "GET", Pattern: "/v1/pos/integrations", Operation: "listPOSIntegrations",
		Upstream: UpstreamUIG, Rewrite: "/v1/bindings/{tenant}", Permission: Read(ResPOS),
		Timeout: fastTimeout, NoBody: true,
		Summary: "Configured POS and ERP integrations.", Tags: []string{"pos"}},
	{Method: "GET", Pattern: "/v1/pos/deliveries", Operation: "listPOSDeliveries",
		Upstream: UpstreamUIG, Rewrite: "/v1/deliveries/{tenant}", Permission: Read(ResPOS),
		Timeout: fastTimeout, NoBody: true,
		Summary: "Recent inbound POS deliveries and their outcomes.", Tags: []string{"pos"}},
}

// Routes returns a copy of the route table.
//
// A copy, so that a caller walking it — the OpenAPI agreement test, the
// console's capability list — cannot mutate the routing of a running gateway.
func Routes() []Route {
	out := make([]Route, len(routes))
	copy(out, routes)
	return out
}

// Key is the "METHOD /path" form used as a map key when comparing the table
// against the OpenAPI document.
func (rt Route) Key() string { return rt.Method + " " + rt.Pattern }

// upstreamPath computes the path to request from the upstream.
//
// With no rewrite the client's path is forwarded unchanged, which is the case
// for every service that already speaks the platform's header-based tenancy.
// With a rewrite, "{tenant}" comes from the principal and every other
// placeholder from a matched wildcard — so a template can never be filled from
// a value the caller chose except through a wildcard the router already
// matched and the authorisation layer already scoped.
func (rt Route) upstreamPath(r *http.Request, p Principal) (string, error) {
	if rt.Rewrite == "" {
		return r.URL.EscapedPath(), nil
	}
	var b strings.Builder
	rest := rt.Rewrite
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		shut := strings.IndexByte(rest[open:], '}')
		if shut < 0 {
			return "", errInternal("route %s has an unterminated placeholder in %q", rt.Operation, rt.Rewrite)
		}
		b.WriteString(rest[:open])
		name := rest[open+1 : open+shut]
		var value string
		if name == "tenant" {
			value = string(p.TenantID)
		} else {
			value = r.PathValue(name)
		}
		if value == "" {
			return "", errInternal("route %s rewrite %q has no value for %q", rt.Operation, rt.Rewrite, name)
		}
		b.WriteString(url.PathEscape(value))
		rest = rest[open+shut+1:]
	}
}

// validateRoutes checks the table's internal consistency at construction time.
// These are programming errors; failing at start-up beats discovering them
// when a customer calls the route.
func validateRoutes(rs []Route, upstreams map[string]*Upstream, natives map[string]http.Handler) error {
	seen := make(map[string]bool, len(rs))
	ops := make(map[string]bool, len(rs))
	for _, rt := range rs {
		if seen[rt.Key()] {
			return fmt.Errorf("apigw: duplicate route %s", rt.Key())
		}
		seen[rt.Key()] = true
		if rt.Operation == "" {
			return fmt.Errorf("apigw: route %s has no operation id", rt.Key())
		}
		if ops[rt.Operation] {
			return fmt.Errorf("apigw: duplicate operation id %q", rt.Operation)
		}
		ops[rt.Operation] = true
		if rt.Summary == "" {
			return fmt.Errorf("apigw: route %s has no summary", rt.Key())
		}
		if (rt.Upstream == "") == (rt.Native == "") {
			return fmt.Errorf("apigw: route %s must name exactly one of Upstream or Native", rt.Key())
		}
		if rt.Upstream != "" {
			if _, ok := upstreams[rt.Upstream]; !ok {
				return fmt.Errorf("apigw: route %s names unknown upstream %q", rt.Key(), rt.Upstream)
			}
		}
		if rt.Native != "" {
			if _, ok := natives[rt.Native]; !ok {
				return fmt.Errorf("apigw: route %s names unknown native handler %q", rt.Key(), rt.Native)
			}
		}
		if rt.Public && !rt.Permission.Zero() {
			return fmt.Errorf("apigw: route %s is public and also requires %s", rt.Key(), rt.Permission)
		}
		if !rt.Public && rt.Permission.Zero() {
			return fmt.Errorf("apigw: route %s is authenticated but requires no permission", rt.Key())
		}
		if rt.Streaming {
			// A WebSocket upgrade is a GET with no body served by the gateway
			// itself: there is nothing to proxy, because the connection is
			// hijacked and the fan-out comes from the event bus.
			if rt.Method != http.MethodGet || !rt.NoBody || rt.Native == "" {
				return fmt.Errorf("apigw: streaming route %s must be a bodyless GET served natively", rt.Key())
			}
		}
		if rt.StorePathValue != "" && !strings.Contains(rt.Pattern, "{"+rt.StorePathValue+"}") {
			return fmt.Errorf("apigw: route %s scopes on %q which is not a wildcard in the pattern",
				rt.Key(), rt.StorePathValue)
		}
	}
	return nil
}

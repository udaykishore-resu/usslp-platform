// Package apigw implements the USSLP API Gateway: the platform's front door.
//
// Everything a retailer's systems, a store operator's browser or a partner
// integration can reach lives behind this one process. It authenticates the
// caller, decides what that caller may do, enforces the tenant boundary,
// rate-limits, routes to the internal services with per-route timeouts and
// circuit breaking, streams live platform events over WebSocket, and serves the
// operator console and the OpenAPI description of the whole surface.
//
// # Why a gateway at all
//
// The internal services trust their callers. label-service reads a tenant from
// a header; device-registry reads one from a path segment. That is the correct
// design for a service mesh where every hop is mutually authenticated and the
// only clients are other USSLP components — and it is catastrophic if any of
// those services is ever exposed directly. The gateway is the single place
// where an untrusted caller becomes a trusted, tenant-bound principal, and it
// is the only process in the platform that faces the public internet.
//
// # The tenant boundary
//
// Tenancy is not a check, it is a construction. A request's tenant is derived
// exclusively from the credential that authenticated it — an API key record, a
// verified JWT, or the SPIFFE identity in a client certificate — and is then
// stamped onto the proxied request, overwriting anything the client sent. The
// gateway strips every inbound tenant-bearing header before routing
// (see [scrubbedRequestHeaders]) so there is no code path in which a client's
// assertion about its own tenancy reaches an upstream service. Upstreams that
// scope by path (the Universal Integration Gateway) are addressed through a
// rewrite template whose tenant segment is filled from the principal, never
// from the URL.
//
// The observable consequence, which [TestCrossTenantLabelAccessIsNotFound]
// pins down: a fully authenticated tenant A asking for a label belonging to
// tenant B gets 404. Not 403 — confirming that an identifier exists somewhere
// in the platform is itself a cross-tenant leak.
//
// # Layout
//
//	principal.go   who the caller is, and the request context that carries it
//	rbac.go        roles, permissions and store scoping
//	apikey.go      tenant-scoped API keys, hashed at rest
//	auth.go        the one authentication and authorisation middleware
//	ratelimit.go   token buckets: per tenant, per credential, per cost class
//	breaker.go     per-upstream circuit breaker
//	proxy.go       the reverse proxy: timeouts, retries, size limits
//	routes.go      the route table — the single source of truth for the surface
//	middleware.go  request id, W3C trace, access log, metrics, panic recovery
//	websocket.go   RFC 6455, by hand, over net/http connection hijacking
//	stream.go      the tenant-filtered live event fan-out
//	native.go      endpoints the gateway answers itself, including composition
//	console.go     the embedded operator console
//	openapi.go     the embedded OpenAPI 3.1 document and its viewer
//	gateway.go     assembly and graceful shutdown
package apigw

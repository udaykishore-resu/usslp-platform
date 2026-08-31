package apigw

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
)

// ---------------------------------------------------------------------------
// The API description
//
// The document is hand-written and embedded. Generating it from the route
// table would guarantee agreement and describe nothing: the parts of an API
// description that are worth reading — what a field means, why a 207 comes
// back from a batch, which errors are worth retrying — cannot be derived from
// a Go struct. So it is written by a human and *checked* against the route
// table by [TestOpenAPIMatchesRouteTable], which fails the build if the two
// disagree about a path, a method, an operation id, a path parameter, whether
// a request has a body, or whether it needs a credential.
//
// That is the arrangement that actually holds: prose that a person wrote, with
// a machine watching for drift.
// ---------------------------------------------------------------------------

//go:embed assets/openapi.json
var openAPIDocument []byte

//go:embed assets/docs.html
var docsPage []byte

// OpenAPIDocument returns the embedded OpenAPI 3.1 description.
func OpenAPIDocument() []byte {
	out := make([]byte, len(openAPIDocument))
	copy(out, openAPIDocument)
	return out
}

// etagOf computes a strong ETag for an embedded asset. The assets are fixed at
// build time, so the tag is stable for the life of a release and a console
// left open across a deployment revalidates in one round trip instead of
// re-downloading.
func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

var (
	openAPIETag = etagOf(openAPIDocument)
	docsETag    = etagOf(docsPage)
)

// serveAsset writes an embedded asset with revalidation support.
func serveAsset(w http.ResponseWriter, r *http.Request, body []byte, contentType, etag string, csp string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("ETag", etag)
	// no-cache, not no-store: the client may keep a copy but must revalidate,
	// which is what makes the ETag useful.
	h.Set("Cache-Control", "no-cache")
	if csp != "" {
		h.Set("Content-Security-Policy", csp)
	}
	// These pages are same-origin only and never framed: the console holds a
	// live credential in browser storage and clickjacking it would be a
	// one-click price change.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (g *Gateway) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, openAPIDocument, "application/json; charset=utf-8", openAPIETag, "")
}

// assetCSP forbids everything the embedded pages do not need. Inline script
// and style are permitted because the pages are single files with no build
// step and no CDN — that is the whole point of embedding them — but nothing
// may be loaded from anywhere, and connections are limited to this origin,
// which covers the console's WebSocket.
const assetCSP = "default-src 'none'; " +
	"script-src 'unsafe-inline'; " +
	"style-src 'unsafe-inline'; " +
	"img-src data:; " +
	"connect-src 'self'; " +
	"form-action 'none'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

func (g *Gateway) handleDocs(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, docsPage, "text/html; charset=utf-8", docsETag, assetCSP)
}

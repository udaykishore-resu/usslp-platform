package apigw

import (
	_ "embed"
	"net/http"
)

// ---------------------------------------------------------------------------
// The operator console
//
// One HTML file, embedded in the binary, with its CSS and JavaScript inline.
// No CDN, no build step, no framework, no fonts fetched from a third party.
//
// That is not minimalism for its own sake. The console is the thing a store
// manager opens when the shelves are showing the wrong price, and a store's
// network at that moment is frequently the reason. A page that needs three
// external origins to render is a page that does not render during the
// incident it exists for. Embedding it also means the console is versioned
// with the gateway that serves it, so an API change and its console change
// ship as one artefact.
// ---------------------------------------------------------------------------

//go:embed assets/console.html
var consolePage []byte

var consoleETag = etagOf(consolePage)

// ConsolePage returns the embedded console, for tests and for anyone wanting
// to serve it from elsewhere.
func ConsolePage() []byte {
	out := make([]byte, len(consolePage))
	copy(out, consolePage)
	return out
}

func (g *Gateway) handleConsole(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, consolePage, "text/html; charset=utf-8", consoleETag, assetCSP)
}

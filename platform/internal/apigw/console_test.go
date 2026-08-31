package apigw

import (
	"net/http"
	"strings"
	"testing"
)

func TestConsoleIsServed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	res := h.do(http.MethodGet, "/console", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type %q", ct)
	}
	if res.Header.Get("ETag") == "" {
		t.Error("no ETag on the console")
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy is missing %q: %s", want, csp)
		}
	}
}

// TestConsoleHasItsOperationalPanels pins the mount points down. The console is
// a deliverable, not decoration: a refactor that quietly drops the latency
// chart or the attestation column should fail the build.
func TestConsoleHasItsOperationalPanels(t *testing.T) {
	t.Parallel()
	body := string(ConsolePage())

	mounts := map[string]string{
		`id="store-select"`:      "store selector",
		`id="health-panel"`:      "live store health",
		`id="h-online"`:          "labels online",
		`id="h-offline"`:         "labels offline",
		`id="h-battery"`:         "battery warnings",
		`id="h-mesh-bar"`:        "mesh health",
		`id="label-grid"`:        "the live label grid",
		`id="latency-chart"`:     "the end-to-end latency chart",
		`id="error-budget"`:      "error-budget burn",
		`id="price-feed"`:        "recent price changes",
		`id="ota-panel"`:         "OTA job progress",
		`id="pos-panel"`:         "POS integration health",
		`id="test-price-form"`:   "the test price action",
		`id="stream-status"`:     "the stream connection indicator",
		`id="credential"`:        "the credential field",
		`.att.signed`:            "attested status styling on a label",
		`.att.unsigned`:          "unattested status styling on a label",
		"3.000s SLO":             "the SLO line on the chart",
		"budget burn":            "the error budget label",
		"error budget for this ": "the burn explanation",
	}
	for needle, what := range mounts {
		if !strings.Contains(body, needle) {
			t.Errorf("the console is missing %s (%q)", what, needle)
		}
	}
}

func TestConsoleDrivesItselfFromTheGatewaysOwnAPI(t *testing.T) {
	t.Parallel()
	body := string(ConsolePage())

	endpoints := []string{
		"/v1/stream",   // the live feed
		"/v1/me",       // identity and store scope
		"/v1/stores/",  // the composed store overview
		"/overview",    //
		"/v1/prices",   // the test price action
		"/v1/pos/inte", // POS integration health
	}
	for _, e := range endpoints {
		if !strings.Contains(body, e) {
			t.Errorf("the console never calls %s", e)
		}
	}
	// The credential travels in the subprotocol, not the query string: a query
	// string is written to every access log between the browser and here.
	if !strings.Contains(body, wsCredentialProtocol) {
		t.Errorf("the console does not use the %q subprotocol to authenticate the stream", wsCredentialProtocol)
	}
	if strings.Contains(body, "?token=") || strings.Contains(body, "&key=") {
		t.Error("the console puts a credential in a query string")
	}
	// And it reacts to the slow-consumer close rather than treating it as a
	// generic error.
	if !strings.Contains(body, "1013") {
		t.Error("the console does not handle the 1013 slow-consumer close")
	}
}

func TestConsoleIsSelfContained(t *testing.T) {
	t.Parallel()
	body := string(ConsolePage())
	assertNoExternalResources(t, body)
	for _, forbidden := range []string{"<script src=", "<link rel=\"stylesheet\"", "@import"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the console loads an external resource: %q", forbidden)
		}
	}
	if !strings.Contains(body, "prefers-color-scheme") {
		t.Error("the console does not adapt to a dark or light desktop")
	}
}
